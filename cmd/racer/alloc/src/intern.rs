//! Caller-side identity interning for [`DiskIndex`](crate::disk_index::DiskIndex).
//!
//! [`SlotInterner`] maps a page key to a [`PageSlot`] from a fixed arena sized
//! once at startup, storing each key once. The caller releases a slot only
//! when the index no longer stores, writes, or holds it.
//!
//! [`CacheTable`] maps a cache key to a generation-fenced
//! [`CacheKey`](crate::disk_index::CacheKey). Removal can span turns: a removed
//! cache's [`CacheIdx`] stays allocated until
//! [`DiskIndex::remove_cache`](crate::disk_index::DiskIndex::remove_cache)
//! reports it drained, so it is never reused while old entries remain.

use crate::disk_index::{CacheIdx, CacheKey, DiskIndex, MAX_CACHES, PageSlot, Removed};
use crate::{Error, Result};
use std::collections::HashMap;
use std::hash::{BuildHasher, BuildHasherDefault, Hash, Hasher};

const NIL: u32 = u32::MAX;

/// Deterministic 64-bit FxHash-style hasher with a fixed key. Interning must
/// not depend on per-process random state (simulation replays), and the hash
/// is only an index into the slot array, which stays authoritative.
#[derive(Clone, Copy, Debug, Default)]
pub struct FixedHasher(u64);

impl Hasher for FixedHasher {
    fn finish(&self) -> u64 {
        // Final avalanche so low bits depend on every input byte.
        let mut hash = self.0;
        hash ^= hash >> 33;
        hash = hash.wrapping_mul(0xff51_afd7_ed55_8ccd);
        hash ^ (hash >> 33)
    }

    fn write(&mut self, bytes: &[u8]) {
        for chunk in bytes.chunks(8) {
            let mut word = [0; 8];
            word[..chunk.len()].copy_from_slice(chunk);
            self.write_u64(u64::from_le_bytes(word));
        }
    }

    fn write_u64(&mut self, word: u64) {
        self.0 = (self.0.rotate_left(5) ^ word).wrapping_mul(0x517c_c1b7_2722_0a95);
    }
}

/// [`BuildHasher`] for [`FixedHasher`].
pub type FixedState = BuildHasherDefault<FixedHasher>;

/// Hasher for map keys that already are [`FixedHasher`] outputs.
#[derive(Clone, Copy, Debug, Default)]
struct PrehashedHasher(u64);

impl Hasher for PrehashedHasher {
    fn finish(&self) -> u64 {
        self.0
    }

    fn write(&mut self, _: &[u8]) {
        unreachable!("prehashed keys are u64");
    }

    fn write_u64(&mut self, hash: u64) {
        self.0 = hash;
    }
}

/// Fixed-capacity `K <-> PageSlot` arena with a free list.
///
/// Each key is stored once, in the slot array. The lookup map is keyed by a
/// deterministic 64-bit hash of the key (`S`, [`FixedState`] by default) and
/// holds the first slot of a chain; colliding keys chain through `next` and
/// are told apart by comparing against the slot array.
#[derive(Debug)]
pub struct SlotInterner<K, S = FixedState> {
    keys: Vec<Option<K>>,

    next: Vec<u32>,

    heads: HashMap<u64, u32, BuildHasherDefault<PrehashedHasher>>,

    free: Vec<u32>,

    len: usize,

    hasher: S,
}

impl<K: Hash + Eq + Clone, S: BuildHasher + Default> SlotInterner<K, S> {
    /// An empty arena of `capacity` slots (fixed for its lifetime).
    pub fn new(capacity: u32) -> Self {
        let mut keys = Vec::new();
        keys.resize_with(capacity as usize, || None);
        Self {
            keys,
            next: vec![NIL; capacity as usize],
            heads: HashMap::default(),
            free: (0..capacity).rev().collect(),
            len: 0,
            hasher: S::default(),
        }
    }

