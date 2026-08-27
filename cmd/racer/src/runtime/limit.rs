//! A rate limiter for a single device, metering operations and bytes at once.
//!
//! The limiter is a governor, not a safety net: set below what the device will take,
//! because pacing ourselves costs a bounded wait while overshooting costs an unbounded
//! one applied to whichever IO is in flight. Operations and bytes per second are metered
//! independently since either can bind first, and an IO waits for the later.
//!
//! Each resource has a virtual clock holding the time at which the budget spent so far
//! will have been earned back. An arrival takes the clock as its slot and pushes it out
//! by its cost, so slots go out in arrival order with no queue and no starvation. `admit`
//! never sleeps: it returns the wait, so it serves both the async runtime (a timer op)
//! and the startup threads (`thread::sleep`).

use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;

/// The unit a transfer is metered in: anything larger costs several operations. Sized to
/// the largest transfer devices generally count singly.
const IO_UNIT: u64 = 256 << 10;

/// How far a clock may lag real time, and so the credit an idle device banks. Big enough
/// for a burst after a quiet spell, small enough to hold the average.
const BURST: Duration = Duration::from_millis(10);

/// Below this a wait is not worth a timer op; the clock has moved, so no budget leaks.
const SLACK: Duration = Duration::from_micros(50);

/// How far ahead the budget must be committed before optional work stands down. Well
/// beyond `SLACK`, so ordinary pacing is not pressure.
const PRESSED: Duration = Duration::from_millis(2);

use crate::kernel::now_ns;

pub(crate) struct Limiter {
    /// The two rates, per second. Zero means unmetered; fixed for the device's life.
    iops: u64,
    bytes_per_sec: u64,
    /// The virtual clocks: when the budget spent so far will have been earned back.
    ops_at: AtomicU64,
    bytes_at: AtomicU64,
    waited_us: AtomicU64,
}

impl Limiter {
    /// A limit of zero on either resource leaves that resource unmetered.
    pub(crate) fn new(iops: u64, bytes_per_sec: u64) -> Limiter {
        Limiter {
            iops,
            bytes_per_sec,
            ops_at: AtomicU64::new(0),
            bytes_at: AtomicU64::new(0),
            waited_us: AtomicU64::new(0),
        }
    }

    /// Claim budget for a transfer of `len` bytes and report how long to wait. Budget is
    /// spent whether or not the caller waits, so a caller that asks must go on to issue.
    pub(crate) fn admit(&self, len: u32) -> Option<Duration> {
        if self.unmetered() {
            return None;
        }
        let wait = self.claim(now_ns(), len as u64);
        if wait <= SLACK.as_nanos() as u64 {
            return None;
        }
        self.waited_us.fetch_add(wait / 1_000, Ordering::Relaxed);
        Some(Duration::from_nanos(wait))
    }

    /// Whether the budget is far enough ahead that droppable work should be dropped.
    pub(super) fn pressed(&self) -> bool {
        if self.unmetered() {
            return false;
        }
        let now = now_ns();
        let ahead = |c: &AtomicU64| c.load(Ordering::Relaxed).saturating_sub(now);
        ahead(&self.ops_at).max(ahead(&self.bytes_at)) > PRESSED.as_nanos() as u64
    }

    /// Total time callers have been told to wait, in microseconds.
    pub(super) fn waited_us(&self) -> u64 {
        self.waited_us.load(Ordering::Relaxed)
    }

    fn unmetered(&self) -> bool {
        self.iops == 0 && self.bytes_per_sec == 0
    }

