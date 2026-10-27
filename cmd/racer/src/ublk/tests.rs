//! Root-only end-to-end tests of ublk: the raw kernel interface on a plain
//! io_uring, and the manager and workers serving devices through the
//! reactor. They run only as root with `/dev/ublk-control` present and
//! `RACER_UBLK_TEST=1`.

use std::fs::{File, OpenOptions};
use std::os::fd::AsFd;
use std::os::unix::fs::FileExt;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc;
use std::thread;
use std::time::{Duration, Instant};

use io_uring::{IoUring, opcode, types};

use super::control::{Control, DevSpec};
use super::queue::{Action, IoDescriptors, Request, Span, io_cmd_bytes, spans};
use super::uapi::{
    Params, U_IO_COMMIT_AND_FETCH_REQ, U_IO_FETCH_REQ, UBLK_F_CMD_IOCTL_ENCODE, UBLK_F_USER_COPY,
    UBLK_IO_RES_ABORT, UBLK_IO_RES_OK, UBLK_S_DEV_LIVE, ublk_pos,
};

const MIB: u64 = 1 << 20;
const OBJECT_LEN: u64 = 40 * MIB + 123;
const PAGE_BYTES: u64 = 16 * MIB;
const DEPTH: u16 = 8;
const MAX_IO: u32 = 1 << 20;

fn pattern(at: u64) -> u8 {
    ((at % 251) as u8) ^ ((at >> 20) as u8)
}

fn expected(offset: u64, len: usize) -> Vec<u8> {
    (offset..offset + len as u64)
        .map(|at| if at < OBJECT_LEN { pattern(at) } else { 0 })
        .collect()
}

fn enabled() -> bool {
    // SAFETY: geteuid has no preconditions.
    let root = unsafe { libc::geteuid() } == 0;
    let ok = root
        && Path::new("/dev/ublk-control").exists()
        && std::env::var("RACER_UBLK_TEST").as_deref() == Ok("1");
    if !ok {
        eprintln!(
            "skipping ublk kernel test: needs root, /dev/ublk-control, and RACER_UBLK_TEST=1"
        );
    }
    ok
}

fn wait_for(path: &Path) -> bool {
    let deadline = Instant::now() + Duration::from_secs(10);
    while Instant::now() < deadline {
        if path.exists() {
            return true;
        }
        thread::sleep(Duration::from_millis(10));
    }
    false
}

/// Stops and deletes the device on every exit path.
struct Guard {
    control: Control,
    id: Option<u32>,
}

impl Drop for Guard {
    fn drop(&mut self) {
        if let Some(id) = self.id.take() {
            let _ = self.control.stop_dev(id);
            if let Err(err) = self.control.del_dev(id) {
                eprintln!("ublk test cleanup: {err}");
            }
        }
    }
}

