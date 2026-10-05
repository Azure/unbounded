//! Explicitly driven worker-local I/O and pinned thread groups.
//!
//! Scheduling and resource policy belong to the caller. Simulation is opt-in.
//! Generic mechanisms accept caller-owned scope, budget, result, and scheduling
//! hooks. Request models, admission classes, retry policy, entropy domain choices,
//! and service-graph ownership belong in application adapters, not this crate.
#![deny(unsafe_op_in_unsafe_fn)]

pub mod channel;

pub mod drivers;

pub mod environment;

pub mod group;

pub mod offload;

pub mod reactor;

#[cfg(any(test, feature = "test-util"))]
/// Optional shared wake instrumentation for tests, disabled in production builds.
pub mod test_util {
    use std::sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    };

    /// Counts owned and borrowed wakes across threads.
    #[derive(Default)]
    pub struct WakeCounter(AtomicUsize);

    impl WakeCounter {
        /// Read the number of wake calls observed so far.
        pub fn count(&self) -> usize {
            self.0.load(Ordering::SeqCst)
        }
    }

    impl std::task::Wake for WakeCounter {
        /// Count an owned wake; borrowed wakes use the trait's default forwarding.
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::SeqCst);
        }
    }
}

use std::{future::Future, pin::Pin};

/// Portable runtime failures, preserving terminal operating-system diagnostics.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    /// The operation's owner requested cancellation.
    Cancelled,
    /// The caller's deadline expired.
    DeadlineExceeded,
    /// A bounded queue or resource has no available capacity.
    Overloaded,
    /// A required resource cannot currently be used.
    Unavailable,
    /// An input value is invalid or cannot be represented.
    InvalidInput,
    /// The runtime was configured inconsistently or a required scope is absent.
    InvalidConfiguration,
    /// The requested resource does not exist.
    NotFound,
    /// The requested resource already exists.
    AlreadyExists,
    /// An I/O failure without a usable operating-system error code.
    Io,
    /// A terminal OS failure. This is diagnostic, not an instruction to retry.
    Os(i32),
}

impl Error {
    /// Preserve positive errno values while classifying common resource failures.
    pub fn from_io(error: std::io::Error) -> Self {
        match error.raw_os_error() {
            Some(libc::ENOENT) => Self::NotFound,
            Some(libc::EEXIST) => Self::AlreadyExists,
            Some(libc::ECANCELED) => Self::Cancelled,
            Some(errno) if errno > 0 => Self::Os(errno),
            _ => Self::Io,
        }
    }
}

impl std::fmt::Display for Error {
    /// Describe the failure without losing raw operating-system diagnostics.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::Cancelled => "operation canceled",
            Self::DeadlineExceeded => "deadline exceeded",
            Self::Overloaded => "capacity exhausted",
            Self::Unavailable => "resource unavailable",
            Self::InvalidInput => "invalid input",
            Self::InvalidConfiguration => "invalid configuration",
            Self::NotFound => "resource not found",
            Self::AlreadyExists => "resource already exists",
            Self::Io => "I/O failure",
            Self::Os(errno) => return write!(f, "OS error {errno}"),
        })
    }
}

// Preserve Copy errors, including raw errno, without fabricating an error source.
impl std::error::Error for Error {}

/// A runtime result, optionally using an application-owned error type.
pub type Result<T, E = Error> = std::result::Result<T, E>;

/// Worker-local future; no hidden executor and no Send requirement.
pub type Operation<'a, T, E = Error> = Pin<Box<dyn Future<Output = Result<T, E>> + 'a>>;

/// Caller-owned operation deadline and cancellation policy.
pub trait Scope: Clone + 'static {
    /// Caller-owned failures that can represent runtime failures.
    type Error: Copy + Send + 'static + From<Error>;

    /// Check admission, cancellation, and deadline policy before starting work.
    fn check(&self) -> Result<(), Self::Error>;

    /// Supply cancellation wakeups, or leave wake scheduling entirely to the owner.
    fn cancellation(&self) -> Option<&environment::Cancellation> {
        None
    }
}

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

/// Bounded owned handoffs. Cancellation is notification, never a completion fence.
pub mod mailbox {
    /// Explicit local execution of accepted mailbox commands.
    pub mod executor;
    use crate::{Error, Result, Scope, environment::CancellationRegistration};
    use futures::task::AtomicWaker;
    use std::{
        collections::VecDeque,
        future::Future,
        pin::Pin,
        sync::{
            Arc, Mutex, MutexGuard,
            atomic::{AtomicBool, AtomicUsize, Ordering},
        },
        task::{Context, Poll, Waker},
    };

    /// Completed work and its retained application budget.
    pub struct Completion<V, B> {
        /// Caller-defined result, independent of mailbox delivery status.
        pub value: V,

        /// Budget retained through execution and transferred to the consumer.
        pub budget: Option<B>,
    }

    /// Owned command queue with capacity shared by commands and unread receipts.
    /// Credits do not retain the mailbox, so queued commands cannot form a cycle.
    /// Active work must be fenced by the caller; mailbox drop is not an I/O fence.
    pub struct Mailbox<W, V, B, S: Scope> {
        capacity: usize,

        state: Mutex<State<W, V, B, S>>,

        credits: Arc<Credits>,
    }

    /// The driver must retain the permit until actual completion, even on cancel.
    pub struct Command<W, V, B, S: Scope> {
        /// Caller-assigned identity accepted by the reply exactly once.
        pub generation: u64,

        /// Work whose ownership was transferred on admission.
        pub work: W,

        /// Original caller policy, retained independently of the receipt.
        pub scope: S,

        /// Application capacity retained through execution.
        pub budget: Option<B>,

        /// Destination for the generation-checked completion.
        pub reply: Arc<Reply<V, B>>,

        /// Execution owner; retain every clone until external resources are safe.
        pub permit: Arc<Producer<V, B>>,
    }

    /// Consumer-side future whose drop abandons delivery, not accepted work.
    /// Use [`Self::poll_completion`] when cancellation must not end the wait early.
    pub struct Receipt<W, V, B, S: Scope> {
        reply: Arc<Reply<V, B>>,

