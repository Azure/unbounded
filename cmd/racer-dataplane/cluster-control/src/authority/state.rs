//! Durable state record kept in the `racer-state` Secret, and writer fencing.
//!
//! A resourceVersion CAS only orders writes. It does not stop an old primary.
//! Every record names the primary term that wrote it: the primary Lease UID,
//! its `leaseTransitions` at claim time, and the holder identity, which has a
//! per-process nonce. A writer refuses to write over a newer term, and becomes
//! permanently fenced once it sees one.

use super::keyring::Durable;
use crate::limits;
use serde::{Deserialize, Serialize};
use std::time::{Duration, Instant};

/// Record schema version.
pub const VERSION: u32 = 1;

/// Largest encoded record, below the 1 MiB object limit.
pub const MAX_BYTES: usize = 900 * 1024;

/// Durable state failures. All of them fail closed.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum StateError {
    /// The record cannot be decoded or breaks an invariant.
    Invalid,
    /// The Secret was initialized before but its data is gone.
    Wiped,
    /// The record names another cluster.
    ClusterMismatch,
    /// The primary Lease was recreated. Its transitions restarted.
    LeaseReplaced,
    TooLarge,
    /// A newer term wrote the record. This writer must stop.
    StaleTerm,
    /// A counter would go backward.
    Rollback,
}

/// A primary term.
#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Term {
    pub lease_uid: String,
    pub transitions: u64,
    /// `<pod name>/<nonce>`.
    pub holder: String,
}

/// The JSON state record.
#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct StateRecord {
    pub version: u32,
    /// Equals the Secret UID.
    pub cluster: String,
    pub term: Term,
    pub writer_pod_uid: String,
    pub sequence: u64,
    pub membership_version: u64,
    pub content_hash: String,
    pub membership_hash: String,
    pub publication_bytes: u64,
    pub desired_seats: u8,
    pub keyring: Durable,
    pub committed_at: u64,
}

fn hex64(v: &str) -> bool {
    v.len() == 64
        && v.bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}

/// Decode Secret data.
///
/// Empty data is a fresh install only when the Secret was never initialized.
/// Initialized Secrets without valid data fail closed.
pub fn decode(
    secret_uid: &str,
    initialized: bool,
    data: Option<&[u8]>,
) -> Result<Option<StateRecord>, StateError> {
    let data = data.unwrap_or_default();
    if data.is_empty() {
        return if initialized {
            Err(StateError::Wiped)
        } else {
            Ok(None)
        };
    }
    if data.len() > MAX_BYTES {
        return Err(StateError::TooLarge);
    }
    let r: StateRecord = serde_json::from_slice(data).map_err(|_| StateError::Invalid)?;
    if r.version != VERSION {
        return Err(StateError::Invalid);
    }
    if r.cluster != secret_uid {
        return Err(StateError::ClusterMismatch);
    }
    if r.term.lease_uid.is_empty() || r.term.holder.is_empty() || r.writer_pod_uid.is_empty() {
        return Err(StateError::Invalid);
    }
    // Before the first publication counters are zero and hashes empty.
    let published = r.sequence != 0;
    if published != (r.membership_version != 0)
        || published != hex64(&r.content_hash)
        || published != hex64(&r.membership_hash)
        || (!published && (!r.content_hash.is_empty() || !r.membership_hash.is_empty()))
        || r.desired_seats as usize > crate::annotations::MAX_SEATS
    {
        return Err(StateError::Invalid);
    }
    r.keyring.validate().map_err(|_| StateError::Invalid)?;
    Ok(Some(r))
}

/// Encode a record for the Secret.
pub fn encode(r: &StateRecord) -> Result<Vec<u8>, StateError> {
    let b = serde_json::to_vec(r).map_err(|_| StateError::Invalid)?;
    if b.len() > MAX_BYTES {
        return Err(StateError::TooLarge);
    }
    Ok(b)
}

/// Check that `term` may write over `current`.
pub fn authorize_write(current: Option<&StateRecord>, term: &Term) -> Result<(), StateError> {
    let Some(c) = current else { return Ok(()) };
    if c.term.lease_uid != term.lease_uid {
        return Err(StateError::LeaseReplaced);
    }
    if c.term.transitions > term.transitions
        || (c.term.transitions == term.transitions && c.term.holder != term.holder)
    {
        return Err(StateError::StaleTerm);
    }
    Ok(())
}

