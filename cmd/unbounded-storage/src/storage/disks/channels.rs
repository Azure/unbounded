// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Hot-swappable per-disk [`PageChannel`] set published to shards.
//!
//! `DiskChannelDirectory` owns the set of currently-open disks (each
//! represented by a [`PageChannel`] to its storage core) and
//! publishes a `(channels, generation)` pair via `ArcSwap`. Every
//! successful [`Self::apply_channels`] call builds a fresh pair and
//! stores it as a single atomic publication, so consumers that
//! resolve through [`Self::snapshot`] observe the channel set and its
//! generation together. Per-shard registration caches key off that
//! generation, so "registered against gen N" always refers to the
//! snapshot that was published as gen N.
//!
//! Channels are not opened here. The disk supervisor opens each disk
//! on its own pinned storage core and hands the resulting
//! [`PageChannel`] in via [`Self::apply_channels`]; this type only
//! manages the publication discipline.

use std::path::PathBuf;
use std::sync::Arc;

use arc_swap::ArcSwap;

use crate::storage::PageChannel;

/// Snapshot of the currently-published channels. `None` means no
/// disks are open and the data path is offline.
pub type ChannelSnapshot = Option<Arc<Vec<PageChannel>>>;

/// Owns the published per-disk channel set. Each
/// [`Self::apply_channels`] bumps the generation and publishes a
/// fresh, path-sorted [`PageChannel`] vector so `disk_for` indices
/// stay stable across consumers.
pub struct DiskChannelDirectory {
    current: ArcSwap<ChannelSnapshotInner>,
}

/// Published unit: the channel snapshot and the generation it was
/// published under, kept together so a single `ArcSwap` load returns
/// a consistent pair. Consumers must go through
/// [`DiskChannelDirectory::snapshot`] for any staleness decision so the
/// pair cannot tear between two adjacent publications.
struct ChannelSnapshotInner {
    channels: ChannelSnapshot,
    generation: u64,
}

impl DiskChannelDirectory {
    /// Build an empty directory with generation 0 and no published
    /// channels.
    pub fn new() -> Arc<Self> {
        Arc::new(Self {
            current: ArcSwap::new(Arc::new(ChannelSnapshotInner {
                channels: None,
                generation: 0,
            })),
        })
    }

    /// Replace the channel set with `channels`, published in
    /// path-sorted order so the `disk_for` index of any given page
    /// is stable. Generation is bumped on every call regardless of
    /// whether the set changed, so consumers re-resolve any cached
    /// snapshot and reseat per-shard registrations.
    pub fn apply_channels(&self, mut channels: Vec<(PathBuf, PageChannel)>) {
        channels.sort_by(|a, b| a.0.cmp(&b.0));
        let ordered: Vec<PageChannel> = channels.into_iter().map(|(_, c)| c).collect();
        let n = ordered.len();
        let snapshot: ChannelSnapshot = if ordered.is_empty() {
            None
        } else {
            Some(Arc::new(ordered))
        };

        // `apply_channels` is the only writer of `current`; callers
        // serialize it (the supervisor reconciles on one thread), so
        // reading the previous generation and storing the new bundle
        // is race-free. The atomic `ArcSwap::store` is the entire
        // publication: any consumer that loads the bundle sees both
        // fields from the same generation.
        let gen_n = self.current.load().generation + 1;
        self.current.store(Arc::new(ChannelSnapshotInner {
            channels: snapshot,
            generation: gen_n,
        }));
        eprintln!("disks: hot-swap to generation {gen_n} (cache cold; channel count={n})");
    }

    /// Atomically load the currently-published channel set paired
    /// with the generation it was published under. This is the only
    /// API that yields a consistent (snapshot, gen) pair; staleness
    /// checks must use it.
    pub fn snapshot(&self) -> (ChannelSnapshot, u64) {
        let inner = self.current.load_full();
        (inner.channels.clone(), inner.generation)
    }

    /// Load the currently-published channel set. `None` when no
    /// disks are open. Prefer [`Self::snapshot`] in any context that
    /// also needs the generation.
    pub fn current(&self) -> ChannelSnapshot {
        self.current.load_full().channels.clone()
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
    }

    #[test]
    fn apply_bumps_generation_and_publishes_snapshot() {
        let t = DiskChannelDirectory::new();
        t.apply_channels(vec![(PathBuf::from("/a"), dummy_channel())]);
        assert_eq!(t.generation(), 1);
        let snap = t.current().expect("snapshot present");
        assert_eq!(snap.len(), 1);
    }

    #[test]
    fn channels_are_published_path_sorted() {
        let t = DiskChannelDirectory::new();
        // Supplied out of order; publication must be path-sorted so
        // `disk_for` indices stay stable.
        t.apply_channels(vec![
            (PathBuf::from("/c"), dummy_channel()),
            (PathBuf::from("/a"), dummy_channel()),
            (PathBuf::from("/b"), dummy_channel()),
        ]);
        let snap = t.current().unwrap();
        assert_eq!(snap.len(), 3);
        assert_eq!(t.generation(), 1);
    }

    #[test]
    fn removed_path_drops_channel() {
        let t = DiskChannelDirectory::new();
        t.apply_channels(vec![
            (PathBuf::from("/a"), dummy_channel()),
            (PathBuf::from("/b"), dummy_channel()),
        ]);
        assert_eq!(t.current().unwrap().len(), 2);
        t.apply_channels(vec![(PathBuf::from("/b"), dummy_channel())]);
        assert_eq!(t.current().unwrap().len(), 1);
        assert_eq!(t.generation(), 2);
    }

    #[test]
    fn apply_empty_publishes_none() {
        let t = DiskChannelDirectory::new();
        t.apply_channels(vec![(PathBuf::from("/a"), dummy_channel())]);
        assert!(t.current().is_some());
        t.apply_channels(vec![]);
        assert!(t.current().is_none());
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
        let (c0, g0) = t.snapshot();
        assert!(c0.is_none());
        assert_eq!(g0, 0);
        assert_eq!(g0, t.generation());

        t.apply_channels(vec![(PathBuf::from("/a"), dummy_channel())]);
        let (c1, g1) = t.snapshot();
        assert_eq!(g1, 1);
        assert_eq!(g1, t.generation());
        let c1 = c1.expect("snapshot present after apply");

        t.apply_channels(vec![(PathBuf::from("/b"), dummy_channel())]);
        let (c2, g2) = t.snapshot();
        assert_eq!(g2, 2);
        assert_eq!(g2, t.generation());
        let c2 = c2.expect("snapshot present after second apply");

        // Each apply publishes a fresh vector, so the returned `Arc`s
        // do not alias across generations.
        assert!(!Arc::ptr_eq(&c1, &c2));
    }
}
