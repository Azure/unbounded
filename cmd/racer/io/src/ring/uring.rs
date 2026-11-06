//! Kernel io_uring backend.
use super::op::Op;
use crate::{Error, Result};
use io_uring::{IoUring, cqueue, types};
use std::{os::fd::AsRawFd, time::Duration};

// Linux UAPI setup flags.
const SETUP_CQSIZE: u32 = 1 << 3;
const SETUP_SUBMIT_ALL: u32 = 1 << 7;
const SETUP_SINGLE_ISSUER: u32 = 1 << 12;
const SETUP_NORMAL: u32 = SETUP_CQSIZE | SETUP_SUBMIT_ALL | SETUP_SINGLE_ISSUER;

/// Counters for kernel entries, used to verify the skip-empty-enter contract.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct EnterStats {
    /// `io_uring_enter` calls that submitted SQEs without waiting.
    pub submits: u64,

    /// `io_uring_enter` calls that waited for a completion.
    pub waits: u64,

    /// Submission requests skipped because the SQ was empty.
    pub skipped: u64,

    /// SQEs queued with `IOSQE_ASYNC`.
    pub offloaded: u64,
}

/// The kernel ring and its setup facts.
pub(crate) struct UringDriver {
    ring: IoUring,

    flags: u32,

    ext_arg: bool,

    pub(crate) stats: EnterStats,
}

impl UringDriver {
    /// Build a ring with `sq` submission and `2 * sq` completion entries,
    /// probing `SINGLE_ISSUER` and `SUBMIT_ALL` and falling back when the
    /// kernel rejects them.
    ///
    /// The kernel charges ring memory to `RLIMIT_MEMLOCK` for unprivileged
    /// users, and the limit is shared by every ring the user holds. When
    /// setup fails with `ENOMEM` the ring is halved, down to
    /// [`MIN_FALLBACK_SQ`]: a full SQ is flushed before the next push and
    /// the kernel keeps overflowing completions, so a smaller ring only
    /// costs extra submissions.
    pub(crate) fn new(sq: u32) -> Result<Self> {
        sq.checked_mul(2).ok_or(Error::InvalidConfiguration)?;
        let mut used = 0;
        let ring = shrink_ring(sq, |sq| {
            probe_ring(|flags| {
                used = flags;
                let mut builder = IoUring::builder();
                builder.setup_cqsize(sq * 2);
                if flags & SETUP_SUBMIT_ALL != 0 {
                    builder.setup_submit_all();
                }
                if flags & SETUP_SINGLE_ISSUER != 0 {
                    builder.setup_single_issuer();
                }
                builder.build(sq)
            })
        })
        .map_err(Error::from_io)?;
        let ext_arg = ring.params().is_feature_ext_arg();
        Ok(Self {
            ring,
            flags: used,
            ext_arg,
            stats: EnterStats::default(),
        })
    }

    /// Setup flags the kernel accepted.
    pub(crate) fn setup_flags(&self) -> u32 {
        self.flags
    }

    /// Queue one SQE; `Err` when the SQ is full.
    pub(crate) fn push(
        &mut self,
        user_data: u64,
        op: &Op,
        offload: bool,
    ) -> std::result::Result<(), ()> {
        let (sqe, offloaded) = op.issue(offload);
        let sqe = sqe.user_data(user_data);
        // SAFETY: the slot owning every pointer in `op` outlives both CQEs.
        unsafe { self.ring.submission().push(&sqe) }.map_err(|_| ())?;
        self.stats.offloaded += u64::from(offloaded);
        Ok(())
    }

    /// Pop one CQE.
    pub(crate) fn next_cqe(&mut self) -> Option<(u64, i32, u32)> {
        let cqe = self.ring.completion().next()?;
        Some((cqe.user_data(), cqe.result(), cqe.flags()))
    }

    /// Whether completions are waiting in the CQ.
    pub(crate) fn has_cqes(&mut self) -> bool {
        !self.ring.completion().is_empty()
    }

    /// Submit queued SQEs, skipping the syscall when nothing requires it.
    pub(crate) fn submit(&mut self) -> Result<()> {
        if !self.enter_required() {
            self.stats.skipped += 1;
            return Ok(());
        }
        self.stats.submits += 1;
        transient(self.ring.submit())
    }

    /// Submit and wait for at least one completion or until `timeout`.
    pub(crate) fn wait(&mut self, timeout: Option<Duration>) -> Result<()> {
        if self.has_cqes() {
            return self.submit();
        }
        self.stats.waits += 1;
        match timeout {
            None => transient(self.ring.submit_and_wait(1)),
            Some(timeout) if self.ext_arg => {
                let ts = types::Timespec::from(timeout);
                let args = types::SubmitArgs::new().timespec(&ts);
                match self.ring.submitter().submit_with_args(1, &args) {
                    Err(error) if error.raw_os_error() == Some(libc::ETIME) => Ok(()),
                    result => transient(result),
                }
            }
            Some(timeout) => {
                // Without EXT_ARG, submit then ppoll the ring descriptor, which
                // becomes readable when CQEs are posted.
                transient(self.ring.submit())?;
                if self.has_cqes() {
                    return Ok(());
                }
                let mut fds = [libc::pollfd {
                    fd: self.ring.as_raw_fd(),
                    events: libc::POLLIN,
                    revents: 0,
                }];
                let ts = libc::timespec {
                    tv_sec: timeout.as_secs().min(i64::MAX as u64) as libc::time_t,
                    tv_nsec: timeout.subsec_nanos() as libc::c_long,
                };
                // SAFETY: borrows initialized locals for the call only.
                let result = unsafe { libc::ppoll(fds.as_mut_ptr(), 1, &ts, std::ptr::null()) };
                if result < 0 && std::io::Error::last_os_error().raw_os_error() != Some(libc::EINTR)
                {
                    return Err(Error::last_os_error());
                }
                Ok(())
            }
        }
    }

