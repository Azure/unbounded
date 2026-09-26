//! An exclusively owned RAM disk. All mountpoints and device nodes are in target/.
use std::{fs, os::fd::AsRawFd, path::PathBuf, process::Command};

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
    pub fn new() -> Self {
        // 4 GiB device + <= 2 GiB service/client/allocator allowance, with ample
        // headroom. Never size buffers or the RAM disk from host CPU count.
        let info = fs::read_to_string("/proc/meminfo").unwrap();
        let available: u64 = info
            .lines()
            .find(|s| s.starts_with("MemAvailable:"))
            .unwrap()
            .split_whitespace()
            .nth(1)
            .unwrap()
            .parse()
            .unwrap();
        assert!(
            available >= 16 * 1024 * 1024,
            "benchmark needs 16 GiB MemAvailable; found {available} KiB"
        );
        // A container's host MemAvailable can be misleading. Check all visible
        // cgroup-v2 ancestors as well; refuse constrained environments.
        let groups = fs::read_to_string("/proc/self/cgroup").unwrap();
        if let Some(group) = groups.lines().find_map(|s| s.strip_prefix("0::")) {
            let mut path = PathBuf::from("/sys/fs/cgroup").join(group.trim_start_matches('/'));
            loop {
                if let Ok(max) = fs::read_to_string(path.join("memory.max")) {
                    if let Ok(max) = max.trim().parse::<u64>() {
                        let used: u64 = fs::read_to_string(path.join("memory.current"))
                            .unwrap()
                            .trim()
                            .parse()
                            .unwrap();
                        assert!(
                            max.saturating_sub(used) >= 16 << 30,
                            "cgroup needs 16 GiB free memory"
                        );
                    }
                }
                if path == std::path::Path::new("/sys/fs/cgroup") || !path.pop() {
                    break;
                }
            }
        }
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
impl Drop for Brd {
    fn drop(&mut self) {
        if self.root.exists() && !self.close() {
            eprintln!("brd cleanup failed: {}", self.root.display());
        }
    }
}