    /// Advance both clocks and return the wait in nanoseconds, on a caller-supplied clock.
    fn claim(&self, now: u64, len: u64) -> u64 {
        // A clock behind this has been idle longer than the credit we allow it to bank.
        let floor = now.saturating_sub(BURST.as_nanos() as u64);
        // Divide, not premultiply: a rounded per-unit price is off by several percent.
        let cost = |units: u64, rate: u64| (units * 1_000_000_000).checked_div(rate).unwrap_or(0);
        let take = |clock: &AtomicU64, cost: u64| -> u64 {
            if cost == 0 {
                return now;
            }
            clock
                .fetch_update(Ordering::Relaxed, Ordering::Relaxed, |at| {
                    Some(at.max(floor) + cost)
                })
                .unwrap_or(now)
                .max(floor)
        };
        let ops = take(&self.ops_at, cost(len.div_ceil(IO_UNIT), self.iops));
        let bytes = take(&self.bytes_at, cost(len, self.bytes_per_sec));
        ops.max(bytes).saturating_sub(now)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Drive `claim` with a clock that only moves when a caller waits: the worst case.
    fn drive(l: &Limiter, ops: u64, len: u64) -> u64 {
        let mut now = 0u64;
        for _ in 0..ops {
            now += l.claim(now, len);
        }
        now
    }

    #[test]
    fn unmetered_is_free() {
        let l = Limiter::new(0, 0);
        assert_eq!(l.admit(4 << 20), None);
        assert!(!l.pressed());
    }

    #[test]
    fn holds_the_operation_rate() {
        let l = Limiter::new(1_000, 0);
        // 10 ms of banked credit, then a millisecond per op.
        let elapsed = drive(&l, 5_000, 4096);
        let rate = 5_000 * 1_000_000_000 / elapsed.max(1);
        assert!((1_000..=1_010).contains(&rate), "rate {rate}");
    }

    #[test]
    fn holds_the_byte_rate() {
        let l = Limiter::new(0, 100 << 20);
        let elapsed = drive(&l, 1_000, 1 << 20);
        let rate = (1_000u64 << 20) * 1_000_000_000 / elapsed.max(1);
        let want = 100u64 << 20;
        assert!(rate.abs_diff(want) < want / 50, "rate {rate}");
    }

    /// Each resource binds on its own: one limit paces small IO on ops, large on bytes.
    #[test]
    fn the_tighter_resource_binds() {
        let small = drive(&Limiter::new(1_000, 100 << 20), 500, 4096);
        let large = drive(&Limiter::new(1_000, 100 << 20), 500, 1 << 20);
        assert!(large > small * 4, "small {small} large {large}");
    }

    /// A transfer larger than the metering unit costs more than one operation.
    #[test]
    fn large_transfers_cost_several_operations() {
        let one = drive(&Limiter::new(1_000, 0), 100, IO_UNIT);
        let sixteen = drive(&Limiter::new(1_000, 0), 100, 16 * IO_UNIT);
        assert_eq!(sixteen / one.max(1), 16);
    }

    /// An idle device absorbs a spike up to the banked credit, and no more.
    #[test]
    fn burst_is_capped() {
        let l = Limiter::new(1_000, 0);
        let now = 10 * BURST.as_nanos() as u64;
        let free = (0..).take_while(|_| l.claim(now, 4096) == 0).count();
        // A millisecond of credit per operation, plus the one landing exactly on now.
        assert_eq!(free, BURST.as_millis() as usize + 1);
    }

    /// Interleaved callers get distinct slots in arrival order, so none is starved.
    #[test]
    fn slots_are_ordered_and_distinct() {
        let l = Limiter::new(1_000, 0);
        let now = 0;
        let mut last = 0;
        for _ in 0..100 {
            let w = l.claim(now, 4096);
            assert!(w >= last, "{w} < {last}");
            last = w;
        }
    }

    /// The one test that reads the live clock, so it does not apply under simulation.
    #[test]
    fn pressure_builds_and_is_not_tripped_by_pacing() {
        let l = Limiter::new(1_000_000, 0);
        assert!(!l.pressed());
        // Far more than the banked credit, so the clocks end up well ahead of real time.
        for _ in 0..200_000 {
            l.admit(4096);
        }
        assert!(l.pressed());
        assert!(l.waited_us() > 0);
    }
}
