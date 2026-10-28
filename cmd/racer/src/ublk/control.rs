//! Blocking ublk control channel over `ublk-control`.
//!
//! A [`Control`] belongs to one dedicated control thread. Control commands
//! need 128-byte SQEs, so it owns its own SQE128 ring. Each method sends one
//! command (or a short retry sequence) and waits for its completion.

use std::ffi::CString;
use std::fmt;
use std::io;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};
use std::os::unix::ffi::OsStrExt;
use std::path::Path;
use std::ptr::NonNull;
use std::time::{Duration, Instant};

use io_uring::{IoUring, cqueue, opcode, squeue, types};

use super::uapi::{
    CTRL_CMD_SIZE, CtrlCmd, DEV_INFO_SIZE, DevInfo, ERESTARTSYS, PARAMS_SIZE, Params,
    U_CMD_ADD_DEV, U_CMD_DEL_DEV, U_CMD_DEL_DEV_ASYNC, U_CMD_GET_DEV_INFO, U_CMD_SET_PARAMS,
    U_CMD_START_DEV, U_CMD_STOP_DEV,
};

/// Size of the payload page. It holds the largest payload (`Params`).
const PAYLOAD_LEN: usize = 4096;

/// How many times START_DEV is resent after an interrupted wait.
const START_RETRIES: usize = 16;

/// How long START_DEV waits between `should_stop` checks.
const START_POLL: Duration = Duration::from_millis(100);

/// Default bound on waiting for one control command.
pub(crate) const DEFAULT_TIMEOUT: Duration = Duration::from_secs(30);

/// User data of a command and of its ASYNC_CANCEL.
const CMD_USER_DATA: u64 = 1;
const CANCEL_USER_DATA: u64 = 2;

/// Retry limits for a pinned DEL_DEV: how many times DEL_DEV is resent
/// after a cancel that came before the driver ran, and how many cancels one
/// DEL_DEV gets.
const PINNED_SUBMITS: u32 = 8;
const PINNED_CANCELS: u32 = 16;

/// A ublk control failure.
#[derive(Debug)]
pub(crate) enum UblkError {
    /// A system call on the control channel failed.
    Io { op: &'static str, source: io::Error },
    /// The kernel completed a command with a negative errno.
    Kernel { op: &'static str, errno: i32 },
    /// An earlier command's completion was not seen. The kernel may still own
    /// the payload buffer, so the channel refuses further commands.
    Poisoned,
    /// A command did not complete in time. Its outcome is unknown and the
    /// channel is now poisoned.
    TimedOut { op: &'static str },
    /// A command may still be outstanding for another reason. The channel is
    /// now poisoned.
    Unknown {
        op: &'static str,
        reason: &'static str,
    },
    /// START_DEV was canceled because the caller asked to stop.
    Canceled,
    /// START_DEV was interrupted more than the retry limit.
    Interrupted,
}

impl UblkError {
    /// The kernel errno, if the kernel rejected the command.
    pub(crate) fn errno(&self) -> Option<i32> {
        match self {
            Self::Kernel { errno, .. } => Some(*errno),
            Self::Io { source, .. } => source.raw_os_error(),
            _ => None,
        }
    }
}

impl fmt::Display for UblkError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Io { op, source } => write!(f, "ublk {op}: {source}"),
            Self::Kernel { op, errno } => {
                write!(f, "ublk {op}: {}", io::Error::from_raw_os_error(*errno))
            }
            Self::Poisoned => f.write_str("ublk control channel unusable after a lost completion"),
            Self::TimedOut { op } => write!(f, "ublk {op}: timed out, outcome unknown"),
            Self::Unknown { op, reason } => write!(f, "ublk {op}: outcome unknown: {reason}"),
            Self::Canceled => f.write_str("ublk START_DEV canceled"),
            Self::Interrupted => f.write_str("ublk START_DEV interrupted too many times"),
        }
    }
}

