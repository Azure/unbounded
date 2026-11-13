//! Known-length progress prediction and bounded synchronous reclamation.

use std::time::{Duration, Instant};

/// A local completion prediction, independent of request authority and timers.
pub struct ProgressBudget {
    observation: Duration,

    complete_by: Instant,

    first: Option<(Instant, usize)>,

    expired: bool,
}

impl ProgressBudget {
    /// Start observing only when the caller reports its first body sample.
    pub fn new(observation: Duration, complete_by: Instant) -> Self {
        Self {
            observation,
            complete_by,
            first: None,
            expired: false,
        }
    }

    /// Check sticky expiry and the active body's fixed completion boundary.
    pub fn expired(&self, now: Instant) -> bool {
        self.expired || (self.first.is_some() && now >= self.complete_by)
    }

    /// Report validated cumulative progress; return false when prediction expires.
    /// The caller checks cancellation and validates `received <= total` first.
    pub fn advance(&mut self, now: Instant, received: usize, total: usize) -> bool {
        assert!(received <= total, "validated progress geometry");
        if self.expired {
            return false;
        }
        if received == total {
            self.first = None;
            return true;
        }
        let (first, initial) = *self.first.get_or_insert((now, received));
        let elapsed = now.saturating_duration_since(first).as_nanos();
        let delivered = received.saturating_sub(initial) as u128;
        let remaining = (total - received) as u128;
        let available = self.complete_by.saturating_duration_since(now).as_nanos();
        if now >= self.complete_by
            || (elapsed >= self.observation.as_nanos()
                && remaining.saturating_mul(elapsed) > delivered.saturating_mul(available))
        {
            self.expired = true;
            return false;
        }
        true
    }
}

/// Retry only after a policy-approved synchronous reclamation quantum.
/// Zero reclaimed bytes need not mean exhaustion. The hook decides whether a
/// retry is useful; the fixed quantum count prevents an unbounded busy scan.
pub fn reclaim_retry<T, E>(
    quanta: usize,
    mut reserve: impl FnMut() -> Result<T, E>,
    mut reclaim: impl FnMut(&E) -> bool,
) -> Result<T, E> {
    let mut result = reserve();
    for _ in 0..quanta {
        let Err(error) = &result else { break };
        if !reclaim(error) {
            break;
        }
        result = reserve();
    }
    result
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::cell::Cell;

    /// Prediction starts at first bytes, permits healthy completion, and is sticky.
    #[test]
    fn first_sample_completion_and_sticky_expiry() {
        let start = Instant::now();
        let mut healthy =
            ProgressBudget::new(Duration::from_secs(1), start + Duration::from_secs(10));
        assert!(!healthy.expired(start + Duration::from_secs(5)));
        assert!(healthy.advance(start + Duration::from_secs(5), 10, 100));
        assert!(healthy.advance(start + Duration::from_secs(6), 50, 100));
        assert!(healthy.advance(start + Duration::from_secs(7), 100, 100));
        assert!(!healthy.expired(start + Duration::from_secs(20)));
        let mut slow = ProgressBudget::new(Duration::from_secs(1), start + Duration::from_secs(10));
        assert!(slow.advance(start, 1, usize::MAX));
        assert!(slow.advance(start, 2, usize::MAX));
        assert!(!slow.advance(start + Duration::from_secs(1), 3, usize::MAX));
        assert!(!slow.advance(start + Duration::from_secs(2), usize::MAX, usize::MAX));
    }

    /// Reclamation is bounded, preserves exact failures, and stops after success.
    #[test]
    fn retry_bounds_and_error_identity() {
        let attempts = Cell::new(0);
        let scans = Cell::new(0);
        let result = reclaim_retry(
            2,
            || {
                attempts.set(attempts.get() + 1);
                Err::<(), _>(7)
            },
            |error| {
                assert_eq!(*error, 7);
                scans.set(scans.get() + 1);
                true
            },
        );
        assert_eq!(result, Err(7));
        assert_eq!((attempts.get(), scans.get()), (3, 2));
        assert_eq!(reclaim_retry(2, || Err::<(), _>(9), |_| false), Err(9));
        assert_eq!(
            reclaim_retry::<_, ()>(2, || Ok(3), |_| panic!("success cannot reclaim")),
            Ok(3)
        );
        assert_eq!(
            reclaim_retry(0, || Err::<(), _>(4), |_| panic!("zero budget")),
            Err(4)
        );
    }
}
