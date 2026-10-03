//! Caller-synchronized bounded records. Clone under the caller's lock, then
//! iterate and format the snapshot after releasing that lock.

#[derive(Clone)]
pub struct Ring<T, const N: usize> {
    entries: [Option<(u64, T)>; N],
    total: u64,
    next: usize,
    len: usize,
}

impl<T, const N: usize> Default for Ring<T, N> {
    fn default() -> Self {
        assert!(N > 0, "ring capacity must be positive");
        Self {
            entries: std::array::from_fn(|_| None),
            total: 0,
            next: 0,
            len: 0,
        }
    }
}

impl<T, const N: usize> Ring<T, N> {
    /// Append a record with a saturating, one-based sequence number.
    pub fn push(&mut self, value: T) {
        self.total = self.total.saturating_add(1);
        self.entries[self.next] = Some((self.total, value));
        self.next = (self.next + 1) % N;
        self.len = (self.len + 1).min(N);
    }

    pub fn total(&self) -> u64 {
        self.total
    }
    pub fn len(&self) -> usize {
        self.len
    }
    pub fn is_empty(&self) -> bool {
        self.len == 0
    }

    /// Borrow retained records oldest first, even after sequence saturation.
    /// No values are cloned; a cloned handle snapshot still shares its records.
    pub fn iter_refs(&self) -> impl ExactSizeIterator<Item = (u64, &T)> + '_ {
        (0..self.len).map(|offset| {
            let index = (self.next + N - self.len + offset) % N;
            let (sequence, value) = self.entries[index].as_ref().expect("retained ring entry");
            (*sequence, value)
        })
    }
}

impl<T: Copy, const N: usize> Ring<T, N> {
    /// Retained sequence numbers and records, oldest first, even after saturation.
    pub fn iter(&self) -> impl ExactSizeIterator<Item = (u64, T)> + '_ {
        self.iter_refs().map(|(sequence, value)| (sequence, *value))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    };

    #[test]
    fn clone_handles_preserve_snapshot_and_external_owners_after_overwrite() {
        struct Record {
            id: usize,
            value: AtomicUsize,
            drops: Arc<[AtomicUsize; 3]>,
        }
        impl Drop for Record {
            fn drop(&mut self) {
                self.drops[self.id].fetch_add(1, Ordering::SeqCst);
            }
        }

        let drops = Arc::new(std::array::from_fn(|_| AtomicUsize::new(0)));
        let record = |id| {
            Arc::new(Record {
                id,
                value: AtomicUsize::new(id),
                drops: drops.clone(),
            })
        };
        let mut ring = Ring::<Arc<Record>, 2>::default();
        assert!(ring.is_empty());
        assert_eq!(ring.iter_refs().len(), 0);

        // External operation owners outlive eviction from diagnostic retention.
        let work = record(0);
        let ticket = work.clone();
        ring.push(work.clone());
        ring.push(record(1));
        assert_eq!(Arc::strong_count(&work), 3);
        let snapshot = ring.clone();
        assert_eq!(Arc::strong_count(&work), 4);
        let retained = {
            let mut entries = snapshot.iter_refs();
            let (sequence, retained) = entries.next().unwrap();
            assert_eq!(sequence, 1);
            assert!(Arc::ptr_eq(retained, &work));
            assert_eq!(entries.len(), 1);
            assert_eq!(Arc::strong_count(&work), 4, "iteration must not clone");
            retained
        };

        ring.push(record(2));
        assert_eq!(ring.total(), 3);
        assert_eq!(ring.len(), 2);
        assert_eq!(Arc::strong_count(&work), 3, "only ring ownership ends");
        assert_eq!(
            ring.iter_refs()
                .map(|(seq, r)| (seq, r.id))
                .collect::<Vec<_>>(),
            [(2, 1), (3, 2)]
        );
        assert_eq!(snapshot.total(), 2);
        assert_eq!(
            snapshot
                .iter_refs()
                .map(|(seq, r)| (seq, r.id))
                .collect::<Vec<_>>(),
            [(1, 0), (2, 1)]
        );

        // A handle snapshot retains objects, not frozen copies of their contents.
        work.value.store(42, Ordering::SeqCst);
        assert_eq!(ticket.value.load(Ordering::SeqCst), 42);
        assert_eq!(retained.value.load(Ordering::SeqCst), 42);
        drop(work);
        drop(ticket);
        assert_eq!(drops[0].load(Ordering::SeqCst), 0);
        drop(ring);
        assert_eq!(drops[0].load(Ordering::SeqCst), 0);
        assert_eq!(drops[1].load(Ordering::SeqCst), 0);
        assert_eq!(drops[2].load(Ordering::SeqCst), 1);
        drop(snapshot);
        for count in drops.iter() {
            assert_eq!(count.load(Ordering::SeqCst), 1);
        }
    }

    #[test]
    fn overwrite_drops_the_last_owner_immediately() {
        struct Record(Arc<AtomicUsize>);
        impl Drop for Record {
            fn drop(&mut self) {
                self.0.fetch_add(1, Ordering::SeqCst);
            }
        }

        let drops = Arc::new(AtomicUsize::new(0));
        // The record itself is neither Copy nor Clone.
        let mut ring = Ring::<Record, 1>::default();
        ring.push(Record(drops.clone()));
        assert_eq!(drops.load(Ordering::SeqCst), 0);
        ring.push(Record(drops.clone()));
        assert_eq!(drops.load(Ordering::SeqCst), 1);
        assert_eq!(ring.iter_refs().next().unwrap().0, 2);
        drop(ring);
        assert_eq!(drops.load(Ordering::SeqCst), 2);
    }

    #[test]
    fn single_entry_and_saturated_sequence_keep_insertion_order() {
        let mut one = Ring::<u8, 1>::default();
        one.push(1);
        one.push(2);
        assert_eq!(one.iter().collect::<Vec<_>>(), [(2, 2)]);
        let mut ring = Ring::<u8, 2>::default();
        ring.total = u64::MAX - 1;
        for value in 1..=4 {
            ring.push(value);
        }
        assert_eq!(ring.total(), u64::MAX);
        assert_eq!(
            ring.iter().collect::<Vec<_>>(),
            [(u64::MAX, 3), (u64::MAX, 4)]
        );
    }

    #[test]
    #[should_panic(expected = "ring capacity must be positive")]
    fn zero_capacity_is_rejected() {
        let _ = Ring::<u8, 0>::default();
    }
}
