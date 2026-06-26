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

use crate::bufferpool::{self, PageRef, StripeKey};
use crate::storage::admission::AdmissionFilter;
use crate::storage::alloc::Allocator;
use crate::storage::blockdev::{BlockDevice, ScratchPool};
use crate::storage::btree::{BTreeIndex, LeafEntry, Mutation};
use crate::storage::lru::SieveLru;
use crate::storage::mutator::{MutatorOutcome, MutatorQueue, MutatorReply, MutatorReq};
use crate::storage::refcount::RefcountTable;
use crate::storage::singleflight::{Acquire, Singleflight};
use crate::storage::traits::{PageChecksum, Xxh3Checksum};
use crate::storage::types::{Lba, PageKey};

#[derive(Clone, Debug)]
pub struct EngineConfig {
    /// Cache page size, in bytes. Default 2 MiB. Must be a
    /// multiple of [`Self::btree_page_bytes`].
    pub page_size_bytes: usize,
    /// Btree page size, in bytes. Must equal the device's atomic
    /// write unit (default 4 KiB).
    pub btree_page_bytes: usize,
    /// Maximum number of mutator requests folded into a single
    /// `BTreeIndex::apply_batch` commit. Larger batches amortize
    /// the CoW B+tree commit cost; the mutator never exceeds this
    /// per commit.
    pub commit_batch_max: usize,
    /// Target batch-coalescing latency, in microseconds. This is a
    /// logical hint, not a value the engine reads as elapsed time:
    /// the deterministic-simulation harness forbids reading the
    /// wall clock, so the mutator realizes this budget as
    /// [`Self::commit_batch_ticks`] cooperative yields rather than
    /// a real deadline. Retained for documentation and future
    /// production tuning.
    pub commit_batch_deadline_us: u64,
    /// Number of cooperative yields the mutator may spend waiting
    /// for more requests to coalesce into a batch that has not yet
    /// reached [`Self::commit_batch_max`]. This is the
    /// wall-clock-free realization of
    /// [`Self::commit_batch_deadline_us`]: each yield lets the
    /// executor interleave producers that are about to enqueue, so
    /// the batch grows under load while a drained or closed queue
    /// commits immediately. Default 8 (roughly
    /// `commit_batch_deadline_us / 25us` per yield); 0 disables
    /// coalescing.
    pub commit_batch_ticks: u32,
    pub eviction_watermark: f32,
    pub probationary_fraction: f32,
    pub admission_sketch_multiplier: usize,
    pub singleflight_shards: usize,
    pub restart_scan_queue_depth: u32,
    /// Number of 4 KiB registered scratch buffers reserved for
    /// btree / meta I/O. This is the hard ceiling on how many
    /// btree page writes a single path-copy commit can keep in
    /// flight at once: the CoW commit path submits every
    /// independent spine-page write concurrently and joins them,
    /// so a larger pool lets a commit overlap more of its 4 KiB
    /// writes (trading registered-buffer memory for commit
    /// latency). At `btree_page_bytes` = 4 KiB the default 64
    /// costs 256 KiB of pinned memory per shard and sits well
    /// under the io_uring queue depth; commits that need more
    /// buffers than this simply drain in waves via backpressure
    /// on the pool. Must be at least a few to cover meta load
    /// plus an in-flight lookup; values below 4 are clamped up.
    pub btree_scratch_pages: usize,
    /// When true, `write_page` skips AdmissionFilter::should_admit
    /// and always proceeds. Intended for benchmarking/tooling;
    /// production should leave this false.
    pub bypass_admission: bool,
    /// When true, reads use the committed in-memory index mirror instead
    /// of reading the terminal btree leaf from disk. This is benchmark
    /// only: production reads must hit the on-disk leaf so corruption and
    /// snapshot semantics are preserved.
    pub bypass_index_read: bool,
    /// When true, reads skip data checksum validation. This is benchmark
    /// only: production reads must verify cached bytes before returning a hit.
    pub bypass_checksum: bool,
    /// When true, [`BTreeIndex::open`] skips the LBA-order leaf
    /// scan on disks that have no valid meta page. Intended for
    /// benchmarking and bring-up against freshly wiped devices
    /// where the full-disk scan would otherwise take many minutes
    /// per terabyte. Production should leave this false so partial
    /// recovery still runs when the meta slots are corrupted.
    pub skip_recovery_scan_if_no_meta: bool,
    /// When true, ignore any existing on-disk btree metadata and
    /// bootstrap an empty cache index. This is destructive: old cache
    /// entries become unreachable and may be overwritten. Intended for
    /// benchmark devices that are explicitly reset between runs.
    pub force_format: bool,
    /// Stable identifier for this disk, used as the `disk` label on
    /// the engine's Prometheus metrics. Empty when unset (e.g. in
    /// tests and benchmarks); production wiring sets it from the
    /// `DiskSpec` id.
    pub disk_id: String,
}