/// Serves queue 0 until every tag is aborted. Returns the abort count.
fn serve(char_path: PathBuf, ready: mpsc::Sender<()>) -> std::io::Result<usize> {
    let file = OpenOptions::new().read(true).write(true).open(&char_path)?;
    let descs = IoDescriptors::map(file.as_fd(), 0, DEPTH)?;
    let mut ring = IoUring::new(u32::from(DEPTH) * 2)?;
    let fd = types::Fd(std::os::fd::AsRawFd::as_raw_fd(&file));
    let push = |ring: &mut IoUring, op: u32, tag: u16, result: i32| {
        let entry = opcode::UringCmd16::new(fd, op)
            .cmd(io_cmd_bytes(0, tag, result))
            .build()
            .user_data(u64::from(tag));
        // SAFETY: the command carries no pointers (USER_COPY uses addr 0).
        unsafe { ring.submission().push(&entry) }.expect("sq has room for every tag");
    };
    for tag in 0..DEPTH {
        push(&mut ring, U_IO_FETCH_REQ, tag, -1);
    }
    ring.submit()?;
    let _ = ready.send(());
    let mut aborted = 0_usize;
    while aborted < usize::from(DEPTH) {
        ring.submit_and_wait(1)?;
        let done: Vec<(u16, i32)> = ring
            .completion()
            .map(|cqe| (cqe.user_data() as u16, cqe.result()))
            .collect();
        for (tag, res) in done {
            if res == UBLK_IO_RES_ABORT {
                aborted += 1;
                continue;
            }
            assert_eq!(res, UBLK_IO_RES_OK, "unexpected fetch result for tag {tag}");
            let result = match Request::decode(descs.get(tag), MAX_IO) {
                Action::Complete(result) => result,
                Action::Read { offset, len } => {
                    let base = ublk_pos(0, tag, 0);
                    let mut result = len as i32;
                    for span in spans(offset, len, OBJECT_LEN, PAGE_BYTES) {
                        let (at, bytes) = match span {
                            Span::Page {
                                number,
                                page_offset,
                                len,
                                request_offset,
                            } => (
                                request_offset,
                                expected(number * PAGE_BYTES + page_offset, len as usize),
                            ),
                            Span::Zero {
                                request_offset,
                                len,
                            } => (request_offset, vec![0; len as usize]),
                        };
                        if file.write_all_at(&bytes, base + u64::from(at)).is_err() {
                            result = -libc::EIO;
                        }
                    }
                    result
                }
            };
            push(&mut ring, U_IO_COMMIT_AND_FETCH_REQ, tag, result);
        }
        ring.submit()?;
    }
    Ok(aborted)
}

#[test]
fn ublk_kernel_round_trip() {
    if !enabled() {
        return;
    }
    let dev_dir = Path::new("/dev");
    let mut control = Control::open(dev_dir).expect("open ublk-control");
    let info = control
        .add_dev(&DevSpec {
            id: None,
            queues: 1,
            depth: DEPTH,
            max_io_bytes: MAX_IO,
            flags: UBLK_F_CMD_IOCTL_ENCODE | UBLK_F_USER_COPY,
            cookie: 0x5241_4345,
        })
        .expect("ADD_DEV");
    let id = info.dev_id;
    eprintln!("ublk test: added device {id}, info {info:?}");
    let mut guard = Guard {
        control,
        id: Some(id),
    };
    let size = OBJECT_LEN.div_ceil(512) * 512;
    guard
        .control
        .set_params(id, &Params::read_only_basic(size, MAX_IO))
        .expect("SET_PARAMS");
    let fetched = guard.control.get_dev_info(id).expect("GET_DEV_INFO");
    assert_eq!(fetched.ublksrv_flags, 0x5241_4345);
    assert_eq!(fetched.nr_hw_queues, 1);

    let char_path = dev_dir.join(format!("ublkc{id}"));
    assert!(
        wait_for(&char_path),
        "{} did not appear",
        char_path.display()
    );
    let (ready_tx, ready_rx) = mpsc::channel();
    let server = thread::spawn(move || serve(char_path, ready_tx));
    ready_rx
        .recv_timeout(Duration::from_secs(10))
        .expect("queue thread ready");

    let stop = Arc::new(AtomicBool::new(false));
    let flag = Arc::clone(&stop);
    // SAFETY: getpid has no preconditions.
    let pid = unsafe { libc::getpid() };
    guard
        .control
        .start_dev(id, pid, &move || flag.load(Ordering::Relaxed))
        .expect("START_DEV");
    assert_eq!(
        guard.control.get_dev_info(id).expect("GET_DEV_INFO").state,
        UBLK_S_DEV_LIVE
    );

    let block_path = dev_dir.join(format!("ublkb{id}"));
    assert!(
        wait_for(&block_path),
        "{} did not appear",
        block_path.display()
    );
    let block = File::open(&block_path).expect("open block device");
    for (offset, len) in [
        (0_u64, 4096_usize),
        (12_345, 100_000),
        (PAGE_BYTES - 4096, 8192),
        (2 * PAGE_BYTES - 1000, 3 * MIB as usize),
        (size - 4096, 4096),
        (size - 512, 512),
    ] {
        let mut buf = vec![0xaa_u8; len];
        block.read_exact_at(&mut buf, offset).expect("pread");
        assert!(buf == expected(offset, len), "mismatch at {offset}+{len}");
    }
    let mut past = [0_u8; 16];
    assert_eq!(block.read_at(&mut past, size).expect("read at end"), 0);
    drop(block);

    let write_err = OpenOptions::new()
        .write(true)
        .open(&block_path)
        .and_then(|file| file.write_all_at(&[1; 512], 0))
        .expect_err("write to read-only device");
    eprintln!("ublk test: write error {write_err:?}");
    assert!(matches!(
        write_err.raw_os_error(),
        Some(libc::EROFS | libc::EACCES | libc::EPERM)
    ));

    guard.control.stop_dev(id).expect("STOP_DEV");
    let aborted = server
        .join()
        .expect("queue thread")
        .expect("queue thread io");
    assert_eq!(aborted, usize::from(DEPTH));
    guard.id = None;
    guard.control.del_dev(id).expect("DEL_DEV");
    drop(stop);
    assert!(
        wait_gone(&block_path),
        "{} still present",
        block_path.display()
    );
}

