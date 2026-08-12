//! A rate limiter for a single device, metering operations and bytes at once.
//!
//! Storage that meters a client and then queues, delays, or fails what exceeds the
//! meter is best served by never exceeding it. Pacing our own submissions costs a
//! bounded, predictable wait; overshooting costs an unbounded one, applied by someone
//! else, to whichever IO happens to be in flight. So the limiter is a governor, not a
//! safety net: it is set below what the device will actually take.
//!
//! Two resources are metered independently, because a device is sold with a cap on
//! each: operations per second, and bytes per second. Either can bind first — small
//! writes exhaust operations while the byte budget idles, a huge page does the reverse
//! — so an IO waits for whichever clears later.
//!
//! The mechanism is a virtual clock per resource, holding the time at which the budget
//! spent so far will have been earned back. An arrival reads the clock, takes its slot,
//! and pushes the clock out by what it costs; the wait is the distance from now to that
//! slot. Slots are handed out in arrival order and each caller waits for its own, so
//! there is no queue to manage, no wakeup storm when the budget frees, and no
//! starvation. Letting a clock fall at most `BURST` behind real time is what lets an
//! idle device absorb a spike instead of pacing it.
//!
//! `admit` never sleeps. It returns how long to wait and leaves the waiting to the
//! caller, which is what lets one implementation serve both the async runtime, where a
//! wait is a timer op, and the startup threads, where it is `thread::sleep`.

use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;

/// The unit a transfer is metered in: anything larger is charged as several operations,
/// so a 4 MiB page does not pass for one. Sized to the largest transfer that is
/// generally counted singly.
const IO_UNIT: u64 = 256 << 10;

/// How far a clock may lag real time, and so the credit an idle device banks. Large
/// enough that a burst of metadata writes after a quiet spell goes out without pacing,
/// small enough that the average is still held over any interval worth measuring.
const BURST: Duration = Duration::from_millis(10);

/// Waits below this are not worth the timer op they would cost. Skipping one does not
/// leak budget: the clock has already moved, so the next arrival waits that much longer.
const SLACK: Duration = Duration::from_micros(50);

/// How far ahead the budget must be committed before optional work should stand down.
/// Well beyond `SLACK`, so ordinary pacing does not read as pressure.
const PRESSED: Duration = Duration::from_millis(2);

/// Nanoseconds since an arbitrary fixed point, shared by every caller on the node.
#[cfg(not(feature = "sim"))]
fn now_ns() -> u64 {
    use std::sync::OnceLock;
    use std::time::Instant;
    static BASE: OnceLock<Instant> = OnceLock::new();
    BASE.get_or_init(Instant::now).elapsed().as_nanos() as u64
}

/// Simulated time. Without this the limiter would meter virtual IO against real time,
/// see a device that is idle by every measure, and never pace anything.
#[cfg(feature = "sim")]
fn now_ns() -> u64 {
    crate::sim::now_us() * 1_000
}

pub(crate) struct Limiter {
    /// The two rates, per second. Zero means the resource is unmetered. Fixed for the
    /// life of the device, like the geometry they were chosen for.
    iops: u64,
    bytes_per_sec: u64,
    /// The virtual clocks: when the budget spent so far will have been earned back.
    ops_at: AtomicU64,
    bytes_at: AtomicU64,
    waited_us: AtomicU64,
}

impl Limiter {
    /// A limit of zero on either resource leaves that resource unmetered; zero on both
    /// makes `admit` a single predictable branch.
    pub(crate) fn new(iops: u64, bytes_per_sec: u64) -> Limiter {
        Limiter {
            iops,
            bytes_per_sec,
            ops_at: AtomicU64::new(0),
            bytes_at: AtomicU64::new(0),
            waited_us: AtomicU64::new(0),
        }
    }

    /// Claim budget for a transfer of `len` bytes and report how long to wait before
    /// issuing it. The budget is spent whether or not the caller waits, so a caller that
    /// asks must go on to issue.
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

    /// Whether the budget is committed far enough ahead that work which can be dropped
    /// should be. Reads the clocks without spending anything.
    pub(crate) fn pressed(&self) -> bool {
        if self.unmetered() {
            return false;
        }
        let now = now_ns();
        let ahead = |c: &AtomicU64| c.load(Ordering::Relaxed).saturating_sub(now);
        ahead(&self.ops_at).max(ahead(&self.bytes_at)) > PRESSED.as_nanos() as u64
    }

    /// Total time callers have been told to wait, in microseconds. The one number that
    /// says whether the limit is set below what the workload needs.
    pub(crate) fn waited_us(&self) -> u64 {
        self.waited_us.load(Ordering::Relaxed)
    }

    fn unmetered(&self) -> bool {
        self.iops == 0 && self.bytes_per_sec == 0
    }

    /// Advance both clocks and return the wait in nanoseconds. Split out from `admit` so
    /// the arithmetic can be driven by a clock the test controls.
    fn claim(&self, now: u64, len: u64) -> u64 {
        // A clock behind this has been idle longer than the credit we allow it to bank.
        let floor = now.saturating_sub(BURST.as_nanos() as u64);
        // Cost is divided rather than premultiplied because a rounded per-unit price is
        // wrong by several percent at the sizes and rates we care about.
        let cost = |units: u64, rate: u64| (units * 1_000_000_000).checked_div(rate).unwrap_or(0);
        let take = |clock: &AtomicU64, cost: u64| -> u64 {
            if cost == 0 {
                return now;
            }
            // The slot is where the clock stood; the clock moves on by what we spend.
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

    /// Drive `claim` with a clock that only moves when a caller waits, which is the
    /// worst case: a single caller issuing back to back for as long as it takes.
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

    /// Each resource binds on its own: the same limits pace small IO on operations and
    /// large IO on bytes.
    #[test]
    fn the_tighter_resource_binds() {
        let small = drive(&Limiter::new(1_000, 100 << 20), 500, 4096);
        let large = drive(&Limiter::new(1_000, 100 << 20), 500, 1 << 20);
        assert!(large > small * 4, "small {small} large {large}");
    }

    /// A transfer larger than the metering unit costs more than one operation, or an
    /// operation limit would be wrong by the ratio between them.
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

    /// Interleaved callers get distinct slots in arrival order, so none is starved and
    /// none is admitted twice into the same one.
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

    /// The one test that reads the live clock. Under simulation there is no clock to
    /// read outside a running simulation, so it does not apply.
    #[test]
    #[cfg(not(feature = "sim"))]
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
