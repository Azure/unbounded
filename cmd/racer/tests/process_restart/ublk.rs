//! Root-only ublk coverage through the real executable: configuration, the
//! multi-worker application startup, reads served by more than one hardware
//! queue, and SIGTERM drain with device removal. Runs only as root with
//! `/dev/ublk-control` present and `RACER_UBLK_TEST=1`.
//!
//! It never touches a ublk device it did not create: the dataplane picks an
//! automatic id and only the id it logs is inspected.

use super::*;
use std::os::unix::fs::{FileExt, OpenOptionsExt};

const SECTOR: usize = 512;
const CHUNK: usize = 256 * 1024;
const READERS: usize = 4;

fn enabled() -> bool {
    // SAFETY: geteuid has no preconditions.
    let ok = unsafe { libc::geteuid() } == 0
        && Path::new("/dev/ublk-control").exists()
        && std::env::var("RACER_UBLK_TEST").as_deref() == Ok("1");
    if !ok {
        eprintln!(
            "skipping ublk kernel test: needs root, /dev/ublk-control, and RACER_UBLK_TEST=1"
        );
    }
    ok
}

/// Sector-aligned buffer for O_DIRECT reads.
struct Aligned {
    ptr: *mut u8,
    layout: std::alloc::Layout,
}
impl Aligned {
    fn new(len: usize) -> Self {
        let layout = std::alloc::Layout::from_size_align(len, 4096).unwrap();
        // SAFETY: the layout has a nonzero size.
        let ptr = unsafe { std::alloc::alloc_zeroed(layout) };
        assert!(!ptr.is_null());
        Self { ptr, layout }
    }
    fn bytes(&mut self) -> &mut [u8] {
        // SAFETY: ptr owns layout.size() initialized bytes.
        unsafe { std::slice::from_raw_parts_mut(self.ptr, self.layout.size()) }
    }
}
impl Drop for Aligned {
    fn drop(&mut self) {
        // SAFETY: allocated with this layout in new.
        unsafe { std::alloc::dealloc(self.ptr, self.layout) };
    }
}

fn expected(offset: u64) -> u8 {
    if offset < LENGTH { byte(offset) } else { 0 }
}

fn pin_to(cpu: usize) {
    // SAFETY: cpu_set_t is plain data; the calls only touch the local set.
    unsafe {
        let mut set: libc::cpu_set_t = std::mem::zeroed();
        libc::CPU_SET(cpu, &mut set);
        assert_eq!(
            libc::sched_setaffinity(0, std::mem::size_of::<libc::cpu_set_t>(), &set),
            0,
            "pin reader to cpu {cpu}: {}",
            io::Error::last_os_error()
        );
    }
}

/// CPUs this process may run on.
fn allowed_cpus() -> Vec<usize> {
    // SAFETY: cpu_set_t is plain data; sched_getaffinity fills it.
    unsafe {
        let mut set: libc::cpu_set_t = std::mem::zeroed();
        assert_eq!(
            libc::sched_getaffinity(0, std::mem::size_of::<libc::cpu_set_t>(), &mut set),
            0
        );
        (0..libc::CPU_SETSIZE as usize)
            .filter(|cpu| libc::CPU_ISSET(*cpu, &set))
            .collect()
    }
}

/// One allowed CPU per hardware queue of device `id`. blk-mq maps a
/// submitting CPU to the queue whose `cpu_list` names it, so readers pinned
/// to these CPUs use different queues and so different I/O workers.
fn queue_cpus(id: u32, allowed: &[usize]) -> Vec<usize> {
    let mut queues: Vec<_> = fs::read_dir(format!("/sys/block/ublkb{id}/mq"))
        .unwrap()
        .map(|entry| entry.unwrap().path())
        .collect();
    queues.sort();
    queues
        .iter()
        .filter_map(|queue| {
            let list = fs::read_to_string(queue.join("cpu_list")).unwrap();
            list.split(',')
                .flat_map(|item| {
                    let item = item.trim();
                    let (first, last) = item.split_once('-').unwrap_or((item, item));
                    first.parse::<usize>().unwrap()..=last.parse::<usize>().unwrap()
                })
                .find(|cpu| allowed.contains(cpu))
        })
        .collect()
}

fn device_size(file: &fs::File) -> u64 {
    let mut size = 0_u64;
    // BLKGETSIZE64 = _IOR(0x12, 114, size_t).
    // SAFETY: the ioctl writes one u64 into size.
    let rc = unsafe { libc::ioctl(file.as_raw_fd(), 0x8008_1272, &mut size) };
    assert_eq!(rc, 0, "BLKGETSIZE64: {}", io::Error::last_os_error());
    size
}

