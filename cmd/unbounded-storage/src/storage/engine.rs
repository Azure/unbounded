// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Per-disk storage engine.
//!
//! Composes the lower-level primitives:
//!
//! - [`crate::storage::blockdev::BlockDevice`] for raw NVMe I/O,
//! - [`crate::storage::alloc::Allocator`] over the disk's LBA
//!   space (meta slots are reserved at open),
//! - [`crate::storage::refcount::RefcountTable`] for pin
//!   tracking + the SIEVE referenced bit,
//! - [`crate::storage::btree::BTreeIndex`] for the on-disk
//!   `(value_hash, page_index) -> (lba, checksum, byte_len)`
//!   mapping,
//! - [`crate::storage::lru::SieveLru`] for eviction selection,
//! - [`crate::storage::admission::AdmissionFilter`] for
//!   one-hit-wonder filtering, and
//! - [`crate::storage::singleflight::Singleflight`] for
//!   per-key write deduplication.
//!
//! Implements the project's
//! [`crate::bufferpool::BlockStore`] surface so a
//! [`crate::bufferpool::Pool`] can be configured against an
//! NVMe-backed engine.

use std::collections::HashMap;
use std::future::Future;
use std::pin::Pin;
use std::sync::{Arc, Mutex};
use std::task::{Context, Poll, Waker};

use crate::bufferpool::{self, BulkRef, PageRef, StripeKey};
use crate::storage::admission::AdmissionFilter;
use crate::storage::alloc::Allocator;
use crate::storage::blockdev::BlockDevice;
use crate::storage::btree::{BTreeIndex, LeafEntry, Mutation};
use crate::storage::lru::SieveLru;
use crate::storage::refcount::RefcountTable;
use crate::storage::singleflight::{Acquire, Singleflight};
use crate::storage::traits::{PageChecksum, Xxh3Checksum};
use crate::storage::types::{Lba, PageKey};

#[derive(Copy, Clone, Debug)]
pub struct EngineConfig {
    /// Cache page size, in bytes. Default 2 MiB. Must be a
    /// multiple of [`Self::btree_page_bytes`].
    pub page_size_bytes: usize,
    /// Btree page size, in bytes. Must equal the device's atomic
    /// write unit (default 4 KiB).
    pub btree_page_bytes: usize,
    pub commit_batch_max: usize,
    pub commit_batch_deadline_us: u64,
    pub eviction_watermark: f32,
    pub probationary_fraction: f32,
    pub admission_sketch_multiplier: usize,
    pub singleflight_shards: usize,
    pub restart_scan_queue_depth: u32,
}

impl Default for EngineConfig {
    fn default() -> Self {
        Self {
            page_size_bytes: 2 * 1024 * 1024,
            btree_page_bytes: 4096,
            commit_batch_max: 1024,
            commit_batch_deadline_us: 200,
            eviction_watermark: 0.9,
            probationary_fraction: 0.1,
            admission_sketch_multiplier: 2,
            singleflight_shards: 64,
            restart_scan_queue_depth: 256,
        }
    }
}

/// Trivial async mutex. Single-threaded executor friendly: a
/// caller awaits a lock and the future resolves the next time
/// the holder drops their guard. Used here to serialize
/// [`BTreeIndex::apply_batch`] across concurrent
/// [`StorageEngine::write_page`] callers.
struct AsyncMutex {
    inner: Mutex<AsyncMutexInner>,
}

struct AsyncMutexInner {
    locked: bool,
    waiters: Vec<Waker>,
}

impl AsyncMutex {
    fn new() -> Self {
        Self {
            inner: Mutex::new(AsyncMutexInner {
                locked: false,
                waiters: Vec::new(),
            }),
        }
    }

    fn lock<'a>(&'a self) -> AsyncMutexLock<'a> {
        AsyncMutexLock { mutex: self }
    }
}

struct AsyncMutexLock<'a> {
    mutex: &'a AsyncMutex,
}

impl<'a> Future for AsyncMutexLock<'a> {
    type Output = AsyncMutexGuard<'a>;
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        let mut inner = self.mutex.inner.lock().unwrap();
        if !inner.locked {
            inner.locked = true;
            Poll::Ready(AsyncMutexGuard { mutex: self.mutex })
        } else {
            if !inner.waiters.iter().any(|w| w.will_wake(cx.waker())) {
                inner.waiters.push(cx.waker().clone());
            }
            Poll::Pending
        }
    }
}