    /// Slot count fixed at construction.
    pub fn capacity(&self) -> u32 {
        self.keys.len() as u32
    }

    /// Interned keys.
    pub fn len(&self) -> usize {
        self.len
    }

    /// Whether no key is interned.
    pub fn is_empty(&self) -> bool {
        self.len == 0
    }

    fn find(&self, hash: u64, key: &K) -> Option<PageSlot> {
        let mut slot = *self.heads.get(&hash)?;
        while slot != NIL {
            if self.keys[slot as usize].as_ref() == Some(key) {
                return Some(PageSlot(slot));
            }
            slot = self.next[slot as usize];
        }
        None
    }

    /// The slot of `key`, if interned.
    pub fn get(&self, key: &K) -> Option<PageSlot> {
        self.find(self.hasher.hash_one(key), key)
    }

    /// The key interned at `slot`.
    pub fn key(&self, slot: PageSlot) -> Option<&K> {
        self.keys.get(slot.0 as usize)?.as_ref()
    }

    /// The slot of `key`, interning it if needed. `Busy` when the arena is full.
    pub fn intern(&mut self, key: &K) -> Result<PageSlot> {
        let hash = self.hasher.hash_one(key);
        if let Some(slot) = self.find(hash, key) {
            return Ok(slot);
        }
        let slot = self.free.pop().ok_or(Error::Busy)?;
        self.keys[slot as usize] = Some(key.clone());
        let head = self.heads.entry(hash).or_insert(NIL);
        self.next[slot as usize] = *head;
        *head = slot;
        self.len += 1;
        Ok(PageSlot(slot))
    }

    /// Return `slot` to the free list. The caller must have checked that the
    /// index no longer stores, writes, or holds it.
    pub fn release(&mut self, slot: PageSlot) -> Option<K> {
        let key = self.keys.get_mut(slot.0 as usize)?.take()?;
        let hash = self.hasher.hash_one(&key);
        let after = std::mem::replace(&mut self.next[slot.0 as usize], NIL);
        let head = self.heads.get_mut(&hash).expect("interned key chain");
        if *head == slot.0 {
            if after == NIL {
                self.heads.remove(&hash);
            } else {
                *head = after;
            }
        } else {
            let mut prev = *head;
            while self.next[prev as usize] != slot.0 {
                prev = self.next[prev as usize];
            }
            self.next[prev as usize] = after;
        }
        self.free.push(slot.0);
        self.len -= 1;
        Some(key)
    }

    /// Release `slot` if `index` no longer references it. Returns the key when released.
    pub fn release_unused(&mut self, index: &DiskIndex, slot: PageSlot) -> Option<K> {
        if index.references(slot) {
            return None;
        }
        self.release(slot)
    }
}

/// `K -> CacheKey` table whose indices are reused only after draining.
#[derive(Debug)]
pub struct CacheTable<K> {
    live: HashMap<K, CacheKey>,

    draining: Vec<CacheKey>,

    free: Vec<u16>,

    next: u16,
}

impl<K: Hash + Eq + Clone> Default for CacheTable<K> {
    fn default() -> Self {
        Self {
            live: HashMap::new(),
            draining: Vec::new(),
            free: Vec::new(),
            next: 0,
        }
    }
}

impl<K: Hash + Eq + Clone> CacheTable<K> {
    /// The live key of `cache`.
    pub fn get(&self, cache: &K) -> Option<CacheKey> {
        self.live.get(cache).copied()
    }

    /// Live caches.
    pub fn len(&self) -> usize {
        self.live.len()
    }

    /// Whether no cache is live.
    pub fn is_empty(&self) -> bool {
        self.live.is_empty()
    }

    /// Caches removed but not yet drained.
    pub fn draining(&self) -> usize {
        self.draining.len()
    }

