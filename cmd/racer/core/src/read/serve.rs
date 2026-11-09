//! `ServeDecision`: the serve side of a peer page request as a sans-IO machine.
//!
//! It owns the per-page streaming counters (bodies this node is writing) and composes the
//! existing rules: mesh v2 hot-page shedding ([`shed_hot_page`]), the dispatch table
//! ([`serve_action`]) and copy-join ([`copy_join_allowed`]). The owner ranking is computed by the
//! interpreter only for a page that is already hot, so a decision may take two steps:
//! [`ServeDecision::decide`] returns [`Step::Rank`], and [`ServeDecision::ranked`] finishes it.

use super::budget::{HotPageLimits, hot_page_bodies, shed_hot_page};
use super::{Error, ServeAction, ServeFacts, copy_join_allowed, serve_action};
use std::collections::HashMap;
use std::hash::Hash;

/// What the request asks, after authentication.
#[derive(Clone, Copy, Debug)]
pub struct ServeRequest {
    pub facts: ServeFacts,
    /// The request may carry a page body: a page request that is not an offer or a ping.
    pub body: bool,
    /// This node runs an acquisition of the page a copy-only request could join.
    pub local_flight: bool,
}

/// How to serve the request.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Decision {
    Reject(Error),
    /// Answer `Miss` without touching storage.
    Miss,
    /// Answer `Overloaded`: the page is hot and the requester should ask its mesh helper.
    Shed,
    /// Serve local copies only; `join` when it may wait on the running local acquisition.
    CopyOnly {
        join: bool,
    },
    /// Open credentials and acquire with the inherited budget.
    Acquire,
}

/// One step of a decision.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Step {
    Decided(Decision),
    /// The page is hot (or a copy-only join needs them): rank its owners and call
    /// [`ServeDecision::ranked`].
    Rank,
}

/// A body this node writes; return it with [`ServeDecision::end_body`].
#[derive(Debug, Eq, PartialEq)]
#[must_use = "a body must be ended"]
pub struct Body<P> {
    page: P,
}

/// Serve-side state of one worker.
pub struct ServeDecision<P> {
    limits: HotPageLimits,
    streaming: HashMap<P, usize>,
    all: usize,
}

impl<P: Clone + Eq + Hash> ServeDecision<P> {
    /// Shedding for `page_bytes` pages under mesh v2 limits.
    pub fn new(page_bytes: u64) -> Self {
        Self::with_limits(HotPageLimits::mesh(hot_page_bodies(page_bytes)))
    }

    pub fn with_limits(limits: HotPageLimits) -> Self {
        Self {
            limits,
            streaming: HashMap::new(),
            all: 0,
        }
    }

    /// Re-derive the mesh v2 hot-page threshold for `page_bytes` pages, the
    /// cluster page size once it is published. Bodies being written keep
    /// their counts.
    pub fn set_page_bytes(&mut self, page_bytes: u64) {
        self.limits = HotPageLimits::mesh(hot_page_bodies(page_bytes));
    }

    /// The hot-page limits in force.
    pub fn limits(&self) -> HotPageLimits {
        self.limits
    }

    /// Bodies of `page` being written, and of every page.
    pub fn streaming(&self, page: &P) -> (usize, usize) {
        (self.streaming.get(page).copied().unwrap_or(0), self.all)
    }

    /// A body of `page` starts.
    pub fn begin_body(&mut self, page: P) -> Body<P> {
        *self.streaming.entry(page.clone()).or_default() += 1;
        self.all += 1;
        Body { page }
    }

    /// A body ended (written, failed or abandoned).
    pub fn end_body(&mut self, body: Body<P>) {
        if let Some(count) = self.streaming.get_mut(&body.page) {
            *count -= 1;
            if *count == 0 {
                self.streaming.remove(&body.page);
            }
            self.all -= 1;
        }
    }

    /// Whether `page` is hot: a body request for it needs the page's ranking to decide whether
    /// to shed.
    pub fn hot(&self, page: &P) -> bool {
        self.streaming(page).0 >= self.limits.threshold.max(1)
    }