fn open_direct(path: &Path) -> fs::File {
    fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_DIRECT)
        .open(path)
        .unwrap_or_else(|error| panic!("open {}: {error}", path.display()))
}

/// Reads chunks `index, index + stride, ...` of the device on `cpu` and
/// checks every byte. The stripes together cover the whole device.
fn verify_stripe(path: &Path, size: u64, cpu: usize, index: usize, stride: usize) {
    pin_to(cpu);
    let file = open_direct(path);
    let mut buffer = Aligned::new(CHUNK);
    let mut offset = (index * CHUNK) as u64;
    while offset < size {
        let len = CHUNK.min((size - offset) as usize);
        let read = file
            .read_at(&mut buffer.bytes()[..len], offset)
            .unwrap_or_else(|error| panic!("read at {offset}: {error}"));
        assert_eq!(read, len, "short read at {offset}");
        for (i, actual) in buffer.bytes()[..len].iter().enumerate() {
            let at = offset + i as u64;
            assert_eq!(*actual, expected(at), "byte {at} on cpu {cpu}");
        }
        offset += (stride * CHUNK) as u64;
    }
}

/// Deletes the test's own device if the dataplane did not, for example after
/// a failed assertion killed it. Declared before the process, so it runs
/// after the dataplane is killed and its character device released.
///
/// The device is deleted only if it is provably the dataplane's: the
/// character device is opened first, which pins the id so it cannot be
/// freed and reused, then GET_DEV_INFO must report the cookie recorded
/// while the dataplane was alive and serving it (see [`owned_cookie`]).
/// Anything else is logged and left alone. Disarmed once the dataplane's
/// own removal is confirmed.
struct Leftover(Option<(u32, u64)>);
impl Drop for Leftover {
    fn drop(&mut self) {
        let Some((id, cookie)) = self.0 else { return };
        match delete_if_owned(id, cookie) {
            Ok(true) => eprintln!("ublk test cleanup: deleted leftover device {id}"),
            Ok(false) => {}
            Err(error) => eprintln!("ublk test cleanup: left device {id} alone: {error}"),
        }
    }
}

/// `_IOR('u', 0x02, struct ublksrv_ctrl_cmd)`.
const U_CMD_GET_DEV_INFO: u32 = 0x8020_7502;

/// `struct ublksrv_ctrl_dev_info` of device `id`.
fn dev_info(id: u32) -> io::Result<[u8; 64]> {
    let mut info = [0_u8; 64];
    control_command(U_CMD_GET_DEV_INFO, id, &mut info)?;
    Ok(info)
}

/// The ownership cookie (`ublksrv_flags`) of device `id`, which must be
/// served by `pid`. Called while the dataplane is alive and holds the
/// device, so the id cannot have been reused. The kernel clears
/// `ublksrv_pid` once the server exits, so the pid cannot identify the
/// device later, but the cookie stays.
fn owned_cookie(id: u32, pid: i32) -> u64 {
    let info = dev_info(id).unwrap_or_else(|error| panic!("GET_DEV_INFO {id}: {error}"));
    let server = i32::from_ne_bytes(info[16..20].try_into().unwrap());
    assert_eq!(server, pid, "device {id} is served by the dataplane");
    u64::from_ne_bytes(info[32..40].try_into().unwrap())
}

/// Deletes device `id` if its cookie is `cookie`, holding its character
/// device for the whole check and delete. Returns false if it is gone.
fn delete_if_owned(id: u32, cookie: u64) -> io::Result<bool> {
    // _IOWR/_IOR('u', nr, struct ublksrv_ctrl_cmd).
    const U_CMD_STOP_DEV: u32 = 0xc020_7507;
    const U_CMD_DEL_DEV_ASYNC: u32 = 0x8020_7514;
    // The open is exclusive, and the killed dataplane's descriptor can take
    // a moment to be released.
    let deadline = Instant::now() + Duration::from_secs(10);
    let char = loop {
        match fs::OpenOptions::new()
            .read(true)
            .write(true)
            .open(format!("/dev/ublkc{id}"))
        {
            Ok(file) => break file,
            Err(error) if error.kind() == io::ErrorKind::NotFound => return Ok(false),
            Err(error)
                if error.raw_os_error() == Some(libc::EBUSY) && Instant::now() < deadline =>
            {
                thread::sleep(Duration::from_millis(50));
            }
            Err(error) => return Err(error),
        }
    };
    // struct ublksrv_ctrl_dev_info: ublksrv_flags at offset 32.
    let info = dev_info(id)?;
    let found = u64::from_ne_bytes(info[32..40].try_into().unwrap());
    if found != cookie {
        return Err(io::Error::other(format!(
            "cookie {found:#x} is not the dataplane's {cookie:#x}"
        )));
    }
    control_command(U_CMD_STOP_DEV, id, &mut [])?;
    // DEL_DEV_ASYNC does not wait for the id to be freed, so it returns
    // while the character device is still held. Kernels without it get an
    // error here and the device is left alone.
    control_command(U_CMD_DEL_DEV_ASYNC, id, &mut [])?;
    drop(char);
    Ok(true)
}

