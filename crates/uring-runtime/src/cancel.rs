//! Cross-thread cancellation tokens.
//!
//! A [`CancelToken`] is a cheap, cloneable, `Send + Sync` flag that can be
//! canceled once from any thread. Futures observe it either by polling
//! [`CancelToken::canceled`] or by holding a [`CancelRegistration`] that wakes a
//! task when the token is canceled.
//!
//! Tokens form a tree: a [`child`](CancelToken::child) is canceled whenever its
//! parent is, but canceling a child does not affect the parent.
//!
//! ```
//! use uring_runtime::CancelToken;
//!
//! let parent = CancelToken::new();
//! let child = parent.child();
//! parent.cancel();
//! assert!(child.is_canceled());
//! ```

use std::future::Future;
use std::pin::Pin;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, MutexGuard, Weak};
use std::task::{Context, Poll, Waker};

/// A cloneable, thread-safe cancellation flag.
///
/// Clones share state: canceling any clone cancels all of them. Registrations
/// are stored in a slab with O(1) insertion and removal and no fixed limit.
#[derive(Clone)]
pub struct CancelToken {
    inner: Arc<Inner>,
}

struct Inner {
    canceled: AtomicBool,
    entries: Mutex<Slab>,
    /// Link into the parent's slab, removed when this token's state is dropped.
    parent: Option<(Arc<Inner>, usize)>,
}

enum Entry {
    Vacant(Option<usize>),
    Waker(Option<Waker>),
    Child(Weak<Inner>),
}

#[derive(Default)]
struct Slab {
    entries: Vec<Entry>,
    free: Option<usize>,
    live: usize,
}

impl Slab {
    fn insert(&mut self, entry: Entry) -> usize {
        self.live += 1;
        match self.free {
            Some(key) => {
                let Entry::Vacant(next) = std::mem::replace(&mut self.entries[key], entry) else {
                    unreachable!("free list points at an occupied entry");
                };
                self.free = next;
                key
            }
            None => {
                self.entries.push(entry);
                self.entries.len() - 1
            }
        }
    }

    fn remove(&mut self, key: usize) {
        if let Some(slot) = self.entries.get_mut(key)
            && !matches!(slot, Entry::Vacant(_))
        {
            *slot = Entry::Vacant(self.free);
            self.free = Some(key);
            self.live -= 1;
        }
    }
}

fn lock(mutex: &Mutex<Slab>) -> MutexGuard<'_, Slab> {
    // The slab is consistent after every operation, so a poisoned lock (a
    // panicking waker clone) is safe to keep using.
    mutex.lock().unwrap_or_else(|e| e.into_inner())
}

impl Drop for Inner {
    fn drop(&mut self) {
        if let Some((parent, key)) = self.parent.take() {
            lock(&parent.entries).remove(key);
        }
    }
}

impl CancelToken {
    /// Create a token that is not canceled.
    pub fn new() -> Self {
        Self {
            inner: Arc::new(Inner {
                canceled: AtomicBool::new(false),
                entries: Mutex::new(Slab::default()),
                parent: None,
            }),
        }
    }

    /// Create a child token that is canceled when `self` is canceled.
    ///
    /// The child keeps the parent's state alive. If `self` is already
    /// canceled, the child starts canceled.
    pub fn child(&self) -> Self {
        let mut entries = lock(&self.inner.entries);
        if self.is_canceled() {
            drop(entries);
            let child = Self::new();
            child.inner.canceled.store(true, Ordering::Release);
            return child;
        }
        let child = Arc::new_cyclic(|weak| {
            let key = entries.insert(Entry::Child(weak.clone()));
            Inner {
                canceled: AtomicBool::new(false),
                entries: Mutex::new(Slab::default()),
                parent: Some((self.inner.clone(), key)),
            }
        });
        Self { inner: child }
    }

    /// Whether this token (or an ancestor) has been canceled.
    pub fn is_canceled(&self) -> bool {
        self.inner.canceled.load(Ordering::Acquire)
    }