    /// Whether to shed a body request for `page` from `requester`, given the page's ranked
    /// `owners` (`budget::shed_hot_page` over this machine's streaming counters).
    pub fn shed<N: PartialEq>(
        &self,
        page: &P,
        owners: &[N],
        local: &N,
        requester: &N,
        column_mate: impl FnOnce() -> bool,
    ) -> bool {
        let (streaming, all) = self.streaming(page);
        shed_hot_page(
            owners,
            local,
            requester,
            column_mate,
            streaming,
            all,
            self.limits,
        )
    }

    /// First step: a hot body request or a joinable copy-only request needs the ranking
    /// ([`Step::Rank`], then [`Self::ranked`]); anything else is decided now.
    pub fn decide(&self, page: &P, request: ServeRequest) -> Step {
        if request.body && self.hot(page) {
            return Step::Rank;
        }
        match self.dispatch(request) {
            Decision::CopyOnly { .. } if request.local_flight => Step::Rank,
            decision => Step::Decided(decision),
        }
    }

    /// Second step with the page's ranked `owners`. `column_mate` reports whether the requester
    /// is this node's column mate in the page's pool; it runs only when it matters.
    pub fn ranked<N: PartialEq>(
        &self,
        page: &P,
        request: ServeRequest,
        owners: &[N],
        local: &N,
        requester: &N,
        column_mate: impl FnOnce() -> bool,
    ) -> Decision {
        if request.body && self.shed(page, owners, local, requester, column_mate) {
            return Decision::Shed;
        }
        match self.dispatch(request) {
            Decision::CopyOnly { .. } => Decision::CopyOnly {
                join: request.local_flight && copy_join_allowed(owners, local, requester),
            },
            decision => decision,
        }
    }

