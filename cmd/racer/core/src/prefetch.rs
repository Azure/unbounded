//! Sans-IO prefetch rules: priorities, the owner peer-operation page and
//! result records, the owner fill budget, and retry backoff.
//!
//! The owner budget is the prefetch admission class: it bounds concurrent
//! prefetch fills and, optionally, origin bytes per second. A demand read that
//! joins a prefetch flight promotes it, which returns its slot and tokens.

use std::collections::{BTreeSet, HashMap};
use std::hash::Hash;
use std::time::{Duration, Instant};

/// Most pages one `prefetch` peer operation names.
pub const MAX_OPERATION_PAGES: usize = 256;

/// Ticket priority. Strict order: `High` before `Normal` before `Low`.
#[derive(Clone, Copy, Debug, Default, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub enum Priority {
    High,
    #[default]
    Normal,
    Low,
}

impl Priority {
    pub const ALL: [Self; 3] = [Self::High, Self::Normal, Self::Low];

    pub fn parse(value: &str) -> Option<Self> {
        match value {
            "high" => Some(Self::High),
            "normal" => Some(Self::Normal),
            "low" => Some(Self::Low),
            _ => None,
        }
    }

    pub fn as_str(self) -> &'static str {
        match self {
            Self::High => "high",
            Self::Normal => "normal",
            Self::Low => "low",
        }
    }

    /// Index into per-priority arrays, in dispatch order.
    pub fn index(self) -> usize {
        self as usize
    }
}

/// Owner failure codes carried in a `failed` record.
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub enum FailCode {
    /// Probe only: the page is not resident.
    NotResident = 0,
    VersionChanged = 1,
    NotFound = 2,
    Forbidden = 3,
    OriginError = 4,
    Overloaded = 5,
    Unavailable = 6,
}

impl FailCode {
    pub fn from_wire(value: u32) -> Option<Self> {
        Some(match value {
            0 => Self::NotResident,
            1 => Self::VersionChanged,
            2 => Self::NotFound,
            3 => Self::Forbidden,
            4 => Self::OriginError,
            5 => Self::Overloaded,
            6 => Self::Unavailable,
            _ => return None,
        })
    }
}

/// One owner result per requested page.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum PageResult {
    /// The owner filled and wrote the page.
    Admitted,
    /// The page was already resident.
    Resident,
    /// Over the owner budget; retry after this many milliseconds.
    Deferred {
        retry_after_ms: u32,
    },
    Failed(FailCode),
}

/// Encoded size of one result record.
pub const RESULT_RECORD_BYTES: usize = 5;

/// Whether a request names a valid page list: 1..=256 ascending unique pages.
pub fn valid_pages(pages: &[u64]) -> bool {
    (1..=MAX_OPERATION_PAGES).contains(&pages.len()) && pages.windows(2).all(|w| w[0] < w[1])
}

/// Concatenated big-endian page numbers. Rejects invalid lists.
pub fn encode_pages(pages: &[u64]) -> Option<Vec<u8>> {
    valid_pages(pages).then(|| pages.iter().flat_map(|p| p.to_be_bytes()).collect())
}

/// Decode and validate [`encode_pages`] output.
pub fn decode_pages(bytes: &[u8]) -> Option<Vec<u64>> {
    if !bytes.len().is_multiple_of(8) {
        return None;
    }
    let pages = bytes
        .chunks_exact(8)
        .map(|chunk| u64::from_be_bytes(chunk.try_into().expect("eight bytes")))
        .collect::<Vec<_>>();
    valid_pages(&pages).then_some(pages)
}

/// Encode one record per page: `u8 kind`, `u32 value` big-endian.
pub fn encode_results(results: &[PageResult]) -> Vec<u8> {
    let mut bytes = Vec::with_capacity(results.len() * RESULT_RECORD_BYTES);
    for result in results {
        let (kind, value) = match *result {
            PageResult::Admitted => (0u8, 0u32),
            PageResult::Resident => (1, 0),
            PageResult::Deferred { retry_after_ms } => (2, retry_after_ms),
            PageResult::Failed(code) => (3, code as u32),
        };
        bytes.push(kind);
        bytes.extend_from_slice(&value.to_be_bytes());
    }
    bytes
}