/// One control command for `id` over a fresh `/dev/ublk-control`, with
/// `buffer` as its payload.
fn control_command(op: u32, id: u32, buffer: &mut [u8]) -> io::Result<()> {
    use io_uring::{IoUring, cqueue, opcode, squeue, types};
    let control = fs::OpenOptions::new()
        .read(true)
        .write(true)
        .open("/dev/ublk-control")?;
    let mut ring = IoUring::<squeue::Entry128, cqueue::Entry>::builder().build(2)?;
    // struct ublksrv_ctrl_cmd: dev_id, queue_id = -1, len, addr, then zeros.
    let mut cmd = [0_u8; 80];
    cmd[..4].copy_from_slice(&id.to_ne_bytes());
    cmd[4..6].copy_from_slice(&u16::MAX.to_ne_bytes());
    cmd[6..8].copy_from_slice(&(buffer.len() as u16).to_ne_bytes());
    cmd[8..16].copy_from_slice(&(buffer.as_mut_ptr() as u64).to_ne_bytes());
    let entry = opcode::UringCmd80::new(types::Fd(control.as_raw_fd()), op)
        .cmd(cmd)
        .build();
    // SAFETY: buffer outlives the command, which completes before return.
    unsafe { ring.submission().push(&entry) }.map_err(io::Error::other)?;
    ring.submit_and_wait(1)?;
    let result = ring
        .completion()
        .next()
        .map_or(-libc::EIO, |cqe| cqe.result());
    if result < 0 {
        return Err(io::Error::from_raw_os_error(-result));
    }
    Ok(())
}

fn started_id(process: &Process) -> Option<u32> {
    process.logs().lines().find_map(|line| {
        let rest = line.split("state=started id=").nth(1)?;
        let (id, rest) = rest.split_once(' ')?;
        rest.starts_with(&format!("path=/dev/ublkb{id} "))
            .then(|| id.parse().ok())?
    })
}

fn wait_until(what: &str, mut done: impl FnMut() -> bool) {
    let deadline = Instant::now() + TIMEOUT;
    while !done() {
        assert!(Instant::now() < deadline, "timed out waiting for {what}");
        thread::sleep(Duration::from_millis(20));
    }
}

