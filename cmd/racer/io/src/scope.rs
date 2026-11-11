//! Caller-owned request policy shared by lifecycle and I/O drivers.
//!
//! A [`Scope`] answers "may this work still run": admission, cancellation,
//! and deadline checks. Cancellation wakes come from the scope's
//! [`CancelToken`]; [`CancelWait`] holds one operation's wake registration.

use std::{future::Future, pin::Pin, task::Waker, time::Duration};

use crate::{CancelToken, Error, OpCtx, cancel::WaitKey};

/// A boxed, single-thread lifecycle or driver operation.
pub type Operation<'a, T, E = Error> = Pin<Box<dyn Future<Output = Result<T, E>> + 'a>>;

/// Caller-owned admission, cancellation, and deadline policy.
pub trait Scope: Clone + 'static {
    /// Caller-owned failures that can represent runtime failures. Equality
    /// lets lifecycle code tell a requested stop (`Error::Canceled`) from a
    /// real failure.
    type Error: Copy + Send + 'static + From<Error> + PartialEq;

    /// Check admission, cancellation, and deadline policy before starting work.
    fn check(&self) -> Result<(), Self::Error>;

    /// Supply cancellation wakeups, or leave wake scheduling entirely to the owner.
    fn cancel_token(&self) -> Option<&CancelToken> {
        None
    }

    /// Longest a checked steady-state run may park before rechecking this
    /// scope; its cancellation wakes the run sooner. `None` means only
    /// cancellation can fail the scope. The default polls every millisecond.
    fn recheck_within(&self) -> Option<Duration> {
        Some(Duration::from_millis(1))
    }

    /// Whether [`Self::check`] can fail for in-flight operations without the
    /// [`Self::op_ctx`] cancel token or deadline firing. The reactor then
    /// rescans such operations on a short interval while it is parked.
    fn polled_checks(&self) -> bool {
        true
    }

    /// The reactor operation context: cancellation token and deadline.
    fn op_ctx(&self) -> OpCtx {
        match self.cancel_token() {
            Some(token) => OpCtx::with_cancel(token),
            None => OpCtx::none(),
        }
    }

    /// One operation's cancellation wake registration, if the scope can be
    /// canceled.
    fn cancel_wait(&self) -> Option<CancelWait> {
        self.cancel_token().map(CancelToken::wait)
    }
}

/// One operation's cancellation wake registration. Drop removes its entry;
/// independent operations may register the same waker.
pub struct CancelWait {
    token: CancelToken,

    key: std::sync::Mutex<Option<WaitKey>>,
}

impl CancelWait {
    /// Register or refresh `waker`, waking it at once if already canceled.
    pub fn register(&self, waker: &Waker) {
        let mut key = self.key.lock().unwrap_or_else(|e| e.into_inner());
        if self.token.register(&mut key, waker).is_some() {
            waker.wake_by_ref();
        }
    }

    /// The token this registration observes.
    pub fn token(&self) -> &CancelToken {
        &self.token
    }
}

impl Drop for CancelWait {
    fn drop(&mut self) {
        let key = self.key.get_mut().unwrap_or_else(|e| e.into_inner());
        self.token.deregister(key);
    }
}

impl CancelToken {
    /// Start an empty wake registration on this token.
    pub fn wait(&self) -> CancelWait {
        CancelWait {
            token: self.clone(),
            key: std::sync::Mutex::new(None),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    };

    struct Count(AtomicUsize);

    impl std::task::Wake for Count {
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::SeqCst);
        }
    }

    /// A registered wait is woken by cancel, a pre-canceled token wakes at
    /// registration, and drop releases the registration.
    #[test]
    fn cancel_wait_wakes_and_releases() {
        let count = Arc::new(Count(AtomicUsize::new(0)));
        let waker = Waker::from(count.clone());
        let token = CancelToken::new();
        let wait = token.wait();
        wait.register(&waker);
        wait.register(&waker);
        assert_eq!(token.waiters(), 1);
        token.cancel();
        assert_eq!(count.0.load(Ordering::SeqCst), 1);
        let late = token.wait();
        late.register(&waker);
        assert_eq!(count.0.load(Ordering::SeqCst), 2);
        drop((wait, late));

        let token = CancelToken::new();
        let wait = token.wait();
        wait.register(&waker);
        drop(wait);
        assert_eq!(token.waiters(), 0);
    }

    #[derive(Clone)]
    struct Plain;

    impl Scope for Plain {
        type Error = Error;

        fn check(&self) -> crate::Result<()> {
            Ok(())
        }
    }

    /// A scope without a token has no cancellation wait and an empty context.
    #[test]
    fn default_scope_has_no_cancellation() {
        assert!(Plain.cancel_wait().is_none());
        assert_eq!(Plain.recheck_within(), Some(Duration::from_millis(1)));
        assert!(Plain.polled_checks());
    }
}
