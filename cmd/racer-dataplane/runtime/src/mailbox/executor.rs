//! Explicit mailbox execution. Cancellation requests never release accepted work.

use super::{Command, Completion, Mailbox, Reply};
use crate::{
    Error, Operation, Result, Scope, drivers::Runnable, environment::CancellationRegistration,
};
use std::{
    collections::VecDeque,
    rc::Rc,
    sync::Arc,
    task::{Context, Poll},
    time::Instant,
};

/// Application work only; the executor retains reply, budget, and completion credit.
pub trait Handler<W, V, B, S: ExecutionScope>: 'static {
    /// Execute borrowed policy without taking the command's completion authority.
    fn execute<'a>(
        &'a self,
        work: W,
        scope: &'a S,
        budget: Option<&'a mut B>,
    ) -> Operation<'a, V, S::Error>;
}

/// Caller policy for independent execution cancellation and deadline inspection.
pub trait ExecutionScope: Scope {
    /// Preserve request policy while creating an independent cancellation source.
    fn detached(&self) -> Result<Self, Self::Error>;

    /// Request cancellation without claiming completion.
    fn cancel(&self) -> Result<(), Self::Error>;

    /// Original caller deadline, used only as a scheduling hint.
    fn deadline(&self) -> Instant;

    /// Classify a duplicate or wrong-generation completion in application terms.
    fn stale_completion() -> Self::Error;
}

/// Worker-owned accepted futures, retained through completion even after detachment.
pub struct Executor<V, B, S: ExecutionScope> {
    active: VecDeque<Active<V, B, S>>,
}

/// One accepted command's execution and independent cancellation registration.
struct Active<V, B, S: ExecutionScope> {
    cancellation: Result<Option<CancellationRegistration>>,

    runnable: Arc<Runnable>,

    future: Operation<'static, (), S::Error>,

    scope: S,

    caller: S,

    reply: Arc<Reply<Result<V, S::Error>, B>>,
}

impl<V, B, S: ExecutionScope> Default for Executor<V, B, S> {
    /// Start with no accepted operations.
    fn default() -> Self {
        Self {
            active: VecDeque::new(),
        }
    }
}

impl<V: 'static, B: 'static, S: ExecutionScope> Executor<V, B, S> {
    /// Whether all started commands have actually completed.
    pub fn is_empty(&self) -> bool {
        self.active.is_empty()
    }

