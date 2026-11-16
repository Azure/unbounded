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
use zeroize::Zeroizing;

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
    /// Page size of the committed publication, or 0 before the first one.
    /// Fixed at cluster creation: later primaries publish this value and
    /// ignore their own `RACER_PAGE_BYTES`, since running nodes refuse a
    /// publication that changes the page size.
    #[serde(default, skip_serializing_if = "is_zero")]
    pub page_bytes: u64,
}

fn is_zero(v: &u64) -> bool {
    *v == 0
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
        || (r.page_bytes != 0 && racer_wire::object::PageSize::from_wire(r.page_bytes).is_err())
    {
        return Err(StateError::Invalid);
    }
    r.keyring.validate().map_err(|_| StateError::Invalid)?;
    Ok(Some(r))
}

/// Encode a record for the Secret. The output holds key material and is
/// wiped on drop; it is written into a buffer reserved up front so no partial
/// copy is freed unwiped.
pub fn encode(r: &StateRecord) -> Result<Zeroizing<Vec<u8>>, StateError> {
    struct Bounded(Zeroizing<Vec<u8>>);
    impl std::io::Write for Bounded {
        fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
            if bytes.len() > MAX_BYTES - self.0.len() {
                return Err(std::io::ErrorKind::FileTooLarge.into());
            }
            self.0.extend_from_slice(bytes);
            Ok(bytes.len())
        }

        fn flush(&mut self) -> std::io::Result<()> {
            Ok(())
        }
    }
    let mut out = Bounded(Zeroizing::new(Vec::with_capacity(MAX_BYTES)));
    serde_json::to_writer(&mut out, r).map_err(|e| {
        if e.is_io() {
            StateError::TooLarge
        } else {
            StateError::Invalid
        }
    })?;
    Ok(out.0)
}

