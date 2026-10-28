//! The single route budget rule shared by the dataplane and `peer-wire`.
//!
//! A route is a path of nodes `origin, relay.., destination`. Each step between
//! two nodes is one link. A route may use at most [`MAX_LINKS`] links.
//!
//! Two encodings differ by one node:
//!
//! - Wire form (signed headers): `visited` includes the sender, so the rule is
//!   `visited.len() + links <= MAX_LINKS + 1` (the origin sends `[origin]`
//!   with 8 links).
//! - Search form: `visited` excludes the holder, so the rule is
//!   `visited.len() + links <= MAX_LINKS`. [`search_budget`] converts.
//!
//! The module also holds per-hop failure tables and response-validity rules.

use std::time::Instant;

/// Maximum number of links on any route.
pub const MAX_LINKS: u8 = 8;

/// Why a route budget is rejected.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum RouteFault {
    /// The signed deadline has passed.
    DeadlineExceeded,
    /// The route would exceed [`MAX_LINKS`] or has no link left to spend.
    HopBudgetExhausted,
    /// The route revisits a node, or is otherwise malformed.
    Invalid,
}

/// Wire-form shape check: nonempty `visited`, `links` in `1..=MAX_LINKS`.
pub fn check_wire(visited: usize, links: u64) -> Result<(), RouteFault> {
    if links == 0 || links > u64::from(MAX_LINKS) || visited == 0 || !within_wire(visited, links) {
        return Err(RouteFault::HopBudgetExhausted);
    }
    Ok(())
}

/// Wire-form sum rule alone, used where an empty route or zero links is handled
/// by a later check (logical request decoding).
pub fn within_wire(visited: usize, links: u64) -> bool {
    (visited as u64).saturating_add(links) <= u64::from(MAX_LINKS) + 1
}

/// Wire-form transition: one signer appends itself and spends exactly one link.
pub fn check_wire_transition(old_links: u64, new_links: u64) -> Result<(), RouteFault> {
    if old_links <= 1 || new_links != old_links - 1 {
        return Err(RouteFault::HopBudgetExhausted);
    }
    Ok(())
}

/// Search-form rule: `visited` excludes `holder`.
pub fn validate<N: PartialEq>(
    visited: &[N],
    holder: &N,
    destination: &N,
    links: u8,
    deadline: Instant,
    now: Instant,
) -> Result<(), RouteFault> {
    if now >= deadline {
        return Err(RouteFault::DeadlineExceeded);
    }
    if visited.len() > usize::from(MAX_LINKS)
        || visited.len() + usize::from(links) > usize::from(MAX_LINKS)
    {
        return Err(RouteFault::HopBudgetExhausted);
    }
    if visited.contains(holder)
        || visited.contains(destination)
        || visited
            .iter()
            .enumerate()
            .any(|(i, node)| visited[..i].contains(node))
    {
        return Err(RouteFault::Invalid);
    }
    Ok(())
}

/// Search-form forward from `holder` to `next`: returns the links left after
/// spending one. The caller appends `holder` to `visited`.
pub fn forward<N: PartialEq>(
    visited: &[N],
    holder: &N,
    next: &N,
    destination: &N,
    links: u8,
    deadline: Instant,
    now: Instant,
) -> Result<u8, RouteFault> {
    validate(visited, holder, destination, links, deadline, now)?;
    if links == 0 {
        return Err(RouteFault::HopBudgetExhausted);
    }
    if next == holder || visited.contains(next) {
        return Err(RouteFault::Invalid);
    }
    Ok(links - 1)
}

/// Search budget for `local` from a received wire-form route: how many
/// leading `visited` entries to keep and the links left. The origin searching
/// its own route spends no link.
pub fn search_budget<N: PartialEq>(
    visited: &[N],
    links: u8,
    local: &N,
) -> Result<(usize, u8), RouteFault> {
    if visited.last() == Some(local) {
        return Ok((visited.len() - 1, links));
    }
    let links = links.checked_sub(1).ok_or(RouteFault::HopBudgetExhausted)?;
    Ok((visited.len(), links))
}

