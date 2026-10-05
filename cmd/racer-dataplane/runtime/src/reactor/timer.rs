//! Explicit timer backend selection; no application test gate is inferred here.
use super::*;

#[derive(Clone, Copy, Debug)]
pub enum SleepMode {
    Kernel,
    /// Poll the scoped clock cooperatively. The owner advances that clock.
    #[cfg(feature = "simulation")]
    ClockPoll,
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
}

impl<S: Scope, B: Budget> Reactor<S, B> {
    /// Sleep under caller-owned scope policy, preserving readiness completion fences.
    /// Backend selection belongs to the caller even when simulation is compiled in.
    pub fn sleep_until<'a>(
        &'a self,
        until: std::time::Instant,
        mode: SleepMode,
        scope: &'a S,
    ) -> Operation<'a, (), S::Error> {
        Box::pin(async move {
            scope.check()?;
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
            // SAFETY: timerfd_create has no pointer arguments; the returned FD is owned.
            let raw = unsafe {
                libc::timerfd_create(
                    libc::CLOCK_MONOTONIC,
                    libc::TFD_CLOEXEC | libc::TFD_NONBLOCK,
                )
            };
            if raw < 0 {
                return Err(Error::Io.into());
            }
            // SAFETY: raw is a newly created descriptor with no other owner.
            let fd = Rc::new(unsafe { Descriptor::from_raw_fd(raw) });
            let interval = libc::itimerspec {
                it_interval: libc::timespec {
                    tv_sec: 0,
                    tv_nsec: 0,
                },
                it_value: libc::timespec {
                    tv_sec: duration
                        .as_secs()
                        .try_into()
                        .map_err(|_| Error::InvalidInput)?,
                    tv_nsec: duration.subsec_nanos() as _,
                },
            };
            // SAFETY: interval is initialized and remains valid for the syscall.
            if unsafe { libc::timerfd_settime(fd.as_raw_fd(), 0, &interval, std::ptr::null_mut()) }
                != 0
            {
                return Err(Error::Io.into());
            }
            self.readiness(fd, libc::POLLIN as u32, scope).await?;
            scope.check()
        })
    }
}