/// Decode [`encode_results`] output, rejecting unknown kinds or codes and
/// nonzero values where the kind defines none.
pub fn decode_results(bytes: &[u8]) -> Option<Vec<PageResult>> {
    if bytes.is_empty() || !bytes.len().is_multiple_of(RESULT_RECORD_BYTES) {
        return None;
    }
    bytes
        .chunks_exact(RESULT_RECORD_BYTES)
        .map(|record| {
            let value = u32::from_be_bytes(record[1..].try_into().expect("four bytes"));
            match (record[0], value) {
                (0, 0) => Some(PageResult::Admitted),
                (1, 0) => Some(PageResult::Resident),
                (2, retry_after_ms) => Some(PageResult::Deferred { retry_after_ms }),
                (3, code) => FailCode::from_wire(code).map(PageResult::Failed),
                _ => None,
            }
        })
        .collect()
}

/// Whether `results` validly answers a request of `pages` pages. A probe
/// answers only `resident` or `failed` with code 0; a fill never uses code 0.
pub fn results_answer(results: &[PageResult], pages: usize, probe: bool) -> bool {
    results.len() == pages
        && results.iter().all(|result| match result {
            PageResult::Resident => true,
            PageResult::Failed(FailCode::NotResident) => probe,
            PageResult::Admitted | PageResult::Deferred { .. } | PageResult::Failed(_) => !probe,
        })
}

/// Retry delay for attempt `attempt` (1 for the first retry): 100 ms base,
/// doubling, 10 s cap, full jitter from `unit` in `[0, 1)`, and never less
/// than the owner's `retry_after`.
pub fn backoff(attempt: u32, unit: f64, retry_after: Option<Duration>) -> Duration {
    let base = Duration::from_millis(100);
    let cap = Duration::from_secs(10);
    let ceiling = base
        .checked_mul(1u32 << attempt.saturating_sub(1).min(16))
        .unwrap_or(cap)
        .min(cap);
    let jittered = ceiling.mul_f64(unit.clamp(0.0, 1.0));
    match retry_after {
        Some(after) if after > jittered => after,
        _ => jittered,
    }
}

/// Longest throttle deferral the owner reports.
const MAX_THROTTLE_WAIT: Duration = Duration::from_secs(3600);

/// Backstop lifetime of an owner fill's slot. A fill that neither finishes nor is promoted
/// within it (its holder leaked) gives the slot back on the next admission.
pub const OWNER_LEASE_TTL: Duration = Duration::from_secs(300);

/// Owner-side prefetch admission. Each admitted fill holds one slot until its
/// last holder finishes, a demand read promotes it, or its lease expires.
///
/// Admissions of a key that is already filling join that fill: they take no
/// slot and no tokens, but each holds a reference, and the slot is released
/// only when the last reference finishes. Every admission returns a
/// [`OwnerLease`] with a unique id that is passed back to [`OwnerBudget::finish`];
/// finishing a lease twice, or a lease from an entry that has since expired, is ignored, so a
/// duplicated release can never drop another holder's reference.
#[derive(Debug)]
pub struct OwnerBudget<K> {
    concurrent: usize,
    rate: u64,
    tokens: f64,
    refilled: Option<Instant>,
    ttl: Duration,
    generation: u64,
    next_lease: u64,
    /// Entries holding a slot (not promoted).
    slots: usize,
    active: HashMap<K, OwnerEntry>,
}

#[derive(Debug)]
struct OwnerEntry {
    bytes: u64,
    /// Ids of the live leases referencing this fill.
    holders: BTreeSet<u64>,
    generation: u64,
    promoted: bool,
    expires: Instant,
}

/// One admission's reference to an owner fill (see [`OwnerBudget::admit`]).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
#[must_use = "a lease must be finished"]
pub struct OwnerLease {
    generation: u64,
    id: u64,
}

impl OwnerLease {
    /// Unique id of this admission's reference.
    pub fn id(self) -> u64 {
        self.id
    }
}

/// Why the owner deferred a fill.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Deferral {
    Concurrency,
    Throttle,
}

impl<K: Eq + Hash + Clone> OwnerBudget<K> {
    /// `rate` is origin bytes per second; zero disables the token bucket. The
    /// bucket holds at most one second of tokens. A fill is admitted once the
    /// bucket holds `min(bytes, rate)` tokens and is charged its full size, so
    /// a page larger than one second's allowance leaves the bucket in debt and
    /// delays later fills until the debt is repaid.
    pub fn new(concurrent: usize, rate: u64) -> Self {
        Self {
            concurrent: concurrent.max(1),
            rate,
            tokens: rate as f64,
            refilled: None,
            ttl: OWNER_LEASE_TTL,
            generation: 0,
            next_lease: 0,
            slots: 0,
            active: HashMap::new(),
        }
    }

