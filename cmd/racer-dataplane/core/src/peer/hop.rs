//! Per-hop decisions and failure tables.
//!
//! Production callers: `Server::dispatch` and `dispatch_verified`
//! (`src/peer/server.rs`), `Relay::forward_inner` and `Requester::exchange`
//! (`src/peer.rs`), and `LinkHealth::run` (`src/topology.rs`).

use super::Fault;
use crate::route;
use crate::route::RouteFault;

/// The fields of a received route that a hop decision reads.
#[derive(Clone, Copy, Debug)]
pub struct HopRoute<'a, N> {
    /// Wire form: includes the sender.
    pub visited: &'a [N],
    pub destination: &'a N,
    pub links: u8,
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

/// Budget arithmetic for a relay, kept as results so the caller can apply
/// them in production order: reject [`RouteFault::Invalid`] first, then take
/// relay admission, then use `search`, find the next node, and use `outbound`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct RelayPlan {
    /// Number of `visited` entries kept and links left for path search.
    pub search: Result<(usize, u8), RouteFault>,
    /// Links on the outbound route after this node appends itself.
    pub outbound: Result<u8, RouteFault>,
}

impl RelayPlan {
    /// The route itself is malformed for a relay at this node.
    pub fn invalid(&self) -> bool {
        self.outbound == Err(RouteFault::Invalid)
    }
}