/// Links on the outbound wire-form route after relay `local` appends itself.
pub fn relay<N: PartialEq>(
    visited: &[N],
    destination: &N,
    links: u8,
    local: &N,
) -> Result<u8, RouteFault> {
    if destination == local || visited.contains(local) {
        return Err(RouteFault::Invalid);
    }
    links.checked_sub(1).ok_or(RouteFault::HopBudgetExhausted)
}

/// Link retry backoff after `failures` consecutive failures: 100 ms
/// doubling per failure up to 20 s, plus `jitter` reduced to at most a
/// quarter of the base. Callers supply `jitter` deterministically per link.
pub fn link_backoff(failures: u32, jitter: u64) -> std::time::Duration {
    let base = (100u64 << failures.saturating_sub(1).min(8)).min(20_000);
    std::time::Duration::from_millis(base + jitter % (base / 4 + 1))
}

/// Failure classes the peer decisions distinguish. `Other` propagates unchanged.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Fault {
    Overloaded,
    Unavailable,
    Io,
    Os,
    DeadlineExceeded,
    BadGateway,
    CorruptRecord,
    Cancelled,
    HopBudgetExhausted,
    IncompatibleMembership,
    NotFound,
    VersionUnavailable,
    Unauthorized,
    OriginForbidden,
    MissingKey,
    Other,
}

/// What a node does with a verified request.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Hop {
    /// The signed membership is not one this node can resolve.
    StaleMembership,
    /// This node is the destination.
    Serve,
    /// Forward toward the destination.
    Relay(RelayPlan),
}

/// Relay budgets, kept as results so the caller can reject
/// [`RouteFault::Invalid`] before relay admission and use the rest after.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct RelayPlan {
    pub search: Result<(usize, u8), RouteFault>,
    pub outbound: Result<u8, RouteFault>,
}

/// Decide the hop at `local` for a verified wire-form route.
pub fn decide_hop<N: PartialEq>(
    local: &N,
    visited: &[N],
    destination: &N,
    links: u8,
    membership_known: bool,
) -> Hop {
    if !membership_known {
        return Hop::StaleMembership;
    }
    if destination == local {
        return Hop::Serve;
    }
    Hop::Relay(RelayPlan {
        search: search_budget(visited, links, local),
        outbound: relay(visited, destination, links, local),
    })
}

/// Signed response kinds a failure can turn into.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Response {
    Overloaded,
    Unavailable,
    NotFound,
    VersionUnavailable,
    OriginRejected,
    OriginForbidden,
    StaleMembership,
}

/// How a relay answers when forwarding failed.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum RelayFailure {
    /// Sign this response.
    Respond(Response),
    /// Sign this response only if the caller's scope is still live.
    RespondIfLive(Response),
    /// Return the error to the caller unsigned.
    Propagate,
}

/// Relay failure table. A relay never signs `Unavailable` for a ping's
/// transport failure: the pinger would hold a possibly alive destination
/// silent (an old destination closes on the unknown ping mode).
pub fn relay_failure(fault: Fault, ping: bool) -> RelayFailure {
    match fault {
        Fault::Io | Fault::Os if ping => RelayFailure::Propagate,
        Fault::Overloaded => RelayFailure::Respond(Response::Overloaded),
        Fault::Unavailable
        | Fault::Io
        | Fault::Os
        | Fault::HopBudgetExhausted
        | Fault::IncompatibleMembership => RelayFailure::RespondIfLive(Response::Unavailable),
        _ => RelayFailure::Propagate,
    }
}

/// Destination failure table. `None` propagates the error unsigned.
pub fn serve_failure(fault: Fault) -> Option<Response> {
    Some(match fault {
        Fault::NotFound => Response::NotFound,
        Fault::VersionUnavailable => Response::VersionUnavailable,
        Fault::Overloaded => Response::Overloaded,
        Fault::Unauthorized => Response::OriginRejected,
        Fault::OriginForbidden => Response::OriginForbidden,
        Fault::Unavailable | Fault::Io | Fault::Os | Fault::MissingKey | Fault::CorruptRecord => {
            Response::Unavailable
        }
        _ => return None,
    })
}

