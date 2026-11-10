//! Lease election rules. Pure: callers pass observations and clocks.
//!
//! Expiry uses the local monotonic clock, like client-go: a Lease is expired
//! when the same resourceVersion was seen unchanged for a full Lease duration.
//! Wall clock renew times from other nodes are not trusted.

use crate::{SeatPolicy, limits};
use std::{
    collections::{BTreeMap, HashMap},
    time::{Duration, Instant},
};

/// Lease spec fields this runtime uses.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct LeaseSpec {
    pub holder: Option<String>,
    pub duration_secs: u32,
    pub acquire_time: Option<String>,
    pub renew_time: Option<String>,
    pub transitions: u64,
}

/// One observed Lease.
#[derive(Clone, Debug, Default)]
pub struct LeaseObservation {
    pub name: String,
    pub uid: String,
    pub resource_version: String,
    pub spec: LeaseSpec,
    pub annotations: BTreeMap<String, String>,
}

/// Tracks when each Lease version was first seen.
#[derive(Default)]
pub struct Observer {
    seen: HashMap<String, (String, Instant)>,
}

impl Observer {
    /// Record an observation at `now`.
    pub fn observe(&mut self, lease: &LeaseObservation, now: Instant) {
        let entry = self
            .seen
            .entry(lease.name.clone())
            .or_insert_with(|| (lease.resource_version.clone(), now));
        if entry.0 != lease.resource_version {
            *entry = (lease.resource_version.clone(), now);
        }
    }

    /// Whether the Lease may be taken over.
    pub fn expired(&self, lease: &LeaseObservation, now: Instant) -> bool {
        if lease.spec.holder.as_deref().is_none_or(str::is_empty) {
            return true;
        }
        let duration = Duration::from_secs(u64::from(lease.spec.duration_secs.max(1)));
        match self.seen.get(&lease.name) {
            Some((rv, at)) if *rv == lease.resource_version => now.duration_since(*at) >= duration,
            _ => false,
        }
    }

    pub fn forget(&mut self, name: &str) {
        self.seen.remove(name);
    }
}

/// Spec for a new claim. Transitions always increase, also for the same Pod.
pub fn claim(prev: &LeaseSpec, me: &str, now: &str) -> LeaseSpec {
    LeaseSpec {
        holder: Some(me.into()),
        duration_secs: limits::LEASE_DURATION.as_secs() as u32,
        acquire_time: Some(now.into()),
        renew_time: Some(now.into()),
        transitions: prev.transitions + 1,
    }
}

/// Spec for a renewal by the current holder.
pub fn renew(prev: &LeaseSpec, now: &str) -> LeaseSpec {
    LeaseSpec {
        renew_time: Some(now.into()),
        ..prev.clone()
    }
}

/// Spec after a voluntary release.
pub fn release(prev: &LeaseSpec) -> LeaseSpec {
    LeaseSpec {
        holder: None,
        acquire_time: None,
        renew_time: None,
        duration_secs: 1,
        transitions: prev.transitions,
    }
}

/// Kubernetes MicroTime, `2006-01-02T15:04:05.000000Z`.
pub fn micro_time(since_epoch: Duration) -> String {
    let secs = since_epoch.as_secs() as i64;
    let days = secs.div_euclid(86400);
    let rem = secs.rem_euclid(86400);
    // Howard Hinnant's civil_from_days.
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z - era * 146_097;
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146_096) / 365;
    let y = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    let y = if m <= 2 { y + 1 } else { y };
    format!(
        "{y:04}-{m:02}-{d:02}T{:02}:{:02}:{:02}.{:06}Z",
        rem / 3600,
        rem % 3600 / 60,
        rem % 60,
        since_epoch.subsec_micros()
    )
}

/// `wall` as MicroTime. Callers pass the wall clock (production:
/// `SystemTime::now()`), so this module stays pure. Times before the epoch
/// render as the epoch.
pub fn now_micro(wall: std::time::SystemTime) -> String {
    micro_time(
        wall.duration_since(std::time::UNIX_EPOCH)
            .unwrap_or_default(),
    )
}

/// Desired seat count.
pub fn desired_seats(policy: SeatPolicy, members: usize, eligible_pods: usize) -> usize {
    let want = match policy {
        SeatPolicy::Auto => members
            .div_ceil(limits::MEMBERS_PER_SEAT)
            .clamp(limits::MIN_AUTO_SEATS, crate::annotations::MAX_SEATS),
        SeatPolicy::Fixed(n) => n,
    };
    want.min(eligible_pods).max(1)
}

/// What a candidate knows about one seat.
#[derive(Clone, Debug)]
pub struct SeatView {
    pub index: usize,
    pub holder: Option<String>,
    pub holder_node: Option<String>,
    pub expired: bool,
}

/// Lowest vacant seat below `desired` that this Pod may claim.
///
/// No claim when this process already holds a seat, or a live seat is held on
/// the same Node.
pub fn seat_to_claim(seats: &[SeatView], desired: usize, my_node: &str, me: &str) -> Option<usize> {
    let live = |s: &&SeatView| !s.expired && s.holder.is_some();
    if seats
        .iter()
        .filter(live)
        .any(|s| s.holder.as_deref() == Some(me) || s.holder_node.as_deref() == Some(my_node))
    {
        return None;
    }
    seats
        .iter()
        .filter(|s| s.index < desired && !live(s))
        .map(|s| s.index)
        .min()
}

/// Whether a held seat must be released after a shrink.
pub fn must_release(held: usize, desired: usize) -> bool {
    held >= desired
}

/// Only a live seat holder may contend for the primary Lease.
pub fn may_claim_primary(holds_seat: bool, primary_expired: bool) -> bool {
    holds_seat && primary_expired
}

