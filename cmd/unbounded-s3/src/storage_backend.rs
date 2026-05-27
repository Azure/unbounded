// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Production `ObjectSource` that adapts S3 byte-range reads onto
//! [`BlockStore::read_page`].
//!
//! The crate has no topology, transport, or P2P logic. Those belong
//! to the [`BlockStore`] implementation that `main.rs` constructs.
//!
//! ## Ring discipline
//!
//! A fixed ring of [`NUM_SLOTS`] x [`PAGE_SIZE`] is registered with
//! the `BlockStore` once at construction. For each in-flight HTTP
//! request a `stream::unfold` state machine acquires a slot, calls
//! `read_page` into it, copies the requested sub-range out into a
//! fresh [`Bytes`], and releases the slot back to the ring before
//! yielding. The ring is therefore only ever a destination for
//! `read_page`; it does not back live response chunks. This satisfies
//! the [`ObjectSource`] contract that "each item is an independent
//! heap allocation" and means consumers can hold any number of
//! yielded `Bytes` without throttling the ring.
//!
//! The copy is one memcpy per page; under hyper's normal backpressure
//! it is on the same hot path as the network write, and the
//! simplicity is worth it. If we later need true zero-copy we can
//! restore an owner-borrowed `Bytes` path with a per-stream lease
//! budget.
//!
//! ## `SendBlockStore`
//!
//! Upstream's [`BlockStore`] declares `async fn read_page` with the
//! `#![allow(async_fn_in_trait)]` workaround, which means the
//! returned future has no `Send` guarantee. Axum's response body
//! requires a `Send` stream, so we abstract over a local
//! `SendBlockStore` trait that boxes the future with an explicit
//! `Send` bound. The blanket-impl path is impossible in stable Rust
//! (the bound applies to the opaque future), so each concrete
//! implementor we want to plug in needs a small adapter impl in this
//! file. Remove `SendBlockStore` when upstream adds a `Send` bound to
//! `BlockStore::read_page`.

use std::collections::VecDeque;
use std::sync::{Arc, Mutex};

use bytes::Bytes;
use futures::future::BoxFuture;
use futures::stream::{self, BoxStream, StreamExt};
use tokio::sync::Semaphore;
use unbounded_storage::backing::{allocate, BackingKind, BackingRequest};
use unbounded_storage::bufferpool::{
    Backing, BlockStore, Error as BpError, NullBlockStore, PageRef, StripeKey,
};

use crate::catalog::ObjectMeta;
use crate::object::{Error, ObjectSource};

/// Size of each ring slot, in bytes. Matches the upstream default
/// page size. The buffer is not exposed via the public API; this
/// constant exists so the math is in one place.
const PAGE_SIZE: usize = 2 * 1024 * 1024;

/// Number of ring slots. Caps the per-process memory the storage
/// backend can pin (`PAGE_SIZE * NUM_SLOTS`) and the upper bound on
/// outstanding `read_page` calls across all in-flight HTTP requests.
const NUM_SLOTS: usize = 4;

/// Local `BlockStore` wrapper whose async methods return `Send`
/// futures. See the module docs for why this is needed.
pub trait SendBlockStore: Send + Sync + 'static {
    fn register_pages(&self, backing: &Backing) -> Result<(), BpError>;

    fn read_page<'a>(
        &'a self,
        key: StripeKey,
        stripe_off: u64,
        dst: PageRef,
    ) -> BoxFuture<'a, Result<bool, BpError>>;
}

impl SendBlockStore for NullBlockStore {
    fn register_pages(&self, backing: &Backing) -> Result<(), BpError> {
        <Self as BlockStore>::register_pages(self, backing)
    }

    fn read_page<'a>(
        &'a self,
        key: StripeKey,
        stripe_off: u64,
        dst: PageRef,
    ) -> BoxFuture<'a, Result<bool, BpError>> {
        Box::pin(<Self as BlockStore>::read_page(self, key, stripe_off, dst))
    }
}