impl Default for EngineConfig {
    fn default() -> Self {
        Self {
            page_size_bytes: 2 * 1024 * 1024,
            btree_page_bytes: 4096,
            commit_batch_max: 1024,
            commit_batch_deadline_us: 200,
            commit_batch_ticks: 8,
            eviction_watermark: 0.9,
            probationary_fraction: 0.1,
            admission_sketch_multiplier: 2,
            singleflight_shards: 64,
            restart_scan_queue_depth: 256,
            btree_scratch_pages: 64,
            bypass_admission: false,
            bypass_index_read: false,
            bypass_checksum: false,
            skip_recovery_scan_if_no_meta: false,
            force_format: false,
            disk_id: String::new(),
        }
    }
}

struct BufferpoolBinding {
    base: Option<*mut u8>,
    page_size: usize,
    page_count: usize,
}

/// Floor on the btree / meta scratch-pool size, independent of
/// [`EngineConfig::btree_scratch_pages`]. Covers the worst-case
/// concurrent demand that is not commit-write parallelism: two
/// pages for [`crate::storage::btree::meta`] load, one for an
/// in-flight lookup, one for a `build_tree` write, plus a little
/// slack. The configured value is clamped up to this floor so a
/// small or zero setting cannot starve those paths.
const MIN_BTREE_SCRATCH_PAGES: usize = 8;

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
    /// Reverse map start-LBA -> (PageKey, byte_len). Eviction
    /// targets pick a start LBA from the LRU; the entry's
    /// byte_len tells the engine how many contiguous LBAs the
    /// run covers so the whole run is freed atomically.
    reverse: Mutex<HashMap<u64, (PageKey, u32)>>,
    /// Runs that have been logically orphaned (no btree entry,
    /// removed from the LRU) but cannot be freed yet because a
    /// reader still holds a pin on the start LBA. Drained by the
    /// mutator once per committed batch. Each entry is
    /// `(start_lba, n_pages)`.
    pending_free: Mutex<Vec<(Lba, u64)>>,
    /// Single-consumer queue: writers and the eviction path
    /// enqueue here, the engine's `run_mutator` task drains and
    /// commits batches.
    mutator_queue: Arc<MutatorQueue>,
    metrics: EngineMetrics,
}

