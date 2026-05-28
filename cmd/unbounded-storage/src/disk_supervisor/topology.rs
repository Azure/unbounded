// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Hot-swappable [`LocalStorage`] published to shards.
//!
//! `LiveDiskTopology` owns the set of currently-engined disks
//! (indexed by path) and publishes a `(LocalStorage, generation)`
//! pair via `ArcSwap`. Every successful [`Self::apply_engines`] call
//! builds a fresh pair and stores it as a single atomic publication,
//! so consumers that resolve through [`Self::snapshot`] observe the
//! snapshot and its generation together. Per-shard registration
//! caches key off that generation, so "registered against gen N"
//! always refers to the snapshot that was published as gen N.
//!
//! Engines are not opened here. `UringBlockDevice` is `!Send`, so any
//! [`crate::storage::engine::StorageEngine::open`] must run on the
//! same thread that owns the device. The caller (production wiring or
//! tests) is responsible for producing `Arc<StorageEngine<B>>` values
//! and handing them in via [`Self::apply_engines`]; this type only
//! manages the publication discipline.

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::Arc;

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
}

/// Published unit: the `LocalStorage` snapshot and the generation it
/// was published under, kept together so a single `ArcSwap` load
/// returns a consistent pair. The generation is *only* read out of
/// this bundle; pairing it with a separately-loaded snapshot
/// reintroduces a tear (an older snapshot can be observed alongside
/// a newer counter), so consumers must go through
/// [`LiveDiskTopology::snapshot`] for any staleness decision.
struct LocalStorageSnapshotInner<B: BlockDevice + 'static> {
    ls: LocalStorageSnapshot<B>,
    generation: u64,
}

impl<B: BlockDevice + 'static> LiveDiskTopology<B> {
    /// Build an empty topology with generation 0 and no published
    /// `LocalStorage`.
    pub fn new() -> Arc<Self> {
        Arc::new(Self {
            engines: std::sync::Mutex::new(HashMap::new()),
            current: ArcSwap::new(Arc::new(LocalStorageSnapshotInner {
                ls: None,
                generation: 0,
            })),
        })
    }

    /// Replace the engine set with `engines`. Engines whose path is
    /// already present are preserved (the supplied `Arc` is
    /// ignored); paths removed from the input are dropped; new
    /// paths are inserted. A new [`LocalStorage`] is built over the
    /// resulting set and published via `ArcSwap` paired with the
    /// next generation number. Generation is bumped on every call
    /// regardless of whether the set changed, so consumers
    /// re-resolve any cached `Arc<LocalStorage>` and reseat
    /// per-shard registrations.
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
        let ls: LocalStorageSnapshot<B> = if ordered.is_empty() {
            None
        } else {
            Some(Arc::new(LocalStorage::new(ordered)))
        };
        let n = table.len();

        // `apply_engines` is serialized by `engines`, so reading
        // the previously-published gen, adding one, and storing
        // the new (ls, gen) bundle is race-free under this lock.
        // The atomic `ArcSwap::store` is the entire publication:
        // any consumer that loads the bundle sees both fields from
        // the same generation, with no possibility of seeing a
        // new gen paired with an old snapshot or vice versa.
        let gen_n = self.current.load().generation + 1;
        self.current
            .store(Arc::new(LocalStorageSnapshotInner {
                ls,
                generation: gen_n,
            }));
        drop(table);
        eprintln!("disks: hot-swap to generation {gen_n} (cache cold; engine count={n})");
    }

    /// Atomically load the currently-published `LocalStorage`
    /// paired with the generation it was published under. This is
    /// the only API that yields a consistent (snapshot, gen) pair;
    /// staleness checks (e.g.
    /// [`crate::disk_supervisor::LiveShardLocalStore`]'s
    /// per-shard registration cache) must use it.
    pub fn snapshot(&self) -> (LocalStorageSnapshot<B>, u64) {
        let inner = self.current.load_full();
        (inner.ls.clone(), inner.generation)
    }

    /// Load the currently-published `LocalStorage` snapshot. `None`
    /// when no engines are open. Prefer [`Self::snapshot`] in any
    /// context that also needs the generation; loading the snapshot
    /// and the generation through separate accessors observes two
    /// independent points in the publication timeline and is not a
    /// sound basis for a staleness check.
    pub fn current(&self) -> LocalStorageSnapshot<B> {
        self.current.load_full().ls.clone()
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

    #[test]
    fn snapshot_returns_matched_pair_across_swaps() {
        // Bundle guarantee: `snapshot()` returns the
        // `LocalStorage` and the generation it was published under
        // from the same `ArcSwap` load. After N applies the
        // generation in the bundle equals N and matches the
        // observability accessor; distinct apply calls publish
        // distinct snapshot `Arc`s.
        let t: Arc<LiveDiskTopology<MockDevice>> = LiveDiskTopology::new();
        let (ls0, g0) = t.snapshot();
        assert!(ls0.is_none());
        assert_eq!(g0, 0);
        assert_eq!(g0, t.generation());

        t.apply_engines(vec![(PathBuf::from("/a"), engine())]);
        let (ls1, g1) = t.snapshot();
        assert_eq!(g1, 1);
        assert_eq!(g1, t.generation());
        let ls1 = ls1.expect("snapshot present after apply");

        t.apply_engines(vec![(PathBuf::from("/b"), engine())]);
        let (ls2, g2) = t.snapshot();
        assert_eq!(g2, 2);
        assert_eq!(g2, t.generation());
        let ls2 = ls2.expect("snapshot present after second apply");

        // Each apply publishes a fresh `LocalStorage`, so the
        // returned `Arc`s do not alias across generations.
        assert!(!Arc::ptr_eq(&ls1, &ls2));
    }
}