    /// Cancel this token and all its descendants, waking every registration.
    ///
    /// Idempotent. Wakers run after the internal lock has been released, so a
    /// waker may freely call back into the token.
    pub fn cancel(&self) {
        let mut pending = vec![self.inner.clone()];
        while let Some(inner) = pending.pop() {
            if inner.canceled.swap(true, Ordering::AcqRel) {
                continue;
            }
            let mut wakers = Vec::new();
            {
                let mut entries = lock(&inner.entries);
                for entry in &mut entries.entries {
                    match entry {
                        Entry::Waker(waker) => wakers.extend(waker.take()),
                        Entry::Child(child) => pending.extend(child.upgrade()),
                        Entry::Vacant(_) => {}
                    }
                }
            }
            for waker in wakers {
                waker.wake();
            }
        }
    }

    /// Register `waker` to be woken when the token is canceled.
    ///
    /// If the token is already canceled, `waker` is woken immediately. The
    /// registration is removed when the returned value is dropped; use
    /// [`CancelRegistration::update`] to replace the waker on re-poll.
    pub fn register(&self, waker: &Waker) -> CancelRegistration {
        let mut registration = CancelRegistration {
            token: self.clone(),
            key: None,
        };
        registration.update(waker);
        registration
    }

    /// A future that resolves once the token is canceled.
    pub fn canceled(&self) -> Canceled {
        Canceled {
            token: self.clone(),
            registration: None,
        }
    }

    /// The number of live waker registrations and child links (for tests and
    /// diagnostics).
    pub fn registrations(&self) -> usize {
        lock(&self.inner.entries).live
    }
}

impl Default for CancelToken {
    fn default() -> Self {
        Self::new()
    }
}

impl std::fmt::Debug for CancelToken {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("CancelToken")
            .field("canceled", &self.is_canceled())
            .finish()
    }
}

/// A waker registration on a [`CancelToken`], removed on drop.
#[derive(Debug)]
pub struct CancelRegistration {
    token: CancelToken,
    key: Option<usize>,
}

impl CancelRegistration {
    /// Replace the registered waker.
    ///
    /// Closes the race with a concurrent [`CancelToken::cancel`]: if the token
    /// is (or becomes) canceled, `waker` is guaranteed to be woken, either by
    /// the canceling thread or immediately by this call.
    pub fn update(&mut self, waker: &Waker) {
        let inner = &self.token.inner;
        let mut entries = lock(&inner.entries);
        // `cancel` sets the flag before taking the lock, so checking under
        // the lock means either we see the flag or `cancel` sees our waker.
        if inner.canceled.load(Ordering::Acquire) {
            drop(entries);
            waker.wake_by_ref();
            return;
        }
        match self.key {
            Some(key) => match &mut entries.entries[key] {
                Entry::Waker(slot) => match slot {
                    Some(old) if old.will_wake(waker) => {}
                    _ => *slot = Some(waker.clone()),
                },
                _ => unreachable!("registration key points at a foreign entry"),
            },
            None => self.key = Some(entries.insert(Entry::Waker(Some(waker.clone())))),
        }
    }

    /// The token this registration observes.
    pub fn token(&self) -> &CancelToken {
        &self.token
    }
}

impl Drop for CancelRegistration {
    fn drop(&mut self) {
        if let Some(key) = self.key {
            lock(&self.token.inner.entries).remove(key);
        }
    }
}

/// Future returned by [`CancelToken::canceled`].
#[derive(Debug)]
#[must_use = "futures do nothing unless polled"]
pub struct Canceled {
    token: CancelToken,
    registration: Option<CancelRegistration>,
}

