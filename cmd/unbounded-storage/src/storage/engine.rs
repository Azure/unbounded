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

use crate::bufferpool::{self, BulkRef, PageRef, StripeKey};
use crate::storage::admission::AdmissionFilter;
use crate::storage::alloc::Allocator;
use crate::storage::blockdev::BlockDevice;
use crate::storage::btree::{BTreeIndex, LeafEntry, Mutation};
use crate::storage::lru::SieveLru;
use crate::storage::mutator::{MutatorOutcome, MutatorQueue, MutatorReply, MutatorReq, yield_once};
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
    /// When true, `write_page` skips AdmissionFilter::should_admit
    /// and always proceeds. Intended for benchmarking/tooling;
    /// production should leave this false.
    pub bypass_admission: bool,
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
            bypass_admission: false,
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
    /// stale btree entries. Only the mutator task and writers
    /// holding their own freshly-allocated LBA mutate this map;
    /// readers are read-only consumers.
    reverse: Mutex<HashMap<u64, PageKey>>,
    /// LBAs that have been logically orphaned (no btree entry,
    /// removed from the LRU) but cannot be freed yet because a
    /// reader still holds a pin. The mutator task drains this
    /// list once per committed batch.
    pending_free: Mutex<Vec<Lba>>,
    /// Single-consumer queue: writers and the eviction path
    /// enqueue here, the engine's `run_mutator` task drains and
    /// commits batches.
    mutator_queue: Arc<MutatorQueue>,
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
    /// Length of the deferred-reclaim queue: LBAs that were
    /// displaced (overwrite or eviction) while their old page
    /// was still pinned and that have not yet been returned to
    /// the allocator. Exposed for DST observability; at end-of-
    /// run quiescence this should drain to zero.
    pub pending_free_len: usize,
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
            mutator_queue: Arc::new(MutatorQueue::new()),
            metrics: EngineMetrics::new(),
        })
    }

    /// Borrow the underlying block device. Used by `LocalStorage`
    /// to fan out [`BlockDevice::progress`] across every engine on
    /// the node without needing a separate handle to each device.
    pub(crate) fn device(&self) -> &Arc<B> {
        &self.device
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
            pending_free_len: self.pending_free.lock().unwrap().len(),
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
        let p = unsafe { base.add(page.page_idx as usize * bp.page_size + page.offset as usize) };
        std::ptr::slice_from_raw_parts_mut(p, page.len as usize)
    }

    /// SAFETY: see [`slice_mut_from_ref`].
    unsafe fn slice_from_ref(&self, page: PageRef) -> *const [u8] {
        let bp = self.bufferpool.lock().unwrap();
        let base = bp.base.expect("register_pages not called");
        let p = unsafe { base.add(page.page_idx as usize * bp.page_size + page.offset as usize) };
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

    fn evict_if_over_watermark<'a>(&'a self) -> Pin<Box<dyn Future<Output = ()> + 'a>> {
        // Boxed because called from inside an async fn that's
        // already 'a; eviction may itself await on the mutator.
        Box::pin(async move {
            if !self
                .lru
                .watermark_exceeded(self.cfg.eviction_watermark as f64)
            {
                return;
            }
            // Routing the sweep through the mutator is the
            // correctness invariant for eviction: victim
            // selection, reverse-map resolution, and the btree
            // Delete must all happen inside the mutator's
            // single-committer critical section, otherwise a
            // concurrent overwrite of a victim key can be
            // clobbered by an Evict batch that captured the
            // pre-overwrite (key, lba) pair. See S6 in
            // designs/.../storage.md and the regression test
            // `eviction_lock_ordering_no_double_free` in
            // tests/storage/recovery.rs.
            let done = MutatorReply::new();
            self.mutator_queue.push(MutatorReq::Evict {
                count: 8,
                done: done.clone(),
            });
            let freed = match done.wait().await {
                MutatorOutcome::EvictCommitted { freed } => freed,
                _ => return,
            };
            if freed.is_empty() {
                return;
            }
            {
                let mut rev = self.reverse.lock().unwrap();
                for lba in &freed {
                    rev.remove(&lba.0);
                }
            }
            for lba in &freed {
                let _ = self.allocator.free(*lba);
                let _ = self.refcount.reset(lba.0);
            }
            self.metric(|m| m.evictions += freed.len() as u64);
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
        let _ = self.device.register_buffers(base, page_size * page_count);
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

        if !self.cfg.bypass_admission && !self.admission.should_admit(&pk) {
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

        // Submit the insert to the mutator and wait for its
        // batched commit. The mutator returns the prior LBA the
        // btree mapped this key to (if any) so we can retire it.
        let done = MutatorReply::new();
        self.mutator_queue.push(MutatorReq::Insert {
            key: pk,
            entry,
            lba,
            done: done.clone(),
        });
        let prior_lba = match done.wait().await {
            MutatorOutcome::InsertCommitted { prior_lba } => prior_lba,
            MutatorOutcome::Failed => {
                self.metric(|m| m.write_io_errors += 1);
                let _ = self.allocator.free(lba);
                leader.abandon();
                return Ok(());
            }
            MutatorOutcome::DeleteCommitted | MutatorOutcome::EvictCommitted { .. } => {
                unreachable!("mutator returned a non-Insert outcome for an Insert request")
            }
        };

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

        self.evict_if_over_watermark().await;

        Ok(())
    }

    /// Drive the single-committer mutator loop. The engine owns
    /// the only consumer; call this exactly once per engine and
    /// keep it alive for the lifetime of the engine. Returns
    /// when [`close_mutator`] is called and the queue has
    /// drained.
    pub async fn run_mutator(self: Arc<Self>) {
        loop {
            // Park until something is queued or the engine is
            // shutting down. `false` here means "closed and
            // empty"; drain and exit.
            if !self.mutator_queue.wait_nonempty().await {
                return;
            }
            let max = self.cfg.commit_batch_max.max(1);
            let mut batch = self.mutator_queue.try_drain_up_to(max);
            // Best-effort coalescing: if we did not fill the
            // batch, yield once to let other producers enqueue
            // before we commit. Wall-clock-free approximation of
            // `commit_batch_deadline_us`; see the note on
            // `mutator::yield_once`.
            if batch.len() < max {
                yield_once().await;
                if batch.len() < max {
                    let more = self.mutator_queue.try_drain_up_to(max - batch.len());
                    batch.extend(more);
                }
            }

            self.process_batch(batch).await;
            self.drain_pending_free();
        }
    }

    /// Signal the mutator loop to exit once it has drained. Safe
    /// to call from any task; idempotent. Primarily used by
    /// tests and shutdown paths.
    pub fn close_mutator(&self) {
        self.mutator_queue.close();
    }

    /// Number of requests currently buffered in the mutator queue.
    /// Tests use this after `close_mutator` + `run_mutator` join
    /// to assert the mutator drained every submitted request
    /// rather than exiting with backlog.
    pub fn mutator_pending_len(&self) -> usize {
        self.mutator_queue.pending_len()
    }

    async fn process_batch(&self, batch: Vec<MutatorReq>) {
        if batch.is_empty() {
            return;
        }
        // Look up prior LBAs for every Insert before applying;
        // the mutator is the single committer so these reads are
        // consistent with what `apply_batch` is about to replace.
        // Singleflight guarantees at most one Insert per key is
        // in flight at a time, so an Insert key cannot also
        // appear in a Delete in the same batch (a Delete only
        // comes from eviction, which would not target a key that
        // has an in-flight write because the LBA is admitted to
        // the LRU only after publish). Debug-assert the simpler
        // condition: each insert key appears at most once.
        #[cfg(debug_assertions)]
        {
            use std::collections::HashSet;
            let mut seen: HashSet<PageKey> = HashSet::new();
            for r in &batch {
                if let MutatorReq::Insert { key, .. } = r {
                    debug_assert!(
                        seen.insert(*key),
                        "singleflight violated: duplicate Insert key in one batch",
                    );
                }
            }
        }

        // Per-request output we need to retain across apply_batch
        // so we can build the right `MutatorOutcome` after commit:
        //  - Insert: prior_lba seen by this commit.
        //  - Delete: nothing per-request.
        //  - Evict: the list of LBAs whose Delete mutations were
        //    added to this batch, in the order they were enqueued.
        enum RequestRecord {
            Insert { prior_lba: Option<Lba> },
            Delete,
            Evict { freed: Vec<Lba> },
        }

        let mut records: Vec<RequestRecord> = Vec::with_capacity(batch.len());
        let mut mutations: Vec<Mutation> = Vec::new();
        // Track keys with a pending Insert in this batch so an
        // Evict request anywhere in the same batch does not
        // queue a Delete that would either:
        //   (a) when ordered before the Insert in apply_batch,
        //       race the original writer's retire_lba on the
        //       same prior LBA (double free); or
        //   (b) when ordered after the Insert, clobber the
        //       freshly inserted mapping (wrong bytes / lost
        //       update).
        // Pre-populate this map BEFORE the main loop so Evict
        // requests positioned earlier in the batch than their
        // colliding Insert still see the pending Insert and skip
        // that victim. The writer that issued the Insert will
        // retire the prior LBA via the InsertCommitted reply
        // and is solely responsible for freeing it.
        let mut pending_inserts: HashMap<PageKey, Lba> = HashMap::new();
        for req in &batch {
            if let MutatorReq::Insert { key, entry, .. } = req {
                pending_inserts.insert(*key, entry.lba);
            }
        }
        for req in &batch {
            match req {
                MutatorReq::Insert { key, entry, .. } => {
                    let prior = self.btree.lookup(key).await.ok().flatten().map(|e| e.lba);
                    records.push(RequestRecord::Insert { prior_lba: prior });
                    mutations.push(Mutation::Insert {
                        key: *key,
                        value: *entry,
                    });
                }
                MutatorReq::Delete { keys, .. } => {
                    records.push(RequestRecord::Delete);
                    for k in keys {
                        mutations.push(Mutation::Delete { key: *k });
                    }
                }
                MutatorReq::Evict { count, .. } => {
                    // Victim selection MUST happen here, inside
                    // the mutator's serialized region, so that
                    // the (lba -> key) resolution and the
                    // matching Delete enter `apply_batch`
                    // together. Pre-sweep selection followed by
                    // a separate Delete submission lets a
                    // concurrent Insert overwrite a victim key
                    // and have its fresh mapping deleted instead
                    // (S6).
                    let candidates = self.lru.sweep(*count);
                    let mut freed: Vec<Lba> = Vec::with_capacity(candidates.len());
                    for lba in candidates {
                        if self.refcount.pin_count(lba.0).unwrap_or(0) != 0 {
                            // Pinned: preserve current behavior
                            // and re-admit to the head. (Issue
                            // S5 - re-admitting to MRU is its
                            // own bug, tracked separately.)
                            self.lru.admit(lba);
                            continue;
                        }
                        let key = {
                            let rev = self.reverse.lock().unwrap();
                            rev.get(&lba.0).copied()
                        };
                        let Some(key) = key else {
                            // No reverse mapping: the LBA is
                            // orphaned or about to be freed by
                            // a writer's retire_lba. Drop it
                            // from our victim set and do not
                            // re-admit; the LBA's lifecycle is
                            // already owned elsewhere.
                            continue;
                        };
                        // Confirm the live btree mapping for
                        // `key` still points at `lba`. If a
                        // concurrent Insert in this very batch
                        // (or a prior batch whose retire_lba
                        // has not yet run) has rewritten the
                        // key to a different LBA, deleting
                        // `key` would clobber that fresh
                        // mapping. Skip such victims. Check
                        // the in-batch pending Inserts first
                        // (they will overwrite the committed
                        // mapping in `apply_batch`), then fall
                        // back to the live snapshot.
                        if let Some(&new_lba) = pending_inserts.get(&key) {
                            if new_lba != lba {
                                continue;
                            }
                            // An in-batch insert that points at
                            // the same LBA we're about to evict
                            // is nonsensical (the writer must
                            // have allocated a fresh LBA), but
                            // be defensive: drop the victim.
                            continue;
                        }
                        let live = self.btree.lookup(&key).await.ok().flatten();
                        match live {
                            Some(e) if e.lba == lba => {
                                mutations.push(Mutation::Delete { key });
                                freed.push(lba);
                            }
                            _ => {
                                // The key was rewritten or
                                // already deleted. Leave the
                                // LBA alone; whoever displaced
                                // it owns the free.
                            }
                        }
                    }
                    records.push(RequestRecord::Evict { freed });
                }
            }
        }

        let ok = if mutations.is_empty() {
            true
        } else {
            self.btree.apply_batch(mutations).await.is_ok()
        };

        for (i, req) in batch.into_iter().enumerate() {
            let done = match &req {
                MutatorReq::Insert { done, .. }
                | MutatorReq::Delete { done, .. }
                | MutatorReq::Evict { done, .. } => done.clone(),
            };
            let outcome = if !ok {
                MutatorOutcome::Failed
            } else {
                match std::mem::replace(&mut records[i], RequestRecord::Delete) {
                    RequestRecord::Insert { prior_lba } => {
                        MutatorOutcome::InsertCommitted { prior_lba }
                    }
                    RequestRecord::Delete => MutatorOutcome::DeleteCommitted,
                    RequestRecord::Evict { freed } => MutatorOutcome::EvictCommitted { freed },
                }
            };
            done.set(outcome);
        }
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
    use std::pin::{Pin, pin};
    use std::task::{Context, Poll, RawWaker, RawWakerVTable, Waker};

    fn noop_waker() -> Waker {
        fn raw() -> RawWaker {
            RawWaker::new(std::ptr::null(), &VTABLE)
        }
        static VTABLE: RawWakerVTable = RawWakerVTable::new(|_| raw(), |_| {}, |_| {}, |_| {});
        unsafe { Waker::from_raw(raw()) }
    }

    /// Pump `body` to completion while concurrently driving
    /// `mutator`. The mutator is expected to stay pending until
    /// `close_mutator` is invoked; we poll it on every spin so
    /// reply wakeups are observed promptly.
    fn block_on_pair<F: Future>(body: F, mutator: Pin<&mut dyn Future<Output = ()>>) -> F::Output {
        let w = noop_waker();
        let mut cx = Context::from_waker(&w);
        let mut body = pin!(body);
        let mut mutator = mutator;
        let mut spins = 0u64;
        loop {
            // Drive the mutator first so that any replies it
            // produces this spin are visible to the body's poll.
            let _ = mutator.as_mut().poll(&mut cx);
            match body.as_mut().poll(&mut cx) {
                Poll::Ready(v) => return v,
                Poll::Pending => {
                    spins += 1;
                    assert!(spins < 1_000_000, "stuck");
                }
            }
        }
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

    fn engine(capacity: u64) -> (Arc<StorageEngine<MockDevice>>, Box<[u8]>) {
        let device = Arc::new(MockDevice::new(MockDeviceConfig {
            page_size: 4096,
            capacity_pages: capacity,
            ..Default::default()
        }));
        let mut cfg = EngineConfig::default();
        cfg.page_size_bytes = 4096; // collapse cache page == device block for tests
        cfg.btree_page_bytes = 4096;
        let eng = Arc::new(block_on(StorageEngine::open(device, cfg)).unwrap());
        // 64 pool pages of 4 KiB each.
        let buf: Box<[u8]> = vec![0u8; 4096 * 64].into_boxed_slice();
        eng.register_pages(buf.as_ptr() as *mut u8, 4096, 64)
            .unwrap();
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
        let eng_body = eng.clone();
        let mutator = eng.clone().run_mutator();
        let mut mutator = pin!(mutator);
        let body = async move {
            let src_page = PageRef {
                page_idx: 0,
                offset: 0,
                len: 4096,
            };
            unsafe {
                let p = buf.as_ptr() as *mut u8;
                for i in 0..4096 {
                    p.add(i).write(((i * 37) & 0xff) as u8);
                }
            }
            // First write: admission filter rejects (first touch).
            eng_body.write_page(stripe(1), 0, src_page).await.unwrap();
            assert_eq!(eng_body.snapshot().rejected_by_filter, 1);
            // Second write: admitted.
            eng_body.write_page(stripe(1), 0, src_page).await.unwrap();
            assert_eq!(eng_body.snapshot().admitted, 1);

            let dst_page = PageRef {
                page_idx: 1,
                offset: 0,
                len: 4096,
            };
            let hit = eng_body.read_page(stripe(1), 0, dst_page).await.unwrap();
            assert!(hit, "expected cache hit");
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
            eng_body.close_mutator();
        };
        block_on_pair(body, mutator.as_mut());
    }

    #[test]
    fn read_unknown_key_is_miss() {
        let (eng, _buf) = engine(64);
        // No writes, no need to drive the mutator: reads do not
        // enqueue. Plain block_on suffices.
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
        let (eng, _buf) = engine(64);
        // First write is rejected pre-mutator; mutator never sees
        // anything so plain block_on is fine.
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
    fn bypass_admission_admits_first_touch() {
        // With bypass_admission = true, a single write to a fresh
        // key skips the doorkeeper rejection and goes through to
        // singleflight + alloc + device write.
        let device = Arc::new(MockDevice::new(MockDeviceConfig {
            page_size: 4096,
            capacity_pages: 64,
            ..Default::default()
        }));
        let cfg = EngineConfig {
            page_size_bytes: 4096,
            btree_page_bytes: 4096,
            bypass_admission: true,
            ..Default::default()
        };
        let eng = Arc::new(block_on(StorageEngine::open(device, cfg)).unwrap());
        let buf: Box<[u8]> = vec![0u8; 4096 * 64].into_boxed_slice();
        eng.register_pages(buf.as_ptr() as *mut u8, 4096, 64)
            .unwrap();

        let eng_body = eng.clone();
        let mutator = eng.clone().run_mutator();
        let mut mutator = pin!(mutator);
        let body = async move {
            let src = PageRef {
                page_idx: 0,
                offset: 0,
                len: 4096,
            };
            eng_body.write_page(stripe(11), 0, src).await.unwrap();
            let s = eng_body.snapshot();
            assert_eq!(s.admitted, 1);
            assert_eq!(s.rejected_by_filter, 0);
            eng_body.close_mutator();
        };
        block_on_pair(body, mutator.as_mut());
    }

    #[test]
    fn checksum_mismatch_reports_miss() {
        let (eng, buf) = engine(64);
        let eng_body = eng.clone();
        let mutator = eng.clone().run_mutator();
        let mut mutator = pin!(mutator);
        let body = async move {
            let src = PageRef {
                page_idx: 0,
                offset: 0,
                len: 4096,
            };
            eng_body.write_page(stripe(3), 0, src).await.unwrap();
            eng_body.write_page(stripe(3), 0, src).await.unwrap();
            assert_eq!(eng_body.snapshot().admitted, 1);
            let device: &MockDevice = &eng_body.device;
            let mut bytes = [0u8; 4096];
            let mut data_lba: Option<u64> = None;
            for lba in 2..64 {
                device.peek(Lba(lba), &mut bytes);
                if bytes.iter().all(|&b| b == 0) {
                    data_lba = Some(lba);
                    break;
                }
            }
            let lba = data_lba.expect("data page on disk");
            let mut bad = [0u8; 4096];
            bad[0] = 0xFF;
            device.poke(Lba(lba), &bad);

            let dst = PageRef {
                page_idx: 1,
                offset: 0,
                len: 4096,
            };
            let hit = eng_body.read_page(stripe(3), 0, dst).await.unwrap();
            assert!(!hit);
            assert!(eng_body.snapshot().checksum_misses >= 1);
            eng_body.close_mutator();
            let _ = buf;
        };
        block_on_pair(body, mutator.as_mut());
    }

    /// Stand-in for the `bench storage block` executor + workload when
    /// hugepages are not available on the host: drive ~64 writes
    /// followed by ~64 reads through a `bypass_admission` engine
    /// over `MockDevice` and assert the bench's success criteria
    /// (ops produced, no errors, every read is a cache hit).
    #[test]
    fn bench_mock_write_then_read_roundtrip() {
        const PAGES: usize = 64;
        const PAGE: usize = 4096;
        let device = Arc::new(MockDevice::new(MockDeviceConfig {
            page_size: PAGE,
            capacity_pages: 256,
            ..Default::default()
        }));
        let cfg = EngineConfig {
            page_size_bytes: PAGE,
            btree_page_bytes: PAGE,
            bypass_admission: true,
            ..Default::default()
        };
        let eng = Arc::new(block_on(StorageEngine::open(device, cfg)).unwrap());
        let buf: Box<[u8]> = vec![0u8; PAGE * PAGES].into_boxed_slice();
        eng.register_pages(buf.as_ptr() as *mut u8, PAGE, PAGES)
            .unwrap();

        let eng_body = eng.clone();
        let mutator = eng.clone().run_mutator();
        let mut mutator = pin!(mutator);
        let body = async move {
            let src = PageRef {
                page_idx: 0,
                offset: 0,
                len: PAGE as u32,
            };
            // Phase 1: 64 writes against distinct keys.
            for i in 0..PAGES as u8 {
                eng_body.write_page(stripe(i), 0, src).await.unwrap();
            }
            let s = eng_body.snapshot();
            assert_eq!(s.admitted, PAGES as u64);
            assert_eq!(s.write_io_errors, 0);

            // Phase 2: read every key back. Each must hit.
            let dst = PageRef {
                page_idx: 1,
                offset: 0,
                len: PAGE as u32,
            };
            for i in 0..PAGES as u8 {
                let hit = eng_body.read_page(stripe(i), 0, dst).await.unwrap();
                assert!(hit, "miss on key {i}");
            }
            let s = eng_body.snapshot();
            assert_eq!(s.hits, PAGES as u64);
            assert_eq!(s.read_io_errors, 0);
            assert_eq!(s.checksum_misses, 0);
            eng_body.close_mutator();
        };
        block_on_pair(body, mutator.as_mut());
    }
}
