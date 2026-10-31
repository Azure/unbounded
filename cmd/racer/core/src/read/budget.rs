//! Acquisition credits: deadline, attempts, and route links.

use crate::read::{Error, Result};
use racer_control_wire::NodeId;
use racer_object_wire::model::{ObjectId, PageNumber};
use std::time::Instant;

/// Original acquisition budget, owned by one metadata/bootstrap operation or page acquisition.
/// Retry never rebuilds one from defaults; fanout partitions credits, and route attempts charge
/// actual forwarded links. No Clone: a replacement caller uses its own remaining credits.
/// ```compile_fail
/// use racer_core::read::AcquisitionBudget;
/// fn duplicate(budget: AcquisitionBudget) { let _other = budget.clone(); }
/// ```
pub struct AcquisitionBudget {
    deadline: Instant,
    attempts: u32,
    links: u8,
    charged_links: u16,
    failed_route: bool,
    /// Attempts sent to destinations as remote credits
    /// ([`Self::send_remote`]) and not refunded yet.
    remote_sent: u32,
}
impl AcquisitionBudget {
    pub fn new(deadline: Instant, attempts: u32, links: u8) -> Self {
        Self {
            deadline,
            attempts,
            links,
            charged_links: 0,
            failed_route: false,
            remote_sent: 0,
        }
    }

    /// Debit `attempts` that one request carries to its destination as
    /// remote credits. Returns them for the request.
    pub fn send_remote(&mut self, attempts: u32) -> Result<u32> {
        self.attempts = self
            .attempts
            .checked_sub(attempts)
            .ok_or(Error::Unavailable)?;
        self.remote_sent = self.remote_sent.saturating_add(attempts);
        Ok(attempts)
    }

    /// Return remote credits that a verified `StaleMembership` answer shows were never used: the
    /// answering node could not resolve the request's membership, so neither it nor the destination
    /// behind it acted on the request. Never more than was sent, so a refund cannot mint credits;
    /// lost or unverified answers are not refunded.
    pub fn refund_remote(&mut self, attempts: u32) -> Result<()> {
        let sent = self
            .remote_sent
            .checked_sub(attempts)
            .ok_or(Error::InvalidRequest)?;
        let available = self
            .attempts
            .checked_add(attempts)
            .ok_or(Error::InvalidRequest)?;
        self.remote_sent = sent;
        self.attempts = available;
        Ok(())
    }

    /// Pure budget validation/debit. The driver also checks original cancellation
    /// and candidate authority before I/O. Return the tighter original deadline.
    pub fn begin_attempt(&mut self, now: Instant, caller_deadline: Instant) -> Result<Instant> {
        let deadline = self.deadline.min(caller_deadline);
        if now >= deadline {
            return Err(Error::DeadlineExceeded);
        }
        self.attempts = self.attempts.checked_sub(1).ok_or(Error::Unavailable)?;
        Ok(deadline)
    }

    pub fn charge_links(&mut self, links: u8) -> Result<()> {
        self.links = self
            .links
            .checked_sub(links)
            .ok_or(Error::HopBudgetExhausted)?;
        self.charged_links = self.charged_links.saturating_add(u16::from(links));
        Ok(())
    }

    /// Reconcile unused links only after a verified completed route. Refunds
    /// cannot exceed prior debits; failed exchanges must retain their full debit.
    pub fn refund_links(&mut self, links: u8) -> Result<()> {
        let charged = self
            .charged_links
            .checked_sub(u16::from(links))
            .ok_or(Error::InvalidRequest)?;
        let available = self.links.checked_add(links).ok_or(Error::InvalidRequest)?;
        self.charged_links = charged;
        self.links = available;
        Ok(())
    }

    pub fn deadline(&self) -> Instant {
        self.deadline
    }
    pub fn remaining_attempts(&self) -> u32 {
        self.attempts
    }
    pub fn remaining_links(&self) -> u8 {
        self.links
    }

    /// Both normal and failure ceilings draw from credits allocated by ingress.
    /// Observing failure changes the per-attempt ceiling, never adds credits.
    pub fn route_links(&self) -> u8 {
        self.links.min(if self.failed_route { 8 } else { 4 })
    }
    pub fn note_route_failure(&mut self) {
        self.failed_route = true;
    }

