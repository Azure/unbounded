// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Multi-stripe pipelined reader.
//!
//! [`PipelinedRead`] generalizes [`crate::bufferpool::WindowedRead`]
//! from a single stripe to an ordered sequence of per-stripe slices.
//! Where `WindowedRead` keeps up to `window` fetch futures in flight
//! within one stripe, `PipelinedRead` keeps `window` fetch futures in
//! flight across stripe boundaries, so origin (and peer) downloads
//! pipeline continuously instead of draining one stripe before the
//! next begins. Pages are still delivered to the consumer strictly in
//! order, one [`PageGuard`] at a time.
//!
//! Motivation: the frontends serve a byte range as a contiguous run
//! of per-stripe slices. Driving each slice's `WindowedRead` to
//! exhaustion before starting the next gates prefetch on the
//! (potentially slow) in-order client send, so prefetch never crosses
//! a stripe boundary and the effective origin-fetch depth collapses
//! to one stripe regardless of the configured budget. `PipelinedRead`
//! treats the whole range as one global page sequence so the prefetch
//! window spans stripes.
//!
//! Streams are admitted lazily, one per slice, only when the window
//! first reaches into that slice, and each is released (its stream
//! slot returned) as soon as the consumer cursor passes its last
//! page. This bounds the number of concurrently admitted streams to
//! roughly `window` rather than the slice count, so a multi-gigabyte
//! object does not exhaust `max_concurrent_streams`.
//!
//! Budgets mirror `WindowedRead`: the global `max_inflight_pages`
//! prefetch budget (reserved via [`StreamSrc::try_reserve_prefetch`])
//! and the per-read `window` depth. The single in-order head page
//! (the page at the consumer cursor) is fetched non-speculatively and
//! never counts against either budget; every page ahead of it is
//! speculative. Because each admitted slice counts as one stream in
//! the pool's `stream_count`, the head reservation in
//! `try_reserve_prefetch` is slightly conservative (it reserves a
//! head page for every admitted slice even though only one global
//! head is ever blocking), which costs a little speculation but is
//! always safe and never deadlocks.

use std::collections::HashMap;
use std::future::Future;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll};

use crate::bufferpool::stream::{PageGuard, StaticBoxFuture, StreamSrc};
use crate::bufferpool::types::{Error, PageRef};

/// One stripe slice in the ordered plan handed to
/// [`crate::bufferpool::Pool::read_pipelined`]. `req` identifies the
/// stripe (content-addressed); `intra_offset`/`intra_len` are the
/// byte sub-range within that stripe to deliver, exactly as produced
/// by the frontend's `stripe_set`.
pub struct StripePlan<R> {
    pub req: R,
    pub intra_offset: u64,
    pub intra_len: u64,
}

/// Per-slice page geometry, precomputed in [`PipelinedRead::new`].
struct SliceGeom {
    /// First backing page number (within the stripe) touched by the
    /// slice: `intra_offset / page_size`.
    first_page: u64,
    intra_offset: u64,
    intra_len: u64,
    /// Number of pages the slice spans.
    pages: u64,
    /// Number of global pages contributed by all earlier slices, so
    /// the slice owns global indices `[cum_before, cum_before + pages)`.
    cum_before: u64,
}

/// One outstanding fetch future tracked by a [`PipelinedRead`],
/// keyed by its global delivery index `g`.
struct PendingFetch {
    g: u64,
    slice_idx: usize,
    page_no: u64,
    fut: StaticBoxFuture<Result<u32, Error>>,
    /// Whether this fetch reserved a global prefetch-budget slot at
    /// launch (true for speculative pages, false for the head).
    counted: bool,
}

/// A completed fetch awaiting consumption in global order.
struct ReadyPage {
    result: Result<u32, Error>,
    counted: bool,
    slice_idx: usize,
    page_no: u64,
}

/// Admission callback: given a slice index, admit (or return the
/// already-admitted) per-stripe [`StreamSrc`]. Boxed so
/// `PipelinedRead` stays free of the pool's `T, S, R` generics, the
/// same erasure `WindowedRead` gets from `Rc<dyn StreamSrc>`.
type AdmitFn<'pool> = Box<dyn Fn(usize) -> Result<Rc<dyn StreamSrc + 'pool>, Error> + 'pool>;