impl std::error::Error for UblkError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::Io { source, .. } => Some(source),
            _ => None,
        }
    }
}

pub(crate) type Result<T> = std::result::Result<T, UblkError>;

fn kernel(op: &'static str, res: i32) -> Result<i32> {
    if res < 0 {
        Err(UblkError::Kernel { op, errno: -res })
    } else {
        Ok(res)
    }
}

/// Whether a failed DEL_DEV_ASYNC should be retried as a pinned DEL_DEV.
/// Any rejection but "no such device" may mean the kernel lacks
/// DEL_DEV_ASYNC: Linux 6.8 answers unknown commands with `-ENOTSUPP`
/// (524), others with `-EOPNOTSUPP` or `-EINVAL`.
fn del_needs_fallback(res: i32) -> bool {
    res < 0 && res != -libc::ENODEV
}

/// Parameters for ADD_DEV.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct DevSpec {
    /// Device id, or `None` for a kernel-chosen id.
    pub id: Option<u32>,
    pub queues: u16,
    pub depth: u16,
    pub max_io_bytes: u32,
    /// `UBLK_F_*` flags.
    pub flags: u64,
    /// Stored in `ublksrv_flags`; the driver returns it from GET_DEV_INFO and
    /// never interprets it.
    pub cookie: u64,
}

/// An anonymous page mapping whose address is handed to the kernel.
struct Payload(NonNull<u8>);

impl Payload {
    fn new() -> io::Result<Self> {
        // SAFETY: anonymous private mapping with no address hint; the result is
        // checked before use.
        let ptr = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                PAYLOAD_LEN,
                libc::PROT_READ | libc::PROT_WRITE,
                libc::MAP_PRIVATE | libc::MAP_ANONYMOUS,
                -1,
                0,
            )
        };
        if ptr == libc::MAP_FAILED {
            return Err(io::Error::last_os_error());
        }
        NonNull::new(ptr.cast())
            .map(Self)
            .ok_or_else(|| io::Error::other("mmap returned null"))
    }

    fn as_mut_slice(&mut self) -> &mut [u8] {
        // SAFETY: the mapping is PAYLOAD_LEN bytes, readable and writable, and
        // lives until `unmap`. `&mut self` gives exclusive access from Rust;
        // the kernel only touches it while a command is outstanding, which is
        // never while this borrow is live.
        unsafe { std::slice::from_raw_parts_mut(self.0.as_ptr(), PAYLOAD_LEN) }
    }

    fn addr(&self) -> u64 {
        self.0.as_ptr() as u64
    }

    fn unmap(self) {
        // SAFETY: the mapping came from mmap with PAYLOAD_LEN and is unmapped once.
        unsafe { libc::munmap(self.0.as_ptr().cast(), PAYLOAD_LEN) };
    }
}

/// Blocking ublk control channel.
pub(crate) struct Control {
    ring: IoUring<squeue::Entry128, cqueue::Entry>,
    fd: OwnedFd,
    payload: Option<Payload>,
    poisoned: bool,
    /// Bound on waiting for one command.
    timeout: Duration,
}

impl Control {
    /// Opens `<device_dir>/ublk-control`.
    pub(crate) fn open(device_dir: &Path) -> Result<Self> {
        let path = device_dir.join("ublk-control");
        let c_path = CString::new(path.as_os_str().as_bytes()).map_err(|_| UblkError::Io {
            op: "open",
            source: io::ErrorKind::InvalidInput.into(),
        })?;
        // SAFETY: c_path is a valid NUL-terminated string.
        let raw = unsafe { libc::open(c_path.as_ptr(), libc::O_RDWR | libc::O_CLOEXEC) };
        if raw < 0 {
            return Err(UblkError::Io {
                op: "open",
                source: io::Error::last_os_error(),
            });
        }
        // SAFETY: raw is a freshly opened descriptor that nothing else owns.
        let fd = unsafe { OwnedFd::from_raw_fd(raw) };
        let ring = IoUring::<squeue::Entry128, cqueue::Entry>::builder()
            .build(4)
            .map_err(|source| UblkError::Io {
                op: "io_uring_setup",
                source,
            })?;
        let payload = Payload::new().map_err(|source| UblkError::Io { op: "mmap", source })?;
        Ok(Self {
            ring,
            fd,
            payload: Some(payload),
            poisoned: false,
            timeout: DEFAULT_TIMEOUT,
        })
    }

