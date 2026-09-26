//! An exclusively owned RAM disk. All mountpoints and device nodes are in target/.
use racer_dataplane::model::limits::Limits;
use std::{fs, os::fd::AsRawFd, path::PathBuf, process::Command};

const MIB: u64 = 1 << 20;
const DEVICE: u64 = 4096 * MIB;

fn memory_budget(workers: usize, limits: &Limits) -> (u64, u64) {
    // Resident plaintext/cache, ciphertext including slab/crypto staging, dirty
    // retention, registered allowance (unused here), and request contexts. Some
    // dimensions overlap; adding them is deliberately conservative.
    let admitted = [
        limits.plaintext_bytes,
        limits.ciphertext_bytes,
        limits.dirty_bytes,
        limits.registered_bytes,
        limits.request_context_bytes,
    ]
    .iter()
    .map(|n| n.get() as u64)
    .sum::<u64>()
        * workers as u64;
    // Per worker: streaming origin (8 connections, 64 KiB chunks), stacks,
    // crypto scratch, queues/indexes, rings, sockets and pipes. Origin never
    // materializes all payloads. Global: allocator retention plus 128 clients'
    // stacks, two 64 KiB buffers apiece and kernel socket buffers.
    let auxiliary = workers as u64 * 512 * MIB + 2048 * MIB + 512 * MIB;
    let envelope = DEVICE + admitted + auxiliary;
    (envelope, (16 << 30).max(envelope * 2))
}

pub fn preflight(workers: usize, limits: &Limits, slab_bytes: u64) {
    assert!((1..=4).contains(&workers));
    assert!(
        slab_bytes * workers as u64 <= 3 << 30,
        "aggregate slab capacity must leave 1 GiB for ext4/headroom"
    );
    let (envelope, required) = memory_budget(workers, limits);
    let info = fs::read_to_string("/proc/meminfo").unwrap();
    let available = info
        .lines()
        .find(|s| s.starts_with("MemAvailable:"))
        .unwrap()
        .split_whitespace()
        .nth(1)
        .unwrap()
        .parse::<u64>()
        .unwrap()
        * 1024;
    println!(
        "memory_preflight workers={workers} device_MiB=4096 slab_capacity_MiB={} plaintext_total_MiB={} ciphertext_total_MiB={} dirty_total_MiB={} context_total_MiB={} auxiliary_MiB={} envelope_MiB={} required_available_MiB={} host_available_MiB={}",
        slab_bytes * workers as u64 / MIB,
        limits.plaintext_bytes.get() as u64 * workers as u64 / MIB,
        limits.ciphertext_bytes.get() as u64 * workers as u64 / MIB,
        limits.dirty_bytes.get() as u64 * workers as u64 / MIB,
        limits.request_context_bytes.get() as u64 * workers as u64 / MIB,
        workers as u64 * 512 + 2560,
        envelope / MIB,
        required.div_ceil(MIB),
        available / MIB
    );
    assert!(
        available >= required,
        "insufficient host RAM; refusing benchmark"
    );
    // Fail closed on layouts we cannot resolve, rather than treating a missing
    // memory.max as unlimited. This fixture supports unified cgroup v2 mounted
    // at its namespace root; it does not silently skip v1 or relocated mounts.
    let mounts = fs::read_to_string("/proc/self/mountinfo").unwrap();
    assert!(
        mounts.lines().any(|line| {
            let Some((before, after)) = line.split_once(" - ") else {
                return false;
            };
            let fields: Vec<_> = before.split_whitespace().collect();
            after.starts_with("cgroup2 ")
                && fields.get(3) == Some(&"/")
                && fields.get(4) == Some(&"/sys/fs/cgroup")
        }),
        "benchmark requires a resolvable unified cgroup-v2 mount"
    );
    let groups = fs::read_to_string("/proc/self/cgroup").unwrap();
    let group = groups
        .lines()
        .find_map(|s| s.strip_prefix("0::"))
        .expect("benchmark requires cgroup v2");
    assert!(!group.split('/').any(|part| part == ".."));
    let root = std::path::Path::new("/sys/fs/cgroup");
    let leaf = root.join(group.trim_start_matches('/'));
    assert!(leaf.is_dir(), "unresolved cgroup membership");
    for path in leaf.ancestors().take_while(|path| path.starts_with(root)) {
        if path == root {
            break;
        } // The root cgroup has no memory controller files.
        let max =
            fs::read_to_string(path.join("memory.max")).expect("cannot check cgroup memory limit");
        let used = fs::read_to_string(path.join("memory.current"))
            .unwrap()
            .trim()
            .parse::<u64>()
            .unwrap();
        println!(
            "cgroup_memory path={} max={} current_MiB={}",
            path.display(),
            max.trim(),
            used / MIB
        );
        if max.trim() != "max" {
            let max = max.trim().parse::<u64>().unwrap();
            assert!(
                max.saturating_sub(used) >= required,
                "insufficient cgroup RAM; refusing benchmark"
            );
        }
    }
}

