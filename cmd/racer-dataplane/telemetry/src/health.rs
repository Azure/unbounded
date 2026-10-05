//! Shared lifecycle observations and fixed probe responses, without resource policy.
use crate::server::Response;
use std::{
    sync::{Arc, Mutex},
    time::Instant,
};

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum State {
    #[default]
    Starting,
    Ready,
    Degraded,
    Draining,
    Stopped,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Unavailable;
impl std::fmt::Display for Unavailable {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("health unavailable")
    }
}
impl std::error::Error for Unavailable {}

struct Status<R> {
    lifecycle: State,
    resources: R,
}

/// Clones share lifecycle and the latest resource observation. Poisoned locks
/// fail closed. Usability is caller-defined and evaluated under the same lock.
pub struct Health<R>(Arc<Mutex<Status<R>>>);
impl<R> Clone for Health<R> {
    fn clone(&self) -> Self {
        Self(self.0.clone())
    }
}
impl<R: Default> Default for Health<R> {
    fn default() -> Self {
        Self(Arc::new(Mutex::new(Status {
            lifecycle: State::Starting,
            resources: R::default(),
        })))
    }
}
impl<R> Health<R> {
    /// Expired/unusable Ready observations report Degraded without changing the
    /// stored lifecycle. A later usable observation can report Ready again.
    pub fn state_at(
        &self,
        now: Instant,
        usable: impl FnOnce(&R, Instant) -> bool,
    ) -> Result<State, Unavailable> {
        let status = self.0.lock().map_err(|_| Unavailable)?;
        Ok(match status.lifecycle {
            State::Ready if !usable(&status.resources, now) => State::Degraded,
            state => state,
        })
    }
    pub fn observe(&self, resources: R) -> Result<(), Unavailable> {
        self.0.lock().map_err(|_| Unavailable)?.resources = resources;
        Ok(())
    }
    /// Stopped is absorbing; Draining permits only Draining or Stopped. Ready
    /// additionally requires a usable observation. The callback may obtain the
    /// caller's current clock under the lock and is not called for other states.
    pub fn transition(
        &self,
        state: State,
        usable: impl FnOnce(&R) -> bool,
    ) -> Result<(), Unavailable> {
        let mut status = self.0.lock().map_err(|_| Unavailable)?;
        if status.lifecycle == State::Stopped && state != State::Stopped
            || status.lifecycle == State::Draining
                && !matches!(state, State::Draining | State::Stopped)
            || state == State::Ready && !usable(&status.resources)
        {
            return Err(Unavailable);
        }
        status.lifecycle = state;
        Ok(())
    }
}

/// Caller-named probe bodies. Routing, readiness decisions, and observations
/// remain outside the response selection mechanism.
pub struct Probe {
    success: &'static str,
    failure: &'static str,
}
impl Probe {
    pub const fn new(success: &'static str, failure: &'static str) -> Self {
        Self { success, failure }
    }
    pub fn response(&self, usable: bool) -> (Response, &'static str) {
        if usable {
            (Response::Text, self.success)
        } else {
            (Response::Unavailable, self.failure)
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::{cell::Cell, time::Duration};
    const STATES: [State; 5] = [
        State::Starting,
        State::Ready,
        State::Degraded,
        State::Draining,
        State::Stopped,
    ];

    #[test]
    fn complete_transition_matrix_preserves_state_on_rejection() {
        for initial in STATES {
            for target in STATES {
                for usable in [false, true] {
                    let health = Health::<()>::default();
                    health.transition(initial, |_| true).unwrap();
                    let called = Cell::new(false);
                    let result = health.transition(target, |_| {
                        called.set(true);
                        usable
                    });
                    let lifecycle_allowed = !(initial == State::Stopped
                        && target != State::Stopped
                        || initial == State::Draining
                            && !matches!(target, State::Draining | State::Stopped));
                    let allowed = lifecycle_allowed && (target != State::Ready || usable);
                    assert_eq!(result, if allowed { Ok(()) } else { Err(Unavailable) });
                    assert_eq!(called.get(), lifecycle_allowed && target == State::Ready);
                    assert_eq!(
                        health.state_at(Instant::now(), |_, _| true).unwrap(),
                        if allowed { target } else { initial }
                    );
                }
            }
        }
    }

    #[test]
    fn shared_observations_expire_inclusively_without_mutating_lifecycle() {
        let health = Health::<Option<Instant>>::default();
        let other = health.clone();
        let now = Instant::now();
        let usable = |expiry: &Option<Instant>, now| expiry.is_some_and(|expiry| now < expiry);
        assert_eq!(
            health.transition(State::Ready, |r| usable(r, now)),
            Err(Unavailable)
        );
        other.observe(Some(now + Duration::from_secs(1))).unwrap();
        health.transition(State::Ready, |r| usable(r, now)).unwrap();
        assert_eq!(other.state_at(now, usable), Ok(State::Ready));
        assert_eq!(
            other.state_at(now + Duration::from_secs(1), usable),
            Ok(State::Degraded)
        );
        other.observe(Some(now + Duration::from_secs(3))).unwrap();
        assert_eq!(
            health.state_at(now + Duration::from_secs(1), usable),
            Ok(State::Ready)
        );
        health
            .transition(State::Degraded, |_| panic!("not Ready"))
            .unwrap();
        assert_eq!(
            health.state_at(now, |_, _| panic!("not Ready")),
            Ok(State::Degraded)
        );
    }

    #[test]
    fn poisoned_health_fails_closed_without_recovering_state() {
        let health = Health::<()>::default();
        let other = health.clone();
        assert!(
            std::thread::spawn(move || {
                let _guard = other.0.lock().unwrap();
                panic!("poison health");
            })
            .join()
            .is_err()
        );
        assert_eq!(health.observe(()), Err(Unavailable));
        assert_eq!(
            health.state_at(Instant::now(), |_, _| true),
            Err(Unavailable)
        );
        assert_eq!(
            health.transition(State::Stopped, |_| true),
            Err(Unavailable)
        );
    }

    #[test]
    fn probes_preserve_caller_bodies_and_response_kind() {
        let probe = Probe::new("healthy\n", "unavailable\n");
        let (response, body) = probe.response(true);
        assert!(matches!(response, Response::Text));
        assert_eq!(body, "healthy\n");
        let (response, body) = probe.response(false);
        assert!(matches!(response, Response::Unavailable));
        assert_eq!(body, "unavailable\n");
        assert_eq!(Probe::new("", "").response(true).1, "");
    }
}
