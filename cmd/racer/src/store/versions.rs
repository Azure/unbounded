//! Bounded in-memory table of object versions with pages on disk.
//!
//! Disk keys carry only hashes, so this table maps a [`VersionKey`] back to the
//! full version, its metadata, and the page keys seen for it. The table holds at
//! most `capacity` entries. Inserting a new version into a full table evicts the
//! oldest inserted entry (FIFO). Updating an existing entry does not change its
//! age. Evicting an entry only loses the reverse mapping: pages stay on disk and
//! their self-describing record headers re-seed the table on the next read.

use super::key::{VersionKey, version_key};
use crate::error::{Error, Result};
use crate::model::{ObjectVersion, VersionMetadata};
use crate::runtime::HashMap;
use racer_control_wire::KeyId;
use std::collections::BTreeMap;

/// Most distinct page keys recorded for one version. Shared with the
/// checkpoint format, which rejects descriptors carrying more.
pub const MAX_VERSION_KEYS: usize = 256;

/// Full identity and metadata of one disk-resident version.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct VersionEntry {
    pub version: ObjectVersion,
    pub metadata: VersionMetadata,
    /// Distinct page keys that encrypted pages of this version. Small in practice.
    pub key_ids: Vec<KeyId>,
}

impl VersionEntry {
    /// Entry for `metadata.version` with no known page keys.
    pub fn new(metadata: VersionMetadata) -> Self {
        Self {
            version: metadata.version.clone(),
            metadata,
            key_ids: Vec::new(),
        }
    }

    /// Record a page key as the newest. Re-adding a known key moves it to the
    /// newest position. At most [`MAX_VERSION_KEYS`] keys are kept; the oldest
    /// is dropped when full so one long-lived version cannot break checkpoints.
    pub fn add_key_id(&mut self, key_id: KeyId) {
        if let Some(i) = self.key_ids.iter().position(|key| *key == key_id) {
            self.key_ids.remove(i);
        } else if self.key_ids.len() >= MAX_VERSION_KEYS {
            self.key_ids.remove(0);
        }
        self.key_ids.push(key_id);
    }

    /// Keep only the newest [`MAX_VERSION_KEYS`] keys.
    pub(crate) fn truncate_key_ids(&mut self) {
        if self.key_ids.len() > MAX_VERSION_KEYS {
            let excess = self.key_ids.len() - MAX_VERSION_KEYS;
            self.key_ids.drain(..excess);
        }
    }
}

/// Bounded FIFO map from version key to [`VersionEntry`].
#[derive(Debug)]
pub(crate) struct VersionTable {
    capacity: usize,
    next: u64,
    entries: HashMap<VersionKey, (u64, VersionEntry)>,
    order: BTreeMap<u64, VersionKey>,
}

impl VersionTable {
    /// Empty table holding at most `capacity` versions.
    pub(crate) fn new(capacity: usize) -> Result<Self> {
        if capacity == 0 {
            return Err(Error::InvalidConfiguration);
        }
        Ok(Self {
            capacity,
            next: 0,
            entries: HashMap::default(),
            order: BTreeMap::new(),
        })
    }

    #[cfg(test)]
    pub(crate) fn capacity(&self) -> usize {
        self.capacity
    }

    /// Change the bound, evicting the oldest entries that no longer fit.
    pub(crate) fn set_capacity(&mut self, capacity: usize) -> Result<Vec<VersionEntry>> {
        if capacity == 0 {
            return Err(Error::InvalidConfiguration);
        }
        self.capacity = capacity;
        let mut evicted = Vec::new();
        while self.entries.len() > self.capacity {
            evicted.extend(self.evict_oldest());
        }
        Ok(evicted)
    }

    #[cfg(test)]
    pub(crate) fn len(&self) -> usize {
        self.entries.len()
    }

    #[cfg(test)]
    pub(crate) fn is_empty(&self) -> bool {
        self.entries.is_empty()
    }

    /// Insert or replace the entry for `entry.version`. A hash collision with a
    /// different version keeps the existing entry and returns
    /// [`Error::CorruptRecord`]; reads compare full identity, so callers treat
    /// this as a miss. Returns the entry evicted to make room, if any.
    pub(crate) fn insert(&mut self, entry: VersionEntry) -> Result<Option<VersionEntry>> {
        if entry.metadata.version != entry.version {
            return Err(Error::InvalidConfiguration);
        }
        let key = version_key(&entry.version);
        if let Some((_, existing)) = self.entries.get_mut(&key) {
            if existing.version != entry.version {
                return Err(Error::CorruptRecord);
            }
            *existing = entry;
            return Ok(None);
        }
        let evicted = if self.entries.len() >= self.capacity {
            self.evict_oldest()
        } else {
            None
        };
        let age = self.next;
        self.next += 1;
        self.order.insert(age, key);
        self.entries.insert(key, (age, entry));
        Ok(evicted)
    }

