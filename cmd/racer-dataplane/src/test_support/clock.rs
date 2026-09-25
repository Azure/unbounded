//! Explicit time and wakeups. Wall-clock corrections never move monotonic deadlines.
use crate::error::{Error, Result};
use std::{
    cell::RefCell,
    collections::BTreeMap,
    future::Future,
    pin::Pin,
    rc::Rc,
    task::{Context, Poll, Waker},
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

#[derive(Clone)]
pub struct Clock(Rc<RefCell<State>>);

struct State {
    origin: Instant,
    elapsed: Duration,
    wall: SystemTime,
    wall_revision: u64,
    sequence: u64,
    timers: BTreeMap<(Duration, u64), Waker>,
}

impl Default for Clock {
    fn default() -> Self {
        Self::new(Instant::now(), UNIX_EPOCH)
    }
}

impl Clock {
    pub fn new(origin: Instant, wall: SystemTime) -> Self {
        Self(Rc::new(RefCell::new(State {
            origin,
            elapsed: Duration::ZERO,
            wall,
            wall_revision: 0,
            sequence: 0,
            timers: BTreeMap::new(),
        })))
    }

    pub fn elapsed(&self) -> Duration {
        self.0.borrow().elapsed
    }

    pub fn now(&self) -> Instant {
        let state = self.0.borrow();
        state.origin + state.elapsed
    }

    pub fn wall(&self) -> SystemTime {
        self.0.borrow().wall
    }

    /// Consumers can invalidate volatile freshness when the wall clock is corrected.
    pub fn wall_revision(&self) -> u64 {
        self.0.borrow().wall_revision
    }

    /// Advance both clocks atomically; overflow leaves time and timers untouched.
    pub fn advance(&self, duration: Duration) -> Result<()> {
        let mut state = self.0.borrow_mut();
        let elapsed = state
            .elapsed
            .checked_add(duration)
            .ok_or(Error::InvalidRange)?;
        state
            .origin
            .checked_add(elapsed)
            .ok_or(Error::InvalidRange)?;
        let wall = state
            .wall
            .checked_add(duration)
            .ok_or(Error::InvalidRange)?;
        state.elapsed = elapsed;
        state.wall = wall;
        let mut wake = Vec::new();
        while state
            .timers
            .first_key_value()
            .is_some_and(|((at, _), _)| *at <= elapsed)
        {
            wake.push(state.timers.pop_first().unwrap().1);
        }
        drop(state);
        for waker in wake {
            waker.wake();
        }
        Ok(())
    }

    pub fn jump_wall(&self, time: SystemTime) -> Result<()> {
        let mut state = self.0.borrow_mut();
        let revision = state
            .wall_revision
            .checked_add(1)
            .ok_or(Error::InvalidRange)?;
        state.wall = time;
        state.wall_revision = revision;
        Ok(())
    }

    pub fn wait_until(&self, at: Duration) -> Timer {
        Timer {
            clock: self.clone(),
            at,
            registration: None,
        }
    }

    pub fn check_deadline(&self, deadline: crate::runtime::deadline::Deadline) -> Result<()> {
        if self.now() >= deadline.0 {
            Err(Error::DeadlineExceeded)
        } else {
            Ok(())
        }
    }
}

pub struct Timer {
    clock: Clock,
    at: Duration,
    registration: Option<u64>,
}

impl Future for Timer {
    type Output = Result<()>;

    fn poll(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        let clock = self.clock.clone();
        let mut state = clock.0.borrow_mut();
        if state.elapsed >= self.at {
            if let Some(id) = self.registration.take() {
                state.timers.remove(&(self.at, id));
            }
            return Poll::Ready(Ok(()));
        }
        let id = match self.registration {
            Some(id) => id,
            None => {
                let Some(next) = state.sequence.checked_add(1) else {
                    return Poll::Ready(Err(Error::Overloaded));
                };
                let id = state.sequence;
                state.sequence = next;
                self.registration = Some(id);
                id
            }
        };
        state.timers.insert((self.at, id), cx.waker().clone());
        Poll::Pending
    }
}

impl Drop for Timer {
    fn drop(&mut self) {
        if let Some(id) = self.registration {
            self.clock.0.borrow_mut().timers.remove(&(self.at, id));
        }
    }
}

/// Stable FIFO ordering at equal timestamps, even when time advances across events.
pub struct Schedule<T> {
    clock: Clock,
    sequence: u64,
    events: BTreeMap<(Duration, u64), T>,
}

impl<T> Schedule<T> {
    pub fn new(clock: Clock) -> Self {
        Self {
            clock,
            sequence: 0,
            events: BTreeMap::new(),
        }
    }

    pub fn push(&mut self, at: Duration, event: T) -> Result<()> {
        let next = self.sequence.checked_add(1).ok_or(Error::Overloaded)?;
        self.events.insert((at, self.sequence), event);
        self.sequence = next;
        Ok(())
    }

    pub fn pop_ready(&mut self) -> Option<T> {
        if self
            .events
            .first_key_value()
            .is_some_and(|((at, _), _)| *at <= self.clock.elapsed())
        {
            self.events.pop_first().map(|(_, event)| event)
        } else {
            None
        }
    }

    pub fn len(&self) -> usize {
        self.events.len()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{runtime::deadline::Deadline, test_support::WakeCounter};
    use std::sync::Arc;

    #[test]
    fn wall_corrections_do_not_extend_original_deadlines() {
        let clock = Clock::default();
        let original = clock.now();
        let deadline = Deadline(original + Duration::from_secs(2));
        clock.advance(Duration::from_secs(1)).unwrap();
        clock
            .jump_wall(UNIX_EPOCH - Duration::from_secs(100))
            .unwrap();
        assert_eq!(clock.now(), original + Duration::from_secs(1));
        assert_eq!(clock.wall_revision(), 1);
        assert_eq!(clock.check_deadline(deadline), Ok(()));
        clock.advance(Duration::from_secs(1)).unwrap();
        assert_eq!(clock.check_deadline(deadline), Err(Error::DeadlineExceeded));
        let before = (clock.now(), clock.wall());
        assert_eq!(clock.advance(Duration::MAX), Err(Error::InvalidRange));
        assert_eq!((clock.now(), clock.wall()), before);
    }

    #[test]
    fn timers_wake_at_boundary_replace_wakers_and_unregister_on_drop() {
        let clock = Clock::default();
        let first = Arc::new(WakeCounter::default());
        let second = Arc::new(WakeCounter::default());
        let mut timer = Box::pin(clock.wait_until(Duration::from_secs(2)));
        assert!(
            timer
                .as_mut()
                .poll(&mut Context::from_waker(&Waker::from(first.clone())))
                .is_pending()
        );
        assert!(
            timer
                .as_mut()
                .poll(&mut Context::from_waker(&Waker::from(second.clone())))
                .is_pending()
        );
        clock
            .jump_wall(UNIX_EPOCH + Duration::from_secs(100))
            .unwrap();
        clock.advance(Duration::from_secs(1)).unwrap();
        assert_eq!(second.count(), 0);
        clock.advance(Duration::from_secs(1)).unwrap();
        assert_eq!(first.count(), 0);
        assert_eq!(second.count(), 1);
        assert_eq!(
            timer.as_mut().poll(&mut Context::from_waker(Waker::noop())),
            Poll::Ready(Ok(()))
        );
        let mut abandoned = Box::pin(clock.wait_until(Duration::from_secs(3)));
        assert!(
            abandoned
                .as_mut()
                .poll(&mut Context::from_waker(&Waker::from(second.clone())))
                .is_pending()
        );
        drop(abandoned);
        clock.advance(Duration::from_secs(1)).unwrap();
        assert_eq!(second.count(), 1);
        assert!(clock.0.borrow().timers.is_empty());
    }

    #[test]
    fn fault_schedule_orders_due_time_then_insertion_without_sleeping() {
        let clock = Clock::default();
        let mut schedule = Schedule::new(clock.clone());
        for (at, value) in [(2, 'b'), (1, 'a'), (2, 'c'), (3, 'd')] {
            schedule.push(Duration::from_secs(at), value).unwrap();
        }
        assert_eq!(schedule.pop_ready(), None);
        clock.advance(Duration::from_secs(2)).unwrap();
        assert_eq!(schedule.pop_ready(), Some('a'));
        assert_eq!(schedule.pop_ready(), Some('b'));
        assert_eq!(schedule.pop_ready(), Some('c'));
        assert_eq!(schedule.pop_ready(), None);
        assert_eq!(schedule.len(), 1);
    }

    #[test]
    fn wall_time_drives_freshness_but_expired_versions_still_answer_pins() {
        use crate::model::{
            identity::{CacheId, CacheKey, ObjectId, ObjectVersion, StrongEtag},
            metadata::{CurrentVersion, ExpiresAt, VersionMetadata},
        };
        let clock = Clock::default();
        let descriptor = VersionMetadata {
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId("cache".into()),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::test_value("v1"),
            },
            length: 42,
        };
        let current = CurrentVersion {
            version: descriptor.version.clone(),
            expires_at: ExpiresAt(clock.wall() + Duration::from_secs(2)),
        };
        assert_eq!(
            current
                .resolve(&descriptor, clock.wall())
                .unwrap()
                .unwrap()
                .length,
            42
        );
        clock.advance(Duration::from_secs(2)).unwrap();
        assert_eq!(current.resolve(&descriptor, clock.wall()), Ok(None));
        assert_eq!(descriptor.for_pin().length, 42);
        assert_eq!(descriptor.for_pin().expires_at, ExpiresAt(UNIX_EPOCH));
        let monotonic = clock.now();
        let revision = clock.wall_revision();
        clock.jump_wall(UNIX_EPOCH).unwrap();
        assert_eq!(clock.now(), monotonic);
        assert_ne!(
            clock.wall_revision(),
            revision,
            "freshness owner must observe correction before reusing its pointer"
        );
    }
}