fn wait_gone(path: &Path) -> bool {
    let deadline = Instant::now() + Duration::from_secs(10);
    while Instant::now() < deadline {
        if !path.exists() {
            return true;
        }
        thread::sleep(Duration::from_millis(10));
    }
    false
}

/// Stub backend: the object is the `pattern` bytes, one version, no cache.
struct StubBackend;

#[derive(Clone)]
struct StubPage(std::rc::Rc<Vec<u8>>);

impl super::service::PageBytes for StubPage {
    fn bytes(&self) -> &[u8] {
        &self.0
    }
}

#[derive(Default)]
struct StubSource {
    pages: std::cell::RefCell<std::collections::HashMap<u64, StubPage>>,
}

impl super::service::PageSource for StubSource {
    type Page = StubPage;

    fn page(&self, number: u64) -> super::service::Local<'_, crate::error::Result<StubPage>> {
        let page = self
            .pages
            .borrow_mut()
            .entry(number)
            .or_insert_with(|| {
                let start = number * PAGE_BYTES;
                let end = (start + PAGE_BYTES).min(OBJECT_LEN);
                StubPage(std::rc::Rc::new(expected(start, (end - start) as usize)))
            })
            .clone();
        Box::pin(async move { Ok(page) })
    }
}

impl super::worker::Backend for StubBackend {
    type Source = StubSource;

    fn resolve(
        self: std::rc::Rc<Self>,
        _device: crate::config::UblkDeviceConfig,
    ) -> super::service::Local<'static, crate::error::Result<super::manager::Resolved>> {
        Box::pin(async move {
            Ok(super::manager::Resolved {
                cache: racer_control_wire::CacheId("stub".into()),
                etag: crate::model::StrongEtag::parse(b"\"v1\"").unwrap(),
                length: OBJECT_LEN,
            })
        })
    }

    fn source(
        &self,
        _device: &crate::config::UblkDeviceConfig,
        _resolved: &super::manager::Resolved,
    ) -> crate::error::Result<StubSource> {
        Ok(StubSource::default())
    }
}

