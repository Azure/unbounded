//! Wire-form route budget checks shared by `peer-wire` and `racer_core::route`.
//!
//! A route `origin, relay.., destination` uses at most [`MAX_LINKS`] links. Wire form (signed
//! headers) counts the sender in `visited`: `visited.len() + links <= MAX_LINKS + 1`.
//! Search-form rules stay in `racer_core::route`.

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
