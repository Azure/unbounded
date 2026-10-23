//! Segment reverse mappings and stable publication order, independent of page schema.

use crate::SegmentId;
use std::cell::Cell;
use std::collections::{BTreeMap, HashMap, hash_map::RandomState};
use std::hash::{BuildHasher, Hash};
use std::ops::Bound::{Excluded, Unbounded};

/// Mappings of one group in age order, with a group-local victim cursor.
struct Group<K> {
    order: BTreeMap<u64, K>,

    cursor: Cell<u64>,
}

impl<K> Default for Group<K> {
    fn default() -> Self {
        Self {
            order: BTreeMap::new(),
            cursor: Cell::new(0),
        }
    }
}

/// Exact page mappings with independent checkpoint and bounded victim cursors.
/// Each mapping also belongs to one caller-chosen group, so victim selection
/// can visit a chosen set of groups without walking the others.
pub struct PageIndex<K, V, H = RandomState, G = ()> {
    pages: HashMap<K, V, H>,

    reverse: HashMap<SegmentId, BTreeMap<u64, K>, H>,

    order: BTreeMap<u64, K>,

    ages: HashMap<K, (u64, SegmentId, G), H>,

    groups: HashMap<G, Group<K>, H>,

    next_age: u64,

    victim_cursor: Cell<u64>,
}

impl<K, V, H: Default, G> Default for PageIndex<K, V, H, G> {
    /// Start empty with no assigned publication ages.
    fn default() -> Self {
        Self {
            pages: HashMap::with_hasher(H::default()),
            reverse: HashMap::with_hasher(H::default()),
            order: BTreeMap::new(),
            ages: HashMap::with_hasher(H::default()),
            groups: HashMap::with_hasher(H::default()),
            next_age: 0,
            victim_cursor: Cell::new(0),
        }
    }
}

/// Scan at most `budget` entries after `cursor`, wrapping once. Return the
/// lowest `(score, age)` and advance the cursor past every visited entry.
fn scan<K: Clone>(
    order: &BTreeMap<u64, K>,
    cursor: &Cell<u64>,
    budget: usize,
    mut score: impl FnMut(&K) -> Option<u64>,
) -> Option<(u64, u64, K)> {
    let after = cursor.get();
    let mut best: Option<(u64, u64, K)> = None;
    for (age, key) in order
        .range((Excluded(after), Unbounded))
        .chain(order.range(..=after))
        .take(budget)
    {
        cursor.set(*age);
        let Some(value) = score(key) else {
            continue;
        };
        if best
            .as_ref()
            .is_none_or(|(v, a, _)| (value, *age) < (*v, *a))
        {
            best = Some((value, *age, key.clone()));
        }
    }
    best
}

impl<K: Eq + Hash + Clone, V, H: BuildHasher> PageIndex<K, V, H> {
    /// Publish a mapping in the default group.
    pub fn insert(&mut self, key: K, value: V, segment: SegmentId) -> Result<Option<V>, (K, V)> {
        self.insert_in(key, value, segment, ())
    }
}

impl<K: Eq + Hash + Clone, V, H: BuildHasher, G: Eq + Hash + Clone> PageIndex<K, V, H, G> {
    /// Number of live mappings.
    pub fn len(&self) -> usize {
        self.pages.len()
    }

    /// Whether all mappings have been removed.
    pub fn is_empty(&self) -> bool {
        self.pages.is_empty()
    }

    /// Borrow a mapping without changing heat or order.
    pub fn get(&self, key: &K) -> Option<&V> {
        self.pages.get(key)
    }

    /// Check identity membership without changing order.
    pub fn contains_key(&self, key: &K) -> bool {
        self.pages.contains_key(key)
    }

    /// Visit mappings using the caller-selected hash state.
    pub fn iter(&self) -> impl Iterator<Item = (&K, &V)> {
        self.pages.iter()
    }

    /// Visit identities without cloning entries.
    pub fn keys(&self) -> impl Iterator<Item = &K> {
        self.pages.keys()
    }

    /// Check age exhaustion before the caller changes residency or other metadata.
    pub fn can_publish(&self) -> bool {
        self.next_age < u64::MAX
    }

