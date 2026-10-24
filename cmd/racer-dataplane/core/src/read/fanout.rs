//! Overflow fan-out for hot pages (Known issue 16).
//!
//! Only a page's ranked owners (at most `REPLICAS`) are candidates, so a page
//! that every node reads at once can be sent no faster than `REPLICAS` NICs
//! allow. An owner already streaming [`HOT_PAGE_BODIES`] bodies of a page
//! sheds further readers with a signed `Overloaded` ([`shed_hot_page`]), and
//! a non-owner whose owner sheds it asks one *helper* to acquire the page for
//! it (see
//! [`super::walk::WalkConfig::helper`]). A helper is an ordinary member that
//! is not an owner of the page; it runs its own non-owner walk against the
//! owners, coalesces concurrent requests for the page in its page flight, and
//! keeps the copy, so later requests are served from its cache.
//!
//! Each page has at most [`fanout_width`] helpers, chosen by a hash of the page
//! that every node computes the same way, so readers of a hot page converge on
//! the same few helpers and the owners see at most that many extra requests.
//! A requester picks one of them by a hash of its own ID.
//!
//! Bounds:
//! - Depth: a helper probe is funded with at most [`HELPER_ATTEMPTS`]
//!   attempts, and only walks that start with at least [`HELPER_MIN_ATTEMPTS`]
//!   (> [`HELPER_ATTEMPTS`]) may ask a helper. A helper's inherited budget is
//!   therefore always too small to ask another helper: requests never chain.
//! - Owner probes are planned exactly as without a helper; the helper is paid
//!   from what is left after the owner that shed the request.
//! - Origin: helpers are non-owners, so their walks never reach origin
//!   (`End::Exhausted` instead of `End::Origin`); origin authority is
//!   unchanged.
//! - Copies: at most [`fanout_width`] extra copies of a page are made per
//!   membership version, and only while owners shed load.
//!
//! Security: a helper's answer is an ordinary signed peer `Page` response,
//! validated exactly like an owner's (`validate_payload` checks the envelope
//! against the immutable metadata, and the reader opens the AEAD ciphertext
//! itself). A helper never holds credentials it would not hold as a reader:
//! it acquires with the requester's signed origin context, as any owner does.

use racer_control_wire::NodeId;
use racer_object_wire::model::{ObjectId, PageNumber};

/// Most attempts a requester funds a helper probe with: the exchange plus
/// the helper's own walk of the owners.
pub const HELPER_ATTEMPTS: u32 = 4;

/// Most links a helper probe is funded with (a full failure-routing envelope).
pub const HELPER_LINKS: u8 = 8;

/// Fewest attempts left for a helper probe: the exchange, and at the helper
/// one exchange with rank 0 plus the credit rank 0 needs for origin.
pub const HELPER_FLOOR_ATTEMPTS: u32 = 3;

/// Fewest links left for a helper probe: a route to the helper and a route
/// from it to an owner.
pub const HELPER_FLOOR_LINKS: u8 = 4;

/// A walk may ask a helper only if it starts with at least this many
/// attempts. Larger than [`HELPER_ATTEMPTS`], so a helper, whose inherited
/// budget is smaller than its funding, never asks another helper.
pub const HELPER_MIN_ATTEMPTS: u32 = HELPER_ATTEMPTS + 1;

/// Most helpers one page has.
pub const MAX_HELPERS: usize = 256;

const _: () = assert!(HELPER_MIN_ATTEMPTS > HELPER_ATTEMPTS);
const _: () = assert!(HELPER_FLOOR_ATTEMPTS <= HELPER_ATTEMPTS);

/// Helpers per page for a membership of `members`: the integer square root,
/// at most [`MAX_HELPERS`]. With `n` readers of a page, owners then send about
/// `sqrt(n)` copies and each helper about `sqrt(n)`, instead of the owners
/// sending all `n`.
pub fn fanout_width(members: usize) -> usize {
    members.isqrt().min(MAX_HELPERS)
}

/// FNV-1a: stable across processes and builds, unlike `DefaultHasher`, so
/// every node picks the same helpers for a page.
fn fnv(parts: &[&[u8]]) -> u64 {
    let mut hash: u64 = 0xcbf2_9ce4_8422_2325;
    for part in parts {
        for byte in (part.len() as u64).to_le_bytes().iter().chain(part.iter()) {
            hash ^= u64::from(*byte);
            hash = hash.wrapping_mul(0x0000_0100_0000_01b3);
        }
    }
    // Finalize (splitmix64) so the low bits used for `%` are well mixed.
    let mut z = hash.wrapping_add(0x9e37_79b9_7f4a_7c15);
    z = (z ^ (z >> 30)).wrapping_mul(0xbf58_476d_1ce4_e5b9);
    z = (z ^ (z >> 27)).wrapping_mul(0x94d0_49bb_1331_11eb);
    z ^ (z >> 31)
}

