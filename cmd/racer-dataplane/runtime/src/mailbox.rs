//! Bounded owned handoffs. Cancellation is notification, never a completion fence.
use crate::{Error, Result, Scope, deadline::CancellationRegistration};
use std::{
    collections::VecDeque,
    future::Future,
    pin::Pin,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
    },
    task::{Context, Poll, Waker},
};

pub struct Completion<V, B> {
    pub value: V,
    pub budget: Option<B>,
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::deadline::Cancellation;
    use std::{sync::atomic::AtomicUsize, task::Wake};

    #[derive(Clone)]
    struct TestScope(Cancellation);
    impl Scope for TestScope {
        type Error = Error;
        fn check(&self) -> Result<()> {
            if self.0.is_cancelled() {
                Err(Error::Cancelled)
            } else {
                Ok(())
            }
        }
        fn cancellation(&self) -> Option<&Cancellation> {
            Some(&self.0)
        }
    }
    struct Tracked(Arc<AtomicUsize>);
    impl Drop for Tracked {
        fn drop(&mut self) {
            self.0.fetch_add(1, Ordering::Relaxed);
        }
    }
    #[derive(Default)]
    struct Count(AtomicUsize);
    impl Wake for Count {
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::Relaxed);
        }
    }
    type TestMailbox = Mailbox<String, usize, Tracked, TestScope>;
    fn setup() -> (Arc<TestMailbox>, TestScope) {
        let mailbox = Arc::new(TestMailbox::new(1).unwrap());
        mailbox.install().unwrap();
        (mailbox, TestScope(Cancellation::new().unwrap()))
    }

    #[test]
    fn cancellation_and_detachment_do_not_release_accepted_ownership() {
        let (mailbox, scope) = setup();
        let dropped = Arc::new(AtomicUsize::new(0));
        let mut receipt = mailbox
            .submit(1, "owned".into(), &scope, Some(Tracked(dropped.clone())))
            .unwrap();
        let mut command = mailbox.pop().unwrap();
        let count = Arc::new(Count::default());
        let waker = Waker::from(count.clone());
        let mut cx = Context::from_waker(&waker);
        mailbox.register(&waker);
        assert!(Pin::new(&mut receipt).poll(&mut cx).is_pending());
        scope.0.cancel().unwrap();
        assert!(count.0.load(Ordering::Relaxed) > 0);
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
        drop(receipt);
        assert_eq!(mailbox.outstanding(), 0);
        drop(completion);
        assert_eq!(dropped.load(Ordering::Relaxed), 1);
    }

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
        let Poll::Ready(completion) = receipt.poll_completion(&mut cx) else {
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
        assert_eq!(mailbox.outstanding(), 1);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(matches!(
            Pin::new(&mut receipt).poll(&mut cx),
            Poll::Ready(Err(Error::Cancelled))
        ));
        drop(receipt);
        mailbox.uninstall().unwrap();
        assert_eq!(mailbox.install(), Err(Error::InvalidConfiguration));
    }

    #[test]
    fn cross_thread_notifications_cover_registration_orders() {
        for submit_first in [false, true] {
            for complete_first in [false, true] {
                let (mailbox, scope) = setup();
                let count = Arc::new(Count::default());
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
                assert_eq!(count.0.load(Ordering::Relaxed), usize::from(!submit_first));
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
                    count.0.load(Ordering::Relaxed),
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
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct StaleCompletion;

pub struct Reply<V, B> {
    generation: u64,
    abandoned: AtomicBool,
    state: Mutex<ReplyState<V, B>>,
}
struct ReplyState<V, B> {
    completion: Option<Completion<V, B>>,
    waker: Option<Waker>,
    finished: bool,
}
impl<V, B> Reply<V, B> {
    pub fn new(generation: u64) -> Self {
        Self {
            generation,
            abandoned: AtomicBool::new(false),
            state: Mutex::new(ReplyState {
                completion: None,
                waker: None,
                finished: false,
            }),
        }
    }
    pub fn is_abandoned(&self) -> bool {
        self.abandoned.load(Ordering::Acquire)
    }
    pub fn complete(
        &self,
        generation: u64,
        completion: Completion<V, B>,
    ) -> Result<(), StaleCompletion> {
        let mut state = self.state.lock().unwrap();
        if generation != self.generation || state.finished {
            return Err(StaleCompletion);
        }
        state.finished = true;
        if !self.is_abandoned() {
            state.completion = Some(completion);
        }
        let waker = state.waker.take();
        drop(state);
        if let Some(waker) = waker {
            waker.wake();
        }
        Ok(())
    }
    fn poll_completion(&self, cx: &mut Context<'_>) -> Poll<Completion<V, B>> {
        let mut state = self.state.lock().unwrap();
        if let Some(completion) = state.completion.take() {
            return Poll::Ready(completion);
        }
        state.waker = Some(cx.waker().clone());
        Poll::Pending
    }
}

/// Owned command queue with capacity shared by commands and unread receipts.
/// The driver must close admission and reap or take queued commands before
/// disposal: queued permits hold an Arc back to this mailbox. Active work must
/// be fenced by the caller; dropping a mailbox is not an I/O completion fence.
pub struct Mailbox<W, V, B, S: Scope> {
    capacity: usize,
    state: Mutex<State<W, V, B, S>>,
}
struct State<W, V, B, S: Scope> {
    installed: bool,
    closed: bool,
    outstanding: usize,
    queue: VecDeque<Command<W, V, B, S>>,
    waker: Option<Waker>,
}

/// Both the accepted command and its receipt retain this permit. Dropping a
/// receipt cannot release capacity while accepted work still owns resources.
pub struct Permit<W, V, B, S: Scope>(Arc<Mailbox<W, V, B, S>>);
impl<W, V, B, S: Scope> Drop for Permit<W, V, B, S> {
    fn drop(&mut self) {
        let mut state = self.0.state.lock().unwrap();
        state.outstanding -= 1;
        let waker = state.waker.clone();
        drop(state);
        if let Some(waker) = waker {
            waker.wake();
        }
    }
}

/// The driver must retain the permit until actual completion, even on cancel.
pub struct Command<W, V, B, S: Scope> {
    pub generation: u64,
    pub work: W,
    pub scope: S,
    pub budget: Option<B>,
    pub reply: Arc<Reply<V, B>>,
    pub permit: Arc<Permit<W, V, B, S>>,
}
pub struct Receipt<W, V, B, S: Scope> {
    reply: Arc<Reply<V, B>>,
    scope: S,
    cancellation: Option<CancellationRegistration>,
    permit: Arc<Permit<W, V, B, S>>,
}
impl<W, V, B, S: Scope> Receipt<W, V, B, S> {
    /// Wait for the accepted work's fence, ignoring cancellation as an early
    /// return condition. The caller must check its scope after completion.
    pub fn poll_completion(&self, cx: &mut Context<'_>) -> Poll<Completion<V, B>> {
        if let Some(cancellation) = &self.cancellation {
            cancellation.register(cx.waker());
        }
        self.reply.poll_completion(cx)
    }
}
impl<W, V, B, S: Scope> Future for Receipt<W, V, B, S> {
    type Output = Result<Completion<V, B>, S::Error>;
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
        self.reply.poll_completion(cx).map(Ok)
    }
}
impl<W, V, B, S: Scope> Drop for Receipt<W, V, B, S> {
    fn drop(&mut self) {
        self.reply.abandoned.store(true, Ordering::Release);
        let waker = self.permit.0.state.lock().unwrap().waker.clone();
        if let Some(waker) = waker {
            waker.wake();
        }
    }
}

impl<W, V, B, S: Scope> Mailbox<W, V, B, S> {
    pub fn new(capacity: usize) -> Result<Self> {
        if capacity == 0 {
            return Err(Error::InvalidConfiguration);
        }
        Ok(Self {
            capacity,
            state: Mutex::new(State {
                installed: false,
                closed: false,
                outstanding: 0,
                queue: VecDeque::new(),
                waker: None,
            }),
        })
    }
    pub fn install(&self) -> Result<()> {
        let mut state = self.state.lock().unwrap();
        if state.installed || state.closed {
            return Err(Error::InvalidConfiguration);
        }
        state.installed = true;
        Ok(())
    }
    pub fn stop_admission(&self) {
        self.state.lock().unwrap().closed = true;
    }
    pub fn outstanding(&self) -> usize {
        self.state.lock().unwrap().outstanding
    }
    pub fn uninstall(&self) -> Result<()> {
        let mut state = self.state.lock().unwrap();
        state.closed = true;
        if state.outstanding != 0 {
            return Err(Error::Unavailable);
        }
        state.installed = false;
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
        let mut state = self.state.lock().unwrap();
        if state.closed || !state.installed {
            return Err(Error::Unavailable.into());
        }
        if state.outstanding >= self.capacity {
            return Err(Error::Overloaded.into());
        }
        state.outstanding += 1;
        let permit = Arc::new(Permit(self.clone()));
        let reply = Arc::new(Reply::new(generation));
        state.queue.push_back(Command {
            generation,
            work,
            scope: scope.clone(),
            budget,
            reply: reply.clone(),
            permit: permit.clone(),
        });
        let waker = state.waker.take();
        drop(state);
        if let Some(waker) = waker {
            waker.wake();
        }
        Ok(Receipt {
            reply,
            scope: scope.clone(),
            cancellation,
            permit,
        })
    }
    pub fn register(&self, waker: &Waker) {
        self.state.lock().unwrap().waker = Some(waker.clone());
    }
    pub fn pop(&self) -> Option<Command<W, V, B, S>> {
        self.state.lock().unwrap().queue.pop_front()
    }
    pub fn has_queued(&self) -> bool {
        !self.state.lock().unwrap().queue.is_empty()
    }
    pub fn abandon_queued(&self) {
        for command in &self.state.lock().unwrap().queue {
            command.reply.abandoned.store(true, Ordering::Release);
        }
    }
    pub fn queued_min<T: Ord>(&self, key: impl Fn(&S) -> T) -> Option<T> {
        self.state
            .lock()
            .unwrap()
            .queue
            .iter()
            .map(|command| key(&command.scope))
            .min()
    }
    /// Remove unstarted commands for caller-defined failure completion or process
    /// loss. Drop outside the lock: permits release against the same mailbox.
    pub fn take_queued(&self) -> VecDeque<Command<W, V, B, S>> {
        std::mem::take(&mut self.state.lock().unwrap().queue)
    }
}