/// Production `ObjectSource` over an arbitrary [`SendBlockStore`].
///
/// Today `main.rs` plugs in
/// [`unbounded_storage::bufferpool::NullBlockStore`] via the adapter
/// impl above, which always reports a miss. When upstream publishes a
/// P2P-aware `BlockStore` it should drop in by adding one more
/// `SendBlockStore` impl in this file.
pub struct BlockStoreObjectSource<S: SendBlockStore> {
    inner: Arc<Inner<S>>,
}

impl<S: SendBlockStore> BlockStoreObjectSource<S> {
    /// Allocate the ring backing, register it with `store`, and wrap
    /// the pair behind an `Arc` so it can be cloned cheaply into
    /// per-request stream states.
    pub fn new(store: S) -> Result<Self, Error> {
        let ring = Ring::new()?;
        store.register_pages(&ring.backing)?;
        let ring = Arc::new(ring);
        Ok(Self {
            inner: Arc::new(Inner { store, ring }),
        })
    }
}

impl<S: SendBlockStore> ObjectSource for BlockStoreObjectSource<S> {
    fn read_range(
        &self,
        meta: &ObjectMeta,
        offset: u64,
        len: u64,
    ) -> BoxStream<'static, Result<Bytes, Error>> {
        if len == 0 {
            return stream::empty().boxed();
        }

        let inner = self.inner.clone();
        let stripe = meta.stripe;
        let pages: VecDeque<PageChunk> =
            pages_covering(offset, len, PAGE_SIZE).collect();

        let state = ReadState {
            inner,
            stripe,
            pages,
        };

        stream::unfold(state, |mut state| async move {
            let chunk = state.pages.pop_front()?;
            let lease = state.inner.ring.clone().acquire().await;
            let dst = PageRef {
                page_idx: lease.page_idx,
                offset: 0,
                len: PAGE_SIZE as u32,
            };
            let stripe = state.stripe;
            let result = state.inner.store.read_page(stripe, chunk.stripe_off, dst).await;
            match result {
                Ok(true) => {
                    // Copy the requested sub-range out of the slot
                    // into a fresh `Bytes`, then drop the lease so
                    // the slot is available for the next `read_page`
                    // even if the consumer holds on to this `Bytes`.
                    //
                    // The unsafe pointer arithmetic and slice/copy
                    // are encapsulated inside `SlotLease::read_into_bytes`
                    // so the raw slice's lifetime is bounded by the
                    // lease borrow. That makes the drop-ordering
                    // invariant ("drop(lease) must remain below the
                    // last use of the slice") compiler-enforced
                    // rather than prose-only.
                    let view = lease.read_into_bytes(chunk.page_offset, chunk.len);
                    drop(lease);
                    Some((Ok(view), state))
                }
                Ok(false) => {
                    // No peer fetch path until upstream ships a
                    // P2P-aware `BlockStore`. A miss is treated as a
                    // hard error: the stripe is not retrievable.
                    // Clear `pages` so the next `unfold` poll ends
                    // the stream.
                    tracing::warn!(
                        "BlockStore miss for stripe {:02x}{:02x}.. at offset {}",
                        stripe.0[0],
                        stripe.0[1],
                        chunk.stripe_off,
                    );
                    state.pages.clear();
                    Some((
                        Err(Error::Internal(format!(
                            "stripe {:02x}{:02x}.. unavailable: \
                             BlockStore miss with no P2P fallback",
                            stripe.0[0], stripe.0[1],
                        ))),
                        state,
                    ))
                }
                Err(e) => {
                    // Symmetric with the `Ok(false)` warn above so an
                    // operator triaging a real I/O failure isn't left
                    // with a silently terminating stream.
                    tracing::warn!(
                        "BlockStore::read_page failed for stripe {:02x}{:02x}.. at offset {}: {}",
                        stripe.0[0],
                        stripe.0[1],
                        chunk.stripe_off,
                        e,
                    );
                    state.pages.clear();
                    Some((Err(Error::from(e)), state))
                }
            }
        })
        .boxed()
    }
}

struct Inner<S> {
    store: S,
    ring: Arc<Ring>,
}