    /// Consume an owned child's unused credits exactly once. Do not reconstruct a
    /// default budget or refund uncertain work after a lost remote response.
    pub fn reunite(&mut self, child: Self) -> Result<()> {
        let attempts = self
            .attempts
            .checked_add(child.attempts)
            .ok_or(Error::InvalidRequest)?;
        let links = self
            .links
            .checked_add(child.links)
            .ok_or(Error::InvalidRequest)?;
        self.deadline = self.deadline.min(child.deadline);
        self.attempts = attempts;
        self.links = links;
        self.failed_route |= child.failed_route;
        Ok(())
    }

    /// Move credits into an independently owned worker/range child. Failure is
    /// atomic; neither fanout nor a worker transfer can mint original credits.
    pub fn partition(&mut self, attempts: u32, links: u8) -> Result<Self> {
        let remaining_attempts = self
            .attempts
            .checked_sub(attempts)
            .ok_or(Error::Unavailable)?;
        let remaining_links = self
            .links
            .checked_sub(links)
            .ok_or(Error::HopBudgetExhausted)?;
        self.attempts = remaining_attempts;
        self.links = remaining_links;
        let mut child = Self::new(self.deadline, attempts, links);
        child.failed_route = self.failed_route;
        Ok(child)
    }

    pub fn transfer(&mut self) -> Self {
        let attempts = std::mem::take(&mut self.attempts);
        let links = std::mem::take(&mut self.links);
        Self {
            deadline: self.deadline,
            attempts,
            links,
            charged_links: std::mem::take(&mut self.charged_links),
            failed_route: self.failed_route,
            remote_sent: std::mem::take(&mut self.remote_sent),
        }
    }

    /// Atomically debit a peer route's attempt and all forwarded links before I/O.
    pub fn begin_peer_attempt(
        &mut self,
        now: Instant,
        caller_deadline: Instant,
        links: u8,
    ) -> Result<Instant> {
        if links == 0 {
            return Err(Error::InvalidRequest);
        }
        let deadline = self.deadline.min(caller_deadline);
        if now >= deadline {
            return Err(Error::DeadlineExceeded);
        }
        if self.attempts == 0 {
            return Err(Error::Unavailable);
        }
        let remaining = self
            .links
            .checked_sub(links)
            .ok_or(Error::HopBudgetExhausted)?;
        self.attempts -= 1;
        self.links = remaining;
        self.charged_links = self.charged_links.saturating_add(u16::from(links));
        Ok(deadline)
    }
}

/// Links a verified `StaleMembership` answer shows were never used: the answering node could not
/// resolve the request's membership, so it neither served the request nor forwarded it, and every
/// link past the `hops` the request travelled to it (`forwarders + 1` on the verified answer) was
/// left unspent ([`AcquisitionBudget::refund_links`]), never more than was charged.
pub fn stale_unused_links(charged: u8, hops: usize) -> u8 {
    charged.saturating_sub(u8::try_from(hops).unwrap_or(u8::MAX))
}

/// Maximum links any route budget may carry (see "Route budget" in
/// `designs/racer-core-simulation.md`).
pub(crate) const MAX_ROUTE_LINKS: u8 = 8;

/// Links per attempt before any route failure has been observed.
pub const NORMAL_ROUTE_LINKS: u8 = 4;

