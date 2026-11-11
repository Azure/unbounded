//! `signalfd`-based stop signals, so a stop request is an ordinary readable
//! descriptor on the lane ring instead of an atomic polled by a tick.
use crate::{Error, Result};
use std::os::fd::{AsFd, AsRawFd, BorrowedFd, FromRawFd, OwnedFd, RawFd};

/// A nonblocking, close-on-exec `signalfd` for a fixed signal set.
///
/// [`SignalFd::new`] blocks the signals in the calling thread's mask. Create
/// it on the main thread before spawning workers so every thread inherits the
/// block and the signals are only ever delivered through the descriptor.
#[derive(Debug)]
pub struct SignalFd {
    fd: OwnedFd,

    signals: Vec<i32>,
}

impl SignalFd {
    /// Block `signals` in the calling thread and open a descriptor for them.
    pub fn new(signals: &[i32]) -> Result<Self> {
        if signals.is_empty() {
            return Err(Error::InvalidInput);
        }
        // SAFETY: sigset_t is plain data initialized by sigemptyset.
        let mut set: libc::sigset_t = unsafe { std::mem::zeroed() };
        // SAFETY: the set is a valid, writable sigset_t.
        unsafe { libc::sigemptyset(&mut set) };
        for &signal in signals {
            // SAFETY: as above; invalid signal numbers fail.
            if unsafe { libc::sigaddset(&mut set, signal) } != 0 {
                return Err(Error::InvalidInput);
            }
        }
        // SAFETY: the set is initialized; the old mask is not requested.
        let result = unsafe { libc::pthread_sigmask(libc::SIG_BLOCK, &set, std::ptr::null_mut()) };
        if result != 0 {
            return Err(Error::from_errno(result));
        }
        // SAFETY: creates a new descriptor from an initialized set.
        let fd = unsafe { libc::signalfd(-1, &set, libc::SFD_NONBLOCK | libc::SFD_CLOEXEC) };
        if fd < 0 {
            return Err(Error::last_os_error());
        }
        Ok(Self {
            // SAFETY: signalfd returned a fresh descriptor we now own.
            fd: unsafe { OwnedFd::from_raw_fd(fd) },
            signals: signals.to_vec(),
        })
    }

    /// The signals this descriptor reports.
    pub fn signals(&self) -> &[i32] {
        &self.signals
    }

    /// Read one pending signal without blocking.
    pub fn try_read(&self) -> Result<Option<i32>> {
        // SAFETY: signalfd_siginfo is plain data.
        let mut info: libc::signalfd_siginfo = unsafe { std::mem::zeroed() };
        let size = std::mem::size_of::<libc::signalfd_siginfo>();
        loop {
            // SAFETY: reads at most `size` bytes into the local struct.
            let read = unsafe {
                libc::read(
                    self.fd.as_raw_fd(),
                    (&mut info as *mut libc::signalfd_siginfo).cast(),
                    size,
                )
            };
            if read == size as isize {
                return Ok(Some(info.ssi_signo as i32));
            }
            match std::io::Error::last_os_error().raw_os_error() {
                Some(libc::EINTR) => continue,
                Some(libc::EAGAIN) => return Ok(None),
                _ if read >= 0 => return Err(Error::Io),
                Some(errno) => return Err(Error::from_errno(errno)),
                None => return Err(Error::Io),
            }
        }
    }
}

impl SignalFd {
    /// Wait on `reactor` for the next signal: a ring poll on the descriptor,
    /// so a stop signal wakes a parked lane without any periodic tick.
    pub async fn recv(&self, reactor: &crate::Reactor, ctx: &crate::OpCtx) -> Result<i32> {
        loop {
            if let Some(signal) = self.try_read()? {
                return Ok(signal);
            }
            // SAFETY: the kernel pins the open file for the poll; `self`
            // outlives this future's use of the descriptor number.
            unsafe { reactor.readable_raw(self.fd.as_raw_fd(), ctx) }.await?;
        }
    }
}

impl AsRawFd for SignalFd {
    /// The signalfd descriptor number.
    fn as_raw_fd(&self) -> RawFd {
        self.fd.as_raw_fd()
    }
}

impl AsFd for SignalFd {
    /// Borrow the signalfd descriptor.
    fn as_fd(&self) -> BorrowedFd<'_> {
        self.fd.as_fd()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn signals_arrive_through_the_descriptor() {
        // Run on a fresh thread so the mask change stays local to the test.
        std::thread::spawn(|| {
            let fd = SignalFd::new(&[libc::SIGUSR2]).unwrap();
            assert_eq!(fd.signals(), [libc::SIGUSR2]);
            assert_eq!(fd.try_read().unwrap(), None);
            // SAFETY: deliver the blocked signal to this thread only.
            assert_eq!(
                unsafe { libc::pthread_kill(libc::pthread_self(), libc::SIGUSR2) },
                0
            );
            assert_eq!(fd.try_read().unwrap(), Some(libc::SIGUSR2));
            assert_eq!(fd.try_read().unwrap(), None);
        })
        .join()
        .unwrap();
        assert_eq!(SignalFd::new(&[]).unwrap_err(), Error::InvalidInput);
    }