struct AsyncMutexGuard<'a> {
    mutex: &'a AsyncMutex,
}

impl<'a> Drop for AsyncMutexGuard<'a> {
    fn drop(&mut self) {
        let wakers = {
            let mut inner = self.mutex.inner.lock().unwrap();
            inner.locked = false;
            std::mem::take(&mut inner.waiters)
        };
        for w in wakers {
            w.wake();
        }
    }
}

struct BufferpoolBinding {
    base: Option<*mut u8>,
    page_size: usize,
    page_count: usize,
}

pub struct StorageEngine<B: BlockDevice> {
    device: Arc<B>,
    allocator: Arc<Allocator>,
    refcount: Arc<RefcountTable>,
    btree: Arc<BTreeIndex<B>>,
    lru: Arc<SieveLru>,
    admission: Arc<AdmissionFilter>,
    singleflight: Arc<Singleflight>,
    cfg: EngineConfig,
    bufferpool: Mutex<BufferpoolBinding>,
    /// Reverse map LBA -> PageKey, used by evictions to remove
    /// stale btree entries. Guarded by the mutator gate.
    reverse: Mutex<HashMap<u64, PageKey>>,
    /// LBAs that have been logically orphaned (no btree entry,
    /// removed from the LRU) but cannot be freed yet because a
    /// reader still holds a pin. The mutator gate's holders
    /// drain this list whenever a candidate becomes unpinned.
    pending_free: Mutex<Vec<Lba>>,
    mutator_gate: AsyncMutex,
    metrics: EngineMetrics,
}

// SAFETY: the only `!Send + !Sync` field is `bufferpool.base`,
// which is an `Option<*mut u8>` pointing into a pinned `Backing`
// owned by the embedder (see `bufferpool::types::Backing`'s
// matching unsafe impl). Production deployments share one engine
// per disk across multiple shard threads (see `LocalStorage` and
// the design doc); the engine reads the pointer only while
// computing slices for `slice_*_from_ref`, and the multi-shard
// path goes through `ShardLocalStore` which never touches this
// field. All other fields are `Arc`/`Mutex` wrappers that are
// already `Send + Sync` because their `B: BlockDevice` is
// (`UringBlockDevice` and `MockDevice` are both `Send + Sync`).
unsafe impl<B: BlockDevice + Send + Sync> Send for StorageEngine<B> {}
unsafe impl<B: BlockDevice + Send + Sync> Sync for StorageEngine<B> {}

/// Cumulative engine counters. All `u64` so they can be read
/// atomically without RMW on x86; production wiring would use
/// the project's metrics crate.
#[derive(Default, Debug)]
struct EngineMetricsInner {
    hits: u64,
    misses: u64,
    admitted: u64,
    rejected_by_filter: u64,
    evictions: u64,
    write_io_errors: u64,
    read_io_errors: u64,
    checksum_misses: u64,
}

struct EngineMetrics {
    inner: Mutex<EngineMetricsInner>,
}

impl EngineMetrics {
    fn new() -> Self {
        Self {
            inner: Mutex::new(EngineMetricsInner::default()),
        }
    }
    fn snapshot(&self) -> EngineMetricsInner {
        let g = self.inner.lock().unwrap();
        EngineMetricsInner { ..*g }
    }
}

#[derive(Debug)]
pub struct EngineSnapshot {
    pub hits: u64,
    pub misses: u64,
    pub admitted: u64,
    pub rejected_by_filter: u64,
    pub evictions: u64,
    pub write_io_errors: u64,
    pub read_io_errors: u64,
    pub checksum_misses: u64,
    pub resident_pages: usize,
    pub btree_entries: usize,
}

impl<B: BlockDevice> StorageEngine<B> {
    pub async fn open(
        device: Arc<B>,
        cfg: EngineConfig,
    ) -> Result<Self, crate::storage::types::Error> {
        let capacity = device.capacity_pages();
        let allocator = Arc::new(Allocator::new(capacity));
        let refcount = Arc::new(RefcountTable::new(capacity));
        let btree = Arc::new(BTreeIndex::open(device.clone(), allocator.clone()).await?);
        let lru = Arc::new(SieveLru::new(capacity, refcount.clone()));
        let admission = Arc::new(AdmissionFilter::new(
            capacity,
            cfg.admission_sketch_multiplier.max(1) as u32,
        ));
        let singleflight = Arc::new(Singleflight::new(cfg.singleflight_shards.max(1)));
        Ok(Self {
            device,
            allocator,
            refcount,
            btree,
            lru,
            admission,
            singleflight,
            cfg,
            bufferpool: Mutex::new(BufferpoolBinding {
                base: None,
                page_size: 0,
                page_count: 0,
            }),
            reverse: Mutex::new(HashMap::new()),
            pending_free: Mutex::new(Vec::new()),
            mutator_gate: AsyncMutex::new(),
            metrics: EngineMetrics::new(),
        })
    }

