//! The owner's own thread: a [`racer_io::exec::Role`] that polls the
//! [`Owner`] and parks on its wake sources.
//!
//! The wake sources are an eventfd (commands, closes, returned buffers, and
//! [`OwnerStop::stop`] all wake it through the owner's waker) and the
//! owner's [`Owner::poll_fds`] (completion channels and, in registration
//! mode, connection manager channels). Timers come from
//! [`Owner::next_wake`]. An idle owner sleeps until a wake source fires;
//! there is no periodic tick.
//!
//! Stopping is a cross-thread drain: [`OwnerStop::stop`] closes the owner,
//! and the role finishes only once the owner drained. A buffer still held
//! elsewhere (a crypto job, say) keeps the role waiting, with a diagnostic
//! every [`DRAIN_REPORT`], but it never frees memory early.

use std::{
    os::fd::{AsRawFd, FromRawFd, OwnedFd},
    sync::{
        Arc,
        atomic::{AtomicBool, AtomicU64, Ordering},
    },
    task::{Wake, Waker},
    time::{Duration, Instant},
};

use racer_io::exec::{Next, Role};

use crate::Owner;

/// Commands one step handles before it polls links.
const BUDGET: usize = 64;

/// Period of the diagnostic a slow drain prints.
pub const DRAIN_REPORT: Duration = Duration::from_secs(10);

/// A nonblocking eventfd whose readability means "poll the owner".
struct EventFd(OwnedFd);

impl EventFd {
    fn new() -> std::io::Result<Self> {
        // SAFETY: plain syscall; a valid descriptor is owned below.
        let fd = unsafe { libc::eventfd(0, libc::EFD_NONBLOCK | libc::EFD_CLOEXEC) };
        if fd < 0 {
            return Err(std::io::Error::last_os_error());
        }
        // SAFETY: `fd` is a fresh descriptor nothing else owns.
        Ok(Self(unsafe { OwnedFd::from_raw_fd(fd) }))
    }

    /// Make the descriptor readable. A full counter is already readable.
    fn signal(&self) {
        let one = 1u64;
        // SAFETY: writes 8 bytes from a live u64.
        let _ = unsafe { libc::write(self.0.as_raw_fd(), (&raw const one).cast(), 8) };
    }

    /// Reset the counter. Nonblocking.
    fn clear(&self) {
        let mut value = 0u64;
        // SAFETY: reads 8 bytes into a live u64.
        let _ = unsafe { libc::read(self.0.as_raw_fd(), (&raw mut value).cast(), 8) };
    }
}

impl Wake for EventFd {
    fn wake(self: Arc<Self>) {
        self.signal();
    }

    fn wake_by_ref(self: &Arc<Self>) {
        self.signal();
    }
}

/// Asks an [`OwnerRole`] to close its owner and finish once drained. `Send`
/// and `Sync`; keep it on the thread that joins the owner's thread.
pub struct OwnerStop {
    requested: AtomicBool,

    wake: Arc<EventFd>,

    /// Times the role returned from a park, for idle-wake measurements.
    wakes: AtomicU64,
}

impl OwnerStop {
    /// Close the owner and let the role finish once it drained. Does not wait.
    pub fn stop(&self) {
        self.requested.store(true, Ordering::Release);
        self.wake.signal();
    }

    /// True once [`Self::stop`] was called.
    pub fn requested(&self) -> bool {
        self.requested.load(Ordering::Acquire)
    }

    /// Times the role woke from a park so far.
    pub fn wakes(&self) -> u64 {
        self.wakes.load(Ordering::Relaxed)
    }
}

/// The owner as the role of its own thread. See the module docs.
pub struct OwnerRole {
    owner: Owner,

    stop: Arc<OwnerStop>,

    waker: Waker,

    /// When the owner was closed for good.
    closing_since: Option<Instant>,

    /// Diagnostics printed for this drain.
    reported: u32,
}

