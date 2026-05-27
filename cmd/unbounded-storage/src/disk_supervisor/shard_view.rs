// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Per-shard [`BlockStore`] view over a hot-swappable
//! [`LiveDiskTopology`].
//!
//! Each shard holds a [`LiveShardLocalStore`] and registers its
//! NUMA-local backing through [`Self::register_backing`]. On every
//! `BlockStore` call the view compares the topology's current
//! generation against the one it last replayed against; on a
//! mismatch it re-registers every recorded backing through the
//! newly-published [`LocalStorage::register_extra_buffer`] before
//! delegating. Multiple backings and concurrent topology swaps are
//! both safe: registration replay always re-seats the *full*
//! recorded set under a single critical section, so we never mark a
//! `LocalStorage` as "fully registered" without having actually
//! registered every entry against it.
//!
//! When the topology has no engines published (empty `[[disks]]`)
//! reads and writes return [`Error::Transport`] - the data path is
//! offline by definition and there is no NullBlockStore semantic to
//! fall back to here.

use std::sync::Arc;
use std::sync::Mutex;

use crate::bufferpool::{self, BlockStore, Error, PageRef, StripeKey};
use crate::storage::LocalStorage;
use crate::storage::blockdev::BlockDevice;

use super::topology::LiveDiskTopology;

/// `BlockStore` that forwards to whichever [`LocalStorage`] is
/// currently published by the topology, replaying buffer
/// registrations whenever the topology generation advances.
pub struct LiveShardLocalStore<B: BlockDevice + 'static> {
    topology: Arc<LiveDiskTopology<B>>,
    state: Mutex<ReplayState>,
}

/// Registered backings plus the topology generation we last
/// replayed against. Kept together behind one mutex so a concurrent
/// `apply_engines` cannot wedge an updated generation between an
/// in-flight registration loop and the bookkeeping that records it.
struct ReplayState {
    registered: Vec<ShardBacking>,
    last_seen_generation: Option<u64>,
}

#[derive(Copy, Clone)]
struct ShardBacking {
    base: *mut u8,
    page_size: usize,
    page_count: usize,
}

// SAFETY: `ShardBacking::base` mirrors the contract on
// [`crate::storage::local::ShardLocalStore`]: it points into a
// pinned, shard-owned region whose lifetime outlives this store and
// is only ever dereferenced from the shard that registered it.
unsafe impl<B: BlockDevice + 'static> Send for LiveShardLocalStore<B> {}
unsafe impl<B: BlockDevice + 'static> Sync for LiveShardLocalStore<B> {}

impl<B: BlockDevice + 'static> LiveShardLocalStore<B> {
    /// Build a per-shard view over `topology`. No backings are
    /// registered until [`Self::register_backing`] is called.
    pub fn new(topology: Arc<LiveDiskTopology<B>>) -> Self {
        Self {
            topology,
            state: Mutex::new(ReplayState {
                registered: Vec::new(),
                last_seen_generation: None,
            }),
        }
    }

    /// Record `backing` and register the full set of recorded
    /// backings against the currently-published [`LocalStorage`]
    /// (if any). Subsequent topology swaps will replay the same
    /// set against the new `LocalStorage`.
    pub fn register_backing(&self, backing: &bufferpool::Backing) -> Result<(), Error> {
        let entry = ShardBacking {
            base: backing.base,
            page_size: backing.page_size,
            page_count: backing.page_count,
        };
        {
            let mut guard = self.state.lock().unwrap();
            guard.registered.push(entry);
            // Force a replay even if the generation matches what
            // we last saw: the new entry has never been
            // registered against the current `LocalStorage`.
            guard.last_seen_generation = None;
        }
        let _ = self.current_or_replay();
        Ok(())
    }

    /// Resolve the currently-published `LocalStorage`, replaying
    /// every registered backing if the topology generation has
    /// advanced since we last replayed. Returns `None` when the
    /// topology has no engines.
    fn current_or_replay(&self) -> Option<Arc<LocalStorage<B>>> {
        let ls = self.topology.current()?;
        // `apply_engines` publishes the snapshot *before* it bumps
        // the generation counter (see `topology::apply_engines`).
        // Reading the generation after `current()` is therefore
        // conservative: if a concurrent swap is in progress we
        // either observe the old snapshot + old gen (we will
        // catch up on the next call) or the new snapshot + an
        // old-or-new gen. We can never observe a new gen paired
        // with an old snapshot, so "have I registered against
        // this gen yet?" is a sound staleness check.
        let gen_n = self.topology.generation();
        let mut guard = self.state.lock().unwrap();
        if guard.last_seen_generation != Some(gen_n) {
            Self::replay_locked(&ls, &guard.registered);
            guard.last_seen_generation = Some(gen_n);
        }
        Some(ls)
    }

    /// Re-register every recorded backing against `ls`. Errors
    /// are logged and swallowed: a single bad registration must
    /// not prevent the rest of the set from being seated.
    fn replay_locked(ls: &LocalStorage<B>, backings: &[ShardBacking]) {
        for b in backings {
            if let Err(e) = ls.register_extra_buffer(b.base, b.page_size * b.page_count) {
                eprintln!("disks: replay register_extra_buffer failed: {e:?}");
            }
        }
    }
}