    pub fn snapshot(&self) -> EngineSnapshot {
        let m = self.metrics.snapshot();
        EngineSnapshot {
            hits: m.hits,
            misses: m.misses,
            admitted: m.admitted,
            rejected_by_filter: m.rejected_by_filter,
            evictions: m.evictions,
            write_io_errors: m.write_io_errors,
            read_io_errors: m.read_io_errors,
            checksum_misses: m.checksum_misses,
            resident_pages: self.lru.len(),
            btree_entries: self.btree.live_entries(),
        }
    }

    fn page_key(stripe: &StripeKey, stripe_off: u64, page_bytes: usize) -> PageKey {
        let mut vh = [0u8; 32];
        vh.copy_from_slice(&stripe.0);
        let page_index = (stripe_off / page_bytes as u64) as u32;
        PageKey::new(vh, page_index)
    }

    /// SAFETY: caller must have called [`register_pages`] with a
    /// `base` valid for the lifetime of the engine; the slice is
    /// only valid until the next mutation of the pool's backing.
    unsafe fn slice_mut_from_ref(&self, page: PageRef) -> *mut [u8] {
        let bp = self.bufferpool.lock().unwrap();
        let base = bp.base.expect("register_pages not called");
        let p = unsafe {
            base.add(page.page_idx as usize * bp.page_size + page.offset as usize)
        };
        std::ptr::slice_from_raw_parts_mut(p, page.len as usize)
    }

    /// SAFETY: see [`slice_mut_from_ref`].
    unsafe fn slice_from_ref(&self, page: PageRef) -> *const [u8] {
        let bp = self.bufferpool.lock().unwrap();
        let base = bp.base.expect("register_pages not called");
        let p = unsafe {
            base.add(page.page_idx as usize * bp.page_size + page.offset as usize)
        };
        std::ptr::slice_from_raw_parts(p, page.len as usize)
    }

    fn metric<F: FnOnce(&mut EngineMetricsInner)>(&self, f: F) {
        f(&mut self.metrics.inner.lock().unwrap())
    }

    /// Reclaim every queued orphan LBA whose pin count has
    /// dropped to zero. Called opportunistically by the writer
    /// path so deferred frees don't accumulate indefinitely.
    fn drain_pending_free(&self) {
        let mut pending = self.pending_free.lock().unwrap();
        pending.retain(|lba| {
            if self.refcount.pin_count(lba.0).unwrap_or(0) == 0 {
                let _ = self.allocator.free(*lba);
                let _ = self.refcount.reset(lba.0);
                false
            } else {
                true
            }
        });
    }

    /// Detach `old` from the engine's bookkeeping and either
    /// free it immediately or queue it for later reclamation
    /// when its readers are done.
    fn retire_lba(&self, old: Lba) {
        {
            let mut rev = self.reverse.lock().unwrap();
            rev.remove(&old.0);
        }
        self.lru.forget(old);
        if self.refcount.pin_count(old.0).unwrap_or(0) == 0 {
            let _ = self.allocator.free(old);
            let _ = self.refcount.reset(old.0);
        } else {
            self.pending_free.lock().unwrap().push(old);
        }
    }