impl OwnerRole {
    /// Wrap `owner`. Call on the owner's thread.
    pub fn new(owner: Owner) -> crate::Result<(Self, Arc<OwnerStop>)> {
        let wake = Arc::new(EventFd::new().map_err(|_| crate::Error::Io)?);
        let stop = Arc::new(OwnerStop {
            requested: AtomicBool::new(false),
            wake: wake.clone(),
            wakes: AtomicU64::new(0),
        });
        let role = Self {
            owner,
            stop: stop.clone(),
            waker: Waker::from(wake),
            closing_since: None,
            reported: 0,
        };
        Ok((role, stop))
    }

    /// The hosted owner.
    pub fn owner(&self) -> &Owner {
        &self.owner
    }

    /// The hosted owner, mutably.
    pub fn owner_mut(&mut self) -> &mut Owner {
        &mut self.owner
    }

    /// The next diagnostic time of a drain that started at `since`.
    fn report_at(&self, since: Instant) -> Instant {
        since + DRAIN_REPORT * (self.reported + 1)
    }
}

impl Role for OwnerRole {
    fn step(&mut self) -> racer_io::Result<Next> {
        let now = self.owner.ctx_now();
        if self.closing_since.is_none() && self.stop.requested() {
            self.owner.close();
            self.closing_since = Some(now);
        }
        // Register before polling: a wake after the poll reads its work, so
        // no wake falls between the poll and the park.
        self.owner.register_driver(&self.waker);
        self.owner
            .poll_budgeted(BUDGET)
            .map_err(|_| racer_io::Error::Io)?;
        let mut report = None;
        if let Some(since) = self.closing_since {
            if self.owner.drained() {
                return Ok(Next::Done);
            }
            if now >= self.report_at(since) {
                self.reported += 1;
                eprintln!(
                    "racer: stage=rdma-owner state=draining waited_secs={} free_buffers_pending=true; \
                     a page buffer is still held, memory is not freed until it returns",
                    now.saturating_duration_since(since).as_secs()
                );
            }
            report = Some(self.report_at(since));
        }
        let wake = match (self.owner.next_wake(), report) {
            (Some(a), Some(b)) => Some(a.min(b)),
            (a, b) => a.or(b),
        };
        Ok(match wake {
            None => Next::Idle,
            Some(at) if at <= now => Next::Ready,
            Some(at) => Next::WaitUntil(at),
        })
    }

    fn park(&mut self, next: Next) -> racer_io::Result<()> {
        let timeout = match next {
            Next::Ready | Next::Done => return Ok(()),
            Next::WaitUntil(at) => Some(at.saturating_duration_since(self.owner.ctx_now())),
            Next::Idle => None,
        };
        let mut fds: Vec<libc::pollfd> = std::iter::once(self.stop.wake.0.as_raw_fd())
            .chain(self.owner.poll_fds())
            .map(|fd| libc::pollfd {
                fd,
                events: libc::POLLIN,
                revents: 0,
            })
            .collect();
        let spec = timeout.map(|t| libc::timespec {
            tv_sec: t.as_secs().min(i64::MAX as u64) as libc::time_t,
            tv_nsec: libc::c_long::from(t.subsec_nanos() as i32),
        });
        // SAFETY: `fds` and `spec` outlive the call; a null timeout blocks.
        let result = unsafe {
            libc::ppoll(
                fds.as_mut_ptr(),
                fds.len() as libc::nfds_t,
                spec.as_ref().map_or(std::ptr::null(), std::ptr::from_ref),
                std::ptr::null(),
            )
        };
        if result < 0 {
            let error = std::io::Error::last_os_error();
            if error.kind() != std::io::ErrorKind::Interrupted {
                return Err(racer_io::Error::from_io(error));
            }
        }
        self.stop.wake.clear();
        self.stop.wakes.fetch_add(1, Ordering::Relaxed);
        Ok(())
    }
}
