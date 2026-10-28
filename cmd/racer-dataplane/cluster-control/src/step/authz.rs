//! Bearer authorization caching and API budget (`auth.rs`).
//!
//! A full authorization costs a TokenReview POST and four GETs (Pod,
//! DaemonSet, ServiceAccount, Node). [`AuthCache`] removes repeated calls and
//! [`ApiBudget`] keeps the rest under a per-process rate, so a storm is shed
//! locally with `Overloaded` instead of by API server 429s. Why caching is
//! safe:
//! - Successful TokenReviews, keyed by the token's SHA-256, for at most
//!   [`REVIEW_TTL`] (kube-apiserver's own window) and never past `exp`. Pod
//!   and Node are still read live and matched by UID; failures are not cached.
//! - Whole successful authorizations for at most [`CALLER_TTL`], so the
//!   bootstrap after a keyring poll skips the API. Revocation is seen within
//!   `CALLER_TTL`, short against the certificate lifetime.
//! - DaemonSet and ServiceAccount UIDs for at most [`BINDINGS_TTL`]; a failed
//!   authorization against cached bindings drops them.

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

/// Longest time a whole successful authorization (TokenReview plus the
/// live Pod and Node reads) is reused for the same token.
pub const CALLER_TTL: Duration = Duration::from_secs(5);

/// Bound on cached reviews, and separately on cached authorizations.
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

/// Cached authorization inputs and results, `C` being the authorized
/// caller. See the module docs for the safety argument.
#[derive(Debug)]
pub struct AuthCache<C = ()> {
    reviews: BTreeMap<TokenKey, (Reviewed, Instant)>,
    callers: BTreeMap<TokenKey, (C, Instant)>,
    bindings: Option<(Bindings, Instant)>,
}

impl<C> Default for AuthCache<C> {
    fn default() -> Self {
        Self {
            reviews: BTreeMap::new(),
            callers: BTreeMap::new(),
            bindings: None,
        }
    }
}

/// Insert into a bounded expiring map: expired entries are purged first,
/// then the entry closest to expiry is evicted.
fn put_bounded<V>(
    map: &mut BTreeMap<TokenKey, (V, Instant)>,
    key: TokenKey,
    v: V,
    until: Instant,
    now: Instant,
) {
    if map.len() >= MAX_REVIEWS && !map.contains_key(&key) {
        map.retain(|_, (_, u)| now < *u);
        if map.len() >= MAX_REVIEWS
            && let Some(k) = map.iter().min_by_key(|(_, (_, u))| *u).map(|(k, _)| *k)
        {
            map.remove(&k);
        }
    }
    map.insert(key, (v, until));
}

impl<C: Clone> AuthCache<C> {
    /// A whole authorization of `key` younger than [`CALLER_TTL`].
    pub fn caller(&mut self, key: &TokenKey, now: Instant) -> Option<C> {
        match self.callers.get(key) {
            Some((c, until)) if now < *until => Some(c.clone()),
            Some(_) => {
                self.callers.remove(key);
                None
            }
            None => None,
        }
    }

    /// Remember a whole successful authorization for [`CALLER_TTL`], never
    /// past the token's expiry.
    pub fn put_caller(&mut self, key: TokenKey, c: C, token_left: Duration, now: Instant) {
        let ttl = CALLER_TTL.min(token_left);
        if !ttl.is_zero() {
            put_bounded(&mut self.callers, key, c, now + ttl, now);
        }
    }