/// State threaded through the `stream::unfold` driving one
/// `read_range` call. `pages` shrinks as chunks are consumed; emptying
/// it (either naturally or on error) terminates the stream.
struct ReadState<S: SendBlockStore> {
    inner: Arc<Inner<S>>,
    stripe: StripeKey,
    pages: VecDeque<PageChunk>,
}

/// Fixed-size ring of registered page slots.
///
/// `backing` owns the heap allocation that backs the slots via its
/// `_own` keep-alive (produced by `unbounded_storage::backing::allocate`);
/// `backing.base` is the stable starting pointer. `free` is the
/// free-slot index queue; `avail` counts free slots so `acquire` can
/// `await` rather than spin when the ring is exhausted.
struct Ring {
    backing: Backing,
    free: Mutex<VecDeque<u32>>,
    avail: Semaphore,
}

impl Ring {
    fn new() -> Result<Self, Error> {
        let backing = allocate(BackingRequest {
            kind: BackingKind::Heap,
            bytes: PAGE_SIZE * NUM_SLOTS,
            numa: None,
        })
        .map_err(|e| Error::Internal(format!("ring backing alloc: {e}")))?;
        // The upstream allocator hardcodes `HUGEPAGE_2MB` as the page
        // size and rounds `bytes` up. We sized our request to be an
        // exact multiple, so the returned geometry must match what
        // `BlockStoreObjectSource` already assumes elsewhere.
        debug_assert_eq!(backing.page_size, PAGE_SIZE);
        debug_assert_eq!(backing.page_count, NUM_SLOTS);
        let free = (0..NUM_SLOTS as u32).collect::<VecDeque<_>>();
        Ok(Self {
            backing,
            free: Mutex::new(free),
            avail: Semaphore::new(NUM_SLOTS),
        })
    }

    /// Reserve one slot, awaiting if the ring is empty.
    ///
    /// The returned [`SlotLease`] owns the release back to the ring;
    /// dropping it frees the slot. `BlockStoreObjectSource::read_range`
    /// drops the lease as soon as the chunk has been copied out, so
    /// the slot is back in the ring before the resulting `Bytes` is
    /// yielded to the consumer.
    async fn acquire(self: Arc<Self>) -> SlotLease {
        let permit = self
            .avail
            .acquire()
            .await
            .expect("semaphore is never closed");
        permit.forget();
        let page_idx = self
            .free
            .lock()
            .expect("ring free list poisoned")
            .pop_front()
            .expect("acquire holds a permit so free list is non-empty");
        SlotLease {
            ring: self,
            page_idx,
        }
    }

    fn release(&self, page_idx: u32) {
        self.free
            .lock()
            .expect("ring free list poisoned")
            .push_back(page_idx);
        self.avail.add_permits(1);
    }

    /// Raw pointer to slot `page_idx`. The caller must hold the
    /// matching [`SlotLease`] (the only path that controls slot
    /// ownership) for as long as the pointer is dereferenced, and
    /// must only read or write within `[0, PAGE_SIZE)` from the
    /// returned address.
    fn slot_ptr(&self, page_idx: u32) -> *mut u8 {
        debug_assert!((page_idx as usize) < NUM_SLOTS);
        // SAFETY: `page_idx < NUM_SLOTS` (debug-asserted) and the
        // `Backing` owns a `PAGE_SIZE * NUM_SLOTS` allocation, so the
        // resulting pointer is in-bounds.
        unsafe { self.backing.base.add(page_idx as usize * PAGE_SIZE) }
    }
}

/// Owned reservation of one ring slot. Drop returns the slot to the
/// ring; this is the only path that does so.
struct SlotLease {
    ring: Arc<Ring>,
    page_idx: u32,
}

impl Drop for SlotLease {
    fn drop(&mut self) {
        self.ring.release(self.page_idx);
    }
}

