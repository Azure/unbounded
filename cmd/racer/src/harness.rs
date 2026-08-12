//! What `racer-bench` and `tests/cluster.rs` both need to run real nodes: a block device
//! made of memory with a fresh ext4 on it, and the lifecycle of one `racer serve`.
//!
//! Both drive real processes over real kernel interfaces, so both want a store that
//! belongs to the run and disappears with it. A memfd holds the bytes, a loop device
//! makes them a block device, and ext4 on top gives racer the file it asks for. Nothing
//! outside the run is touched, nothing survives it, and a run that dies without unwinding
//! leaves only a detached mount the kernel reaps on its own.
//!
//! Topology, configuration text, assertions and load generation stay with the caller.
//! This knows how to run one node, not what a cluster should look like.

// The two callers use different halves: the benchmark pins cores and never asks a store
// its length, the test does the reverse.
#![allow(dead_code)]

use std::fs::{File, OpenOptions};
use std::io::{BufRead, BufReader};
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd, RawFd};
use std::os::unix::process::CommandExt;
use std::path::{Path, PathBuf};
use std::process::{Child, ChildStdout, Command, Stdio};
use std::time::{Duration, Instant};

use racer::config::Config;

const LOOP_CTL_GET_FREE: libc::c_ulong = 0x4c82;
const LOOP_SET_FD: libc::c_ulong = 0x4c00;
const LOOP_CLR_FD: libc::c_ulong = 0x4c01;
const LOOP_SET_BLOCK_SIZE: libc::c_ulong = 0x4c09;

// ---------------------------------------------------------------------------
// backing store: memfd -> loop -> ext4
// ---------------------------------------------------------------------------

/// One node's storage, held in memory and owned by this value.
pub struct Backing {
    mnt: PathBuf,
    /// Cleared before `_memfd` closes, since a loop device outlives its backing file.
    loop_dev: Option<OwnedFd>,
    _memfd: OwnedFd,
}

impl Backing {
    /// A file system of `bytes` mounted at `mnt`. `tag` names the memfd only, which is
    /// what `/proc/self/fd` shows while the run is up.
    ///
    /// The bytes are never written until something stores them, so the tail of an
    /// oversized file system costs nothing.
    pub fn new(mnt: &Path, bytes: u64, tag: &str) -> Backing {
        std::fs::create_dir_all(mnt).expect("create the mount point");

        let name = std::ffi::CString::new(format!("racer-{tag}")).unwrap();
        let fd = unsafe { libc::memfd_create(name.as_ptr(), 0) };
        assert!(fd >= 0, "memfd_create: {}", last_error());
        let memfd = unsafe { OwnedFd::from_raw_fd(fd) };
        assert_eq!(
            unsafe { libc::ftruncate(fd, bytes as i64) },
            0,
            "ftruncate: {}",
            last_error()
        );

        let (loop_dev, loop_path) = loop_attach(fd);
        // One 4 KiB block, few inodes and no journal: the file system carries a single
        // large file opened `O_DIRECT`, and everything else on it is waste.
        let ok = Command::new("mkfs.ext4")
            .args([
                "-q",
                "-F",
                "-b",
                "4096",
                "-N",
                "64",
                "-O",
                "^has_journal",
                &loop_path,
            ])
            .status()
            .expect("mkfs.ext4");
        assert!(ok.success(), "mkfs.ext4 failed on {loop_path}");

        let rc = unsafe {
            libc::mount(
                cstr(Path::new(&loop_path)).as_ptr(),
                cstr(mnt).as_ptr(),
                c"ext4".as_ptr(),
                libc::MS_NOATIME,
                std::ptr::null(),
            )
        };
        assert_eq!(rc, 0, "mount {loop_path}: {}", last_error());

        Backing {
            mnt: mnt.to_path_buf(),
            loop_dev: Some(loop_dev),
            _memfd: memfd,
        }
    }

    /// A path on this file system. Nothing is created here: the caller decides whether
    /// racer places its own store or finds one waiting.
    pub fn path(&self, name: &str) -> PathBuf {
        self.mnt.join(name)
    }

    pub fn mount_point(&self) -> &Path {
        &self.mnt
    }
}

