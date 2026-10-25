//! Following committed state on a seat, lifted from `seat::follow_once` and
//! `seat::install_record`.

use super::{Record, StateError, StateRecord};
use crate::authority::state;
use std::time::{Duration, Instant};

/// Least time between state Secret reads.
pub const MIN_READ_INTERVAL: Duration = Duration::from_secs(1);

/// What a seat read from the state Secret.
#[derive(Clone, Copy, Debug)]
pub struct SecretRead<'a> {
    /// Cluster ID this process serves.
    pub cluster: &'a str,
    /// Secret UID.
    pub uid: &'a str,
    /// The initialized annotation is set.
    pub initialized: bool,
    /// Initialized markers and `data.cluster` equal the UID.
    pub markers_match: bool,
    /// `data.state`: absent, valid base64 decoded, or undecodable.
    pub data: Option<Result<&'a [u8], ()>>,
    /// This process already accepted a record (high water is set).
    pub accepted_before: bool,
}

/// Decision for one read.
#[derive(Debug, Eq, PartialEq)]
pub enum Read {
    /// State is missing, invalid, or wiped: stop serving.
    FailClosed,
    /// Not initialized yet: nothing to install.
    Uninitialized,
    /// Install this record, see [`admit`].
    Install(Box<StateRecord>),
}

/// Classify one state Secret read.
pub fn read(r: &SecretRead<'_>) -> Read {
    if r.uid != r.cluster {
        return Read::FailClosed;
    }
    if r.initialized && !r.markers_match {
        return Read::FailClosed;
    }
    let data = match r.data {
        Some(Ok(d)) => Some(d),
        Some(Err(())) => return Read::FailClosed,
        None => None,
    };
    match state::decode(r.uid, r.initialized, data) {
        Ok(Some(record)) => Read::Install(Box::new(record)),
        Ok(None) if r.accepted_before => Read::FailClosed,
        Ok(None) => Read::Uninitialized,
        Err(_) => Read::FailClosed,
    }
}

/// Whether a state read is older news rather than a rollback: another
/// installer (the primary's own commit, `primary.rs` `reconcile`) raised the
/// high water mark while the read was in flight, and the read is older
/// than it (the same sequence is installed as usual, so a seat still
/// learns it serves that sequence). The read may have been answered before that commit; it is
/// dropped instead of failing closed. A genuine rollback is still refused by
/// the next read, which starts after the installation (Known issue 421).
pub fn superseded(
    installed_during_read: bool,
    high_water: Option<&StateRecord>,
    record: &StateRecord,
) -> bool {
    installed_during_read && high_water.is_some_and(|h| record.sequence < h.sequence)
}

/// Whether `record` may replace the high water mark. A rollback is never
/// accepted and makes the caller fail closed.
pub fn admit(high_water: Option<&StateRecord>, record: &StateRecord) -> Result<(), StateError> {
    match high_water {
        Some(old) => state::check_monotonic(old, record).map_err(|_| StateError::Rollback),
        None => Ok(()),
    }
}

/// Whether a seat must copy the image for `record` from the primary.
pub fn need_image(record: &StateRecord, image: Option<&Record>, is_primary: bool) -> bool {
    !is_primary
        && record.sequence != 0
        && image
            .is_none_or(|i| i.sequence != record.sequence || i.content_hash != record.content_hash)
}

/// Committed state as a seat holds it, without payloads. The simulator uses
/// it directly; production keeps the same fields in `shared::Serving`.
#[derive(Clone, Debug, Default)]
pub struct Follower {
    pub high_water: Option<StateRecord>,
    pub record: Option<StateRecord>,
    pub confirmed: Option<Instant>,
    pub image: Option<Record>,
    /// Records installed so far (see [`superseded`]).
    pub installs: u64,
}

impl Follower {
    /// Install a record confirmed at `confirmed`.
    pub fn install(&mut self, record: StateRecord, confirmed: Instant) -> Result<(), StateError> {
        if let Err(e) = admit(self.high_water.as_ref(), &record) {
            self.fail_closed();
            return Err(e);
        }
        self.high_water = Some(record.clone());
        self.record = Some(record);
        self.confirmed = Some(confirmed);
        self.installs += 1;
        Ok(())
    }

    /// Stop serving. The high water mark is kept.
    pub fn fail_closed(&mut self) {
        self.record = None;
        self.confirmed = None;
        self.image = None;
    }

    /// Install a verified image for the current record. Returns false when
    /// it does not match.
    pub fn install_image(&mut self, image: Record) -> bool {
        let ok = self
            .record
            .as_ref()
            .is_some_and(|r| r.sequence == image.sequence && r.content_hash == image.content_hash);
        if ok {
            self.image = Some(image);
        }
        ok
    }

