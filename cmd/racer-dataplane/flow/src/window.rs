//! Exact-once credits shared by pending and issued items.
use crate::{Error, Result};
use std::collections::BTreeMap;

/// A bounded set of reservations. Issuing an item does not return capacity;
/// only releasing its exact length after issuance does.
pub struct Window<K: Ord> {
    slots: usize,
    bytes: u64,
    max_item: u64,
    used: u64,
    outstanding: BTreeMap<K, (u64, bool)>,
}

impl<K: Ord> Window<K> {
    pub fn new(slots: usize, bytes: u64, max_item: u64) -> Result<Self> {
        if slots == 0 || bytes == 0 || max_item == 0 {
            return Err(Error::InvalidInput);
        }
        Ok(Self {
            slots,
            bytes,
            max_item,
            used: 0,
            outstanding: BTreeMap::new(),
        })
    }

    /// Whether a valid length fits both limits, independent of item identity.
    pub fn can_reserve(&self, length: u64) -> bool {
        length != 0
            && length <= self.max_item
            && self.outstanding.len() < self.slots
            && length <= self.bytes - self.used
    }

    pub fn reserve(&mut self, key: K, length: u64) -> Result<()> {
        if length == 0 || length > self.max_item {
            return Err(Error::InvalidInput);
        }
        if !self.can_reserve(length) || self.outstanding.contains_key(&key) {
            return Err(Error::Overloaded);
        }
        self.outstanding.insert(key, (length, false));
        self.used += length;
        Ok(())
    }

    pub fn issued(&mut self, key: K) -> Result<()> {
        let entry = self.outstanding.get_mut(&key).ok_or(Error::InvalidInput)?;
        if entry.1 {
            return Err(Error::InvalidInput);
        }
        entry.1 = true;
        Ok(())
    }

    pub fn release(&mut self, key: K, length: u64) -> Result<()> {
        if self.outstanding.get(&key) != Some(&(length, true)) {
            return Err(Error::InvalidInput);
        }
        self.outstanding.remove(&key);
        self.used -= length;
        Ok(())
    }

    pub fn is_empty(&self) -> bool {
        self.outstanding.is_empty()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn validates_configuration_and_item_lengths_without_consuming_credit() {
        for (slots, bytes, max_item) in [(0, 1, 1), (1, 0, 1), (1, 1, 0)] {
            assert!(matches!(
                Window::<String>::new(slots, bytes, max_item),
                Err(Error::InvalidInput)
            ));
        }
        let mut window = Window::new(2, 10, 6).unwrap();
        for length in [0, 7, u64::MAX] {
            assert!(!window.can_reserve(length));
            assert_eq!(window.reserve("item", length), Err(Error::InvalidInput));
            assert!(window.is_empty());
        }
        window.reserve("item", 6).unwrap();
        assert!(!window.is_empty());
        assert!(window.can_reserve(4));
        assert!(!window.can_reserve(5));
    }

    #[test]
    fn pending_and_issued_share_limits_and_release_exactly_once() {
        let mut window = Window::new(2, 10, 10).unwrap();
        window.reserve(String::from("first"), 4).unwrap();
        assert_eq!(window.release("first".into(), 4), Err(Error::InvalidInput));
        window.issued("first".into()).unwrap();
        assert_eq!(window.issued("first".into()), Err(Error::InvalidInput));
        assert_eq!(window.reserve("first".into(), 1), Err(Error::Overloaded));
        window.reserve("second".into(), 6).unwrap();
        assert!(!window.can_reserve(1));
        assert_eq!(window.reserve("third".into(), 1), Err(Error::Overloaded));
        assert_eq!(window.release("first".into(), 3), Err(Error::InvalidInput));
        assert_eq!(window.release("first".into(), 5), Err(Error::InvalidInput));
        assert_eq!(
            window.release("missing".into(), 4),
            Err(Error::InvalidInput)
        );
        assert_eq!(window.issued("missing".into()), Err(Error::InvalidInput));
        assert!(!window.can_reserve(1));
        window.release("first".into(), 4).unwrap();
        assert_eq!(window.release("first".into(), 4), Err(Error::InvalidInput));
        assert!(window.can_reserve(4));
        assert!(!window.can_reserve(5));
        window.issued("second".into()).unwrap();
        window.release("second".into(), 6).unwrap();
        assert!(window.is_empty());
        window.reserve("first".into(), 10).unwrap();
        window.issued("first".into()).unwrap();
        window.release("first".into(), 10).unwrap();
        assert!(window.is_empty());
    }

    #[test]
    fn slot_and_byte_limits_are_independent() {
        let mut slots = Window::new(1, 10, 10).unwrap();
        slots.reserve(1, 1).unwrap();
        assert!(!slots.can_reserve(1));
        assert_eq!(slots.reserve(2, 1), Err(Error::Overloaded));
        slots.issued(1).unwrap();
        assert!(!slots.can_reserve(1));
        slots.release(1, 1).unwrap();
        assert!(slots.can_reserve(10));

        // An item ceiling larger than the byte budget still permits small items.
        let mut bytes = Window::new(100, 3, 10).unwrap();
        assert_eq!(bytes.reserve(1, 4), Err(Error::Overloaded));
        assert!(bytes.is_empty());
        bytes.reserve(1, 2).unwrap();
        assert!(bytes.can_reserve(1));
        assert_eq!(bytes.reserve(2, 2), Err(Error::Overloaded));
        bytes.reserve(2, 1).unwrap();
        assert!(!bytes.can_reserve(1));
    }

    #[test]
    fn full_u64_budget_does_not_overflow_and_generic_keys_need_only_ord() {
        #[derive(Eq, PartialEq, Ord, PartialOrd)]
        struct Key(u8);
        let mut window = Window::new(usize::MAX, u64::MAX, u64::MAX).unwrap();
        window.reserve(Key(1), u64::MAX - 1).unwrap();
        window.reserve(Key(2), 1).unwrap();
        assert!(!window.can_reserve(1));
        assert_eq!(window.reserve(Key(3), 1), Err(Error::Overloaded));
        window.issued(Key(1)).unwrap();
        window.release(Key(1), u64::MAX - 1).unwrap();
        assert!(window.can_reserve(u64::MAX - 1));
        assert!(!window.can_reserve(u64::MAX));
        window.issued(Key(2)).unwrap();
        window.release(Key(2), 1).unwrap();
        assert!(window.is_empty());
        assert!(window.can_reserve(u64::MAX));
    }
}
