//! Bearer authorization caching and API budget, lifted from `auth.rs`.
//!
//! A full bearer authorization costs one TokenReview POST and four GETs
//! (Pod, DaemonSet, ServiceAccount, Node). During an enrollment storm every
//! seat runs up to [`limits::MAX_BOOTSTRAPS`] of these at once, and an
//! unenrolled client authorizes again for its bearer keyring poll and again
//! when that poll returns data. [`AuthCache`] removes the repeated calls and
//! [`ApiBudget`] keeps the remainder under a per-process rate so a storm is
//! shed locally with `Overloaded` instead of by API server 429s, which would
//! waste the calls already made for the same authorization.
//!
//! What is cached, and why it is safe:
//! - A successful TokenReview, keyed by the SHA-256 of the token, for at most
//!   [`REVIEW_TTL`] and never past the token's `exp`. This is the same
//!   window kube-apiserver uses for its own token authentication cache. A
//!   review result for a bound token only says the signature is valid and
//!   the bound Pod, Node and ServiceAccount existed. Revocation by deleting
//!   the Pod or Node is still seen at once: both are read live on every
//!   authorization and must match the review by UID. Deleting the
//!   ServiceAccount is seen within [`BINDINGS_TTL`] through the binding
//!   cache. Failed reviews are never cached.
//! - The DaemonSet and ServiceAccount UIDs, shared by every caller, for at
//!   most [`BINDINGS_TTL`]. Both are cluster objects an operator changes
//!   rarely; a replaced or deleting DaemonSet or ServiceAccount is refused
//!   at most `BINDINGS_TTL` later. A failed authorization against cached
//!   bindings drops them, so the next attempt reads them live.

use crate::limits;
use sha2::{Digest, Sha256};
use std::{
    collections::BTreeMap,
    time::{Duration, Instant},
};

/// Longest time a successful TokenReview is reused.
pub const REVIEW_TTL: Duration = Duration::from_secs(10);

/// Longest time the DaemonSet and ServiceAccount UIDs are reused.
pub const BINDINGS_TTL: Duration = Duration::from_secs(5);

/// Bound on cached reviews; expired entries are dropped first, then the
/// entry closest to expiry.
pub const MAX_REVIEWS: usize = 8192;

/// API calls of an authorization with nothing cached.
pub const FULL_CALLS: u32 = 5;

/// Cache key of a bearer token.
pub type TokenKey = [u8; 32];

pub fn token_key(token: &str) -> TokenKey {
    Sha256::digest(token.as_bytes()).into()
}

/// The parts of a reviewed token used after the review.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Reviewed {
    pub uid: String,
    pub pod_name: String,
    pub pod_uid: String,
    pub node_name: String,
    pub node_uid: String,
}

/// Live UIDs of the managed DaemonSet (empty while deleting) and the
/// dataplane ServiceAccount.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Bindings {
    pub daemonset_uid: String,
    pub service_account_uid: String,
    /// The ServiceAccount has a deletionTimestamp.
    pub service_account_deleting: bool,
}

/// Cached authorization inputs. See the module docs for the safety argument.
#[derive(Debug, Default)]
pub struct AuthCache {
    reviews: BTreeMap<TokenKey, (Reviewed, Instant)>,
    bindings: Option<(Bindings, Instant)>,
    pub stats: CacheStats,
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct CacheStats {
    pub review_hits: u64,
    pub review_misses: u64,
    pub binding_hits: u64,
    pub binding_misses: u64,
}

impl AuthCache {
    /// A cached review of `key` still valid at `now`.
    pub fn review(&mut self, key: &TokenKey, now: Instant) -> Option<Reviewed> {
        match self.reviews.get(key) {
            Some((r, until)) if now < *until => {
                self.stats.review_hits += 1;
                Some(r.clone())
            }
            Some(_) => {
                self.reviews.remove(key);
                self.stats.review_misses += 1;
                None
            }
            None => {
                self.stats.review_misses += 1;
                None
            }
        }
    }

    /// Remember a successful review until `REVIEW_TTL` from `now` or the
    /// token's expiry, whichever is first. `token_left` is the time until
    /// `exp`; zero stores nothing.
    pub fn put_review(&mut self, key: TokenKey, r: Reviewed, token_left: Duration, now: Instant) {
        let ttl = REVIEW_TTL.min(token_left);
        if ttl.is_zero() {
            return;
        }
        if self.reviews.len() >= MAX_REVIEWS && !self.reviews.contains_key(&key) {
            self.reviews.retain(|_, (_, until)| now < *until);
            if self.reviews.len() >= MAX_REVIEWS
                && let Some(k) = self
                    .reviews
                    .iter()
                    .min_by_key(|(_, (_, until))| *until)
                    .map(|(k, _)| *k)
            {
                self.reviews.remove(&k);
            }
        }
        self.reviews.insert(key, (r, now + ttl));
    }

    /// Forget a review, for example after the live checks refused it.
    pub fn drop_review(&mut self, key: &TokenKey) {
        self.reviews.remove(key);
    }

