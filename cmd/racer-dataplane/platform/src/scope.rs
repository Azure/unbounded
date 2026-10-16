//! Caller-owned operation policy: scopes, budgets and cancellation.
//!
//! [`Scope`] is the library's [`uring::Scope`] with the extra bounds Racer's
//! state machines rely on (`Copy` errors that can represent [`Error`]). It is
//! implemented automatically: implement [`LibraryScope`] (the library trait)
//! for a type and it becomes a Racer [`Scope`] whenever its error qualifies.
//!
//! Cancellation is the library's [`CancelToken`], re-exported under its old
//! name [`Cancellation`]. [`CancellationRegistration`] keeps the old
//! "subscribe once, re-register every poll through `&self`" shape on top of
//! [`uring::CancelRegistration`].

use crate::{Error, Result};
use std::{sync::Mutex, task::Waker};

pub use uring::scope::Scope as LibraryScope;
pub use uring::{CancelRegistration, CancelToken, Unscoped};

/// Shared cross-thread cancellation state (the library [`CancelToken`]).
///
/// Differences from the old runtime type: `new()` and `cancel()` are
/// infallible, `is_cancelled` is spelled [`CancelToken::is_canceled`], and
/// there is no registration limit.
pub type Cancellation = CancelToken;

/// Caller-owned operation deadline and cancellation policy.
///
/// This is a blanket sub-trait of [`LibraryScope`]: every library scope whose
/// error is `Copy + Send` and can represent a Racer [`Error`] is a Racer
/// scope. Do not implement it directly; implement [`LibraryScope`] instead,
/// which also requires `Error: From<uring::Error>` (usually written as
/// `crate::Error::from(error).into()`).
pub trait Scope: LibraryScope<Error: Copy + Send + 'static + From<Error>> {}

impl<S> Scope for S where S: LibraryScope<Error: Copy + Send + 'static + From<Error>> {}

/// Accounting hook. Charges stay alive until their resources are fenced.
pub trait Budget: 'static {
    /// An owned accounting guard retained until the charged resource is fenced.
    type Charge: 'static;

    /// Reserve accounting capacity for bytes without choosing application policy.
    fn charge(&self, bytes: usize) -> Result<Self::Charge>;
}

impl Budget for () {
    /// No accounting guard is needed when the caller disables charging.
    type Charge = ();

    /// Accept every charge without retaining application capacity.
    fn charge(&self, _bytes: usize) -> Result<()> {
        Ok(())
    }
}

/// Subscribe to a [`Cancellation`] for one operation's lifetime.
pub trait CancellationExt {
    /// Subscribe for one operation's lifetime, even when operations share a
    /// waker. Dropping the subscription removes its wake entry. Unlike the old
    /// runtime this cannot fail: registrations are unbounded.
    fn subscribe(&self) -> CancellationRegistration;
}

impl CancellationExt for CancelToken {
    fn subscribe(&self) -> CancellationRegistration {
        CancellationRegistration::new(self)
    }
}

/// Operation-owned cancellation wake registration. Drop removes its live entry;
/// independent operations may safely register the same executor waker.
#[derive(Debug)]
pub struct CancellationRegistration {
    registration: Mutex<CancelRegistration>,
}

impl CancellationRegistration {
    /// Subscribe to `token` without a waiting task yet.
    pub fn new(token: &CancelToken) -> Self {
        Self {
            registration: Mutex::new(token.register(Waker::noop())),
        }
    }

    /// Replace the waiting task and close the race with concurrent cancellation:
    /// an already canceled token wakes `waker` immediately.
    pub fn register(&self, waker: &Waker) {
        self.lock().update(waker);
    }

    /// The token this registration observes.
    pub fn token(&self) -> CancelToken {
        self.lock().token().clone()
    }

    /// Lock the registration, ignoring poisoning (no invariant spans a panic).
    fn lock(&self) -> std::sync::MutexGuard<'_, CancelRegistration> {
        self.registration
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::test_util::WakeCounter;
    use std::sync::Arc;

