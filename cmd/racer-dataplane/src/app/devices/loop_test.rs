//! Opt-in root test. Only newly configured, autoclearing loop devices are written.

use super::*;
use std::os::unix::fs::OpenOptionsExt;
use std::process::Command;
use std::rc::Rc;
use std::time::{Duration, Instant};

const TEST: &str = "app::devices::loop_test::disposable_loop_devices_read_only_bind_restart";
const CHILD: &str = "RACER_DISPOSABLE_LOOP_TEST_CHILD";

fn command(name: &str, args: &[&std::ffi::OsStr]) {
    let status = Command::new("timeout")
        .args(["--signal=TERM", "--kill-after=10s", "60s", name])
        .args(args)
        .status()
        .unwrap();
    assert!(status.success(), "{name} failed: {status}");
}

#[repr(C)]
struct LoopInfo {
    device: u64,
    inode: u64,
    rdevice: u64,
    offset: u64,
    size_limit: u64,
    number: u32,
    encrypt_type: u32,
    encrypt_key_size: u32,
    flags: u32,
    file_name: [u8; 64],
    crypt_name: [u8; 64],
    encrypt_key: [u8; 32],
    init: [u64; 2],
}

impl Default for LoopInfo {
    fn default() -> Self {
        // SAFETY: this kernel ABI record contains only integers and byte arrays.
        unsafe { std::mem::zeroed() }
    }
}

struct Mount(std::path::PathBuf);
impl Drop for Mount {
    fn drop(&mut self) {
        let path = std::ffi::CString::new(self.0.as_os_str().as_encoded_bytes()).unwrap();
        // SAFETY: only our private namespace's disposable bind is detached.
        unsafe {
            libc::umount2(path.as_ptr(), libc::MNT_DETACH);
        }
    }
}

#[repr(C)]
struct LoopConfig {
    fd: u32,
    block_size: u32,
    info: LoopInfo,
    reserved: [u64; 8],
}

fn new_loop(root: &Path, name: &str, bytes: u64) -> File {
    let backing = fs::OpenOptions::new()
        .read(true)
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(root.join(format!("{name}.img")))
        .unwrap();
    backing.set_len(bytes).unwrap();
    let control = fs::OpenOptions::new()
        .read(true)
        .write(true)
        .open("/dev/loop-control")
        .expect("requires /dev/loop-control and CAP_SYS_ADMIN");
    for attempt in 0..16 {
        // SAFETY: LOOP_CTL_GET_FREE only returns an unused loop number.
        let number = unsafe { libc::ioctl(control.as_raw_fd(), 0x4c82) };
        assert!(
            number >= 0,
            "LOOP_CTL_GET_FREE: {}",
            io::Error::last_os_error()
        );
        let path = root.join("dev").join(format!("{name}-{attempt}"));
        let cpath = std::ffi::CString::new(path.as_os_str().as_encoded_bytes()).unwrap();
        // SAFETY: private fresh path; this node refers only to a candidate free loop.
        assert_eq!(
            unsafe {
                libc::mknod(
                    cpath.as_ptr(),
                    libc::S_IFBLK | 0o600,
                    libc::makedev(7, number as u32),
                )
            },
            0
        );
        let file = fs::OpenOptions::new()
            .read(true)
            .write(true)
            .open(&path)
            .unwrap();
        let config = LoopConfig {
            fd: backing.as_raw_fd() as u32,
            block_size: 512,
            info: LoopInfo {
                flags: 4,
                ..Default::default()
            }, // LO_FLAGS_AUTOCLEAR
            reserved: [0; 8],
        };
        // SAFETY: LOOP_CONFIGURE atomically binds only an unconfigured loop to our fd.
        let result = unsafe { libc::ioctl(file.as_raw_fd(), 0x4c0a, &config) };
        if result == 0 {
            std::os::unix::fs::symlink(
                format!("../../{name}-{attempt}"),
                root.join("dev/disk/by-id").join(name),
            )
            .unwrap();
            return file; // Closing the last fd detaches, including on process death.
        }
        let error = io::Error::last_os_error();
        assert_eq!(
            error.raw_os_error(),
            Some(libc::EBUSY),
            "LOOP_CONFIGURE: {error}"
        );
    }
    panic!("could not claim a free loop after 16 races");
}