    /// Publish a mapping in `group` with a fresh age, or return it untouched
    /// on exhaustion.
    pub fn insert_in(
        &mut self,
        key: K,
        value: V,
        segment: SegmentId,
        group: G,
    ) -> Result<Option<V>, (K, V)> {
        let Some(age) = self.next_age.checked_add(1) else {
            return Err((key, value));
        };
        let old = self.remove(&key);
        self.next_age = age;
        self.reverse
            .entry(segment)
            .or_default()
            .insert(age, key.clone());
        self.order.insert(age, key.clone());
        self.groups
            .entry(group.clone())
            .or_default()
            .order
            .insert(age, key.clone());
        self.ages.insert(key.clone(), (age, segment, group));
        self.pages.insert(key, value);
        Ok(old)
    }

    /// Remove the mapping and all reverse/order state together.
    pub fn remove(&mut self, key: &K) -> Option<V> {
        let old = self.pages.remove(key)?;
        let (age, segment, group) = self.ages.remove(key).expect("indexed page age");
        self.order.remove(&age);
        let members = self.groups.get_mut(&group).expect("indexed group");
        members.order.remove(&age);
        if members.order.is_empty() {
            self.groups.remove(&group);
        }
        let pages = self.reverse.get_mut(&segment).expect("indexed segment");
        pages.remove(&age);
        if pages.is_empty() {
            self.reverse.remove(&segment);
        }
        Some(old)
    }

    /// Resume a monotonic checkpoint scan, unaffected by victim selection.
    pub fn after(&self, after: u64, limit: usize) -> impl Iterator<Item = (u64, &K, &V)> {
        self.order
            .range((Excluded(after), Unbounded))
            .take(limit)
            .map(|(age, key)| (*age, key, &self.pages[key]))
    }

    /// Select low value, then oldest age, among at most `budget` candidates.
    pub fn victim(&self, budget: usize, mut score: impl FnMut(&K, &V) -> u64) -> Option<K> {
        self.victim_if(budget, |key, value| Some(score(key, value)))
    }

    /// A missing score is a hard veto, even when all candidates are protected.
    pub fn victim_if(
        &self,
        budget: usize,
        mut score: impl FnMut(&K, &V) -> Option<u64>,
    ) -> Option<K> {
        scan(&self.order, &self.victim_cursor, budget, |key| {
            score(key, &self.pages[key])
        })
        .map(|(_, _, key)| key)
    }

    /// Groups that currently hold at least one mapping.
    pub fn groups(&self) -> impl Iterator<Item = &G> {
        self.groups.keys()
    }