pub struct Brd {
    pub root: PathBuf,
    _lock: fs::File,
    loaded: bool,
    mounted: bool,
}

fn privileged(args: &[&str]) {
    let status = Command::new("sudo").arg("-n").args(args).status().unwrap();
    assert!(status.success(), "sudo {args:?}: {status}");
}

impl Brd {
    pub fn new(workers: usize, limits: &Limits, slab_bytes: u64) -> Self {
        preflight(workers, limits, slab_bytes);
        let target = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("target");
        fs::create_dir_all(&target).unwrap();
        let lock = fs::OpenOptions::new()
            .create(true)
            .truncate(false)
            .write(true)
            .open(target.join("hotpath.lock"))
            .unwrap();
        // SAFETY: live descriptor, advisory exclusive lock released by File::drop.
        assert_eq!(
            unsafe { libc::flock(lock.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) },
            0,
            "another benchmark owns brd"
        );
        assert!(
            !std::path::Path::new("/sys/module/brd").exists(),
            "brd already loaded; refusing to touch existing RAM disks"
        );
        let root = target.join(format!("hotpath-brd-{}", std::process::id()));
        fs::create_dir(&root).unwrap();
        let mut fixture = Self {
            root,
            _lock: lock,
            loaded: false,
            mounted: false,
        };
        fs::create_dir(fixture.root.join("ext4")).unwrap();
        privileged(&[
            "modprobe",
            "brd",
            "rd_nr=1",
            "rd_size=4194304",
            "max_part=1",
        ]);
        fixture.loaded = true;
        let device = fixture.root.join("device");
        privileged(&["mknod", device.to_str().unwrap(), "b", "1", "0"]);
        privileged(&[
            "mkfs.ext4",
            "-q",
            "-m",
            "0",
            "-E",
            "lazy_itable_init=0,lazy_journal_init=0",
            device.to_str().unwrap(),
        ]);
        privileged(&[
            "mount",
            "-t",
            "ext4",
            "-o",
            "noatime",
            device.to_str().unwrap(),
            fixture.mount().to_str().unwrap(),
        ]);
        fixture.mounted = true;
        // SAFETY: getuid/getgid have no preconditions.
        let owner = unsafe { format!("{}:{}", libc::getuid(), libc::getgid()) };
        privileged(&["chown", &owner, fixture.mount().to_str().unwrap()]);
        fixture
    }
    pub fn mount(&self) -> PathBuf {
        self.root.join("ext4")
    }
    pub fn close(&mut self) -> bool {
        if self.mounted {
            let ok = Command::new("sudo")
                .args(["-n", "umount"])
                .arg(self.mount())
                .status()
                .is_ok_and(|s| s.success());
            if !ok {
                return false;
            }
            self.mounted = false;
        }
        if self.loaded {
            let ok = Command::new("sudo")
                .args(["-n", "modprobe", "-r", "brd"])
                .status()
                .is_ok_and(|s| s.success());
            if !ok {
                return false;
            }
            self.loaded = false;
        }
        fs::remove_dir_all(&self.root).is_ok()
    }
}

#[test]
fn memory_envelope_scales_with_workers_and_retains_headroom() {
    let limits = super::budgets();
    let (one, one_required) = memory_budget(1, &limits);
    let (four, four_required) = memory_budget(4, &limits);
    assert!(four > one);
    assert!(one_required >= 16 << 30);
    assert_eq!(four_required, four * 2);
    assert_eq!(four, 12416 * MIB + 4 * 16);
}
impl Drop for Brd {
    fn drop(&mut self) {
        if self.root.exists() && !self.close() {
            eprintln!("brd cleanup failed: {}", self.root.display());
        }
    }
}