/// Ordered, cross-stripe pipelined consumer surface. Yields one
/// [`PageGuard`] at a time in global order while prefetching ahead
/// across stripe boundaries.
pub struct PipelinedRead<'pool> {
    admit: AdmitFn<'pool>,
    geom: Vec<SliceGeom>,
    /// Lazily admitted stream per slice; `None` until first needed
    /// and reset to `None` once the slice is fully consumed.
    srcs: Vec<Option<Rc<dyn StreamSrc + 'pool>>>,
    page_size: u64,
    /// Global index of the next page to deliver, in `[0, total_g]`.
    head_g: u64,
    total_g: u64,
    window: usize,
    max_inflight_pages: usize,
    pending: Vec<PendingFetch>,
    ready: HashMap<u64, ReadyPage>,
    /// Lowest slice index not yet fully consumed; slices below this
    /// have been released. Used as the scan start for `locate`.
    first_active_slice: usize,
}

impl<'pool> std::fmt::Debug for PipelinedRead<'pool> {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("PipelinedRead")
            .field("head_g", &self.head_g)
            .field("total_g", &self.total_g)
            .field("slices", &self.geom.len())
            .field("window", &self.window)
            .field("pending", &self.pending.len())
            .field("ready", &self.ready.len())
            .finish()
    }
}

impl<'pool> PipelinedRead<'pool> {
    /// Build a pipelined read from an admission callback and the
    /// ordered list of `(intra_offset, intra_len)` slices. The caller
    /// (the pool) must supply a `slices` list aligned with whatever
    /// the `admit` closure indexes; both already have zero-length
    /// slices removed.
    pub(super) fn new(
        admit: AdmitFn<'pool>,
        slices: Vec<(u64, u64)>,
        page_size: u64,
        window: usize,
        max_inflight_pages: usize,
    ) -> Self {
        let mut geom = Vec::with_capacity(slices.len());
        let mut cum_before = 0u64;
        for (intra_offset, intra_len) in slices {
            // `slices` never contains zero-length entries (the pool
            // filters them), so `intra_len >= 1` and `last_page` does
            // not underflow.
            let first_page = intra_offset / page_size;
            let last_page = (intra_offset + intra_len - 1) / page_size;
            let pages = last_page - first_page + 1;
            geom.push(SliceGeom {
                first_page,
                intra_offset,
                intra_len,
                pages,
                cum_before,
            });
            cum_before += pages;
        }
        let total_g = cum_before;
        let srcs = (0..geom.len()).map(|_| None).collect();

        Self {
            admit,
            geom,
            srcs,
            page_size,
            head_g: 0,
            total_g,
            window: window.max(1),
            max_inflight_pages,
            pending: Vec::new(),
            ready: HashMap::new(),
            first_active_slice: 0,
        }
    }