        scope: S,

        cancellation: Option<CancellationRegistration>,

        permit: Arc<Permit>,

        work: std::marker::PhantomData<fn() -> W>,
    }

    /// A generation-checked, one-shot result slot shared by producer and receipt.
    pub struct Reply<V, B> {
        generation: u64,

        abandoned: AtomicBool,

        state: Mutex<ReplyState<V, B>>,
    }

    /// Keep this owner through execution and drop only after resources are fenced.
    /// Its loss terminates delivery; it is not itself an I/O fence.
    pub struct Producer<V, B> {
        reply: Arc<Reply<V, B>>,

        credit: Arc<Permit>,
    }

    /// A completion targeted another generation or a reply that already finished.
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub struct StaleCompletion;

    impl<W, V, B, S: Scope> Mailbox<W, V, B, S> {
        /// Create an uninstalled mailbox with a positive accepted-work capacity.
        pub fn new(capacity: usize) -> Result<Self> {
            if capacity == 0 {
                return Err(Error::InvalidConfiguration);
            }
            Ok(Self {
                capacity,
                state: Mutex::new(State {
                    admission: Admission::Uninstalled,
                    queue: VecDeque::new(),
                }),
                credits: Arc::new(Credits {
                    outstanding: AtomicUsize::new(0),
                    waker: AtomicWaker::new(),
                }),
            })
        }

        /// Enable admission once; a closed mailbox cannot be reinstalled.
        pub fn install(&self) -> Result<()> {
            let mut state = lock(&self.state);
            if state.admission != Admission::Uninstalled {
                return Err(Error::InvalidConfiguration);
            }
            state.admission = Admission::Open;
            Ok(())
        }

        /// Permanently close admission without discarding queued or executing work.
        pub fn stop_admission(&self) {
            lock(&self.state).admission = Admission::Closed;
        }

        /// Count capacity retained by accepted commands and unread receipts.
        pub fn outstanding(&self) -> usize {
            self.credits.outstanding.load(Ordering::Acquire)
        }

        /// Close admission and detach only when all retained capacity has returned.
        pub fn uninstall(&self) -> Result<()> {
            let mut state = lock(&self.state);
            state.admission = Admission::Closed;
            if self.outstanding() != 0 {
                return Err(Error::Unavailable);
            }
            Ok(())
        }

        /// The caller allocates generations, allowing a shared sequence with other
        /// application identities. Each reply accepts only its own generation once.
        pub fn submit(
            self: &Arc<Self>,
            generation: u64,
            work: W,
            scope: &S,
            budget: Option<B>,
        ) -> Result<Receipt<W, V, B, S>, S::Error> {
            scope.check()?;
            let cancellation = scope
                .cancellation()
                .map(|c| c.subscribe())
                .transpose()
                .map_err(S::Error::from)?;
            // Clone all caller policy before reserving credit or taking a lock.
            let command_scope = scope.clone();
            let receipt_scope = scope.clone();
            let queued_scope = Arc::new(scope.clone());
            let mut state = lock(&self.state);
            if state.admission != Admission::Open {
                return Err(Error::Unavailable.into());
            }
            if self.outstanding() >= self.capacity {
                return Err(Error::Overloaded.into());
            }
            state
                .queue
                .try_reserve(1)
                .map_err(|_| S::Error::from(Error::Overloaded))?;
            self.credits.outstanding.fetch_add(1, Ordering::AcqRel);
            let permit = Arc::new(Permit {
                credits: self.credits.clone(),
                released: AtomicBool::new(false),
            });
            let reply = Arc::new(Reply::new(generation));
            let producer = Arc::new(Producer {
                reply: reply.clone(),
                credit: permit.clone(),
            });
            state.queue.push_back((
                queued_scope,
                Command {
                    generation,
                    work,
                    scope: command_scope,
                    budget,
                    reply: reply.clone(),
                    permit: producer,
                },
            ));
            drop(state);
            self.credits.waker.wake();
            Ok(Receipt {
                reply,
                scope: receipt_scope,
                cancellation,
                permit,
                work: std::marker::PhantomData,
            })
        }

        /// Replace the driver's work and capacity notification waker before checking.
        pub fn register(&self, waker: &Waker) {
            self.credits.waker.register(waker);
        }

        /// Transfer the oldest queued command to its execution owner without waiting.
        pub fn pop(&self) -> Option<Command<W, V, B, S>> {
            let entry = lock(&self.state).queue.pop_front();
            entry.map(|(_, command)| command)
        }

        /// Whether accepted work remains queued rather than executing.
        pub fn has_queued(&self) -> bool {
            !lock(&self.state).queue.is_empty()
        }

        /// Mark queued replies abandoned without releasing commands or their capacity.
        /// Delivery may end immediately, but completion-only polling still waits for
        /// actual completion (reported as `Cancelled`) or producer loss (`Unavailable`).
        pub fn abandon_queued(&self) {
            let replies: Vec<_> = lock(&self.state)
                .queue
                .iter()
                .map(|(_, command)| {
                    command.reply.abandoned.store(true, Ordering::Release);
                    command.reply.clone()
                })
                .collect();
            for reply in replies {
                let waker = lock(&reply.state).waker.take();
                if let Some(waker) = waker {
                    waker.wake();
                }
            }
        }

        /// Compute a minimum over a scope snapshot, invoking caller policy outside locks.
        pub fn queued_min<T: Ord>(&self, key: impl Fn(&S) -> T) -> Option<T> {
            let scopes: Vec<_> = lock(&self.state)
                .queue
                .iter()
                .map(|(scope, _)| scope.clone())
                .collect();
            scopes.iter().map(|scope| key(scope)).min()
        }

        /// Remove unstarted commands for caller-defined failure completion or process
        /// loss. Drop outside the lock: permits release against the same mailbox.
        pub fn take_queued(&self) -> VecDeque<Command<W, V, B, S>> {
            let queue = std::mem::take(&mut lock(&self.state).queue);
            queue.into_iter().map(|(_, command)| command).collect()
        }
    }