/// Concurrent bodies of one page an owner writes before it may shed further
/// readers of that page with `Overloaded` (see [`shed_hot_page`]). Chosen by
/// measurement ("Round 4: fanout" in `designs/racer-core-simulation.md`):
/// at 4 to 16 owners shed Zipf-popular pages they would have served within
/// the deadline, and the relayed helper hop failed more reads than it saved;
/// at 32 the Zipf scenario is unchanged within seed noise while storms keep
/// most of the gain.
pub const HOT_PAGE_BODIES: usize = 32;

/// Whether an owner sheds a request for a page it is already writing to at
/// least [`HOT_PAGE_BODIES`] requesters, when that page is at least half of
/// the `all` bodies the owner is writing (the page is the owner's queue; when
/// many pages share a busy NIC, moving their readers to helpers only adds a
/// relayed hop for each of them). The shed requester asks its helper (the
/// walk's overflow step), which the owner always serves, so the owner's NIC
/// feeds at most [`fanout_width`] helpers plus a few readers per page instead
/// of a queue of readers that time out. Owners and helpers of the page are
/// never shed (`exempt`), and non-owners never shed: a helper queues the
/// readers it was chosen for.
pub fn shed_hot_page(owner: bool, streaming: usize, all: usize, exempt: bool) -> bool {
    owner && !exempt && streaming >= HOT_PAGE_BODIES && streaming * 2 >= all
}

/// Whether the member at `position` is one of `page`'s helpers (any reader
/// may pick it; see [`fanout_helper`]). At most [`fanout_width`] hashes.
pub fn is_helper(object: &ObjectId, page: PageNumber, position: usize, members: usize) -> bool {
    let cache = object.cache.0.as_bytes();
    let key = &object.key.0[..];
    let number = page.0.to_le_bytes();
    (0..fanout_width(members) as u64)
        .any(|j| fnv(&[cache, key, &number, &j.to_le_bytes()]) as usize % members == position)
}

/// Position (in membership order) of the member `requester` asks to acquire
/// `page` for it, or `None` when the membership has no eligible helper.
///
/// `excluded(position)` must be true for the page's owners and for the
/// requester itself. Helper `j` of a page is `hash(page, j) % members`; the
/// requester starts at `hash(requester, page) % width` and takes the first
/// helper that is not excluded and is `preferred` (a direct overlay neighbor,
/// so the exchange needs no relay), else the first that is not excluded.
/// Scans at most [`fanout_width`] helpers; callers compute it only when the
/// walk asks for the helper.
pub fn fanout_helper(
    object: &ObjectId,
    page: PageNumber,
    requester: &NodeId,
    members: usize,
    excluded: impl Fn(usize) -> bool,
    preferred: impl Fn(usize) -> bool,
) -> Option<usize> {
    let width = fanout_width(members);
    if width == 0 {
        return None;
    }
    let cache = object.cache.0.as_bytes();
    let key = &object.key.0[..];
    let number = page.0.to_le_bytes();
    let start = fnv(&[requester.0.as_bytes(), cache, key, &number]) as usize % width;
    let mut first = None;
    for offset in 0..width {
        let j = ((start + offset) % width) as u64;
        let position = fnv(&[cache, key, &number, &j.to_le_bytes()]) as usize % members;
        if excluded(position) {
            continue;
        }
        if preferred(position) {
            return Some(position);
        }
        first.get_or_insert(position);
    }
    first
}

#[cfg(test)]
mod tests {
    use super::*;
    use racer_control_wire::CacheId;
    use racer_object_wire::model::CacheKey;

    fn object() -> ObjectId {
        ObjectId {
            cache: CacheId("cache".into()),
            key: CacheKey([7; 32]),
        }
    }

    #[test]
    fn width_is_square_root_and_bounded() {
        assert_eq!(fanout_width(0), 0);
        assert_eq!(fanout_width(3), 1);
        assert_eq!(fanout_width(5_000), 70);
        assert_eq!(fanout_width(50_000), 223);
        assert_eq!(fanout_width(1_000_000), MAX_HELPERS);
    }