impl Drop for Backing {
    fn drop(&mut self) {
        // Lazily: a node's last file may still be closing as this runs.
        unsafe { libc::umount2(cstr(&self.mnt).as_ptr(), libc::MNT_DETACH) };
        if let Some(d) = self.loop_dev.take() {
            unsafe { libc::ioctl(d.as_raw_fd(), LOOP_CLR_FD) };
        }
    }
}

/// Bind a free loop device to `backing`. udev creates the node a moment later.
fn loop_attach(backing: RawFd) -> (OwnedFd, String) {
    let ctl = File::open("/dev/loop-control").expect("/dev/loop-control");
    let n = unsafe { libc::ioctl(ctl.as_raw_fd(), LOOP_CTL_GET_FREE) };
    assert!(n >= 0, "LOOP_CTL_GET_FREE: {}", last_error());
    let path = format!("/dev/loop{n}");
    wait_for(Path::new(&path));
    let dev = OpenOptions::new()
        .read(true)
        .write(true)
        .open(&path)
        .expect("open loop device");
    let rc = unsafe { libc::ioctl(dev.as_raw_fd(), LOOP_SET_FD, backing) };
    assert_eq!(rc, 0, "LOOP_SET_FD: {}", last_error());
    unsafe { libc::ioctl(dev.as_raw_fd(), LOOP_SET_BLOCK_SIZE, 4096) };
    (OwnedFd::from(dev), path)
}

// ---------------------------------------------------------------------------
// one node process
// ---------------------------------------------------------------------------

/// A `racer serve` process and what it published on the way up.
pub struct Proc {
    pub id: u32,
    /// Where this node's generations are written.
    pub dir: PathBuf,
    /// The store file. It is this process's own, so it is passed in rather than named in
    /// a generation every node shares.
    pub store: PathBuf,
    exe: PathBuf,
    /// Logical CPUs to pin to, or empty to leave the placement to the scheduler.
    cpus: Vec<usize>,
    child: Option<Child>,
    out: Option<BufReader<ChildStdout>>,
    pub devices: Vec<(u32, PathBuf)>,
    pub fabric: PathBuf,
    /// Where this node's prometheus endpoint landed; the port is ephemeral.
    pub metrics: String,
}

impl Proc {
    /// A node that has not started yet. `exe` is passed in because the benchmark finds
    /// the binary beside its own and the test is handed one by cargo.
    pub fn new(id: u32, dir: PathBuf, store: PathBuf, exe: PathBuf) -> Proc {
        std::fs::create_dir_all(&dir).expect("create the node directory");
        Proc {
            id,
            dir,
            store,
            exe,
            cpus: Vec::new(),
            child: None,
            out: None,
            devices: Vec::new(),
            fabric: PathBuf::new(),
            metrics: String::new(),
        }
    }

    /// Confine the process to these logical CPUs from its first instruction.
    pub fn pin(&mut self, cpus: Vec<usize>) {
        self.cpus = cpus;
    }

    /// The generation this node reads, which is what an installer renames over.
    pub fn config(&self) -> PathBuf {
        self.dir.join("node.pb")
    }

    pub fn exe(&self) -> &Path {
        &self.exe
    }

    /// Start `racer serve` and read back the devices it published.
    pub fn serve(&mut self) {
        let mut cmd = Command::new(&self.exe);
        cmd.arg("serve")
            .arg(self.config())
            .env("METRICS_ADDR", "127.0.0.1:0")
            .env(racer::config::STORE_PATH_ENV, &self.store)
            .stdout(Stdio::piped());
        if !self.cpus.is_empty() {
            let cpus = self.cpus.clone();
            unsafe {
                cmd.pre_exec(move || {
                    let mut set: libc::cpu_set_t = std::mem::zeroed();
                    libc::CPU_ZERO(&mut set);
                    for &c in &cpus {
                        libc::CPU_SET(c, &mut set);
                    }
                    libc::sched_setaffinity(0, std::mem::size_of::<libc::cpu_set_t>(), &set);
                    Ok(())
                })
            };
        }
        let mut child = cmd.spawn().expect("spawn racer serve");
        let mut out = BufReader::new(child.stdout.take().unwrap());
        self.devices.clear();
        loop {
            let line = next_line(&mut out, self.id);
            if let Some(rest) = line.strip_prefix("metrics -> ") {
                self.metrics = rest.to_string();
            } else if let Some(rest) = line.strip_prefix("device ") {
                let (id, path) = rest.split_once(" -> ").expect("device line");
                self.devices
                    .push((id.parse().unwrap(), PathBuf::from(path)));
            } else if let Some(rest) = line.strip_prefix("universe ") {
                // One universe: its fabric device ends the banner and peers attach to it.
                let (_, path) = rest.split_once(" fabric -> ").expect("universe line");
                self.fabric = PathBuf::from(path);
                break;
            }
        }
        for (_, p) in &self.devices {
            wait_for(p);
        }
        wait_for(&self.fabric);
        self.child = Some(child);
        self.out = Some(out);
    }