    impl<W, V, B, S: Scope> Receipt<W, V, B, S> {
        /// Wait for the accepted work's fence, ignoring cancellation as an early
        /// return condition. The caller must check its scope after completion.
        /// `Cancelled` means actual completion arrived but abandoned delivery discarded
        /// its output, not merely that cancellation or abandonment was requested.
        /// Producer loss returns `Unavailable`, not a successful ownership fence.
        /// Producers must retain `Command::permit` until external resources are safe.
        /// Values are consumed once; polling again after a value returns `Pending`,
        /// not `Cancelled`. Discarded and producer-loss errors remain observable.
        pub fn poll_completion(&self, cx: &mut Context<'_>) -> Poll<Result<Completion<V, B>>> {
            if let Some(cancellation) = &self.cancellation {
                cancellation.register(cx.waker());
            }
            self.reply.poll_completion(cx)
        }
    }

    impl<W, V, B, S: Scope> Future for Receipt<W, V, B, S> {
        /// Delivery observes caller policy separately from accepted-work ownership.
        type Output = Result<Completion<V, B>, S::Error>;

        /// Register cancellation before checking policy and the one-shot reply.
        fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
            if let Some(cancellation) = &self.cancellation {
                cancellation.register(cx.waker());
            }
            if let Err(error) = self.scope.check() {
                return Poll::Ready(Err(error));
            }
            if self.reply.is_abandoned() {
                return Poll::Ready(Err(Error::Cancelled.into()));
            }
            let result = self.reply.poll_completion(cx);
            // Abandonment may precede waker registration after the first check.
            if result.is_pending() && self.reply.is_abandoned() {
                return Poll::Ready(Err(Error::Cancelled.into()));
            }
            result.map(|result| result.map_err(Into::into))
        }
    }

    impl<W, V, B, S: Scope> Drop for Receipt<W, V, B, S> {
        /// Abandon delivery and discard unread output without fencing producer work.
        fn drop(&mut self) {
            self.reply.abandoned.store(true, Ordering::Release);
            let completion = lock(&self.reply.state).completion.take();
            drop(completion);
            self.permit.credits.waker.wake();
        }
    }

    impl<V, B> Reply<V, B> {
        /// Create a reply that accepts at most one completion for this generation.
        pub fn new(generation: u64) -> Self {
            Self {
                generation,
                abandoned: AtomicBool::new(false),
                state: Mutex::new(ReplyState {
                    completion: None,
                    waker: None,
                    status: ReplyStatus::Pending,
                }),
            }
        }

        /// Whether the consumer abandoned delivery, without canceling accepted work.
        pub fn is_abandoned(&self) -> bool {
            self.abandoned.load(Ordering::Acquire)
        }

        /// Finish this generation once, discarding abandoned results outside the lock.
        pub fn complete(
            &self,
            generation: u64,
            completion: Completion<V, B>,
        ) -> Result<(), StaleCompletion> {
            let mut state = lock(&self.state);
            if generation != self.generation || state.status != ReplyStatus::Pending {
                return Err(StaleCompletion);
            }
            let discarded = if !self.is_abandoned() {
                state.status = ReplyStatus::Completed;
                state.completion = Some(completion);
                None
            } else {
                state.status = ReplyStatus::Discarded;
                Some(completion)
            };
            let waker = state.waker.take();
            drop(state);
            drop(discarded);
            if let Some(waker) = waker {
                waker.wake();
            }
            Ok(())
        }

        /// Consume a result or register the sole delivery waiter, ignoring cancellation.
        fn poll_completion(&self, cx: &mut Context<'_>) -> Poll<Result<Completion<V, B>>> {
            let waker = cx.waker().clone();
            let mut state = lock(&self.state);
            if let Some(completion) = state.completion.take() {
                return Poll::Ready(Ok(completion));
            }
            if state.status == ReplyStatus::Lost {
                return Poll::Ready(Err(Error::Unavailable));
            }
            if state.status == ReplyStatus::Discarded {
                return Poll::Ready(Err(Error::Cancelled));
            }
            let old = state.waker.replace(waker);
            drop(state);
            drop(old);
            Poll::Pending
        }
    }

    impl<V, B> Drop for Producer<V, B> {
        /// Publish producer loss and return credit only if completion never arrived.
        fn drop(&mut self) {
            let mut state = lock(&self.reply.state);
            if state.status != ReplyStatus::Pending {
                return;
            }
            state.status = ReplyStatus::Lost;
            let waker = state.waker.take();
            drop(state);
            self.credit.release();
            if let Some(waker) = waker {
                waker.wake();
            }
        }
    }

    /// Independently snapshottable scope paired with its uniquely owned command.
    type Queued<W, V, B, S> = (Arc<S>, Command<W, V, B, S>);

    /// Admission lifecycle and unstarted work, protected by one queue lock.
    struct State<W, V, B, S: Scope> {
        admission: Admission,

        queue: VecDeque<Queued<W, V, B, S>>,
    }

    /// Admission only moves forward; closing never permits installation again.
    #[derive(Clone, Copy, Eq, PartialEq)]
    enum Admission {
        /// No driver has installed this mailbox yet.
        Uninstalled,
        /// The installed driver accepts work within the shared capacity limit.
        Open,
        /// Admission stopped permanently, independently of retained work and receipts.
        Closed,
    }

    /// Mutually exclusive lifecycle outcomes; producer loss is not successful fencing.
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    enum ReplyStatus {
        /// An execution owner may still publish the first completion.
        Pending,
        /// Completion arrived with output, which may already have been consumed.
        Completed,
        /// Completion arrived but abandoned delivery discarded its output.
        Discarded,
        /// The last execution owner disappeared before publishing completion.
        Lost,
    }

    /// Result and waiter state, always detached before invoking caller code.
    struct ReplyState<V, B> {
        completion: Option<Completion<V, B>>,

        waker: Option<Waker>,

        status: ReplyStatus,
    }

    /// Capacity accounting that does not keep the mailbox itself alive.
    struct Credits {
        outstanding: AtomicUsize,

        waker: AtomicWaker,
    }

    /// Both the accepted command and its receipt retain this permit. Dropping a
    /// receipt cannot release capacity while accepted work still owns resources.
    struct Permit {
        credits: Arc<Credits>,

        released: AtomicBool,
    }

    impl Permit {
        /// Release capacity at most once, notifying the mailbox driver afterward.
        fn release(&self) {
            if !self.released.swap(true, Ordering::AcqRel) {
                self.credits.outstanding.fetch_sub(1, Ordering::AcqRel);
                self.credits.waker.wake();
            }
        }
    }

    impl Drop for Permit {
        /// Return retained capacity when the last shared owner disappears.
        fn drop(&mut self) {
            self.release();
        }
    }

    /// Recover poisoned state without running user callbacks or destructors under lock.
    /// Mutations preserve ownership invariants even if caller code later panics.
    fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
        mutex.lock().unwrap_or_else(|error| error.into_inner())
    }

    /// Pure ownership, admission, wake-ordering, and reentrant disposal regressions.
    #[cfg(test)]
    mod tests {
        use super::*;
        use crate::{environment::Cancellation, test_util::WakeCounter};
        use std::sync::atomic::AtomicUsize;

        /// Producer loss is terminal even when a surviving reply receives a late result.
        #[test]
        fn late_completion_after_producer_loss_is_rejected_and_dropped_once() {
            let (mailbox, scope) = setup();
            let receipt = mailbox.submit(7, String::new(), &scope, None).unwrap();
            let command = mailbox.pop().unwrap();
            let reply = command.reply.clone();
            drop(command);
            assert_eq!(mailbox.outstanding(), 0);
            let dropped = Arc::new(AtomicUsize::new(0));
            assert_eq!(
                reply.complete(
                    7,
                    Completion {
                        value: 42,
                        budget: Some(Tracked(dropped.clone())),
                    }
                ),
                Err(StaleCompletion)
            );
            assert_eq!(dropped.load(Ordering::Relaxed), 1);
            assert!(matches!(
                receipt.poll_completion(&mut Context::from_waker(Waker::noop())),
                Poll::Ready(Err(Error::Unavailable))
            ));
            drop(receipt);
            assert_eq!(mailbox.outstanding(), 0);
        }

        /// Cancellation-only policy for exercising receipt and producer ownership.
        #[derive(Clone)]
        struct TestScope(Cancellation);

        impl Scope for TestScope {
            /// Mailbox fixtures use portable runtime failure classifications.
            type Error = Error;

            /// Admit work only while its shared cancellation scope remains active.
            fn check(&self) -> Result<()> {
                if self.0.is_cancelled() {
                    Err(Error::Cancelled)
                } else {
                    Ok(())
                }
            }

            /// Supply the shared cancellation source for receipt wake registration.
            fn cancellation(&self) -> Option<&Cancellation> {
                Some(&self.0)
            }
        }

        /// Counts destruction of the application budget retained by a command.
        struct Tracked(Arc<AtomicUsize>);

        impl Drop for Tracked {
            /// Record release of the application budget independently of mailbox credit.
            fn drop(&mut self) {
                self.0.fetch_add(1, Ordering::Relaxed);
            }
        }

        /// One-command fixture carrying owned text and a tracked budget.
        type TestMailbox = Mailbox<String, usize, Tracked, TestScope>;

        /// Install a fresh one-slot mailbox with an uncanceled scope.
        fn setup() -> (Arc<TestMailbox>, TestScope) {
            let mailbox = Arc::new(TestMailbox::new(1).unwrap());
            mailbox.install().unwrap();
            (mailbox, TestScope(Cancellation::new().unwrap()))
        }

        /// Canceling and dropping delivery retains accepted work and application capacity.
        #[test]
        fn cancellation_and_detachment_do_not_release_accepted_ownership() {
            let (mailbox, scope) = setup();
            let dropped = Arc::new(AtomicUsize::new(0));
            let mut receipt = mailbox
                .submit(1, "owned".into(), &scope, Some(Tracked(dropped.clone())))
                .unwrap();
            let mut command = mailbox.pop().unwrap();
            let count = Arc::new(WakeCounter::default());
            let waker = Waker::from(count.clone());
            let mut cx = Context::from_waker(&waker);
            mailbox.register(&waker);
            assert!(Pin::new(&mut receipt).poll(&mut cx).is_pending());
            scope.0.cancel().unwrap();
            assert!(count.count() > 0);
            assert!(matches!(
                Pin::new(&mut receipt).poll(&mut cx),
                Poll::Ready(Err(Error::Cancelled))
            ));
            assert!(receipt.poll_completion(&mut cx).is_pending());
            drop(receipt);
            assert!(command.reply.is_abandoned());
            assert_eq!(mailbox.outstanding(), 1);
            assert_eq!(dropped.load(Ordering::Relaxed), 0);
            let fresh = TestScope(Cancellation::new().unwrap());
            assert!(matches!(
                mailbox.submit(2, String::new(), &fresh, None),
                Err(Error::Overloaded)
            ));
            command
                .reply
                .complete(
                    command.generation,
                    Completion {
                        value: 9,
                        budget: command.budget.take(),
                    },
                )
                .unwrap();
            assert!(command.reply.state.lock().unwrap().completion.is_none());
            assert_eq!(dropped.load(Ordering::Relaxed), 1);
            assert_eq!(mailbox.outstanding(), 1);
            drop(command);
            assert_eq!(mailbox.outstanding(), 0);
        }

        /// Abandonment wakes delivery outside locks without fencing accepted ownership.
        #[test]
        fn abandoning_pending_delivery_wakes_without_releasing_ownership() {
            let (mailbox, scope) = setup();
            let dropped = Arc::new(AtomicUsize::new(0));
            let mut receipt = mailbox
                .submit(7, String::new(), &scope, Some(Tracked(dropped.clone())))
                .unwrap();

            /// Reenter both locks from wake to verify notification is detached.
            struct Reenter {
                mailbox: Arc<TestMailbox>,

                reply: Arc<Reply<usize, Tracked>>,

                wakes: AtomicUsize,
            }

            impl std::task::Wake for Reenter {
                /// Inspect queued ownership and the reply during notification.
                fn wake(self: Arc<Self>) {
                    assert!(self.mailbox.state.try_lock().is_ok());
                    assert!(self.reply.state.try_lock().is_ok());
                    assert_eq!(self.mailbox.outstanding(), 1);
                    self.wakes.fetch_add(1, Ordering::Relaxed);
                }
            }
            let counter = Arc::new(Reenter {
                mailbox: mailbox.clone(),
                reply: receipt.reply.clone(),
                wakes: AtomicUsize::new(0),
            });
            let waker = Waker::from(counter.clone());
            let mut cx = Context::from_waker(&waker);
            assert!(Pin::new(&mut receipt).poll(&mut cx).is_pending());
            mailbox.abandon_queued();
            assert_eq!(counter.wakes.load(Ordering::Relaxed), 1);
            assert!(matches!(
                Pin::new(&mut receipt).poll(&mut cx),
                Poll::Ready(Err(Error::Cancelled))
            ));
            assert!(mailbox.has_queued());
            assert_eq!(mailbox.outstanding(), 1);
            assert_eq!(dropped.load(Ordering::Relaxed), 0);
            let mut fence_cx = Context::from_waker(Waker::noop());
            assert!(receipt.poll_completion(&mut fence_cx).is_pending());
            let mut command = mailbox.pop().unwrap();
            command
                .reply
                .complete(
                    7,
                    Completion {
                        value: 42,
                        budget: command.budget.take(),
                    },
                )
                .unwrap();
            assert!(matches!(
                receipt.poll_completion(&mut fence_cx),
                Poll::Ready(Err(Error::Cancelled))
            ));
            assert_eq!(dropped.load(Ordering::Relaxed), 1);
            drop(command);
            assert_eq!(mailbox.outstanding(), 1);
            drop(receipt);
            assert_eq!(mailbox.outstanding(), 0);
        }

        /// Abandonment during registration cannot strand a delivery waiter.
        #[test]
        fn abandonment_during_waker_registration_returns_cancellation() {
            use std::task::{RawWaker, RawWakerVTable};
            thread_local! {
                static MAILBOX: std::cell::RefCell<Option<Arc<TestMailbox>>> = const { std::cell::RefCell::new(None) };
            }

            /// Inject abandonment between the initial check and reply registration.
            unsafe fn clone_raw(_: *const ()) -> RawWaker {
                MAILBOX.with(|slot| {
                    if let Some(mailbox) = slot.borrow_mut().take() {
                        mailbox.abandon_queued();
                    }
                });
                RawWaker::new(std::ptr::null(), &VTABLE)
            }

            /// No payload is owned by this stateless waker.
            unsafe fn noop(_: *const ()) {}

            static VTABLE: RawWakerVTable = RawWakerVTable::new(clone_raw, noop, noop, noop);
            let (mailbox, scope) = setup();
            let mut receipt = mailbox.submit(7, String::new(), &scope, None).unwrap();
            // Isolate reply registration from the separate scope-cancellation waker.
            receipt.cancellation = None;
            MAILBOX.with(|slot| *slot.borrow_mut() = Some(mailbox.clone()));
            // SAFETY: callbacks own no pointer and only access thread-local test state.
            let waker = unsafe { Waker::from_raw(RawWaker::new(std::ptr::null(), &VTABLE)) };
            assert!(matches!(
                Pin::new(&mut receipt).poll(&mut Context::from_waker(&waker)),
                Poll::Ready(Err(Error::Cancelled))
            ));
            assert_eq!(mailbox.outstanding(), 1);
            drop(mailbox.take_queued());
            assert_eq!(mailbox.outstanding(), 0);
        }

        /// Abandonment is not a fence; discarded completion wakes and retains owner credit.
        #[test]
        fn abandoned_queued_completion_is_terminal_only_after_completion() {
            for poll_before_complete in [false, true] {
                for receipt_dropped_first in [false, true] {
                    let mailbox =
                        Arc::new(Mailbox::<(), Tracked, Tracked, TestScope>::new(1).unwrap());
                    mailbox.install().unwrap();
                    let scope = TestScope(Cancellation::new().unwrap());
                    let values = Arc::new(AtomicUsize::new(0));
                    let budgets = Arc::new(AtomicUsize::new(0));
                    let mut receipt = mailbox
                        .submit(7, (), &scope, Some(Tracked(budgets.clone())))
                        .unwrap();
                    mailbox.abandon_queued();
                    let count = Arc::new(WakeCounter::default());
                    let waker = Waker::from(count.clone());
                    let mut cx = Context::from_waker(&waker);
                    assert!(matches!(
                        Pin::new(&mut receipt).poll(&mut cx),
                        Poll::Ready(Err(Error::Cancelled))
                    ));
                    if poll_before_complete {
                        assert!(receipt.poll_completion(&mut cx).is_pending());
                    }
                    let mut command = mailbox.pop().unwrap();
                    assert!(command.reply.is_abandoned());
                    assert_eq!(mailbox.outstanding(), 1);
                    assert_eq!(budgets.load(Ordering::Relaxed), 0);
                    assert_eq!(count.count(), 0);
                    command
                        .reply
                        .complete(
                            7,
                            Completion {
                                value: Tracked(values.clone()),
                                budget: command.budget.take(),
                            },
                        )
                        .unwrap();
                    assert_eq!(count.count(), usize::from(poll_before_complete));
                    assert_eq!(values.load(Ordering::Relaxed), 1);
                    assert_eq!(budgets.load(Ordering::Relaxed), 1);
                    for _ in 0..2 {
                        assert!(matches!(
                            receipt.poll_completion(&mut cx),
                            Poll::Ready(Err(Error::Cancelled))
                        ));
                    }
                    assert_eq!(
                        command.reply.complete(
                            7,
                            Completion {
                                value: Tracked(values.clone()),
                                budget: Some(Tracked(budgets.clone())),
                            },
                        ),
                        Err(StaleCompletion)
                    );
                    assert_eq!(values.load(Ordering::Relaxed), 2);
                    assert_eq!(budgets.load(Ordering::Relaxed), 2);
                    assert_eq!(mailbox.outstanding(), 1);
                    assert!(matches!(
                        mailbox.submit(8, (), &scope, None),
                        Err(Error::Overloaded)
                    ));
                    if receipt_dropped_first {
                        drop(receipt);
                        assert_eq!(mailbox.outstanding(), 1);
                        drop(command);
                    } else {
                        drop(command);
                        assert_eq!(mailbox.outstanding(), 1);
                        assert!(matches!(
                            receipt.poll_completion(&mut cx),
                            Poll::Ready(Err(Error::Cancelled))
                        ));
                        drop(receipt);
                    }
                    assert_eq!(mailbox.outstanding(), 0);
                    assert_eq!(values.load(Ordering::Relaxed), 2);
                    assert_eq!(budgets.load(Ordering::Relaxed), 2);
                }
            }
        }

        /// Stale results cannot fence abandoned work; last producer loss remains distinct.
        #[test]
        fn abandoned_queued_stale_completion_waits_for_last_producer_loss() {
            let (mailbox, scope) = setup();
            let dropped = Arc::new(AtomicUsize::new(0));
            let receipt = mailbox
                .submit(7, String::new(), &scope, Some(Tracked(dropped.clone())))
                .unwrap();
            mailbox.abandon_queued();
            let count = Arc::new(WakeCounter::default());
            let waker = Waker::from(count.clone());
            let mut cx = Context::from_waker(&waker);
            assert!(receipt.poll_completion(&mut cx).is_pending());
            let command = mailbox.pop().unwrap();
            let reply = command.reply.clone();
            let producer = command.permit.clone();
            assert_eq!(
                reply.complete(
                    8,
                    Completion {
                        value: 0,
                        budget: Some(Tracked(dropped.clone())),
                    },
                ),
                Err(StaleCompletion)
            );
            assert_eq!(dropped.load(Ordering::Relaxed), 1);
            assert!(receipt.poll_completion(&mut cx).is_pending());
            assert_eq!(count.count(), 0);
            assert_eq!(mailbox.outstanding(), 1);
            drop(command);
            assert_eq!(dropped.load(Ordering::Relaxed), 2);
            assert!(receipt.poll_completion(&mut cx).is_pending());
            assert_eq!(count.count(), 0);
            assert_eq!(mailbox.outstanding(), 1);
            drop(producer);
            assert_eq!(count.count(), 1);
            assert_eq!(mailbox.outstanding(), 0);
            assert!(matches!(
                receipt.poll_completion(&mut cx),
                Poll::Ready(Err(Error::Unavailable))
            ));
            assert_eq!(
                reply.complete(
                    7,
                    Completion {
                        value: 1,
                        budget: Some(Tracked(dropped.clone())),
                    },
                ),
                Err(StaleCompletion)
            );
            assert_eq!(dropped.load(Ordering::Relaxed), 3);
            assert!(matches!(
                receipt.poll_completion(&mut cx),
                Poll::Ready(Err(Error::Unavailable))
            ));
            drop(receipt);
            assert_eq!(mailbox.outstanding(), 0);
        }

        /// Generation checks and one-shot completion preserve the first owned result.
        #[test]
        fn stale_and_duplicate_completions_cannot_replace_owned_result() {
            let (mailbox, scope) = setup();
            let dropped = Arc::new(AtomicUsize::new(0));
            let mut receipt = mailbox
                .submit(17, "work".into(), &scope, Some(Tracked(dropped.clone())))
                .unwrap();
            let mut command = mailbox.pop().unwrap();
            assert_eq!(command.work, "work");
            assert_eq!(
                command.reply.complete(
                    18,
                    Completion {
                        value: 0,
                        budget: None
                    }
                ),
                Err(StaleCompletion)
            );
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(Pin::new(&mut receipt).poll(&mut cx).is_pending());
            command
                .reply
                .complete(
                    17,
                    Completion {
                        value: 42,
                        budget: command.budget.take(),
                    },
                )
                .unwrap();
            assert_eq!(
                command.reply.complete(
                    17,
                    Completion {
                        value: 0,
                        budget: None
                    }
                ),
                Err(StaleCompletion)
            );
            drop(command);
            assert_eq!(mailbox.outstanding(), 1);
            assert!(matches!(
                mailbox.submit(19, String::new(), &scope, None),
                Err(Error::Overloaded)
            ));
            let Poll::Ready(Ok(completion)) = Pin::new(&mut receipt).poll(&mut cx) else {
                panic!("missing completion")
            };
            assert_eq!(completion.value, 42);
            assert!(completion.budget.is_some());
            assert_eq!(dropped.load(Ordering::Relaxed), 0);
            // A consumed normal value is not a discarded completion or producer loss.
            assert!(receipt.poll_completion(&mut cx).is_pending());
            assert!(Pin::new(&mut receipt).poll(&mut cx).is_pending());
            drop(receipt);
            assert_eq!(mailbox.outstanding(), 0);
            drop(completion);
            assert_eq!(dropped.load(Ordering::Relaxed), 1);
        }

        /// Completion-only polling ignores cancellation, and unread output releases on drop.
        #[test]
        fn completion_only_poll_fences_cancel_and_unread_drop_reclaims_budget() {
            let (mailbox, scope) = setup();
            let dropped = Arc::new(AtomicUsize::new(0));
            let receipt = mailbox
                .submit(1, String::new(), &scope, Some(Tracked(dropped.clone())))
                .unwrap();
            let mut command = mailbox.pop().unwrap();
            scope.0.cancel().unwrap();
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(receipt.poll_completion(&mut cx).is_pending());
            command
                .reply
                .complete(
                    1,
                    Completion {
                        value: 7,
                        budget: command.budget.take(),
                    },
                )
                .unwrap();
            let Poll::Ready(Ok(completion)) = receipt.poll_completion(&mut cx) else {
                panic!("missing fence")
            };
            assert_eq!(completion.value, 7);
            drop((command, receipt, completion));
            assert_eq!(dropped.load(Ordering::Relaxed), 1);
            assert_eq!(mailbox.outstanding(), 0);

            let scope = TestScope(Cancellation::new().unwrap());
            let receipt = mailbox
                .submit(2, String::new(), &scope, Some(Tracked(dropped.clone())))
                .unwrap();
            let mut command = mailbox.pop().unwrap();
            command
                .reply
                .complete(
                    2,
                    Completion {
                        value: 8,
                        budget: command.budget.take(),
                    },
                )
                .unwrap();
            drop(command);
            assert_eq!(dropped.load(Ordering::Relaxed), 1);
            drop(receipt);
            assert_eq!(dropped.load(Ordering::Relaxed), 2);
            assert_eq!(mailbox.outstanding(), 0);
        }

        /// Closing admission retains accepted ownership until queued producers are disposed.
        #[test]
        fn admission_close_and_queue_drop_retain_receipt_capacity() {
            assert!(matches!(
                TestMailbox::new(0),
                Err(Error::InvalidConfiguration)
            ));
            let mailbox = Arc::new(TestMailbox::new(1).unwrap());
            let scope = TestScope(Cancellation::new().unwrap());
            assert!(matches!(
                mailbox.submit(1, String::new(), &scope, None),
                Err(Error::Unavailable)
            ));
            mailbox.install().unwrap();
            assert_eq!(mailbox.install(), Err(Error::InvalidConfiguration));
            let mut receipt = mailbox.submit(2, String::new(), &scope, None).unwrap();
            assert!(mailbox.has_queued());
            assert_eq!(mailbox.queued_min(|_| 5), Some(5));
            mailbox.stop_admission();
            mailbox.abandon_queued();
            assert_eq!(mailbox.uninstall(), Err(Error::Unavailable));
            assert!(matches!(
                mailbox.submit(3, String::new(), &scope, None),
                Err(Error::Unavailable)
            ));
            drop(mailbox.take_queued());
            assert!(!mailbox.has_queued());
            assert_eq!(mailbox.outstanding(), 0);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(matches!(
                Pin::new(&mut receipt).poll(&mut cx),
                Poll::Ready(Err(Error::Cancelled))
            ));
            drop(receipt);
            mailbox.uninstall().unwrap();
            assert_eq!(mailbox.install(), Err(Error::InvalidConfiguration));
        }

        /// Closing an uninstalled or idle mailbox is permanent, even after successful detach.
        #[test]
        fn admission_never_reopens_after_close_or_uninstall() {
            let scope = TestScope(Cancellation::new().unwrap());
            for install_first in [false, true] {
                for stop_first in [false, true] {
                    let mailbox = Arc::new(TestMailbox::new(1).unwrap());
                    if install_first {
                        mailbox.install().unwrap();
                    }
                    if stop_first {
                        mailbox.stop_admission();
                    }
                    mailbox.uninstall().unwrap();
                    mailbox.uninstall().unwrap();
                    assert_eq!(mailbox.install(), Err(Error::InvalidConfiguration));
                    assert!(matches!(
                        mailbox.submit(1, String::new(), &scope, None),
                        Err(Error::Unavailable)
                    ));
                    assert_eq!(mailbox.outstanding(), 0);
                }
            }
        }

        /// Work and completion notifications cover registration before and after readiness.
        #[test]
        fn cross_thread_notifications_cover_registration_orders() {
            for submit_first in [false, true] {
                for complete_first in [false, true] {
                    let (mailbox, scope) = setup();
                    let count = Arc::new(WakeCounter::default());
                    let waker = Waker::from(count.clone());
                    let mut cx = Context::from_waker(&waker);
                    if !submit_first {
                        mailbox.register(&waker);
                    }
                    let sender = mailbox.clone();
                    let mut receipt = std::thread::spawn(move || {
                        sender.submit(1, String::new(), &scope, None).unwrap()
                    })
                    .join()
                    .unwrap();
                    assert_eq!(count.count(), usize::from(!submit_first));
                    mailbox.register(&waker);
                    assert!(mailbox.has_queued());
                    if !complete_first {
                        assert!(Pin::new(&mut receipt).poll(&mut cx).is_pending());
                    }
                    let command = mailbox.pop().unwrap();
                    command
                        .reply
                        .complete(
                            1,
                            Completion {
                                value: 1,
                                budget: None,
                            },
                        )
                        .unwrap();
                    assert_eq!(
                        count.count(),
                        usize::from(!submit_first) + usize::from(!complete_first)
                    );
                    assert!(matches!(
                        Pin::new(&mut receipt).poll(&mut cx),
                        Poll::Ready(Ok(_))
                    ));
                    drop((command, receipt));
                    assert_eq!(mailbox.outstanding(), 0);
                }
            }
        }

        /// Producer loss wakes both receipt paths and returns admission without a false fence.
        #[test]
        fn producer_loss_wakes_both_wait_paths_and_releases_credit() {
            let (mailbox, scope) = setup();
            let mut receipt = mailbox.submit(1, String::new(), &scope, None).unwrap();
            let count = Arc::new(WakeCounter::default());
            let waker = Waker::from(count.clone());
            let mut cx = Context::from_waker(&waker);
            assert!(receipt.poll_completion(&mut cx).is_pending());
            let command = mailbox.pop().unwrap();
            std::thread::spawn(move || drop(command)).join().unwrap();
            assert_eq!(count.count(), 1);
            assert!(matches!(
                receipt.poll_completion(&mut cx),
                Poll::Ready(Err(Error::Unavailable))
            ));
            assert!(matches!(
                Pin::new(&mut receipt).poll(&mut cx),
                Poll::Ready(Err(Error::Unavailable))
            ));
            assert_eq!(mailbox.outstanding(), 0);
            assert!(mailbox.submit(2, String::new(), &scope, None).is_ok());
        }

        /// Caller callbacks run outside queue locks and poisoned state remains disposable.
        #[test]
        fn queued_callbacks_reenter_and_panics_do_not_poison_disposal() {
            let (mailbox, scope) = setup();
            let receipt = mailbox.submit(1, String::new(), &scope, None).unwrap();
            assert_eq!(mailbox.queued_min(|_| mailbox.outstanding()), Some(1));
            assert!(
                std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                    mailbox.queued_min::<usize>(|_| panic!("callback"));
                }))
                .is_err()
            );
            // Deliberately poison the internal mutex to exercise Drop recovery too.
            let _ = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                let _guard = mailbox.state.lock().unwrap();
                panic!("poison");
            }));
            drop(mailbox.take_queued());
            drop(receipt);
            assert_eq!(mailbox.outstanding(), 0);
        }

        /// Panicking caller policy clones cannot reserve admission or poison the queue.
        #[test]
        fn panicking_scope_clone_never_reserves_or_locks() {
            /// Injects failure while copying caller-owned admission policy.
            struct Panics;

            impl Clone for Panics {
                /// Fail before the mailbox can lock or reserve capacity.
                fn clone(&self) -> Self {
                    panic!("clone");
                }
            }

            impl Scope for Panics {
                /// The fixture uses portable runtime failures.
                type Error = Error;

                /// Admit work so submission reaches the deliberately panicking clone.
                fn check(&self) -> Result<()> {
                    Ok(())
                }
            }
            let mailbox = Arc::new(Mailbox::<(), (), (), Panics>::new(1).unwrap());
            mailbox.install().unwrap();
            assert!(
                std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                    let _ = mailbox.submit(1, (), &Panics, None);
                }))
                .is_err()
            );
            assert_eq!(mailbox.outstanding(), 0);
            assert!(!mailbox.has_queued());
            mailbox.uninstall().unwrap();
        }

        /// Accepted commands retain credit without creating a cycle back to the mailbox.
        #[test]
        fn queued_owners_do_not_keep_mailbox_alive() {
            let (mailbox, scope) = setup();
            let weak = Arc::downgrade(&mailbox);
            let dropped = Arc::new(AtomicUsize::new(0));
            let receipt = mailbox
                .submit(1, String::new(), &scope, Some(Tracked(dropped.clone())))
                .unwrap();
            drop(mailbox);
            assert!(weak.upgrade().is_none());
            assert_eq!(dropped.load(Ordering::Relaxed), 1);
            assert!(matches!(
                receipt.poll_completion(&mut Context::from_waker(futures::task::noop_waker_ref())),
                Poll::Ready(Err(Error::Unavailable))
            ));
        }

        /// Discarding abandoned output permits its destructor to reenter the completed reply.
        #[test]
        fn abandoned_completion_destructor_can_reenter_reply() {
            /// Checks terminal reply state during application budget destruction.
            struct Reenter(std::sync::Weak<Reply<(), Reenter>>);

            impl Drop for Reenter {
                /// Reenter the reply lock after completion has detached discarded output.
                fn drop(&mut self) {
                    let reply = self.0.upgrade().unwrap();
                    assert_eq!(lock(&reply.state).status, ReplyStatus::Discarded);
                }
            }
            let reply = Arc::new(Reply::new(1));
            reply.abandoned.store(true, Ordering::Release);
            reply
                .complete(
                    1,
                    Completion {
                        value: (),
                        budget: Some(Reenter(Arc::downgrade(&reply))),
                    },
                )
                .unwrap();
            assert!(lock(&reply.state).completion.is_none());
        }

        /// Cloned execution owners retain both loss notification and capacity until last drop.
        #[test]
        fn producer_clones_retain_loss_notification_until_last_owner() {
            let (mailbox, scope) = setup();
            let receipt = mailbox.submit(1, String::new(), &scope, None).unwrap();
            let command = mailbox.pop().unwrap();
            let producer = command.permit.clone();
            drop(command);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(receipt.poll_completion(&mut cx).is_pending());
            assert_eq!(mailbox.outstanding(), 1);
            drop(producer);
            assert!(matches!(
                receipt.poll_completion(&mut cx),
                Poll::Ready(Err(Error::Unavailable))
            ));
            assert_eq!(mailbox.outstanding(), 0);
        }
    }
}

