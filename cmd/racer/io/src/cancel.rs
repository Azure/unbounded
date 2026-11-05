//! Cancellation tokens and absolute deadlines.
//!
//! A [`CancelToken`] is shared, `Send + Sync`, and cancels its children in
//! O(children). Waiting is waker based: registration stores one waker per
//! waiter and cancellation wakes them all, so nothing polls for cancellation.
use crate::Error;
use std::{
    future::Future,
    pin::Pin,
    sync::{
        Arc, Mutex, Weak,
        atomic::{AtomicU8, AtomicUsize, Ordering},
    },
    task::{Context, Poll, Waker},
    time::{Duration, Instant},
};

const LIVE: u8 = 0;
const CANCELLED: u8 = 1;
const EXPIRED: u8 = 2;

/// A shared cancellation flag with registered wakers and child tokens.
#[derive(Clone)]
pub struct CancelToken(Arc<Node>);

struct Node {
    state: AtomicU8,

    inner: Mutex<Inner>,
}

struct Inner {
    waiters: Vec<Waiter>,

    free: Vec<u32>,

    next_id: u64,

    children: Vec<Weak<Node>>,

    /// Child count that triggers pruning of dropped children (amortized O(1)).
    prune_at: usize,
}

struct Waiter {
    id: u64,

    waker: Option<Waker>,
}

/// Identifies one waiter registration on a [`CancelToken`].
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct WaitKey {
    index: u32,

    id: u64,
}

impl std::fmt::Debug for CancelToken {
    /// Show only the cancellation state.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_tuple("CancelToken").field(&self.reason()).finish()
    }
}

impl Default for CancelToken {
    /// A fresh, uncanceled root token.
    fn default() -> Self {
        Self::new()
    }
}

impl CancelToken {
    /// A fresh, uncanceled root token.
    pub fn new() -> Self {
        Self(Arc::new(Node {
            state: AtomicU8::new(LIVE),
            inner: Mutex::new(Inner {
                waiters: Vec::new(),
                free: Vec::new(),
                next_id: 0,
                children: Vec::new(),
                prune_at: 16,
            }),
        }))
    }

    /// A token canceled whenever this one is. Canceling the child does not
    /// affect the parent. A child of a canceled token starts canceled.
    pub fn child(&self) -> Self {
        let child = Self::new();
        let mut inner = self.lock();
        let state = self.0.state.load(Ordering::Acquire);
        if state != LIVE {
            drop(inner);
            child.cancel_with(reason(state));
            return child;
        }
        if inner.children.len() >= inner.prune_at {
            inner.children.retain(|node| node.strong_count() != 0);
            inner.prune_at = (inner.children.len() * 2).max(16);
        }
        inner.children.push(Arc::downgrade(&child.0));
        drop(inner);
        child
    }

    /// Whether cancellation was requested.
    #[inline]
    pub fn is_cancelled(&self) -> bool {
        self.0.state.load(Ordering::Acquire) != LIVE
    }

    /// The cancellation reason: [`Error::Cancelled`] or
    /// [`Error::DeadlineExceeded`].
    #[inline]
    pub fn reason(&self) -> Option<Error> {
        match self.0.state.load(Ordering::Acquire) {
            LIVE => None,
            state => Some(reason(state)),
        }
    }

    /// Fail with the cancellation reason, if any.
    #[inline]
    pub fn check(&self) -> crate::Result<()> {
        self.reason().map_or(Ok(()), Err)
    }

    /// Cancel with [`Error::Cancelled`].
    pub fn cancel(&self) {
        self.cancel_with(Error::Cancelled);
    }

    /// Cancel with `reason`; [`Error::DeadlineExceeded`] is kept, every other
    /// reason is recorded as [`Error::Cancelled`]. The first reason wins.
    pub fn cancel_with(&self, reason: Error) {
        let state = if reason == Error::DeadlineExceeded {
            EXPIRED
        } else {
            CANCELLED
        };
        if self
            .0
            .state
            .compare_exchange(LIVE, state, Ordering::AcqRel, Ordering::Acquire)
            .is_err()
        {
            return;
        }
        let (wakers, children) = {
            let mut inner = self.lock();
            let wakers: Vec<Waker> = inner
                .waiters
                .iter_mut()
                .filter_map(|waiter| waiter.waker.take())
                .collect();
            (wakers, std::mem::take(&mut inner.children))
        };
        for waker in wakers {
            waker.wake();
        }
        for child in children {
            if let Some(node) = child.upgrade() {
                Self(node).cancel_with(reason_of(state));
            }
        }
    }