    #[test]
    fn readers_converge_on_at_most_width_helpers_and_never_excluded_ones() {
        let members = 5_000;
        let owners = [7usize, 11, 13];
        let mut chosen = std::collections::BTreeSet::new();
        for r in 0..members {
            let requester = NodeId(format!("node-{r}"));
            let helper = fanout_helper(
                &object(),
                PageNumber(3),
                &requester,
                members,
                |p| owners.contains(&p) || p == r,
                |_| false,
            )
            .expect("helper");
            assert!(!owners.contains(&helper) && helper != r);
            chosen.insert(helper);
        }
        // Each reader may skip at most its own position, so the page's helper
        // set is the width plus at most a handful of substitutes.
        assert!(chosen.len() <= fanout_width(members) + owners.len() + 1);
        assert!(chosen.len() >= fanout_width(members) / 2);
        // Deterministic across calls (and processes: FNV, not DefaultHasher).
        let a = fanout_helper(
            &object(),
            PageNumber(3),
            &NodeId("x".into()),
            members,
            |_| false,
            |_| false,
        );
        let b = fanout_helper(
            &object(),
            PageNumber(3),
            &NodeId("x".into()),
            members,
            |_| false,
            |_| false,
        );
        assert_eq!(a, b);
    }

    #[test]
    fn every_chosen_helper_is_recognized_and_few_others_are() {
        let members = 5_000;
        let mut helpers = 0;
        for r in 0..members {
            let requester = NodeId(format!("node-{r}"));
            let h = fanout_helper(
                &object(),
                PageNumber(9),
                &requester,
                members,
                |p| p == r,
                |p| p % 7 == r % 7,
            )
            .expect("helper");
            assert!(is_helper(&object(), PageNumber(9), h, members));
        }
        for p in 0..members {
            helpers += usize::from(is_helper(&object(), PageNumber(9), p, members));
        }
        assert!(helpers <= fanout_width(members), "{helpers}");
    }

    #[test]
    fn owners_shed_only_non_exempt_requesters_of_a_dominant_hot_page() {
        let n = HOT_PAGE_BODIES;
        assert!(!shed_hot_page(true, n - 1, n - 1, false));
        assert!(shed_hot_page(true, n, n, false));
        assert!(shed_hot_page(true, n, 2 * n, false));
        // A page that is not most of the owner's queue is not shed.
        assert!(!shed_hot_page(true, n, 2 * n + 1, false));
        assert!(!shed_hot_page(true, n, n, true));
        assert!(!shed_hot_page(false, n, n, false));
    }

    #[test]
    fn a_neighbor_helper_is_preferred_but_never_required() {
        let members = 5_000;
        let r = NodeId("r".into());
        let all: Vec<usize> = (0..members)
            .filter(|p| is_helper(&object(), PageNumber(4), *p, members))
            .collect();
        let pick = |neighbor: usize| {
            fanout_helper(
                &object(),
                PageNumber(4),
                &r,
                members,
                |_| false,
                |p| p == neighbor,
            )
        };
        // Each helper of the page is chosen when it is the only neighbor.
        for neighbor in &all {
            assert_eq!(pick(*neighbor), Some(*neighbor));
        }
        // A neighbor outside the helper set is never chosen.
        let outside = (0..members).find(|p| !all.contains(p)).unwrap();
        let chosen = pick(outside).unwrap();
        assert!(all.contains(&chosen));
        // An excluded neighbor is never chosen.
        let excluded = all[0];
        let chosen = fanout_helper(
            &object(),
            PageNumber(4),
            &r,
            members,
            |p| p == excluded,
            |p| p == excluded,
        )
        .unwrap();
        assert_ne!(chosen, excluded);
    }

    #[test]
    fn no_helper_when_everyone_is_excluded_or_membership_is_empty() {
        let r = NodeId("r".into());
        assert_eq!(
            fanout_helper(&object(), PageNumber(0), &r, 0, |_| false, |_| false),
            None
        );
        assert_eq!(
            fanout_helper(&object(), PageNumber(0), &r, 4, |_| true, |_| true),
            None
        );
    }

    #[test]
    fn different_pages_use_different_helpers() {
        let r = NodeId("r".into());
        let helpers: std::collections::BTreeSet<_> = (0..64)
            .filter_map(|p| {
                fanout_helper(&object(), PageNumber(p), &r, 5_000, |_| false, |_| false)
            })
            .collect();
        assert!(helpers.len() > 32);
    }
}