impl SlotLease {
    /// Copy `len` bytes starting at `page_offset` from this slot's
    /// backing memory into a fresh, owned [`Bytes`].
    ///
    /// The returned `Bytes` does not share storage with the slot, so
    /// the lease is safe to drop as soon as this call returns; the
    /// caller can release the slot before the chunk is forwarded
    /// downstream.
    ///
    /// Encapsulating the unsafe pointer arithmetic and slice
    /// construction inside this method (rather than inlining at the
    /// call site) makes the drop-ordering invariant compiler-
    /// enforced: the raw `&[u8]` cannot outlive the `&self` borrow
    /// that proves the lease is held, so a future refactor cannot
    /// accidentally let the slice survive past `drop(lease)`.
    ///
    /// # Panics (debug)
    ///
    /// `page_offset + len > PAGE_SIZE`.
    fn read_into_bytes(&self, page_offset: usize, len: usize) -> Bytes {
        debug_assert!(
            page_offset
                .checked_add(len)
                .map_or(false, |end| end <= PAGE_SIZE),
            "read range {page_offset}..{page_offset}+{len} exceeds PAGE_SIZE={PAGE_SIZE}",
        );
        // SAFETY: `&self` borrow keeps the slot exclusively reserved
        // by this lease for the duration of the call, so no other
        // task can read or write the slot. The raw slice is
        // constructed and consumed entirely within this function and
        // its lifetime is bounded by `&self`, so it cannot outlive
        // the lease. `page_offset + len <= PAGE_SIZE` (debug-asserted
        // above) keeps the pointer arithmetic in-bounds for the
        // ring's `PAGE_SIZE * NUM_SLOTS` allocation.
        let slice = unsafe {
            let ptr = self.ring.slot_ptr(self.page_idx).add(page_offset);
            std::slice::from_raw_parts(ptr, len)
        };
        Bytes::copy_from_slice(slice)
    }
}

/// Description of one page-aligned chunk inside a requested byte
/// range, in terms the loop in `read_range` consumes directly.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
struct PageChunk {
    /// Offset within the stripe to pass to `read_page`. Always a
    /// multiple of `page_size`.
    stripe_off: u64,
    /// Byte offset inside the just-read page where the caller's data
    /// starts. Bounded by `page_size`.
    page_offset: usize,
    /// Number of bytes from `page_offset` the caller wants. Bounded
    /// by `page_size - page_offset`.
    len: usize,
}

/// Iterator over the page-aligned chunks covering `[offset, offset+len)`.
///
/// Each yielded chunk corresponds to exactly one `read_page` call.
/// The first and last chunks may be partial (when the request is not
/// page-aligned or shorter than a page); middle chunks fill an entire
/// page. Returns an empty iterator when `len == 0`.
fn pages_covering(offset: u64, len: u64, page_size: usize) -> PagesCovering {
    PagesCovering {
        cursor: offset,
        end: offset.saturating_add(len),
        page_size: page_size as u64,
    }
}

struct PagesCovering {
    cursor: u64,
    end: u64,
    page_size: u64,
}

impl Iterator for PagesCovering {
    type Item = PageChunk;