fn drive<T>(reactor: &crate::runtime::Reactor, mut operation: crate::error::Operation<'_, T>) -> T {
    let end = Instant::now() + Duration::from_secs(20);
    let mut cx = std::task::Context::from_waker(std::task::Waker::noop());
    loop {
        assert!(Instant::now() < end, "loop-device I/O deadline");
        reactor.poll_budgeted(64).unwrap();
        if let std::task::Poll::Ready(result) = operation.as_mut().poll(&mut cx) {
            return result.unwrap();
        }
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
}

#[test]
#[ignore = "requires root, loop-control, CAP_SYS_ADMIN, CAP_MKNOD and io_uring; uses only disposable loops"]
fn disposable_loop_devices_read_only_bind_restart() {
    assert_eq!(
        unsafe { libc::geteuid() },
        0,
        "run this ignored test binary as root"
    );
    if std::env::var_os(CHILD).is_none() {
        let executable = std::env::current_exe().unwrap();
        let status = Command::new("timeout")
            .args([
                "--signal=TERM",
                "--kill-after=10s",
                "240s",
                "unshare",
                "--mount",
                "--propagation",
                "private",
            ])
            .arg(executable)
            .args([
                "--exact",
                TEST,
                "--ignored",
                "--nocapture",
                "--test-threads=1",
            ])
            .env(CHILD, "1")
            .status()
            .unwrap();
        assert!(
            status.success(),
            "private mount namespace test failed: {status}"
        );
        return;
    }
    let fixture = super::tests::Fixture::new();
    let root = &fixture.0;
    fs::create_dir_all(root.join("dev/disk/by-id")).unwrap();
    fs::create_dir(root.join("mounted")).unwrap();
    let mut config = crate::test_support::cluster::config(false);
    config.segment_bytes = 32 * 1024 * 1024;
    config.free_segment_reserve = 1;
    config.device_directory = root.join("mounted");
    config.slab_directory = root.join("checkpoints");
    assert!(!config.slab_directory.exists());
    let segment = config.segment_bytes;
    let first = new_loop(root, "cache-a", 5 * segment + 2 * GUARD_BYTES);
    let second = new_loop(root, "cache-b", 6 * segment + 2 * GUARD_BYTES);
    command(
        "mount",
        &[
            "--bind".as_ref(),
            root.join("dev").as_os_str(),
            config.device_directory.as_os_str(),
        ],
    );
    let mount = Mount(config.device_directory.clone());
    command(
        "mount",
        &[
            "-o".as_ref(),
            "remount,bind,ro".as_ref(),
            config.device_directory.as_os_str(),
        ],
    );
    assert_eq!(
        fs::write(config.device_directory.join("must-not-create"), "x")
            .unwrap_err()
            .raw_os_error(),
        Some(libc::EROFS)
    );
    let plan = discover(&config, Some("cache-"), 2)
        .expect("both disposable loops must be usable through read-only bind");
    assert_eq!(
        plan.workers
            .iter()
            .map(|w| w.placements.len())
            .collect::<Vec<_>>(),
        [6, 5]
    );
    let worker = &plan.workers[0];
    assert_eq!(worker.placements[0].offset, GUARD_BYTES);
    assert_eq!(worker.placements[5].offset, GUARD_BYTES);
    assert_eq!(
        worker
            .placements
            .iter()
            .map(|p| p.file.metadata().unwrap().rdev())
            .collect::<HashSet<_>>()
            .len(),
        2
    );
    assert!(
        discover(&config, Some("cache-"), 2).is_none(),
        "second exclusive claim must fail"
    );
    let slab = page_alloc::Slab::<()>::from_devices(
        worker
            .placements
            .iter()
            .map(|p| page_alloc::DevicePlacement {
                file: p.file.clone(),
                offset: p.offset,
            })
            .collect(),
        segment,
        4096,
        worker.alignment,
    )
    .unwrap();
    let segments = Rc::new(page_alloc::Segments::new(segment));
    let _ = slab.open_configured(&segments).unwrap();
    assert_eq!(slab.capacity_bytes(), 6 * segment);
    let admission = Rc::new(flow_control::Quotas::new(
        crate::admission::AdmissionPolicy::new(config.limits.clone()),
    ));
    let reactor = crate::runtime::Reactor::new(admission);
    let scope = crate::runtime::RequestScope::new(
        crate::model::RequestId([99; 16]),
        Instant::now() + Duration::from_secs(30),
    )
    .unwrap();
    // One full extent per segment reaches both physical devices in worker zero.
    for index in 0..6 {
        let (lease, extent) = segments.append(segment as usize).unwrap();
        let mut buffer = slab.allocate(segment as usize, ()).unwrap();
        buffer.as_mut_slice().fill(index as u8 + 1);
        drop(drive(
            &reactor,
            slab.write(&reactor, extent, buffer, lease, &scope),
        ));
        let lease = segments
            .lease(page_alloc::SegmentId(index), page_alloc::Generation(1))
            .unwrap();
        let buffer = slab.allocate(segment as usize, ()).unwrap();
        let buffer = drive(&reactor, slab.read(&reactor, extent, buffer, lease, &scope));
        assert!(buffer.as_slice().iter().all(|b| *b == index as u8 + 1));
    }
    let other = &plan.workers[1];
    let other_slab = page_alloc::Slab::<()>::from_devices(
        other
            .placements
            .iter()
            .map(|p| page_alloc::DevicePlacement {
                file: p.file.clone(),
                offset: p.offset,
            })
            .collect(),
        segment,
        4096,
        other.alignment,
    )
    .unwrap();
    let other_segments = Rc::new(page_alloc::Segments::new(segment));
    let _ = other_slab.open_configured(&other_segments).unwrap();
    assert_eq!(other.placements.len(), 5);
    for index in 0..other.placements.len() {
        let (lease, extent) = other_segments.append(segment as usize).unwrap();
        let mut buffer = other_slab.allocate(segment as usize, ()).unwrap();
        buffer.as_mut_slice().fill(index as u8 + 32);
        drop(drive(
            &reactor,
            other_slab.write(&reactor, extent, buffer, lease, &scope),
        ));
        let lease = other_segments
            .lease(
                page_alloc::SegmentId(index as u64),
                page_alloc::Generation(1),
            )
            .unwrap();
        let buffer = other_slab.allocate(segment as usize, ()).unwrap();
        let buffer = drive(
            &reactor,
            other_slab.read(&reactor, extent, buffer, lease, &scope),
        );
        assert!(buffer.as_slice().iter().all(|b| *b == index as u8 + 32));
    }
    drop(other_slab);
    for (file, bytes) in [(&first, 5 * segment), (&second, 6 * segment)] {
        validate_guards(
            bytes + 2 * GUARD_BYTES,
            worker.alignment,
            |buffer, offset| file.read_at(buffer, offset),
        )
        .unwrap();
    }
    let index = Rc::new(crate::store::catalog::Index::new(
        crate::model::WorkerId(0),
        16,
        crate::test_support::availability(),
    ));
    let checkpoint = crate::store::checkpoint::Checkpointer::new(
        config.slab_directory.clone(),
        index.clone(),
        segments.clone(),
    )
    .with_layout(worker.digest);
    checkpoint
        .configure_geometry(slab.geometry().unwrap().into())
        .unwrap();
    let image = futures::executor::block_on(checkpoint.snapshot_shard()).unwrap();
    drive(
        &reactor,
        Box::pin(checkpoint.prepare_directory(&reactor, &scope)),
    );
    assert!(config.slab_directory.is_dir());
    let reactor = Rc::new(reactor);
    drive(
        &reactor,
        checkpoint
            .publish_async(
                vec![image],
                reactor.clone(),
                scope.clone(),
                1,
                0,
                1024 * 1024,
            )
            .unwrap(),
    );
    checkpoint.finish_snapshot();
    drive(&reactor, reactor.drain());
    let digests: Vec<_> = plan.workers.iter().map(|w| w.digest).collect();
    drop(slab);
    drop(plan);
    let restarted = discover(&config, Some("cache-"), 2).unwrap();
    for (file, bytes) in [(&first, 5 * segment), (&second, 6 * segment)] {
        validate_guards(
            bytes + 2 * GUARD_BYTES,
            restarted.workers[0].alignment,
            |buffer, offset| file.read_at(buffer, offset),
        )
        .unwrap();
    }
    assert_eq!(
        restarted
            .workers
            .iter()
            .map(|w| w.digest)
            .collect::<Vec<_>>(),
        digests
    );
    let (_, mut recovered) = crate::store::checkpoint::read_candidates(&config.slab_directory)
        .unwrap()
        .remove(0);
    let recovery =
        crate::store::checkpoint::Recovery::new(config.slab_directory.clone(), index, segments);
    let mut geometry = checkpoint.geometry().unwrap();
    geometry.layout_digest = restarted.workers[0].digest;
    recovery.configure_geometry(geometry).unwrap();
    futures::executor::block_on(recovery.install_shard(Some(recovered.shards.remove(0)))).unwrap();
    drop(restarted);
    command("umount", &[config.device_directory.as_os_str()]);
    drop(mount);
    fs::rename(
        root.join("dev/disk/by-id/cache-b"),
        root.join("dev/disk/by-id/cache-c"),
    )
    .unwrap();
    config.device_directory = root.join("dev");
    let changed = discover(&config, Some("cache-"), 2).unwrap();
    assert_ne!(changed.workers[0].digest, digests[0]);
    assert_ne!(changed.workers[1].digest, digests[1]);
    let (_, mut recovered) = crate::store::checkpoint::read_candidates(&config.slab_directory)
        .unwrap()
        .remove(0);
    geometry.layout_digest = changed.workers[0].digest;
    recovery.configure_geometry(geometry).unwrap();
    assert_eq!(
        futures::executor::block_on(recovery.install_shard(Some(recovered.shards.remove(0)))),
        Err(Error::CorruptRecord)
    );
    drop(changed);
    drop(first);
    drop(second);
}