#[test]
fn ublk_device_served_by_multiple_workers_and_removed_on_sigterm() {
    if !enabled() {
        return;
    }
    let cpus = allowed_cpus();
    assert!(cpus.len() >= 2, "needs two CPUs for two I/O workers");
    let scratch = Scratch::new();
    // Declared before the process so it runs after the process is gone.
    let mut leftover = Leftover(None);
    let control = control::Control::start(&scratch.0);
    let devices = format!(r#"[{{"cache":"{NAME}","key":"{}"}}]"#, "ab".repeat(32));
    let (mut process, mut origins) = Process::launch(
        &scratch,
        &control,
        0,
        None,
        &[
            // Two I/O workers plus one crypto thread.
            ("RACER_MAX_THREADS", "3".into()),
            ("RACER_REQUEST_CONTEXT_BYTES", (32 << 20).to_string()),
            ("RACER_DEVICE_DIRECTORY", "/dev".into()),
            ("RACER_CLIENT_CONNECTIONS", "24".into()),
            ("RACER_UBLK_DEVICES", devices),
            ("RACER_UBLK_QUEUE_DEPTH", "16".into()),
            ("RACER_UBLK_MAX_IO_BYTES", (128 * 1024).to_string()),
        ],
    );
    let _origin = origins.remove(0);
    let mut id = None;
    wait_until("the ublk device to start", || {
        process.assert_running();
        id = started_id(&process);
        id.is_some()
    });
    let id = id.unwrap();
    leftover.0 = Some((id, owned_cookie(id, process.child.id() as i32)));
    let block = PathBuf::from(format!("/dev/ublkb{id}"));
    let char = PathBuf::from(format!("/dev/ublkc{id}"));
    wait_until("the block node", || block.exists());

    assert!(
        process
            .logs()
            .contains("stage=worker-plan state=final io_workers=2 "),
        "two I/O workers: {}",
        process.logs()
    );
    let cpus = queue_cpus(id, &cpus);
    assert!(
        cpus.len() >= 2,
        "one hardware queue per I/O worker, each with a usable CPU: {cpus:?}"
    );
    assert_eq!(
        fs::read_to_string(format!("/sys/block/ublkb{id}/ro"))
            .unwrap()
            .trim(),
        "1"
    );

    let size = device_size(&open_direct(&block));
    assert_eq!(size, LENGTH.next_multiple_of(SECTOR as u64), "device size");

    // Full verification from several CPUs so more than one queue serves reads.
    let readers: Vec<_> = (0..READERS)
        .map(|index| {
            let (block, cpu) = (block.clone(), cpus[index % cpus.len()]);
            thread::spawn(move || verify_stripe(&block, size, cpu, index, READERS))
        })
        .collect();
    for reader in readers {
        reader.join().expect("reader");
    }
    // A single read that crosses the first page boundary and one of the tail.
    let file = open_direct(&block);
    let mut buffer = Aligned::new(64 * 1024);
    for offset in [P - 32 * 1024, size - 64 * 1024] {
        let read = file.read_at(buffer.bytes(), offset).unwrap();
        assert_eq!(read, 64 * 1024);
        for (i, actual) in buffer.bytes().iter().enumerate() {
            assert_eq!(
                *actual,
                expected(offset + i as u64),
                "byte {}",
                offset + i as u64
            );
        }
    }

    // Writes are refused at open or at write time.
    match fs::OpenOptions::new().write(true).open(&block) {
        Err(error) => assert!(
            matches!(
                error.raw_os_error(),
                Some(libc::EROFS | libc::EPERM | libc::EACCES)
            ),
            "write open: {error}"
        ),
        Ok(writer) => {
            let error = writer.write_at(&[1; SECTOR], 0).unwrap_err();
            assert!(
                matches!(error.raw_os_error(), Some(libc::EROFS | libc::EPERM)),
                "write: {error}"
            );
        }
    }

    // SIGTERM while readers on every CPU keep reads in flight. Reads may fail
    // once the device stops; data they do return must still be correct.
    let stop = Arc::new(AtomicBool::new(false));
    let completed = Arc::new(AtomicUsize::new(0));
    let busy: Vec<_> = cpus
        .iter()
        .take(READERS)
        .map(|&cpu| {
            let (block, stop, completed) = (block.clone(), stop.clone(), completed.clone());
            thread::spawn(move || {
                pin_to(cpu);
                let Ok(file) = fs::OpenOptions::new()
                    .read(true)
                    .custom_flags(libc::O_DIRECT)
                    .open(&block)
                else {
                    return;
                };
                let mut buffer = Aligned::new(CHUNK);
                let mut offset = (cpu * CHUNK) as u64 % size;
                while !stop.load(Ordering::Acquire) {
                    offset = (offset - offset % SECTOR as u64) % (size - CHUNK as u64);
                    match file.read_at(buffer.bytes(), offset) {
                        Ok(read) => {
                            for (i, actual) in buffer.bytes()[..read].iter().enumerate() {
                                assert_eq!(*actual, expected(offset + i as u64));
                            }
                            completed.fetch_add(1, Ordering::Relaxed);
                        }
                        Err(_) => return,
                    }
                    offset += (7 * CHUNK) as u64;
                }
            })
        })
        .collect();
    wait_until("reads in flight", || {
        completed.load(Ordering::Relaxed) >= 2 * busy.len()
    });
    let started = Instant::now();
    let status = process.stop(libc::SIGTERM);
    let elapsed = started.elapsed();
    stop.store(true, Ordering::Release);
    for reader in busy {
        reader.join().expect("in-flight reader");
    }
    assert!(
        status.success(),
        "graceful exit: {status:?}\n{}",
        process.logs()
    );
    assert!(elapsed < Duration::from_secs(20), "drain took {elapsed:?}");
    let logs = process.logs();
    assert!(logs.contains(&format!("id={id}")), "{logs}");
    wait_until("ublk nodes to disappear", || {
        !block.exists() && !char.exists()
    });
    assert!(
        !Path::new(&format!("/sys/block/ublkb{id}")).exists(),
        "block device still registered"
    );
    // Removal is confirmed; the id may now belong to someone else.
    leftover.0 = None;
}
