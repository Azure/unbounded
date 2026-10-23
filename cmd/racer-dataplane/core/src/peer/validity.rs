//! Typed response-validity rules.
//!
//! The dataplane parses signed headers and calls these rules in the order its
//! checks short-circuit (`response_matches` and `response_authority` in
//! `src/peer/forwarding.rs`). The simulator calls them on plain values.

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

    /// Outcomes that assert successful data or authoritative absence.
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

/// A relay may report its own failure, but only the requested destination may
/// assert successful data or authoritative absence.
pub fn authority_ok(outcome: Outcome, signer_is_destination: bool) -> bool {
    signer_is_destination || !outcome.asserts_success()
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

/// An owner may shorten a hold to its own TTL cap, never extend the ask.
pub fn hold_ok(granted_until: u64, requested_until: u64) -> bool {
    granted_until <= requested_until
}

/// A bootstrap carries page zero exactly when the object is nonempty.
pub fn bootstrap_presence_ok(length: u64, present: u64) -> bool {
    present == u64::from(length != 0)
}

/// A present bootstrap page is page zero.
pub fn bootstrap_page_ok(present: u64, page: u64) -> bool {
    present != 1 || page == 0
}

/// Selected-grant fields, for callers that hold typed values.
#[derive(Clone, Copy, Debug)]
pub struct Grant<'a, N> {
    pub page_demanded: bool,
    pub membership: u64,
    pub receiver: &'a N,
    pub deadline_ms: u64,
    pub page_budget: u64,
    pub byte_budget: u64,
    pub ciphertext: u64,
    pub flights: Option<u16>,
}

/// Request fields a selected grant is checked against.
#[derive(Clone, Copy, Debug)]
pub struct GrantRequest<'a, N> {
    pub membership: u64,
    pub visited: &'a [N],
    pub deadline_ms: u64,
    pub page_budget: u64,
    pub byte_budget: u64,
    pub flights: Option<u16>,
}

impl<N: PartialEq> Grant<'_, N> {
    /// All selected-grant rules, in production order.
    pub fn valid(&self, request: &GrantRequest<'_, N>, now_ms: u64) -> bool {
        self.page_demanded
            && grant_membership_ok(self.membership, request.membership)
            && grant_receiver_ok(request.visited, self.receiver)
            && grant_deadline_ok(self.deadline_ms, request.deadline_ms, now_ms)
            && page_budget_ok(self.page_budget, request.page_budget)
            && byte_budget_ok(self.byte_budget, self.ciphertext, request.byte_budget)
            && flights_ok(self.flights, request.flights)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

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
            assert!(authority_ok(outcome, true));
            assert_eq!(authority_ok(outcome, false), !success, "{wire}");
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
        let request = GrantRequest {
            membership: 7,
            visited: &visited,
            deadline_ms: 1000,
            page_budget: 4,
            byte_budget: 100,
            flights: Some(3),
        };
        let good = Grant {
            page_demanded: true,
            membership: 7,
            receiver: &1,
            deadline_ms: 1000,
            page_budget: 3,
            byte_budget: 60,
            ciphertext: 40,
            flights: Some(2),
        };
        assert!(good.valid(&request, 999));
        assert!(!good.valid(&request, 1000), "expired grant");
        let receiver = 2;
        for bad in [
            Grant {
                page_demanded: false,
                ..good
            },
            Grant {
                membership: 8,
                ..good
            },
            Grant {
                receiver: &receiver,
                ..good
            },
            Grant {
                deadline_ms: 1001,
                ..good
            },
            Grant {
                page_budget: 4,
                ..good
            },
            Grant {
                byte_budget: 61,
                ..good
            },
            Grant {
                flights: Some(4),
                ..good
            },
            Grant {
                flights: None,
                ..good
            },
        ] {
            assert!(!bad.valid(&request, 0), "{bad:?}");
        }
        assert!(!page_budget_ok(u64::MAX, 0));
        assert!(!byte_budget_ok(u64::MAX, 1, 0));
        assert!(flights_ok(None, None));
        assert!(!flights_ok(Some(1), None));
        assert!(!grant_receiver_ok::<u32>(&[], &1));
        assert!(hold_ok(5, 5) && hold_ok(4, 5) && !hold_ok(6, 5));
        assert!(bootstrap_presence_ok(0, 0) && bootstrap_presence_ok(9, 1));
        assert!(!bootstrap_presence_ok(0, 1) && !bootstrap_presence_ok(9, 0));
        assert!(bootstrap_page_ok(0, 9) && bootstrap_page_ok(1, 0) && !bootstrap_page_ok(1, 1));
    }
}
