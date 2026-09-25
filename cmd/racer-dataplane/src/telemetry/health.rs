//! Readiness follows observed usable resources and credential expiry.
use crate::error::{Error, Result};
use std::{
    sync::{Arc, Mutex},
    time::Instant,
};

#[derive(Clone, Default)]
pub struct Health(Arc<Mutex<Status>>);
#[derive(Default)]
struct Status {
    lifecycle: State,
    resources: Resources,
}
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum State {
    #[default]
    Starting,
    Ready,
    Degraded,
    Draining,
    Stopped,
}
/// Publish a complete observation, aggregated over all required workers. No
/// identities, credentials, paths, or error strings enter health state.
#[derive(Clone, Copy, Debug, Default)]
pub struct Resources {
    pub workers_usable: bool,
    pub storage_usable: bool,
    pub listeners_usable: bool,
    pub membership_usable: bool,
    pub admission_usable: bool,
    /// Earliest expiry among required node credentials/keys, mapped to monotonic time.
    pub credentials_valid_until: Option<Instant>,
    /// Bounds staleness of the entire observation, including worker progress.
    pub observed_until: Option<Instant>,
}
impl Resources {
    pub fn usable_at(&self, now: Instant) -> bool {
        self.workers_usable
            && self.storage_usable
            && self.listeners_usable
            && self.membership_usable
            && self.admission_usable
            && self
                .credentials_valid_until
                .is_some_and(|expiry| now < expiry)
            && self.observed_until.is_some_and(|expiry| now < expiry)
    }
}
impl Health {
    pub fn state(&self) -> Result<State> {
        self.state_at(Instant::now())
    }
    pub fn state_at(&self, now: Instant) -> Result<State> {
        let status = self.0.lock().map_err(|_| Error::Unavailable)?;
        Ok(match status.lifecycle {
            State::Ready if !status.resources.usable_at(now) => State::Degraded,
            state => state,
        })
    }
    pub fn observe(&self, resources: Resources) -> Result<()> {
        self.0.lock().map_err(|_| Error::Unavailable)?.resources = resources;
        Ok(())
    }
    pub fn ready(&self) -> bool {
        self.state().is_ok_and(|state| state == State::Ready)
    }
    /// Starting, degraded, and draining workers remain live. A serving response
    /// itself establishes progress on the worker reactor; stopped is not live.
    pub fn live(&self) -> bool {
        self.state().is_ok_and(|state| state != State::Stopped)
    }
    pub fn transition(&self, state: State) -> Result<()> {
        let mut status = self.0.lock().map_err(|_| Error::Unavailable)?;
        if status.lifecycle == State::Stopped && state != State::Stopped
            || status.lifecycle == State::Draining
                && !matches!(state, State::Draining | State::Stopped)
            || state == State::Ready && !status.resources.usable_at(Instant::now())
        {
            return Err(Error::Unavailable);
        }
        status.lifecycle = state;
        Ok(())
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    use std::time::Duration;
    pub(super) fn resources(now: Instant) -> Resources {
        Resources {
            workers_usable: true,
            storage_usable: true,
            listeners_usable: true,
            membership_usable: true,
            admission_usable: true,
            credentials_valid_until: Some(now + Duration::from_secs(10)),
            observed_until: Some(now + Duration::from_secs(5)),
        }
    }
    #[test]
    fn readiness_is_fail_closed_and_expires_without_updates() {
        let now = Instant::now();
        let health = Health::default();
        assert!(health.live());
        assert!(!health.ready());
        assert_eq!(health.transition(State::Ready), Err(Error::Unavailable));
        let good = resources(now);
        health.observe(good).unwrap();
        health.transition(State::Ready).unwrap();
        assert!(health.ready());
        assert_eq!(
            health.state_at(now + Duration::from_secs(5)).unwrap(),
            State::Degraded
        );
        for bad in [
            Resources {
                workers_usable: false,
                ..good
            },
            Resources {
                storage_usable: false,
                ..good
            },
            Resources {
                listeners_usable: false,
                ..good
            },
            Resources {
                membership_usable: false,
                ..good
            },
            Resources {
                admission_usable: false,
                ..good
            },
            Resources {
                credentials_valid_until: Some(now),
                ..good
            },
            Resources {
                observed_until: None,
                ..good
            },
        ] {
            health.observe(bad).unwrap();
            assert!(!health.ready());
            assert!(health.live());
        }
        health.observe(good).unwrap();
        assert!(health.ready());
        health.transition(State::Draining).unwrap();
        assert!(!health.ready());
        assert!(health.live());
        assert!(health.transition(State::Ready).is_err());
        health.transition(State::Stopped).unwrap();
        assert!(!health.live());
        assert!(health.transition(State::Starting).is_err());
    }
}
