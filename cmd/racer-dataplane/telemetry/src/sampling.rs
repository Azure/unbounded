//! Serialized, single-owner sampling admission with caller-owned limits and time.

use std::time::{Duration, Instant};

#[derive(Clone, Copy, Debug)]
pub struct SampleLimits {
    pub total: u64,
    pub duration: Duration,
    pub interval: Duration,
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct SampleCounts {
    pub eligible: u64,
    pub sampled: u64,
    pub skipped: u64,
    pub busy: bool,
}

/// Protect this state with the same lock as the caller's sample publication.
/// The first eligible attempt starts the window, including rejected attempts.
pub struct SampleBudget {
    limits: SampleLimits,
    first: Option<Instant>,
    last: Option<Instant>,
    counts: SampleCounts,
}

impl SampleBudget {
    pub fn new(limits: SampleLimits) -> Self {
        Self {
            limits,
            first: None,
            last: None,
            counts: SampleCounts::default(),
        }
    }

    /// Admit an eligible attempt, returning its one-based sequence number.
    /// The interval boundary is inclusive; the total-duration boundary is not.
    /// Backward time saturates to zero elapsed time rather than panicking.
    pub fn acquire(&mut self, now: Instant) -> Option<u64> {
        self.counts.eligible = self.counts.eligible.saturating_add(1);
        let first = *self.first.get_or_insert(now);
        if self.counts.busy
            || self.counts.sampled >= self.limits.total
            || now.saturating_duration_since(first) >= self.limits.duration
            || self
                .last
                .is_some_and(|last| now.saturating_duration_since(last) < self.limits.interval)
        {
            self.counts.skipped = self.counts.skipped.saturating_add(1);
            return None;
        }
        self.counts.busy = true;
        self.last = Some(now);
        self.counts.sampled += 1;
        Some(self.counts.sampled)
    }

    /// Release the active owner, including on abandoned work. Does not refund a
    /// sample or reset the interval/window. Call only for a successful acquire.
    pub fn release(&mut self) {
        self.counts.busy = false;
    }

    pub fn counts(&self) -> SampleCounts {
        self.counts
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn limits() -> SampleLimits {
        SampleLimits {
            total: 3,
            duration: Duration::from_secs(10),
            interval: Duration::from_secs(2),
        }
    }

    #[test]
    fn owner_interval_and_total_are_independent_bounds() {
        let now = Instant::now();
        let mut budget = SampleBudget::new(limits());
        assert_eq!(budget.acquire(now), Some(1));
        assert_eq!(budget.acquire(now + Duration::from_secs(2)), None);
        budget.release();
        assert_eq!(budget.acquire(now - Duration::from_secs(1)), None);
        assert_eq!(
            budget.acquire(now + Duration::from_secs(2) - Duration::from_nanos(1)),
            None
        );
        assert_eq!(budget.acquire(now + Duration::from_secs(2)), Some(2));
        budget.release();
        assert_eq!(budget.acquire(now + Duration::from_secs(4)), Some(3));
        budget.release();
        assert_eq!(budget.acquire(now + Duration::from_secs(6)), None);
        assert_eq!(
            budget.counts(),
            SampleCounts {
                eligible: 7,
                sampled: 3,
                skipped: 4,
                busy: false
            }
        );
    }

    #[test]
    fn window_expires_exactly_and_zero_limits_disable_sampling() {
        let now = Instant::now();
        let mut budget = SampleBudget::new(SampleLimits {
            interval: Duration::ZERO,
            ..limits()
        });
        assert_eq!(budget.acquire(now), Some(1));
        budget.release();
        assert_eq!(
            budget.acquire(now + Duration::from_secs(10) - Duration::from_nanos(1)),
            Some(2)
        );
        budget.release();
        assert_eq!(budget.acquire(now + Duration::from_secs(10)), None);
        for limits in [
            SampleLimits {
                total: 0,
                ..limits()
            },
            SampleLimits {
                duration: Duration::ZERO,
                ..limits()
            },
        ] {
            let mut budget = SampleBudget::new(limits);
            assert_eq!(budget.acquire(now), None);
            assert_eq!(budget.first, Some(now));
            assert_eq!(
                budget.counts(),
                SampleCounts {
                    eligible: 1,
                    skipped: 1,
                    ..SampleCounts::default()
                }
            );
        }
    }

    #[test]
    fn counters_saturate_without_wrapping_sequence() {
        let now = Instant::now();
        let mut budget = SampleBudget::new(SampleLimits {
            total: u64::MAX,
            interval: Duration::ZERO,
            ..limits()
        });
        budget.counts = SampleCounts {
            eligible: u64::MAX,
            sampled: u64::MAX - 1,
            skipped: u64::MAX,
            busy: false,
        };
        assert_eq!(budget.acquire(now), Some(u64::MAX));
        budget.release();
        assert_eq!(budget.acquire(now), None);
        assert_eq!(
            budget.counts(),
            SampleCounts {
                eligible: u64::MAX,
                sampled: u64::MAX,
                skipped: u64::MAX,
                busy: false
            }
        );
    }
}