    /// The live key of `cache`, adding it if needed. `Busy` once
    /// [`MAX_CACHES`] indices are live or draining.
    pub fn add(&mut self, index: &mut DiskIndex, cache: &K) -> Result<CacheKey> {
        if let Some(key) = self.get(cache) {
            return Ok(key);
        }
        let idx = match self.free.pop() {
            Some(idx) => idx,
            None if usize::from(self.next) < MAX_CACHES => {
                self.next += 1;
                self.next - 1
            }
            None => return Err(Error::Busy),
        };
        match index.add_cache(CacheIdx(idx)) {
            Ok(key) => {
                self.live.insert(cache.clone(), key);
                Ok(key)
            }
            Err(error) => {
                self.free.push(idx);
                Err(error)
            }
        }
    }

    /// Stop serving `cache`; its entries drain through [`Self::drain`].
    pub fn remove(&mut self, cache: &K) -> bool {
        let Some(key) = self.live.remove(cache) else {
            return false;
        };
        self.draining.push(key);
        true
    }

    /// Remove up to `budget` entries of draining caches. A cache's index is
    /// freed only once the index reports it gone. Returns entries removed.
    pub fn drain(
        &mut self,
        index: &mut DiskIndex,
        mut budget: usize,
        mut removed: impl FnMut(PageSlot, Removed),
    ) -> usize {
        let mut count = 0;
        let mut position = 0;
        while position < self.draining.len() {
            let key = self.draining[position];
            let mut taken = 0;
            let done = index
                .remove_cache(key, budget, |slot, entry| {
                    taken += 1;
                    removed(slot, entry);
                })
                .unwrap_or(true);
            count += taken;
            budget -= taken;
            if done {
                self.draining.swap_remove(position);
                self.free.push(key.idx().0);
            } else {
                position += 1;
            }
            if budget == 0 {
                break;
            }
        }
        count
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::disk_index::Monotonic;
    use crate::{Alignment, Segments};

    fn segments() -> Segments {
        let segments = Segments::new(4096);
        segments
            .configure(4096 * 4, 4, Alignment::new(512, 512, 512).unwrap())
            .unwrap();
        segments
    }

    #[test]
    fn slots_intern_once_fill_and_reuse_after_release() {
        let mut slots = SlotInterner::<String>::new(2);
        let a = slots.intern(&"a".into()).unwrap();
        assert_eq!(slots.intern(&"a".into()).unwrap(), a);
        let b = slots.intern(&"b".into()).unwrap();
        assert_ne!(a, b);
        assert_eq!(slots.intern(&"c".into()), Err(Error::Busy));
        assert_eq!(slots.key(a).map(String::as_str), Some("a"));
        assert_eq!(slots.release(a).as_deref(), Some("a"));
        assert_eq!(slots.release(a), None);
        assert_eq!(slots.get(&"a".into()), None);
        assert_eq!(slots.intern(&"c".into()).unwrap(), a);
        assert_eq!((slots.len(), slots.capacity()), (2, 2));
    }

    /// Sends every key to one hash so lookups rely on the slot array.
    #[derive(Default)]
    struct Colliding;

    impl std::hash::Hasher for Colliding {
        fn finish(&self) -> u64 {
            7
        }

        fn write(&mut self, _: &[u8]) {}
    }

    #[test]
    fn colliding_keys_resolve_against_the_slot_array() {
        let mut slots = SlotInterner::<u64, std::hash::BuildHasherDefault<Colliding>>::new(4);
        let ids: Vec<_> = (10..14).map(|key| slots.intern(&key).unwrap()).collect();
        assert_eq!(slots.intern(&99), Err(Error::Busy));
        for (key, id) in (10..14).zip(&ids) {
            assert_eq!(slots.get(&key), Some(*id));
            assert_eq!(slots.key(*id), Some(&key));
        }
        // Remove from the middle, head, and tail of the one chain.
        assert_eq!(slots.release(ids[1]), Some(11));
        assert_eq!(slots.release(ids[3]), Some(13));
        assert_eq!(slots.release(ids[0]), Some(10));
        assert_eq!(slots.get(&11), None);
        assert_eq!(slots.get(&12), Some(ids[2]));
        assert_eq!(slots.len(), 1);
        assert_eq!(slots.release(ids[2]), Some(12));
        assert!(slots.is_empty() && slots.heads.is_empty());
        assert_eq!(slots.release(ids[2]), None);
        for key in 20..24 {
            slots.intern(&key).unwrap();
        }
        assert_eq!(slots.get(&22).map(|slot| slots.key(slot)), Some(Some(&22)));
    }

    #[test]
    fn fixed_hash_is_deterministic() {
        let state = FixedState::default();
        assert_eq!(
            state.hash_one("page"),
            FixedState::default().hash_one("page")
        );
        assert_ne!(state.hash_one(1u64), state.hash_one(2u64));
    }

    #[test]
    fn referenced_slots_are_not_released() {
        let segments = segments();
        let mut index = DiskIndex::new(4, 4, 4).unwrap();
        let mut caches = CacheTable::<&str>::default();
        let cache = caches.add(&mut index, &"c").unwrap();
        let mut slots = SlotInterner::<u64>::new(4);
        let slot = slots.intern(&7).unwrap();
        let ticket = index.reserve(slot, cache).unwrap();
        assert_eq!(slots.release_unused(&index, slot), None);
        let (_lease, writing) = index.append(&segments, ticket, 512).unwrap();
        let written = index.write_complete(writing);
        index.publish(written).unwrap();
        assert_eq!(slots.release_unused(&index, slot), None);
        let hold = index.hold(&[slot], Monotonic(10), Monotonic(0)).unwrap();
        index.remove(slot).unwrap();
        assert_eq!(slots.release_unused(&index, slot), None);
        index.release_hold(hold).unwrap();
        assert_eq!(slots.release_unused(&index, slot), Some(7));
    }

    #[test]
    fn removed_cache_index_is_not_reused_until_drained_across_turns() {
        let segments = segments();
        let mut index = DiskIndex::new(8, 8, 4).unwrap();
        let mut caches = CacheTable::<&str>::default();
        let old = caches.add(&mut index, &"old").unwrap();
        for slot in 0..3 {
            let ticket = index.reserve(PageSlot(slot), old).unwrap();
            let (_lease, writing) = index.append(&segments, ticket, 512).unwrap();
            let written = index.write_complete(writing);
            index.publish(written).unwrap();
        }
        assert!(caches.remove(&"old"));
        assert!(!caches.remove(&"old"));
        let mut gone = Vec::new();
        assert_eq!(caches.drain(&mut index, 2, |slot, _| gone.push(slot)), 2);
        assert_eq!(caches.draining(), 1);
        let other = caches.add(&mut index, &"other").unwrap();
        assert_ne!(other.idx(), old.idx(), "draining index must not be reused");
        assert_eq!(caches.drain(&mut index, 2, |slot, _| gone.push(slot)), 1);
        assert_eq!((caches.draining(), gone.len()), (0, 3));
        let reused = caches.add(&mut index, &"new").unwrap();
        assert_eq!(reused.idx(), old.idx());
        assert_ne!(reused, old, "generation fences the reused index");
        assert_eq!(index.cache_bytes(reused.idx()), 0);
    }

    #[test]
    fn cache_table_is_bounded() {
        let mut index = DiskIndex::new(1, 1, 1).unwrap();
        let mut caches = CacheTable::<usize>::default();
        for cache in 0..MAX_CACHES {
            caches.add(&mut index, &cache).unwrap();
        }
        assert_eq!(caches.add(&mut index, &MAX_CACHES), Err(Error::Busy));
        assert!(caches.remove(&0));
        caches.drain(&mut index, 1, |_, _| {});
        caches.add(&mut index, &MAX_CACHES).unwrap();
    }
}