impl Future for Canceled {
    type Output = ();

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
        let this = self.get_mut();
        if this.token.is_canceled() {
            this.registration = None;
            return Poll::Ready(());
        }
        match &mut this.registration {
            Some(registration) => registration.update(cx.waker()),
            None => this.registration = Some(this.token.register(cx.waker())),
        }
        if this.token.is_canceled() {
            this.registration = None;
            Poll::Ready(())
        } else {
            Poll::Pending
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::AtomicUsize;
    use std::task::Wake;

    struct Counter(AtomicUsize);

    impl Wake for Counter {
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::SeqCst);
        }
    }

    fn counter() -> (Arc<Counter>, Waker) {
        let counter = Arc::new(Counter(AtomicUsize::new(0)));
        (counter.clone(), Waker::from(counter))
    }

    #[test]
    fn cancel_wakes_registrations_once() {
        let token = CancelToken::new();
        let (count, waker) = counter();
        let _a = token.register(&waker);
        let _b = token.register(&waker);
        assert!(!token.is_canceled());
        token.cancel();
        token.cancel();
        assert!(token.is_canceled());
        assert_eq!(count.0.load(Ordering::SeqCst), 2);
    }

    #[test]
    fn register_after_cancel_wakes_immediately() {
        let token = CancelToken::new();
        token.cancel();
        let (count, waker) = counter();
        let registration = token.register(&waker);
        assert_eq!(count.0.load(Ordering::SeqCst), 1);
        assert_eq!(registration.key, None);
        assert_eq!(token.registrations(), 0);
    }

    #[test]
    fn drop_reclaims_slot() {
        let token = CancelToken::new();
        let (count, waker) = counter();
        let a = token.register(&waker);
        let b = token.register(&waker);
        assert_eq!(token.registrations(), 2);
        drop(a);
        assert_eq!(token.registrations(), 1);
        let c = token.register(&waker);
        // The freed slot is reused rather than growing the slab.
        assert_eq!(token.inner.entries.lock().unwrap().entries.len(), 2);
        drop((b, c));
        assert_eq!(token.registrations(), 0);
        token.cancel();
        assert_eq!(count.0.load(Ordering::SeqCst), 0);
    }

    #[test]
    fn update_replaces_waker() {
        let token = CancelToken::new();
        let (first, first_waker) = counter();
        let (second, second_waker) = counter();
        let mut registration = token.register(&first_waker);
        registration.update(&second_waker);
        token.cancel();
        assert_eq!(first.0.load(Ordering::SeqCst), 0);
        assert_eq!(second.0.load(Ordering::SeqCst), 1);
    }

    #[test]
    fn children_follow_parent_only() {
        let parent = CancelToken::new();
        let child = parent.child();
        let grandchild = child.child();
        let (count, waker) = counter();
        let _registration = grandchild.register(&waker);
        child.cancel();
        assert!(!parent.is_canceled());
        assert!(grandchild.is_canceled());
        assert_eq!(count.0.load(Ordering::SeqCst), 1);

        let other = parent.child();
        parent.cancel();
        assert!(other.is_canceled());
        assert!(parent.child().is_canceled());
    }

    #[test]
    fn dropped_child_unlinks() {
        let parent = CancelToken::new();
        let child = parent.child();
        let clone = child.clone();
        assert_eq!(parent.registrations(), 1);
        drop(child);
        assert_eq!(parent.registrations(), 1);
        drop(clone);
        assert_eq!(parent.registrations(), 0);
        parent.cancel();
    }

    #[test]
    fn waker_may_reenter_token() {
        struct Reenter(CancelToken, AtomicUsize);
        impl Wake for Reenter {
            fn wake(self: Arc<Self>) {
                // Would deadlock if called while the slab lock is held.
                let _ = self.0.registrations();
                self.0.cancel();
                self.1.fetch_add(1, Ordering::SeqCst);
            }
        }
        let token = CancelToken::new();
        let reenter = Arc::new(Reenter(token.clone(), AtomicUsize::new(0)));
        let waker = Waker::from(reenter.clone());
        let _registration = token.register(&waker);
        token.cancel();
        assert_eq!(reenter.1.load(Ordering::SeqCst), 1);
    }

    #[test]
    fn concurrent_cancel_and_register_never_lose_wake() {
        for _ in 0..200 {
            let token = CancelToken::new();
            let (count, waker) = counter();
            let canceler = {
                let token = token.clone();
                std::thread::spawn(move || token.cancel())
            };
            let registration = token.register(&waker);
            canceler.join().unwrap();
            assert!(token.is_canceled());
            assert_eq!(count.0.load(Ordering::SeqCst), 1);
            drop(registration);
        }
    }

    #[test]
    fn canceled_future_resolves() {
        let token = CancelToken::new();
        let mut future = token.canceled();
        let (count, waker) = counter();
        let mut cx = Context::from_waker(&waker);
        assert!(Pin::new(&mut future).poll(&mut cx).is_pending());
        assert!(Pin::new(&mut future).poll(&mut cx).is_pending());
        assert_eq!(token.registrations(), 1);
        let remote = token.clone();
        std::thread::spawn(move || remote.cancel()).join().unwrap();
        assert_eq!(count.0.load(Ordering::SeqCst), 1);
        assert!(Pin::new(&mut future).poll(&mut cx).is_ready());
        assert_eq!(token.registrations(), 0);
        futures::executor::block_on(token.canceled());
    }
}