    /// Whether `io_uring_enter` is needed: queued SQEs, CQ overflow, or task
    /// work. Unknown setup modes stay fail-closed.
    fn enter_required(&mut self) -> bool {
        let sq = self.ring.submission();
        submission_required(self.flags, sq.is_empty(), sq.cq_overflow(), sq.taskrun())
    }
}

/// Whether a multishot CQE will be followed by more.
pub(crate) fn more(flags: u32) -> bool {
    cqueue::more(flags)
}

/// Probe setup combinations, keeping the first ring the kernel accepts.
fn probe_ring<T>(mut build: impl FnMut(u32) -> std::io::Result<T>) -> std::io::Result<T> {
    for flags in [SETUP_NORMAL, SETUP_CQSIZE | SETUP_SUBMIT_ALL, SETUP_CQSIZE] {
        match build(flags) {
            Err(error) if error.raw_os_error() == Some(libc::EINVAL) && flags != SETUP_CQSIZE => (),
            result => return result,
        }
    }
    unreachable!("the baseline setup returns its result")
}

/// Smallest SQ [`shrink_ring`] falls back to under memory pressure.
const MIN_FALLBACK_SQ: u32 = 64;

/// Build with `sq` entries, halving on `ENOMEM` until it succeeds, fails
/// otherwise, or reaches [`MIN_FALLBACK_SQ`] (or `sq`, if smaller).
fn shrink_ring<T>(
    mut sq: u32,
    mut build: impl FnMut(u32) -> std::io::Result<T>,
) -> std::io::Result<T> {
    let floor = sq.min(MIN_FALLBACK_SQ);
    loop {
        match build(sq) {
            Err(error) if error.raw_os_error() == Some(libc::ENOMEM) && sq > floor => {
                sq = (sq / 2).max(floor);
            }
            result => return result,
        }
    }
}

/// Skip empty submissions only for known interrupt-driven modes.
fn submission_required(setup_flags: u32, empty: bool, cq_overflow: bool, taskrun: bool) -> bool {
    setup_flags & !SETUP_NORMAL != 0 || !empty || cq_overflow || taskrun
}

/// Defer transient enter failures to the next turn; report fatal ones.
fn transient(result: std::io::Result<usize>) -> Result<()> {
    match result {
        Ok(_) => Ok(()),
        Err(error)
            if matches!(
                error.raw_os_error(),
                Some(libc::EINTR | libc::EAGAIN | libc::EBUSY | libc::ETIME)
            ) =>
        {
            Ok(())
        }
        Err(error) => Err(Error::from_io(error)),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn probing_falls_back_only_on_einval() {
        let mut tried = Vec::new();
        let result = probe_ring(|flags| {
            tried.push(flags);
            if flags == SETUP_CQSIZE {
                Ok(flags)
            } else {
                Err(std::io::Error::from_raw_os_error(libc::EINVAL))
            }
        });
        assert_eq!(result.unwrap(), SETUP_CQSIZE);
        assert_eq!(tried.len(), 3);
        let result: std::io::Result<u32> =
            probe_ring(|_| Err(std::io::Error::from_raw_os_error(libc::ENOMEM)));
        assert_eq!(result.unwrap_err().raw_os_error(), Some(libc::ENOMEM));
    }

    #[test]
    fn ring_shrinks_only_on_enomem() {
        let enomem = || std::io::Error::from_raw_os_error(libc::ENOMEM);
        let mut tried = Vec::new();
        let result = shrink_ring(1024, |sq| {
            tried.push(sq);
            if sq <= 256 { Ok(sq) } else { Err(enomem()) }
        });
        assert_eq!(result.unwrap(), 256);
        assert_eq!(tried, [1024, 512, 256]);

        // Stops at the floor and reports the last error.
        tried.clear();
        let result: std::io::Result<u32> = shrink_ring(300, |sq| {
            tried.push(sq);
            Err(enomem())
        });
        assert_eq!(result.unwrap_err().raw_os_error(), Some(libc::ENOMEM));
        assert_eq!(tried, [300, 150, 75, 64]);

        // A ring already below the floor is tried once.
        tried.clear();
        let result: std::io::Result<u32> = shrink_ring(8, |sq| {
            tried.push(sq);
            Err(enomem())
        });
        assert!(result.is_err());
        assert_eq!(tried, [8]);

        // Other errors are not retried.
        tried.clear();
        let result: std::io::Result<u32> = shrink_ring(1024, |sq| {
            tried.push(sq);
            Err(std::io::Error::from_raw_os_error(libc::EPERM))
        });
        assert_eq!(result.unwrap_err().raw_os_error(), Some(libc::EPERM));
        assert_eq!(tried, [1024]);
    }

    #[test]
    fn empty_submission_is_skipped_only_in_known_modes() {
        assert!(!submission_required(SETUP_NORMAL, true, false, false));
        assert!(submission_required(SETUP_NORMAL, false, false, false));
        assert!(submission_required(SETUP_NORMAL, true, true, false));
        assert!(submission_required(SETUP_NORMAL, true, false, true));
        assert!(submission_required(1 << 13, true, false, false));
    }

    #[test]
    fn transient_errors_are_deferred() {
        for errno in [libc::EINTR, libc::EAGAIN, libc::EBUSY, libc::ETIME] {
            assert_eq!(
                transient(Err(std::io::Error::from_raw_os_error(errno))),
                Ok(())
            );
        }
        assert_eq!(
            transient(Err(std::io::Error::from_raw_os_error(libc::EBADF))),
            Err(Error::Os(libc::EBADF))
        );
    }
}
