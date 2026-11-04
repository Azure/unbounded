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
}