    /// Like `victim_if`, but visit only the listed groups, at most
    /// `budget_per_group` candidates each, using each group's own cursor.
    /// Total work is bounded by the number of listed groups times the budget.
    pub fn victim_in_groups<'a>(
        &self,
        groups: impl IntoIterator<Item = &'a G>,
        budget_per_group: usize,
        mut score: impl FnMut(&K, &V) -> Option<u64>,
    ) -> Option<K>
    where
        G: 'a,
    {
        let mut best: Option<(u64, u64, K)> = None;
        for group in groups {
            let Some(members) = self.groups.get(group) else {
                continue;
            };
            let found = scan(&members.order, &members.cursor, budget_per_group, |key| {
                score(key, &self.pages[key])
            });
            if let Some(found) = found
                && best
                    .as_ref()
                    .is_none_or(|(v, a, _)| (found.0, found.1) < (*v, *a))
            {
                best = Some(found);
            }
        }
        best.map(|(_, _, key)| key)
    }

    /// Visit only a bounded prefix of one segment's mappings.
    pub fn segment(&self, segment: SegmentId, budget: usize) -> impl Iterator<Item = (&K, &V)> {
        self.reverse
            .get(&segment)
            .into_iter()
            .flat_map(|pages| pages.values())
            .take(budget)
            .map(|key| (key, &self.pages[key]))
    }

    /// Whether no mapping still points into the segment. Leases remain caller-owned.
    pub fn segment_empty(&self, segment: SegmentId) -> bool {
        !self.reverse.contains_key(&segment)
    }

    /// Sum a bounded prefix and conservatively charge the unseen suffix.
    pub fn segment_score(
        &self,
        segment: SegmentId,
        budget: usize,
        unseen_value: u64,
        mut score: impl FnMut(&K, &V) -> u64,
    ) -> u64 {
        let count = self.reverse.get(&segment).map_or(0, BTreeMap::len);
        let known = self
            .segment(segment, budget)
            .fold(0u64, |sum, (key, value)| {
                sum.saturating_add(score(key, value))
            });
        known.saturating_add((count.saturating_sub(budget) as u64).saturating_mul(unseen_value))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn replacement_reverse_maps_and_monotonic_scans_survive_victim_wrap() {
        let mut index = PageIndex::<u64, u64>::default();
        for key in 0..65 {
            index.insert(key, key, SegmentId(0)).unwrap();
        }
        let cursor = index.after(0, 1).next().unwrap().0;
        let mut calls = 0;
        assert_eq!(
            index.victim(64, |_, _| {
                calls += 1;
                0
            }),
            Some(0)
        );
        assert_eq!(calls, 64);
        assert_eq!(index.victim_cursor.get(), 64);
        assert_eq!(index.victim(1, |_, _| 0), Some(64));
        assert_eq!(index.victim(1, |_, _| 0), Some(0));
        index.remove(&0);
        assert_eq!(index.after(cursor, 100).count(), 64);
        assert_eq!(index.insert(1, 99, SegmentId(1)).unwrap(), Some(1));
        assert_eq!(index.segment(SegmentId(1), 1).next(), Some((&1, &99)));
        index.remove(&1);
        assert!(index.segment_empty(SegmentId(1)));
        assert_eq!(index.victim(0, |_, _| panic!("zero budget")), None);
        assert_eq!(index.segment_score(SegmentId(0), 2, 100, |_, _| 1), 6102);
    }

    #[test]
    fn age_exhaustion_rejects_replacement_without_mutation() {
        let mut index = PageIndex::<u64, u64>::default();
        index.insert(1, 2, SegmentId(0)).unwrap();
        index.next_age = u64::MAX;
        assert!(!index.can_publish());
        assert_eq!(index.insert(1, 3, SegmentId(1)), Err((1, 3)));
        assert_eq!(index.get(&1), Some(&2));
        assert!(index.segment_empty(SegmentId(1)));
    }

    #[test]
    fn group_victims_visit_only_listed_groups_with_own_cursor() {
        let mut index = PageIndex::<u64, u64, RandomState, bool>::default();
        // 100 "keep" mappings come first, then 3 "evict" mappings.
        for key in 0..100 {
            index.insert_in(key, key, SegmentId(0), false).unwrap();
        }
        for key in 100..103 {
            index.insert_in(key, key, SegmentId(1), true).unwrap();
        }
        assert_eq!(index.groups().count(), 2);
        let mut calls = 0;
        let victim = index.victim_in_groups([&true], 64, |key, _| {
            calls += 1;
            assert!(*key >= 100, "visited unlisted group");
            Some(0)
        });
        assert_eq!(victim, Some(100));
        assert_eq!(calls, 3);
        // The group cursor advances independently of the global one.
        assert_eq!(index.victim_cursor.get(), 0);
        assert_eq!(
            index.victim_in_groups([&true], 1, |_, _| Some(0)),
            Some(100)
        );
        assert_eq!(
            index.victim_in_groups([&true], 1, |_, _| Some(0)),
            Some(101)
        );
        // Vetoes skip; lowest score wins across groups, then age.
        assert_eq!(
            index.victim_in_groups([&true, &false], 2, |key, _| (*key != 102).then_some(1)),
            Some(0)
        );
        assert_eq!(index.victim_in_groups([&true], 64, |_, _| None), None);
        assert_eq!(
            index.victim_in_groups([&true], 0, |_, _| panic!("zero")),
            None
        );
        // Removing the last member drops the group.
        for key in 100..103 {
            index.remove(&key);
        }
        assert_eq!(index.groups().collect::<Vec<_>>(), vec![&false]);
        assert_eq!(index.victim_in_groups([&true], 64, |_, _| Some(0)), None);
        // Replacement moves a key between groups.
        index.insert_in(5, 5, SegmentId(0), true).unwrap();
        assert_eq!(index.groups().count(), 2);
        assert_eq!(index.victim_in_groups([&true], 64, |_, _| Some(0)), Some(5));
    }

    #[test]
    fn hard_veto_is_not_a_large_soft_score() {
        let mut index = PageIndex::<u64, u64>::default();
        index.insert(1, 1, SegmentId(0)).unwrap();
        index.insert(2, 2, SegmentId(0)).unwrap();
        assert_eq!(index.victim_if(64, |_, _| None), None);
        assert_eq!(
            index.victim_if(64, |key, _| (*key == 2).then_some(u64::MAX)),
            Some(2)
        );
    }
}
