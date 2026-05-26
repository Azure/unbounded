// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Per-shard [`BlockStore`] view over a hot-swappable
//! [`LiveDiskTopology`].
//!
//! Each shard holds a [`LiveShardLocalStore`] and registers its
//! NUMA-local backing exactly once via [`Self::register_backing`].
//! On every `BlockStore` call the view compares the topology's
//! currently-published `Arc<LocalStorage>` against the one it last
//! saw; on a mismatch it replays the registered backing through the
//! newly-published [`LocalStorage::register_extra_buffer`] before
//! delegating.
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
/// registrations whenever the published `LocalStorage` changes.
pub struct LiveShardLocalStore<B: BlockDevice + 'static> {
    topology: Arc<LiveDiskTopology<B>>,
    registered: Mutex<Vec<ShardBacking>>,
    last_seen_ptr: Mutex<Option<*const LocalStorage<B>>>,
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
            registered: Mutex::new(Vec::new()),
            last_seen_ptr: Mutex::new(None),
        }
    }

    /// Record `backing` and register it against the currently-
    /// published [`LocalStorage`] (if any). Subsequent changes to
    /// the published `LocalStorage` will replay the same
    /// registration against the new instance.
    pub fn register_backing(&self, backing: &bufferpool::Backing) -> Result<(), Error> {
        let entry = ShardBacking {
            base: backing.base,
            page_size: backing.page_size,
            page_count: backing.page_count,
        };
        self.registered.lock().unwrap().push(entry);
        if let Some(ls) = self.topology.current() {
            ls.register_extra_buffer(entry.base, entry.page_size * entry.page_count)?;
            // Mark *this* `Arc<LocalStorage>` as the one we have
            // registered against. Reading the pointer from the
            // same `Arc` we just registered avoids the TOCTOU
            // where another `apply_engines` between the
            // `current()` call and a generation read would cause
            // the next swap to be missed.
            *self.last_seen_ptr.lock().unwrap() = Some(Arc::as_ptr(&ls));
        }
        Ok(())
    }

    /// Resolve the currently-published `LocalStorage`, replaying
    /// any registered backings if the published instance changed.
    /// Returns `None` when the topology has no engines.
    fn current_or_replay(&self) -> Option<Arc<LocalStorage<B>>> {
        let ls = self.topology.current()?;
        let ls_ptr = Arc::as_ptr(&ls);
        let mut last_ptr = self.last_seen_ptr.lock().unwrap();
        if *last_ptr != Some(ls_ptr) {
            let backings: Vec<ShardBacking> = self.registered.lock().unwrap().clone();
            for b in &backings {
                if let Err(e) = ls.register_extra_buffer(b.base, b.page_size * b.page_count) {
                    eprintln!("disks: replay register_extra_buffer failed: {e:?}");
                }
            }
            *last_ptr = Some(ls_ptr);
        }
        Some(ls)
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
    fn replays_registration_after_topology_swap() {
        let t: Arc<LiveDiskTopology<MockDevice>> = LiveDiskTopology::new();
        t.apply_engines(vec![(PathBuf::from("/a"), engine())]);

        let view = LiveShardLocalStore::new(t.clone());
        let mut buf = vec![0u8; 4096 * 8].into_boxed_slice();
        let backing = bufferpool::Backing {
            base: buf.as_mut_ptr(),
            page_size: 4096,
            page_count: 8,
            _own: Box::new(()),
        };
        view.register_backing(&backing).unwrap();
        let ls1 = t.current().unwrap();
        let p1 = view.last_seen_ptr.lock().unwrap().clone();
        assert_eq!(p1, Some(Arc::as_ptr(&ls1)));

        // Swap to a new engine set; the view must catch up on the
        // next `current_or_replay` call by re-registering against
        // the freshly-published `LocalStorage`.
        t.apply_engines(vec![(PathBuf::from("/b"), engine())]);
        let ls2 = t.current().unwrap();
        assert!(!Arc::ptr_eq(&ls1, &ls2));
        let _ = view.current_or_replay();
        let p2 = view.last_seen_ptr.lock().unwrap().clone();
        assert_eq!(p2, Some(Arc::as_ptr(&ls2)));
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