    /// A cached review of `key` still valid at `now`.
    pub fn review(&mut self, key: &TokenKey, now: Instant) -> Option<Reviewed> {
        match self.reviews.get(key) {
            Some((r, until)) if now < *until => Some(r.clone()),
            Some(_) => {
                self.reviews.remove(key);
                None
            }
            None => None,
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
        put_bounded(&mut self.reviews, key, r, now + ttl, now);
    }

    /// Forget a review and authorization, for example after the live
    /// checks refused it.
    pub fn drop_review(&mut self, key: &TokenKey) {
        self.reviews.remove(key);
        self.callers.remove(key);
    }

    /// Cached bindings still valid at `now`.
    pub fn bindings(&mut self, now: Instant) -> Option<Bindings> {
        match &self.bindings {
            Some((b, until)) if now < *until => Some(b.clone()),
            _ => {
                self.bindings = None;
                None
            }
        }
    }

    pub fn put_bindings(&mut self, b: Bindings, now: Instant) {
        self.bindings = Some((b, now + BINDINGS_TTL));
    }

    /// Forget every cached whole authorization, as if [`CALLER_TTL`] had
    /// passed; reviews and bindings stay.
    pub fn drop_callers(&mut self) {
        self.callers.clear();
    }

    /// Forget the bindings so the next authorization reads them live.
    pub fn drop_bindings(&mut self) {
        self.bindings = None;
    }

    /// API calls the next authorization of `key` makes at `now`: none
    /// when the whole authorization is cached, else Pod and Node, plus the
    /// TokenReview and two binding reads when not cached. Does not count
    /// as a cache lookup.
    pub fn calls(&self, key: &TokenKey, now: Instant) -> u32 {
        if self.callers.get(key).is_some_and(|(_, u)| now < *u) {
            return 0;
        }
        let review = self.reviews.get(key).is_some_and(|(_, u)| now < *u);
        let bindings = self.bindings.as_ref().is_some_and(|(_, u)| now < *u);
        2 + u32::from(!review) + 2 * u32::from(!bindings)
    }

    /// Cached entries (reviews plus authorizations).
    pub fn len(&self) -> usize {
        self.reviews.len() + self.callers.len()
    }

    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }
}

/// Per-process token bucket for authorization API calls. An authorization
/// reserves all of its calls up front, so it runs to completion or is refused
/// before its first call. AIMD against the API server: a 429 halves the rate
/// (once per [`CUT_INTERVAL`]) and drops the saved burst; it never falls below
/// 1/16 of the configured rate and recovers linearly over [`RECOVERY`].
#[derive(Clone, Debug)]
pub struct ApiBudget {
    pub max_qps: f64,
    pub burst: f64,
    /// Current sustained rate.
    pub qps: f64,
    tokens: f64,
    at: Option<Instant>,
    cut_at: Option<Instant>,
}

/// Minimum time between two rate cuts.
pub const CUT_INTERVAL: Duration = Duration::from_secs(1);