/// Waiter admission at the destination: only overload is answered.
pub fn waiter_failure(fault: Fault) -> Option<Response> {
    (fault == Fault::Overloaded).then_some(Response::Overloaded)
}

/// Evidence about one link, independent of process readiness.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum LinkOutcome {
    Success,
    Timeout,
    Refused,
    ProtocolFailure,
}

/// Link evidence from one operation. Misses and credential rejections do not
/// open transport circuits.
pub fn operation_link_outcome(result: Result<(), Fault>) -> Option<LinkOutcome> {
    match result {
        Err(Fault::Io | Fault::Os | Fault::Unavailable) => Some(LinkOutcome::Refused),
        Err(Fault::DeadlineExceeded) => Some(LinkOutcome::Timeout),
        Err(Fault::BadGateway | Fault::CorruptRecord) => Some(LinkOutcome::ProtocolFailure),
        Err(Fault::Cancelled | Fault::Overloaded) => None,
        _ => Some(LinkOutcome::Success),
    }
}

/// Result of one outbound exchange, as the requester sees it after verifying
/// the reverse chain.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Exchange {
    /// A materialized response. `overloaded` is true for a signed overload.
    Complete { overloaded: bool },
    /// An opaque HTTP relay response, still streaming.
    Streaming,
    /// The exchange failed. `socket_failure` is true when the immediate socket
    /// failed.
    Failed { fault: Fault, socket_failure: bool },
}

/// Link, adaptive admission, and silence evidence from one exchange. Only a
/// socket failure blames the immediate peer; a downstream error never does.
pub fn exchange_evidence(
    exchange: Exchange,
    admitted: bool,
) -> (
    Option<LinkOutcome>,
    flow_control::AdaptiveOutcome,
    Option<bool>,
) {
    use flow_control::AdaptiveOutcome as Outcome;
    match exchange {
        Exchange::Complete { overloaded: true } => {
            (Some(LinkOutcome::Success), Outcome::Neutral, Some(true))
        }
        Exchange::Complete { overloaded: false } => {
            (Some(LinkOutcome::Success), Outcome::Verified, Some(true))
        }
        Exchange::Streaming => (None, Outcome::Neutral, Some(true)),
        Exchange::Failed {
            fault,
            socket_failure,
        } => (
            (socket_failure && !admitted).then_some(LinkOutcome::Refused),
            match fault {
                Fault::Overloaded => Outcome::LocalPressure,
                _ => Outcome::Neutral,
            },
            (!socket_failure && matches!(fault, Fault::DeadlineExceeded | Fault::Cancelled))
                .then_some(false),
        ),
    }
}

/// Authenticated outcome of a peer response.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Outcome {
    Page,
    Metadata,
    Bootstrap,
    Selected,
    NotFound,
    Held,
    Warmed,
    Released,
    /// Failure outcomes any hop may report (`unavailable`, `overloaded`, ...).
    Other,
}

impl Outcome {
    pub fn from_wire(value: &str) -> Self {
        match value {
            "page" => Self::Page,
            "metadata" => Self::Metadata,
            "bootstrap" => Self::Bootstrap,
            "selected" => Self::Selected,
            "not-found" => Self::NotFound,
            "held" => Self::Held,
            "warmed" => Self::Warmed,
            "released" => Self::Released,
            _ => Self::Other,
        }
    }

    /// Outcomes that assert successful data or authoritative absence; only the
    /// requested destination may sign these, a relay only its own failure.
    pub fn asserts_success(self) -> bool {
        !matches!(self, Self::Other)
    }

    /// The warm action this outcome answers, if it is a warm outcome.
    pub fn warm_action(self) -> Option<&'static str> {
        match self {
            Self::Held => Some("hold"),
            Self::Warmed => Some("fill"),
            Self::Released => Some("release"),
            _ => None,
        }
    }

    /// Outcomes that must echo the requested operation name.
    pub fn echoes_operation(self) -> bool {
        matches!(self, Self::Page | Self::Metadata | Self::Bootstrap)
    }
}