    fn dispatch(&self, request: ServeRequest) -> Decision {
        match serve_action(request.facts) {
            ServeAction::Reject(error) => Decision::Reject(error),
            ServeAction::Miss => Decision::Miss,
            ServeAction::CopyOnly => Decision::CopyOnly { join: false },
            ServeAction::Acquire => Decision::Acquire,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::read::OperationKind;

    fn request(
        operation: OperationKind,
        copy_only: bool,
        body: bool,
        flight: bool,
    ) -> ServeRequest {
        ServeRequest {
            facts: ServeFacts {
                operation,
                copy_only,
                object_matches: true,
                cache_available: true,
            },
            body,
            local_flight: flight,
        }
    }

    fn page() -> ServeRequest {
        request(OperationKind::Page, false, true, false)
    }

    #[test]
    fn page_bytes_rederive_the_hot_threshold() {
        let mut serve = ServeDecision::<u32>::new(16 << 20);
        assert_eq!(serve.limits().threshold, hot_page_bodies(16 << 20));
        let _body = serve.begin_body(7);
        serve.set_page_bytes(64 << 10);
        assert_eq!(
            serve.limits(),
            HotPageLimits::mesh(hot_page_bodies(64 << 10))
        );
        assert!(serve.limits().threshold > hot_page_bodies(16 << 20));
        assert_eq!(serve.streaming(&7), (1, 1));
    }

    fn machine(threshold: usize) -> ServeDecision<u32> {
        ServeDecision::with_limits(HotPageLimits::mesh(threshold))
    }

    #[test]
    fn cold_pages_dispatch_without_a_ranking() {
        let m = machine(2);
        assert_eq!(m.decide(&1, page()), Step::Decided(Decision::Acquire));
        assert_eq!(
            m.decide(&1, request(OperationKind::Page, true, true, false)),
            Step::Decided(Decision::CopyOnly { join: false })
        );
        assert_eq!(
            m.decide(&1, request(OperationKind::Subscribe, false, false, false)),
            Step::Decided(Decision::Reject(Error::InvalidRequest))
        );
        let mut miss = page();
        miss.facts.cache_available = false;
        assert_eq!(m.decide(&1, miss), Step::Decided(Decision::Miss));
        // Prefetch fills always acquire.
        assert_eq!(
            m.decide(&1, request(OperationKind::Prefetch, true, true, false)),
            Step::Decided(Decision::Acquire)
        );
    }

    #[test]
    fn counters_track_bodies_per_page() {
        let mut m = machine(2);
        let a = m.begin_body(1);
        let b = m.begin_body(1);
        let c = m.begin_body(2);
        assert_eq!(m.streaming(&1), (2, 3));
        m.end_body(a);
        assert_eq!(m.streaming(&1), (1, 2));
        m.end_body(b);
        m.end_body(c);
        assert_eq!(m.streaming(&1), (0, 0));
        assert!(m.streaming.is_empty());
    }

    #[test]
    fn hot_pages_shed_readers_but_not_owners() {
        let mut m = machine(2);
        let bodies = [m.begin_body(7), m.begin_body(7)];
        assert_eq!(m.decide(&7, page()), Step::Rank);
        let owners = [1, 2, 3];
        assert_eq!(
            m.ranked(&7, page(), &owners, &1, &9, || false),
            Decision::Shed
        );
        // Another owner is never shed.
        assert_eq!(
            m.ranked(&7, page(), &owners, &1, &2, || false),
            Decision::Acquire
        );
        // A non-owner never sheds.
        assert_eq!(
            m.ranked(&7, page(), &owners, &5, &9, || false),
            Decision::Acquire
        );
        // A column mate is exempt below the exempt cap.
        assert_eq!(
            m.ranked(&7, page(), &owners, &1, &9, || true),
            Decision::Acquire
        );
        // Offers and pings carry no body: not shed.
        let offer = request(OperationKind::Page, false, false, false);
        assert_eq!(m.decide(&7, offer), Step::Decided(Decision::Acquire));
        for body in bodies {
            m.end_body(body);
        }
        assert_eq!(m.decide(&7, page()), Step::Decided(Decision::Acquire));
    }

    #[test]
    fn a_hot_page_that_is_a_minority_of_bodies_is_not_shed() {
        let mut m = machine(2);
        let _hot = [m.begin_body(7), m.begin_body(7)];
        let _other = [m.begin_body(8), m.begin_body(8), m.begin_body(8)];
        assert_eq!(m.decide(&7, page()), Step::Rank);
        assert_eq!(
            m.ranked(&7, page(), &[1, 2, 3], &1, &9, || false),
            Decision::Acquire
        );
    }

    #[test]
    fn column_mates_are_shed_past_the_exempt_cap() {
        let mut m = machine(2);
        let cap = HotPageLimits::mesh(2).exempt_cap;
        let _bodies: Vec<_> = (0..cap).map(|_| m.begin_body(7)).collect();
        let mut asked = false;
        assert_eq!(
            m.ranked(&7, page(), &[1, 2, 3], &1, &9, || {
                asked = true;
                true
            }),
            Decision::Shed
        );
        assert!(!asked, "column mate check skipped past the cap");
    }

    #[test]
    fn copy_only_joins_follow_rank_order() {
        let m = machine(2);
        let copy = request(OperationKind::Page, true, true, true);
        assert_eq!(m.decide(&1, copy), Step::Rank);
        let owners = [1, 2, 3];
        // A reader may join; an owner ranked after this node may join; one ranked before may not.
        assert_eq!(
            m.ranked(&1, copy, &owners, &2, &9, || false),
            Decision::CopyOnly { join: true }
        );
        assert_eq!(
            m.ranked(&1, copy, &owners, &2, &3, || false),
            Decision::CopyOnly { join: true }
        );
        assert_eq!(
            m.ranked(&1, copy, &owners, &2, &1, || false),
            Decision::CopyOnly { join: false }
        );
        // Without a local flight there is nothing to join.
        let alone = request(OperationKind::Page, true, true, false);
        assert_eq!(
            m.ranked(&1, alone, &owners, &2, &9, || false),
            Decision::CopyOnly { join: false }
        );
    }

    #[test]
    fn page_size_sets_the_threshold() {
        let m: ServeDecision<u32> = ServeDecision::new(16 << 20);
        assert_eq!(m.limits.threshold, 2);
        let m: ServeDecision<u32> = ServeDecision::new(1 << 20);
        assert_eq!(m.limits.threshold, 32);
    }

    #[test]
    fn hot_pages_shed_only_past_the_threshold() {
        let mut m = machine(2);
        let owners = [1u32, 2, 3];
        assert!(!m.hot(&7));
        let a = m.begin_body(7);
        assert!(!m.hot(&7));
        let b = m.begin_body(7);
        assert!(m.hot(&7));
        // Rank 0 sheds a non-owner reader; another owner is never shed.
        assert!(m.shed(&7, &owners, &1, &9, || false));
        assert!(!m.shed(&7, &owners, &1, &2, || false));
        m.end_body(a);
        assert!(!m.hot(&7));
        m.end_body(b);
        assert_eq!(m.streaming(&7), (0, 0));
    }
}
