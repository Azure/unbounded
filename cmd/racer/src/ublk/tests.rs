//! Root-only end-to-end test of the ublk kernel interface, without the
//! reactor. Runs only as root with `/dev/ublk-control` present and
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
