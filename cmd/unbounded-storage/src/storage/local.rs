// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Per-node `LocalStorage`: routes page operations across the
//! per-disk `StorageEngine` instances on a single node. This is the
//! node-level half of the two-axis sharding the design specifies; the
//! cluster-level chunk-to-owner mapping is the orthogonal concern of
//! `RoutingTable` and is not implemented here.
//!
//! The disk selector is the pure function
//! `hash(value_hash, page_idx) mod num_local_devices`. It is
//! deliberately not parameterized: every component on the node that
//! needs to address a disk - admission, eviction, the read/write
//! path - must agree on the mapping by construction, and the cheapest
//! way to enforce that is a single function with no inputs other than
//! the page identity.

use std::sync::Arc;

use crate::bufferpool;
use crate::bufferpool::{Error, PageRef, StripeKey};
use crate::storage::blockdev::BlockDevice;
use crate::storage::engine::StorageEngine;
use crate::storage::traits::{PageChecksum, Xxh3Checksum};

/// Fans the [`bufferpool::BlockStore`] surface across `N` per-disk
/// engines on a single node. Construction takes ownership of the
/// engines; the router never recreates them and never reassigns
/// pages across disks. The selector is stable for a given
/// `num_local_devices`, so if the disk count changes the cache must
/// be considered cold.
pub struct LocalStorage<B: BlockDevice + 'static> {
    engines: Vec<Arc<StorageEngine<B>>>,
}

impl<B: BlockDevice + 'static> LocalStorage<B> {
    /// Build a router over `engines`. Panics if `engines` is empty:
    /// a node with zero disks cannot serve any page and the caller
    /// should fail loudly rather than route to a phantom disk.
    pub fn new(engines: Vec<Arc<StorageEngine<B>>>) -> Self {
        assert!(
            !engines.is_empty(),
            "LocalStorage requires at least one engine",
        );
        Self { engines }
    }

    /// Number of underlying disks. Stable for the lifetime of the
    /// router.
    pub fn num_disks(&self) -> usize {
        self.engines.len()
    }

    /// Borrow the engine for disk index `idx`. Out-of-bounds panics
    /// rather than returns `None`: every caller that holds a
    /// `LocalStorage` already knows `num_disks`.
    pub fn engine(&self, idx: usize) -> &StorageEngine<B> {
        &self.engines[idx]
    }

    /// Owned-Arc accessor used by callers (notably the DST harness
    /// and tests) that need to spawn the engine's mutator task on
    /// a separate executor task.
    pub fn engine_arc(&self, idx: usize) -> Arc<StorageEngine<B>> {
        self.engines[idx].clone()
    }

    /// Disk selector. Pure function over the page identity; see the
    /// module-level comment for the invariant this preserves.
    pub fn disk_for(&self, key: StripeKey, stripe_off: u64) -> usize {
        disk_for(&key, stripe_off, self.engines.len())
    }

    /// Drive every engine's mutator loop to completion concurrently.
    ///
    /// Intended to be called once per shard thread by the embedder:
    /// one thread owns the `LocalStorage` for that shard and pumps
    /// all of its engines from a single task. `ShardLocalStore`
    /// deliberately does not expose this; it is a per-shard handle
    /// over a shared `LocalStorage`, and the embedder drives the
    /// shared instance directly.
    pub async fn run(&self) {
        use std::future::Future;
        use std::pin::Pin;
        use std::task::{Context, Poll};

        struct RunAll<'a> {
            futs: Vec<Option<Pin<Box<dyn Future<Output = ()> + 'a>>>>,
        }
        impl<'a> Future for RunAll<'a> {
            type Output = ();
            fn poll(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
                let mut all_done = true;
                for slot in self.futs.iter_mut() {
                    if let Some(f) = slot.as_mut() {
                        match f.as_mut().poll(cx) {
                            Poll::Ready(()) => *slot = None,
                            Poll::Pending => all_done = false,
                        }
                    }
                }
                if all_done {
                    Poll::Ready(())
                } else {
                    Poll::Pending
                }
            }
        }

        let futs = self
            .engines
            .iter()
            .map(|e| {
                Some(Box::pin(e.clone().run_mutator()) as Pin<Box<dyn Future<Output = ()> + '_>>)
            })
            .collect();
        RunAll { futs }.await
    }

    /// Fan [`BlockDevice::progress`] out to every engine's device.
    /// Bails on the first error to match the crate's existing
    /// error-propagation style; a partial sweep is acceptable
    /// because the embedder calls this repeatedly on idle.
    pub fn progress(&self) -> Result<(), crate::storage::types::Error> {
        for e in &self.engines {
            e.device().progress()?;
        }
        Ok(())
    }
}