/// Check that `term` may write over `current`.
///
/// Transitions reset when the Lease is recreated, so term ordering only
/// holds within one Lease UID. A record stamped under another UID may be
/// taken over only when `live_lease_uid` (read from the API after the
/// record) is this term's UID: the stamped Lease is then gone, and its
/// holder can no longer renew. The rule is asymmetric: once the live term
/// stamps the record, the old holder reads a foreign UID while the live
/// UID is not its own, so it is fenced and cannot take the record back.
/// Counters stay monotonic because takeover stamps on top of `current`.
pub fn authorize_write(
    current: Option<&StateRecord>,
    term: &Term,
    live_lease_uid: Option<&str>,
) -> Result<(), StateError> {
    let Some(c) = current else { return Ok(()) };
    if c.term.lease_uid != term.lease_uid {
        if live_lease_uid == Some(term.lease_uid.as_str()) {
            return Ok(());
        }
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
    /// `live_lease_uid` is the UID of the primary Lease read after `current`,
    /// if known; see [`authorize_write`] for the takeover rule.
    pub fn observe(
        &mut self,
        current: Option<&StateRecord>,
        live_lease_uid: Option<&str>,
    ) -> Result<(), StateError> {
        if self.fenced {
            return Err(StateError::StaleTerm);
        }
        if let Err(e) = authorize_write(current, &self.term, live_lease_uid) {
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

    fn legacy_state() -> serde_json::Value {
        use base64::{Engine, engine::general_purpose::STANDARD};

        let mut r = record();
        r.sequence = 7;
        r.membership_version = 3;
        r.content_hash = "a".repeat(64);
        r.membership_hash = "b".repeat(64);
        r.publication_bytes = 1234;
        r.desired_seats = 2;
        r.committed_at = 100;
        r.keyring.generation = 2;
        r.keyring.next_rotation = 200;
        r.keyring.activate_at = 150;
        let mut value = serde_json::to_value(r).unwrap();
        // v0.23 wrote hex IDs and base64 material in a JSON list, without page_bytes.
        value["keyring"]["keys"] = serde_json::json!([
            {"cache": UID, "id": "524b4731000000000000000200000004",
             "purpose": "origin_credentials", "state": "prepared", "material": STANDARD.encode([4; 32])},
            {"cache": UID, "id": "524b4731000000000000000100000002",
             "purpose": "origin_credentials", "state": "active", "material": STANDARD.encode([2; 32])},
            {"cache": UID, "id": "524b4731000000000000000200000003",
             "purpose": "page", "state": "prepared", "material": STANDARD.encode([3; 32])},
            {"cache": UID, "id": "524b4731000000000000000100000001",
             "purpose": "page", "state": "active", "material": STANDARD.encode([1; 32])}
        ]);
        value
    }

    #[test]
    fn legacy_keys_upgrade_to_compact_state() {
        use racer_wire::control::{
            CacheEncryptionKey, CacheId, CacheKeyPurpose, CacheKeyRef, CacheKeyState, KeyId,
        };

        let legacy = legacy_state();
        assert!(legacy.get("page_bytes").is_none());
        let decoded = decode(UID, true, Some(&serde_json::to_vec(&legacy).unwrap()))
            .unwrap()
            .unwrap();
        let mut expected = record();
        expected.sequence = 7;
        expected.membership_version = 3;
        expected.content_hash = "a".repeat(64);
        expected.membership_hash = "b".repeat(64);
        expected.publication_bytes = 1234;
        expected.desired_seats = 2;
        expected.committed_at = 100;
        expected.keyring.generation = 2;
        expected.keyring.next_rotation = 200;
        expected.keyring.activate_at = 150;
        for (purpose, generation, suffix, state) in [
            (CacheKeyPurpose::Page, 1, 1, CacheKeyState::Active),
            (CacheKeyPurpose::Page, 2, 3, CacheKeyState::Prepared),
            (
                CacheKeyPurpose::OriginCredentials,
                1,
                2,
                CacheKeyState::Active,
            ),
            (
                CacheKeyPurpose::OriginCredentials,
                2,
                4,
                CacheKeyState::Prepared,
            ),
        ] {
            expected.keyring.keys.push(CacheEncryptionKey::new(
                CacheKeyRef {
                    cache: CacheId(UID.into()),
                    id: KeyId::from_generation(generation, suffix).unwrap(),
                    purpose,
                },
                state,
                Zeroizing::new([suffix as u8; 32]),
            ));
        }
        assert_eq!(decoded, expected);
        let encoded = encode(&decoded).unwrap();
        let compact: serde_json::Value = serde_json::from_slice(&encoded).unwrap();
        assert!(compact["keyring"]["keys"].is_string());
        assert_eq!(decode(UID, true, Some(&encoded)), Ok(Some(expected)));

        let mut empty = legacy_state();
        empty["keyring"]["keys"] = serde_json::json!([]);
        assert!(decode(UID, true, Some(&serde_json::to_vec(&empty).unwrap())).is_ok());
    }

    #[test]
    fn legacy_keys_reject_invalid_entries() {
        for (field, bad) in [
            ("id", "00"),
            ("id", "zz4b4731000000000000000200000004"),
            ("id", "éééééééééééééééé"),
            ("purpose", "unknown"),
            ("state", "unknown"),
            ("material", "!"),
            ("material", "AA=="),
            ("cache", "not-a-uuid"),
            ("id", "524b4731000000000000000300000004"),
            ("extra", "unknown"),
        ] {
            let mut value = legacy_state();
            value["keyring"]["keys"][0][field] = bad.into();
            assert_eq!(
                decode(UID, true, Some(&serde_json::to_vec(&value).unwrap())),
                Err(StateError::Invalid),
                "{field}={bad}"
            );
        }
        let mut duplicate = legacy_state();
        duplicate["keyring"]["keys"][0] = duplicate["keyring"]["keys"][1].clone();
        assert_eq!(
            decode(UID, true, Some(&serde_json::to_vec(&duplicate).unwrap())),
            Err(StateError::Invalid)
        );
    }

    /// The page size is optional (older records lack it) and must be a
    /// valid size when present.
    #[test]
    fn page_bytes_optional_and_validated() {
        let r = record();
        let b = encode(&r).unwrap();
        assert!(!String::from_utf8_lossy(&b).contains("page_bytes"));
        let mut sized = r.clone();
        sized.page_bytes = 1 << 20;
        let b = encode(&sized).unwrap();
        assert_eq!(decode(UID, true, Some(&b)).unwrap().unwrap(), sized);
        sized.page_bytes = 3 << 20;
        assert_eq!(
            decode(UID, true, Some(&encode(&sized).unwrap())),
            Err(StateError::Invalid)
        );
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
        assert_eq!(authorize_write(None, &term(1, "a"), None), Ok(()));
        assert_eq!(authorize_write(Some(&r), &term(1, "pod/aa"), None), Ok(()));
        assert_eq!(
            authorize_write(Some(&r), &term(2, "other/bb"), None),
            Ok(())
        );
        assert_eq!(
            authorize_write(Some(&r), &term(0, "pod/aa"), None),
            Err(StateError::StaleTerm)
        );
        // Same transitions, other process of the same Pod.
        assert_eq!(
            authorize_write(Some(&r), &term(1, "pod/bb"), None),
            Err(StateError::StaleTerm)
        );
        let mut replaced = term(5, "pod/aa");
        replaced.lease_uid = "lease-2".into();
        assert_eq!(
            authorize_write(Some(&r), &replaced, None),
            Err(StateError::LeaseReplaced)
        );
        // The stamped Lease is still live: the other UID is dead.
        assert_eq!(
            authorize_write(Some(&r), &replaced, Some("lease-1")),
            Err(StateError::LeaseReplaced)
        );
        // Recreated Lease: the live term takes over, even at lower transitions.
        let mut fresh = term(0, "other/bb");
        fresh.lease_uid = "lease-2".into();
        assert_eq!(authorize_write(Some(&r), &fresh, Some("lease-2")), Ok(()));
    }

    #[test]
    fn live_lease_takes_over_and_dead_lease_is_fenced() {
        let mut new_term = term(0, "other/bb");
        new_term.lease_uid = "lease-2".into();
        let mut newer = Writer::new(new_term.clone());
        assert!(newer.observe(Some(&record()), Some("lease-2")).is_ok());
        let stamped = newer.stamp(record(), UID, "q", 9);
        assert_eq!(check_monotonic(&record(), &stamped), Ok(()));
        // Later reads of its own record need no live UID.
        assert!(newer.observe(Some(&stamped), None).is_ok());
        // The old holder sees the new UID, which is not its own live UID.
        let mut old = Writer::new(term(1, "pod/aa"));
        assert_eq!(
            old.observe(Some(&stamped), Some("lease-2")),
            Err(StateError::LeaseReplaced)
        );
        assert!(!old.may_write());
        // Even with a stale view claiming its Lease is live, it stays fenced.
        assert_eq!(
            old.observe(Some(&record()), Some("lease-1")),
            Err(StateError::StaleTerm)
        );
    }

    #[test]
    fn old_writer_never_retries_after_replacement() {
        let mut old = Writer::new(term(1, "pod/aa"));
        assert!(old.observe(Some(&record()), None).is_ok());
        let mut newer = record();
        newer.term = term(2, "other/bb");
        assert_eq!(old.observe(Some(&newer), None), Err(StateError::StaleTerm));
        assert!(!old.may_write());
        // Even if the old record shows up again, the writer stays fenced.
        assert_eq!(
            old.observe(Some(&record()), None),
            Err(StateError::StaleTerm)
        );
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
