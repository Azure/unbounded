//! What `racer-bench` and `tests/cluster.rs` both need to run real nodes: a block device
//! made of memory with a fresh ext4 on it, and the lifecycle of one `racer serve`.
//!
//! Both drive real processes over real kernel interfaces, so both want a store that
//! belongs to the run and disappears with it. A `brd` ram disk is the block device and
//! ext4 on top gives racer the file it asks for. Nothing outside the run is touched and
//! nothing survives it: a run that dies without unwinding leaves a mount behind, and the
//! next run to name that device takes it back before it makes a file system on it.
//!
//! `brd` and not a loop device over a memfd, which is what this used to be: the loop
//! driver gives a device one hardware queue and hands every request to a single kernel
//! worker, which caps one device near 8 GiB/s and 380k IOPS no matter how many threads
//! ask. A benchmark that measures that is measuring the loop driver. `brd` has no request
//! queue at all - it copies in the submitter's own context - so what the numbers show is
//! racer and the machine.
//!
//! Topology, configuration text, assertions and load generation stay with the caller.
//! This knows how to run one node, not what a cluster should look like.

// The two callers use different halves: the benchmark pins cores and never asks a store
// its length, the test does the reverse.
#![allow(dead_code)]

use std::fs::OpenOptions;
use std::io::{BufRead, BufReader};
use std::os::fd::AsRawFd;
use std::os::unix::fs::OpenOptionsExt;
use std::os::unix::process::CommandExt;
use std::path::{Path, PathBuf};
use std::process::{Child, ChildStdout, Command, Stdio};
use std::time::{Duration, Instant};

use racer::config::Config;

/// `BLKDISCARD`, which frees the pages a ram disk is holding.
const BLKDISCARD: libc::c_ulong = 0x1277;

/// Ram disks the harness may use, and how big each one is. Both are module load
/// parameters, so both are global to the machine: `brd` has no call that adds one device
/// of a given size the way `LOOP_CTL_GET_FREE` adds a loop device. A device is therefore
/// named by the caller rather than allocated, and the callers partition the range the
/// same way they already partition ublk minors, so a benchmark and a test suite may run
/// at once. Pages are allocated as they are written, so the sum of the sizes is an
/// address space and not a reservation.
const RAM_COUNT: u32 = 12;
const RAM_BYTES: u64 = 8 << 30;

// ---------------------------------------------------------------------------
// backing store: brd -> ext4
// ---------------------------------------------------------------------------

/// One node's storage, held in memory and owned by this value.
pub struct Backing {
    mnt: PathBuf,
    dev: u32,
}

impl Backing {
    /// A file system of `bytes` mounted at `mnt`, on ram disk `dev`.
    ///
    /// The file system is sized explicitly rather than given the whole device, because
    /// every ram disk is the same size and a caller asking for less should get less: the
    /// store growth tests want a file system a good deal larger than the store on it.
    pub fn new(mnt: &Path, bytes: u64, dev: u32) -> Backing {
        assert!(
            dev < RAM_COUNT,
            "ram disk {dev} is outside the harness range"
        );
        ram_disks();
        let path = format!("/dev/ram{dev}");
        wait_for(Path::new(&path));
        let have = ram_bytes(dev);
        assert!(
            have >= bytes,
            "{path} holds {have} B and this run needs {bytes} B; \
             `modprobe -r brd` and let the harness load it again",
        );
        std::fs::create_dir_all(mnt).expect("create the mount point");
        claim(dev);
        // Pages an earlier run left are still resident and `mkfs` writes over only what
        // it uses. Dropping them here is what keeps a long session's footprint the size
        // of one run rather than the sum of them.
        discard(dev);

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
                &path,
                &(bytes / 4096).to_string(),
            ])
            .status()
            .expect("mkfs.ext4");
        assert!(ok.success(), "mkfs.ext4 failed on {path}");

        let rc = unsafe {
            libc::mount(
                cstr(Path::new(&path)).as_ptr(),
                cstr(mnt).as_ptr(),
                c"ext4".as_ptr(),
                libc::MS_NOATIME,
                std::ptr::null(),
            )
        };
        assert_eq!(rc, 0, "mount {path}: {}", last_error());

        Backing {
            mnt: mnt.to_path_buf(),
            dev,
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
        // Plainly first, so the pages can go back now. A node's last file may still be
        // closing, and then the device is still live and only a detached mount will do:
        // the kernel reaps it, and the next run's `mkfs` writes over what it left.
        if unsafe { libc::umount(cstr(&self.mnt).as_ptr()) } == 0 {
            discard(self.dev);
        } else {
            unsafe { libc::umount2(cstr(&self.mnt).as_ptr(), libc::MNT_DETACH) };
        }
    }
}