impl<B: BlockDevice + 'static> BlockStore for LiveShardLocalStore<B> {
    fn register_pages(&self, backing: &bufferpool::Backing) -> Result<(), Error> {
        self.register_backing(backing)
    }

    async fn read_page(
        &self,
        key: StripeKey,
        stripe_off: u64,
        dst: PageRef,
    ) -> Result<bool, Error> {
        let Some(ls) = self.current_or_replay() else {
            return Err(Error::from("no disks open"));
        };
        ls.read_page(key, stripe_off, dst).await
    }

    async fn write_page(
        &self,
        key: StripeKey,
        stripe_off: u64,
        page: PageRef,
    ) -> Result<(), Error> {
        let Some(ls) = self.current_or_replay() else {
            return Err(Error::from("no disks open"));
        };
        ls.write_page(key, stripe_off, page).await
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::storage::blockdev::{MockDevice, MockDeviceConfig};
    use crate::storage::{EngineConfig, StorageEngine};
    use std::future::Future;
    use std::path::PathBuf;
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
        engine_with_device().0
    }

    fn engine_with_device() -> (Arc<StorageEngine<MockDevice>>, Arc<MockDevice>) {
        let device = Arc::new(MockDevice::new(MockDeviceConfig {
            page_size: 4096,
            capacity_pages: 64,
            ..Default::default()
        }));
        let mut cfg = EngineConfig::default();
        cfg.page_size_bytes = 4096;
        cfg.btree_page_bytes = 4096;
        let eng = Arc::new(block_on(StorageEngine::open(device.clone(), cfg)).unwrap());
        (eng, device)
    }

    fn make_backing(pages: usize) -> (Box<[u8]>, bufferpool::Backing) {
        let mut buf = vec![0u8; 4096 * pages].into_boxed_slice();
        let base = buf.as_mut_ptr();
        let backing = bufferpool::Backing {
            base,
            page_size: 4096,
            page_count: pages,
            _own: Box::new(()),
        };
        (buf, backing)
    }

    #[test]
    fn replays_registration_after_topology_swap() {
        let t: Arc<LiveDiskTopology<MockDevice>> = LiveDiskTopology::new();
        t.apply_engines(vec![(PathBuf::from("/a"), engine())]);

        let view = LiveShardLocalStore::new(t.clone());
        let (_buf, backing) = make_backing(8);
        view.register_backing(&backing).unwrap();
        let gen1 = t.generation();
        assert_eq!(view.state.lock().unwrap().last_seen_generation, Some(gen1));

        // Swap to a new engine set; the view must catch up on the
        // next `current_or_replay` call by re-registering against
        // the freshly-published `LocalStorage` and recording the
        // new generation.
        t.apply_engines(vec![(PathBuf::from("/b"), engine())]);
        let gen2 = t.generation();
        assert_ne!(gen1, gen2);
        let _ = view.current_or_replay();
        assert_eq!(view.state.lock().unwrap().last_seen_generation, Some(gen2));
    }

    #[test]
    fn replays_every_backing_against_newest_local_storage() {
        // Registers two backings across two topology swaps, then
        // forces a third swap and asserts that BOTH backings are
        // re-registered against the freshest `LocalStorage`.
        // Observability comes from the underlying `MockDevice`,
        // which records every `register_buffers` call.
        let t: Arc<LiveDiskTopology<MockDevice>> = LiveDiskTopology::new();
        let (e1, _d1) = engine_with_device();
        t.apply_engines(vec![(PathBuf::from("/a"), e1)]);

        let view = LiveShardLocalStore::new(t.clone());
        let (_buf_a, backing_a) = make_backing(4);
        view.register_backing(&backing_a).unwrap();

        // Swap to a fresh engine; the next BlockStore-style call
        // will replay `backing_a` against the new device.
        let (e2, _d2) = engine_with_device();
        t.apply_engines(vec![(PathBuf::from("/b"), e2)]);

        // Register a second backing. This must also replay
        // `backing_a` against whatever LocalStorage is current
        // (otherwise we would mark the new LocalStorage "fully
        // registered" with only `backing_b` actually wired up).
        let (_buf_b, backing_b) = make_backing(4);
        view.register_backing(&backing_b).unwrap();

        // Now drive a third swap with a brand-new device we can
        // inspect. `StorageEngine::open` may itself perform
        // bookkeeping registrations on the device, so snapshot
        // the registration count immediately after the swap and
        // measure the delta produced by `current_or_replay`.
        let (e3, d3) = engine_with_device();
        t.apply_engines(vec![(PathBuf::from("/c"), e3)]);
        let baseline = d3.registered_count();
        let baseline_bytes = d3.registered_len();
        let _ = view.current_or_replay();

        assert_eq!(
            d3.registered_count() - baseline,
            2,
            "expected both backings to be replayed against the newest device"
        );
        assert_eq!(
            d3.registered_len() - baseline_bytes,
            backing_a.page_size * backing_a.page_count
                + backing_b.page_size * backing_b.page_count,
            "replay sizes do not match the recorded backings"
        );
        assert_eq!(
            view.state.lock().unwrap().last_seen_generation,
            Some(t.generation())
        );
    }

    #[test]
    fn empty_topology_returns_io_error() {
        let t: Arc<LiveDiskTopology<MockDevice>> = LiveDiskTopology::new();
        let view = LiveShardLocalStore::new(t);
        let dst = PageRef {
            page_idx: 0,
            offset: 0,
            len: 4096,
        };
        let err = block_on(view.read_page(StripeKey([0; 32]), 0, dst));
        assert!(matches!(err, Err(Error::Transport(_))));
    }
}