    fn next(&mut self) -> Option<PageChunk> {
        if self.cursor >= self.end {
            return None;
        }
        let stripe_off = (self.cursor / self.page_size) * self.page_size;
        let page_offset = (self.cursor - stripe_off) as usize;
        let page_end = stripe_off + self.page_size;
        let chunk_end = page_end.min(self.end);
        let len = (chunk_end - self.cursor) as usize;
        self.cursor = chunk_end;
        Some(PageChunk {
            stripe_off,
            page_offset,
            len,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicU32, AtomicU64, Ordering};

    fn meta(stripe: StripeKey) -> ObjectMeta {
        ObjectMeta {
            stripe,
            size: u64::MAX,
            etag: "\"deadbeefdeadbeef\"".into(),
            content_type: "application/octet-stream".into(),
            last_modified: "Thu, 01 Jan 1970 00:00:00 GMT".into(),
        }
    }

    // ---- pages_covering -----------------------------------------------------

    #[test]
    fn pages_covering_zero_length_is_empty() {
        let v: Vec<_> = pages_covering(0, 0, 8).collect();
        assert!(v.is_empty());
        let v: Vec<_> = pages_covering(123, 0, 8).collect();
        assert!(v.is_empty());
    }

    #[test]
    fn pages_covering_single_aligned_page() {
        let v: Vec<_> = pages_covering(0, 8, 8).collect();
        assert_eq!(
            v,
            vec![PageChunk { stripe_off: 0, page_offset: 0, len: 8 }]
        );
    }

    #[test]
    fn pages_covering_sub_page_within_one_page() {
        let v: Vec<_> = pages_covering(2, 3, 8).collect();
        assert_eq!(
            v,
            vec![PageChunk { stripe_off: 0, page_offset: 2, len: 3 }]
        );
    }

    #[test]
    fn pages_covering_unaligned_spans_two_pages() {
        // [6, 12) covers tail of page 0 ([6,8)) and head of page 1
        // ([8,12)).
        let v: Vec<_> = pages_covering(6, 6, 8).collect();
        assert_eq!(
            v,
            vec![
                PageChunk { stripe_off: 0, page_offset: 6, len: 2 },
                PageChunk { stripe_off: 8, page_offset: 0, len: 4 },
            ]
        );
    }

    #[test]
    fn pages_covering_multi_page_with_partial_first_and_last() {
        // [3, 19): tail of page 0, full page 1, head of page 2.
        let v: Vec<_> = pages_covering(3, 16, 8).collect();
        assert_eq!(
            v,
            vec![
                PageChunk { stripe_off: 0, page_offset: 3, len: 5 },
                PageChunk { stripe_off: 8, page_offset: 0, len: 8 },
                PageChunk { stripe_off: 16, page_offset: 0, len: 3 },
            ]
        );
    }

    #[test]
    fn pages_covering_exactly_two_aligned_pages() {
        let v: Vec<_> = pages_covering(8, 16, 8).collect();
        assert_eq!(
            v,
            vec![
                PageChunk { stripe_off: 8, page_offset: 0, len: 8 },
                PageChunk { stripe_off: 16, page_offset: 0, len: 8 },
            ]
        );
    }

    // ---- read_range against NullBlockStore (always miss) --------------------

    #[tokio::test]
    async fn null_blockstore_yields_internal_error_and_ends() {
        let src = BlockStoreObjectSource::new(NullBlockStore::new()).unwrap();
        let stripe = StripeKey([0xab; 32]);
        let mut s = src.read_range(&meta(stripe), 0, 1024);

        match s.next().await {
            Some(Err(Error::Internal(msg))) => {
                assert!(msg.contains("unavailable"), "got: {msg}");
                assert!(msg.contains("abab"), "got: {msg}");
            }
            other => panic!("expected Internal error, got {other:?}"),
        }
        assert!(s.next().await.is_none(), "stream should end after error");
    }

    #[tokio::test]
    async fn zero_length_read_yields_empty_stream() {
        let src = BlockStoreObjectSource::new(NullBlockStore::new()).unwrap();
        let stripe = StripeKey([0; 32]);
        let mut s = src.read_range(&meta(stripe), 0, 0);
        assert!(s.next().await.is_none());
    }

    // ---- read_range against an always-hit mock ------------------------------

    /// Test-only `BlockStore` that fills the destination page with a
    /// deterministic per-byte pattern derived from `stripe_off`.
    struct PatternBlockStore {
        base: AtomicU64,       // base ptr published by register_pages
        page_size: AtomicU64,  // page size published by register_pages
        page_count: AtomicU32, // page count published by register_pages
    }

    impl PatternBlockStore {
        fn new() -> Self {
            Self {
                base: AtomicU64::new(0),
                page_size: AtomicU64::new(0),
                page_count: AtomicU32::new(0),
            }
        }

        /// Returns the byte the mock writes at absolute stripe
        /// position `pos`. Pure function: keeps the oracle short.
        fn pattern_byte(pos: u64) -> u8 {
            (pos & 0xff) as u8
        }
    }

    impl BlockStore for PatternBlockStore {
        fn register_pages(&self, backing: &Backing) -> Result<(), BpError> {
            self.base.store(backing.base as u64, Ordering::SeqCst);
            self.page_size
                .store(backing.page_size as u64, Ordering::SeqCst);
            self.page_count
                .store(backing.page_count as u32, Ordering::SeqCst);
            Ok(())
        }

        async fn read_page(
            &self,
            _key: StripeKey,
            stripe_off: u64,
            dst: PageRef,
        ) -> Result<bool, BpError> {
            let base = self.base.load(Ordering::SeqCst) as *mut u8;
            let ps = self.page_size.load(Ordering::SeqCst) as usize;
            let pc = self.page_count.load(Ordering::SeqCst) as usize;
            assert!(!base.is_null(), "register_pages must run before read_page");
            assert!((dst.page_idx as usize) < pc);
            assert!(dst.offset as usize + dst.len as usize <= ps);
            // SAFETY: the test holds `&BlockStoreObjectSource` so the
            // ring backing is alive; `dst.page_idx` and `dst.len` are
            // bounded by the registered geometry above.
            let slice = unsafe {
                std::slice::from_raw_parts_mut(
                    base.add(dst.page_idx as usize * ps + dst.offset as usize),
                    dst.len as usize,
                )
            };
            for (i, b) in slice.iter_mut().enumerate() {
                *b = Self::pattern_byte(stripe_off + i as u64);
            }
            Ok(true)
        }

        async fn write_page(
            &self,
            _key: StripeKey,
            _stripe_off: u64,
            _page: PageRef,
        ) -> Result<(), BpError> {
            Ok(())
        }
    }

    // Adapter mirroring the one for `NullBlockStore`, so the test
    // type can plug into `BlockStoreObjectSource`.
    impl SendBlockStore for PatternBlockStore {
        fn register_pages(&self, backing: &Backing) -> Result<(), BpError> {
            <Self as BlockStore>::register_pages(self, backing)
        }

        fn read_page<'a>(
            &'a self,
            key: StripeKey,
            stripe_off: u64,
            dst: PageRef,
        ) -> BoxFuture<'a, Result<bool, BpError>> {
            Box::pin(<Self as BlockStore>::read_page(self, key, stripe_off, dst))
        }
    }

