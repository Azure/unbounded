//! Explicit timer backend selection; no application test gate is inferred here.
use super::*;

#[derive(Clone, Copy, Debug)]
pub enum SleepMode {
    Kernel,
    /// Poll the scoped clock cooperatively. The owner advances that clock.
    #[cfg(feature = "simulation")]
    ClockPoll,
}

impl<S: Scope, B: Budget> Reactor<S, B> {
    /// Sleep under caller-owned scope policy with an owned io_uring timeout.
    /// `until` must belong to the currently entered clock domain. Instant itself
    /// carries no domain ID, so an instant copied from another clock is unsupported.
    /// Kernel mode rejects simulated clocks/drivers, including expired deadlines.
    /// ClockPoll requires a simulated clock and cooperatively wakes its executor.
    pub fn sleep_until<'a>(
        &'a self,
        until: std::time::Instant,
        mode: SleepMode,
        scope: &'a S,
    ) -> Operation<'a, (), S::Error> {
        Box::pin(async move {
            scope.check()?;
            #[cfg(feature = "simulation")]
            match mode {
                SleepMode::Kernel
                    if crate::environment::simulation_seed().is_some()
                        || self.state.borrow().simulation.is_some() =>
                {
                    return Err(Error::InvalidConfiguration.into());
                }
                SleepMode::ClockPoll if crate::environment::simulation_seed().is_none() => {
                    return Err(Error::InvalidConfiguration.into());
                }
                _ => (),
            }
            let duration = until.saturating_duration_since(crate::environment::now());
            if duration.is_zero() {
                return Ok(());
            }
            match mode {
                #[cfg(feature = "simulation")]
                SleepMode::ClockPoll => {
                    return std::future::poll_fn(|cx| {
                        scope.check()?;
                        if crate::environment::now() >= until {
                            Poll::Ready(Ok(()))
                        } else {
                            cx.waker().wake_by_ref();
                            Poll::Pending
                        }
                    })
                    .await;
                }
                SleepMode::Kernel => (),
            }
            if duration.as_secs() > i64::MAX as u64 {
                return Err(Error::InvalidInput.into());
            }
            let timeout = SyscallArg::new(types::Timespec::from(duration));
            let sqe = opcode::Timeout::new(timeout.as_ptr()).build();
            self.submit(Submission::Real(sqe), scope, false, move |result| {
                drop(timeout);
                match result? {
                    KernelResult::Value(value) if value == -libc::ETIME => Ok(()),
                    other => {
                        other.value()?;
                        Ok(())
                    }
                }
            })?
            .await?;
            scope.check()
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::reactor::tests::{drive, kernel_reactor, poll, scope};

    #[test]
    fn kernel_timer_completes_and_cancel_retains_fence() {
        let Some(reactor) = kernel_reactor(4) else {
            return;
        };
        let scope = scope();
        drive(
            &reactor,
            reactor.sleep_until(crate::environment::now(), SleepMode::Kernel, &scope),
        )
        .unwrap();
        let until = crate::environment::now() + Duration::from_millis(2);
        drive(
            &reactor,
            reactor.sleep_until(until, SleepMode::Kernel, &scope),
        )
        .unwrap();
        assert!(crate::environment::now() >= until);
        let mut future = reactor.sleep_until(
            crate::environment::now() + Duration::from_secs(10),
            SleepMode::Kernel,
            &scope,
        );
        assert!(poll(&mut future).is_pending());
        assert_eq!(reactor.in_flight(), 1);
        scope.cancel().unwrap();
        assert_eq!(drive(&reactor, future), Err(Error::Cancelled));
        assert_eq!(reactor.in_flight(), 0);
    }

    #[cfg(feature = "simulation")]
    #[test]
    fn clock_poll_never_submits_and_preserves_scope_precedence() {
        use crate::reactor::tests::fixtures::{Admission, Limits, Reactor};
        use crate::test_util::WakeCounter;
        let clock = crate::environment::SimulationClock::new(33);
        let _clock = clock.environment(0).enter();
        let reactor = Reactor::new(Rc::new(Admission::new(Limits {
            queue_entries: std::num::NonZeroUsize::new(4).unwrap(),
        })));
        let scope = scope();
        let until = crate::environment::now() + Duration::from_millis(5);
        let count = Arc::new(WakeCounter::default());
        let waker = Waker::from(count.clone());
        let mut cx = Context::from_waker(&waker);
        let mut future = reactor.sleep_until(until, SleepMode::ClockPoll, &scope);
        assert!(future.as_mut().poll(&mut cx).is_pending());
        assert_eq!(count.count(), 1);
        assert_eq!(reactor.in_flight(), 0);
        clock.advance(Duration::from_millis(5));
        assert_eq!(future.as_mut().poll(&mut cx), Poll::Ready(Ok(())));
        scope.cancel().unwrap();
        assert_eq!(
            poll(&mut reactor.sleep_until(until, SleepMode::ClockPoll, &scope)),
            Poll::Ready(Err(Error::Cancelled))
        );
    }

    #[cfg(feature = "simulation")]
    #[test]
    fn timer_rejects_mixed_clock_and_backend_even_for_expired_deadlines() {
        let request = scope();
        let reactor = Reactor::<crate::reactor::tests::fixtures::RequestScope, ()>::new(2, ());
        let expired = crate::environment::now();
        assert_eq!(
            poll(&mut reactor.sleep_until(expired, SleepMode::ClockPoll, &request)),
            Poll::Ready(Err(Error::InvalidConfiguration))
        );
        let clock = crate::environment::SimulationClock::new(4);
        let _clock = clock.environment(0).enter();
        assert_eq!(
            poll(&mut reactor.sleep_until(expired, SleepMode::Kernel, &request)),
            Poll::Ready(Err(Error::InvalidConfiguration))
        );
        assert_eq!(reactor.in_flight(), 0);
    }
}