    /// Install a generation the way the control plane does: write, then rename atomically.
    pub fn install(&self, text: &str) {
        let cfg = Config::parse(text).expect("parse config");
        cfg.validate().expect("validate config");
        let tmp = self.dir.join("node.next");
        std::fs::write(&tmp, cfg.encode()).expect("write the next generation");
        std::fs::rename(&tmp, self.config()).expect("install the next generation");
    }

    /// Block until the node says it took the generation last installed.
    pub fn await_reload(&mut self) {
        let out = self.out.as_mut().expect("a running node");
        while next_line(out, self.id) != "racer: configuration applied" {}
    }

    pub fn signal(&mut self, sig: i32) {
        if let Some(c) = &self.child {
            unsafe { libc::kill(c.id() as i32, sig) };
        }
    }

    pub fn reap(&mut self) {
        if let Some(mut c) = self.child.take() {
            let _ = c.wait();
        }
        self.out = None;
    }

    /// This node's block device `id`.
    pub fn device(&self, id: u32) -> &Path {
        &self
            .devices
            .iter()
            .find(|(v, _)| *v == id)
            .expect("device")
            .1
    }

    /// Bytes the store holds, which is what `size=` asked for at this node's last start.
    pub fn store_len(&self) -> u64 {
        std::fs::metadata(&self.store)
            .expect("stat the store")
            .len()
    }
}

impl Drop for Proc {
    fn drop(&mut self) {
        self.signal(libc::SIGKILL);
        self.reap();
    }
}

/// Stop nodes together: a ublk device cannot be torn down while a peer holds it open, so
/// asking one to leave while the others are up wedges it until it is killed.
pub fn shutdown<'a>(procs: impl IntoIterator<Item = &'a mut Proc>) {
    let mut procs: Vec<&mut Proc> = procs.into_iter().collect();
    for p in procs.iter_mut() {
        p.signal(libc::SIGTERM);
    }
    let deadline = Instant::now() + Duration::from_secs(15);
    for p in procs.iter_mut() {
        while Instant::now() < deadline {
            match p.child.as_mut().map(|c| c.try_wait().unwrap()) {
                None | Some(Some(_)) => break,
                Some(None) => std::thread::sleep(Duration::from_millis(20)),
            }
        }
        p.signal(libc::SIGKILL);
        p.reap();
    }
}

// ---------------------------------------------------------------------------
// plumbing
// ---------------------------------------------------------------------------

/// One line of a node's stdout, or a panic if it died.
fn next_line(out: &mut BufReader<ChildStdout>, id: u32) -> String {
    let mut s = String::new();
    let n = out.read_line(&mut s).expect("read node stdout");
    assert!(n > 0, "node {id} exited before publishing its devices");
    s.trim_end().to_string()
}

/// Wait up to ten seconds for a path udev or racer is about to create.
pub fn wait_for(p: &Path) {
    for _ in 0..1000 {
        if p.exists() {
            return;
        }
        std::thread::sleep(Duration::from_millis(10));
    }
    panic!("{} never appeared", p.display());
}

pub fn cstr(p: &Path) -> std::ffi::CString {
    std::ffi::CString::new(p.as_os_str().as_encoded_bytes()).unwrap()
}

pub fn last_error() -> std::io::Error {
    std::io::Error::last_os_error()
}
