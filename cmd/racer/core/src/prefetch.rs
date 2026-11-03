//! Sans-IO prefetch rules: priorities, the owner peer-operation page and
//! result records, the owner fill budget, and retry backoff.
//!
//! The owner budget is the prefetch admission class: it bounds concurrent
//! prefetch fills and, optionally, origin bytes per second. A demand read that
//! joins a prefetch flight promotes it, which returns its slot and tokens.

use std::collections::HashMap;
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

/// Owner-side prefetch admission. Each admitted fill holds one slot until it
/// finishes or a demand read promotes it.
#[derive(Debug)]
pub struct OwnerBudget<K> {
    concurrent: usize,
    rate: u64,
    tokens: f64,
    refilled: Option<Instant>,
    active: HashMap<K, u64>,
}

/// Why the owner deferred a fill.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Deferral {
    Concurrency,
    Throttle,
}

impl<K: Eq + Hash + Clone> OwnerBudget<K> {
    /// `rate` is origin bytes per second; zero disables the token bucket. The
    /// bucket holds at most one second of tokens.
    pub fn new(concurrent: usize, rate: u64) -> Self {
        Self {
            concurrent: concurrent.max(1),
            rate,
            tokens: rate as f64,
            refilled: None,
            active: HashMap::new(),
        }
    }

    /// Fills holding a slot.
    pub fn active(&self) -> usize {
        self.active.len()
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

    /// Admit a fill of `bytes` for `key`, or say how long to wait. A key that
    /// is already active is admitted again without another slot (the caller
    /// joins the same flight).
    pub fn admit(&mut self, key: K, bytes: u64, now: Instant) -> Result<(), (Deferral, Duration)> {
        if self.active.contains_key(&key) {
            return Ok(());
        }
        if self.active.len() >= self.concurrent {
            return Err((Deferral::Concurrency, Duration::from_millis(100)));
        }
        self.refill(now);
        if self.rate != 0 {
            let need = bytes.min(self.rate) as f64;
            if self.tokens < need {
                let wait = (need - self.tokens) / self.rate as f64;
                return Err((
                    Deferral::Throttle,
                    Duration::from_secs_f64(wait).max(Duration::from_millis(1)),
                ));
            }
            self.tokens -= need;
        }
        self.active.insert(key, bytes);
        Ok(())
    }

    /// Release the slot of a finished fill. Returns false if it was already
    /// released (finished or promoted).
    pub fn finish(&mut self, key: &K) -> bool {
        self.active.remove(key).is_some()
    }

    /// A demand read joined `key`'s flight: release its slot and refund its
    /// tokens. Returns whether it was an active prefetch fill.
    pub fn promote(&mut self, key: &K) -> bool {
        match self.active.remove(key) {
            Some(bytes) => {
                if self.rate != 0 {
                    self.tokens = (self.tokens + bytes.min(self.rate) as f64).min(self.rate as f64);
                }
                true
            }
            None => false,
        }
    }

    /// Whether `key` holds a slot.
    pub fn is_active(&self, key: &K) -> bool {
        self.active.contains_key(key)
    }
}

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod tests {
    use super::*;

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
        assert!(budget.admit(1, 16, now).is_ok());
        assert!(budget.admit(2, 16, now).is_ok());
        // Joining an active key takes no slot.
        assert!(budget.admit(1, 16, now).is_ok());
        assert_eq!(
            budget.admit(3, 16, now).unwrap_err().0,
            Deferral::Concurrency
        );
        assert!(budget.promote(&1));
        assert!(!budget.finish(&1), "promotion already released the slot");
        assert!(budget.admit(3, 16, now).is_ok());
        assert!(budget.finish(&3));
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
}