// SAFETY: the only `!Send + !Sync` field is `bufferpool.base`,
// which is an `Option<*mut u8>` pointing into a pinned `Backing`
// owned by the embedder (see `crate::memory::Backing`'s
// matching unsafe impl). Production deployments share one engine
// per disk across multiple shard threads (see `LocalStorage` and
// the design doc); the engine reads the pointer only while
// computing slices for `slice_*_from_ref`, and the multi-shard
// path goes through `ShardLocalStore` which never touches this
// field. All other fields are `Arc`/`Mutex` wrappers that are
// already `Send + Sync` because their `B: BlockDevice` is
// (`CoreLocalDevice` and `MockDevice` are both `Send + Sync`).
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
    pub btree_lookup_cache_bytes: usize,
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
        // Scratch pool for btree/meta I/O: every page handed to
        // BlockDevice::{read,write} must lie inside a region
        // registered with the device. The bufferpool's larger
        // backing is registered later (via `register_pages`);
        // this small dedicated region covers the structural I/O
        // BTreeIndex::open issues before any user-data I/O.
        let scratch = ScratchPool::new(
            &*device,
            cfg.btree_page_bytes,
            cfg.btree_scratch_pages.max(MIN_BTREE_SCRATCH_PAGES),
        )?;
        let btree = Arc::new(
            BTreeIndex::open(
                device.clone(),
                allocator.clone(),
                scratch.clone(),
                cfg.btree_page_bytes,
                cfg.skip_recovery_scan_if_no_meta,
                cfg.force_format,
            )
            .await?,
        );
        let lru = Arc::new(SieveLru::new(capacity, refcount.clone()));
        let admission = Arc::new(AdmissionFilter::new(
            capacity,
            cfg.admission_sketch_multiplier.max(1) as u32,
        ));
        let singleflight = Arc::new(Singleflight::new(cfg.singleflight_shards.max(1)));
        let engine = Self {
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
        };
        engine.publish_usage_gauges();
        Ok(engine)
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
            btree_lookup_cache_bytes: self.btree.lookup_cache_bytes(),
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

    /// The `disk` label for this engine's Prometheus metrics.
    fn disk(&self) -> &str {
        &self.cfg.disk_id
    }

    fn n_pages(&self, byte_len: u32) -> u64 {
        // byte_len is the size of the cached page in bytes; each
        // contiguous run is `byte_len / btree_page_bytes` device
        // pages long. `write_page_from` rejects any write whose
        // length is not a positive multiple of `btree_page_bytes`,
        // so every `LeafEntry.byte_len` reaching this helper
        // divides exactly.
        debug_assert!(
            byte_len as usize % self.cfg.btree_page_bytes == 0 && byte_len > 0,
            "LeafEntry.byte_len ({byte_len}) must be a positive multiple of btree_page_bytes ({})",
            self.cfg.btree_page_bytes,
        );
        (byte_len as usize / self.cfg.btree_page_bytes) as u64
    }

    /// Reclaim every queued orphan run whose pin count has
    /// dropped to zero. Called opportunistically by the writer
    /// path so deferred frees don't accumulate indefinitely.
    fn drain_pending_free(&self) {
        let mut pending = self.pending_free.lock().unwrap();
        pending.retain(|(lba, n)| {
            if self.refcount.pin_count(lba.0).unwrap_or(0) == 0 {
                let _ = self.allocator.free_range(*lba, *n);
                let _ = self.refcount.reset(lba.0);
                false
            } else {
                true
            }
        });
    }

    /// Detach a `n`-page run starting at `old` from the engine's
    /// bookkeeping and either free it immediately or queue it for
    /// later reclamation when its readers are done.
    fn retire_range(&self, old: Lba, n: u64) {
        {
            let mut rev = self.reverse.lock().unwrap();
            rev.remove(&old.0);
        }
        self.lru.forget(old);
        if self.refcount.pin_count(old.0).unwrap_or(0) == 0 {
            let _ = self.allocator.free_range(old, n);
            let _ = self.refcount.reset(old.0);
        } else {
            self.pending_free.lock().unwrap().push((old, n));
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
            let candidates = self.lru.sweep(8);
            if candidates.is_empty() {
                return;
            }
            // Skip pinned LBAs - an in-flight reader still
            // references them. Re-admit so they remain tracked
            // and are reconsidered on a later sweep.
            let mut victims: Vec<Lba> = Vec::with_capacity(candidates.len());
            for candidate in candidates {
                if self.refcount.pin_count(candidate.lba.0).unwrap_or(0) == 0 {
                    victims.push(candidate.lba);
                } else {
                    self.lru.admit(candidate.lba, candidate.priority);
                }
            }
            if victims.is_empty() {
                return;
            }
            // Resolve victim start-LBAs to (key, byte_len) via
            // the reverse map before submitting: the mutator only
            // needs the keys to commit the delete, and we still
            // own the LBA runs here for the post-commit free
            // below.
            let mut keys: Vec<PageKey> = Vec::with_capacity(victims.len());
            let mut victim_runs: Vec<(Lba, u64)> = Vec::with_capacity(victims.len());
            {
                let rev = self.reverse.lock().unwrap();
                for lba in &victims {
                    if let Some((k, byte_len)) = rev.get(&lba.0).copied() {
                        keys.push(k);
                        victim_runs.push((*lba, self.n_pages(byte_len)));
                    }
                }
            }
            if keys.is_empty() {
                return;
            }
            // The btree mutation MUST commit before the allocator
            // slot is released: a concurrent writer that observes
            // the stale btree entry would otherwise call
            // retire_lba and double-free the slot the allocator
            // already handed back. The mutator is the single
            // committer, so this ordering is preserved by
            // construction once `done.wait()` returns.
            let done = MutatorReply::new();
            self.mutator_queue.push(MutatorReq::Delete {
                keys,
                done: done.clone(),
            });
            match done.wait().await {
                MutatorOutcome::DeleteCommitted => {}
                _ => {
                    // Apply failed or queue closed: leave the
                    // LBAs allocated and tracked. A later sweep
                    // will retry.
                    return;
                }
            }
            {
                let mut rev = self.reverse.lock().unwrap();
                for (lba, _) in &victim_runs {
                    rev.remove(&lba.0);
                }
            }
            for (lba, n) in &victim_runs {
                let _ = self.allocator.free_range(*lba, *n);
                let _ = self.refcount.reset(lba.0);
            }
            self.metric(|m| m.evictions += victim_runs.len() as u64);
            for _ in &victim_runs {
                crate::metrics::storage_eviction(self.disk());
            }
        })
    }
}