    /// Bound each later command wait by `timeout`.
    pub(crate) fn set_timeout(&mut self, timeout: Duration) {
        self.timeout = timeout;
    }

    /// Whether a command's outcome is unknown, so no more are sent.
    pub(crate) fn is_poisoned(&self) -> bool {
        self.poisoned
    }

    fn sqe(&mut self, op: u32, dev_id: u32, data: u64, payload_len: usize) -> squeue::Entry128 {
        let mut cmd = CtrlCmd {
            dev_id,
            queue_id: u16::MAX,
            data,
            ..CtrlCmd::default()
        };
        if payload_len > 0 {
            cmd.addr = self.payload.as_ref().map_or(0, Payload::addr);
            cmd.len = payload_len as u16;
        }
        let mut bytes = [0_u8; 80];
        bytes[..CTRL_CMD_SIZE].copy_from_slice(&cmd.to_bytes());
        opcode::UringCmd80::new(types::Fd(self.fd.as_raw_fd()), op)
            .cmd(bytes)
            .build()
            .user_data(CMD_USER_DATA)
    }

    /// Queues one entry. A full queue means an earlier entry was never
    /// consumed, so the channel state is unknown and it is poisoned.
    fn push(&mut self, entry: &squeue::Entry128) -> Result<()> {
        // SAFETY: the entry's only pointer is the payload page, which outlives
        // the command: it is unmapped only on drop when no command is
        // outstanding, and leaked otherwise.
        let pushed = unsafe { self.ring.submission().push(entry) };
        pushed.map_err(|_| {
            self.poisoned = true;
            UblkError::Io {
                op: "io_uring push",
                source: io::Error::other("submission queue full"),
            }
        })
    }

    fn pop(&mut self) -> Option<(u64, i32)> {
        self.ring
            .completion()
            .next()
            .map(|cqe| (cqe.user_data(), cqe.result()))
    }

    /// Waits for one completion until `deadline`. Running out of time, or
    /// a failed wait, leaves the command's fate unknown and poisons the
    /// channel.
    fn wait_until(&mut self, op: &'static str, deadline: Instant) -> Result<(u64, i32)> {
        loop {
            let now = Instant::now();
            if now >= deadline {
                if let Some(done) = self.pop() {
                    return Ok(done);
                }
                self.poisoned = true;
                return Err(UblkError::TimedOut { op });
            }
            if let Some(done) = self.wait_timeout(deadline - now)? {
                return Ok(done);
            }
        }
    }

    /// Returns a completion, waiting at most `timeout` for one.
    fn wait_timeout(&mut self, timeout: Duration) -> Result<Option<(u64, i32)>> {
        if let Some(done) = self.pop() {
            return Ok(Some(done));
        }
        let ts = types::Timespec::from(timeout);
        let args = types::SubmitArgs::new().timespec(&ts);
        match self.ring.submitter().submit_with_args(1, &args) {
            Ok(_) => {}
            Err(err) if matches!(err.raw_os_error(), Some(libc::ETIME | libc::EINTR)) => {}
            Err(source) => {
                self.poisoned = true;
                return Err(UblkError::Io {
                    op: "io_uring_enter",
                    source,
                });
            }
        }
        Ok(self.pop())
    }