    /// Next page in global order. Returns `None` at EOF. The returned
    /// [`PageGuard`] borrows `&mut self`, enforcing one-at-a-time
    /// consumption while the reader keeps prefetching ahead.
    pub async fn next_page<'s>(&'s mut self) -> Option<Result<PageGuard<'s>, Error>> {
        if self.head_g >= self.total_g {
            return None;
        }

        let head_g = self.head_g;
        let (slice_idx, page_no, intra_off, intra_len) = self.locate(head_g);
        // Admitting the head's stream can fail (stream limit); surface
        // it in order without advancing.
        if let Err(e) = self.src_for(slice_idx) {
            return Some(Err(e));
        }

        // Launch the head plus as many speculative pages as the window
        // and global budget permit, then drive every pending future
        // until the head completes.
        self.ensure_head_and_refill();
        DrivePipeline { r: self, head_g }.await;

        let entry = self
            .ready
            .remove(&head_g)
            .expect("head page must be ready after DrivePipeline resolves");
        if entry.counted {
            if let Some(src) = &self.srcs[entry.slice_idx] {
                src.release_prefetch();
            }
        }
        let page_idx = match entry.result {
            Ok(pi) => pi,
            // No consumer hold was transferred on error; surface it in
            // order without advancing the cursor.
            Err(e) => return Some(Err(e)),
        };

        // Capture the slice's stream for the guard BEFORE releasing
        // passed slices. `release_passed_slices` may `decrement_stream`
        // (and drop our cached `Rc`) for this very slice when this is
        // its last page; the guard's own `Rc` clone keeps the
        // `StreamSrc` alive, and the page's consumer hold keeps the
        // slot pinned until the guard drops, so `decrement_stream`'s
        // recyclable-slot sweep cannot reclaim it.
        let src_for_guard = self.srcs[slice_idx]
            .clone()
            .expect("slice stream admitted above");

        self.head_g += 1;
        self.release_passed_slices();
        self.ensure_head_and_refill();

        let base = src_for_guard.base();
        // SAFETY: identical invariants to `WindowedRead::next_page`.
        // `page_idx` names a pinned backing page; the fetch left a
        // consumer hold pinning the slot for the guard's lifetime, and
        // `intra_off + intra_len <= page_size`.
        let bytes =
            unsafe { base.add(page_idx as usize * self.page_size as usize + intra_off as usize) };
        let page_ref = PageRef {
            page_idx,
            offset: intra_off,
            len: intra_len,
        };
        let src: Rc<dyn StreamSrc + 's> = src_for_guard;
        let guard = PageGuard::new(src, page_no, bytes, intra_len, page_ref);
        Some(Ok(guard))
    }

    /// Test-only: current global head index.
    #[cfg(test)]
    #[allow(dead_code)]
    pub(crate) fn head_g(&self) -> u64 {
        self.head_g
    }

    /// Map a global delivery index to its slice, in-stripe page
    /// number, and the byte sub-range within that page.
    fn locate(&self, g: u64) -> (usize, u64, u32, u32) {
        let mut s = self.first_active_slice;
        // Advance past slices whose page span ends at or before `g`.
        while s + 1 < self.geom.len() && self.geom[s + 1].cum_before <= g {
            s += 1;
        }
        let geom = &self.geom[s];
        let k = g - geom.cum_before;
        let page_no = geom.first_page + k;
        let page_start = page_no * self.page_size;
        let lo = std::cmp::max(page_start, geom.intra_offset);
        let hi = std::cmp::min(
            page_start + self.page_size,
            geom.intra_offset + geom.intra_len,
        );
        let intra_off = (lo - page_start) as u32;
        let intra_len = (hi - lo) as u32;
        (s, page_no, intra_off, intra_len)
    }

    /// Get (admitting on first use) the stream for slice `s`.
    fn src_for(&mut self, s: usize) -> Result<Rc<dyn StreamSrc + 'pool>, Error> {
        if let Some(src) = &self.srcs[s] {
            return Ok(src.clone());
        }
        let src = (self.admit)(s)?;
        self.srcs[s] = Some(src.clone());
        Ok(src)
    }

    fn is_tracked(&self, g: u64) -> bool {
        self.ready.contains_key(&g) || self.pending.iter().any(|p| p.g == g)
    }

    /// Ensure the head page has an in-flight (or completed) fetch,
    /// then launch speculative fetches for subsequent global pages
    /// while the window has room and the global prefetch budget
    /// allows. Mirrors `WindowedRead::ensure_head_and_refill` but
    /// walks the global index across stripe boundaries.
    fn ensure_head_and_refill(&mut self) {
        if self.head_g >= self.total_g {
            return;
        }

        // The head is always fetched non-speculatively (blocking
        // alloc) and never counts against the prefetch budget;
        // `DrivePipeline` polls it before any speculative sibling so
        // it wins free-list races. If admitting its stream fails we
        // skip launching here; `next_page` already surfaced the error
        // via its own `src_for` check.
        let head_g = self.head_g;
        if !self.is_tracked(head_g) {
            let (s, page_no, _, _) = self.locate(head_g);
            if let Ok(src) = self.src_for(s) {
                let fut = src.fetch_page_owned(page_no, false);
                self.pending.push(PendingFetch {
                    g: head_g,
                    slice_idx: s,
                    page_no,
                    fut,
                    counted: false,
                });
            }
        }

        let mut g = head_g + 1;
        while g < self.total_g {
            if self.pending.len() + self.ready.len() >= self.window {
                break;
            }
            if self.is_tracked(g) {
                g += 1;
                continue;
            }
            let (s, page_no, _, _) = self.locate(g);
            let src = match self.src_for(s) {
                Ok(src) => src,
                // Stream limit reached while reaching into a new
                // stripe: stop speculating. The head still proceeds,
                // and the slice is retried as a (blocking) head later.
                Err(_) => break,
            };
            // Speculative pages must reserve global budget; if it is
            // exhausted, stop. Its leader allocates non-blocking and
            // backs off to `Error::PrefetchBackoff` if no page is
            // free, so speculation never parks ahead of the head.
            if !src.try_reserve_prefetch(self.max_inflight_pages) {
                break;
            }
            let fut = src.fetch_page_owned(page_no, true);
            self.pending.push(PendingFetch {
                g,
                slice_idx: s,
                page_no,
                fut,
                counted: true,
            });
            g += 1;
        }
    }

    /// Release every slice whose pages have all been consumed,
    /// returning its stream slot to the pool. Safe to call after each
    /// `head_g` advance: a fully-passed slice can have no outstanding
    /// pending/ready entries (those only exist for `g >= head_g`).
    fn release_passed_slices(&mut self) {
        while self.first_active_slice < self.geom.len() {
            let s = self.first_active_slice;
            let end_g = self.geom[s].cum_before + self.geom[s].pages;
            if self.head_g >= end_g {
                if let Some(src) = self.srcs[s].take() {
                    src.decrement_stream();
                }
                self.first_active_slice += 1;
            } else {
                break;
            }
        }
    }
}