    /// Drain `s` into a single `Vec<u8>` so byte-for-byte assertions
    /// are independent of how many chunks the stream is split into.
    async fn drain(mut s: BoxStream<'static, Result<Bytes, Error>>) -> Vec<u8> {
        let mut out = Vec::new();
        while let Some(item) = s.next().await {
            out.extend_from_slice(&item.expect("stream item should be Ok"));
        }
        out
    }

    fn expected_range(offset: u64, len: u64) -> Vec<u8> {
        (0..len)
            .map(|i| PatternBlockStore::pattern_byte(offset + i))
            .collect()
    }

    #[tokio::test]
    async fn aligned_full_page_returns_pattern() {
        let src = BlockStoreObjectSource::new(PatternBlockStore::new()).unwrap();
        let stripe = StripeKey([1; 32]);
        let s = src.read_range(&meta(stripe), 0, PAGE_SIZE as u64);
        let got = drain(s).await;
        assert_eq!(got.len(), PAGE_SIZE);
        // Spot-check (full equality at 2 MiB makes the failure
        // output unreadable on regression).
        assert_eq!(got[..16], expected_range(0, 16)[..]);
        assert_eq!(
            got[PAGE_SIZE - 16..],
            expected_range(PAGE_SIZE as u64 - 16, 16)[..]
        );
    }

    #[tokio::test]
    async fn sub_page_unaligned_returns_pattern() {
        let src = BlockStoreObjectSource::new(PatternBlockStore::new()).unwrap();
        let stripe = StripeKey([1; 32]);
        let offset = 100u64;
        let len = 500u64;
        let s = src.read_range(&meta(stripe), offset, len);
        let got = drain(s).await;
        assert_eq!(got, expected_range(offset, len));
    }