    /// Poll a bounded round-robin turn, registering before observing queued work.
    pub fn poll<W: 'static>(
        &mut self,
        mailbox: &Mailbox<W, Result<V, S::Error>, B, S>,
        cx: &mut Context<'_>,
        budget: usize,
        handler: Rc<impl Handler<W, V, B, S>>,
    ) -> Result<(), S::Error> {
        mailbox.register(cx.waker());
        let mut remaining = self.active.len();
        for _ in 0..budget {
            if let Some(command) = mailbox.pop() {
                remaining += 1;
                let caller = command.scope.clone();
                let scope = caller.detached()?;
                let reply = command.reply.clone();
                let active_scope = scope.clone();
                let handler = handler.clone();
                self.active.push_back(Active {
                    cancellation: caller.cancellation().map(|c| c.subscribe()).transpose(),
                    runnable: Runnable::new(),
                    scope: active_scope,
                    caller,
                    reply,
                    future: Box::pin(async move {
                        let Command {
                            generation,
                            work,
                            mut budget,
                            reply,
                            permit,
                            ..
                        } = command;
                        let value = if reply.is_abandoned() {
                            Err(Error::Cancelled.into())
                        } else {
                            handler.execute(work, &scope, budget.as_mut()).await
                        };
                        let completed = reply.complete(generation, Completion { value, budget });
                        drop(permit);
                        completed.map_err(|_| S::stale_completion())
                    }),
                });
            }
            if remaining != 0
                && let Some(mut active) = self.active.pop_front()
            {
                remaining -= 1;
                if let Ok(Some(cancellation)) = &active.cancellation {
                    cancellation.register(cx.waker());
                }
                if active.cancellation.is_err()
                    || active.reply.is_abandoned()
                    || active.caller.check().is_err()
                {
                    let _ = active.scope.cancel();
                }
                let force = active.scope.check().is_err();
                match active
                    .runnable
                    .poll(std::pin::Pin::new(&mut active.future), cx, force)
                {
                    Poll::Pending => self.active.push_back(active),
                    Poll::Ready(result) => result?,
                }
            }
        }
        if budget != 0 && mailbox.has_queued() {
            cx.waker().wake_by_ref();
        }
        Ok(())
    }

    /// Cancel started work and explicitly complete unstarted commands on expired drain.
    pub fn poll_drain<W: 'static>(
        &mut self,
        mailbox: &Mailbox<W, Result<V, S::Error>, B, S>,
        scope: &S,
        cx: &mut Context<'_>,
        handler: Rc<impl Handler<W, V, B, S>>,
    ) -> Poll<Result<(), S::Error>> {
        mailbox.stop_admission();
        if scope.check().is_err() {
            self.cancel(mailbox);
        }
        self.poll(mailbox, cx, 64, handler)?;
        if mailbox.outstanding() == 0 {
            Poll::Ready(Ok(()))
        } else {
            Poll::Pending
        }
    }

    /// Cancel started work and explicitly complete unstarted commands on expired drain.
    fn cancel<W>(&self, mailbox: &Mailbox<W, Result<V, S::Error>, B, S>) {
        for active in &self.active {
            let _ = active.scope.cancel();
        }
        Self::reject_queued(mailbox, Error::Cancelled.into());
    }

    /// Fence unstarted work by non-submission, returning the exact retained budget.
    pub fn reject_queued<W>(mailbox: &Mailbox<W, Result<V, S::Error>, B, S>, error: S::Error) {
        for mut command in mailbox.take_queued() {
            let _ = command.reply.complete(
                command.generation,
                Completion {
                    value: Err(error),
                    budget: command.budget.take(),
                },
            );
        }
    }

    /// Earliest caller deadline across accepted and queued operations.
    pub fn next_deadline<W>(
        &self,
        mailbox: &Mailbox<W, Result<V, S::Error>, B, S>,
    ) -> Option<Instant> {
        self.active
            .iter()
            .map(|a| a.caller.deadline())
            .chain(mailbox.queued_min(S::deadline))
            .min()
    }

    /// Drop accepted tasks only after the simulated operating system's crash cut.
    #[cfg(feature = "simulation")]
    pub fn simulation_crash(&mut self) {
        assert!(crate::reactor::simulation::Simulation::current().is_some());
        self.active.clear();
    }
}

impl<V, B, S: ExecutionScope> Drop for Executor<V, B, S> {
    /// Fail closed on teardown misuse: cancellation cannot free live I/O owners.
    fn drop(&mut self) {
        if !self.active.is_empty() {
            std::mem::forget(std::mem::take(&mut self.active));
        }
    }
}

/// Scheduling and fence regressions moved from the application dispatch driver.
#[cfg(test)]
mod tests {
    use super::*;
    use crate::{environment::Cancellation, test_util::WakeCounter};
    use std::{cell::RefCell, task::Waker};

    /// Cancellation-only execution policy with independent accepted scopes.
    #[derive(Clone)]
    struct TestScope(Cancellation);
    impl Scope for TestScope {
        /// Use portable runtime failures in ownership fixtures.
        type Error = Error;

        /// Reject cancellation without manufacturing a completion fence.
        fn check(&self) -> Result<()> {
            if self.0.is_cancelled() {
                Err(Error::Cancelled)
            } else {
                Ok(())
            }
        }

        /// Supply the operation-owned wake subscription source.
        fn cancellation(&self) -> Option<&Cancellation> {
            Some(&self.0)
        }
    }
    impl ExecutionScope for TestScope {
        /// Give accepted work independent cancellation ownership.
        fn detached(&self) -> Result<Self> {
            Ok(Self(Cancellation::new()?))
        }

        /// Notify the accepted scope.
        fn cancel(&self) -> Result<()> {
            self.0.cancel()
        }