impl<B: BlockDevice> bufferpool::BlockStore for StorageEngine<B> {
    fn register_pages(&self, backing: &crate::memory::Backing) -> Result<(), bufferpool::Error> {
        let mut bp = self.bufferpool.lock().unwrap();
        bp.base = Some(backing.base);
        bp.page_size = backing.page_size;
        bp.page_count = backing.page_count;
        // Best-effort: also register against the underlying
        // device for io_uring's fixed-buffer table. Errors here
        // are not fatal - the device falls back to its slower
        // path.
        let _ = self
            .device
            .register_buffers(backing.base, backing.page_size * backing.page_count);
        Ok(())
    }

    async fn read_page<R: bufferpool::Req + ?Sized>(
        &self,
        req: &R,
        stripe_off: u64,
        dst: PageRef,
    ) -> Result<bool, bufferpool::Error> {
        // SAFETY: register_pages installed a valid base; this
        // slice lives until we return from this future.
        let dst_buf: *mut [u8] = unsafe { self.slice_mut_from_ref(dst) };
        // SAFETY: see comment above.
        unsafe { self.read_page_into(req.key(), stripe_off, dst_buf).await }
    }

    async fn write_page<R: bufferpool::Req + ?Sized>(
        &self,
        req: &R,
        stripe_off: u64,
        page: PageRef,
    ) -> Result<(), bufferpool::Error> {
        // SAFETY: see register_pages contract.
        let src_buf: *const [u8] = unsafe { self.slice_from_ref(page) };
        // SAFETY: see comment above.
        unsafe {
            self.write_page_from_with_priority(req.key(), stripe_off, src_buf, 0)
                .await
        }
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

        let entry = match self.lookup_entry(&pk).await {
            Some(e) => e,
            None => {
                self.metric(|m| m.misses += 1);
                crate::metrics::storage_lookup(crate::metrics::Lookup::Miss);
                return Ok(false);
            }
        };

        let _pin = match self.refcount.pin(entry.lba.0) {
            Ok(g) => g,
            Err(_) => {
                self.metric(|m| m.misses += 1);
                crate::metrics::storage_lookup(crate::metrics::Lookup::Miss);
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
            crate::metrics::storage_disk_op(
                self.disk(),
                crate::metrics::DiskOp::Read,
                crate::metrics::Outcome::Err,
            );
            crate::metrics::storage_lookup(crate::metrics::Lookup::Miss);
            return Ok(false);
        }
        crate::metrics::storage_disk_op(
            self.disk(),
            crate::metrics::DiskOp::Read,
            crate::metrics::Outcome::Ok,
        );

        if !self.cfg.bypass_checksum
            && Xxh3Checksum::checksum_of(dst_buf).0 != entry.data_checksum.0
        {
            self.metric(|m| {
                m.misses += 1;
                m.checksum_misses += 1;
            });
            crate::metrics::storage_lookup(crate::metrics::Lookup::Miss);
            return Ok(false);
        }

        self.lru.touch(entry.lba);
        self.admission.record_frequency(&pk);
        self.metric(|m| m.hits += 1);
        crate::metrics::storage_lookup(crate::metrics::Lookup::Hit);
        Ok(true)
    }

    async fn lookup_entry(&self, pk: &PageKey) -> Option<LeafEntry> {
        if self.cfg.bypass_index_read {
            return self.btree.lookup_committed_mirror(pk);
        }

        match self.btree.lookup(pk).await {
            Ok(Some(e)) => Some(e),
            Ok(None) | Err(_) => None,
        }
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
        unsafe {
            self.write_page_from_with_priority(key, stripe_off, src, 0)
                .await
        }
    }

    /// Write the contents of `src` to `(key, stripe_off)` and record
    /// the page's cache priority for eviction ordering.
    ///
    /// SAFETY: same contract as [`Self::write_page_from`].
    pub async unsafe fn write_page_from_with_priority(
        &self,
        key: StripeKey,
        stripe_off: u64,
        src: *const [u8],
        priority: i32,
    ) -> Result<(), bufferpool::Error> {
        let pk = Self::page_key(&key, stripe_off, self.cfg.page_size_bytes);

        if !self.cfg.bypass_admission && !self.admission.should_admit(&pk) {
            self.metric(|m| m.rejected_by_filter += 1);
            crate::metrics::storage_admission_rejected(self.disk());
            return Ok(());
        }

        let acquire = self.singleflight.acquire(pk);
        let leader = match acquire {
            Acquire::Leader(g) => g,
            Acquire::Follower(_w) => {
                return Ok(());
            }
        };

        // SAFETY: caller-provided contract on `src`.
        let src_buf: &[u8] = unsafe { &*src };

        // The engine only accepts writes whose length is a
        // positive multiple of `btree_page_bytes`. This keeps
        // alloc, on-disk layout, and retire bookkeeping in lockstep
        // (alloc and retire both compute the run length as
        // `byte_len / btree_page_bytes` with no rounding). Callers
        // that need partial-page semantics must pad the source
        // buffer up to a full btree page before calling.
        if src_buf.is_empty() || src_buf.len() % self.cfg.btree_page_bytes != 0 {
            debug_assert!(
                false,
                "write_page_from requires src.len() ({}) to be a positive multiple of btree_page_bytes ({})",
                src_buf.len(),
                self.cfg.btree_page_bytes,
            );
            leader.abandon();
            return Err(bufferpool::Error::Io(libc::EINVAL));
        }

        let n_pages = (src_buf.len() / self.cfg.btree_page_bytes) as u64;
        let lba = match self.allocator.alloc_contig(n_pages) {
            Ok(l) => l,
            Err(_) => {
                leader.abandon();
                return Ok(());
            }
        };

        if let Err(e) = self.device.write(lba, src_buf).await {
            // A device write that did not land on disk MUST surface
            // as an error to the caller. Returning Ok here would let
            // benchmarks and any other "I observed a successful
            // write" code path believe a page is durable when it is
            // not: an immediate read of `key` would then miss
            // because no btree entry was published. The engine
            // still records the IO error in metrics for the
            // operator-facing snapshot.
            self.metric(|m| m.write_io_errors += 1);
            crate::metrics::storage_disk_op(
                self.disk(),
                crate::metrics::DiskOp::Write,
                crate::metrics::Outcome::Err,
            );
            let _ = self.allocator.free_range(lba, n_pages);
            leader.abandon();
            return Err(bufferpool::Error::Io(io_errno(&e)));
        }
        crate::metrics::storage_disk_op(
            self.disk(),
            crate::metrics::DiskOp::Write,
            crate::metrics::Outcome::Ok,
        );

        let cs = Xxh3Checksum::checksum_of(src_buf);
        let entry = LeafEntry {
            lba,
            data_checksum: cs,
            byte_len: src_buf.len() as u32,
        };

        // Submit the insert to the mutator and wait for its
        // batched commit. The mutator returns the prior LeafEntry
        // the btree mapped this key to (if any) so we can retire
        // the entire prior run.
        let done = MutatorReply::new();
        self.mutator_queue.push(MutatorReq::Insert {
            key: pk,
            entry,
            done: done.clone(),
        });
        let prior = match done.wait().await {
            MutatorOutcome::InsertCommitted { prior } => prior,
            MutatorOutcome::Failed => {
                // Btree commit failed: the device write landed on
                // an LBA run that is no longer referenced by the
                // index, i.e. the page is not durably published.
                // Treat this as a hard write failure rather than a
                // silent success - see the matching reasoning at
                // the device-write branch above.
                self.metric(|m| m.write_io_errors += 1);
                let _ = self.allocator.free_range(lba, n_pages);
                leader.abandon();
                return Err(bufferpool::Error::Io(libc::EIO));
            }
            MutatorOutcome::DeleteCommitted => {
                unreachable!("mutator returned DeleteCommitted for an Insert request")
            }
        };

        if let Some(old) = prior {
            self.retire_range(old.lba, self.n_pages(old.byte_len));
        }

        {
            let mut rev = self.reverse.lock().unwrap();
            rev.insert(lba.0, (pk, src_buf.len() as u32));
        }
        self.lru.admit(lba, priority);
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
            // Coalesce up to `commit_batch_max`, spending a bounded
            // budget of cooperative yields (`commit_batch_ticks`)
            // so producers that are about to enqueue join this
            // commit. Wall-clock-free stand-in for
            // `commit_batch_deadline_us`; see
            // `MutatorQueue::drain_batch`. The drain stops early if
            // the queue closes, so a draining shutdown still
            // terminates.
            let batch = self
                .mutator_queue
                .drain_batch(max, self.cfg.commit_batch_ticks)
                .await;

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

        let mut priors: Vec<Option<LeafEntry>> = Vec::with_capacity(batch.len());
        let mut mutations: Vec<Mutation> = Vec::new();
        for req in &batch {
            match req {
                MutatorReq::Insert { key, entry, .. } => {
                    let prior = self.btree.lookup(key).await.ok().flatten();
                    priors.push(prior);
                    mutations.push(Mutation::Insert {
                        key: *key,
                        value: *entry,
                    });
                }
                MutatorReq::Delete { keys, .. } => {
                    priors.push(None);
                    for k in keys {
                        mutations.push(Mutation::Delete { key: *k });
                    }
                }
            }
        }

        let ok = if mutations.is_empty() {
            true
        } else {
            let started = std::time::Instant::now();
            let res = self.btree.apply_batch(mutations).await.is_ok();
            crate::metrics::storage_btree_commit_duration(started.elapsed().as_secs_f64());
            res
        };

        for (i, req) in batch.into_iter().enumerate() {
            let done = match &req {
                MutatorReq::Insert { done, .. } | MutatorReq::Delete { done, .. } => done.clone(),
            };
            let outcome = if !ok {
                MutatorOutcome::Failed
            } else {
                match req {
                    MutatorReq::Insert { .. } => {
                        MutatorOutcome::InsertCommitted { prior: priors[i] }
                    }
                    MutatorReq::Delete { .. } => MutatorOutcome::DeleteCommitted,
                }
            };
            done.set(outcome);
        }

        self.publish_usage_gauges();
    }

    /// Publishes the per-disk capacity/used byte gauges from the
    /// allocator's current page accounting. Cheap; called after each
    /// commit batch and once at open so the gauges reflect on-disk
    /// occupancy without a render-time pull (the engine is per-shard
    /// `!Send`, so a global pull gauge cannot reach it).
    fn publish_usage_gauges(&self) {
        // The allocator works in device LBAs sized `btree_page_bytes`,
        // not cache pages (`page_size_bytes`), so the byte conversion
        // must use the LBA size to avoid overstating occupancy.
        let lba = self.cfg.btree_page_bytes as i64;
        crate::metrics::storage_capacity_bytes(self.disk(), self.allocator.capacity() as i64 * lba);
        crate::metrics::storage_used_bytes(self.disk(), self.allocator.used_pages() as i64 * lba);
    }
}

/// Best-effort `errno` extraction for surfacing a device-layer
/// error through the [`bufferpool::Error::Io`] variant. The block
/// device crate exposes a small enum of failure shapes; the only
/// one that carries an `errno` today is `Io(i32)`. Other variants
/// (out-of-range, corruption, ...) collapse to `EIO` because the
/// upper layers only need to know "the write was rejected".
fn io_errno(e: &crate::storage::types::Error) -> i32 {
    match e {
        crate::storage::types::Error::Io(n) => *n,
        _ => libc::EIO,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::bufferpool::BlockStore;
    use crate::memory::Backing;
    use crate::storage::blockdev::{MockDevice, MockDeviceConfig, MockFaultMode};
    use crate::storage::types::Lba;

    fn test_backing(base: *mut u8, page_size: usize, page_count: usize) -> Backing {
        Backing {
            base,
            page_size,
            page_count,
            keepalive: std::sync::Arc::new(()),
        }
    }
    use std::future::Future;
    use std::pin::{Pin, pin};
    use std::task::{Context, Poll};

    use crate::runtime::noop_waker;

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
        eng.register_pages(&test_backing(buf.as_ptr() as *mut u8, 4096, 64))
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
            eng_body.write_page(&stripe(1), 0, src_page).await.unwrap();
            assert_eq!(eng_body.snapshot().rejected_by_filter, 1);
            // Second write: admitted.
            eng_body.write_page(&stripe(1), 0, src_page).await.unwrap();
            assert_eq!(eng_body.snapshot().admitted, 1);

            let dst_page = PageRef {
                page_idx: 1,
                offset: 0,
                len: 4096,
            };
            let hit = eng_body.read_page(&stripe(1), 0, dst_page).await.unwrap();
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
        let hit = block_on(eng.read_page(&stripe(7), 0, dst)).unwrap();
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
        block_on(eng.write_page(&stripe(9), 0, src)).unwrap();
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
        eng.register_pages(&test_backing(buf.as_ptr() as *mut u8, 4096, 64))
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
            eng_body.write_page(&stripe(11), 0, src).await.unwrap();
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
            eng_body.write_page(&stripe(3), 0, src).await.unwrap();
            eng_body.write_page(&stripe(3), 0, src).await.unwrap();
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
            let hit = eng_body.read_page(&stripe(3), 0, dst).await.unwrap();
            assert!(!hit);
            assert!(eng_body.snapshot().checksum_misses >= 1);
            eng_body.close_mutator();
            let _ = buf;
        };
        block_on_pair(body, mutator.as_mut());
    }