    /// Register or refresh `waker` under `key`. Returns the reason if already
    /// canceled (the waker is then not retained).
    pub fn register(&self, key: &mut Option<WaitKey>, waker: &Waker) -> Option<Error> {
        if let Some(reason) = self.reason() {
            return Some(reason);
        }
        let mut inner = self.lock();
        if let Some(existing) = *key
            && let Some(waiter) = inner.waiters.get_mut(existing.index as usize)
            && waiter.id == existing.id
        {
            match &mut waiter.waker {
                Some(old) if old.will_wake(waker) => (),
                slot => *slot = Some(waker.clone()),
            }
        } else {
            inner.next_id += 1;
            let id = inner.next_id;
            let waiter = Waiter {
                id,
                waker: Some(waker.clone()),
            };
            let index = match inner.free.pop() {
                Some(index) => {
                    inner.waiters[index as usize] = waiter;
                    index
                }
                None => {
                    inner.waiters.push(waiter);
                    (inner.waiters.len() - 1) as u32
                }
            };
            *key = Some(WaitKey { index, id });
        }
        drop(inner);
        // Close the race with a concurrent cancel that ran before the insert.
        self.reason()
    }

    /// Remove a registration made by [`Self::register`].
    pub fn deregister(&self, key: &mut Option<WaitKey>) {
        let Some(key) = key.take() else {
            return;
        };
        let mut inner = self.lock();
        if let Some(waiter) = inner.waiters.get_mut(key.index as usize)
            && waiter.id == key.id
        {
            waiter.id = 0;
            let old = waiter.waker.take();
            inner.free.push(key.index);
            drop(inner);
            drop(old);
        }
    }

    /// Resolve with the reason once canceled.
    pub fn cancelled(&self) -> Cancelled<'_> {
        Cancelled {
            token: self,
            key: None,
        }
    }

    /// Live waiter registrations (diagnostics and tests).
    pub fn waiters(&self) -> usize {
        let inner = self.lock();
        inner.waiters.len() - inner.free.len()
    }

    /// Whether both handles name the same token.
    pub fn same(&self, other: &Self) -> bool {
        Arc::ptr_eq(&self.0, &other.0)
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, Inner> {
        // Wakers never run under this lock, so poisoning cannot leave it torn.
        self.0.inner.lock().unwrap_or_else(|e| e.into_inner())
    }
}

fn reason(state: u8) -> Error {
    if state == EXPIRED {
        Error::DeadlineExceeded
    } else {
        Error::Cancelled
    }
}

fn reason_of(state: u8) -> Error {
    reason(state)
}

/// Future returned by [`CancelToken::cancelled`].
pub struct Cancelled<'a> {
    token: &'a CancelToken,

    key: Option<WaitKey>,
}

impl Future for Cancelled<'_> {
    type Output = Error;

    /// Ready with the reason once the token is canceled.
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Error> {
        let this = self.get_mut();
        match this.token.register(&mut this.key, cx.waker()) {
            Some(reason) => {
                this.token.deregister(&mut this.key);
                Poll::Ready(reason)
            }
            None => Poll::Pending,
        }
    }
}

impl Drop for Cancelled<'_> {
    /// Remove the registration.
    fn drop(&mut self) {
        self.token.deregister(&mut self.key);
    }
}

/// An absolute monotonic deadline, or [`Deadline::NEVER`].
#[derive(Clone, Copy, Debug, Default, Eq, Hash, PartialEq)]
pub struct Deadline(Option<Instant>);

impl Deadline {
    /// No deadline. Use only for explicitly untimed fences.
    pub const NEVER: Self = Self(None);