    fn evict_if_over_watermark<'a>(
        &'a self,
    ) -> Pin<Box<dyn Future<Output = ()> + 'a>> {
        // Boxed because called from inside an async fn that's
        // already 'a; eviction may itself await on the mutator.
        Box::pin(async move {
            if !self.lru.watermark_exceeded(self.cfg.eviction_watermark as f64) {
                return;
            }
            let candidates = self.lru.sweep(8);
            if candidates.is_empty() {
                return;
            }
            // Skip pinned LBAs - an in-flight reader still
            // references them. Re-admit so they remain tracked
            // and are reconsidered on a later sweep.
            let mut victims: Vec<Lba> = Vec::with_capacity(candidates.len());
            for lba in candidates {
                if self.refcount.pin_count(lba.0).unwrap_or(0) == 0 {
                    victims.push(lba);
                } else {
                    self.lru.admit(lba);
                }
            }
            if victims.is_empty() {
                return;
            }
            // Serialize against writers. The btree mutation must
            // commit BEFORE the allocator slot is released: a
            // concurrent writer that observes the stale btree
            // entry would otherwise call retire_lba and double-
            // free the slot the allocator already handed back to
            // a new write.
            let _g = self.mutator_gate.lock().await;
            let mut deletes: Vec<Mutation> = Vec::with_capacity(victims.len());
            {
                let rev = self.reverse.lock().unwrap();
                for lba in &victims {
                    if let Some(k) = rev.get(&lba.0).copied() {
                        deletes.push(Mutation::Delete { key: k });
                    }
                }
            }
            if !deletes.is_empty() && self.btree.apply_batch(deletes).await.is_err() {
                // Apply failed: leave the LBAs allocated and
                // tracked - a later sweep will retry.
                return;
            }
            {
                let mut rev = self.reverse.lock().unwrap();
                for lba in &victims {
                    rev.remove(&lba.0);
                }
            }
            for lba in &victims {
                let _ = self.allocator.free(*lba);
                let _ = self.refcount.reset(lba.0);
            }
            self.metric(|m| m.evictions += victims.len() as u64);
        })
    }
}

impl<B: BlockDevice> bufferpool::BlockStore for StorageEngine<B> {
    fn register_pages(
        &self,
        base: *mut u8,
        page_size: usize,
        page_count: usize,
    ) -> Result<(), bufferpool::Error> {
        let mut bp = self.bufferpool.lock().unwrap();
        bp.base = Some(base);
        bp.page_size = page_size;
        bp.page_count = page_count;
        // Best-effort: also register against the underlying
        // device for io_uring's fixed-buffer table. Errors here
        // are not fatal - the device falls back to its slower
        // path.
        let _ = self
            .device
            .register_buffers(base, page_size * page_count);
        Ok(())
    }

    async fn read_page(
        &self,
        key: StripeKey,
        stripe_off: u64,
        dst: PageRef,
    ) -> Result<bool, bufferpool::Error> {
        // SAFETY: register_pages installed a valid base; this
        // slice lives until we return from this future.
        let dst_buf: *mut [u8] = unsafe { self.slice_mut_from_ref(dst) };
        // SAFETY: see comment above.
        unsafe { self.read_page_into(key, stripe_off, dst_buf).await }
    }

    async fn write_page(
        &self,
        key: StripeKey,
        stripe_off: u64,
        page: PageRef,
    ) -> Result<(), bufferpool::Error> {
        // SAFETY: see register_pages contract.
        let src_buf: *const [u8] = unsafe { self.slice_from_ref(page) };
        // SAFETY: see comment above.
        unsafe { self.write_page_from(key, stripe_off, src_buf).await }
    }
}

impl<B: BlockDevice> StorageEngine<B> {
    /// Register an additional pinned region with the underlying
    /// block device. Unlike [`register_pages`] this does not
    /// install a "single backing" used by the engine's own slice
    /// helpers; it just makes the region eligible for fixed-buffer
    /// DMA. Callers that resolve `PageRef`s on their own (the
    /// per-shard `ShardLocalStore` wrapper) use this so multiple
    /// NUMA-local backings can coexist behind a shared engine.
    pub fn register_extra_buffer(
        &self,
        base: *mut u8,
        bytes: usize,
    ) -> Result<(), bufferpool::Error> {
        let _ = self.device.register_buffers(base, bytes);
        Ok(())
    }

    /// Read `(key, stripe_off)` into the byte range described by
    /// `dst`. The caller owns the slice and guarantees it is valid
    /// for writes for the duration of the returned future.
    ///
    /// SAFETY: `dst` must point to a writable region of `dst.len()`
    /// bytes that lives until the future resolves. The region must
    /// be pinned (the device may DMA into it directly).
    pub async unsafe fn read_page_into(
        &self,
        key: StripeKey,
        stripe_off: u64,
        dst: *mut [u8],
    ) -> Result<bool, bufferpool::Error> {
        let pk = Self::page_key(&key, stripe_off, self.cfg.page_size_bytes);

        let entry = match self.btree.lookup(&pk).await {
            Ok(Some(e)) => e,
            Ok(None) | Err(_) => {
                self.metric(|m| m.misses += 1);
                return Ok(false);
            }
        };

        let _pin = match self.refcount.pin(entry.lba.0) {
            Ok(g) => g,
            Err(_) => {
                self.metric(|m| m.misses += 1);
                return Ok(false);
            }
        };

        // SAFETY: caller-provided contract on `dst`.
        let dst_buf: &mut [u8] = unsafe { &mut *dst };

        if self.device.read(entry.lba, dst_buf).await.is_err() {
            self.metric(|m| {
                m.misses += 1;
                m.read_io_errors += 1;
            });
            return Ok(false);
        }

        let cs = Xxh3Checksum::checksum_of(dst_buf);
        if cs.0 != entry.data_checksum.0 {
            self.metric(|m| {
                m.misses += 1;
                m.checksum_misses += 1;
            });
            return Ok(false);
        }

        self.lru.touch(entry.lba);
        self.admission.record_frequency(&pk);
        self.metric(|m| m.hits += 1);
        Ok(true)
    }