/// Load `brd` once per process. Already loaded is success: the size check the caller
/// makes next is what decides whether the devices will do, and reloading the module with
/// other parameters would take any other run on the machine down with it.
fn ram_disks() {
    static ONCE: std::sync::Once = std::sync::Once::new();
    ONCE.call_once(|| {
        let ok = Command::new("modprobe")
            .args([
                "brd",
                &format!("rd_nr={RAM_COUNT}"),
                &format!("rd_size={}", RAM_BYTES / 1024),
            ])
            .status()
            .expect("modprobe");
        assert!(ok.success(), "modprobe brd failed");
    });
}

/// What the kernel says ram disk `dev` holds. `size` is in 512 byte sectors whatever the
/// device's block size is.
fn ram_bytes(dev: u32) -> u64 {
    let p = format!("/sys/block/ram{dev}/size");
    let s = std::fs::read_to_string(&p).unwrap_or_else(|e| panic!("read {p}: {e}"));
    s.trim().parse::<u64>().expect("a sector count") * 512
}

/// Take ram disk `dev` back, so `mkfs` will have it.
///
/// A device is named and not allocated, so unlike the loop device this replaced it does
/// not come back free. Two things can still hold it: a run killed before it could unwind,
/// whose file system is mounted yet, and the rung before this one, which detached its
/// mount and left the kernel to reap the superblock. Neither is reachable by the path
/// this run means to use - a detached mount has no path at all - so what is unmounted
/// here is found by the device, and what is waited for is the device itself.
///
/// `O_EXCL` on a block device asks exactly the question `mkfs` asks: it fails while any
/// file system still holds the device and succeeds once the last one is gone.
fn claim(dev: u32) {
    let path = format!("/dev/ram{dev}");
    for m in mounts_of(&path) {
        // Plainly first, so the device is free on return. Detaching is the fallback
        // because a mount whose files are still closing cannot come off any other way.
        if unsafe { libc::umount(cstr(&m).as_ptr()) } != 0 {
            unsafe { libc::umount2(cstr(&m).as_ptr(), libc::MNT_DETACH) };
        }
    }
    let deadline = Instant::now() + Duration::from_secs(15);
    loop {
        let free = OpenOptions::new()
            .read(true)
            .write(true)
            .custom_flags(libc::O_EXCL)
            .open(&path)
            .is_ok();
        if free {
            return;
        }
        assert!(
            Instant::now() < deadline,
            "{path} is still held by a file system; \
             something outside this run is using the harness range",
        );
        std::thread::sleep(Duration::from_millis(20));
    }
}

/// Every mount point `/proc/self/mounts` says `dev` is under, newest first: a mount
/// stacked on another is listed after it and has to come off before it.
fn mounts_of(dev: &str) -> Vec<PathBuf> {
    let Ok(f) = std::fs::File::open("/proc/self/mounts") else {
        return Vec::new();
    };
    let mut out: Vec<PathBuf> = BufReader::new(f)
        .lines()
        .map_while(Result::ok)
        .filter_map(|l| {
            let mut it = l.split_whitespace();
            let src = it.next()?;
            let mnt = it.next()?;
            (src == dev).then(|| PathBuf::from(mnt))
        })
        .collect();
    out.reverse();
    out
}

/// Give ram disk `dev`'s pages back to the machine. Best effort, because `brd` need not
/// offer discard: a kernel that does not keeps the pages until something writes over
/// them, which costs memory and never correctness.
fn discard(dev: u32) {
    let path = format!("/dev/ram{dev}");
    let Ok(f) = OpenOptions::new().read(true).write(true).open(&path) else {
        return;
    };
    let range: [u64; 2] = [0, ram_bytes(dev)];
    unsafe { libc::ioctl(f.as_raw_fd(), BLKDISCARD, range.as_ptr()) };
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
/// Stop every node with SIGTERM and insist it goes on its own.
///
/// The SIGKILL is a cleanup path, not a shutdown path: a node that needs it has failed
/// its teardown, and letting that pass silently is how a shutdown deadlock stays hidden
/// for as long as it takes a test to notice something else. Every node is killed and
/// reaped before the assertion fires, so a failure here still leaves no strays behind.
pub fn shutdown<'a>(procs: impl IntoIterator<Item = &'a mut Proc>) {
    let mut procs: Vec<&mut Proc> = procs.into_iter().collect();
    for p in procs.iter_mut() {
        p.signal(libc::SIGTERM);
    }
    let deadline = Instant::now() + Duration::from_secs(15);
    let mut wedged = Vec::new();
    for p in procs.iter_mut() {
        let mut exited = false;
        while Instant::now() < deadline {
            match p.child.as_mut().map(|c| c.try_wait().unwrap()) {
                None | Some(Some(_)) => {
                    exited = true;
                    break;
                }
                Some(None) => std::thread::sleep(Duration::from_millis(20)),
            }
        }
        if !exited {
            wedged.push(p.id);
        }
        p.signal(libc::SIGKILL);
        p.reap();
    }
    assert!(
        wedged.is_empty(),
        "nodes {wedged:?} did not exit within 15s of SIGTERM and had to be killed",
    );
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