    /// Sends one command, copying `input` into the payload page first. After
    /// completion, `output` is filled from the page. Returns the CQE result.
    fn command(
        &mut self,
        name: &'static str,
        op: u32,
        dev_id: u32,
        input: &[u8],
        output: &mut [u8],
    ) -> Result<i32> {
        if self.poisoned {
            return Err(UblkError::Poisoned);
        }
        let len = input.len().max(output.len());
        assert!(len <= PAYLOAD_LEN, "ublk control payload too large");
        if len > 0 {
            let page = self
                .payload
                .as_mut()
                .expect("payload present while not poisoned")
                .as_mut_slice();
            page[..len].fill(0);
            page[..input.len()].copy_from_slice(input);
        }
        let entry = self.sqe(op, dev_id, 0, len);
        self.push(&entry)?;
        let deadline = Instant::now() + self.timeout;
        let res = loop {
            if let (CMD_USER_DATA, res) = self.wait_until(name, deadline)? {
                break res;
            }
        };
        if !output.is_empty() {
            let page = self
                .payload
                .as_mut()
                .expect("payload present")
                .as_mut_slice();
            output.copy_from_slice(&page[..output.len()]);
        }
        Ok(res)
    }

    /// ADD_DEV. Returns the device info the kernel filled in, including the
    /// id and any values it adjusted.
    pub(crate) fn add_dev(&mut self, spec: &DevSpec) -> Result<DevInfo> {
        let info = DevInfo {
            nr_hw_queues: spec.queues,
            queue_depth: spec.depth,
            max_io_buf_bytes: spec.max_io_bytes,
            dev_id: spec.id.unwrap_or(u32::MAX),
            flags: spec.flags,
            ublksrv_flags: spec.cookie,
            ..DevInfo::default()
        };
        let mut out = [0_u8; DEV_INFO_SIZE];
        let res = self.command(
            "ADD_DEV",
            U_CMD_ADD_DEV,
            info.dev_id,
            &info.to_bytes(),
            &mut out,
        )?;
        kernel("ADD_DEV", res)?;
        Ok(DevInfo::from_bytes(&out))
    }

    /// SET_PARAMS.
    pub(crate) fn set_params(&mut self, id: u32, params: &Params) -> Result<()> {
        let bytes: [u8; PARAMS_SIZE] = params.to_bytes();
        let res = self.command("SET_PARAMS", U_CMD_SET_PARAMS, id, &bytes, &mut [])?;
        kernel("SET_PARAMS", res).map(drop)
    }

    /// GET_DEV_INFO.
    pub(crate) fn get_dev_info(&mut self, id: u32) -> Result<DevInfo> {
        let mut out = [0_u8; DEV_INFO_SIZE];
        let res = self.command("GET_DEV_INFO", U_CMD_GET_DEV_INFO, id, &[], &mut out)?;
        kernel("GET_DEV_INFO", res)?;
        Ok(DevInfo::from_bytes(&out))
    }

    /// START_DEV. The kernel waits until every tag has a fetch outstanding,
    /// so this can block for a long time. It checks `should_stop` every
    /// 100 ms and, once it returns true, cancels the command and returns
    /// [`UblkError::Canceled`] (or the command's own result if it finished
    /// first). Interrupted waits are retried a bounded number of times.
    pub(crate) fn start_dev(
        &mut self,
        id: u32,
        pid: i32,
        should_stop: &dyn Fn() -> bool,
    ) -> Result<()> {
        for _ in 0..START_RETRIES {
            if self.poisoned {
                return Err(UblkError::Poisoned);
            }
            if should_stop() {
                return Err(UblkError::Canceled);
            }
            let entry = self.sqe(U_CMD_START_DEV, id, pid as u64, 0);
            self.push(&entry)?;
            let (res, canceled) = self.wait_cancellable(should_stop)?;
            if res == -libc::EINTR || res == -ERESTARTSYS || res == -libc::ECANCELED {
                if canceled {
                    return Err(UblkError::Canceled);
                }
                continue;
            }
            return kernel("START_DEV", res).map(drop);
        }
        Err(UblkError::Interrupted)
    }