/// Inherit a forwarded peer's remaining route allowance without extending its deadline.
/// `scope_deadline` is the local request deadline; the caller checks cancellation.
pub fn inherited_budget(
    scope_deadline: Instant,
    route_deadline: Instant,
    remaining_attempts: u32,
    remaining_links: u8,
    visited: usize,
) -> Result<AcquisitionBudget> {
    if remaining_links > MAX_ROUTE_LINKS || remaining_links == 0 || visited == 0 {
        return Err(Error::HopBudgetExhausted);
    }
    let mut budget = AcquisitionBudget::new(
        scope_deadline.min(route_deadline),
        remaining_attempts,
        remaining_links
            .checked_sub(1)
            .ok_or(Error::HopBudgetExhausted)?,
    );
    if remaining_links > NORMAL_ROUTE_LINKS {
        budget.note_route_failure();
    }
    Ok(budget)
}

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod budget_tests {
    use super::*;
    use std::time::Duration;

    #[test]
    fn stale_answers_refund_only_the_links_past_the_answerer() {
        assert_eq!(stale_unused_links(8, 1), 7);
        assert_eq!(stale_unused_links(4, 3), 1);
        assert_eq!(stale_unused_links(4, 4), 0);
        assert_eq!(stale_unused_links(4, 9), 0);
        assert_eq!(stale_unused_links(4, 1_000), 0);
        let mut budget = AcquisitionBudget::new(Instant::now() + Duration::from_secs(1), 2, 16);
        budget
            .begin_peer_attempt(Instant::now(), budget.deadline(), 8)
            .unwrap();
        budget.refund_links(stale_unused_links(8, 2)).unwrap();
        assert_eq!(budget.remaining_links(), 14);
        // The links that carried the request stay spent: no second refund.
        assert!(budget.refund_links(3).is_err());
    }

    #[test]
    fn remote_credits_refund_only_what_was_sent() {
        let now = Instant::now();
        let mut budget = AcquisitionBudget::new(now + Duration::from_secs(5), 8, 16);
        assert_eq!(budget.send_remote(3), Ok(3));
        assert_eq!(budget.remaining_attempts(), 5);
        assert_eq!(budget.refund_remote(4), Err(Error::InvalidRequest));
        assert_eq!(budget.remaining_attempts(), 5);
        assert_eq!(budget.refund_remote(3), Ok(()));
        assert_eq!(budget.remaining_attempts(), 8);
        // Refunded once: never again.
        assert_eq!(budget.refund_remote(1), Err(Error::InvalidRequest));
        assert_eq!(budget.send_remote(9), Err(Error::Unavailable));
        assert_eq!(budget.remaining_attempts(), 8);
        // A partitioned child refunds into itself, then reunites.
        let mut child = budget.partition(4, 4).unwrap();
        assert_eq!(child.send_remote(3), Ok(3));
        child.refund_remote(2).unwrap();
        budget.reunite(child).unwrap();
        assert_eq!(budget.remaining_attempts(), 7);
        assert_eq!(budget.refund_remote(1), Err(Error::InvalidRequest));
    }

    #[test]
    fn begin_attempt_debits_and_respects_tighter_deadline() {
        let now = Instant::now();
        let mut budget = AcquisitionBudget::new(now + Duration::from_secs(5), 1, 8);
        let deadline: Instant = budget
            .begin_attempt(now, now + Duration::from_secs(1))
            .unwrap();
        assert_eq!(deadline, now + Duration::from_secs(1));
        assert_eq!(budget.remaining_attempts(), 0);
        assert_eq!(
            budget.begin_attempt(now, now + Duration::from_secs(1)),
            Err(Error::Unavailable)
        );
        assert_eq!(
            budget.begin_attempt(now + Duration::from_secs(5), now + Duration::from_secs(9)),
            Err(Error::DeadlineExceeded)
        );
    }

    #[test]
    fn links_charge_refund_and_never_mint() {
        let now = Instant::now();
        let mut budget = AcquisitionBudget::new(now, 3, 8);
        budget.charge_links(3).unwrap();
        assert_eq!(budget.remaining_links(), 5);
        assert_eq!(budget.refund_links(4), Err(Error::InvalidRequest));
        budget.refund_links(3).unwrap();
        assert_eq!(budget.remaining_links(), 8);
        assert_eq!(budget.charge_links(9), Err(Error::HopBudgetExhausted));
        assert_eq!(budget.route_links(), 4);
        budget.note_route_failure();
        assert_eq!(budget.route_links(), 8);
    }

    #[test]
    fn partition_and_reunite_conserve_credits() {
        let now = Instant::now();
        let mut parent = AcquisitionBudget::new(now, 4, 8);
        let child = parent.partition(3, 5).unwrap();
        assert_eq!(
            (parent.remaining_attempts(), parent.remaining_links()),
            (1, 3)
        );
        assert!(matches!(parent.partition(2, 0), Err(Error::Unavailable)));
        assert!(matches!(
            parent.partition(0, 4),
            Err(Error::HopBudgetExhausted)
        ));
        parent.reunite(child).unwrap();
        assert_eq!(
            (parent.remaining_attempts(), parent.remaining_links()),
            (4, 8)
        );
        let moved = parent.transfer();
        assert_eq!(
            (parent.remaining_attempts(), parent.remaining_links()),
            (0, 0)
        );
        assert_eq!(
            (moved.remaining_attempts(), moved.remaining_links()),
            (4, 8)
        );
    }

    #[test]
    fn peer_attempt_is_atomic() {
        let now = Instant::now();
        let later = now + Duration::from_secs(1);
        let mut budget = AcquisitionBudget::new(later, 1, 2);
        assert_eq!(
            budget.begin_peer_attempt(now, later, 0),
            Err(Error::InvalidRequest)
        );
        assert_eq!(
            budget.begin_peer_attempt(now, later, 3),
            Err(Error::HopBudgetExhausted)
        );
        assert_eq!(
            (budget.remaining_attempts(), budget.remaining_links()),
            (1, 2)
        );
        assert_eq!(budget.begin_peer_attempt(now, later, 2), Ok(later));
        assert_eq!(
            budget.begin_peer_attempt(now, later, 1),
            Err(Error::Unavailable)
        );
    }

    #[test]
    fn inherited_budget_bounds() {
        let now = Instant::now();
        let soon = now + Duration::from_secs(1);
        let later = now + Duration::from_secs(9);
        for (links, visited, ok) in [
            (0, 1, false),
            (9, 1, false),
            (3, 0, false),
            (1, 1, true),
            (8, 1, true),
        ] {
            assert_eq!(
                inherited_budget(later, soon, 2, links, visited).is_ok(),
                ok,
                "{links} {visited}"
            );
        }
        let budget = inherited_budget(later, soon, 2, 5, 1).unwrap();
        assert_eq!(budget.deadline(), soon);
        assert_eq!(budget.remaining_links(), 4);
        assert_eq!(budget.route_links(), 4);
        let budget = inherited_budget(soon, later, 2, 4, 1).unwrap();
        assert_eq!(budget.deadline(), soon);
        assert_eq!(budget.route_links(), 3);
    }
}