    /// A deadline at `instant`.
    pub fn at(instant: Instant) -> Self {
        Self(Some(instant))
    }

    /// A deadline `duration` after `now`; saturates to [`Self::NEVER`].
    pub fn after(now: Instant, duration: Duration) -> Self {
        Self(now.checked_add(duration))
    }

    /// The deadline instant, if any.
    pub fn instant(self) -> Option<Instant> {
        self.0
    }

    /// Whether the deadline is at or before `now`.
    pub fn expired(self, now: Instant) -> bool {
        self.0.is_some_and(|deadline| deadline <= now)
    }

    /// Time left before the deadline (`None` for never).
    pub fn remaining(self, now: Instant) -> Option<Duration> {
        self.0
            .map(|deadline| deadline.saturating_duration_since(now))
    }

    /// The earlier of two deadlines.
    pub fn min(self, other: Self) -> Self {
        match (self.0, other.0) {
            (Some(a), Some(b)) => Self(Some(a.min(b))),
            (Some(a), None) | (None, Some(a)) => Self(Some(a)),
            (None, None) => Self(None),
        }
    }

    /// Fail with [`Error::DeadlineExceeded`] once expired.
    pub fn check(self, now: Instant) -> crate::Result<()> {
        if self.expired(now) {
            Err(Error::DeadlineExceeded)
        } else {
            Ok(())
        }
    }
}

impl From<Instant> for Deadline {
    /// A deadline at `instant`.
    fn from(instant: Instant) -> Self {
        Self::at(instant)
    }
}

/// Shared cancellation state with a caller-bounded set of operation wakeups.
#[derive(Clone)]
pub struct Cancellation {
    state: Arc<CancellationState>,
}

/// Operation-owned cancellation wake registration. Drop removes its live entry;
/// independent operations may safely register the same executor waker.
pub struct CancellationRegistration {
    cancellation: Cancellation,

    key: Mutex<Option<WaitKey>>,
}

/// Cancellation state shared by handles and their operation registrations.
struct CancellationState {
    token: CancelToken,

    registration_limit: usize,

    live: AtomicUsize,
}

impl Cancellation {
    /// Construct with a 1024-live-registration limit. Construction cannot fail;
    /// the Result contract is retained for existing callers.
    pub fn new() -> crate::Result<Self> {
        Ok(Self::with_registration_limit(1024))
    }

    /// Choose a subscription bound; zero still permits canceling and querying.
    fn with_registration_limit(registration_limit: usize) -> Self {
        Self {
            state: Arc::new(CancellationState {
                token: CancelToken::new(),
                registration_limit,
                live: AtomicUsize::new(0),
            }),
        }
    }

    /// The reactor cancellation token that this cancellation drives.
    pub fn token(&self) -> &CancelToken {
        &self.state.token
    }

    /// Whether any owner has requested cancellation.
    pub fn is_cancelled(&self) -> bool {
        self.state.token.is_cancelled()
    }

    /// Subscribe for one operation's lifetime, even when operations share a waker.
    /// Dropping the subscription reclaims its capacity and wake.
    pub fn subscribe(&self) -> crate::Result<CancellationRegistration> {
        self.state
            .live
            .fetch_update(Ordering::AcqRel, Ordering::Acquire, |live| {
                (live < self.state.registration_limit).then_some(live + 1)
            })
            .map_err(|_| Error::Overloaded)?;
        Ok(CancellationRegistration {
            cancellation: self.clone(),
            key: Mutex::new(None),
        })
    }

    /// Mark cancellation and wake live subscribers.
    pub fn cancel(&self) -> crate::Result<()> {
        self.state.token.cancel();
        Ok(())
    }
}

impl CancellationRegistration {
    /// Replace the waiting task and close the race with concurrent cancellation.
    pub fn register(&self, waker: &Waker) {
        let mut key = self.key.lock().unwrap_or_else(|poison| poison.into_inner());
        if self
            .cancellation
            .state
            .token
            .register(&mut key, waker)
            .is_some()
        {
            waker.wake_by_ref();
        }
    }
}