    #[test]
    fn a_parked_lane_wakes_on_a_signal() {
        std::thread::spawn(|| {
            let Some(reactor) = crate::ring::tests::kernel(4) else {
                return;
            };
            let fd = std::rc::Rc::new(SignalFd::new(&[libc::SIGUSR1]).unwrap());
            // SAFETY: pthread_self is the calling thread.
            let thread = unsafe { libc::pthread_self() } as usize;
            let sender = std::thread::spawn(move || {
                std::thread::sleep(std::time::Duration::from_millis(20));
                // SAFETY: the target thread blocks SIGUSR1 and outlives this call.
                unsafe { libc::pthread_kill(thread as libc::pthread_t, libc::SIGUSR1) };
            });
            let executor = crate::exec::LocalExecutor::new(Some(reactor.wake_handle()));
            let (r, f) = (reactor.clone(), fd.clone());
            let signal = executor
                .block_on(
                    &reactor,
                    async move { f.recv(&r, &crate::OpCtx::none()).await },
                )
                .unwrap();
            assert_eq!(signal.unwrap(), libc::SIGUSR1);
            sender.join().unwrap();
        })
        .join()
        .unwrap();
    }
}

/// Descriptor of the process stop eventfd, or -1 before [`stop_fd`].
static STOP_FD: std::sync::atomic::AtomicI32 = std::sync::atomic::AtomicI32::new(-1);

/// Serializes creation of the process stop eventfd.
static STOP_INIT: std::sync::Mutex<()> = std::sync::Mutex::new(());

/// The process stop eventfd, created on first use and never closed.
///
/// Kernel reactors created after this call poll it once, so a stop request
/// raised by a signal handler wakes every parked lane without a tick.
pub fn stop_fd() -> Result<RawFd> {
    use std::sync::atomic::Ordering;
    let _init = STOP_INIT.lock().unwrap_or_else(|e| e.into_inner());
    let fd = STOP_FD.load(Ordering::Acquire);
    if fd >= 0 {
        return Ok(fd);
    }
    // SAFETY: eventfd returns a fresh descriptor or fails.
    let fd = unsafe { libc::eventfd(0, libc::EFD_CLOEXEC | libc::EFD_NONBLOCK) };
    if fd < 0 {
        return Err(Error::last_os_error());
    }
    STOP_FD.store(fd, Ordering::Release);
    Ok(fd)
}

/// The process stop eventfd if [`stop_fd`] created it.
pub(crate) fn existing_stop_fd() -> Option<RawFd> {
    let fd = STOP_FD.load(std::sync::atomic::Ordering::Acquire);
    (fd >= 0).then_some(fd)
}

/// Set by [`raise_stop`], cleared by [`reset_stop`].
static STOP_RAISED: std::sync::atomic::AtomicBool = std::sync::atomic::AtomicBool::new(false);

/// Whether a stop was raised since the last [`reset_stop`]. One relaxed load,
/// cheap enough to check on every lane turn.
pub fn stop_raised() -> bool {
    STOP_RAISED.load(std::sync::atomic::Ordering::Relaxed)
}

/// Record a stop request and make the stop eventfd readable.
/// Async-signal-safe: one atomic store, one atomic load, and one `write(2)`;
/// the eventfd write does nothing before [`stop_fd`].
pub fn raise_stop() {
    STOP_RAISED.store(true, std::sync::atomic::Ordering::Relaxed);
    if let Some(fd) = existing_stop_fd() {
        let value = 1u64;
        // SAFETY: the descriptor is never closed; the value is a live u64.
        unsafe { libc::write(fd, (&value as *const u64).cast(), 8) };
    }
}

/// Consume a raised stop so later reactors do not wake for it.
pub fn reset_stop() {
    STOP_RAISED.store(false, std::sync::atomic::Ordering::Relaxed);
    if let Some(fd) = existing_stop_fd() {
        let mut value = 0u64;
        // SAFETY: nonblocking read into a local u64.
        unsafe { libc::read(fd, (&mut value as *mut u64).cast(), 8) };
    }
}

#[cfg(test)]
mod stop_tests {
    use super::*;

    /// A raised stop wakes a parked kernel reactor created after `stop_fd`.
    #[test]
    fn raised_stop_wakes_parked_reactor() {
        stop_fd().unwrap();
        reset_stop();
        let reactor = match crate::Reactor::new(
            crate::Driver::Uring,
            crate::ring::Config::default(),
            crate::WorkerCtx::host(),
        ) {
            Ok(reactor) => reactor,
            Err(_) if std::env::var_os("RUNTIME_REQUIRE_IO_URING").is_none() => return,
            Err(error) => panic!("{error:?}"),
        };
        let raiser = std::thread::spawn(|| {
            std::thread::sleep(std::time::Duration::from_millis(50));
            raise_stop();
        });
        let started = std::time::Instant::now();
        reactor
            .park(Some(started + std::time::Duration::from_secs(10)))
            .unwrap();
        assert!(started.elapsed() < std::time::Duration::from_secs(5));
        raiser.join().unwrap();
        assert!(stop_raised());
        reset_stop();
        assert!(!stop_raised());
    }
}