    pub(crate) fn get(&self, key: &VersionKey) -> Option<&VersionEntry> {
        self.entries.get(key).map(|(_, entry)| entry)
    }

    pub(crate) fn get_mut(&mut self, key: &VersionKey) -> Option<&mut VersionEntry> {
        self.entries.get_mut(key).map(|(_, entry)| entry)
    }

    /// Entry for an exact version; `None` on a miss or a hash collision.
    pub(crate) fn get_version(&self, version: &ObjectVersion) -> Option<&VersionEntry> {
        self.get(&version_key(version))
            .filter(|entry| entry.version == *version)
    }

    pub(crate) fn remove(&mut self, key: &VersionKey) -> Option<VersionEntry> {
        let (age, entry) = self.entries.remove(key)?;
        self.order.remove(&age);
        Some(entry)
    }

    /// Remove every entry whose key starts with `cache`; returns the count.
    pub(crate) fn remove_cache(&mut self, cache: &[u8; 8]) -> usize {
        let before = self.entries.len();
        let order = &mut self.order;
        self.entries.retain(|key, (age, _)| {
            let keep = key[..8] != cache[..];
            if !keep {
                order.remove(age);
            }
            keep
        });
        before - self.entries.len()
    }

    /// Entries oldest first, for checkpoint serialization.
    pub(crate) fn iter(&self) -> impl Iterator<Item = (&VersionKey, &VersionEntry)> {
        self.order
            .values()
            .filter_map(|key| self.entries.get(key).map(|(_, entry)| (key, entry)))
    }

    /// Up to `limit` entries inserted after the entry of age `after` (from the
    /// oldest if `None`), oldest first, with their ages as cursors.
    pub(crate) fn iter_after(
        &self,
        after: Option<u64>,
        limit: usize,
    ) -> impl Iterator<Item = (u64, &VersionEntry)> {
        let start = match after {
            Some(age) => std::ops::Bound::Excluded(age),
            None => std::ops::Bound::Unbounded,
        };
        self.order
            .range((start, std::ops::Bound::Unbounded))
            .filter_map(|(age, key)| self.entries.get(key).map(|(_, entry)| (*age, entry)))
            .take(limit)
    }

    pub(crate) fn clear(&mut self) {
        self.entries.clear();
        self.order.clear();
    }