    /// Waits for the outstanding command, canceling it once `should_stop`
    /// is true. Returns its result and whether a cancel was sent. On return
    /// nothing is in flight; on error the channel is poisoned.
    fn wait_cancellable(&mut self, should_stop: &dyn Fn() -> bool) -> Result<(i32, bool)> {
        let mut canceled = false;
        loop {
            if let Some((user_data, res)) = self.wait_timeout(START_POLL)? {
                if user_data == CMD_USER_DATA {
                    return Ok((res, canceled));
                }
                continue;
            }
            if !should_stop() {
                continue;
            }
            canceled = true;
            let cancel = squeue::Entry128::from(opcode::AsyncCancel::new(CMD_USER_DATA).build())
                .user_data(CANCEL_USER_DATA);
            // START_DEV is still in flight, so a failed push poisons.
            self.push(&cancel)?;
            let deadline = Instant::now() + self.timeout;
            let mut cmd_res = None;
            let cancel_res = loop {
                match self.wait_until("START_DEV", deadline)? {
                    (CANCEL_USER_DATA, res) => break res,
                    (CMD_USER_DATA, res) => cmd_res = Some(res),
                    _ => {}
                }
            };
            if let Some(res) = cmd_res {
                return Ok((res, canceled));
            }
            if cancel_res == 0 || cancel_res == -libc::EALREADY {
                // Found pending or running: the command now completes on its own.
                loop {
                    if let (CMD_USER_DATA, res) = self.wait_until("START_DEV", deadline)? {
                        return Ok((res, canceled));
                    }
                }
            }
            if cancel_res != -libc::ENOENT {
                // The command is still outstanding with an unknown fate.
                self.poisoned = true;
                return Err(UblkError::Kernel {
                    op: "ASYNC_CANCEL",
                    errno: -cancel_res,
                });
            }
            // ENOENT: not visible yet, or already completed. The next wait sees it.
        }
    }

    /// STOP_DEV. Waits at most the channel timeout: STOP_DEV blocks until
    /// every request the kernel handed out is answered.
    pub(crate) fn stop_dev(&mut self, id: u32) -> Result<()> {
        let res = self.command("STOP_DEV", U_CMD_STOP_DEV, id, &[], &mut [])?;
        kernel("STOP_DEV", res).map(drop)
    }

    /// Deletes a device whose character device the caller holds open, so
    /// its id cannot be freed and reused before this returns. Tries
    /// DEL_DEV_ASYNC, and on any rejection but `ENODEV` falls back to a
    /// pinned DEL_DEV (see [`pinned_del`]). A device that no longer exists
    /// counts as deleted. If this fails with the channel poisoned, a
    /// command may still be running and the caller must keep the character
    /// device open.
    pub(crate) fn del_dev(&mut self, id: u32) -> Result<()> {
        let res = self.command("DEL_DEV_ASYNC", U_CMD_DEL_DEV_ASYNC, id, &[], &mut [])?;
        if res == -libc::ENODEV {
            return Ok(());
        }
        if !del_needs_fallback(res) {
            return Ok(());
        }
        let deadline = Instant::now() + self.timeout;
        let result = pinned_del(&mut PinnedRing {
            control: self,
            id,
            deadline,
        });
        if matches!(
            result,
            Err(UblkError::TimedOut { .. } | UblkError::Unknown { .. } | UblkError::Io { .. })
        ) {
            self.poisoned = true;
        }
        result
    }
}

/// User data of a pinned DEL_DEV and its cancel.
const PINNED_DEL_USER_DATA: u64 = 1 << 62 | 1;
const PINNED_CANCEL_USER_DATA: u64 = 1 << 62 | 2;

