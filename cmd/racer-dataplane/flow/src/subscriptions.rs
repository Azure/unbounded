//! Compact interval scheduling and bounded, completion-owned subscription fanout.

use std::{collections::HashMap, hash::Hash, task::Waker};

/// A half-open interval; canonical sets are sorted, disjoint, and nonadjacent.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Interval {
    /// First included item.
    pub start: u64,

    /// First excluded item.
    pub end: u64,
}

/// Check a compact set without expanding any interval into individual items.
pub fn canonical(intervals: &[Interval], maximum: usize) -> bool {
    intervals.len() <= maximum
        && intervals.iter().all(|i| i.start < i.end)
        && intervals.windows(2).all(|w| w[0].end < w[1].start)
}

/// Test membership using the canonical set's sorted end points.
pub fn contains(intervals: &[Interval], item: u64) -> bool {
    let index = intervals.partition_point(|i| i.end <= item);
    intervals.get(index).is_some_and(|i| i.start <= item)
}

/// Subtract completed intervals from compact demand without expanding items.
pub fn outstanding(demand: &[Interval], completed: &[Interval]) -> Vec<Interval> {
    let mut result = Vec::new();
    let mut index = 0;
    for interval in demand {
        let mut start = interval.start;
        while index < completed.len() && completed[index].end <= start {
            index += 1;
        }
        let mut cursor = index;
        while cursor < completed.len() && completed[cursor].start < interval.end {
            let done = completed[cursor];
            if start < done.start {
                result.push(Interval {
                    start,
                    end: done.start,
                });
            }
            start = start.max(done.end);
            cursor += 1;
        }
        if start < interval.end {
            result.push(Interval {
                start,
                end: interval.end,
            });
        }
    }
    result
}

/// Merge one selected item into a completed set. Selection must exclude u64::MAX.
pub fn include(intervals: &[Interval], item: u64) -> Vec<Interval> {
    let mut result = intervals.to_vec();
    result.push(Interval {
        start: item,
        end: item.checked_add(1).expect("half-open selected item"),
    });
    result.sort_unstable_by_key(|i| i.start);
    let mut merged: Vec<Interval> = Vec::with_capacity(result.len());
    for interval in result {
        if let Some(last) = merged.last_mut()
            && last.end >= interval.start
        {
            last.end = last.end.max(interval.end);
            continue;
        }
        merged.push(interval);
    }
    merged
}