impl<'pool> Drop for PipelinedRead<'pool> {
    fn drop(&mut self) {
        // Balance consumer holds for ready-but-unconsumed `Ok` pages
        // and release any prefetch budget still reserved for them.
        let ready = std::mem::take(&mut self.ready);
        for (_g, entry) in ready {
            if entry.result.is_ok() {
                if let Some(src) = &self.srcs[entry.slice_idx] {
                    src.release_guard(entry.page_no);
                }
            }
            if entry.counted {
                if let Some(src) = &self.srcs[entry.slice_idx] {
                    src.release_prefetch();
                }
            }
        }
        // Dropping a pending future runs its own internal RAII cleanup
        // (LeaderGuard / ConsumerHold inside `fetch_page`); we only owe
        // back the prefetch-budget reservation it held.
        let pending = std::mem::take(&mut self.pending);
        for pf in pending {
            if pf.counted {
                if let Some(src) = &self.srcs[pf.slice_idx] {
                    src.release_prefetch();
                }
            }
        }
        // Decrement every still-admitted stream exactly once
        // (idempotent guard inside `decrement_stream`).
        for slot in self.srcs.iter_mut() {
            if let Some(src) = slot.take() {
                src.decrement_stream();
            }
        }
    }
}

/// Drives every outstanding fetch future concurrently with a single
/// `cx`, moving completions into `ready`. Polls the head future first
/// so it wins any free-list allocation race against speculative
/// siblings. Resolves once the head page is `ready`. Mirrors
/// `window::DrivePending` over the global index.
struct DrivePipeline<'a, 'pool> {
    r: &'a mut PipelinedRead<'pool>,
    head_g: u64,
}

impl<'a, 'pool> Future for DrivePipeline<'a, 'pool> {
    type Output = ();

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
        // `DrivePipeline` holds only a `&mut` and a `u64`; it is `Unpin`.
        let this = self.get_mut();
        let head_g = this.head_g;
        let r = &mut *this.r;