    #[tokio::test]
    async fn multi_page_with_partial_ends_returns_pattern() {
        let src = BlockStoreObjectSource::new(PatternBlockStore::new()).unwrap();
        let stripe = StripeKey([2; 32]);
        // Start mid-page-0, end mid-page-2: exercises 3 chunks total
        // including a full middle page.
        let offset = (PAGE_SIZE as u64) / 3;
        let len = (PAGE_SIZE as u64) * 2;
        let s = src.read_range(&meta(stripe), offset, len);
        let got = drain(s).await;
        assert_eq!(got.len(), len as usize);
        // Spot-check first, middle, last bytes.
        assert_eq!(got[0], PatternBlockStore::pattern_byte(offset));
        assert_eq!(
            got[got.len() / 2],
            PatternBlockStore::pattern_byte(offset + (len / 2)),
        );
        assert_eq!(
            *got.last().unwrap(),
            PatternBlockStore::pattern_byte(offset + len - 1),
        );
    }

    // ---- backpressure / slot release ----------------------------------------

    #[tokio::test]
    async fn slots_return_to_ring_after_stream_drained() {
        let src = BlockStoreObjectSource::new(PatternBlockStore::new()).unwrap();
        let stripe = StripeKey([7; 32]);

        // Drain a multi-page request so every slot is touched at
        // least once.
        let s = src.read_range(&meta(stripe), 0, (PAGE_SIZE as u64) * 3);
        let _ = drain(s).await;

        // After all the Bytes (and their leases) are dropped, every
        // permit should be back in the semaphore.
        // Allow a few yield turns for any pending drops to settle.
        for _ in 0..16 {
            if src.inner.ring.avail.available_permits() == NUM_SLOTS {
                break;
            }
            tokio::task::yield_now().await;
        }
        assert_eq!(src.inner.ring.avail.available_permits(), NUM_SLOTS);
        assert_eq!(src.inner.ring.free.lock().unwrap().len(), NUM_SLOTS);
    }

    #[tokio::test]
    async fn many_concurrent_requests_complete_with_only_four_slots() {
        // Hammer the ring with more concurrent requests than slots
        // and assert every one of them completes. This is a smoke
        // test against deadlock under contention, not a scheduling
        // assertion.
        let src = Arc::new(BlockStoreObjectSource::new(PatternBlockStore::new()).unwrap());
        let mut joins = Vec::new();
        for i in 0..NUM_SLOTS * 4 {
            let src = src.clone();
            let stripe = StripeKey([i as u8; 32]);
            joins.push(tokio::spawn(async move {
                let s = src.read_range(&meta(stripe), 0, (PAGE_SIZE as u64) * 2);
                let got = drain(s).await;
                assert_eq!(got.len(), (PAGE_SIZE * 2) as usize);
            }));
        }
        for j in joins {
            j.await.expect("task panicked");
        }
    }

    #[tokio::test]
    async fn collect_more_than_num_slots_does_not_deadlock() {
        // Regression test for the ring-lease deadlock: a consumer
        // that retains every yielded `Bytes` before polling the next
        // one (`stream.collect`) must still make progress, because
        // copy-at-yield releases the slot before the chunk is
        // surfaced. Under the previous `Bytes::from_owner` path this
        // hangs forever once `NUM_SLOTS` chunks are outstanding.
        let src = BlockStoreObjectSource::new(PatternBlockStore::new()).unwrap();
        let stripe = StripeKey([9; 32]);
        let pages = (NUM_SLOTS as u64) + 2;
        let s = src.read_range(&meta(stripe), 0, PAGE_SIZE as u64 * pages);
        let collected: Vec<Result<Bytes, Error>> = s.collect().await;
        let total: usize = collected
            .iter()
            .map(|r| r.as_ref().expect("chunk should be Ok").len())
            .sum();
        assert_eq!(total, PAGE_SIZE * pages as usize);

        // The ring should also be fully drained back to its initial
        // state once the collected `Bytes` are still alive but no
        // leases remain - copy-at-yield releases each lease before
        // the chunk is yielded.
        assert_eq!(src.inner.ring.avail.available_permits(), NUM_SLOTS);
        assert_eq!(src.inner.ring.free.lock().unwrap().len(), NUM_SLOTS);
        // Keep `collected` alive to the end of the test so the
        // optimizer doesn't drop it before the assertion above.
        drop(collected);
    }
}