/// Decide the hop for a verified request at `local`.
///
/// `membership_known` is false when the signed membership version cannot be
/// resolved (`Error::IncompatibleMembership` from `PeerNetwork::membership`).
pub fn decide_hop<N: PartialEq>(local: &N, route: HopRoute<'_, N>, membership_known: bool) -> Hop {
    if !membership_known {
        return Hop::StaleMembership;
    }
    if route.destination == local {
        return Hop::Serve;
    }
    Hop::Relay(RelayPlan {
        search: route::search_budget(route.visited, route.links, local),
        outbound: route::relay(route.visited, route.destination, route.links, local),
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

/// Relay failure table (`dispatch_verified`, relay branch).
pub fn relay_failure(fault: Fault) -> RelayFailure {
    match fault {
        Fault::Overloaded => RelayFailure::Respond(Response::Overloaded),
        Fault::Unavailable
        | Fault::Io
        | Fault::Os
        | Fault::HopBudgetExhausted
        | Fault::IncompatibleMembership => RelayFailure::RespondIfLive(Response::Unavailable),
        _ => RelayFailure::Propagate,
    }
}

/// Relay failure table for one forwarded exchange; `ping` marks a silence
/// ping (`racer-mode: ping`). The pinging node reads a relay's signed
/// `Unavailable` as "the destination is unreachable" and holds it silent
/// (Known issues 41 and 43), so a ping's relay signs it only for a refusal
/// of its own (`Unavailable`: it holds the destination silent, or has no
/// route) and never for a transport failure (`Io`, `Os`). A destination
/// that predates the ping mode rejects the request and closes the
/// connection, which the relay sees as `Io`: that destination is alive.
/// The error then propagates unsigned, which the pinging node counts as
/// inconclusive. Other modes use [`relay_failure`].
pub fn relay_failure_for(fault: Fault, ping: bool) -> RelayFailure {
    if ping && matches!(fault, Fault::Io | Fault::Os) {
        return RelayFailure::Propagate;
    }
    relay_failure(fault)
}

/// Local service failure table (`dispatch_verified`, destination branch).
/// `None` propagates the error unsigned.
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

/// Link evidence from one operation over a link (`LinkHealth::run`).
/// Application misses and credential rejection do not open transport circuits.
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

/// Link evidence for an exchange (`Requester::exchange`). Without adaptive
/// admission, only a socket failure blames the immediate peer; downstream
/// errors are not evidence against it.
pub fn exchange_link_outcome(exchange: Exchange, admitted: bool) -> Option<LinkOutcome> {
    match exchange {
        Exchange::Complete { .. } => Some(LinkOutcome::Success),
        Exchange::Failed {
            socket_failure: true,
            ..
        } if !admitted => Some(LinkOutcome::Refused),
        _ => None,
    }
}

/// Silence evidence (`super::silence`) from an exchange over the last link
/// to its destination: `Some(true)` when the destination answered (a
/// response or a body), `Some(false)` when the exchange ended unanswered by
/// a time bound or the upstream's cancellation without a socket failure,
/// and `None` otherwise (a failed connection is link health's evidence).
pub fn silence_evidence(exchange: Exchange) -> Option<bool> {
    match exchange {
        Exchange::Complete { .. } | Exchange::Streaming => Some(true),
        Exchange::Failed {
            fault: Fault::DeadlineExceeded | Fault::Cancelled,
            socket_failure: false,
        } => Some(false),
        Exchange::Failed { .. } => None,
    }
}

/// Adaptive admission evidence for an exchange (`Requester::exchange`).
/// A downstream overload is not attributable to the immediate peer.
pub fn exchange_admission_outcome(exchange: Exchange) -> flow_control::AdaptiveOutcome {
    use flow_control::AdaptiveOutcome as Outcome;
    match exchange {
        Exchange::Complete { overloaded: true } => Outcome::Neutral,
        Exchange::Complete { overloaded: false } => Outcome::Verified,
        Exchange::Failed {
            fault: Fault::Overloaded,
            ..
        } => Outcome::LocalPressure,
        _ => Outcome::Neutral,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn route(visited: &[u32], destination: &u32, links: u8) -> Hop {
        decide_hop(
            &5,
            HopRoute {
                visited,
                destination,
                links,
            },
            true,
        )
    }

    #[test]
    fn decide_hop_serves_relays_or_reports_stale() {
        let r = HopRoute {
            visited: &[1u32],
            destination: &5,
            links: 4,
        };
        assert_eq!(decide_hop(&5, r, false), Hop::StaleMembership);
        assert_eq!(decide_hop(&5, r, true), Hop::Serve);
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
        assert!(!plan.invalid());
        assert_eq!(plan.search, Err(RouteFault::HopBudgetExhausted));
        assert_eq!(plan.outbound, Err(RouteFault::HopBudgetExhausted));
        // A cycle through this node is invalid before any budget is spent.
        let Hop::Relay(plan) = route(&[1, 5], &9, 4) else {
            panic!()
        };
        assert!(plan.invalid());
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
            let relay = relay_failure(fault);
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
            assert_eq!(relay_failure_for(fault, false), expected, "{fault:?}");
            let ping = if matches!(fault, Io | Os) {
                RelayFailure::Propagate
            } else {
                expected
            };
            assert_eq!(relay_failure_for(fault, true), ping, "{fault:?}");
        }
        // A pre-ping destination closes on the unknown mode (`Io` at the
        // relay): unsigned, not an `Unavailable` that silences it. A relay
        // that holds the destination silent still signs `Unavailable`.
        assert_eq!(relay_failure_for(Io, true), RelayFailure::Propagate);
        assert_eq!(
            relay_failure_for(Unavailable, true),
            RelayFailure::RespondIfLive(Response::Unavailable)
        );
        assert_eq!(
            relay_failure_for(Overloaded, true),
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
            exchange_link_outcome(Exchange::Complete { overloaded: true }, true),
            Some(LinkOutcome::Success)
        );
        assert_eq!(
            exchange_link_outcome(failed(Fault::Io, true), false),
            Some(LinkOutcome::Refused)
        );
        assert_eq!(exchange_link_outcome(failed(Fault::Io, true), true), None);
        assert_eq!(exchange_link_outcome(failed(Fault::Io, false), false), None);
        assert_eq!(exchange_link_outcome(Exchange::Streaming, false), None);
        assert!(matches!(
            exchange_admission_outcome(Exchange::Complete { overloaded: true }),
            A::Neutral
        ));
        assert!(matches!(
            exchange_admission_outcome(Exchange::Complete { overloaded: false }),
            A::Verified
        ));
        assert!(matches!(
            exchange_admission_outcome(failed(Fault::Overloaded, false)),
            A::LocalPressure
        ));
        assert!(matches!(
            exchange_admission_outcome(Exchange::Streaming),
            A::Neutral
        ));
    }

    #[test]
    fn silence_evidence_counts_answers_and_unanswered_exchanges_only() {
        assert_eq!(
            silence_evidence(Exchange::Complete { overloaded: true }),
            Some(true)
        );
        assert_eq!(silence_evidence(Exchange::Streaming), Some(true));
        for fault in [Fault::DeadlineExceeded, Fault::Cancelled] {
            assert_eq!(
                silence_evidence(Exchange::Failed {
                    fault,
                    socket_failure: false
                }),
                Some(false)
            );
            // A failed socket is link health's evidence.
            assert_eq!(
                silence_evidence(Exchange::Failed {
                    fault,
                    socket_failure: true
                }),
                None
            );
        }
        for fault in [Fault::Unavailable, Fault::Io, Fault::Overloaded] {
            assert_eq!(
                silence_evidence(Exchange::Failed {
                    fault,
                    socket_failure: false
                }),
                None
            );
        }
    }
}
