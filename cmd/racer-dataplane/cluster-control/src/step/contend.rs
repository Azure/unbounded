//! Primary Lease contention, lifted from `primary::contend`.

use crate::{
    authority::election::{self, LeaseObservation, Observer},
    entropy::Entropy,
    limits,
};
use std::time::{Duration, Instant};

/// Contention state of one seat holder.
#[derive(Default)]
pub struct Contend {
    observer: Observer,
}

impl Contend {
    /// Record a read of the primary Lease and decide whether to claim it.
    pub fn observe(&mut self, obs: &LeaseObservation, now: Instant, holds_seat: bool) -> bool {
        self.observer.observe(obs, now);
        election::may_claim_primary(holds_seat, self.observer.expired(obs, now))
    }

    /// A term this Pod led ended; forget its observation so the next read
    /// starts a fresh expiry clock.
    pub fn term_ended(&mut self) {
        self.observer.forget(crate::annotations::PRIMARY_LEASE);
    }

    /// Delay before the next read.
    pub fn wait(rng: &mut dyn Entropy) -> Duration {
        limits::LEASE_RENEW + Duration::from_millis(u64::from(rng.u16_or_zero()) % 3000)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        authority::election::LeaseSpec,
        entropy::{Failing, Seeded},
    };

    fn primary(rv: &str, holder: Option<&str>) -> LeaseObservation {
        LeaseObservation {
            name: crate::annotations::PRIMARY_LEASE.into(),
            resource_version: rv.into(),
            spec: LeaseSpec {
                holder: holder.map(Into::into),
                duration_secs: 30,
                ..Default::default()
            },
            ..Default::default()
        }
    }

    #[test]
    fn claims_only_vacant_or_expired_with_a_seat() {
        let t = Instant::now();
        let mut c = Contend::default();
        assert!(c.observe(&primary("1", None), t, true));
        assert!(!c.observe(&primary("1", None), t, false));
        let held = primary("2", Some("x"));
        assert!(!c.observe(&held, t, true));
        assert!(!c.observe(&held, t + Duration::from_secs(29), true));
        assert!(c.observe(&held, t + limits::LEASE_DURATION, true));
        c.term_ended();
        // Fresh clock after forgetting.
        assert!(!c.observe(&held, t + limits::LEASE_DURATION * 2, true));
    }

    #[test]
    fn wait_is_bounded() {
        assert_eq!(Contend::wait(&mut Failing), limits::LEASE_RENEW);
        for s in 0..16 {
            let w = Contend::wait(&mut Seeded::new(s));
            assert!(w >= limits::LEASE_RENEW && w < limits::LEASE_RENEW + Duration::from_secs(3));
        }
    }
}