        // Head-of-line: poll the head first so a freshly freed page is
        // claimed by the head's leader before any speculative sibling.
        if let Some(i) = r.pending.iter().position(|p| p.g == head_g) {
            if let Poll::Ready(res) = r.pending[i].fut.as_mut().poll(cx) {
                let pf = r.pending.swap_remove(i);
                r.ready.insert(
                    pf.g,
                    ReadyPage {
                        result: res,
                        counted: pf.counted,
                        slice_idx: pf.slice_idx,
                        page_no: pf.page_no,
                    },
                );
            }
        }

        // Drive the remaining (speculative) futures.
        let mut i = 0;
        while i < r.pending.len() {
            if r.pending[i].g == head_g {
                i += 1;
                continue;
            }
            match r.pending[i].fut.as_mut().poll(cx) {
                Poll::Ready(res) => {
                    let pf = r.pending.swap_remove(i);
                    // A speculative leader that found no free page
                    // backs off rather than parking. Not a
                    // consumer-visible error: drop it and return its
                    // prefetch-budget reservation so it can be retried
                    // when pages free up. By the time the cursor could
                    // reach this page it has been removed here, so
                    // `ensure_head_and_refill` relaunches it as a
                    // blocking head; a head fetch is never speculative
                    // and so never backs off, which is why the head
                    // branch above never sees this.
                    if matches!(res, Err(Error::PrefetchBackoff)) {
                        debug_assert!(pf.counted);
                        if pf.counted {
                            if let Some(src) = &r.srcs[pf.slice_idx] {
                                src.release_prefetch();
                            }
                        }
                        continue;
                    }
                    r.ready.insert(
                        pf.g,
                        ReadyPage {
                            result: res,
                            counted: pf.counted,
                            slice_idx: pf.slice_idx,
                            page_no: pf.page_no,
                        },
                    );
                }
                Poll::Pending => i += 1,
            }
        }

        if r.ready.contains_key(&head_g) {
            Poll::Ready(())
        } else {
            Poll::Pending
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Build a `PipelinedRead` with an admit closure that panics if
    /// called. The geometry-only methods under test (`locate`, `new`)
    /// never admit a stream, so this isolates the global<->stripe
    /// index mapping from the pool.
    fn geom_only<'a>(slices: Vec<(u64, u64)>, page_size: u64) -> PipelinedRead<'a> {
        let admit: AdmitFn<'a> = Box::new(|_s| panic!("admit must not be called in geometry test"));
        PipelinedRead::new(admit, slices, page_size, 8, 8)
    }

    #[test]
    fn single_full_stripe_two_pages() {
        let r = geom_only(vec![(0, 4096)], 2048);
        assert_eq!(r.total_g, 2);
        assert_eq!(r.locate(0), (0, 0, 0, 2048));
        assert_eq!(r.locate(1), (0, 1, 0, 2048));
    }

    #[test]
    fn intra_page_offsets_on_first_and_last() {
        // Slice [512, 2560) over 2048-byte pages spans pages 0 and 1
        // with partial coverage on both.
        let r = geom_only(vec![(512, 2048)], 2048);
        assert_eq!(r.total_g, 2);
        // Page 0: bytes [512, 2048) -> offset 512, len 1536.
        assert_eq!(r.locate(0), (0, 0, 512, 1536));
        // Page 1: bytes [2048, 2560) -> offset 0, len 512.
        assert_eq!(r.locate(1), (0, 1, 0, 512));
    }

    #[test]
    fn multi_slice_global_mapping() {
        // Slice 0: one page (g=0). Slice 1: two pages (g=1, g=2).
        let r = geom_only(vec![(0, 2048), (0, 4096)], 2048);
        assert_eq!(r.total_g, 3);
        assert_eq!(r.locate(0), (0, 0, 0, 2048));
        assert_eq!(r.locate(1), (1, 0, 0, 2048));
        assert_eq!(r.locate(2), (1, 1, 0, 2048));
    }

    #[test]
    fn sub_page_single_slice() {
        // A slice smaller than a page yields exactly one global page
        // with the requested sub-range.
        let r = geom_only(vec![(100, 200)], 2048);
        assert_eq!(r.total_g, 1);
        assert_eq!(r.locate(0), (0, 0, 100, 200));
    }
}
