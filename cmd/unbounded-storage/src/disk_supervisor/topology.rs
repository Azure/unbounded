// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Hot-swappable [`LocalStorage`] published to shards.
//!
//! `LiveDiskTopology` owns the set of currently-engined disks
//! (indexed by path), publishes an `Arc<LocalStorage<B>>` via
//! `ArcSwap`, and bumps a generation counter on every successful
//! [`Self::apply_engines`] call. Shards observe the swap through
//! [`Self::current`] and use [`Self::generation`] to detect when the
//! pointer they last cached is stale.
//!
//! Engines are not opened here. `UringBlockDevice` is `!Send`, so any
//! [`crate::storage::engine::StorageEngine::open`] must run on the
//! same thread that owns the device. The caller (production wiring or
//! tests) is responsible for producing `Arc<StorageEngine<B>>` values
//! and handing them in via [`Self::apply_engines`]; this type only
//! manages the generation/swap discipline.

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};

use arc_swap::ArcSwap;

use crate::storage::blockdev::BlockDevice;
use crate::storage::{LocalStorage, StorageEngine};

/// Snapshot of the currently-published engines. `None` means no
/// disks are open and [`LocalStorage`] cannot be constructed (it
/// asserts at least one engine).
pub type LocalStorageSnapshot<B> = Option<Arc<LocalStorage<B>>>;

/// Owns the per-path engine table and publishes a `LocalStorage`
/// snapshot to consumers. Generic over the block device type so
/// tests can exercise the swap/generation behavior with mocks.
pub struct LiveDiskTopology<B: BlockDevice + 'static> {
    engines: std::sync::Mutex<HashMap<PathBuf, Arc<StorageEngine<B>>>>,
    current: ArcSwap<LocalStorageSnapshotInner<B>>,
    generation: AtomicU64,
}

// `ArcSwap` requires `Sized + 'static` over the inner; wrap the
// `Option` so we can store it directly.
struct LocalStorageSnapshotInner<B: BlockDevice + 'static>(LocalStorageSnapshot<B>);