/// The io_uring access a pinned DEL_DEV needs. Tests script it.
trait DelRing {
    /// Submit DEL_DEV with `PINNED_DEL_USER_DATA` and `IOSQE_ASYNC`.
    fn submit_del(&mut self) -> Result<()>;
    /// Submit an ASYNC_CANCEL of the DEL_DEV with `PINNED_CANCEL_USER_DATA`.
    fn submit_cancel(&mut self) -> Result<()>;
    /// A completion, if one is ready.
    fn poll(&mut self) -> Option<(u64, i32)>;
    /// Wait for the next completion.
    fn wait(&mut self) -> Result<(u64, i32)>;
    /// Pause before a cancel so the io-wq worker can reach its wait.
    fn pause(&mut self, round: u32);
}

/// DEL_DEV while the caller holds the character device, for kernels without
/// DEL_DEV_ASYNC (Linux 6.8).
///
/// The kernel frees a device id only when the last reference to its
/// character device goes away, so while the caller holds it the id cannot
/// be reused and DEL_DEV by id reaches only the caller's device. DEL_DEV
/// removes the device and then waits, interruptibly, for the id to be
/// freed. That wait cannot end while the caller holds the device, so the
/// command is interrupted with ASYNC_CANCEL:
///
/// - Control commands always run on an io-wq worker (`IOSQE_ASYNC`).
/// - Canceling pending io-wq work completes it with `-ECANCELED` before the
///   driver runs, so nothing was deleted and DEL_DEV is sent again.
/// - Canceling running work signals the worker, which ends the wait;
///   DEL_DEV then completes with `-EINTR`, meaning the device is deleted.
///
/// This is a port of the racer-block Go implementation (`pinnedDel`).
fn pinned_del(ring: &mut dyn DelRing) -> Result<()> {
    for round in 0..PINNED_SUBMITS {
        match pinned_del_once(ring, round)? {
            res if res == 0 || res == -libc::EINTR || res == -ERESTARTSYS => return Ok(()),
            res if res == -libc::ENODEV => return Ok(()),
            res if res == -libc::ECANCELED => continue,
            res => return kernel("DEL_DEV", res).map(drop),
        }
    }
    Err(UblkError::Kernel {
        op: "DEL_DEV",
        errno: libc::ECANCELED,
    })
}

/// One DEL_DEV. Returns its result once it and any cancel have completed,
/// so nothing is left in flight.
fn pinned_del_once(ring: &mut dyn DelRing, round: u32) -> Result<i32> {
    let unknown = |reason| UblkError::Unknown {
        op: "DEL_DEV",
        reason,
    };
    ring.submit_del()?;
    let mut del = None;
    let take = |del: &mut Option<i32>, (user_data, res): (u64, i32)| {
        if user_data != PINNED_DEL_USER_DATA {
            return Err(unknown("unexpected completion"));
        }
        if del.replace(res).is_some() {
            return Err(unknown("duplicate completion"));
        }
        Ok(())
    };
    for cancels in 0.. {
        if del.is_some() {
            break;
        }
        if cancels == PINNED_CANCELS {
            return Err(unknown("not cancelable"));
        }
        ring.pause(round + cancels);
        // DEL_DEV may already have failed without waiting.
        while del.is_none() {
            let Some(cqe) = ring.poll() else {
                break;
            };
            take(&mut del, cqe)?;
        }
        if del.is_some() {
            break;
        }
        ring.submit_cancel()?;
        let cancel_res = loop {
            let cqe = ring.wait()?;
            if cqe.0 == PINNED_CANCEL_USER_DATA {
                break cqe.1;
            }
            take(&mut del, cqe)?;
        };
        match cancel_res {
            0 => {}
            res if res == -libc::EALREADY => {}
            // Not found: completed, which the next poll sees, or not yet
            // visible to the cancel. Try again.
            res if res == -libc::ENOENT => continue,
            _ => return Err(unknown("ASYNC_CANCEL failed")),
        }
        // Found pending (completes -ECANCELED) or running (signaled).
        while del.is_none() {
            let cqe = ring.wait()?;
            take(&mut del, cqe)?;
        }
    }
    Ok(del.expect("loop ends with a result"))
}