/// Pseudo-random delay before contending, spread by Pod UID and attempt.
/// Timing jitter only, so a non-cryptographic hash (FNV-1a, then a
/// splitmix64 finalizer) is enough.
pub fn claim_delay(pod_uid: &str, attempt: u32, spread: Duration) -> Duration {
    let mut n = 0xcbf2_9ce4_8422_2325u64;
    for b in pod_uid.bytes().chain([b'/']).chain(attempt.to_be_bytes()) {
        n = (n ^ u64::from(b)).wrapping_mul(0x0100_0000_01b3);
    }
    n = (n ^ (n >> 30)).wrapping_mul(0xbf58_476d_1ce4_e5b9);
    n = (n ^ (n >> 27)).wrapping_mul(0x94d0_49bb_1331_11eb);
    n ^= n >> 31;
    let ms = spread.as_millis().max(1) as u64;
    Duration::from_millis(n % ms)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn lease(rv: &str, holder: Option<&str>) -> LeaseObservation {
        LeaseObservation {
            name: "racer-seat-0".into(),
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
    fn expiry_uses_local_observation() {
        let t = Instant::now();
        let mut o = Observer::default();
        let l = lease("1", Some("p/0000000000000000"));
        assert!(!o.expired(&l, t));
        o.observe(&l, t);
        assert!(!o.expired(&l, t + Duration::from_secs(29)));
        assert!(o.expired(&l, t + Duration::from_secs(30)));
        // A renewal resets the clock.
        let l2 = lease("2", Some("p/0000000000000000"));
        o.observe(&l2, t + Duration::from_secs(29));
        assert!(!o.expired(&l2, t + Duration::from_secs(58)));
        assert!(o.expired(&lease("3", None), t));
    }

    #[test]
    fn claims_always_increment_transitions() {
        // Holder identity is the Pod name; a restarted process claims again.
        let first = claim(&LeaseSpec::default(), "pod-a", "t");
        assert_eq!(first.transitions, 1);
        let second = claim(&first, "pod-a", "t2");
        assert_eq!(second.transitions, 2);
        assert_eq!(renew(&second, "t3").transitions, 2);
        let released = release(&second);
        assert_eq!(released.holder, None);
        assert_eq!(released.transitions, 2);
    }

    #[test]
    fn micro_time_vectors() {
        assert_eq!(micro_time(Duration::ZERO), "1970-01-01T00:00:00.000000Z");
        assert_eq!(
            micro_time(Duration::from_secs(1_709_164_800)),
            "2024-02-29T00:00:00.000000Z"
        );
        assert_eq!(
            micro_time(Duration::from_micros(951_782_400_000_001)),
            "2000-02-29T00:00:00.000001Z"
        );
        assert_eq!(
            micro_time(Duration::from_secs(253_402_300_799)),
            "9999-12-31T23:59:59.000000Z"
        );
    }

    #[test]
    fn now_micro_uses_given_wall_time() {
        let t = std::time::UNIX_EPOCH + Duration::from_secs(1_709_164_800);
        assert_eq!(now_micro(t), "2024-02-29T00:00:00.000000Z");
        let before = std::time::UNIX_EPOCH - Duration::from_secs(1);
        assert_eq!(now_micro(before), "1970-01-01T00:00:00.000000Z");
    }

    #[test]
    fn seat_math() {
        for (members, eligible, want) in [
            (0, 100, 3),
            (6145, 100, 4),
            (100_000, 100, 16),
            (5000, 2, 2),
            (0, 0, 1),
        ] {
            assert_eq!(
                desired_seats(SeatPolicy::Auto, members, eligible),
                want,
                "{members}/{eligible}"
            );
        }
        assert_eq!(desired_seats(SeatPolicy::Fixed(5), 0, 3), 3);
        assert_eq!(desired_seats(SeatPolicy::Fixed(1), 100_000, 30), 1);
    }

    #[test]
    fn seat_selection() {
        let s = |i, h: Option<&str>, n: Option<&str>, e| SeatView {
            index: i,
            holder: h.map(Into::into),
            holder_node: n.map(Into::into),
            expired: e,
        };
        let seats = vec![
            s(0, Some("x"), Some("n1"), false),
            s(1, None, None, false),
            s(2, Some("y"), Some("n2"), true),
        ];
        assert_eq!(seat_to_claim(&seats, 3, "n3", "me"), Some(1));
        assert_eq!(seat_to_claim(&seats, 1, "n3", "me"), None);
        // Same node as a live holder.
        assert_eq!(seat_to_claim(&seats, 3, "n1", "me"), None);
        // Expired holder on my node does not block.
        assert_eq!(
            seat_to_claim(&[s(0, Some("y"), Some("n2"), true)], 3, "n2", "me"),
            Some(0)
        );
        // Already holding.
        assert_eq!(
            seat_to_claim(
                &[s(0, Some("me"), Some("n3"), false), s(1, None, None, false)],
                3,
                "n9",
                "me"
            ),
            None
        );
        assert!(must_release(3, 3));
        assert!(!must_release(2, 3));
        assert!(may_claim_primary(true, true));
        assert!(!may_claim_primary(false, true));
        assert!(!may_claim_primary(true, false));
    }

    #[test]
    fn delays_spread() {
        let a = claim_delay("u1", 0, Duration::from_secs(5));
        let b = claim_delay("u2", 0, Duration::from_secs(5));
        assert!(a < Duration::from_secs(5) && b < Duration::from_secs(5));
        assert_ne!(a, b);
        assert_eq!(a, claim_delay("u1", 0, Duration::from_secs(5)));
    }
}
