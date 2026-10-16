//! Time and randomness sources.
//!
//! The runtime never reads ambient time or randomness directly; it asks its
//! [`Backend`](crate::Backend) for a [`Clock`] and an [`Entropy`] source, so a
//! simulation backend can make both deterministic.
//!
//! Design rule: clock and entropy handles are read on hot paths, so they must
//! be cheap to obtain and to clone: a zero-sized type, an `Rc`, or a borrow.
//! Implementations must never require an `Arc` clone or a lock per read.
//!
//! ```
//! use uring_runtime::{Clock, Entropy, SystemClock, SystemEntropy};
//!
//! let earlier = SystemClock.now();
//! assert!(SystemClock.now() >= earlier);
//!
//! let mut seed = [0u8; 16];
//! SystemEntropy.fill(&mut seed).unwrap();
//! ```

use std::time::{Instant, SystemTime};

use crate::{Error, Result};

/// A source of monotonic and wall-clock time.
pub trait Clock {
    /// The current monotonic instant.
    fn now(&self) -> Instant;

    /// The current wall-clock time.
    fn wall_now(&self) -> SystemTime;
}

/// The operating system clock (`Instant::now` and `SystemTime::now`).
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Hash)]
pub struct SystemClock;

impl Clock for SystemClock {
    fn now(&self) -> Instant {
        Instant::now()
    }

    fn wall_now(&self) -> SystemTime {
        SystemTime::now()
    }
}

impl<C: Clock + ?Sized> Clock for &C {
    fn now(&self) -> Instant {
        (**self).now()
    }

    fn wall_now(&self) -> SystemTime {
        (**self).wall_now()
    }
}

/// A source of random bytes.
pub trait Entropy {
    /// Fill `buf` entirely with random bytes.
    fn fill(&self, buf: &mut [u8]) -> Result<()>;
}

impl<E: Entropy + ?Sized> Entropy for &E {
    fn fill(&self, buf: &mut [u8]) -> Result<()> {
        (**self).fill(buf)
    }
}

/// The kernel's random number generator, read with `getrandom(2)`.
///
/// Interrupted calls are retried and short reads are continued until the
/// buffer is full.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Hash)]
pub struct SystemEntropy;

impl Entropy for SystemEntropy {
    fn fill(&self, buf: &mut [u8]) -> Result<()> {
        fill_with(buf, |chunk| {
            // SAFETY: `chunk` is a valid, writable region of `chunk.len()` bytes.
            unsafe { libc::getrandom(chunk.as_mut_ptr().cast(), chunk.len(), 0) }
        })
    }
}

/// Drive a `getrandom`-shaped call until `buf` is full.
fn fill_with(mut buf: &mut [u8], mut call: impl FnMut(&mut [u8]) -> isize) -> Result<()> {
    while !buf.is_empty() {
        let n = call(buf);
        if n < 0 {
            let error = Error::last_os_error();
            if error.is_interrupted() {
                continue;
            }
            return Err(error.with_context("getrandom"));
        }
        let n = usize::try_from(n).unwrap_or(0);
        if n == 0 || n > buf.len() {
            return Err(Error::from_errno(libc::EIO).with_context("getrandom"));
        }
        buf = &mut buf[n..];
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn set_errno(errno: i32) {
        // SAFETY: `__errno_location` returns the calling thread's errno slot.
        unsafe { *libc::__errno_location() = errno };
    }

    #[test]
    #[cfg_attr(miri, ignore = "wall-clock time is not available under Miri isolation")]
    fn system_clock_is_monotonic() {
        let clock = SystemClock;
        let a = clock.now();
        let b = Clock::now(&&clock);
        assert!(b >= a);
        assert!(clock.wall_now() > SystemTime::UNIX_EPOCH);
        assert_eq!(std::mem::size_of::<SystemClock>(), 0);
    }

    #[test]
    fn system_entropy_fills() {
        let mut a = [0u8; 64];
        let mut b = [0u8; 64];
        SystemEntropy.fill(&mut a).unwrap();
        Entropy::fill(&&SystemEntropy, &mut b).unwrap();
        assert_ne!(a, b);
        SystemEntropy.fill(&mut []).unwrap();
        let mut large = vec![0u8; 1 << 20];
        SystemEntropy.fill(&mut large).unwrap();
        assert!(large.iter().any(|&b| b != 0));
    }

    #[test]
    fn short_reads_and_eintr_are_retried() {
        let mut calls = 0;
        let mut buf = [0u8; 10];
        fill_with(&mut buf, |chunk| {
            calls += 1;
            if calls == 1 {
                set_errno(libc::EINTR);
                return -1;
            }
            let n = 3.min(chunk.len());
            chunk[..n].fill(7);
            n as isize
        })
        .unwrap();
        assert_eq!(buf, [7; 10]);
        assert_eq!(calls, 5);
    }

    #[test]
    fn errors_are_reported() {
        let mut buf = [0u8; 4];
        let error = fill_with(&mut buf, |_| {
            set_errno(libc::ENOSYS);
            -1
        })
        .unwrap_err();
        assert_eq!(error.raw_os_error(), Some(libc::ENOSYS));
        let error = fill_with(&mut buf, |_| 0).unwrap_err();
        assert_eq!(error.raw_os_error(), Some(libc::EIO));
        let error = fill_with(&mut buf, |_| 99).unwrap_err();
        assert_eq!(error.raw_os_error(), Some(libc::EIO));
    }
}