/// A pinned DEL_DEV on the control ring, bounded by `deadline`.
struct PinnedRing<'a> {
    control: &'a mut Control,
    id: u32,
    deadline: Instant,
}

impl DelRing for PinnedRing<'_> {
    fn submit_del(&mut self) -> Result<()> {
        let entry = self
            .control
            .sqe(U_CMD_DEL_DEV, self.id, 0, 0)
            .flags(squeue::Flags::ASYNC)
            .user_data(PINNED_DEL_USER_DATA);
        self.control.push(&entry)?;
        self.submit()
    }

    fn submit_cancel(&mut self) -> Result<()> {
        let cancel = squeue::Entry128::from(opcode::AsyncCancel::new(PINNED_DEL_USER_DATA).build())
            .user_data(PINNED_CANCEL_USER_DATA);
        self.control.push(&cancel)?;
        self.submit()
    }

    fn poll(&mut self) -> Option<(u64, i32)> {
        self.control.pop()
    }

    fn wait(&mut self) -> Result<(u64, i32)> {
        self.control.wait_until("DEL_DEV", self.deadline)
    }

    fn pause(&mut self, round: u32) {
        std::thread::sleep(
            Duration::from_millis(1 << round.min(8)).min(Duration::from_millis(200)),
        );
    }
}

impl PinnedRing<'_> {
    fn submit(&mut self) -> Result<()> {
        match self.control.ring.submit() {
            Ok(_) => Ok(()),
            Err(source) => {
                self.control.poisoned = true;
                Err(UblkError::Io {
                    op: "io_uring_enter",
                    source,
                })
            }
        }
    }
}