impl Drop for CancellationRegistration {
    /// Remove this subscription without removing other operations' wakes.
    fn drop(&mut self) {
        let key = self
            .key
            .get_mut()
            .unwrap_or_else(|poison| poison.into_inner());
        self.cancellation.state.token.deregister(key);
        self.cancellation.state.live.fetch_sub(1, Ordering::AcqRel);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::AtomicUsize;

    struct Count(AtomicUsize);

    impl std::task::Wake for Count {
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::SeqCst);
        }
    }

    #[test]
    fn cancel_wakes_registered_waiters_once_and_reports_reason() {
        let token = CancelToken::new();
        let count = Arc::new(Count(AtomicUsize::new(0)));
        let waker = Waker::from(count.clone());
        let (mut a, mut b) = (None, None);
        assert_eq!(token.register(&mut a, &waker), None);
        assert_eq!(token.register(&mut a, &waker), None);
        assert_eq!(token.register(&mut b, &waker), None);
        assert_eq!(token.waiters(), 2);
        token.deregister(&mut b);
        assert_eq!(token.waiters(), 1);
        token.cancel_with(Error::DeadlineExceeded);
        token.cancel();
        assert_eq!(count.0.load(Ordering::SeqCst), 1);
        assert_eq!(token.reason(), Some(Error::DeadlineExceeded));
        assert_eq!(
            token.register(&mut b, &waker),
            Some(Error::DeadlineExceeded)
        );
        token.deregister(&mut a);
        assert_eq!(token.waiters(), 0);
    }

    #[test]
    fn children_follow_parents_but_not_the_reverse() {
        let parent = CancelToken::new();
        let child = parent.child();
        let grandchild = child.child();
        let sibling = parent.child();
        sibling.cancel();
        assert!(!parent.is_cancelled());
        for _ in 0..1000 {
            drop(parent.child());
        }
        assert!(parent.lock().children.len() <= 32);
        parent.cancel();
        assert!(child.is_cancelled() && grandchild.is_cancelled());
        assert!(parent.child().is_cancelled());
        let expired = CancelToken::new();
        expired.cancel_with(Error::DeadlineExceeded);
        assert_eq!(expired.child().reason(), Some(Error::DeadlineExceeded));
    }

    #[test]
    fn cancelled_future_resolves_and_cancels_from_another_thread() {
        let token = CancelToken::new();
        let count = Arc::new(Count(AtomicUsize::new(0)));
        let waker = Waker::from(count.clone());
        let mut cx = Context::from_waker(&waker);
        let mut wait = Box::pin(token.cancelled());
        assert!(wait.as_mut().poll(&mut cx).is_pending());
        let remote = token.clone();
        std::thread::spawn(move || remote.cancel()).join().unwrap();
        assert_eq!(count.0.load(Ordering::SeqCst), 1);
        assert_eq!(wait.as_mut().poll(&mut cx), Poll::Ready(Error::Cancelled));
        drop(wait);
        assert_eq!(token.waiters(), 0);
    }

    #[test]
    fn deadlines_order_and_expire() {
        let now = Instant::now();
        let soon = Deadline::after(now, Duration::from_millis(1));
        assert!(!soon.expired(now));
        assert!(soon.expired(now + Duration::from_millis(1)));
        assert_eq!(soon.min(Deadline::NEVER), soon);
        assert_eq!(Deadline::NEVER.min(Deadline::NEVER), Deadline::NEVER);
        assert_eq!(Deadline::after(now, Duration::MAX), Deadline::NEVER);
        assert_eq!(
            soon.check(now + Duration::from_secs(1)),
            Err(Error::DeadlineExceeded)
        );
        assert_eq!(soon.remaining(now), Some(Duration::from_millis(1)));
    }

    /// Counts wakes, for cancellation registration tests.
    #[derive(Default)]
    struct WakeCount(AtomicUsize);

    impl WakeCount {
        fn count(&self) -> usize {
            self.0.load(Ordering::SeqCst)
        }
    }

    impl std::task::Wake for WakeCount {
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::SeqCst);
        }
    }

    /// Cancellation and registration races preserve notifications and capacity.
    #[test]
    fn caller_selected_registration_bound_and_concurrent_cancel() {
        let disabled = Cancellation::with_registration_limit(0);
        assert!(matches!(disabled.subscribe(), Err(Error::Overloaded)));
        disabled.cancel().unwrap();
        assert!(disabled.is_cancelled());
        for _ in 0..64 {
            let cancellation = Cancellation::with_registration_limit(1);
            let registration = cancellation.subscribe().unwrap();
            assert!(matches!(cancellation.subscribe(), Err(Error::Overloaded)));
            let count = Arc::new(WakeCount::default());
            let waker = Waker::from(count.clone());
            std::thread::scope(|threads| {
                threads.spawn(|| cancellation.cancel().unwrap());
                threads.spawn(|| registration.register(&waker));
            });
            assert!(count.count() >= 1);
            drop(registration);
            assert!(cancellation.subscribe().is_ok());
        }
    }

    /// Each subscription owns its capacity even when tasks use the same waker.
    #[test]
    fn operation_registrations_reclaim_capacity_and_do_not_remove_shared_wakes() {
        let cancellation = Cancellation::new().unwrap();
        let count = Arc::new(WakeCount::default());
        let waker = Waker::from(count.clone());
        for _ in 0..2048 {
            let registration = cancellation.subscribe().unwrap();
            registration.register(&waker);
        }
        assert_eq!(cancellation.state.live.load(Ordering::Acquire), 0);
        assert_eq!(cancellation.token().waiters(), 0);
        let first = cancellation.subscribe().unwrap();
        let second = cancellation.subscribe().unwrap();
        first.register(&waker);
        second.register(&waker);
        drop(first);
        cancellation.cancel().unwrap();
        assert_eq!(count.count(), 1);
    }

    /// Subscription drop releases executor ownership before the shared scope ends.
    #[test]
    fn dropping_subscription_releases_executor_resources_before_scope_ends() {
        let cancellation = Cancellation::new().unwrap();
        let executor = Arc::new(WakeCount::default());
        let weak = Arc::downgrade(&executor);
        let registration = cancellation.subscribe().unwrap();
        registration.register(&Waker::from(executor));
        assert!(weak.upgrade().is_some());
        drop(registration);
        assert!(weak.upgrade().is_none());
        assert!(!cancellation.is_cancelled());

        // A worker that subscribes after cancellation must still be notified.
        cancellation.cancel().unwrap();
        let executor = Arc::new(WakeCount::default());
        let registration = cancellation.subscribe().unwrap();
        registration.register(&Waker::from(executor.clone()));
        assert_eq!(executor.count(), 1);
    }

    /// Dropping one live registration immediately admits its replacement.
    #[test]
    fn cancellation_drives_the_reactor_token_and_its_waiters() {
        let cancellation = Cancellation::new().unwrap();
        let token = cancellation.token().clone();
        let registration = cancellation.subscribe().unwrap();
        let woke = Arc::new(AtomicUsize::new(0));
        struct Count(Arc<AtomicUsize>);
        impl std::task::Wake for Count {
            fn wake(self: Arc<Self>) {
                self.0.fetch_add(1, Ordering::AcqRel);
            }
        }
        let waker = Waker::from(Arc::new(Count(woke.clone())));
        registration.register(&waker);
        assert_eq!(token.waiters(), 1);
        assert_eq!(token.check(), Ok(()));
        cancellation.cancel().unwrap();
        assert_eq!(woke.load(Ordering::Acquire), 1);
        assert_eq!(token.check(), Err(Error::Cancelled));
        // Registering after cancellation wakes immediately.
        registration.register(&waker);
        assert_eq!(woke.load(Ordering::Acquire), 2);
        drop(registration);
        assert_eq!(token.waiters(), 0);
    }

    #[test]
    fn live_registration_limit_is_reclaimed_after_drop() {
        let cancellation = Cancellation::new().unwrap();
        let mut registrations: Vec<_> = (0..1024)
            .map(|_| cancellation.subscribe().unwrap())
            .collect();
        assert!(matches!(cancellation.subscribe(), Err(Error::Overloaded)));
        registrations.pop();
        assert!(cancellation.subscribe().is_ok());
    }
}