/// Free-standing version of [`LocalStorage::disk_for`] so other
/// modules (admission, eviction, tests) can compute the same index
/// without having to hold a `LocalStorage` instance.
pub fn disk_for(key: &StripeKey, stripe_off: u64, num_disks: usize) -> usize {
    debug_assert!(num_disks > 0);
    let mut buf = [0u8; 40];
    buf[..32].copy_from_slice(&key.0);
    buf[32..].copy_from_slice(&stripe_off.to_le_bytes());
    let h = Xxh3Checksum::checksum_of(&buf).0;
    (h % num_disks as u64) as usize
}

impl<B: BlockDevice + 'static> LocalStorage<B> {
    /// Register a per-shard pinned region with every engine's
    /// underlying device so it is eligible for fixed-buffer DMA.
    /// Unlike [`bufferpool::BlockStore::register_pages`], this does
    /// not install a single canonical backing on each engine; it
    /// just teaches each device about the region. Used by
    /// [`ShardLocalStore`] so multiple NUMA-local backings can
    /// coexist behind a shared `LocalStorage`.
    pub fn register_extra_buffer(&self, base: *mut u8, bytes: usize) -> Result<(), Error> {
        for eng in &self.engines {
            eng.register_extra_buffer(base, bytes)?;
        }
        Ok(())
    }

    /// Route a raw-slice read to the disk that owns the page.
    ///
    /// SAFETY: see [`StorageEngine::read_page_into`].
    pub async unsafe fn read_page_into(
        &self,
        key: StripeKey,
        stripe_off: u64,
        dst: *mut [u8],
    ) -> Result<bool, Error> {
        unsafe {
            self.read_page_into_with_priority(key, stripe_off, dst, 0)
                .await
        }
    }

    /// Route a raw-slice read to the disk that owns the page and
    /// promote any resident hit to at least `priority` for eviction.
    ///
    /// SAFETY: see [`StorageEngine::read_page_into`].
    pub async unsafe fn read_page_into_with_priority(
        &self,
        key: StripeKey,
        stripe_off: u64,
        dst: *mut [u8],
        priority: i32,
    ) -> Result<bool, Error> {
        let idx = self.disk_for(key, stripe_off);
        unsafe {
            self.engines[idx]
                .read_page_into_with_priority(key, stripe_off, dst, priority)
                .await
        }
    }

    /// Route a raw-slice write to the disk that owns the page.
    ///
    /// SAFETY: see [`StorageEngine::write_page_from`].
    pub async unsafe fn write_page_from(
        &self,
        key: StripeKey,
        stripe_off: u64,
        src: *const [u8],
    ) -> Result<(), Error> {
        unsafe {
            self.write_page_from_with_priority(key, stripe_off, src, 0)
                .await
        }
    }

    /// Route a raw-slice write to the disk that owns the page and
    /// record the page's cache priority for eviction ordering.
    ///
    /// SAFETY: see [`StorageEngine::write_page_from`].
    pub async unsafe fn write_page_from_with_priority(
        &self,
        key: StripeKey,
        stripe_off: u64,
        src: *const [u8],
        priority: i32,
    ) -> Result<(), Error> {
        let idx = self.disk_for(key, stripe_off);
        unsafe {
            self.engines[idx]
                .write_page_from_with_priority(key, stripe_off, src, priority)
                .await
        }
    }
}