    /// Stand-in storage workload when hugepages are not available on
    /// the host: drive ~64 writes followed by ~64 reads through a
    /// `bypass_admission` engine over `MockDevice` and assert the
    /// success criteria (ops produced, no errors, every read is a
    /// cache hit).
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
        eng.register_pages(&test_backing(buf.as_ptr() as *mut u8, PAGE, PAGES))
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
                eng_body.write_page(&stripe(i), 0, src).await.unwrap();
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
                let hit = eng_body.read_page(&stripe(i), 0, dst).await.unwrap();
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

    /// A device-level write failure MUST propagate to the caller
    /// as an `Err` so benchmarks and any other "I observed a
    /// successful write" code path cannot treat the page as
    /// durable. Before this contract was enforced, the storage-layer
    /// workload happily reported gigabytes per second while every
    /// `write_page_from` call silently swallowed the underlying
    /// device error and returned `Ok(())`, leaving the btree with
    /// no entry for the key.
    #[test]
    fn write_page_returns_err_when_device_write_fails() {
        const PAGE: usize = 4096;
        let device = Arc::new(MockDevice::new(MockDeviceConfig {
            page_size: PAGE,
            capacity_pages: 64,
            ..Default::default()
        }));
        let cfg = EngineConfig {
            page_size_bytes: PAGE,
            btree_page_bytes: PAGE,
            // Bypass the doorkeeper so the first write actually
            // reaches the device, instead of being swallowed by
            // the admission filter and producing a benign Ok.
            bypass_admission: true,
            ..Default::default()
        };
        let eng = Arc::new(block_on(StorageEngine::open(device.clone(), cfg)).unwrap());
        let buf: Box<[u8]> = vec![0u8; PAGE * 4].into_boxed_slice();
        eng.register_pages(&test_backing(buf.as_ptr() as *mut u8, PAGE, 4))
            .unwrap();

        // Surface every write as EIO so the engine sees a device-
        // level failure on the user-data write path. Engaging the
        // fault *after* open lets BTreeIndex::open complete its
        // structural meta writes; we only want to fault the
        // user-data write here.
        device.set_fault_mode(MockFaultMode::WriteIo);

        let eng_body = eng.clone();
        let mutator = eng.clone().run_mutator();
        let mut mutator = pin!(mutator);
        let body = async move {
            let src = PageRef {
                page_idx: 0,
                offset: 0,
                len: PAGE as u32,
            };
            let err = eng_body
                .write_page(&stripe(17), 0, src)
                .await
                .expect_err("device WriteIo fault must propagate as Err");
            match err {
                bufferpool::Error::Io(n) => assert_eq!(n, libc::EIO),
                other => panic!("expected bufferpool::Error::Io(EIO), got {other:?}"),
            }
            let s = eng_body.snapshot();
            assert_eq!(s.write_io_errors, 1);
            assert_eq!(
                s.admitted, 0,
                "failed write must not be counted as admitted"
            );

            // A subsequent read for the same key must miss: the
            // btree was never updated, so the engine should not
            // pretend the page is on disk.
            let dst = PageRef {
                page_idx: 1,
                offset: 0,
                len: PAGE as u32,
            };
            let hit = eng_body.read_page(&stripe(17), 0, dst).await.unwrap();
            assert!(!hit, "key with failed write must not appear as a cache hit");
            eng_body.close_mutator();
        };
        block_on_pair(body, mutator.as_mut());
    }
}