/// Time to grow from the floor back to the configured rate.
pub const RECOVERY: Duration = Duration::from_secs(30);

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
        }
    }

    fn refill(&mut self, now: Instant) {
        if let Some(at) = self.at {
            let dt = now.saturating_duration_since(at).as_secs_f64();
            self.tokens = (self.tokens + dt * self.qps).min(self.burst);
            self.qps = (self.qps + dt * self.max_qps / RECOVERY.as_secs_f64()).min(self.max_qps);
        }
        self.at = Some(self.at.map_or(now, |a| a.max(now)));
    }

    /// Reserve `calls` tokens at `now`, borrowing against future refill:
    /// returns how long to wait before the calls, at most `max_wait`. Fails,
    /// taking nothing, when the wait would be longer.
    pub fn reserve(&mut self, calls: u32, now: Instant, max_wait: Duration) -> Option<Duration> {
        self.refill(now);
        let n = f64::from(calls);
        let short = n - self.tokens;
        let wait = if short <= 1e-9 {
            Duration::ZERO
        } else {
            Duration::from_secs_f64(short / self.qps)
        };
        if wait > max_wait {
            return None;
        }
        self.tokens -= n;
        Some(wait)
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
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    impl ApiBudget {
        fn try_reserve(&mut self, calls: u32, now: Instant) -> bool {
            self.reserve(calls, now, Duration::ZERO).is_some()
        }
    }

    fn reviewed(pod: &str) -> Reviewed {
        Reviewed {
            pod_name: pod.into(),
            ..Reviewed::default()
        }
    }

    #[test]
    fn review_ttl_is_capped_by_token_expiry() {
        let t0 = Instant::now();
        let mut c: AuthCache = AuthCache::default();
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
    }

    #[test]
    fn whole_authorizations_are_reused_briefly() {
        let t0 = Instant::now();
        let mut c: AuthCache<u32> = AuthCache::default();
        let k = token_key("a");
        c.put_caller(k, 7, Duration::from_secs(60), t0);
        assert_eq!(c.calls(&k, t0), 0);
        assert_eq!(c.caller(&k, t0 + Duration::from_secs(4)), Some(7));
        assert_eq!(c.caller(&k, t0 + CALLER_TTL), None);
        assert_eq!(c.calls(&k, t0 + CALLER_TTL), FULL_CALLS);
        // Capped by token expiry, and dropped with the review.
        c.put_caller(k, 7, Duration::from_secs(1), t0);
        assert_eq!(c.caller(&k, t0 + Duration::from_secs(1)), None);
        c.put_caller(k, 7, Duration::from_secs(60), t0);
        c.drop_review(&k);
        assert_eq!(c.caller(&k, t0), None);
    }

    #[test]
    fn distinct_tokens_do_not_share_reviews() {
        let t0 = Instant::now();
        let mut c: AuthCache = AuthCache::default();
        c.put_review(token_key("a"), reviewed("p"), REVIEW_TTL, t0);
        assert_eq!(c.review(&token_key("b"), t0), None);
        c.drop_review(&token_key("a"));
        assert_eq!(c.review(&token_key("a"), t0), None);
    }

    #[test]
    fn bindings_expire_and_drop() {
        let t0 = Instant::now();
        let mut c: AuthCache = AuthCache::default();
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
        let mut c: AuthCache = AuthCache::default();
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
        assert!(
            b.reserve(1, t0 + Duration::from_secs(1), Duration::ZERO)
                .is_some()
        );
        assert!(b.try_reserve(5, t0 + Duration::from_secs(100)));
        assert!(!b.try_reserve(1, t0 + Duration::from_secs(100)));
    }

    #[test]
    fn budget_reservations_wait_up_to_a_bound() {
        let t0 = Instant::now();
        let mut b = ApiBudget::new(10.0, 5.0);
        assert_eq!(b.reserve(5, t0, Duration::ZERO), Some(Duration::ZERO));
        // 0.5 s of refill borrowed.
        let w = b.reserve(5, t0, Duration::from_secs(1)).unwrap();
        assert!((w.as_secs_f64() - 0.5).abs() < 1e-6, "{w:?}");
        // Next in line waits behind it; past the bound it is refused and
        // nothing is taken.
        assert_eq!(b.reserve(10, t0, Duration::from_secs(1)), None);
        let w = b.reserve(5, t0, Duration::from_secs(1)).unwrap();
        assert!((w.as_secs_f64() - 1.0).abs() < 1e-6, "{w:?}");
        // 15 tokens reserved against a burst of 5 and no refill.
        assert!((b.tokens + 10.0).abs() < 1e-9, "{}", b.tokens);
    }

    #[test]
    fn budget_backs_off_on_throttling_and_recovers() {
        let t0 = Instant::now();
        let s = |n: u64| t0 + Duration::from_millis(n);
        let mut b = ApiBudget::new(160.0, 160.0);
        assert!(b.try_reserve(5, t0));
        b.throttled(s(10));
        assert_eq!(b.qps, 80.0);
        // Saved burst is dropped.
        assert!(!b.try_reserve(5, s(10)));
        // 429s within CUT_INTERVAL count once.
        b.throttled(s(500));
        assert_eq!(b.qps, 80.0);
        for i in 0..10 {
            b.throttled(s(1100 + i * 1000));
        }
        // Never below 1/16 of the configured rate.
        assert!(b.qps >= 10.0, "{}", b.qps);
        assert!(b.qps < 12.0, "{}", b.qps);
        // Linear recovery to the configured rate.
        b.try_reserve(0, s(10_100 + RECOVERY.as_millis() as u64));
        assert_eq!(b.qps, 160.0);
    }
}