impl<B: BlockDevice + 'static> bufferpool::BlockStore for LocalStorage<B> {
    fn register_pages(&self, backing: &crate::memory::Backing) -> Result<(), Error> {
        // Single-backing convenience path used by inline tests and
        // the single-shard configuration: every engine sees the
        // same backing and resolves `PageRef`s against it. Multi-
        // shard deployments wrap `LocalStorage` in
        // `ShardLocalStore` instead and never call this method.
        for eng in &self.engines {
            <StorageEngine<B> as bufferpool::BlockStore>::register_pages(eng, backing)?;
        }
        Ok(())
    }

    async fn read_page<R: bufferpool::Req + ?Sized>(
        &self,
        req: &R,
        stripe_off: u64,
        dst: PageRef,
    ) -> Result<bool, Error> {
        let key = req.key();
        let idx = self.disk_for(key, stripe_off);
        self.engines[idx].read_page(req, stripe_off, dst).await
    }

    async fn write_page<R: bufferpool::Req + ?Sized>(
        &self,
        req: &R,
        stripe_off: u64,
        page: PageRef,
    ) -> Result<(), Error> {
        let key = req.key();
        let idx = self.disk_for(key, stripe_off);
        self.engines[idx].write_page(req, stripe_off, page).await
    }
}

/// Per-shard `BlockStore` wrapper around a shared
/// [`LocalStorage`]. Each NIC shard has its own NUMA-local
/// `Backing`; the shard hands its own `ShardLocalStore` to its
/// `Pool`. The wrapper remembers the shard's backing geometry so
/// `PageRef`s emitted by that pool can be resolved to absolute
/// addresses locally, then routed to the engine that owns the
/// destination disk via the shared `LocalStorage`. The shared
/// engines themselves never see the shard's `PageRef` directly;
/// they receive precomputed slices, which is why `LocalStorage`
/// (and the engine beneath it) can be shared across shards even
/// though each shard's backing lives in a different NUMA region.
pub struct ShardLocalStore<B: BlockDevice + 'static> {
    inner: std::sync::Arc<LocalStorage<B>>,
    backing: std::sync::Mutex<Option<ShardBacking>>,
}

#[derive(Copy, Clone, Debug)]
struct ShardBacking {
    base: *mut u8,
    page_size: usize,
    page_count: usize,
}

// SAFETY: `ShardBacking::base` is a `*mut u8` pointing into a
// pinned, shard-owned region whose lifetime is managed by the
// shard's `Backing` (see `crate::memory::Backing`). The pool
// upholds the per-NUMA pinning invariant; we only ever read the
// pointer to compute page offsets and never alias it across
// shards.
unsafe impl<B: BlockDevice + 'static> Send for ShardLocalStore<B> {}
unsafe impl<B: BlockDevice + 'static> Sync for ShardLocalStore<B> {}

impl<B: BlockDevice + 'static> ShardLocalStore<B> {
    /// Wrap a shared [`LocalStorage`] for use by a single shard's
    /// `Pool`. The backing is installed lazily by the pool calling
    /// [`bufferpool::BlockStore::register_pages`].
    pub fn new(inner: std::sync::Arc<LocalStorage<B>>) -> Self {
        Self {
            inner,
            backing: std::sync::Mutex::new(None),
        }
    }

    fn resolve(&self, page: PageRef) -> (*mut u8, usize) {
        let b = self
            .backing
            .lock()
            .unwrap()
            .expect("register_pages not called on ShardLocalStore");
        debug_assert!((page.page_idx as usize) < b.page_count);
        // SAFETY: `page_idx < page_count` (debug-asserted) and the
        // backing region is `page_count * page_size` bytes long, so
        // the resulting pointer is in-bounds for the registered
        // region. The pool guarantees `offset + len <= page_size`.
        let p = unsafe {
            b.base
                .add(page.page_idx as usize * b.page_size + page.offset as usize)
        };
        (p, page.len as usize)
    }
}