/// `not-found` answers only a fresh acquire for metadata or bootstrap.
pub fn not_found_ok(operation: &str, mode: &str, selector: &str) -> bool {
    matches!(operation, "metadata" | "bootstrap") && mode == "acquire" && selector == "fresh"
}

/// A grant is issued for the route's membership.
pub fn grant_membership_ok(grant: u64, route: u64) -> bool {
    grant == route
}

/// A grant names the origin of the route as its receiver.
pub fn grant_receiver_ok<N: PartialEq>(visited: &[N], receiver: &N) -> bool {
    visited.first() == Some(receiver)
}

/// A grant never outlives the signed route and is not already expired.
pub fn grant_deadline_ok(grant_ms: u64, route_ms: u64, now_ms: u64) -> bool {
    grant_ms <= route_ms && grant_ms > now_ms
}

/// A selected page spends exactly one page of the requested page budget.
pub fn page_budget_ok(granted: u64, requested: u64) -> bool {
    granted.checked_add(1) == Some(requested)
}

/// A selected page spends exactly its ciphertext length of the byte budget.
pub fn byte_budget_ok(granted: u64, ciphertext: u64, requested: u64) -> bool {
    granted.checked_add(ciphertext) == Some(requested)
}

/// A granted flight count answers exactly a request that asked for one, and
/// never exceeds what was asked.
pub fn flights_ok(granted: Option<u16>, requested: Option<u16>) -> bool {
    granted.is_some() == requested.is_some() && granted <= requested
}