impl Drop for Control {
    fn drop(&mut self) {
        // This runs before the fields drop, so the payload is handled while
        // the ring is still open. That is safe: it is unmapped only when no
        // command is outstanding. If one may be, the page is leaked, since
        // closing the ring does not wait for in-flight commands and the
        // kernel could write into whatever is mapped there next.
        if let Some(payload) = self.payload.take() {
            if self.poisoned {
                // Payload has no Drop, so dropping it leaks the mapping.
                let _leaked = payload;
            } else {
                payload.unmap();
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn open_missing_directory_fails() {
        let err = Control::open(Path::new("/nonexistent-ublk-dir"))
            .err()
            .expect("open fails");
        assert_eq!(err.errno(), Some(libc::ENOENT));
        assert!(err.to_string().contains("open"));
    }

    /// A scripted pinned-delete ring. `script` lists, for each submit or
    /// cancel, the completions it makes ready.
    #[derive(Default)]
    struct Script {
        ready: std::collections::VecDeque<(u64, i32)>,
        on_del: std::collections::VecDeque<Vec<(u64, i32)>>,
        on_cancel: std::collections::VecDeque<Vec<(u64, i32)>>,
        dels: u32,
        cancels: u32,
    }

    impl DelRing for Script {
        fn submit_del(&mut self) -> Result<()> {
            self.dels += 1;
            self.ready
                .extend(self.on_del.pop_front().unwrap_or_default());
            Ok(())
        }

        fn submit_cancel(&mut self) -> Result<()> {
            self.cancels += 1;
            self.ready
                .extend(self.on_cancel.pop_front().unwrap_or_default());
            Ok(())
        }

        fn poll(&mut self) -> Option<(u64, i32)> {
            self.ready.pop_front()
        }

        fn wait(&mut self) -> Result<(u64, i32)> {
            self.ready
                .pop_front()
                .ok_or(UblkError::TimedOut { op: "DEL_DEV" })
        }

        fn pause(&mut self, _round: u32) {}
    }

    const DEL: u64 = PINNED_DEL_USER_DATA;
    const CANCEL: u64 = PINNED_CANCEL_USER_DATA;

    fn script(on_del: Vec<Vec<(u64, i32)>>, on_cancel: Vec<Vec<(u64, i32)>>) -> Script {
        Script {
            on_del: on_del.into(),
            on_cancel: on_cancel.into(),
            ..Script::default()
        }
    }

    #[test]
    fn del_falls_back_on_any_error_but_enodev() {
        assert!(del_needs_fallback(-524), "Linux 6.8 ENOTSUPP");
        assert!(del_needs_fallback(-libc::EOPNOTSUPP));
        assert!(del_needs_fallback(-libc::EINVAL));
        assert!(del_needs_fallback(-libc::EPERM));
        assert!(!del_needs_fallback(-libc::ENODEV));
        assert!(!del_needs_fallback(0));
    }

    #[test]
    fn pinned_del_interrupted_wait_means_deleted() {
        let mut ring = script(vec![vec![]], vec![vec![(CANCEL, 0), (DEL, -libc::EINTR)]]);
        pinned_del(&mut ring).expect("deleted");
        assert_eq!((ring.dels, ring.cancels), (1, 1));
    }

    #[test]
    fn pinned_del_failing_at_once_needs_no_cancel() {
        let mut ring = script(vec![vec![(DEL, -libc::EPERM)]], vec![]);
        let err = pinned_del(&mut ring).expect_err("rejected");
        assert_eq!(err.errno(), Some(libc::EPERM));
        assert_eq!(ring.cancels, 0);
        let mut ring = script(vec![vec![(DEL, 0)]], vec![]);
        pinned_del(&mut ring).expect("deleted");
    }

    #[test]
    fn pinned_del_canceled_before_dispatch_is_resent() {
        let mut ring = script(
            vec![vec![], vec![]],
            vec![
                vec![(DEL, -libc::ECANCELED), (CANCEL, 0)],
                vec![(CANCEL, -libc::EALREADY), (DEL, -libc::EINTR)],
            ],
        );
        pinned_del(&mut ring).expect("deleted");
        assert_eq!((ring.dels, ring.cancels), (2, 2));
    }

    #[test]
    fn pinned_del_cancel_not_found_is_retried() {
        let mut ring = script(
            vec![vec![]],
            vec![
                vec![(CANCEL, -libc::ENOENT)],
                vec![(CANCEL, 0), (DEL, -libc::EINTR)],
            ],
        );
        pinned_del(&mut ring).expect("deleted");
        assert_eq!(ring.cancels, 2);
    }

    #[test]
    fn pinned_del_lost_completion_is_unknown() {
        let mut ring = script(vec![vec![]], vec![vec![(CANCEL, 0)]]);
        let err = pinned_del(&mut ring).expect_err("unknown");
        assert!(matches!(err, UblkError::TimedOut { .. }), "{err}");
        let mut ring = script(vec![vec![]], vec![vec![(CANCEL, -libc::EINVAL)]]);
        assert!(matches!(
            pinned_del(&mut ring),
            Err(UblkError::Unknown { .. })
        ));
        let mut ring = script(vec![vec![(7, 0)]], vec![]);
        assert!(matches!(
            pinned_del(&mut ring),
            Err(UblkError::Unknown { .. })
        ));
    }

    #[test]
    fn pinned_del_gives_up_after_repeated_early_cancels() {
        let mut ring = script(
            vec![vec![]; PINNED_SUBMITS as usize],
            vec![vec![(DEL, -libc::ECANCELED), (CANCEL, 0)]; PINNED_SUBMITS as usize],
        );
        let err = pinned_del(&mut ring).expect_err("gave up");
        assert_eq!(err.errno(), Some(libc::ECANCELED));
        assert_eq!(ring.dels, PINNED_SUBMITS);
    }

    #[test]
    fn kernel_result_mapping() {
        assert_eq!(kernel("X", 3).ok(), Some(3));
        let err = kernel("X", -libc::EROFS).expect_err("error");
        assert_eq!(err.errno(), Some(libc::EROFS));
        assert_eq!(UblkError::Poisoned.errno(), None);
    }
}