/// Sweep endpoints and count distinct voters, preferring the first best point.
/// Each input is a voter identity, whether it is the target, and outstanding
/// intervals. Eligibility remains caller policy; unknown points suspend selection
/// without reserving any work, so callers can compute policy outside their lock.
pub fn select<'a, K: Eq + Hash + 'a>(
    demands: impl IntoIterator<Item = (&'a K, bool, Vec<Interval>)>,
    eligible: impl Fn(u64) -> Option<bool>,
) -> Result<Option<u64>, u64> {
    let mut events = Vec::new();
    for (receiver, target, intervals) in demands {
        for interval in intervals {
            events.push((interval.start, receiver, target, true));
            events.push((interval.end, receiver, target, false));
        }
    }
    events.sort_unstable_by_key(|e| e.0);
    let mut receivers: HashMap<&K, usize> = HashMap::new();
    let mut active = false;
    let mut best = None;
    let mut score = 0;
    let mut cursor = 0;
    while cursor < events.len() {
        let point = events[cursor].0;
        while cursor < events.len() && events[cursor].0 == point {
            let (_, receiver, target, start) = events[cursor];
            if start {
                *receivers.entry(receiver).or_default() += 1;
            } else if let Some(count) = receivers.get_mut(receiver) {
                *count -= 1;
                if *count == 0 {
                    receivers.remove(receiver);
                }
            }
            if target {
                active = start;
            }
            cursor += 1;
        }
        if active && receivers.len() > score && eligible(point).ok_or(point)? {
            best = Some(point);
            score = receivers.len();
        }
    }
    Ok(best)
}

/// One retained response slot, including completed but unconsumed responses.
pub struct Pending<O, V, E> {
    /// Application contract associated with this response.
    pub owner: O,

    /// Current acquisition owner, possibly a promoted follower.
    pub flight: u64,

    /// Nonrenewable caller-supplied expiry in the caller's clock domain.
    pub deadline: u64,

    /// Completed responses retain their slot until consumed or abandoned.
    pub result: Option<Result<V, E>>,

    /// Last registered response notification.
    pub waker: Option<Waker>,

    /// Whether this waiter has an acquisition owner available to take.
    pub promoted: bool,
}

/// Bounded flight and response storage. The caller serializes transactions and
/// invokes returned wakers after releasing its lock. Keys and results carry no
/// authentication or routing assumptions.
pub struct Fanout<K, O, V, E> {
    next: u64,

    flights: HashMap<u64, K>,

    pending: HashMap<u64, Pending<O, V, E>>,
}

impl<K, O, V, E> Default for Fanout<K, O, V, E> {
    /// Start with no consumed identifiers or retained response slots.
    fn default() -> Self {
        Self {
            next: 0,
            flights: HashMap::new(),
            pending: HashMap::new(),
        }
    }
}

impl<K: Eq, O, V, E> Fanout<K, O, V, E> {
    /// Inspect active acquisition owners, including abandoned or expired work.
    pub fn flights(&self) -> &HashMap<u64, K> {
        &self.flights
    }

    /// Inspect bounded response slots without changing ownership.
    pub fn pending(&self) -> &HashMap<u64, Pending<O, V, E>> {
        &self.pending
    }

    /// Mutate response state while the caller holds its transaction lock.
    pub fn pending_mut(&mut self, token: u64) -> Option<&mut Pending<O, V, E>> {
        self.pending.get_mut(&token)
    }

    /// Attach to live work or reserve a unique new acquisition owner. Failure
    /// leaves tokens, capacity and caller credits unchanged.
    pub fn attach(
        &mut self,
        key: K,
        owner: O,
        deadline: u64,
        now: u64,
        max_pending: usize,
        max_flights: usize,
    ) -> Option<(u64, u64, bool)> {
        if self.pending.len() >= max_pending {
            return None;
        }
        let existing = self.flights.iter().find_map(|(&token, candidate)| {
            (candidate == &key
                && self
                    .pending
                    .values()
                    .any(|p| p.flight == token && p.result.is_none() && p.deadline > now))
            .then_some(token)
        });
        if existing.is_none() && self.flights.len() >= max_flights {
            return None;
        }
        let token = self.next.checked_add(1)?;
        self.next = token;
        let flight = existing.unwrap_or(token);
        if existing.is_none() {
            self.flights.insert(flight, key);
        }
        self.pending.insert(
            token,
            Pending {
                owner,
                flight,
                deadline,
                result: None,
                waker: None,
                promoted: false,
            },
        );
        Some((token, flight, existing.is_none()))
    }

    /// Expire responses without releasing any acquisition owner's capacity.
    pub fn expire(&mut self, now: u64, mut error: impl FnMut() -> E, wake: &mut Vec<Waker>) {
        for pending in self.pending.values_mut() {
            if pending.deadline <= now {
                pending.result = Some(Err(error()));
                if let Some(waker) = pending.waker.take() {
                    wake.push(waker);
                }
            }
        }
    }

    /// Consume or abandon exactly one response slot.
    pub fn remove(&mut self, token: u64) -> Option<Pending<O, V, E>> {
        self.pending.remove(&token)
    }

    /// Complete one owner, optionally promoting exactly one retained follower.
    /// Caller policy decides promotion and constructs each response, including
    /// per-owner accounting failures. No authority or credits are synthesized.
    pub fn finish(
        &mut self,
        flight: u64,
        promote: bool,
        mut complete: impl FnMut(u64, &O, &K) -> Result<V, E>,
        wake: &mut Vec<Waker>,
    ) where
        K: Clone,
    {
        let Some(key) = self.flights.remove(&flight) else {
            return;
        };
        let mut tokens: Vec<_> = self
            .pending
            .iter()
            .filter_map(|(&token, p)| (p.flight == flight && p.result.is_none()).then_some(token))
            .collect();
        tokens.sort_unstable();
        if promote && let Some(next) = tokens.iter().copied().find(|token| *token != flight) {
            self.flights.insert(next, key.clone());
            for token in tokens.iter().copied().filter(|token| *token != flight) {
                let pending = self.pending.get_mut(&token).expect("retained follower");
                pending.flight = next;
                pending.promoted = token == next;
                if let Some(waker) = pending.waker.take() {
                    wake.push(waker);
                }
            }
            tokens.retain(|token| *token == flight);
        }
        for token in tokens {
            let pending = self.pending.get_mut(&token).expect("pending token");
            pending.result = Some(complete(token, &pending.owner, &key));
            if let Some(waker) = pending.waker.take() {
                wake.push(waker);
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Canonical demand is compact even at full width; completion merges holes.
    #[test]
    fn compact_intervals_and_distinct_voter_sweep() {
        assert!(!canonical(&[Interval { start: 1, end: 1 }], 64));
        assert!(!canonical(
            &[Interval { start: 1, end: 3 }, Interval { start: 3, end: 4 }],
            64
        ));
        let full = vec![Interval {
            start: 0,
            end: u64::MAX,
        }];
        assert!(canonical(&full, 64));
        assert!(contains(&full, u64::MAX - 1));
        assert!(!contains(&full, u64::MAX));
        let mut completed = Vec::new();
        for item in [4, 2, 3, 1, 0] {
            completed = include(&completed, item);
        }
        assert_eq!(completed, vec![Interval { start: 0, end: 5 }]);
        for end in [10, u64::MAX] {
            assert_eq!(
                outstanding(&[Interval { start: 0, end }], &completed),
                vec![Interval { start: 5, end }]
            );
        }
        let demands = || {
            vec![
                (&"target", true, full.clone()),
                (&"other", false, vec![Interval { start: 8, end: 9 }]),
            ]
        };
        assert_eq!(select(demands(), |_| Some(true)), Ok(Some(8)));
        assert_eq!(select(demands(), |p| (p == 0).then_some(true)), Err(8));
        assert_eq!(select(demands(), |p| Some(p != 8)), Ok(Some(0)));
        // Two contracts from the same voter must not outweigh two distinct voters.
        let duplicate = vec![
            (&"target", true, full),
            (&"duplicate", false, vec![Interval { start: 0, end: 1 }]),
            (&"duplicate", false, vec![Interval { start: 0, end: 1 }]),
            (&"a", false, vec![Interval { start: 8, end: 9 }]),
            (&"b", false, vec![Interval { start: 8, end: 9 }]),
        ];
        assert_eq!(select(duplicate, |_| Some(true)), Ok(Some(8)));
    }

    /// Completion wakes each subscriber after storing its independent outcome.
    #[test]
    fn fanout_wakes_and_keeps_payload_shared_with_independent_errors() {
        use std::sync::{
            Arc,
            atomic::{AtomicUsize, Ordering},
        };
        struct Wake(AtomicUsize);
        impl std::task::Wake for Wake {
            fn wake(self: Arc<Self>) {
                self.0.fetch_add(1, Ordering::SeqCst);
            }
        }
        let count = Arc::new(Wake(AtomicUsize::new(0)));
        let waker = Waker::from(count.clone());
        let mut fanout = Fanout::<u8, u8, Arc<[u8]>, u8>::default();
        let payload: Arc<[u8]> = vec![7, 8].into();
        for owner in 1..=3 {
            let (token, _, _) = fanout.attach(1, owner, 10, 0, 3, 1).unwrap();
            fanout.pending_mut(token).unwrap().waker = Some(waker.clone());
        }
        let mut wakes = Vec::new();
        fanout.finish(
            1,
            false,
            |_, owner, _| {
                if *owner == 3 {
                    Err(9)
                } else {
                    Ok(payload.clone())
                }
            },
            &mut wakes,
        );
        assert_eq!(
            count.0.load(Ordering::SeqCst),
            0,
            "caller wakes outside lock"
        );
        for wake in wakes {
            wake.wake();
        }
        assert_eq!(count.0.load(Ordering::SeqCst), 3);
        for token in 1..=2 {
            let received = fanout.remove(token).unwrap().result.unwrap().unwrap();
            assert!(Arc::ptr_eq(&received, &payload));
        }
        assert_eq!(fanout.remove(3).unwrap().result, Some(Err(9)));
        assert!(fanout.pending().is_empty());
        assert!(fanout.flights().is_empty());
    }

    /// Finished responses retain capacity; expiry does not fence live work.
    #[test]
    fn bounded_fanout_promotion_and_completion() {
        let mut fanout = Fanout::<u8, u8, u8, u8>::default();
        let mut wake = Vec::new();
        assert_eq!(fanout.attach(4, 1, 10, 0, 2, 1), Some((1, 1, true)));
        assert_eq!(fanout.attach(4, 2, 10, 0, 2, 1), Some((2, 1, false)));
        assert_eq!(fanout.attach(4, 3, 10, 0, 2, 1), None);
        fanout.finish(
            1,
            true,
            |_, owner, key| {
                assert_eq!(*key, 4);
                Err(*owner)
            },
            &mut wake,
        );
        assert_eq!(fanout.pending()[&1].result, Some(Err(1)));
        assert!(fanout.pending()[&2].promoted);
        assert_eq!(fanout.flights().len(), 1);
        assert_eq!(fanout.attach(4, 3, 10, 0, 2, 1), None);
        fanout.remove(1);
        fanout.finish(2, false, |_, owner, _| Ok(*owner), &mut wake);
        assert_eq!(fanout.pending()[&2].result, Some(Ok(2)));
        fanout.expire(10, || 9, &mut wake);
        assert_eq!(fanout.remove(2).unwrap().result, Some(Err(9)));
        assert_eq!(fanout.attach(5, 3, 20, 10, 2, 1), Some((3, 3, true)));
        fanout.expire(20, || 9, &mut wake);
        assert_eq!(fanout.flights().len(), 1);
        assert_eq!(fanout.attach(5, 4, 30, 20, 2, 1), None);
        fanout.finish(3, false, |_, _, _| panic!("expired response"), &mut wake);
        fanout.next = u64::MAX;
        assert_eq!(fanout.attach(5, 4, 30, 20, 2, 1), None);
    }
}