/// Runs one worker: a real reactor serving queue `q` until `done`.
fn run_worker(q: u16, manager: Arc<super::manager::Manager>, done: Arc<AtomicBool>) {
    let reactor = crate::test_support::enrollment::reactor().expect("io_uring");
    let mut worker =
        super::worker::UblkWorker::new(q, manager, std::rc::Rc::new(StubBackend), reactor.clone());
    let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
    while !(done.load(Ordering::Relaxed) && worker.is_idle()) {
        worker.poll(&mut cx);
        reactor.poll_budgeted(64).expect("reactor poll");
        reactor
            .wait(Duration::from_millis(2))
            .expect("reactor wait");
    }
    // Let canceled commands reach their fence before the reactor drops.
    let deadline = Instant::now() + Duration::from_secs(5);
    while reactor.in_flight() > 0 && Instant::now() < deadline {
        reactor.poll_budgeted(64).expect("reactor poll");
        reactor
            .wait(Duration::from_millis(2))
            .expect("reactor wait");
    }
}

/// The dataplane queue path end to end: the manager's control thread adds
/// and starts a device, two worker threads serve its queues through the
/// reactor, and reads through `/dev/ublkbN` match the object.
#[test]
fn ublk_worker_queues_serve_reads() {
    if !enabled() {
        return;
    }
    let config = crate::config::UblkConfig {
        devices: vec![crate::config::UblkDeviceConfig {
            cache: "stub".into(),
            key: crate::model::CacheKey([3; 32]),
            etag: None,
            id: None,
        }],
        queue_depth: DEPTH,
        max_io_bytes: MAX_IO,
    };
    let manager = Arc::new(super::manager::Manager::new(
        &config,
        2,
        Path::new("/dev"),
        Duration::from_secs(10),
        Duration::from_secs(10),
    ));
    let done = Arc::new(AtomicBool::new(false));
    let workers: Vec<_> = (0..2)
        .map(|q| {
            let manager = manager.clone();
            let done = done.clone();
            thread::spawn(move || run_worker(q, manager, done))
        })
        .collect();
    let control = {
        let manager = manager.clone();
        thread::spawn(move || manager.run())
    };
    let deadline = Instant::now() + Duration::from_secs(10);
    let id = loop {
        if let Some(id) = manager.dev_id(0) {
            break id;
        }
        assert!(Instant::now() < deadline, "device was not added");
        thread::sleep(Duration::from_millis(10));
    };
    eprintln!("ublk worker test: device {id}");
    let block_path = Path::new("/dev").join(format!("ublkb{id}"));
    assert!(
        wait_for(&block_path),
        "{} did not appear",
        block_path.display()
    );
    let size = OBJECT_LEN.div_ceil(512) * 512;
    let block = loop {
        // The node can appear just before START_DEV returns.
        match File::open(&block_path) {
            Ok(block) => break block,
            Err(_) if Instant::now() < deadline + Duration::from_secs(5) => {
                thread::sleep(Duration::from_millis(10))
            }
            Err(error) => panic!("open {}: {error}", block_path.display()),
        }
    };
    // O_DIRECT is not needed: each range is read once and compared.
    for (offset, len) in [
        (0_u64, 4096_usize),
        (12_345, 100_000),
        (PAGE_BYTES - 4096, 8192),
        (2 * PAGE_BYTES - 1000, 3 * MIB as usize),
        (size - 4096, 4096),
        (size - 512, 512),
    ] {
        let mut buf = vec![0xaa_u8; len];
        block.read_exact_at(&mut buf, offset).expect("pread");
        assert!(buf == expected(offset, len), "mismatch at {offset}+{len}");
    }
    let mut past = [0_u8; 16];
    assert_eq!(block.read_at(&mut past, size).expect("read at end"), 0);
    drop(block);

    manager.request_stop();
    control.join().expect("control thread");
    assert!(manager.control_done());
    done.store(true, Ordering::Relaxed);
    for worker in workers {
        worker.join().expect("worker thread");
    }
    assert!(
        wait_gone(&block_path),
        "{} still present",
        block_path.display()
    );
    let char_path = Path::new("/dev").join(format!("ublkc{id}"));
    assert!(
        wait_gone(&char_path),
        "{} still present",
        char_path.display()
    );
}