// Overflow fan-out for hot pages. An owner streaming [`hot_page_bodies`] bodies sheds further
// readers ([`shed_hot_page`]); a shed non-owner asks one of the page's [`fanout_width`] hash-chosen
// helpers ([`crate::read::walk::WalkConfig::helper`]). A helper probe carries at most
// [`HELPER_ATTEMPTS`] and only walks starting with [`HELPER_MIN_ATTEMPTS`] may ask one, so requests
// never chain. Helpers are non-owners, so they never reach origin.

/// Most attempts a requester funds a helper probe with: the exchange plus
/// the helper's own walk of the owners.
pub const HELPER_ATTEMPTS: u32 = 4;

/// Most links a helper probe is funded with (a full failure-routing envelope).
pub const HELPER_LINKS: u8 = 8;

/// Fewest attempts left for a helper probe: the exchange, and at the helper
/// one exchange with rank 0 plus the credit rank 0 needs for origin.
pub const HELPER_FLOOR_ATTEMPTS: u32 = 3;

/// Fewest links left for a helper probe: a route to the helper and a route from it to an owner.
pub const HELPER_FLOOR_LINKS: u8 = 4;

/// A walk may ask a helper only if it starts with at least this many attempts. Larger than
/// [`HELPER_ATTEMPTS`], so a helper never asks another helper.
pub const HELPER_MIN_ATTEMPTS: u32 = HELPER_ATTEMPTS + 1;

/// Most helpers one page has.
pub const MAX_HELPERS: usize = 256;

const _: () = assert!(HELPER_MIN_ATTEMPTS > HELPER_ATTEMPTS);
const _: () = assert!(HELPER_FLOOR_ATTEMPTS <= HELPER_ATTEMPTS);

/// Helpers per page for a membership of `members`: the integer square root, at most
/// [`MAX_HELPERS`]. With `n` readers, owners and each helper then send about `sqrt(n)` copies.
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

/// Queued egress of one page, in bytes, an owner writes before it may shed further readers of that
/// page with `Overloaded` (see [`shed_hot_page`] and [`hot_page_bodies`]). In bytes so the rule
/// means the same backlog (about 27 ms at 10 Gbit/s) for any page size.
pub const HOT_PAGE_QUEUE_BYTES: u64 = 32 << 20;

/// Fewest concurrent bodies of one page before an owner may shed, whatever the page size: below
/// this a shed reader's extra helper hop costs more than waiting behind the other bodies.
pub const HOT_PAGE_MIN_BODIES: usize = 2;