    /// Write the contents of `src` to `(key, stripe_off)`. The
    /// caller owns the slice and guarantees it is valid for reads
    /// for the duration of the returned future.
    ///
    /// SAFETY: `src` must point to a readable region of `src.len()`
    /// bytes that lives until the future resolves. The region must
    /// be pinned (the device may DMA from it directly).
    pub async unsafe fn write_page_from(
        &self,
        key: StripeKey,
        stripe_off: u64,
        src: *const [u8],
    ) -> Result<(), bufferpool::Error> {
        let pk = Self::page_key(&key, stripe_off, self.cfg.page_size_bytes);

        if !self.admission.should_admit(&pk) {
            self.metric(|m| m.rejected_by_filter += 1);
            return Ok(());
        }

        let acquire = self.singleflight.acquire(pk);
        let leader = match acquire {
            Acquire::Leader(g) => g,
            Acquire::Follower(_w) => {
                return Ok(());
            }
        };

        let lba = match self.allocator.alloc() {
            Ok(l) => l,
            Err(_) => {
                leader.abandon();
                return Ok(());
            }
        };

        // SAFETY: caller-provided contract on `src`.
        let src_buf: &[u8] = unsafe { &*src };

        if self.device.write(lba, src_buf).await.is_err() {
            self.metric(|m| m.write_io_errors += 1);
            let _ = self.allocator.free(lba);
            leader.abandon();
            return Ok(());
        }

        let cs = Xxh3Checksum::checksum_of(src_buf);
        let entry = LeafEntry {
            lba,
            data_checksum: cs,
            byte_len: src_buf.len() as u32,
        };

        let prior_lba: Option<Lba>;
        {
            let _g = self.mutator_gate.lock().await;
            prior_lba = self
                .btree
                .lookup(&pk)
                .await
                .ok()
                .flatten()
                .map(|e| e.lba);
            if self
                .btree
                .apply_batch(vec![Mutation::Insert {
                    key: pk,
                    value: entry,
                }])
                .await
                .is_err()
            {
                self.metric(|m| m.write_io_errors += 1);
                let _ = self.allocator.free(lba);
                leader.abandon();
                return Ok(());
            }
        }

        if let Some(old) = prior_lba {
            self.retire_lba(old);
        }

        {
            let mut rev = self.reverse.lock().unwrap();
            rev.insert(lba.0, pk);
        }
        self.lru.admit(lba);
        self.metric(|m| m.admitted += 1);
        leader.publish(entry);

        self.drain_pending_free();
        self.evict_if_over_watermark().await;

        Ok(())
    }
}