    /// Change the slot count and rate in place. Live leases keep their entries
    /// and lease numbering continues, so a lease admitted before the change
    /// can never match, and release, one admitted after it. The bucket starts
    /// full at the new rate, as in [`Self::new`].
    pub fn set_limits(&mut self, concurrent: usize, rate: u64) {
        self.concurrent = concurrent.max(1);
        self.rate = rate;
        self.tokens = rate as f64;
        self.refilled = None;
    }

    /// Replace the backstop lease lifetime ([`OWNER_LEASE_TTL`]).
    pub fn with_lease_ttl(mut self, ttl: Duration) -> Self {
        self.ttl = ttl;
        self
    }

    /// Fills holding a slot.
    pub fn active(&self) -> usize {
        self.slots
    }

    fn refill(&mut self, now: Instant) {
        if self.rate == 0 {
            return;
        }
        if let Some(last) = self.refilled {
            let elapsed = now.saturating_duration_since(last).as_secs_f64();
            self.tokens = (self.tokens + elapsed * self.rate as f64).min(self.rate as f64);
        }
        self.refilled = Some(now);
    }

    /// Drop entries whose lease expired by `now`, releasing their slots (their tokens stay
    /// spent). Returns the dropped keys.
    pub fn expire(&mut self, now: Instant) -> Vec<K> {
        let mut expired = Vec::new();
        let slots = &mut self.slots;
        self.active.retain(|key, entry| {
            if entry.expires > now {
                return true;
            }
            if !entry.promoted {
                *slots -= 1;
            }
            expired.push(key.clone());
            false
        });
        expired
    }

    /// Admit a fill of `bytes` for `key`, or say how long to wait. A key that
    /// is already filling is admitted again without another slot or tokens
    /// (the caller joins the same flight) and takes another reference.
    pub fn admit(
        &mut self,
        key: K,
        bytes: u64,
        now: Instant,
    ) -> Result<OwnerLease, (Deferral, Duration)> {
        self.expire(now);
        let id = self.next_lease;
        if let Some(entry) = self.active.get_mut(&key) {
            self.next_lease += 1;
            entry.holders.insert(id);
            return Ok(OwnerLease {
                generation: entry.generation,
                id,
            });
        }
        if self.slots >= self.concurrent {
            return Err((Deferral::Concurrency, Duration::from_millis(100)));
        }
        self.refill(now);
        if self.rate != 0 {
            let need = bytes.min(self.rate) as f64;
            if self.tokens < need {
                let wait = (need - self.tokens) / self.rate as f64;
                return Err((
                    Deferral::Throttle,
                    // Debt from a huge page can imply an unrepresentable
                    // wait; cap it so callers can add it to an `Instant`.
                    Duration::try_from_secs_f64(wait)
                        .unwrap_or(MAX_THROTTLE_WAIT)
                        .clamp(Duration::from_millis(1), MAX_THROTTLE_WAIT),
                ));
            }
            // Charge the whole page; the balance may go negative (debt).
            self.tokens -= bytes as f64;
        }
        self.generation += 1;
        self.next_lease += 1;
        let generation = self.generation;
        self.active.insert(
            key,
            OwnerEntry {
                bytes,
                holders: BTreeSet::from([id]),
                generation,
                promoted: false,
                expires: now.checked_add(self.ttl).unwrap_or(now),
            },
        );
        self.slots += 1;
        Ok(OwnerLease { generation, id })
    }

    /// Return one admission's reference. Returns true if this released the
    /// fill's slot: it was the last reference, and the fill was neither
    /// promoted nor expired.
    pub fn finish(&mut self, key: &K, lease: OwnerLease) -> bool {
        let Some(entry) = self.active.get_mut(key) else {
            return false;
        };
        if entry.generation != lease.generation {
            return false;
        }
        if !entry.holders.remove(&lease.id) || !entry.holders.is_empty() {
            return false;
        }
        let promoted = entry.promoted;
        self.active.remove(key);
        if !promoted {
            self.slots -= 1;
        }
        !promoted
    }

    /// A demand read joined `key`'s flight: release its slot and refund its
    /// tokens. The fill's references stay until they finish. Returns whether
    /// it was an active prefetch fill.
    pub fn promote(&mut self, key: &K) -> bool {
        match self.active.get_mut(key) {
            Some(entry) if !entry.promoted => {
                entry.promoted = true;
                self.slots -= 1;
                if self.rate != 0 {
                    // Refund exactly what admit charged, capped at a full bucket.
                    self.tokens = (self.tokens + entry.bytes as f64).min(self.rate as f64);
                }
                true
            }
            _ => false,
        }
    }