    fn evict_oldest(&mut self) -> Option<VersionEntry> {
        let (_, key) = self.order.pop_first()?;
        self.entries.remove(&key).map(|(_, entry)| entry)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::{CacheKey, ObjectId, StrongEtag};
    use crate::store::key::cache_hash;
    use racer_control_wire::CacheId;

    fn metadata(cache: &str, etag: &str) -> VersionMetadata {
        VersionMetadata {
            content_type: None,
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId(cache.into()),
                    key: CacheKey([1; 32]),
                },
                etag: StrongEtag::test_value(etag),
            },
            length: 10,
        }
    }

    fn entry(cache: &str, etag: &str) -> VersionEntry {
        VersionEntry::new(metadata(cache, etag))
    }

    #[test]
    fn zero_capacity_is_rejected() {
        assert_eq!(
            VersionTable::new(0).unwrap_err(),
            Error::InvalidConfiguration
        );
        let mut table = VersionTable::new(1).unwrap();
        assert_eq!(
            table.set_capacity(0).unwrap_err(),
            Error::InvalidConfiguration
        );
    }

    #[test]
    fn insert_get_remove() {
        let mut table = VersionTable::new(4).unwrap();
        assert!(table.is_empty());
        let a = entry("c", "a");
        let key = version_key(&a.version);
        assert_eq!(table.insert(a.clone()).unwrap(), None);
        assert_eq!(table.len(), 1);
        assert_eq!(table.get(&key), Some(&a));
        assert_eq!(table.get_version(&a.version), Some(&a));
        assert_eq!(table.get_version(&entry("c", "b").version), None);
        assert_eq!(table.remove(&key), Some(a));
        assert_eq!(table.remove(&key), None);
        assert!(table.is_empty());
        assert_eq!(table.iter().count(), 0);
    }

    #[test]
    fn mismatched_metadata_is_rejected() {
        let mut table = VersionTable::new(4).unwrap();
        let mut bad = entry("c", "a");
        bad.version = entry("c", "b").version;
        assert_eq!(table.insert(bad).unwrap_err(), Error::InvalidConfiguration);
        assert!(table.is_empty());
    }

    #[test]
    fn replace_keeps_age_and_updates_entry() {
        let mut table = VersionTable::new(2).unwrap();
        table.insert(entry("c", "a")).unwrap();
        table.insert(entry("c", "b")).unwrap();
        let mut updated = entry("c", "a");
        updated.metadata.length = 99;
        updated.add_key_id(KeyId([3; 16]));
        assert_eq!(table.insert(updated.clone()).unwrap(), None);
        assert_eq!(table.len(), 2);
        // "a" is still the oldest and goes first.
        let evicted = table.insert(entry("c", "z")).unwrap();
        assert_eq!(evicted, Some(updated));
    }

    #[test]
    fn full_table_evicts_oldest() {
        let mut table = VersionTable::new(2).unwrap();
        let a = entry("c", "a");
        table.insert(a.clone()).unwrap();
        table.insert(entry("c", "b")).unwrap();
        assert_eq!(table.insert(entry("c", "d")).unwrap(), Some(a.clone()));
        assert_eq!(table.len(), 2);
        assert_eq!(table.get_version(&a.version), None);
        let order: Vec<_> = table.iter().map(|(_, e)| e.version.etag.clone()).collect();
        assert_eq!(
            order,
            vec![StrongEtag::test_value("b"), StrongEtag::test_value("d")]
        );
    }

    #[test]
    fn shrinking_capacity_evicts_oldest() {
        let mut table = VersionTable::new(3).unwrap();
        for etag in ["a", "b", "c"] {
            table.insert(entry("c", etag)).unwrap();
        }
        let evicted = table.set_capacity(1).unwrap();
        assert_eq!(evicted, vec![entry("c", "a"), entry("c", "b")]);
        assert_eq!(table.capacity(), 1);
        assert_eq!(table.len(), 1);
    }

    #[test]
    fn remove_cache_drops_only_that_cache() {
        let mut table = VersionTable::new(8).unwrap();
        table.insert(entry("one", "a")).unwrap();
        table.insert(entry("two", "a")).unwrap();
        table.insert(entry("one", "b")).unwrap();
        let hash = cache_hash(&CacheId("one".into()));
        assert_eq!(table.remove_cache(&hash), 2);
        assert_eq!(table.remove_cache(&hash), 0);
        assert_eq!(table.len(), 1);
        assert_eq!(table.iter().count(), 1);
        // Order bookkeeping stays consistent after bulk removal.
        for etag in ["x", "y", "z", "w", "v", "u", "t"] {
            table.insert(entry("two", etag)).unwrap();
        }
        assert_eq!(table.len(), 8);
        assert_eq!(
            table.insert(entry("two", "s")).unwrap(),
            Some(entry("two", "a"))
        );
    }

    #[test]
    fn key_ids_deduplicate() {
        let mut table = VersionTable::new(1).unwrap();
        let a = entry("c", "a");
        let key = version_key(&a.version);
        table.insert(a).unwrap();
        let e = table.get_mut(&key).unwrap();
        e.add_key_id(KeyId([1; 16]));
        e.add_key_id(KeyId([1; 16]));
        e.add_key_id(KeyId([2; 16]));
        assert_eq!(table.get(&key).unwrap().key_ids.len(), 2);
        table.clear();
        assert!(table.is_empty());
    }

    #[test]
    fn key_ids_keep_newest_bounded() {
        let mut e = entry("c", "a");
        let key = |n: u32| {
            let mut k = [0; 16];
            k[..4].copy_from_slice(&n.to_le_bytes());
            KeyId(k)
        };
        for n in 0..(MAX_VERSION_KEYS as u32 + 10) {
            e.add_key_id(key(n));
        }
        assert_eq!(e.key_ids.len(), MAX_VERSION_KEYS);
        assert_eq!(e.key_ids[0], key(10));
        assert_eq!(*e.key_ids.last().unwrap(), key(MAX_VERSION_KEYS as u32 + 9));
        // Re-adding an existing key moves it to newest without evicting.
        e.add_key_id(key(10));
        assert_eq!(e.key_ids.len(), MAX_VERSION_KEYS);
        assert_eq!(e.key_ids[0], key(11));
        assert_eq!(*e.key_ids.last().unwrap(), key(10));
        // Defensive truncation keeps the newest keys.
        for n in 0..20 {
            e.key_ids.push(key(1000 + n));
        }
        e.truncate_key_ids();
        assert_eq!(e.key_ids.len(), MAX_VERSION_KEYS);
        assert_eq!(*e.key_ids.last().unwrap(), key(1019));
        assert_eq!(e.key_ids[0], key(31));
    }
}