// The BulkRef parameter on BlockStore impls is unused but
// referenced by the bufferpool crate's import statement; pull it
// in here so the symbol is reachable from the module tree.
#[allow(dead_code)]
fn _unused_bulkref() -> Option<BulkRef> {
    None
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::bufferpool::BlockStore;
    use crate::storage::blockdev::{MockDevice, MockDeviceConfig};
    use crate::storage::types::Lba;
    use std::future::Future;
    use std::pin::pin;
    use std::task::{Context, Poll, RawWaker, RawWakerVTable, Waker};

    fn noop_waker() -> Waker {
        fn raw() -> RawWaker {
            RawWaker::new(std::ptr::null(), &VTABLE)
        }
        static VTABLE: RawWakerVTable =
            RawWakerVTable::new(|_| raw(), |_| {}, |_| {}, |_| {});
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

    fn engine(capacity: u64) -> (StorageEngine<MockDevice>, Box<[u8]>) {
        let device = Arc::new(MockDevice::new(MockDeviceConfig {
            page_size: 4096,
            capacity_pages: capacity,
            ..Default::default()
        }));
        let mut cfg = EngineConfig::default();
        cfg.page_size_bytes = 4096; // collapse cache page == device block for tests
        cfg.btree_page_bytes = 4096;
        let eng = block_on(StorageEngine::open(device, cfg)).unwrap();
        // 64 pool pages of 4 KiB each.
        let buf: Box<[u8]> = vec![0u8; 4096 * 64].into_boxed_slice();
        eng.register_pages(buf.as_ptr() as *mut u8, 4096, 64).unwrap();
        (eng, buf)
    }

    fn stripe(i: u8) -> StripeKey {
        let mut s = [0u8; 32];
        s[0] = i;
        StripeKey(s)
    }

    #[test]
    fn write_then_read_roundtrip() {
        let (eng, buf) = engine(256);
        let src_page = PageRef {
            page_idx: 0,
            offset: 0,
            len: 4096,
        };
        // Fill page idx 0 with a pattern.
        unsafe {
            let p = buf.as_ptr() as *mut u8;
            for i in 0..4096 {
                p.add(i).write(((i * 37) & 0xff) as u8);
            }
        }
        // First write: admission filter rejects (first touch).
        block_on(eng.write_page(stripe(1), 0, src_page)).unwrap();
        assert_eq!(eng.snapshot().rejected_by_filter, 1);
        // Second write: admitted.
        block_on(eng.write_page(stripe(1), 0, src_page)).unwrap();
        assert_eq!(eng.snapshot().admitted, 1);

        // Read into page idx 1.
        let dst_page = PageRef {
            page_idx: 1,
            offset: 0,
            len: 4096,
        };
        let hit = block_on(eng.read_page(stripe(1), 0, dst_page)).unwrap();
        assert!(hit, "expected cache hit");
        // Verify bytes match source pattern.
        unsafe {
            let p = buf.as_ptr().add(4096);
            for i in 0..4096 {
                assert_eq!(
                    p.add(i).read(),
                    ((i * 37) & 0xff) as u8,
                    "mismatch at byte {i}"
                );
            }
        }
    }

    #[test]
    fn read_unknown_key_is_miss() {
        let (eng, _) = engine(64);
        let dst = PageRef {
            page_idx: 0,
            offset: 0,
            len: 4096,
        };
        let hit = block_on(eng.read_page(stripe(7), 0, dst)).unwrap();
        assert!(!hit);
        assert_eq!(eng.snapshot().misses, 1);
    }

    #[test]
    fn admission_filter_drops_first_touch() {
        let (eng, _) = engine(64);
        let src = PageRef {
            page_idx: 0,
            offset: 0,
            len: 4096,
        };
        block_on(eng.write_page(stripe(9), 0, src)).unwrap();
        let s = eng.snapshot();
        assert_eq!(s.rejected_by_filter, 1);
        assert_eq!(s.admitted, 0);
    }

    #[test]
    fn checksum_mismatch_reports_miss() {
        let (eng, buf) = engine(64);
        let src = PageRef {
            page_idx: 0,
            offset: 0,
            len: 4096,
        };
        // Admit a page.
        block_on(eng.write_page(stripe(3), 0, src)).unwrap();
        block_on(eng.write_page(stripe(3), 0, src)).unwrap();
        assert_eq!(eng.snapshot().admitted, 1);
        // Corrupt the underlying device storage.
        // Find the LBA from the btree.
        let mut bytes = [0u8; 4096];
        // Page 0 in the cache lives at some allocated LBA >= 2.
        // Scan to find it.
        let device: &MockDevice = &eng.device;
        let mut data_lba: Option<u64> = None;
        for lba in 2..64 {
            device.peek(Lba(lba), &mut bytes);
            // Look for our pattern (zeros in buf at page_idx 0).
            if bytes.iter().all(|&b| b == 0) {
                data_lba = Some(lba);
                break;
            }
        }
        let lba = data_lba.expect("data page on disk");
        let mut bad = [0u8; 4096];
        bad[0] = 0xFF;
        device.poke(Lba(lba), &bad);

        // Now lookup should checksum-miss.
        let dst = PageRef {
            page_idx: 1,
            offset: 0,
            len: 4096,
        };
        let hit = block_on(eng.read_page(stripe(3), 0, dst)).unwrap();
        assert!(!hit);
        assert!(eng.snapshot().checksum_misses >= 1);
        let _ = buf; // keep alive
    }
}