        /// Provide a scheduling hint for these deadline-free fixtures.
        fn deadline(&self) -> Instant {
            Instant::now()
        }

        /// Identify invalid completion authority.
        fn stale_completion() -> Error {
            Error::InvalidInput
        }
    }

    /// Reject unexpected queued work in fixtures that inject accepted futures.
    struct Unused;
    impl Handler<(), (), (), TestScope> for Unused {
        /// These injected-future fixtures never execute queued work.
        fn execute<'a>(
            &'a self,
            _: (),
            _: &'a TestScope,
            _: Option<&'a mut ()>,
        ) -> Operation<'a, ()> {
            unreachable!()
        }
    }

    /// Construct independent cancellation without any application resources.
    fn scope() -> TestScope {
        TestScope(Cancellation::new().unwrap())
    }

    /// Returns application failures without acquiring completion authority.
    struct Echo;
    impl Handler<Result<usize>, usize, usize, TestScope> for Echo {
        /// Spend one credit and preserve the caller's chosen outcome.
        fn execute<'a>(
            &'a self,
            work: Result<usize>,
            _: &'a TestScope,
            budget: Option<&'a mut usize>,
        ) -> Operation<'a, usize> {
            Box::pin(async move {
                *budget.unwrap() += 1;
                work
            })
        }
    }

    /// Success, application failure, and queued abandonment retain the exact budget.
    #[test]
    fn handler_completion_and_queued_cancellation_preserve_credit_and_budget() {
        for result in [Ok(42), Err(Error::Io)] {
            let mailbox = Arc::new(Mailbox::new(1).unwrap());
            mailbox.install().unwrap();
            let scope = scope();
            let receipt = mailbox.submit(1, result, &scope, Some(8)).unwrap();
            let mut executor = Executor::default();
            let mut cx = Context::from_waker(std::task::Waker::noop());
            executor.poll(&mailbox, &mut cx, 1, Rc::new(Echo)).unwrap();
            assert!(executor.is_empty());
            assert_eq!(mailbox.outstanding(), 1);
            let Poll::Ready(Ok(completion)) = receipt.poll_completion(&mut cx) else {
                panic!("missing completion");
            };
            assert_eq!(completion.value, result);
            assert_eq!(completion.budget, Some(9));
            drop(receipt);
            assert_eq!(mailbox.outstanding(), 0);
            let receipt = mailbox.submit(2, result, &scope, Some(10)).unwrap();
            scope.cancel().unwrap();
            assert!(
                executor
                    .poll_drain(&mailbox, &scope, &mut cx, Rc::new(Echo))
                    .is_pending()
            );
            let Poll::Ready(Ok(completion)) = receipt.poll_completion(&mut cx) else {
                panic!("missing queued fence");
            };
            assert_eq!(completion.value, Err(Error::Cancelled));
            assert_eq!(completion.budget, Some(10));
            drop(receipt);
            assert_eq!(
                executor.poll_drain(&mailbox, &scope, &mut cx, Rc::new(Echo)),
                Poll::Ready(Ok(()))
            );
        }
    }

    /// Misordered owner drop never releases the live command's producer credit.
    #[test]
    fn executor_drop_fails_closed_for_unfenced_operations() {
        let mailbox = Arc::new(Mailbox::new(1).unwrap());
        mailbox.install().unwrap();
        let caller = scope();
        let receipt = mailbox.submit(1, (), &caller, None).unwrap();
        let command = mailbox.pop().unwrap();
        let mut executor = Executor::<(), (), TestScope>::default();
        executor.active.push_back(Active {
            cancellation: Ok(None),
            runnable: Runnable::new(),
            scope: scope(),
            caller,
            reply: command.reply.clone(),
            future: Box::pin(async move {
                std::future::pending::<()>().await;
                drop(command);
                Ok(())
            }),
        });
        drop(executor);
        assert!(
            receipt
                .poll_completion(&mut Context::from_waker(std::task::Waker::noop()))
                .is_pending()
        );
        drop(receipt);
        assert_eq!(mailbox.outstanding(), 1);
        assert_eq!(mailbox.uninstall(), Err(Error::Unavailable));
        assert_eq!(mailbox.install(), Err(Error::InvalidConfiguration));
    }

    /// Expired shutdown notifies active work, but detached receipts cannot release it.
    #[test]
    fn expired_drain_keeps_active_work_until_its_completion_fence() {
        let mailbox = Arc::new(Mailbox::new(1).unwrap());
        mailbox.install().unwrap();
        let mut executor = Executor::default();
        let caller = scope();
        let receipt = mailbox.submit(1, (), &caller, None).unwrap();
        let command = mailbox.pop().unwrap();
        let active_scope = scope();
        let (finish, fence) = futures::channel::oneshot::channel::<()>();
        executor.active.push_back(Active {
            cancellation: caller.cancellation().map(|c| c.subscribe()).transpose(),
            runnable: Runnable::new(),
            scope: active_scope.clone(),
            caller,
            reply: command.reply.clone(),
            future: Box::pin(async move {
                fence.await.map_err(|_| Error::Unavailable)?;
                command
                    .reply
                    .complete(
                        command.generation,
                        Completion {
                            value: Err(Error::Cancelled),
                            budget: None,
                        },
                    )
                    .map_err(|_| Error::InvalidInput)?;
                drop(command);
                Ok(())
            }),
        });
        let mut cx = Context::from_waker(Waker::noop());
        executor
            .poll(&mailbox, &mut cx, 1, Rc::new(Unused))
            .unwrap();
        let shutdown = scope();
        shutdown.cancel().unwrap();
        assert!(
            executor
                .poll_drain(&mailbox, &shutdown, &mut cx, Rc::new(Unused))
                .is_pending()
        );
        assert_eq!(active_scope.check(), Err(Error::Cancelled));
        assert!(receipt.poll_completion(&mut cx).is_pending());
        drop(receipt);
        assert_eq!(mailbox.outstanding(), 1);
        assert!(
            executor
                .poll_drain(&mailbox, &shutdown, &mut cx, Rc::new(Unused))
                .is_pending()
        );
        finish.send(()).unwrap();
        assert_eq!(
            executor.poll_drain(&mailbox, &shutdown, &mut cx, Rc::new(Unused)),
            Poll::Ready(Ok(()))
        );
        assert!(executor.is_empty());
        assert_eq!(mailbox.outstanding(), 0);
        mailbox.uninstall().unwrap();
    }

    /// Blocked accepted futures do not self-wake or exceed a round-robin work budget.
    #[test]
    fn blocked_endpoint_is_quiet_and_round_robin_is_budgeted() {
        let mailbox = Mailbox::new(4).unwrap();
        let mut executor = Executor::default();
        let count = Arc::new(WakeCounter::default());
        let waker = Waker::from(count.clone());
        let mut cx = Context::from_waker(&waker);
        let order = Rc::new(RefCell::new(Vec::new()));
        for id in 0..3 {
            let order = order.clone();
            let caller = scope();
            executor.active.push_back(Active {
                cancellation: caller.cancellation().map(|c| c.subscribe()).transpose(),
                runnable: Runnable::new(),
                scope: scope(),
                caller,
                reply: Arc::new(Reply::new(id)),
                future: Box::pin(std::future::poll_fn(move |_| {
                    order.borrow_mut().push(id);
                    Poll::Pending
                })),
            });
        }
        executor
            .poll(&mailbox, &mut cx, 0, Rc::new(Unused))
            .unwrap();
        assert!(order.borrow().is_empty());
        for _ in 0..6 {
            executor
                .poll(&mailbox, &mut cx, 1, Rc::new(Unused))
                .unwrap();
        }
        assert_eq!(&*order.borrow(), &[0, 1, 2]);
        assert_eq!(
            count.count(),
            0,
            "active length does not imply runnable work"
        );
        for active in &executor.active {
            std::task::Wake::wake_by_ref(&active.runnable);
        }
        executor
            .poll(&mailbox, &mut cx, 64, Rc::new(Unused))
            .unwrap();
        assert_eq!(&*order.borrow(), &[0, 1, 2, 0, 1, 2]);
        executor.active.clear(); // These fixture futures never submitted I/O.
    }
}