impl<B: BlockDevice + 'static> LiveDiskTopology<B> {
    /// Build an empty topology with generation 0 and no published
    /// `LocalStorage`.
    pub fn new() -> Arc<Self> {
        Arc::new(Self {
            engines: std::sync::Mutex::new(HashMap::new()),
            current: ArcSwap::new(Arc::new(LocalStorageSnapshotInner(None))),
            generation: AtomicU64::new(0),
        })
    }

    /// Replace the engine set with `engines`. Engines whose path is
    /// already present are preserved (the supplied `Arc` is
    /// ignored); paths removed from the input are dropped; new
    /// paths are inserted. A new [`LocalStorage`] is built over the
    /// resulting set and published via `ArcSwap`. Generation is
    /// bumped on every call regardless of whether the set changed,
    /// so consumers re-resolve any cached `Arc<LocalStorage>` and
    /// reseat per-shard registrations.
    pub fn apply_engines(&self, engines: Vec<(PathBuf, Arc<StorageEngine<B>>)>) {
        let mut table = self.engines.lock().unwrap();
        let mut next: HashMap<PathBuf, Arc<StorageEngine<B>>> = HashMap::new();
        for (path, eng) in engines {
            let kept = table.remove(&path).unwrap_or(eng);
            next.insert(path, kept);
        }
        *table = next;

        let ordered: Vec<Arc<StorageEngine<B>>> = {
            let mut paths: Vec<&PathBuf> = table.keys().collect();
            paths.sort();
            paths.iter().map(|p| table[*p].clone()).collect()
        };
        let snapshot: LocalStorageSnapshot<B> = if ordered.is_empty() {
            None
        } else {
            Some(Arc::new(LocalStorage::new(ordered)))
        };
        let n = table.len();

        // Publish and bump the generation while still holding the
        // engines lock so concurrent `apply_engines` calls cannot
        // reorder publications relative to the generation counter.
        self.current
            .store(Arc::new(LocalStorageSnapshotInner(snapshot)));
        let gen_n = self.generation.fetch_add(1, Ordering::AcqRel) + 1;
        drop(table);
        eprintln!("disks: hot-swap to generation {gen_n} (cache cold; engine count={n})");
    }

    /// Load the currently-published `LocalStorage` snapshot. `None`
    /// when no engines are open.
    pub fn current(&self) -> LocalStorageSnapshot<B> {
        self.current.load_full().0.clone()
    }

    /// Generation counter; advances on every [`Self::apply_engines`].
    pub fn generation(&self) -> u64 {
        self.generation.load(Ordering::Acquire)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::storage::blockdev::{MockDevice, MockDeviceConfig};
    use crate::storage::{EngineConfig, StorageEngine};
    use std::future::Future;
    use std::pin::pin;
    use std::task::{Context, Poll, RawWaker, RawWakerVTable, Waker};

    fn noop_waker() -> Waker {
        fn raw() -> RawWaker {
            RawWaker::new(std::ptr::null(), &VTABLE)
        }
        static VTABLE: RawWakerVTable = RawWakerVTable::new(|_| raw(), |_| {}, |_| {}, |_| {});
        unsafe { Waker::from_raw(raw()) }
    }

    fn block_on<F: Future>(f: F) -> F::Output {
        let w = noop_waker();
        let mut cx = Context::from_waker(&w);
        let mut f = pin!(f);
        let mut spins = 0u64;
        loop {
            match f.as_mut().poll(&mut cx) {
                Poll::Ready(v) => return v,
                Poll::Pending => {
                    spins += 1;
                    assert!(spins < 1_000_000, "stuck");
                }
            }
        }
    }

    fn engine() -> Arc<StorageEngine<MockDevice>> {
        let device = Arc::new(MockDevice::new(MockDeviceConfig {
            page_size: 4096,
            capacity_pages: 64,
            ..Default::default()
        }));
        let mut cfg = EngineConfig::default();
        cfg.page_size_bytes = 4096;
        cfg.btree_page_bytes = 4096;
        Arc::new(block_on(StorageEngine::open(device, cfg)).unwrap())
    }

    #[test]
    fn empty_topology_has_no_snapshot_and_gen_zero() {
        let t: Arc<LiveDiskTopology<MockDevice>> = LiveDiskTopology::new();
        assert!(t.current().is_none());
        assert_eq!(t.generation(), 0);
    }

    #[test]
    fn apply_bumps_generation_and_publishes_snapshot() {
        let t: Arc<LiveDiskTopology<MockDevice>> = LiveDiskTopology::new();
        let e = engine();
        t.apply_engines(vec![(PathBuf::from("/a"), e.clone())]);
        assert_eq!(t.generation(), 1);
        let snap = t.current().expect("snapshot present");
        assert_eq!(snap.num_disks(), 1);
    }

    #[test]
    fn unchanged_path_preserves_engine_arc() {
        let t: Arc<LiveDiskTopology<MockDevice>> = LiveDiskTopology::new();
        let e_orig = engine();
        t.apply_engines(vec![(PathBuf::from("/a"), e_orig.clone())]);
        let snap1 = t.current().unwrap();
        let kept_ptr = Arc::as_ptr(&snap1.engine_arc(0));

        // Supplying a *different* Arc for the same path must be
        // ignored: the topology preserves the original engine.
        let e_new = engine();
        t.apply_engines(vec![(PathBuf::from("/a"), e_new)]);
        let snap2 = t.current().unwrap();
        assert_eq!(Arc::as_ptr(&snap2.engine_arc(0)), kept_ptr);
        assert_eq!(t.generation(), 2);
    }

    #[test]
    fn removed_path_drops_engine() {
        let t: Arc<LiveDiskTopology<MockDevice>> = LiveDiskTopology::new();
        let e_a = engine();
        let e_b = engine();
        t.apply_engines(vec![
            (PathBuf::from("/a"), e_a.clone()),
            (PathBuf::from("/b"), e_b.clone()),
        ]);
        assert_eq!(t.current().unwrap().num_disks(), 2);
        t.apply_engines(vec![(PathBuf::from("/b"), e_b)]);
        assert_eq!(t.current().unwrap().num_disks(), 1);
    }

    #[test]
    fn apply_empty_publishes_none() {
        let t: Arc<LiveDiskTopology<MockDevice>> = LiveDiskTopology::new();
        t.apply_engines(vec![(PathBuf::from("/a"), engine())]);
        assert!(t.current().is_some());
        t.apply_engines(vec![]);
        assert!(t.current().is_none());
        assert_eq!(t.generation(), 2);
    }
}
