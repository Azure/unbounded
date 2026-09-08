// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Hot-swappable per-disk [`PageChannel`] set published to shards.
//!
//! `DiskChannelDirectory` owns the set of currently-open disks (each
//! represented by a [`PageChannel`] to its storage core) and
//! publishes one immutable snapshot via `ArcSwap`. A
//! [`Self::apply_channels`] call that actually changes the set builds
//! a fresh pair and stores it as a single atomic publication, so
//! consumers that resolve through [`Self::snapshot`] observe channels,
//! topology, policy, and generation together; an apply of the identical
//! set is skipped so the generation does not churn. Per-shard
//! registration caches key off that generation, so "registered
//! against gen N" always refers to the snapshot that was published as
//! gen N.
//!
//! Channels are not opened here. The disk supervisor opens each disk
//! on its own pinned storage core and hands the resulting
//! [`PageChannel`] in via [`Self::apply_channels`]; this type only
//! manages the publication discipline.

use std::path::PathBuf;
use std::sync::Arc;

use arc_swap::ArcSwap;

use crate::storage::PageChannel;

/// Published channel metadata, aligned index-for-index by path-sorted
/// disk order.
pub struct ChannelSet {
    pub channels: Vec<PageChannel>,
    pub page_cache_enabled: Vec<bool>,
    pub drive_numa: Vec<Option<u16>>,
    pub generation: u64,
    key: Vec<(PathBuf, u64, Option<u16>)>,
}

/// Owns the published per-disk channel set. A change-bearing
/// [`Self::apply_channels`] bumps the generation and publishes a
/// fresh, path-sorted [`PageChannel`] vector so `disk_for` indices
/// stay stable across consumers.
pub struct DiskChannelDirectory {
    current: ArcSwap<ChannelSet>,
}

impl DiskChannelDirectory {
    /// Build an empty directory with generation 0 and no published
    /// channels.
    pub fn new() -> Arc<Self> {
        Arc::new(Self {
            current: ArcSwap::new(Arc::new(ChannelSet {
                channels: Vec::new(),
                page_cache_enabled: Vec::new(),
                drive_numa: Vec::new(),
                generation: 0,
                key: Vec::new(),
            })),
        })
    }

    /// Replace the channel set with `channels`, published in
    /// path-sorted order so the `disk_for` index of any given page
    /// is stable. Each entry carries the drive's NUMA node (the node
    /// its pinned storage core lives on, or `None` when unpinned),
    /// published as a parallel `drive_numa` vector the router uses to
    /// keep a stripe's serving shard on the same node as its disk.
    /// The generation is bumped only when the published set actually
    /// changes (a different path set, the same path bound to a
    /// different storage-core service, or a drive that moved NUMA
    /// node); a re-apply of the identical set is a no-op so consumers
    /// do not needlessly reseat per-shard registrations.
    pub fn apply_channels(&self, mut channels: Vec<(PathBuf, PageChannel, Option<u16>, bool)>) {
        channels.sort_by(|a, b| a.0.cmp(&b.0));
        let key: Vec<(PathBuf, u64, Option<u16>)> = channels
            .iter()
            .map(|(p, c, numa, _)| (p.clone(), c.service_id(), *numa))
            .collect();
        let page_cache_enabled: Vec<bool> = channels
            .iter()
            .map(|(_, _, _, page_cache_enabled)| *page_cache_enabled)
            .collect();

        // `apply_channels` is the only writer of `current`; callers
        // serialize it (the supervisor reconciles on one thread), so
        // reading the previous bundle and storing the new one is
        // race-free.
        let prev = self.current.load();
        if prev.key == key && prev.page_cache_enabled == page_cache_enabled {
            // Identical publication: nothing downstream needs to
            // reseat, so skip the generation bump and the store.
            return;
        }

        let drive_numa: Vec<Option<u16>> = channels.iter().map(|(_, _, numa, _)| *numa).collect();
        let ordered: Vec<PageChannel> = channels.into_iter().map(|(_, c, _, _)| c).collect();
        let n = ordered.len();
        // The atomic `ArcSwap::store` is the entire publication: any
        // consumer sees all fields from the same generation.
        let gen_n = if prev.key == key {
            prev.generation
        } else {
            prev.generation + 1
        };
        self.current.store(Arc::new(ChannelSet {
            channels: ordered,
            page_cache_enabled,
            drive_numa,
            generation: gen_n,
            key,
        }));
        eprintln!("disks: hot-swap to generation {gen_n} (cache cold; channel count={n})");
    }