/// Runtime error and shared wake instrumentation contracts.
#[cfg(test)]
mod tests {
    use super::*;

    /// Shared wake instrumentation counts owned and borrowed notifications equally.
    #[test]
    fn counts_owned_and_borrowed_wakes() {
        let counter = std::sync::Arc::new(test_util::WakeCounter::default());
        let waker = std::task::Waker::from(counter.clone());
        waker.wake_by_ref();
        waker.wake();
        assert_eq!(counter.count(), 2);
    }

    /// Common errno values retain classifications while other positive diagnostics survive.
    #[test]
    fn os_errors_preserve_errno_and_existing_classifications() {
        for (errno, expected) in [
            (libc::ENOENT, Error::NotFound),
            (libc::EEXIST, Error::AlreadyExists),
            (libc::ECANCELED, Error::Cancelled),
            (libc::EIO, Error::Os(libc::EIO)),
            (libc::EINTR, Error::Os(libc::EINTR)),
        ] {
            assert_eq!(
                Error::from_io(std::io::Error::from_raw_os_error(errno)),
                expected
            );
        }
        assert_eq!(Error::from_io(std::io::Error::other("opaque")), Error::Io);
    }

    /// Portable failures describe themselves without fabricating nested error sources.
    #[test]
    fn classified_errors_implement_standard_error_without_fabricated_sources() {
        for error in [
            Error::Cancelled,
            Error::DeadlineExceeded,
            Error::Overloaded,
            Error::Unavailable,
            Error::InvalidInput,
            Error::InvalidConfiguration,
            Error::NotFound,
            Error::AlreadyExists,
            Error::Io,
        ] {
            let standard: &dyn std::error::Error = &error;
            assert!(!standard.to_string().is_empty());
            assert!(standard.source().is_none());
        }
    }
}