    /// Cached bindings still valid at `now`.
    pub fn bindings(&mut self, now: Instant) -> Option<Bindings> {
        match &self.bindings {
            Some((b, until)) if now < *until => {
                self.stats.binding_hits += 1;
                Some(b.clone())
            }
            _ => {
                self.bindings = None;
                self.stats.binding_misses += 1;
                None
            }
        }
    }

    pub fn put_bindings(&mut self, b: Bindings, now: Instant) {
        self.bindings = Some((b, now + BINDINGS_TTL));
    }

    /// Forget the bindings so the next authorization reads them live.
    pub fn drop_bindings(&mut self) {
        self.bindings = None;
    }

    /// API calls the next authorization of `key` makes at `now`: Pod and
    /// Node always, plus the TokenReview and two binding reads when not
    /// cached. Does not count as a cache lookup.
    pub fn calls(&self, key: &TokenKey, now: Instant) -> u32 {
        let review = self.reviews.get(key).is_some_and(|(_, u)| now < *u);
        let bindings = self.bindings.as_ref().is_some_and(|(_, u)| now < *u);
        2 + u32::from(!review) + 2 * u32::from(!bindings)
    }

    pub fn len(&self) -> usize {
        self.reviews.len()
    }

    pub fn is_empty(&self) -> bool {
        self.reviews.is_empty()
    }
}

/// Per-process token bucket for authorization API calls. An authorization
/// reserves all of its calls up front, so it either runs to completion
/// under the budget or is refused before its first call.
///
/// The rate adapts to the API server (additive increase, multiplicative
/// decrease): an API 429 halves it, at most once per [`CUT_INTERVAL`] so a
/// burst of 429s from calls already in flight counts once, and drops the
/// saved burst. It never falls below 1/16 of the configured rate and grows
/// back to it linearly over [`RECOVERY`]. Seats share one API server
/// without coordinating, so this is what keeps their sum near its capacity
/// instead of each retrying into 429s.
#[derive(Clone, Debug)]
pub struct ApiBudget {
    pub max_qps: f64,
    pub burst: f64,
    qps: f64,
    tokens: f64,
    at: Option<Instant>,
    cut_at: Option<Instant>,
    pub stats: BudgetStats,
}

/// Minimum time between two rate cuts.
pub const CUT_INTERVAL: Duration = Duration::from_secs(1);

/// Time to grow from the floor back to the configured rate.
pub const RECOVERY: Duration = Duration::from_secs(30);

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct BudgetStats {
    pub reserved: u64,
    pub refused: u64,
    pub cuts: u64,
}

impl Default for ApiBudget {
    fn default() -> Self {
        Self::new(limits::AUTH_API_QPS, limits::AUTH_API_BURST)
    }
}

impl ApiBudget {
    pub fn new(qps: f64, burst: f64) -> Self {
        Self {
            max_qps: qps,
            burst,
            qps,
            tokens: burst,
            at: None,
            cut_at: None,
            stats: BudgetStats::default(),
        }
    }

    /// Current sustained rate.
    pub fn qps(&self) -> f64 {
        self.qps
    }

    fn refill(&mut self, now: Instant) {
        if let Some(at) = self.at {
            let dt = now.saturating_duration_since(at).as_secs_f64();
            self.tokens = (self.tokens + dt * self.qps).min(self.burst);
            self.qps = (self.qps + dt * self.max_qps / RECOVERY.as_secs_f64()).min(self.max_qps);
        }
        self.at = Some(self.at.map_or(now, |a| a.max(now)));
    }

    /// Reserve `calls` tokens at `now`. Fails, taking nothing, when short.
    pub fn try_reserve(&mut self, calls: u32, now: Instant) -> bool {
        self.refill(now);
        let n = f64::from(calls);
        if self.tokens + 1e-9 >= n {
            self.tokens -= n;
            self.stats.reserved += u64::from(calls);
            true
        } else {
            self.stats.refused += 1;
            false
        }
    }

    /// Return tokens of calls that were reserved but not made.
    pub fn refund(&mut self, calls: u32) {
        self.tokens = (self.tokens + f64::from(calls)).min(self.burst);
    }

