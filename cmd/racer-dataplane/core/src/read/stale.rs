//! Walking the previous membership when the current one's owners are behind.
//!
//! While a membership version propagates, a requester that already adopted
//! version N asks owners that still hold N - 1. They cannot resolve N and
//! answer `StaleMembership`; with nothing newer to refresh to, the requester
//! skipped them as stale destinations ([`super::walk::stale_action`]), each
//! skip spending the credits that funded the probe. When every owner of a
//! page was behind (for example all of them joined in N), the walk was
//! exhausted and the read failed, although the page's owners under N - 1,
//! which every peer still resolves during the grace window, could serve it
//! (Known issue 19).
//!
//! [`previous_fallback`] decides when a non-owner, at its first stale
//! destination, stops walking N and walks the page's owners under N - 1
//! instead, once, with what is left of the same budget. A destination behind
//! N resolves N - 1, and so does one that adopted N (grace retention), so
//! the second walk does not meet the same skips. Budgets never grow, and the
//! walk under N - 1 must not give this node origin authority:
//! [`fallback_rank_allowed`] ends it when this node owns the page there.

/// Facts about a walk under membership N whose destination was behind.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct StaleSkip {
    /// This node owns the page under N (owners have their own
    /// previous-membership probes, `WalkConfig::previous` and handoff
    /// targets, and their origin authority comes from N).
    pub owner: bool,
    /// This read already refreshed its membership or fell back once.
    pub retried: bool,
    /// N - 1 is still resolvable here, was replaced within
    /// [`super::walk::PREVIOUS_MEMBERSHIP_WITHIN`] (so peers that adopted N
    /// still hold it), and lists this node (peers reject non-members).
    pub previous_recent_member: bool,
}

/// Whether to walk the page's owners under N - 1 instead of skipping a
/// destination that is behind.
pub fn previous_fallback(s: StaleSkip) -> bool {
    !s.owner && !s.retried && s.previous_recent_member
}

/// Whether the fallback walk may proceed with this node at `rank` under
/// N - 1. A page owner under N - 1 that is not one under N would gain
/// origin authority it no longer has, so the walk ends instead.
pub fn fallback_rank_allowed(rank: Option<usize>) -> bool {
    rank.is_none()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn skip() -> StaleSkip {
        StaleSkip {
            owner: false,
            retried: false,
            previous_recent_member: true,
        }
    }

    #[test]
    fn non_owner_falls_back_once() {
        assert!(previous_fallback(skip()));
    }

    #[test]
    fn no_fallback_without_a_usable_previous_membership() {
        // Already retried (refreshed or fell back): at most one more walk.
        assert!(!previous_fallback(StaleSkip {
            retried: true,
            ..skip()
        }));
        // Peers may no longer resolve N - 1, or would reject this node.
        assert!(!previous_fallback(StaleSkip {
            previous_recent_member: false,
            ..skip()
        }));
        // Owners keep their walk under N.
        assert!(!previous_fallback(StaleSkip {
            owner: true,
            ..skip()
        }));
        assert!(!previous_fallback(StaleSkip::default()));
    }

    #[test]
    fn fallback_never_gives_origin_authority() {
        assert!(fallback_rank_allowed(None));
        for rank in 0..3 {
            assert!(!fallback_rank_allowed(Some(rank)));
        }
    }
}
