//! The single route budget rule shared by the dataplane and `peer-wire`.
//!
//! A route is a path of nodes `origin, relay.., destination`. Each step between
//! two nodes is one link. A route may use at most [`MAX_LINKS`] links.
//!
//! Two encodings of the same budget exist, and they differ by one node:
//!
//! - Wire form (signed `racer-route-visited` and `racer-route-links`): `visited`
//!   includes the node that sent the message, so the origin sends
//!   `visited = [origin]`. Then `visited.len() - 1` links are used and `links`
//!   remain, so the rule is `visited.len() + links <= MAX_LINKS + 1`. The origin
//!   builds this form at `src/read/candidates.rs` (`visited: vec![self.node]`,
//!   `remaining_links` up to `FAILURE_LINKS = 8`), so a sum of 9 is the normal
//!   eight-link request, not an overrun.
//! - Search form (the budget a holder passes to path search): `visited` excludes
//!   the holder. Then `visited.len()` links are used, so the rule is
//!   `visited.len() + links <= MAX_LINKS`. [`search_budget`] converts the wire
//!   form a node received into this form.
//!
//! Both forms enforce the same eight-link limit.

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

/// Wire-form length rule: `visited` includes the sender.
///
/// This is the shape check only. `visited` must be nonempty and `links` must be
/// in `1..=MAX_LINKS`.
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
///
/// The signer must still hold a link to give away (`old_links > 1`), and the
/// next state must carry exactly one fewer link.
pub fn check_wire_transition(old_links: u64, new_links: u64) -> Result<(), RouteFault> {
    if old_links <= 1 || new_links != old_links - 1 {
        return Err(RouteFault::HopBudgetExhausted);
    }
    Ok(())
}

/// Search-form rule: `visited` excludes `holder`.
///
/// Rejects a passed deadline, a route over [`MAX_LINKS`], a holder or
/// destination already visited, and any repeated node.
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

/// Search budget for `local` from a received wire-form route.
///
/// Returns how many leading `visited` entries to keep and the links left. A
/// route whose last entry is `local` is already local (the origin searching its
/// own route), so `local` is dropped and no link is spent. Otherwise one link
/// is reserved for the hop that reached `local`.
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

/// Relay decision for a received wire-form route at `local`: the links on the
/// outbound wire-form route after `local` appends itself.
///
/// The destination never relays, and a relay never appears twice.
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

#[cfg(test)]
mod tests {
    use super::*;
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
}