    /// The API server answered 429: slow down.
    pub fn throttled(&mut self, now: Instant) {
        self.refill(now);
        if self
            .cut_at
            .is_some_and(|c| now.saturating_duration_since(c) < CUT_INTERVAL)
        {
            return;
        }
        self.cut_at = Some(now);
        self.qps = (self.qps / 2.0).max(self.max_qps / 16.0);
        self.tokens = self.tokens.min(0.0);
        self.stats.cuts += 1;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn reviewed(pod: &str) -> Reviewed {
        Reviewed {
            pod_name: pod.into(),
            ..Reviewed::default()
        }
    }

    #[test]
    fn review_ttl_is_capped_by_token_expiry() {
        let t0 = Instant::now();
        let mut c = AuthCache::default();
        let k = token_key("a");
        assert_eq!(c.calls(&k, t0), FULL_CALLS);
        c.put_review(k, reviewed("p"), Duration::from_secs(60), t0);
        assert_eq!(
            c.review(&k, t0 + Duration::from_secs(9)),
            Some(reviewed("p"))
        );
        assert_eq!(c.review(&k, t0 + REVIEW_TTL), None);
        // Token expiring in 2 s is never reused past its exp.
        c.put_review(k, reviewed("p"), Duration::from_secs(2), t0);
        assert!(c.review(&k, t0 + Duration::from_secs(1)).is_some());
        assert_eq!(c.review(&k, t0 + Duration::from_secs(2)), None);
        // Already expired: nothing stored.
        c.put_review(k, reviewed("p"), Duration::ZERO, t0);
        assert_eq!(c.review(&k, t0), None);
        assert_eq!(c.stats.review_hits, 2);
    }

    #[test]
    fn distinct_tokens_do_not_share_reviews() {
        let t0 = Instant::now();
        let mut c = AuthCache::default();
        c.put_review(token_key("a"), reviewed("p"), REVIEW_TTL, t0);
        assert_eq!(c.review(&token_key("b"), t0), None);
        c.drop_review(&token_key("a"));
        assert_eq!(c.review(&token_key("a"), t0), None);
    }

    #[test]
    fn bindings_expire_and_drop() {
        let t0 = Instant::now();
        let mut c = AuthCache::default();
        let k = token_key("a");
        assert_eq!(c.bindings(t0), None);
        c.put_bindings(Bindings::default(), t0);
        assert_eq!(c.calls(&k, t0), 3);
        c.put_review(k, reviewed("p"), REVIEW_TTL, t0);
        assert_eq!(c.calls(&k, t0), 2);
        assert!(c.bindings(t0 + Duration::from_secs(4)).is_some());
        assert_eq!(c.bindings(t0 + BINDINGS_TTL), None);
        c.put_bindings(Bindings::default(), t0);
        c.drop_bindings();
        assert_eq!(c.bindings(t0), None);
    }

    #[test]
    fn review_cache_is_bounded() {
        let t0 = Instant::now();
        let mut c = AuthCache::default();
        for i in 0..MAX_REVIEWS + 10 {
            let left = Duration::from_millis(1 + i as u64);
            c.put_review(token_key(&i.to_string()), reviewed("p"), left, t0);
        }
        assert_eq!(c.len(), MAX_REVIEWS);
        // The newest survives; the one closest to expiry went first.
        let last = (MAX_REVIEWS + 9).to_string();
        assert!(c.review(&token_key(&last), t0).is_some());
        assert!(c.review(&token_key("0"), t0).is_none());
        // Expired entries are purged before evicting live ones.
        let later = t0 + REVIEW_TTL;
        c.put_review(token_key("new"), reviewed("p"), REVIEW_TTL, later);
        assert_eq!(c.len(), 1);
    }

    #[test]
    fn budget_reserves_whole_authorizations() {
        let t0 = Instant::now();
        let mut b = ApiBudget::new(10.0, 5.0);
        assert!(b.try_reserve(5, t0));
        // Short by one: nothing is taken.
        assert!(!b.try_reserve(1, t0));
        assert!(!b.try_reserve(3, t0 + Duration::from_millis(200)));
        assert!(b.try_reserve(3, t0 + Duration::from_millis(300)));
        b.refund(10);
        assert!(b.try_reserve(5, t0 + Duration::from_millis(300)));
        assert!(!b.try_reserve(1, t0 + Duration::from_millis(300)));
        // Time going backwards does not mint tokens.
        assert!(!b.try_reserve(1, t0));
        // Refill caps at burst.
        assert!(b.try_reserve(5, t0 + Duration::from_secs(100)));
        assert!(!b.try_reserve(1, t0 + Duration::from_secs(100)));
        assert_eq!(b.stats.refused, 5);
    }

    #[test]
    fn budget_backs_off_on_throttling_and_recovers() {
        let t0 = Instant::now();
        let s = |n: u64| t0 + Duration::from_millis(n);
        let mut b = ApiBudget::new(160.0, 160.0);
        assert!(b.try_reserve(5, t0));
        b.throttled(s(10));
        assert_eq!(b.qps(), 80.0);
        // Saved burst is dropped.
        assert!(!b.try_reserve(5, s(10)));
        // 429s within CUT_INTERVAL count once.
        b.throttled(s(500));
        assert_eq!(b.stats.cuts, 1);
        for i in 0..10 {
            b.throttled(s(1100 + i * 1000));
        }
        // Never below 1/16 of the configured rate.
        assert!(b.qps() >= 10.0, "{}", b.qps());
        assert!(b.qps() < 12.0, "{}", b.qps());
        // Linear recovery to the configured rate.
        b.try_reserve(0, s(10_100 + RECOVERY.as_millis() as u64));
        assert_eq!(b.qps(), 160.0);
    }
}