impl<B: BlockDevice + 'static> bufferpool::BlockStore for ShardLocalStore<B> {
    fn register_pages(&self, backing: &crate::memory::Backing) -> Result<(), Error> {
        {
            let mut b = self.backing.lock().unwrap();
            *b = Some(ShardBacking {
                base: backing.base,
                page_size: backing.page_size,
                page_count: backing.page_count,
            });
        }
        self.inner
            .register_extra_buffer(backing.base, backing.page_size * backing.page_count)
    }

    async fn read_page<R: bufferpool::Req + ?Sized>(
        &self,
        req: &R,
        stripe_off: u64,
        dst: PageRef,
    ) -> Result<bool, Error> {
        let (p, len) = self.resolve(dst);
        let slice = std::ptr::slice_from_raw_parts_mut(p, len);
        // SAFETY: `resolve` produced an in-bounds pointer into the
        // shard's pinned backing; the pool guarantees the page is
        // not aliased for the duration of this future.
        unsafe {
            self.inner
                .read_page_into(req.key(), stripe_off, slice)
                .await
        }
    }

    async fn write_page<R: bufferpool::Req + ?Sized>(
        &self,
        req: &R,
        stripe_off: u64,
        page: PageRef,
    ) -> Result<(), Error> {
        let (p, len) = self.resolve(page);
        let slice = std::ptr::slice_from_raw_parts(p as *const u8, len);
        // SAFETY: see `read_page` above.
        unsafe {
            self.inner
                .write_page_from(req.key(), stripe_off, slice)
                .await
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn disk_for_is_stable_for_same_input() {
        let k = StripeKey([7u8; 32]);
        let a = disk_for(&k, 42, 8);
        let b = disk_for(&k, 42, 8);
        assert_eq!(a, b);
    }

    #[test]
    fn disk_for_changes_with_offset() {
        // Not strictly required (hash collisions exist) but for the
        // tiny inputs we use here this is true; the test exists so
        // a regression that ignored `stripe_off` is loud.
        let k = StripeKey([0u8; 32]);
        let mut seen = std::collections::HashSet::new();
        for off in 0u64..16 {
            seen.insert(disk_for(&k, off, 8));
        }
        assert!(seen.len() > 1, "offset must influence routing");
    }

    #[test]
    fn disk_for_changes_with_key() {
        let mut seen = std::collections::HashSet::new();
        for b in 0u8..16 {
            let k = StripeKey([b; 32]);
            seen.insert(disk_for(&k, 0, 8));
        }
        assert!(seen.len() > 1, "key bytes must influence routing");
    }

    #[test]
    fn disk_for_in_range() {
        let k = StripeKey([0xa5; 32]);
        for n in 1usize..=32 {
            for off in 0u64..64 {
                let idx = disk_for(&k, off, n);
                assert!(idx < n);
            }
        }
    }

    // The empty-engines panic is covered indirectly: `new` is a
    // public assert on a `Vec::is_empty()` check and the message is
    // exercised by callers that construct `LocalStorage` with zero
    // disks (currently nothing in main.rs does this). A dedicated
    // test would have to fabricate a `BlockDevice` impl just to
    // pick a type parameter, which is more noise than signal; the
    // assert itself is one line.

    use crate::bufferpool::BlockStore;
    use crate::runtime::noop_waker;
    use crate::storage::blockdev::{MockDevice, MockDeviceConfig};
    use crate::storage::engine::{EngineConfig, StorageEngine};
    use std::future::Future;
    use std::pin::{Pin, pin};
    use std::sync::Arc;
    use std::task::{Context, Poll};

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

    /// Pump `body` to completion while concurrently driving every
    /// future in `aux`. Used to keep per-engine mutator loops
    /// running alongside a test body in tests that exercise
    /// writes through one or more engines.
    fn block_on_with_aux<F: Future>(
        body: F,
        aux: &mut [Pin<&mut dyn Future<Output = ()>>],
    ) -> F::Output {
        let w = noop_waker();
        let mut cx = Context::from_waker(&w);
        let mut body = pin!(body);
        let mut spins = 0u64;
        loop {
            for a in aux.iter_mut() {
                let _ = a.as_mut().poll(&mut cx);
            }
            match body.as_mut().poll(&mut cx) {
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
            capacity_pages: 128,
            ..Default::default()
        }));
        let mut cfg = EngineConfig::default();
        cfg.page_size_bytes = 4096;
        cfg.btree_page_bytes = 4096;
        Arc::new(block_on(StorageEngine::open(device, cfg)).unwrap())
    }

    #[test]
    fn shard_local_store_roundtrip_across_disks() {
        // Two engines = two disks. Each shard has its own backing.
        // We exercise the wrapper from two simulated shards in
        // sequence (the executor is single-threaded; "different
        // shard" here just means "different backing region").
        let e0 = engine();
        let e1 = engine();
        let inner = Arc::new(LocalStorage::new(vec![e0.clone(), e1.clone()]));

        // Shard A: 32 pool pages of 4 KiB at base_a.
        let mut buf_a = vec![0u8; 4096 * 32].into_boxed_slice();
        let backing_a = crate::memory::Backing {
            base: buf_a.as_mut_ptr(),
            page_size: 4096,
            page_count: 32,
            keepalive: std::sync::Arc::new(()),
        };
        let store_a = ShardLocalStore::new(inner.clone());
        store_a.register_pages(&backing_a).unwrap();

        // Shard B: distinct backing.
        let mut buf_b = vec![0u8; 4096 * 32].into_boxed_slice();
        let backing_b = crate::memory::Backing {
            base: buf_b.as_mut_ptr(),
            page_size: 4096,
            page_count: 32,
            keepalive: std::sync::Arc::new(()),
        };
        let store_b = ShardLocalStore::new(inner.clone());
        store_b.register_pages(&backing_b).unwrap();

        // Write a pattern from shard A's page 0 to (key, off=0).
        let key = StripeKey([0x42; 32]);
        unsafe {
            let p = buf_a.as_mut_ptr();
            for i in 0..4096 {
                p.add(i).write(((i * 13) & 0xff) as u8);
            }
        }
        let src = PageRef {
            page_idx: 0,
            offset: 0,
            len: 4096,
        };

        let e0_body = e0.clone();
        let e1_body = e1.clone();
        let store_a_clone = &store_a;
        let store_b_clone = &store_b;
        let body = async move {
            // First write is rejected by the admission filter on
            // first touch; the second admits.
            store_a_clone.write_page(&key, 0, src).await.unwrap();
            store_a_clone.write_page(&key, 0, src).await.unwrap();

            let dst = PageRef {
                page_idx: 7,
                offset: 0,
                len: 4096,
            };
            let hit = store_b_clone.read_page(&key, 0, dst).await.unwrap();
            assert!(hit, "expected cache hit across shards");
            e0_body.close_mutator();
            e1_body.close_mutator();
        };
        let m0 = e0.clone().run_mutator();
        let m1 = e1.clone().run_mutator();
        let mut m0 = pin!(m0);
        let mut m1 = pin!(m1);
        let aux: &mut [Pin<&mut dyn Future<Output = ()>>] = &mut [m0.as_mut(), m1.as_mut()];
        block_on_with_aux(body, aux);

        unsafe {
            let p = buf_b.as_ptr().add(4096 * 7);
            for i in 0..4096 {
                assert_eq!(
                    p.add(i).read(),
                    ((i * 13) & 0xff) as u8,
                    "shard B page byte {i} mismatch"
                );
            }
        }
        // Shard A's page 0 must be unchanged (no aliasing).
        unsafe {
            let p = buf_a.as_ptr();
            for i in 0..4096 {
                assert_eq!(p.add(i).read(), ((i * 13) & 0xff) as u8);
            }
        }
    }

    #[test]
    #[should_panic(expected = "register_pages not called")]
    fn shard_local_store_without_register_panics() {
        let inner = Arc::new(LocalStorage::new(vec![engine()]));
        let store = ShardLocalStore::new(inner);
        let dst = PageRef {
            page_idx: 0,
            offset: 0,
            len: 4096,
        };
        // Triggers resolve() with no backing installed.
        let _ = block_on(store.read_page(&StripeKey([0; 32]), 0, dst));
    }
}
