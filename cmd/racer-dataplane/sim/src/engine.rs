//! Deterministic discrete-event core.
//!
//! Time is `u64` nanoseconds since the start of the run. Events with the same
//! time run in the order they were scheduled (a monotonically increasing
//! sequence number breaks ties), so a run is a pure function of its seed and
//! configuration. [`TraceHash`] folds an order-sensitive digest of the events
//! a scenario chooses to record; two runs with the same inputs must produce the
//! same hash (see the replay tests).

use std::cmp::Ordering;
use std::collections::BinaryHeap;

use racer_core::time::SplitMix64;

/// Simulated time in nanoseconds.
pub type Nanos = u64;

pub const MICROS: Nanos = 1_000;
pub const MILLIS: Nanos = 1_000_000;
pub const SECONDS: Nanos = 1_000_000_000;

/// Deterministic hasher for simulator maps (fixed-key SipHash).
pub type Fixed = std::hash::BuildHasherDefault<std::collections::hash_map::DefaultHasher>;

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
    // Reversed so the max-heap pops the earliest (time, seq) first.
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
    pub trace: TraceHash,
}

impl<E> Default for Engine<E> {
    fn default() -> Self {
        Self::new()
    }
}

impl<E> Engine<E> {
    pub fn new() -> Self {
        Self {
            now: 0,
            seq: 0,
            heap: BinaryHeap::new(),
            processed: 0,
            trace: TraceHash::default(),
        }
    }

    pub fn now(&self) -> Nanos {
        self.now
    }

    /// Number of events popped so far.
    pub fn processed(&self) -> u64 {
        self.processed
    }

    pub fn pending(&self) -> usize {
        self.heap.len()
    }

    /// Schedule `event` at absolute time `at`. Times in the past are clamped
    /// to now, so causality is never violated.
    pub fn at(&mut self, at: Nanos, event: E) {
        let at = at.max(self.now);
        self.seq += 1;
        self.heap.push(Entry {
            at,
            seq: self.seq,
            event,
        });
    }

    /// Schedule `event` after `delay` from now.
    pub fn after(&mut self, delay: Nanos, event: E) {
        self.at(self.now.saturating_add(delay), event);
    }

    /// Time of the next pending event, if any.
    pub fn next_at(&self) -> Option<Nanos> {
        self.heap.peek().map(|e| e.at)
    }

    /// Pop the next event no later than `end`, advancing the clock.
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

    /// Advance the clock to `end` (after the queue is drained up to it).
    pub fn finish(&mut self, end: Nanos) {
        self.now = self.now.max(end);
    }

    pub fn approximate_bytes(&self) -> usize {
        self.heap.capacity() * std::mem::size_of::<Entry<E>>()
    }
}

/// Cheap order-sensitive digest of recorded events.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct TraceHash(u64);

impl Default for TraceHash {
    fn default() -> Self {
        Self(0xCBF2_9CE4_8422_2325)
    }
}

impl TraceHash {
    /// Fold one record. Order matters: `a then b` differs from `b then a`.
    pub fn record(&mut self, time: Nanos, tag: u64, a: u64, b: u64) {
        for word in [time, tag, a, b] {
            self.0 = mix(self.0 ^ word).wrapping_mul(0x0000_0100_0000_01B3);
        }
    }

    pub fn value(&self) -> u64 {
        self.0
    }
}

/// SplitMix64 finalizer, used as a fast keyed mix.
pub const fn mix(mut z: u64) -> u64 {
    z = (z ^ (z >> 30)).wrapping_mul(0xBF58_476D_1CE4_E5B9);
    z = (z ^ (z >> 27)).wrapping_mul(0x94D0_49BB_1331_11EB);
    z ^ (z >> 31)
}

/// Roles that own an independent random stream. Adding a role never shifts
/// the sequence another role sees.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
#[repr(u64)]
pub enum Role {
    Workload = 1,
    Origin = 2,
    Faults = 3,
    Topology = 4,
    Churn = 5,
    Node = 6,
    Control = 7,
}

/// Derives a deterministic [`SplitMix64`] per (role, index).
#[derive(Clone, Copy, Debug)]
pub struct Streams {
    seed: u64,
}

impl Streams {
    pub const fn new(seed: u64) -> Self {
        Self { seed }
    }

    pub fn stream(&self, role: Role, index: u64) -> SplitMix64 {
        SplitMix64::new(mix(mix(
            self.seed ^ (role as u64).wrapping_mul(0x9E37_79B9_7F4A_7C15)
        ) ^ index))
    }
}

/// Uniform float in `[0, 1)` from 53 random bits.
pub fn unit(rng: &mut SplitMix64) -> f64 {
    (rng.next_u64() >> 11) as f64 * (1.0 / (1u64 << 53) as f64)
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
        e.at(0, 3); // in the past: clamped to now (100)
        assert_eq!(e.pop_until(150), Some(3));
        assert_eq!(e.now(), 100);
        e.finish(150);
        assert_eq!(e.now(), 150);
        assert_eq!(e.pending(), 1);
        assert_eq!(e.next_at(), Some(200));
        assert_eq!(Engine::<u8>::new().next_at(), None);
    }

    #[test]
    fn trace_is_order_sensitive() {
        let (mut a, mut b) = (TraceHash::default(), TraceHash::default());
        a.record(1, 1, 1, 0);
        a.record(1, 2, 1, 0);
        b.record(1, 2, 1, 0);
        b.record(1, 1, 1, 0);
        assert_ne!(a, b);
    }

    #[test]
    fn streams_independent_and_stable() {
        let s = Streams::new(9);
        assert_eq!(
            s.stream(Role::Node, 3).next_u64(),
            s.stream(Role::Node, 3).next_u64()
        );
        assert_ne!(
            s.stream(Role::Node, 3).next_u64(),
            s.stream(Role::Node, 4).next_u64()
        );
        assert_ne!(
            s.stream(Role::Node, 3).next_u64(),
            s.stream(Role::Origin, 3).next_u64()
        );
        let mut r = s.stream(Role::Workload, 0);
        for _ in 0..1000 {
            let u = unit(&mut r);
            assert!((0.0..1.0).contains(&u));
        }
    }
}