/// Concurrent bodies of one `page_bytes` page an owner writes before it may shed:
/// [`HOT_PAGE_QUEUE_BYTES`] of that page, at least [`HOT_PAGE_MIN_BODIES`]. 32 for 1 MiB pages, 2
/// for 16 MiB.
pub const fn hot_page_bodies(page_bytes: u64) -> usize {
    let bodies = HOT_PAGE_QUEUE_BYTES / if page_bytes == 0 { 1 } else { page_bytes };
    if bodies < HOT_PAGE_MIN_BODIES as u64 {
        HOT_PAGE_MIN_BODIES
    } else if bodies > u32::MAX as u64 {
        u32::MAX as usize
    } else {
        bodies as usize
    }
}

/// Whether `local` sheds `requester`'s request for a page it is already writing to at least
/// `threshold` requesters ([`hot_page_bodies`]), when that page is at least half of the `all`
/// bodies it is writing (otherwise moving readers to helpers only adds a relayed hop). Only one
/// of the first three ranked `owners` sheds, and never sheds another owner or one of the page's
/// helpers (`helper`, evaluated last), so the owner's NIC feeds at most [`fanout_width`] helpers.
pub fn shed_hot_page<N: PartialEq>(
    owners: &[N],
    local: &N,
    requester: &N,
    helper: impl FnOnce() -> bool,
    streaming: usize,
    all: usize,
    threshold: usize,
) -> bool {
    let owners = &owners[..owners.len().min(3)];
    streaming >= threshold.max(1)
        && streaming * 2 >= all
        && owners.contains(local)
        && !owners.contains(requester)
        && !helper()
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

/// Position (in membership order) of the member `requester` asks to acquire `page` for it, or
/// `None` when the membership has no eligible helper. `excluded(position)` must be true for the
/// page's owners and the requester. Starting at `hash(requester, page) % width`, takes the first
/// non-excluded `preferred` (direct neighbor) helper, else the first non-excluded one; scans at
/// most [`fanout_width`] helpers.
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
mod fanout_tests {
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
        let shed = |owners: &[u8], requester, helper, streaming, all, n| {
            shed_hot_page(owners, &0, &requester, || helper, streaming, all, n)
        };
        for n in [hot_page_bodies(1 << 20), hot_page_bodies(16 << 20)] {
            assert!(!shed(&[0, 1, 2], 9, false, n - 1, n - 1, n));
            assert!(shed(&[0, 1, 2], 9, false, n, n, n));
            assert!(shed(&[1, 0, 2], 9, false, n, 2 * n, n));
            // A page that is not most of the owner's queue is not shed.
            assert!(!shed(&[0, 1, 2], 9, false, n, 2 * n + 1, n));
            // Owners and helpers are exempt.
            assert!(!shed(&[0, 1, 2], 2, false, n, n, n));
            assert!(!shed(&[0, 1, 2], 9, true, n, n, n));
            // Non-owners, including ranks past the first three, never shed.
            assert!(!shed(&[1, 2, 3], 9, false, n, n, n));
            assert!(!shed(&[1, 2, 3, 0], 9, false, n, n, n));
            // A requester ranked past the first three is not an owner.
            assert!(shed(&[0, 1, 2, 9], 9, false, n, n, n));
        }
        // A zero threshold still needs one body in flight.
        assert!(!shed(&[0], 9, false, 0, 0, 0));
    }

    #[test]
    fn hot_page_threshold_is_the_same_backlog_in_bytes() {
        assert_eq!(hot_page_bodies(1 << 20), 32);
        assert_eq!(hot_page_bodies(16 << 20), 2);
        assert_eq!(hot_page_bodies(8 << 20), 4);
        // Huge pages never go below the floor; tiny and zero pages saturate.
        assert_eq!(hot_page_bodies(1 << 30), HOT_PAGE_MIN_BODIES);
        assert_eq!(hot_page_bodies(1), (32 << 20) as usize);
        assert_eq!(hot_page_bodies(0), (32 << 20) as usize);
        // Within the floor, the queued bytes never exceed the byte budget by more than one page.
        for shift in 10..=26 {
            let page = 1u64 << shift;
            let bodies = hot_page_bodies(page) as u64;
            assert!(bodies * page <= HOT_PAGE_QUEUE_BYTES.max(page * HOT_PAGE_MIN_BODIES as u64));
        }
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