/// Reject decreasing counters or a reused version with other content.
pub fn check_monotonic(prev: &StateRecord, next: &StateRecord) -> Result<(), StateError> {
    if next.sequence < prev.sequence
        || next.membership_version < prev.membership_version
        || next.keyring.generation < prev.keyring.generation
        || (next.sequence == prev.sequence && next.content_hash != prev.content_hash)
        || (next.membership_version == prev.membership_version
            && next.membership_hash != prev.membership_hash)
    {
        return Err(StateError::Rollback);
    }
    Ok(())
}

/// Tracks whether this process may still write.
#[derive(Debug)]
pub struct Writer {
    term: Term,
    fenced: bool,
}

impl Writer {
    pub fn new(term: Term) -> Self {
        Self {
            term,
            fenced: false,
        }
    }

    pub fn term(&self) -> &Term {
        &self.term
    }

    /// Feed every record read from the API. A newer term fences for good.
    pub fn observe(&mut self, current: Option<&StateRecord>) -> Result<(), StateError> {
        if self.fenced {
            return Err(StateError::StaleTerm);
        }
        if let Err(e) = authorize_write(current, &self.term) {
            self.fenced = true;
            return Err(e);
        }
        Ok(())
    }

    pub fn may_write(&self) -> bool {
        !self.fenced
    }

    /// Stamp this term on a copy of `current`, keeping counters.
    pub fn stamp(
        &self,
        base: StateRecord,
        cluster: &str,
        pod_uid: &str,
        now_unix: u64,
    ) -> StateRecord {
        StateRecord {
            version: VERSION,
            cluster: cluster.into(),
            term: self.term.clone(),
            writer_pod_uid: pod_uid.into(),
            committed_at: now_unix,
            ..base
        }
    }
}

/// Authority with a monotonic deadline. Every serve, issue, and write checks it.
#[derive(Clone, Copy, Debug)]
pub struct Freshness {
    until: Option<Instant>,
}

impl Freshness {
    pub const fn none() -> Self {
        Self { until: None }
    }

    /// Fresh until `confirmed + limit`.
    pub fn confirmed(confirmed: Instant, limit: Duration) -> Self {
        Self {
            until: Some(confirmed + limit),
        }
    }

    pub fn fresh(&self, now: Instant) -> bool {
        self.until.is_some_and(|u| now < u)
    }

    pub fn deadline(&self) -> Option<Instant> {
        self.until
    }

    /// The earlier of two deadlines.
    pub fn and(self, other: Self) -> Self {
        Self {
            until: match (self.until, other.until) {
                (Some(a), Some(b)) => Some(a.min(b)),
                _ => None,
            },
        }
    }
}

/// Lease authority: fresh for [`limits::LEASE_FRESHNESS`] after a renew was sent.
pub fn lease_authority(renew_started: Instant) -> Freshness {
    Freshness::confirmed(renew_started, limits::LEASE_FRESHNESS)
}

#[cfg(test)]
mod tests {
    use super::*;

    const UID: &str = "11111111-1111-4111-8111-111111111111";

    fn term(t: u64, holder: &str) -> Term {
        Term {
            lease_uid: "lease-1".into(),
            transitions: t,
            holder: holder.into(),
        }
    }

    fn record() -> StateRecord {
        StateRecord {
            version: VERSION,
            cluster: UID.into(),
            term: term(1, "pod/aa"),
            writer_pod_uid: "p".into(),
            ..Default::default()
        }
    }

    #[test]
    fn fresh_empty_and_wiped() {
        assert_eq!(decode(UID, false, None), Ok(None));
        assert_eq!(decode(UID, false, Some(b"")), Ok(None));
        assert_eq!(decode(UID, true, None), Err(StateError::Wiped));
        assert_eq!(decode(UID, true, Some(b"")), Err(StateError::Wiped));
    }