/// Bytes of the directory-backed object: each page holds its page number.
fn directory_expected(length: u64, offset: u64, len: usize) -> Vec<u8> {
    (offset..offset + len as u64)
        .map(|at| {
            if at < length {
                (at / PAGE_BYTES) as u8
            } else {
                0
            }
        })
        .collect()
}

/// The full production read path behind a device: one worker serves the
/// only queue through `DirectorySource`, `WorkerDirectory`, and a scripted
/// origin, and reads through `/dev/ublkbN` match the origin object.
#[test]
fn ublk_device_serves_the_directory_read_path() {
    use super::directory::tests::{Fixture, NAME, device};

    if !enabled() {
        return;
    }
    const LENGTH: u64 = 2 * PAGE_BYTES + 5000;
    let config = crate::config::UblkConfig {
        devices: vec![device(NAME, None)],
        queue_depth: DEPTH,
        max_io_bytes: MAX_IO,
    };
    let manager = Arc::new(super::manager::Manager::new(
        &config,
        1,
        Path::new("/dev"),
        Duration::from_secs(10),
        Duration::from_secs(10),
    ));
    let done = Arc::new(AtomicBool::new(false));
    let worker = {
        let manager = manager.clone();
        let done = done.clone();
        thread::spawn(move || {
            let fixture = Fixture::object(LENGTH);
            let mut ublk = super::worker::UblkWorker::new(
                0,
                manager,
                fixture.backend.clone(),
                fixture.reactor.clone(),
            );
            let _queue = fixture.worker.drivers.enter();
            let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
            while !(done.load(Ordering::Relaxed) && ublk.is_idle()) {
                ublk.poll(&mut cx);
                fixture.worker.poll(&mut cx);
                fixture.reactor.poll_budgeted(64).expect("reactor poll");
                fixture
                    .reactor
                    .wait(Duration::from_millis(1))
                    .expect("reactor wait");
            }
            let deadline = Instant::now() + Duration::from_secs(5);
            while fixture.reactor.in_flight() > 0 && Instant::now() < deadline {
                fixture.worker.poll(&mut cx);
                fixture.reactor.poll_budgeted(64).expect("reactor poll");
                fixture
                    .reactor
                    .wait(Duration::from_millis(1))
                    .expect("reactor wait");
            }
        })
    };
    let control = {
        let manager = manager.clone();
        thread::spawn(move || manager.run())
    };
    let deadline = Instant::now() + Duration::from_secs(20);
    let id = loop {
        if let Some(id) = manager.dev_id(0) {
            break id;
        }
        assert!(Instant::now() < deadline, "device was not added");
        thread::sleep(Duration::from_millis(10));
    };
    eprintln!("ublk directory test: device {id}");
    let block_path = Path::new("/dev").join(format!("ublkb{id}"));
    assert!(
        wait_for(&block_path),
        "{} did not appear",
        block_path.display()
    );
    let block = loop {
        match File::open(&block_path) {
            Ok(block) => break block,
            Err(_) if Instant::now() < deadline => thread::sleep(Duration::from_millis(10)),
            Err(error) => panic!("open {}: {error}", block_path.display()),
        }
    };
    let size = LENGTH.div_ceil(512) * 512;
    for (offset, len) in [
        (0_u64, 4096_usize),
        (PAGE_BYTES - 4096, 8192),
        (2 * PAGE_BYTES - 1000, 6000),
        (size - 512, 512),
    ] {
        let mut buf = vec![0xaa_u8; len];
        block.read_exact_at(&mut buf, offset).expect("pread");
        assert!(
            buf == directory_expected(LENGTH, offset, len),
            "mismatch at {offset}+{len}"
        );
    }
    drop(block);

    manager.request_stop();
    control.join().expect("control thread");
    assert!(manager.control_done());
    done.store(true, Ordering::Relaxed);
    worker.join().expect("worker thread");
    assert!(
        wait_gone(&block_path),
        "{} still present",
        block_path.display()
    );
}

