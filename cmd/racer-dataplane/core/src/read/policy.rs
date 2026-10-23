//! Small read-path policy rules shared by fill, serve, and race drivers.

use super::candidates::ResponseKind;
use super::error::Error;

/// Whether a contender's failure leaves the other contender of a race free to
/// win. Malformed framing is local to that contender, not authority to cancel
/// another independently authenticated usable page.
pub fn recoverable(error: Error) -> bool {
    matches!(
        error,
        Error::Unavailable
            | Error::Io
            | Error::Os(_)
            | Error::Overloaded
            | Error::CorruptRecord
            | Error::MissingKey
            | Error::Cancelled
            | Error::InvalidRequest
            | Error::HeaderTooLarge
    )
}

/// Translate an expected acquisition failure into the peer response that
/// reports it. `None` means the error is internal and must propagate.
pub fn peer_error(error: Error) -> Option<ResponseKind> {
    Some(match error {
        Error::NotFound => ResponseKind::NotFound,
        Error::VersionUnavailable => ResponseKind::VersionUnavailable,
        Error::Overloaded => ResponseKind::Overloaded,
        Error::OriginRejected => ResponseKind::OriginRejected,
        Error::OriginForbidden => ResponseKind::OriginForbidden,
        Error::Unavailable | Error::HopBudgetExhausted | Error::DeadlineExceeded => {
            ResponseKind::Unavailable
        }
        _ => return None,
    })
}

/// Optional writes may use half of the aggregate class budget, with a floor
/// sufficient for one valid page when the configured budget can hold it. Owned
/// usage counts against this conservative ceiling; quota admission remains the
/// authority for per-cache fairness and all completion-owned allocations.
pub fn optional_headroom(limit: usize, used: usize, amount: usize, one_page: usize) -> bool {
    let ceiling = optional_ceiling(limit, one_page);
    amount <= ceiling && used <= ceiling - amount
}

/// Ceiling for optional (non-owned) usage of one resource class.
pub fn optional_ceiling(limit: usize, one_page: usize) -> usize {
    limit.min((limit / 2).max(one_page))
}

/// Second-sight admission: owned pages always qualify; others only when
/// retention is enabled and the page was seen before.
pub fn retention_eligible(owned: bool, enabled: bool, seen: bool) -> bool {
    owned || (enabled && seen)
}

/// Resident retention score: exact heat plus one for owned pages, zero when
/// retention is disabled.
pub fn retention_score(heat: u8, owned: bool, enabled: bool) -> u8 {
    if enabled { heat + u8::from(owned) } else { 0 }
}

/// Why a verified page is or is not persisted to local disk.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Persist {
    /// Not eligible under second-sight, or metadata did not validate.
    Ineligible,
    /// Already resident in a copy-only slot or the disk index.
    Resident,
    /// A non-owned page lacked optional dirty headroom.
    NoHeadroom,
    /// Reserve and enqueue the write.
    Write,
}

/// Facts needed to decide persistence of one verified page.
#[derive(Clone, Copy, Debug)]
pub struct PersistFacts {
    pub eligible: bool,
    pub metadata_valid: bool,
    pub resident: bool,
    pub owned: bool,
    pub dirty_limit: usize,
    pub dirty_used: usize,
    pub amount: usize,
    pub one_page: usize,
}

/// Persist decision before reservation; reservation failures are separate.
pub fn persist(facts: PersistFacts) -> Persist {
    if !facts.eligible || !facts.metadata_valid {
        Persist::Ineligible
    } else if facts.resident {
        Persist::Resident
    } else if !facts.owned
        && !optional_headroom(
            facts.dirty_limit,
            facts.dirty_used,
            facts.amount,
            facts.one_page,
        )
    {
        Persist::NoHeadroom
    } else {
        Persist::Write
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn recoverable_errors() {
        for error in [
            Error::Unavailable,
            Error::Io,
            Error::Os(5),
            Error::Overloaded,
            Error::CorruptRecord,
            Error::MissingKey,
            Error::Cancelled,
            Error::InvalidRequest,
            Error::HeaderTooLarge,
        ] {
            assert!(recoverable(error), "{error:?}");
        }
        for error in [
            Error::Unauthorized,
            Error::DeadlineExceeded,
            Error::NotFound,
            Error::OriginForbidden,
        ] {
            assert!(!recoverable(error), "{error:?}");
        }
    }

    #[test]
    fn peer_error_table() {
        assert_eq!(peer_error(Error::NotFound), Some(ResponseKind::NotFound));
        assert_eq!(
            peer_error(Error::DeadlineExceeded),
            Some(ResponseKind::Unavailable)
        );
        assert_eq!(
            peer_error(Error::HopBudgetExhausted),
            Some(ResponseKind::Unavailable)
        );
        assert_eq!(
            peer_error(Error::OriginRejected),
            Some(ResponseKind::OriginRejected)
        );
        assert_eq!(peer_error(Error::Unauthorized), None);
        assert_eq!(peer_error(Error::Internal), None);
    }

    #[test]
    fn optional_headroom_floor_and_half() {
        assert!(optional_headroom(100, 0, 50, 10));
        assert!(!optional_headroom(100, 1, 50, 10));
        assert!(optional_headroom(100, 0, 60, 60));
        assert!(!optional_headroom(50, 0, 60, 60));
        assert!(!optional_headroom(0, 0, 1, 1));
        assert_eq!(optional_ceiling(10, 100), 10);
    }

    #[test]
    fn retention_rules() {
        assert!(retention_eligible(true, false, false));
        assert!(!retention_eligible(false, false, true));
        assert!(retention_eligible(false, true, true));
        assert!(!retention_eligible(false, true, false));
        assert_eq!(retention_score(2, true, true), 3);
        assert_eq!(retention_score(2, true, false), 0);
    }

    #[test]
    fn persist_decisions() {
        let base = PersistFacts {
            eligible: true,
            metadata_valid: true,
            resident: false,
            owned: false,
            dirty_limit: 100,
            dirty_used: 0,
            amount: 10,
            one_page: 10,
        };
        assert_eq!(persist(base), Persist::Write);
        assert_eq!(
            persist(PersistFacts {
                eligible: false,
                ..base
            }),
            Persist::Ineligible
        );
        assert_eq!(
            persist(PersistFacts {
                metadata_valid: false,
                ..base
            }),
            Persist::Ineligible
        );
        assert_eq!(
            persist(PersistFacts {
                resident: true,
                ..base
            }),
            Persist::Resident
        );
        assert_eq!(
            persist(PersistFacts {
                dirty_used: 45,
                ..base
            }),
            Persist::NoHeadroom
        );
        assert_eq!(
            persist(PersistFacts {
                dirty_used: 45,
                owned: true,
                ..base
            }),
            Persist::Write
        );
    }
}
