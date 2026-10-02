//! Caller-synchronized bounded records. Clone under the caller's lock, then
//! iterate and format the snapshot after releasing that lock.

#[derive(Clone)]
pub struct Ring<T: Copy, const N: usize> {
    entries: [Option<(u64, T)>; N],
    total: u64,
    next: usize,
    len: usize,
}

impl<T: Copy, const N: usize> Default for Ring<T, N> {
    fn default() -> Self {
        assert!(N > 0, "ring capacity must be positive");
        Self {
            entries: [None; N],
            total: 0,
            next: 0,
            len: 0,
        }
    }
}

impl<T: Copy, const N: usize> Ring<T, N> {
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

    /// Retained sequence numbers and records, oldest first, even after saturation.
    pub fn iter(&self) -> impl ExactSizeIterator<Item = (u64, T)> + '_ {
        (0..self.len).map(|offset| {
            let index = (self.next + N - self.len + offset) % N;
            self.entries[index].expect("retained ring entry")
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

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