    #[derive(Clone)]
    struct Racer(Cancellation);

    impl LibraryScope for Racer {
        type Error = Error;

        fn check(&self) -> Result<()> {
            if self.0.is_canceled() {
                Err(Error::Canceled)
            } else {
                Ok(())
            }
        }

        fn cancellation(&self) -> Option<&Cancellation> {
            Some(&self.0)
        }
    }

    fn require_scope<S: Scope>(scope: &S) -> Result<(), S::Error> {
        scope.check()?;
        Err(Error::Overloaded.into())
    }

    /// Library scopes with qualifying errors are Racer scopes automatically.
    #[test]
    fn blanket_scope_keeps_old_bounds() {
        let scope = Racer(Cancellation::new());
        assert_eq!(require_scope(&scope), Err(Error::Overloaded));
        scope.0.cancel();
        assert_eq!(require_scope(&scope), Err(Error::Canceled));
    }

    /// Registrations wake the latest waker once and immediately after cancellation.
    #[test]
    fn registration_wakes_latest_waker_and_after_cancel() {
        let token = Cancellation::new();
        let registration = token.subscribe();
        let first = Arc::new(WakeCounter::default());
        let second = Arc::new(WakeCounter::default());
        registration.register(&Waker::from(first.clone()));
        registration.register(&Waker::from(second.clone()));
        assert_eq!(token.registrations(), 1);
        token.cancel();
        assert_eq!((first.count(), second.count()), (0, 1));
        registration.register(&Waker::from(first.clone()));
        assert_eq!(first.count(), 1);
        assert!(registration.token().is_canceled());
        drop(registration);
        assert_eq!(token.registrations(), 0);
    }

    /// Concurrent cancellation and registration never lose the notification.
    #[test]
    fn concurrent_cancel_and_register_notify() {
        for _ in 0..64 {
            let cancellation = Cancellation::new();
            let registration = cancellation.subscribe();
            let count = Arc::new(WakeCounter::default());
            let waker = Waker::from(count.clone());
            std::thread::scope(|threads| {
                threads.spawn(|| cancellation.cancel());
                threads.spawn(|| registration.register(&waker));
            });
            assert!(count.count() >= 1);
        }
    }

    /// Each subscription owns its entry even when operations share a waker.
    #[test]
    fn registrations_reclaim_entries_and_do_not_remove_shared_wakes() {
        let cancellation = Cancellation::new();
        let count = Arc::new(WakeCounter::default());
        let waker = Waker::from(count.clone());
        for _ in 0..2048 {
            let registration = cancellation.subscribe();
            registration.register(&waker);
        }
        assert_eq!(cancellation.registrations(), 0);
        let first = cancellation.subscribe();
        let second = cancellation.subscribe();
        first.register(&waker);
        second.register(&waker);
        drop(first);
        cancellation.cancel();
        assert_eq!(count.count(), 1);
    }

    /// Subscription drop releases the waker before the shared scope ends.
    #[test]
    fn dropping_subscription_releases_waker_before_scope_ends() {
        let cancellation = Cancellation::new();
        let executor = Arc::new(WakeCounter::default());
        let weak = Arc::downgrade(&executor);
        let registration = cancellation.subscribe();
        registration.register(&Waker::from(executor));
        assert!(weak.upgrade().is_some());
        drop(registration);
        assert!(weak.upgrade().is_none());
        assert!(!cancellation.is_canceled());

        // A worker that subscribes after cancellation must still be notified.
        cancellation.cancel();
        let executor = Arc::new(WakeCounter::default());
        let registration = cancellation.subscribe();
        registration.register(&Waker::from(executor.clone()));
        assert_eq!(executor.count(), 1);
    }

    /// The unit budget accepts every charge.
    #[test]
    fn unit_budget_accepts_charges() {
        assert!(().charge(usize::MAX).is_ok());
    }
}
