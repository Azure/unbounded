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
//!
//! A reader that cannot fall back (it joined in N, so it is not a member of
//! N - 1) waits for the destination to adopt N instead, at most
//! [`MAX_STALE_WAITS`] times ([`stale_wait`], Known issues 26 and 36).

use std::time::Duration;

/// Most time one wait for a destination that is behind may take
/// ([`stale_wait`]).
pub const MAX_STALE_WAIT: Duration = Duration::from_secs(4);

/// Most waits one read makes for destinations that are behind
/// ([`stale_wait`]). A membership reaches every client within the seats'
/// write window after a commit (about 5 s at 50,000 clients, Known issue
/// 36): with a 10 s deadline, two waits (a third, then a third of the rest)
/// cover 5.5 s and leave the rest for the remaining owners.
pub const MAX_STALE_WAITS: u8 = 2;

/// Shortest wait worth making: below this the destination is unlikely to
/// have adopted the membership, and the read keeps walking instead.
pub const MIN_STALE_WAIT: Duration = Duration::from_millis(250);

/// Facts about a destination that answered `StaleMembership` because it is
/// behind this node, for a reader that cannot walk the previous membership.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct StaleWaitFacts {
    /// This node owns the page (owners go on to their own fetch).
    pub owner: bool,
    /// [`previous_fallback`] walks the previous membership instead.
    pub fallback: bool,
    /// Waits this read already made for destinations that are behind.
    pub waits: u8,
    /// One more exchange with the destination still leaves the walk
    /// enough attempts for its later visits
    /// (`super::walk::CandidateWalk::retry_affordable`). Required for every
    /// wait after the first.
    pub affordable: bool,
    /// Time left until the read's deadline.
    pub remaining: Duration,
}

/// How long a non-owner waits before asking the same destination again,
/// instead of skipping it, when the destination is behind this node's
/// membership and the previous membership is no help (Known issue 26).
///
/// A reader that joined in membership N is not a member of N - 1, so peers
/// would reject it there ([`previous_fallback`] refuses), and an owner still
/// on N - 1 cannot resolve N. Skipping every such owner exhausted the walk
/// and failed the read, although the owners adopt N within the propagation
/// window. Waiting a third of the time left (at most [`MAX_STALE_WAIT`],
/// at most [`MAX_STALE_WAITS`] times per read, never below
/// [`MIN_STALE_WAIT`]) leaves the rest of the
/// deadline for the remaining owners. The destination's verified
/// `StaleMembership` returns the credits it was sent
/// ([`super::budget::AcquisitionBudget::refund_remote`]), so the second ask
/// costs one exchange and its links, like any probe; budgets never grow.
pub fn stale_wait(facts: StaleWaitFacts) -> Option<Duration> {
    if facts.owner
        || facts.fallback
        || facts.waits >= MAX_STALE_WAITS
        || (facts.waits > 0 && !facts.affordable)
    {
        return None;
    }
    let wait = (facts.remaining / 3).min(MAX_STALE_WAIT);
    (wait >= MIN_STALE_WAIT).then_some(wait)
}

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

    fn waiting() -> StaleWaitFacts {
        StaleWaitFacts {
            remaining: Duration::from_secs(9),
            affordable: true,
            ..StaleWaitFacts::default()
        }
    }

    #[test]
    fn a_reader_that_cannot_fall_back_waits_twice() {
        assert_eq!(stale_wait(waiting()), Some(Duration::from_secs(3)));
        // The first wait does not depend on funding (Known issue 26).
        assert_eq!(
            stale_wait(StaleWaitFacts {
                affordable: false,
                ..waiting()
            }),
            Some(Duration::from_secs(3))
        );
        // Known issue 36: a second wait, a third of what is left, covers
        // the 50k propagation window (about 5 s) within a 10 s deadline.
        let deadline = Duration::from_secs(10);
        let first = stale_wait(StaleWaitFacts {
            remaining: deadline,
            ..waiting()
        })
        .unwrap();
        let second = stale_wait(StaleWaitFacts {
            remaining: deadline - first,
            waits: 1,
            ..waiting()
        })
        .unwrap();
        assert!(
            first + second >= Duration::from_millis(5500),
            "{first:?} {second:?}"
        );
        assert_eq!(
            stale_wait(StaleWaitFacts {
                remaining: deadline - first - second,
                waits: 2,
                ..waiting()
            }),
            None
        );
        // Capped, and a third of what is left.
        let long = StaleWaitFacts {
            remaining: Duration::from_secs(60),
            ..waiting()
        };
        assert_eq!(stale_wait(long), Some(MAX_STALE_WAIT));
        let short = StaleWaitFacts {
            remaining: Duration::from_millis(900),
            ..waiting()
        };
        assert_eq!(stale_wait(short), Some(Duration::from_millis(300)));
    }

    #[test]
    fn no_wait_for_owners_fallbacks_repeats_or_short_deadlines() {
        for facts in [
            StaleWaitFacts {
                owner: true,
                ..waiting()
            },
            StaleWaitFacts {
                fallback: true,
                ..waiting()
            },
            StaleWaitFacts {
                waits: MAX_STALE_WAITS,
                ..waiting()
            },
            // A further wait must leave the later visits funded.
            StaleWaitFacts {
                waits: 1,
                affordable: false,
                ..waiting()
            },
            StaleWaitFacts {
                remaining: Duration::from_millis(749),
                ..waiting()
            },
            StaleWaitFacts::default(),
        ] {
            assert_eq!(stale_wait(facts), None, "{facts:?}");
        }
        let edge = StaleWaitFacts {
            remaining: Duration::from_millis(750),
            ..waiting()
        };
        assert_eq!(stale_wait(edge), Some(MIN_STALE_WAIT));
    }

    #[test]
    fn fallback_never_gives_origin_authority() {
        assert!(fallback_rank_allowed(None));
        for rank in 0..3 {
            assert!(!fallback_rank_allowed(Some(rank)));
        }
    }
}
