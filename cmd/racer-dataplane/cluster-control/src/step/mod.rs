//! Sans-IO control plane steps.
//!
//! Each step is a small state machine or pure function that takes the
//! current monotonic time (`now: Instant`), wall time where a protocol needs
//! it, an explicit [`Entropy`] source, and typed observations, and returns a
//! decision. The production tasks in `seat.rs`, `primary.rs`, and
//! `server.rs` perform the I/O (Lease reads and writes, Secret reads, image
//! copies, sleeps) and call these steps for every decision, so the simulator
//! (`racer-sim`) drives the same rules without sockets, TLS, or threads.
//!
//! - [`lottery`]: seat lottery pacing, candidate choice, and claim check.
//! - [`renew`]: seat renewal schedule, release reasons, conflict handling.
//! - [`follower`]: committed record admission and the need-image rule.
//! - [`contend`]: primary Lease contention.
//! - [`primary`]: one primary reconcile pass and discovery annotations.
//! - [`admission`]: seat server admission (write and bootstrap slots, long
//!   poll cursors, delta versus full image bodies).

pub mod admission;
pub mod contend;
pub mod follower;
pub mod lottery;
pub mod primary;
pub mod renew;

pub use crate::{
    authority::{
        catalog::CacheInfo,
        election::{LeaseObservation, LeaseSpec, Observer, SeatView},
        keyring::Policy,
        members::{NodeInfo, Ownership, PodInfo},
        publication::{Image, Record},
        state::{StateError, StateRecord, Term, Writer},
    },
    entropy::{Entropy, OsEntropy, Seeded},
    shared::PrimaryView,
};
use std::time::Duration;

/// Pseudo-random delay below `max`, spread by Pod UID, `salt`, and `rng`.
pub fn jitter(pod_uid: &str, rng: &mut dyn Entropy, salt: u32, max: Duration) -> Duration {
    let r = rng.u32_or_zero();
    crate::authority::election::claim_delay(pod_uid, salt ^ r, max)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn jitter_is_bounded_and_seeded() {
        let max = Duration::from_secs(5);
        let a = jitter("u", &mut Seeded::new(1), 0, max);
        assert!(a < max);
        assert_eq!(a, jitter("u", &mut Seeded::new(1), 0, max));
        assert_eq!(
            jitter("u", &mut crate::entropy::Failing, 3, max),
            crate::authority::election::claim_delay("u", 3, max)
        );
    }
}