/// STOP_DEV on a device that was never started completes every pending
/// FETCH with `UBLK_IO_RES_ABORT`, and ASYNC_CANCEL cannot cancel one.
/// The manager relies on both: it stops never-started devices so their
/// queues finish, rather than relying on the reactor's cancel.
#[test]
fn ublk_stop_before_start_aborts_fetches() {
    if !enabled() {
        return;
    }
    let dev_dir = Path::new("/dev");
    let mut control = Control::open(dev_dir).expect("open ublk-control");
    let info = control
        .add_dev(&DevSpec {
            id: None,
            queues: 1,
            depth: DEPTH,
            max_io_bytes: MAX_IO,
            flags: UBLK_F_CMD_IOCTL_ENCODE | UBLK_F_USER_COPY,
            cookie: 0x5241_4346,
        })
        .expect("ADD_DEV");
    let id = info.dev_id;
    assert_ne!(id, 0, "the test must never touch device 0");
    let mut guard = Guard {
        control,
        id: Some(id),
    };
    guard
        .control
        .set_params(
            id,
            &Params::read_only_basic(OBJECT_LEN.div_ceil(512) * 512, MAX_IO),
        )
        .expect("SET_PARAMS");
    let char_path = dev_dir.join(format!("ublkc{id}"));
    assert!(
        wait_for(&char_path),
        "{} did not appear",
        char_path.display()
    );
    let file = OpenOptions::new()
        .read(true)
        .write(true)
        .open(&char_path)
        .expect("open char device");
    let _descs = IoDescriptors::map(file.as_fd(), 0, DEPTH).expect("map descriptors");
    let mut ring = IoUring::new(u32::from(DEPTH) * 2).expect("ring");
    let fd = types::Fd(std::os::fd::AsRawFd::as_raw_fd(&file));
    for tag in 0..DEPTH {
        let entry = opcode::UringCmd16::new(fd, U_IO_FETCH_REQ)
            .cmd(io_cmd_bytes(0, tag, -1))
            .build()
            .user_data(u64::from(tag));
        // SAFETY: the command carries no pointers.
        unsafe { ring.submission().push(&entry) }.expect("sq room");
    }
    ring.submit().expect("submit fetches");
    let cancel = opcode::AsyncCancel::new(0).build().user_data(1000);
    // SAFETY: ASYNC_CANCEL carries no pointers.
    unsafe { ring.submission().push(&cancel) }.expect("sq room");
    ring.submit_and_wait(1).expect("cancel");
    let mut aborted = 0;
    let mut cancel_res = None;
    collect_fetches(&mut ring, &mut aborted, &mut cancel_res);
    assert_eq!(
        cancel_res,
        Some(-libc::ENOENT),
        "a pending FETCH is not cancelable"
    );
    assert_eq!(aborted, 0, "no FETCH completes before STOP_DEV");
    guard
        .control
        .stop_dev(id)
        .expect("STOP_DEV before START_DEV");
    let deadline = Instant::now() + Duration::from_secs(10);
    while aborted < usize::from(DEPTH) {
        assert!(
            Instant::now() < deadline,
            "STOP_DEV left FETCH commands pending"
        );
        let ts = types::Timespec::from(Duration::from_millis(100));
        let args = types::SubmitArgs::new().timespec(&ts);
        let _ = ring.submitter().submit_with_args(1, &args);
        collect_fetches(&mut ring, &mut aborted, &mut cancel_res);
    }
    guard.id = None;
    guard.control.del_dev(id).expect("DEL_DEV");
}

fn collect_fetches(ring: &mut IoUring, aborted: &mut usize, cancel_res: &mut Option<i32>) {
    for cqe in ring.completion() {
        if cqe.user_data() == 1000 {
            *cancel_res = Some(cqe.result());
        } else {
            assert_eq!(cqe.result(), UBLK_IO_RES_ABORT, "tag {}", cqe.user_data());
            *aborted += 1;
        }
    }
}
