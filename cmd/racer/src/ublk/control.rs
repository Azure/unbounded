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
use std::time::Duration;

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

/// User data of a command and of its ASYNC_CANCEL.
const CMD_USER_DATA: u64 = 1;
const CANCEL_USER_DATA: u64 = 2;

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
    /// START_DEV was canceled because the caller asked to stop.
    Canceled,
    /// START_DEV was interrupted more than the retry limit.
    Interrupted,
}

impl UblkError {
    /// The kernel errno, if the kernel rejected the command.
    #[cfg_attr(not(test), allow(dead_code))]
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
    // Field order matters on drop: the ring goes first so in-flight commands
    // are canceled before the payload is (maybe) unmapped.
    ring: IoUring<squeue::Entry128, cqueue::Entry>,
    fd: OwnedFd,
    payload: Option<Payload>,
    poisoned: bool,
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
        })
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

    fn push(&mut self, entry: &squeue::Entry128) -> Result<()> {
        // SAFETY: the entry's only pointer is the payload page, which outlives
        // the command: it is unmapped only on drop when no command is
        // outstanding, and leaked otherwise.
        unsafe { self.ring.submission().push(entry) }.map_err(|_| UblkError::Io {
            op: "io_uring push",
            source: io::Error::other("submission queue full"),
        })
    }

    fn pop(&mut self) -> Option<(u64, i32)> {
        self.ring
            .completion()
            .next()
            .map(|cqe| (cqe.user_data(), cqe.result()))
    }

    /// Waits for at least one completion. A failure here leaves the command's
    /// fate unknown, so it poisons the channel.
    fn wait_one(&mut self) -> Result<(u64, i32)> {
        loop {
            if let Some(done) = self.pop() {
                return Ok(done);
            }
            match self.ring.submit_and_wait(1) {
                Ok(_) => {}
                Err(err) if err.raw_os_error() == Some(libc::EINTR) => {}
                Err(source) => {
                    self.poisoned = true;
                    return Err(UblkError::Io {
                        op: "io_uring_enter",
                        source,
                    });
                }
            }
        }
    }

    /// Like `wait_one`, but returns `None` after `timeout` without a completion.
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
        op: u32,
        dev_id: u32,
        data: u64,
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
        let entry = self.sqe(op, dev_id, data, len);
        self.push(&entry)?;
        let (_, res) = self.wait_one()?;
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

    /// ADD_DEV. Returns the device info the kernel filled in, including the id.
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
        let res = self.command(U_CMD_ADD_DEV, info.dev_id, 0, &info.to_bytes(), &mut out)?;
        kernel("ADD_DEV", res)?;
        Ok(DevInfo::from_bytes(&out))
    }

    /// SET_PARAMS.
    pub(crate) fn set_params(&mut self, id: u32, params: &Params) -> Result<()> {
        let bytes: [u8; PARAMS_SIZE] = params.to_bytes();
        let res = self.command(U_CMD_SET_PARAMS, id, 0, &bytes, &mut [])?;
        kernel("SET_PARAMS", res).map(drop)
    }

    /// GET_DEV_INFO.
    #[cfg_attr(not(test), allow(dead_code))]
    pub(crate) fn get_dev_info(&mut self, id: u32) -> Result<DevInfo> {
        let mut out = [0_u8; DEV_INFO_SIZE];
        let res = self.command(U_CMD_GET_DEV_INFO, id, 0, &[], &mut out)?;
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
    /// nothing is in flight.
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
            // SAFETY: ASYNC_CANCEL carries no pointers.
            unsafe { self.ring.submission().push(&cancel) }.map_err(|_| UblkError::Io {
                op: "io_uring push",
                source: io::Error::other("submission queue full"),
            })?;
            let mut cmd_res = None;
            let cancel_res = loop {
                match self.wait_one()? {
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
                    if let (CMD_USER_DATA, res) = self.wait_one()? {
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

    /// STOP_DEV.
    pub(crate) fn stop_dev(&mut self, id: u32) -> Result<()> {
        let res = self.command(U_CMD_STOP_DEV, id, 0, &[], &mut [])?;
        kernel("STOP_DEV", res).map(drop)
    }

    /// Deletes a device. Tries DEL_DEV_ASYNC, which does not wait for the char
    /// device to be released, and falls back to DEL_DEV on kernels without it.
    /// A device that no longer exists counts as deleted.
    pub(crate) fn del_dev(&mut self, id: u32) -> Result<()> {
        let mut res = self.command(U_CMD_DEL_DEV_ASYNC, id, 0, &[], &mut [])?;
        if res == -libc::EOPNOTSUPP || res == -libc::EINVAL {
            res = self.command(U_CMD_DEL_DEV, id, 0, &[], &mut [])?;
        }
        if res == -libc::ENODEV {
            return Ok(());
        }
        kernel("DEL_DEV", res).map(drop)
    }
}

impl Drop for Control {
    fn drop(&mut self) {
        // If a command may still be in flight, keep the page mapped. Closing
        // the ring does not wait for in-flight commands, and the kernel would
        // write through the old address into whatever is mapped there next.
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

    #[test]
    fn kernel_result_mapping() {
        assert_eq!(kernel("X", 3).ok(), Some(3));
        let err = kernel("X", -libc::EROFS).expect_err("error");
        assert_eq!(err.errno(), Some(libc::EROFS));
        assert_eq!(UblkError::Poisoned.errno(), None);
    }
}