/// A bootstrap carries page zero exactly when the object is nonempty.
pub fn bootstrap_presence_ok(length: u64, present: u64) -> bool {
    present == u64::from(length != 0)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn link_backoff_doubles_caps_and_bounds_jitter() {
        use std::time::Duration;
        assert_eq!(link_backoff(0, 0), Duration::from_millis(100));
        assert_eq!(link_backoff(1, 0), Duration::from_millis(100));
        assert_eq!(link_backoff(2, 0), Duration::from_millis(200));
        assert_eq!(link_backoff(40, 0), Duration::from_millis(20_000));
        assert_eq!(link_backoff(1, 25), Duration::from_millis(125));
        assert_eq!(link_backoff(1, 26), Duration::from_millis(100));
        assert!(link_backoff(40, u64::MAX) <= Duration::from_millis(25_000));
    }

    use std::time::Duration;

    #[allow(clippy::disallowed_methods)]
    fn times() -> (Instant, Instant) {
        let now = Instant::now();
        (now + Duration::from_secs(1), now)
    }

    #[test]
    fn wire_form_allows_exactly_eight_links() {
        // Origin sends visited = [origin] with eight links: a sum of 9.
        assert_eq!(check_wire(1, 8), Ok(()));
        // Eight links already used, one left.
        assert_eq!(check_wire(8, 1), Ok(()));
        // Nine links in total are rejected, however they are split.
        for (visited, links) in [(1, 9), (2, 8), (9, 1), (5, 5)] {
            assert_eq!(
                check_wire(visited, links),
                Err(RouteFault::HopBudgetExhausted),
                "{visited} + {links}"
            );
        }
        assert_eq!(check_wire(0, 1), Err(RouteFault::HopBudgetExhausted));
        assert_eq!(check_wire(1, 0), Err(RouteFault::HopBudgetExhausted));
        assert_eq!(check_wire(1, u64::MAX), Err(RouteFault::HopBudgetExhausted));
        assert!(within_wire(0, 9));
        assert!(!within_wire(usize::MAX, u64::MAX));
    }

    #[test]
    fn wire_transition_spends_one_link() {
        assert_eq!(check_wire_transition(8, 7), Ok(()));
        assert_eq!(check_wire_transition(2, 1), Ok(()));
        for (old, new) in [(1, 0), (0, 0), (8, 8), (8, 6), (3, 3)] {
            assert_eq!(
                check_wire_transition(old, new),
                Err(RouteFault::HopBudgetExhausted)
            );
        }
    }

    #[test]
    fn search_form_allows_exactly_eight_links() {
        let (deadline, now) = times();
        let visited: Vec<u32> = (0..4).collect();
        assert_eq!(validate(&visited, &10, &11, 4, deadline, now), Ok(()));
        assert_eq!(
            validate(&visited, &10, &11, 5, deadline, now),
            Err(RouteFault::HopBudgetExhausted)
        );
        let nine: Vec<u32> = (0..9).collect();
        assert_eq!(
            validate(&nine, &10, &11, 0, deadline, now),
            Err(RouteFault::HopBudgetExhausted)
        );
        assert_eq!(
            validate(&visited, &10, &11, 4, now, now),
            Err(RouteFault::DeadlineExceeded)
        );
        for (visited, holder, destination) in [
            (vec![1, 2, 1], 10, 11),
            (vec![1, 10], 10, 11),
            (vec![1, 11], 10, 11),
        ] {
            assert_eq!(
                validate(&visited, &holder, &destination, 1, deadline, now),
                Err(RouteFault::Invalid)
            );
        }
    }

    #[test]
    fn forward_is_monotonic_and_rejects_cycles() {
        let (deadline, now) = times();
        assert_eq!(forward(&[1u32], &2, &3, &9, 4, deadline, now), Ok(3));
        assert_eq!(
            forward(&[1u32], &2, &3, &9, 0, deadline, now),
            Err(RouteFault::HopBudgetExhausted)
        );
        assert_eq!(
            forward(&[1u32], &2, &2, &9, 4, deadline, now),
            Err(RouteFault::Invalid)
        );
        assert_eq!(
            forward(&[1u32], &2, &1, &9, 4, deadline, now),
            Err(RouteFault::Invalid)
        );
    }

    #[test]
    fn search_budget_and_relay_match_wire_form() {
        // The origin searching its own route keeps all links.
        assert_eq!(search_budget(&[0u32], 8, &0), Ok((0, 8)));
        // A relay reserves the link that reached it.
        assert_eq!(search_budget(&[0u32], 8, &1), Ok((1, 7)));
        assert_eq!(
            search_budget(&[0u32], 0, &1),
            Err(RouteFault::HopBudgetExhausted)
        );
        // Every search budget derived from a valid wire route is valid search form.
        let (deadline, now) = times();
        for links in 1..=8u8 {
            let visited: Vec<u32> = (0..u32::from(9 - links)).collect();
            assert_eq!(check_wire(visited.len(), u64::from(links)), Ok(()));
            let (keep, left) = search_budget(&visited, links, &100).unwrap();
            assert_eq!(
                validate(&visited[..keep], &100, &200, left, deadline, now),
                Ok(())
            );
            // The relay's outbound route stays valid wire form when it has links.
            let out = relay(&visited, &200, links, &100).unwrap();
            assert_eq!(out, links - 1);
            if out > 0 {
                assert_eq!(check_wire(visited.len() + 1, u64::from(out)), Ok(()));
            }
        }
        assert_eq!(relay(&[0u32], &1, 3, &1), Err(RouteFault::Invalid));
        assert_eq!(relay(&[0u32, 1], &5, 3, &1), Err(RouteFault::Invalid));
        assert_eq!(
            relay(&[0u32], &5, 0, &1),
            Err(RouteFault::HopBudgetExhausted)
        );
    }

    fn route(visited: &[u32], destination: &u32, links: u8) -> Hop {
        decide_hop(&5, visited, destination, links, true)
    }

    #[test]
    fn decide_hop_serves_relays_or_reports_stale() {
        assert_eq!(decide_hop(&5, &[1u32], &5, 4, false), Hop::StaleMembership);
        assert_eq!(decide_hop(&5, &[1u32], &5, 4, true), Hop::Serve);
        assert_eq!(
            route(&[1], &9, 4),
            Hop::Relay(RelayPlan {
                search: Ok((1, 3)),
                outbound: Ok(3),
            })
        );
        // The last link reaches a relay that cannot forward.
        let Hop::Relay(plan) = route(&[1], &9, 0) else {
            panic!()
        };
        assert_ne!(plan.outbound, Err(RouteFault::Invalid));
        assert_eq!(plan.search, Err(RouteFault::HopBudgetExhausted));
        assert_eq!(plan.outbound, Err(RouteFault::HopBudgetExhausted));
        // A cycle through this node is invalid before any budget is spent.
        let Hop::Relay(plan) = route(&[1, 5], &9, 4) else {
            panic!()
        };
        assert_eq!(plan.outbound, Err(RouteFault::Invalid));
    }

    #[test]
    fn failure_tables_match_production() {
        use Fault::*;
        let all = [
            Overloaded,
            Unavailable,
            Io,
            Os,
            DeadlineExceeded,
            BadGateway,
            CorruptRecord,
            Cancelled,
            HopBudgetExhausted,
            IncompatibleMembership,
            NotFound,
            VersionUnavailable,
            Unauthorized,
            OriginForbidden,
            MissingKey,
            Other,
        ];
        for fault in all {
            let relay = relay_failure(fault, false);
            let expected = match fault {
                Overloaded => RelayFailure::Respond(Response::Overloaded),
                Unavailable | Io | Os | HopBudgetExhausted | IncompatibleMembership => {
                    RelayFailure::RespondIfLive(Response::Unavailable)
                }
                _ => RelayFailure::Propagate,
            };
            assert_eq!(relay, expected, "{fault:?}");
            // Other modes keep the table; a ping drops only transport
            // failures to unsigned.
            let ping = if matches!(fault, Io | Os) {
                RelayFailure::Propagate
            } else {
                expected
            };
            assert_eq!(relay_failure(fault, true), ping, "{fault:?}");
        }
        // A pre-ping destination closes on the unknown mode (`Io` at the
        // relay): unsigned, not an `Unavailable` that silences it. A relay
        // that holds the destination silent still signs `Unavailable`.
        assert_eq!(relay_failure(Io, true), RelayFailure::Propagate);
        assert_eq!(
            relay_failure(Unavailable, true),
            RelayFailure::RespondIfLive(Response::Unavailable)
        );
        assert_eq!(
            relay_failure(Overloaded, true),
            RelayFailure::Respond(Response::Overloaded)
        );
        for fault in all {
            assert_eq!(
                waiter_failure(fault).is_some(),
                fault == Overloaded,
                "{fault:?}"
            );
            let link = operation_link_outcome(Err(fault));
            assert_eq!(
                link.is_none(),
                matches!(fault, Cancelled | Overloaded),
                "{fault:?}"
            );
        }
        assert_eq!(serve_failure(Unauthorized), Some(Response::OriginRejected));
        assert_eq!(serve_failure(MissingKey), Some(Response::Unavailable));
        assert_eq!(serve_failure(DeadlineExceeded), None);
        assert_eq!(serve_failure(HopBudgetExhausted), None);
        assert_eq!(operation_link_outcome(Ok(())), Some(LinkOutcome::Success));
        assert_eq!(
            operation_link_outcome(Err(NotFound)),
            Some(LinkOutcome::Success)
        );
        assert_eq!(
            operation_link_outcome(Err(DeadlineExceeded)),
            Some(LinkOutcome::Timeout)
        );
    }

    #[test]
    fn exchange_evidence_never_blames_downstream() {
        use flow_control::AdaptiveOutcome as A;
        let failed = |fault, socket_failure| Exchange::Failed {
            fault,
            socket_failure,
        };
        assert_eq!(
            exchange_evidence(Exchange::Complete { overloaded: true }, true).0,
            Some(LinkOutcome::Success)
        );
        assert_eq!(
            exchange_evidence(failed(Fault::Io, true), false).0,
            Some(LinkOutcome::Refused)
        );
        assert_eq!(exchange_evidence(failed(Fault::Io, true), true).0, None);
        assert_eq!(exchange_evidence(failed(Fault::Io, false), false).0, None);
        assert_eq!(exchange_evidence(Exchange::Streaming, false).0, None);
        assert!(matches!(
            exchange_evidence(Exchange::Complete { overloaded: true }, false).1,
            A::Neutral
        ));
        assert!(matches!(
            exchange_evidence(Exchange::Complete { overloaded: false }, false).1,
            A::Verified
        ));
        assert!(matches!(
            exchange_evidence(failed(Fault::Overloaded, false), false).1,
            A::LocalPressure
        ));
        assert!(matches!(
            exchange_evidence(Exchange::Streaming, false).1,
            A::Neutral
        ));
    }

    #[test]
    fn silence_evidence_counts_answers_and_unanswered_exchanges_only() {
        assert_eq!(
            exchange_evidence(Exchange::Complete { overloaded: true }, false).2,
            Some(true)
        );
        assert_eq!(exchange_evidence(Exchange::Streaming, false).2, Some(true));
        for fault in [Fault::DeadlineExceeded, Fault::Cancelled] {
            assert_eq!(
                exchange_evidence(
                    Exchange::Failed {
                        fault,
                        socket_failure: false
                    },
                    false
                )
                .2,
                Some(false)
            );
            // A failed socket is link health's evidence.
            assert_eq!(
                exchange_evidence(
                    Exchange::Failed {
                        fault,
                        socket_failure: true
                    },
                    false
                )
                .2,
                None
            );
        }
        for fault in [Fault::Unavailable, Fault::Io, Fault::Overloaded] {
            assert_eq!(
                exchange_evidence(
                    Exchange::Failed {
                        fault,
                        socket_failure: false
                    },
                    false
                )
                .2,
                None
            );
        }
    }

    #[test]
    fn outcomes_and_authority() {
        for (wire, success) in [
            ("page", true),
            ("metadata", true),
            ("bootstrap", true),
            ("selected", true),
            ("not-found", true),
            ("held", true),
            ("warmed", true),
            ("released", true),
            ("unavailable", false),
            ("overloaded", false),
            ("stale-membership", false),
        ] {
            let outcome = Outcome::from_wire(wire);
            assert_eq!(outcome.asserts_success(), success, "{wire}");
        }
        assert_eq!(Outcome::Held.warm_action(), Some("hold"));
        assert_eq!(Outcome::Warmed.warm_action(), Some("fill"));
        assert_eq!(Outcome::Released.warm_action(), Some("release"));
        assert_eq!(Outcome::Page.warm_action(), None);
        assert!(Outcome::Bootstrap.echoes_operation());
        assert!(!Outcome::Selected.echoes_operation());
        assert!(not_found_ok("metadata", "acquire", "fresh"));
        assert!(not_found_ok("bootstrap", "acquire", "fresh"));
        assert!(!not_found_ok("page", "acquire", "fresh"));
        assert!(!not_found_ok("metadata", "copy-only", "fresh"));
        assert!(!not_found_ok("metadata", "acquire", "pinned"));
    }

    #[test]
    fn grant_arithmetic() {
        let visited = [1u32, 2, 3];
        // A grant valid against membership 7, receiver 1, deadline 1000, page
        // budget 4, byte budget 100, and 3 flights, then each field broken.
        assert!(grant_membership_ok(7, 7) && !grant_membership_ok(8, 7));
        assert!(grant_receiver_ok(&visited, &1) && !grant_receiver_ok(&visited, &2));
        assert!(grant_deadline_ok(1000, 1000, 999));
        assert!(!grant_deadline_ok(1000, 1000, 1000), "expired grant");
        assert!(!grant_deadline_ok(1001, 1000, 0));
        assert!(page_budget_ok(3, 4) && !page_budget_ok(4, 4));
        assert!(byte_budget_ok(60, 40, 100) && !byte_budget_ok(61, 40, 100));
        assert!(flights_ok(Some(2), Some(3)));
        assert!(!flights_ok(Some(4), Some(3)) && !flights_ok(None, Some(3)));
        assert!(!page_budget_ok(u64::MAX, 0));
        assert!(!byte_budget_ok(u64::MAX, 1, 0));
        assert!(flights_ok(None, None));
        assert!(!flights_ok(Some(1), None));
        assert!(!grant_receiver_ok::<u32>(&[], &1));
        assert!(bootstrap_presence_ok(0, 0) && bootstrap_presence_ok(9, 1));
        assert!(!bootstrap_presence_ok(0, 1) && !bootstrap_presence_ok(9, 0));
    }
}