    /// Atomically load the current channels, topology, policy, and
    /// generation as one immutable snapshot.
    pub fn snapshot(&self) -> Arc<ChannelSet> {
        self.current.load_full()
    }

    /// Load the currently-published channel set. `None` when no
    /// disks are open. Prefer [`Self::snapshot`] in any context that
    /// also needs the generation.
    pub fn current(&self) -> Option<Arc<ChannelSet>> {
        let snapshot = self.snapshot();
        (!snapshot.channels.is_empty()).then_some(snapshot)
    }

    /// Generation of the currently-published snapshot. Observability
    /// only; read together with the snapshot via [`Self::snapshot`]
    /// when both are needed.
    pub fn generation(&self) -> u64 {
        self.current.load().generation
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A `PageChannel` whose receiver is dropped. Directory tests only
    /// exercise the publication discipline, never the data path.
    fn dummy_channel() -> PageChannel {
        PageChannel::new().0
    }

    #[test]
    fn empty_topology_has_no_snapshot_and_gen_zero() {
        let t = DiskChannelDirectory::new();
        assert!(t.current().is_none());
        assert_eq!(t.generation(), 0);
        assert!(t.snapshot().drive_numa.is_empty());
    }

    #[test]
    fn apply_bumps_generation_and_publishes_snapshot() {
        let t = DiskChannelDirectory::new();
        t.apply_channels(vec![(PathBuf::from("/a"), dummy_channel(), None, true)]);
        assert_eq!(t.generation(), 1);
        let snap = t.current().expect("snapshot present");
        assert_eq!(snap.channels.len(), 1);
    }

    #[test]
    fn channels_are_published_path_sorted() {
        let t = DiskChannelDirectory::new();
        // Supplied out of order; publication must be path-sorted so
        // `disk_for` indices stay stable.
        t.apply_channels(vec![
            (PathBuf::from("/c"), dummy_channel(), None, true),
            (PathBuf::from("/a"), dummy_channel(), None, true),
            (PathBuf::from("/b"), dummy_channel(), None, true),
        ]);
        let snap = t.current().unwrap();
        assert_eq!(snap.channels.len(), 3);
        assert_eq!(t.generation(), 1);
    }

    #[test]
    fn drive_numa_is_published_path_sorted_and_aligned() {
        // The NUMA vector must be reordered with the channels (by path)
        // so `drive_numa[disk_for(...)]` names the node of that drive.
        let t = DiskChannelDirectory::new();
        t.apply_channels(vec![
            (PathBuf::from("/c"), dummy_channel(), Some(2), true),
            (PathBuf::from("/a"), dummy_channel(), Some(0), true),
            (PathBuf::from("/b"), dummy_channel(), None, true),
        ]);
        let snapshot = t.snapshot();
        let numa = &snapshot.drive_numa;
        // Path order /a,/b,/c -> NUMA 0, None, 2.
        assert_eq!(numa, &[Some(0), None, Some(2)]);
        assert_eq!(numa.len(), snapshot.channels.len());
    }

    #[test]
    fn page_cache_policy_is_published_path_sorted_and_aligned() {
        let t = DiskChannelDirectory::new();
        t.apply_channels(vec![
            (PathBuf::from("/c"), dummy_channel(), None, true),
            (PathBuf::from("/a"), dummy_channel(), None, false),
            (PathBuf::from("/b"), dummy_channel(), None, true),
        ]);
        let snap = t.current().expect("snapshot present");
        assert_eq!(snap.page_cache_enabled, vec![false, true, true]);
    }

    #[test]
    fn removed_path_drops_channel() {
        let t = DiskChannelDirectory::new();
        t.apply_channels(vec![
            (PathBuf::from("/a"), dummy_channel(), None, true),
            (PathBuf::from("/b"), dummy_channel(), None, true),
        ]);
        assert_eq!(t.current().unwrap().channels.len(), 2);
        t.apply_channels(vec![(PathBuf::from("/b"), dummy_channel(), None, true)]);
        assert_eq!(t.current().unwrap().channels.len(), 1);
        assert_eq!(t.generation(), 2);
    }

    #[test]
    fn apply_empty_publishes_none() {
        let t = DiskChannelDirectory::new();
        t.apply_channels(vec![(PathBuf::from("/a"), dummy_channel(), Some(0), true)]);
        assert!(t.current().is_some());
        assert_eq!(t.snapshot().drive_numa.len(), 1);
        t.apply_channels(vec![]);
        assert!(t.current().is_none());
        assert!(t.snapshot().drive_numa.is_empty());
        assert_eq!(t.generation(), 2);
    }

    #[test]
    fn snapshot_returns_matched_pair_across_swaps() {
        // Bundle guarantee: `snapshot()` returns the channel set and
        // the generation it was published under from the same
        // `ArcSwap` load. After N applies the generation equals N and
        // matches the observability accessor; distinct apply calls
        // publish distinct snapshot `Arc`s.
        let t = DiskChannelDirectory::new();
        let c0 = t.snapshot();
        assert!(c0.channels.is_empty());
        assert_eq!(c0.generation, 0);
        assert_eq!(c0.generation, t.generation());

        t.apply_channels(vec![(PathBuf::from("/a"), dummy_channel(), None, true)]);
        let c1 = t.snapshot();
        assert_eq!(c1.generation, 1);
        assert_eq!(c1.generation, t.generation());

        t.apply_channels(vec![(PathBuf::from("/b"), dummy_channel(), None, true)]);
        let c2 = t.snapshot();
        assert_eq!(c2.generation, 2);
        assert_eq!(c2.generation, t.generation());

        // Each apply publishes a fresh vector, so the returned `Arc`s
        // do not alias across generations.
        assert!(!Arc::ptr_eq(&c1, &c2));
    }

    #[test]
    fn identical_reapply_is_a_noop() {
        // Re-applying the same path bound to the same service (clones
        // of one channel share identity) must not bump the generation
        // or republish, so consumers keep their seated registrations.
        let t = DiskChannelDirectory::new();
        let c = dummy_channel();
        t.apply_channels(vec![(PathBuf::from("/a"), c.clone(), Some(1), true)]);
        assert_eq!(t.generation(), 1);
        let first = t.current().expect("snapshot present");

        t.apply_channels(vec![(PathBuf::from("/a"), c.clone(), Some(1), true)]);
        assert_eq!(t.generation(), 1, "identical re-apply must not bump");
        let second = t.current().expect("snapshot still present");
        assert!(
            Arc::ptr_eq(&first, &second),
            "no-op apply must not republish a fresh snapshot",
        );
    }

    #[test]
    fn same_path_new_service_bumps_generation() {
        // Same path, but a distinct service identity (a fresh channel)
        // is a real change and must republish.
        let t = DiskChannelDirectory::new();
        t.apply_channels(vec![(PathBuf::from("/a"), dummy_channel(), None, true)]);
        assert_eq!(t.generation(), 1);
        t.apply_channels(vec![(PathBuf::from("/a"), dummy_channel(), None, true)]);
        assert_eq!(t.generation(), 2);
    }

    #[test]
    fn numa_repinning_bumps_generation_and_republishes() {
        // Same path bound to the same service, but the drive moved to a
        // different NUMA node: a real routing-topology change that must
        // republish so the router sees the new node.
        let t = DiskChannelDirectory::new();
        let c = dummy_channel();
        t.apply_channels(vec![(PathBuf::from("/a"), c.clone(), Some(0), true)]);
        assert_eq!(t.generation(), 1);
        assert_eq!(t.snapshot().drive_numa, vec![Some(0)]);

        t.apply_channels(vec![(PathBuf::from("/a"), c.clone(), Some(1), true)]);
        assert_eq!(t.generation(), 2, "NUMA move must bump");
        assert_eq!(t.snapshot().drive_numa, vec![Some(1)]);
    }

    #[test]
    fn reapply_after_reorder_is_a_noop() {
        // Input order does not matter: the set is path-sorted before
        // comparison, so supplying the same channels in a different
        // order is still a no-op.
        let t = DiskChannelDirectory::new();
        let a = dummy_channel();
        let b = dummy_channel();
        t.apply_channels(vec![
            (PathBuf::from("/a"), a.clone(), Some(0), true),
            (PathBuf::from("/b"), b.clone(), Some(1), true),
        ]);
        assert_eq!(t.generation(), 1);
        t.apply_channels(vec![
            (PathBuf::from("/b"), b.clone(), Some(1), true),
            (PathBuf::from("/a"), a.clone(), Some(0), true),
        ]);
        assert_eq!(t.generation(), 1);
    }

    #[test]
    fn page_cache_policy_change_republishes_without_generation_bump() {
        let t = DiskChannelDirectory::new();
        let c = dummy_channel();
        t.apply_channels(vec![(PathBuf::from("/a"), c.clone(), Some(1), true)]);
        let first = t.current().expect("snapshot present");

        t.apply_channels(vec![(PathBuf::from("/a"), c.clone(), Some(1), false)]);

        assert_eq!(t.generation(), 1);
        let second = t.current().expect("snapshot still present");
        assert!(!Arc::ptr_eq(&first, &second));
        assert_eq!(second.page_cache_enabled, vec![false]);
    }
}