    /// Serving: fresh committed state and a matching image.
    pub fn ready(&self, now: Instant) -> bool {
        let fresh = self
            .confirmed
            .is_some_and(|c| now.duration_since(c) < crate::limits::STATE_FRESHNESS);
        fresh
            && self.record.as_ref().is_some_and(|r| {
                !need_image(r, self.image.as_ref(), false)
                    && self
                        .image
                        .as_ref()
                        .is_some_and(|i| i.sequence == r.sequence)
            })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const C: &str = "c1c1c1c1-0000-4000-8000-000000000001";

    fn record(sequence: u64) -> StateRecord {
        let hash = |c: char| {
            if sequence == 0 {
                String::new()
            } else {
                c.to_string().repeat(64)
            }
        };
        StateRecord {
            version: state::VERSION,
            cluster: C.into(),
            term: state::Term {
                lease_uid: "l".into(),
                transitions: 1,
                holder: "p/00".into(),
            },
            writer_pod_uid: "u".into(),
            sequence,
            membership_version: sequence,
            content_hash: hash('a'),
            membership_hash: hash('b'),
            ..Default::default()
        }
    }

    fn secret<'a>(data: Option<Result<&'a [u8], ()>>, initialized: bool) -> SecretRead<'a> {
        SecretRead {
            cluster: C,
            uid: C,
            initialized,
            markers_match: true,
            data,
            accepted_before: false,
        }
    }

    #[test]
    fn superseded_reads_are_dropped_only_after_an_install_during_the_read() {
        let high = record(5);
        // Older news while another installer raised the mark.
        assert!(superseded(true, Some(&high), &record(4)));
        // The same record: installed as usual.
        assert!(!superseded(true, Some(&high), &record(5)));
        // Newer: installed as usual.
        assert!(!superseded(true, Some(&high), &record(6)));
        // Nothing installed during the read: a rollback is a rollback.
        assert!(!superseded(false, Some(&high), &record(4)));
        assert!(!superseded(true, None, &record(4)));
        let mut f = Follower::default();
        let now = Instant::now();
        f.install(record(4), now).unwrap();
        f.install(record(5), now).unwrap();
        assert_eq!(f.installs, 2);
        assert!(f.install(record(4), now).is_err());
        assert_eq!(f.installs, 2, "a refused record is not an install");
    }

    #[test]
    fn read_classification() {
        let bytes = state::encode(&record(3)).unwrap();
        assert_eq!(
            read(&secret(Some(Ok(&bytes)), true)),
            Read::Install(Box::new(record(3)))
        );
        assert_eq!(read(&secret(None, false)), Read::Uninitialized);
        let mut wiped = secret(None, false);
        wiped.accepted_before = true;
        assert_eq!(read(&wiped), Read::FailClosed);
        assert_eq!(read(&secret(Some(Err(())), true)), Read::FailClosed);
        let mut other = secret(Some(Ok(&bytes)), true);
        other.uid = "x";
        assert_eq!(read(&other), Read::FailClosed);
        let mut markers = secret(Some(Ok(&bytes)), true);
        markers.markers_match = false;
        assert_eq!(read(&markers), Read::FailClosed);
        assert_eq!(read(&secret(Some(Ok(b"{")), true)), Read::FailClosed);
    }

    #[test]
    fn install_refuses_rollback_and_keeps_high_water() {
        let t = Instant::now();
        let mut f = Follower::default();
        f.install(record(5), t).unwrap();
        assert!(f.install(record(4), t).is_err());
        assert!(f.record.is_none());
        assert_eq!(f.high_water.as_ref().unwrap().sequence, 5);
        f.install(record(6), t).unwrap();
    }

    #[test]
    fn image_rules() {
        let t = Instant::now();
        let r = record(2);
        let img = Record {
            sequence: 2,
            membership_version: 2,
            content_hash: r.content_hash.clone(),
            membership_hash: r.membership_hash.clone(),
        };
        assert!(need_image(&r, None, false));
        assert!(!need_image(&r, None, true));
        assert!(!need_image(&record(0), None, false));
        assert!(!need_image(&r, Some(&img), false));
        let mut f = Follower::default();
        assert!(!f.install_image(img.clone()));
        f.install(r, t).unwrap();
        assert!(!f.ready(t));
        assert!(f.install_image(img));
        assert!(f.ready(t));
        assert!(!f.ready(t + crate::limits::STATE_FRESHNESS));
    }
}