    #[test]
    fn roundtrip_and_invalid() {
        let r = record();
        let b = encode(&r).unwrap();
        assert_eq!(decode(UID, true, Some(&b)).unwrap().unwrap(), r);
        assert_eq!(
            decode("22222222-2222-4222-8222-222222222222", true, Some(&b)),
            Err(StateError::ClusterMismatch)
        );
        assert_eq!(decode(UID, true, Some(b"{")), Err(StateError::Invalid));
        let mut v: serde_json::Value = serde_json::from_slice(&b).unwrap();
        v["extra"] = 1.into();
        assert_eq!(
            decode(UID, true, Some(&serde_json::to_vec(&v).unwrap())),
            Err(StateError::Invalid)
        );
        let mut bad = r.clone();
        bad.version = 2;
        assert_eq!(
            decode(UID, true, Some(&encode(&bad).unwrap())),
            Err(StateError::Invalid)
        );
        let mut bad = r.clone();
        bad.sequence = 1;
        assert_eq!(
            decode(UID, true, Some(&encode(&bad).unwrap())),
            Err(StateError::Invalid)
        );
        bad.membership_version = 1;
        bad.content_hash = "a".repeat(64);
        bad.membership_hash = "b".repeat(64);
        assert!(
            decode(UID, true, Some(&encode(&bad).unwrap()))
                .unwrap()
                .is_some()
        );
        assert_eq!(
            decode(UID, true, Some(&vec![b' '; MAX_BYTES + 1])),
            Err(StateError::TooLarge)
        );
        let mut big = r;
        big.term.holder = "x".repeat(MAX_BYTES);
        assert_eq!(encode(&big), Err(StateError::TooLarge));
    }

    #[test]
    fn fencing() {
        let r = record();
        assert_eq!(authorize_write(None, &term(1, "a")), Ok(()));
        assert_eq!(authorize_write(Some(&r), &term(1, "pod/aa")), Ok(()));
        assert_eq!(authorize_write(Some(&r), &term(2, "other/bb")), Ok(()));
        assert_eq!(
            authorize_write(Some(&r), &term(0, "pod/aa")),
            Err(StateError::StaleTerm)
        );
        // Same transitions, other process of the same Pod.
        assert_eq!(
            authorize_write(Some(&r), &term(1, "pod/bb")),
            Err(StateError::StaleTerm)
        );
        let mut replaced = term(5, "pod/aa");
        replaced.lease_uid = "lease-2".into();
        assert_eq!(
            authorize_write(Some(&r), &replaced),
            Err(StateError::LeaseReplaced)
        );
    }

    #[test]
    fn old_writer_never_retries_after_replacement() {
        let mut old = Writer::new(term(1, "pod/aa"));
        assert!(old.observe(Some(&record())).is_ok());
        let mut newer = record();
        newer.term = term(2, "other/bb");
        assert_eq!(old.observe(Some(&newer)), Err(StateError::StaleTerm));
        assert!(!old.may_write());
        // Even if the old record shows up again, the writer stays fenced.
        assert_eq!(old.observe(Some(&record())), Err(StateError::StaleTerm));
        let stamped = Writer::new(term(2, "other/bb")).stamp(record(), UID, "q", 9);
        assert_eq!(stamped.term.transitions, 2);
        assert_eq!(stamped.writer_pod_uid, "q");
        assert_eq!(stamped.committed_at, 9);
    }

    #[test]
    fn monotonic() {
        let mut a = record();
        a.sequence = 5;
        a.membership_version = 3;
        a.content_hash = "a".repeat(64);
        a.membership_hash = "b".repeat(64);
        let mut b = a.clone();
        assert_eq!(check_monotonic(&a, &b), Ok(()));
        b.sequence = 4;
        assert_eq!(check_monotonic(&a, &b), Err(StateError::Rollback));
        b.sequence = 5;
        b.content_hash = "c".repeat(64);
        assert_eq!(check_monotonic(&a, &b), Err(StateError::Rollback));
        b.sequence = 6;
        assert_eq!(check_monotonic(&a, &b), Ok(()));
        b.membership_hash = "d".repeat(64);
        assert_eq!(check_monotonic(&a, &b), Err(StateError::Rollback));
    }

    #[test]
    fn freshness() {
        let now = Instant::now();
        let f = Freshness::confirmed(now, Duration::from_secs(1));
        assert!(f.fresh(now));
        assert!(!f.fresh(now + Duration::from_secs(1)));
        assert!(!Freshness::none().fresh(now));
        let g = f.and(Freshness::confirmed(now, Duration::from_millis(10)));
        assert!(!g.fresh(now + Duration::from_millis(10)));
        assert!(!f.and(Freshness::none()).fresh(now));
    }
}