    /// Whether `key` holds a slot.
    pub fn is_active(&self, key: &K) -> bool {
        self.active.get(key).is_some_and(|entry| !entry.promoted)
    }

    /// Whether any admission still references `key` (holding a slot or promoted).
    pub fn holds(&self, key: &K) -> bool {
        self.active.contains_key(key)
    }
}

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod tests {
    use super::*;

    #[test]
    fn set_limits_keeps_live_leases_and_their_numbering() {
        let now = Instant::now();
        let mut owner = OwnerBudget::new(1, 0);
        let old = owner.admit(1u8, 10, now).unwrap();
        owner.set_limits(2, 0);
        // The old fill still holds its slot under the new limits.
        assert!(owner.holds(&1));
        assert_eq!(owner.active(), 1);
        let other = owner.admit(2u8, 10, now).unwrap();
        assert_ne!(other.id(), old.id());
        assert!(owner.admit(3u8, 10, now).is_err(), "two slots in use");
        // Finishing the old lease releases only its own fill.
        assert!(owner.finish(&1, old));
        assert!(owner.holds(&2));
        assert_eq!(owner.active(), 1);
        // A lower limit defers new fills until live ones finish.
        owner.set_limits(0, 100);
        assert_eq!(
            owner.admit(4u8, 10, now).unwrap_err().0,
            Deferral::Concurrency
        );
        assert!(owner.finish(&2, other));
        assert!(owner.admit(4u8, 10, now).is_ok());
    }

    #[test]
    fn owner_leases_are_unique_and_finish_once() {
        let now = Instant::now();
        let mut owner = OwnerBudget::new(1, 0);
        let a = owner.admit(1u8, 10, now).unwrap();
        let b = owner.admit(1u8, 10, now).unwrap();
        assert_ne!(a.id(), b.id());
        // Finishing `a` twice must not drop `b`'s reference.
        assert!(!owner.finish(&1, a));
        assert!(!owner.finish(&1, a));
        assert!(owner.holds(&1));
        assert_eq!(owner.active(), 1);
        assert!(owner.finish(&1, b));
        assert!(!owner.holds(&1));
        assert_eq!(owner.active(), 0);
        assert!(!owner.finish(&1, b));
    }

    #[test]
    fn owner_lease_expiry_releases_slot_and_ignores_late_finish() {
        let now = Instant::now();
        let ttl = Duration::from_secs(5);
        let mut owner = OwnerBudget::new(1, 0).with_lease_ttl(ttl);
        let old = owner.admit(1u8, 10, now).unwrap();
        assert!(
            owner
                .expire(now + ttl - Duration::from_millis(1))
                .is_empty()
        );
        assert_eq!(owner.expire(now + ttl), vec![1]);
        assert_eq!(owner.active(), 0);
        // A new fill of the same key; the expired lease cannot release it.
        let new = owner.admit(1u8, 10, now + ttl).unwrap();
        assert!(!owner.finish(&1, old));
        assert!(owner.is_active(&1));
        assert!(owner.finish(&1, new));
    }

    #[test]
    fn priorities_round_trip_and_order() {
        for priority in Priority::ALL {
            assert_eq!(Priority::parse(priority.as_str()), Some(priority));
        }
        assert!(Priority::High < Priority::Normal && Priority::Normal < Priority::Low);
        assert_eq!(Priority::parse("urgent"), None);
        assert_eq!(Priority::default(), Priority::Normal);
    }

    #[test]
    fn page_lists_are_ascending_unique_and_bounded() {
        let pages = [0, 3, 9];
        let bytes = encode_pages(&pages).unwrap();
        assert_eq!(bytes.len(), 24);
        assert_eq!(decode_pages(&bytes).unwrap(), pages);
        assert!(encode_pages(&[]).is_none());
        assert!(encode_pages(&[2, 1]).is_none());
        assert!(encode_pages(&[1, 1]).is_none());
        let max = (0..256).collect::<Vec<_>>();
        assert!(encode_pages(&max).is_some());
        let over = (0..257).collect::<Vec<_>>();
        assert!(encode_pages(&over).is_none());
        assert!(decode_pages(&bytes[..23]).is_none());
        assert!(decode_pages(&[]).is_none());
    }

    #[test]
    fn results_round_trip_and_reject_unknown_records() {
        let results = [
            PageResult::Admitted,
            PageResult::Resident,
            PageResult::Deferred {
                retry_after_ms: 250,
            },
            PageResult::Failed(FailCode::Unavailable),
            PageResult::Failed(FailCode::NotResident),
        ];
        let bytes = encode_results(&results);
        assert_eq!(bytes.len(), 25);
        assert_eq!(&bytes[10..15], &[2, 0, 0, 0, 250]);
        assert_eq!(decode_results(&bytes).unwrap(), results);
        for bad in [
            vec![4, 0, 0, 0, 0],
            vec![0, 0, 0, 0, 1],
            vec![1, 0, 0, 0, 1],
            vec![3, 0, 0, 0, 7],
            vec![0, 0, 0, 0],
            vec![],
        ] {
            assert!(decode_results(&bad).is_none(), "{bad:?}");
        }
    }

    #[test]
    fn probe_and_fill_results_are_distinct() {
        let probe = [
            PageResult::Resident,
            PageResult::Failed(FailCode::NotResident),
        ];
        assert!(results_answer(&probe, 2, true));
        assert!(!results_answer(&probe, 2, false));
        assert!(!results_answer(&probe, 3, true));
        assert!(!results_answer(&[PageResult::Admitted], 1, true));
        assert!(results_answer(
            &[
                PageResult::Admitted,
                PageResult::Deferred { retry_after_ms: 1 }
            ],
            2,
            false
        ));
    }

    #[test]
    fn backoff_doubles_caps_and_honors_retry_after() {
        assert_eq!(backoff(1, 1.0, None), Duration::from_millis(100));
        assert_eq!(backoff(2, 1.0, None), Duration::from_millis(200));
        assert_eq!(backoff(4, 1.0, None), Duration::from_millis(800));
        assert_eq!(backoff(30, 1.0, None), Duration::from_secs(10));
        assert_eq!(backoff(3, 0.5, None), Duration::from_millis(200));
        assert_eq!(backoff(3, 0.0, None), Duration::ZERO);
        assert_eq!(
            backoff(1, 1.0, Some(Duration::from_secs(2))),
            Duration::from_secs(2)
        );
        assert_eq!(
            backoff(5, 1.0, Some(Duration::from_millis(1))),
            Duration::from_millis(1600)
        );
    }

    #[test]
    fn owner_budget_bounds_concurrency_and_promotes() {
        let now = Instant::now();
        let mut budget = OwnerBudget::new(2, 0);
        let one = budget.admit(1, 16, now).unwrap();
        assert!(budget.admit(2, 16, now).is_ok());
        // Joining an active key takes no slot.
        let joined = budget.admit(1, 16, now).unwrap();
        assert_eq!(
            budget.admit(3, 16, now).unwrap_err().0,
            Deferral::Concurrency
        );
        assert!(budget.promote(&1));
        assert!(
            !budget.finish(&1, one),
            "promotion already released the slot"
        );
        let three = budget.admit(3, 16, now).unwrap();
        assert!(budget.finish(&3, three));
        assert!(!budget.finish(&1, joined));
        assert_eq!(budget.active(), 1);
    }

    #[test]
    fn owner_budget_token_bucket_throttles_and_refills() {
        let start = Instant::now();
        let mut budget = OwnerBudget::new(8, 100);
        assert!(budget.admit(1, 100, start).is_ok());
        let (why, wait) = budget.admit(2, 50, start).unwrap_err();
        assert_eq!(why, Deferral::Throttle);
        assert!(wait > Duration::from_millis(400) && wait <= Duration::from_millis(500));
        assert!(
            budget
                .admit(2, 50, start + Duration::from_millis(500))
                .is_ok()
        );
        // Promotion refunds the promoted fill's tokens.
        assert!(budget.promote(&2));
        assert!(
            budget
                .admit(3, 50, start + Duration::from_millis(500))
                .is_ok()
        );
        // Pages larger than one second of tokens wait for a full bucket only.
        let mut small = OwnerBudget::new(8, 10);
        assert!(small.admit(1, 1000, start).is_ok());
    }

    #[test]
    fn owner_budget_charges_whole_pages_larger_than_the_rate() {
        const MIB: u64 = 1 << 20;
        let start = Instant::now();
        let mut budget = OwnerBudget::new(64, MIB);
        let page = 16 * MIB;
        let mut now = start;
        for key in 0..4u64 {
            loop {
                match budget.admit(key, page, now) {
                    Ok(lease) => {
                        assert!(budget.finish(&key, lease));
                        break;
                    }
                    Err((why, wait)) => {
                        assert_eq!(why, Deferral::Throttle);
                        now += wait;
                    }
                }
            }
        }
        // The first page starts at once; each later one waits 16 s of debt.
        let elapsed = now - start;
        assert!(
            elapsed >= Duration::from_secs(48) && elapsed < Duration::from_millis(48_010),
            "{elapsed:?}"
        );
    }

    #[test]
    fn owner_budget_refunds_whole_pages_without_overflowing() {
        let start = Instant::now();
        let mut budget = OwnerBudget::new(8, 10);
        assert!(budget.admit(1, 1000, start).is_ok());
        let (_, wait) = budget.admit(2, 1, start).unwrap_err();
        assert!(wait >= Duration::from_secs(99), "{wait:?}");
        // Promotion repays the full charge but caps at one second of tokens.
        assert!(budget.promote(&1));
        assert!(budget.admit(2, 10, start).is_ok());
        let (_, wait) = budget.admit(3, 1, start).unwrap_err();
        assert!(wait <= Duration::from_millis(100), "{wait:?}");
        assert!(!budget.promote(&1), "double promotion refunds nothing");
    }

    #[test]
    fn owner_budget_edge_cases() {
        let start = Instant::now();
        // Rate 0 turns throttling off, even for huge pages.
        let mut off = OwnerBudget::new(8, 0);
        for key in 0..8u64 {
            assert!(off.admit(key, u64::MAX, start).is_ok());
        }
        // A huge page and a huge rate stay finite and refund cleanly.
        let mut huge = OwnerBudget::new(8, 1 << 40);
        assert!(huge.admit(1, u64::MAX, start).is_ok());
        let (_, wait) = huge.admit(2, 1, start).unwrap_err();
        assert_eq!(wait, MAX_THROTTLE_WAIT);
        let mut tiny = OwnerBudget::new(8, 1);
        assert!(tiny.admit(1, u64::MAX, start).is_ok());
        assert_eq!(tiny.admit(2, 1, start).unwrap_err().1, MAX_THROTTLE_WAIT);
        assert!(huge.promote(&1));
        assert!(huge.admit(2, 1 << 40, start).is_ok());
        // Long idle refills only to one second of tokens.
        let mut idle = OwnerBudget::new(8, 10);
        assert!(idle.admit(1, 10, start).is_ok());
        let later = start + Duration::from_secs(1_000_000);
        assert!(idle.admit(2, 10, later).is_ok());
        assert!(idle.admit(3, 1, later).is_err());
    }

    /// A second admission of an active key holds a reference: the first
    /// holder finishing must not release the slot under the second.
    #[test]
    fn owner_budget_same_key_admissions_are_refcounted() {
        let now = Instant::now();
        let mut budget = OwnerBudget::new(1, 0);
        let first = budget.admit(1, 16, now).unwrap();
        let second = budget.admit(1, 16, now).unwrap();
        assert!(!budget.finish(&1, first));
        assert!(budget.is_active(&1));
        assert_eq!(budget.active(), 1);
        assert_eq!(
            budget.admit(2, 16, now).unwrap_err().0,
            Deferral::Concurrency
        );
        assert!(budget.finish(&1, second));
        assert_eq!(budget.active(), 0);
        assert!(!budget.holds(&1));
        // A lease finished twice is ignored once its entry is gone.
        assert!(!budget.finish(&1, second));
        assert!(budget.admit(2, 16, now).is_ok());
    }

    /// A leaked lease expires and gives its slot back; its stale lease cannot
    /// release a later fill of the same key.
    #[test]
    fn owner_budget_leases_expire() {
        let now = Instant::now();
        let ttl = Duration::from_secs(5);
        let mut budget = OwnerBudget::new(1, 0).with_lease_ttl(ttl);
        let leaked = budget.admit(1, 16, now).unwrap();
        assert!(budget.admit(2, 16, now + ttl / 2).is_err());
        assert_eq!(budget.expire(now + ttl / 2), Vec::<i32>::new());
        let fresh = budget.admit(1, 16, now + ttl).unwrap();
        assert_ne!(fresh, leaked);
        assert!(!budget.finish(&1, leaked));
        assert!(budget.is_active(&1));
        // Promoted entries expire too.
        assert!(budget.promote(&1));
        assert_eq!(budget.expire(now + 3 * ttl), vec![1]);
        assert!(!budget.finish(&1, fresh));
        assert_eq!(budget.active(), 0);
    }
}
