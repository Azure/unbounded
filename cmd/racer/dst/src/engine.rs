//! Discrete-event core: virtual time, an event queue, and the trace digest.
//!
//! Time is `u64` nanoseconds since
//! the start of the run. Events with the same time pop in the order they were
//! scheduled (a sequence number breaks ties), so the queue itself never adds
//! nondeterminism; any reordering the scenario wants must be drawn from
//! [`crate::Choices`]. [`TraceHash`] folds an order-sensitive digest of what
//! the run records; the determinism double-run compares it.
use std::{cmp::Ordering, collections::BinaryHeap};

use crate::choices::mix;

/// Simulated time in nanoseconds.
pub type Nanos = u64;

/// One microsecond.
pub const MICROS: Nanos = 1_000;
/// One millisecond.
pub const MILLIS: Nanos = 1_000_000;
/// One second.
pub const SECONDS: Nanos = 1_000_000_000;

struct Entry<E> {
    at: Nanos,
    seq: u64,
    event: E,
}

impl<E> PartialEq for Entry<E> {
    fn eq(&self, other: &Self) -> bool {
        self.at == other.at && self.seq == other.seq
    }
}

impl<E> Eq for Entry<E> {}

impl<E> PartialOrd for Entry<E> {
    fn partial_cmp(&self, other: &Self) -> Option<Ordering> {
        Some(self.cmp(other))
    }
}

impl<E> Ord for Entry<E> {
    /// Reversed so the max-heap pops the earliest `(time, seq)` first.
    fn cmp(&self, other: &Self) -> Ordering {
        (other.at, other.seq).cmp(&(self.at, self.seq))
    }
}

/// Event queue plus the simulated clock.
pub struct Engine<E> {
    now: Nanos,
    seq: u64,
    heap: BinaryHeap<Entry<E>>,
    processed: u64,
}

impl<E> Default for Engine<E> {
    fn default() -> Self {
        Self::new()
    }
}

impl<E> Engine<E> {
    /// An empty queue at time zero.
    pub fn new() -> Self {
        Self {
            now: 0,
            seq: 0,
            heap: BinaryHeap::new(),
            processed: 0,
        }
    }

    /// Current simulated time.
    pub fn now(&self) -> Nanos {
        self.now
    }

    /// Number of events popped so far.
    pub fn processed(&self) -> u64 {
        self.processed
    }

    /// Number of queued events.
    pub fn pending(&self) -> usize {
        self.heap.len()
    }

    /// Schedule `event` at absolute time `at`; past times clamp to now.
    pub fn at(&mut self, at: Nanos, event: E) {
        let at = at.max(self.now);
        self.seq += 1;
        self.heap.push(Entry {
            at,
            seq: self.seq,
            event,
        });
    }

    /// Schedule `event` after `delay`.
    pub fn after(&mut self, delay: Nanos, event: E) {
        self.at(self.now.saturating_add(delay), event);
    }

    /// Time of the next queued event.
    pub fn next_at(&self) -> Option<Nanos> {
        self.heap.peek().map(|e| e.at)
    }

    /// Pop the next event no later than `end`, advancing the clock to it.
    pub fn pop_until(&mut self, end: Nanos) -> Option<E> {
        if self.heap.peek()?.at > end {
            return None;
        }
        let entry = self.heap.pop()?;
        debug_assert!(entry.at >= self.now, "time went backwards");
        self.now = entry.at;
        self.processed += 1;
        Some(entry.event)
    }

    /// Advance the clock to `end` without popping (never backwards).
    pub fn advance_to(&mut self, end: Nanos) {
        self.now = self.now.max(end);
    }
}

/// Order-sensitive digest of recorded events.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct TraceHash(u64);

impl Default for TraceHash {
    fn default() -> Self {
        Self(0xCBF2_9CE4_8422_2325)
    }
}

impl TraceHash {
    /// Fold one record; `a then b` differs from `b then a`.
    pub fn record(&mut self, time: Nanos, tag: u64, a: u64, b: u64) {
        for word in [time, tag, a, b] {
            self.0 = mix(self.0 ^ word).wrapping_mul(0x0000_0100_0000_01B3);
        }
    }

    /// Fold arbitrary bytes (for example data a role observed).
    pub fn record_bytes(&mut self, time: Nanos, tag: u64, bytes: &[u8]) {
        let mut h = 0xCBF2_9CE4_8422_2325u64;
        for b in bytes {
            h = (h ^ u64::from(*b)).wrapping_mul(0x0000_0100_0000_01B3);
        }
        self.record(time, tag, bytes.len() as u64, h);
    }

    /// The digest.
    pub fn value(&self) -> u64 {
        self.0
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pops_in_time_then_schedule_order() {
        let mut e = Engine::new();
        e.at(10, "c");
        e.at(5, "a");
        e.at(10, "d");
        e.at(5, "b");
        let mut out = Vec::new();
        while let Some(ev) = e.pop_until(u64::MAX) {
            out.push((e.now(), ev));
        }
        assert_eq!(out, vec![(5, "a"), (5, "b"), (10, "c"), (10, "d")]);
        assert_eq!(e.processed(), 4);
    }

    #[test]
    fn respects_end_and_clamps_past() {
        let mut e = Engine::new();
        e.at(100, 1);
        e.at(200, 2);
        assert_eq!(e.pop_until(150), Some(1));
        assert_eq!(e.pop_until(150), None);
        e.at(0, 3);
        assert_eq!(e.pop_until(150), Some(3));
        assert_eq!(e.now(), 100);
        e.advance_to(150);
        e.advance_to(10);
        assert_eq!(e.now(), 150);
        e.after(5, 4);
        assert_eq!(e.pending(), 2);
        assert_eq!(e.next_at(), Some(155));
        assert_eq!(Engine::<u8>::default().next_at(), None);
    }

    #[test]
    fn trace_is_order_and_content_sensitive() {
        let (mut a, mut b) = (TraceHash::default(), TraceHash::default());
        a.record(1, 1, 1, 0);
        a.record(1, 2, 1, 0);
        b.record(1, 2, 1, 0);
        b.record(1, 1, 1, 0);
        assert_ne!(a, b);
        let (mut c, mut d) = (TraceHash::default(), TraceHash::default());
        c.record_bytes(0, 0, b"ab");
        d.record_bytes(0, 0, b"ba");
        assert_ne!(c.value(), d.value());
    }
}
