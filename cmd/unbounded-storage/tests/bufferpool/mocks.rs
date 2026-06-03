// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! DST-aware mocks for the bufferpool's `Transport` and `BlockStore`.
//!
//! Each async method routes its "I/O latency" through [`yield_n`]
//! with a per-call random count drawn from the framework's
//! [`SimState::rng`], and optionally returns a synthetic error
//! governed by [`MockSimConfig::io_fault_rate`]. The simulation knobs
//! (delay bound, fault rate, cache hit rate) live here rather than
//! in the framework so the DST framework remains project-agnostic.
//! Counters are exposed so tests can assert higher-level properties
//! (e.g. single-flight coalescing).

use std::cell::{Cell, RefCell};
use std::collections::HashMap;
use std::future::Future;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll};

use rand::Rng;
use unbounded_storage::bufferpool::{
    BlockStore, BulkRef, Error, PageRef, PageStream, Req, StripeKey, Transport,
};
use unbounded_storage::memory::Backing;

use crate::framework::executor::{with_sim, yield_n};

/// Bufferpool-specific simulation knobs that ride alongside the
/// framework's [`SimState`]. Held behind an `Rc` so both mocks plus
/// the workload driver can share a single configuration instance
/// without leaking knowledge into the framework crate.
#[derive(Default)]
pub struct MockSimConfig {
    /// Maximum number of `yield_once` pends an I/O mock will emit
    /// before completing. The actual count per call is drawn from
    /// the framework's PRNG.
    pub max_io_delay: Cell<u32>,
    /// Probability in `[0, 100]` that an I/O mock returns a
    /// synthetic error after its delay. `0` disables faults (the
    /// happy-path regime); positive values exercise the
    /// leader-error / `ParkOutcome::Error` paths in `pool.rs`.
    pub io_fault_rate: Cell<u32>,
}

impl MockSimConfig {
    pub fn new() -> Rc<Self> {
        Rc::new(Self::default())
    }
}

/// Helper: draw a `[0, max_io_delay]` delay from the framework PRNG.
fn draw_delay(cfg: &MockSimConfig) -> u32 {
    let max = cfg.max_io_delay.get();
    if max == 0 {
        0
    } else {
        with_sim(|s| s.rng.gen_range(0..=max))
    }
}

/// Helper: draw a fault decision from the framework PRNG.
fn draw_fault(cfg: &MockSimConfig) -> bool {
    let rate = cfg.io_fault_rate.get();
    rate > 0 && with_sim(|s| s.rng.gen_ratio(rate.min(100), 100))
}

/// Test request type. The pool only inspects `req.key()`.
#[derive(Clone, Debug)]
pub struct TestReq {
    pub key: StripeKey,
}

impl Req for TestReq {
    fn key(&self) -> StripeKey {
        self.key
    }
}

/// Canonical bytes per stripe. The transport copies out of this map
/// on `bulk_get`; tests use it as the oracle for byte verification.
pub type Stripes = Rc<RefCell<HashMap<StripeKey, Vec<u8>>>>;

#[derive(Default)]
pub struct CallCounts {
    pub bulk_get: Cell<u32>,
    pub read_page: Cell<u32>,
    pub write_page: Cell<u32>,
    /// `(key, page_no) -> bulk_get count` for the single-flight
    /// invariant. Keyed by intra-stripe page number, not byte offset.
    pub bulk_get_by_page: RefCell<HashMap<(StripeKey, u64), u32>>,
    /// `(key, page_no) -> max observed in-flight `bulk_get`s`. The
    /// single-flight invariant tolerates sequential re-issues
    /// (slot recycled, then refetched later) but forbids two
    /// `bulk_get`s overlapping for the same logical page.
    pub bulk_get_inflight: RefCell<HashMap<(StripeKey, u64), u32>>,
    pub bulk_get_max_inflight: RefCell<HashMap<(StripeKey, u64), u32>>,
}

pub struct DstTransport {
    stripes: Stripes,
    counts: Rc<CallCounts>,
    cfg: Rc<MockSimConfig>,
    /// Bound at construction. The `Transport` trait no longer
    /// carries a `register_pages` hook: the embedder pre-registers
    /// the backing out-of-band, so the mock learns the geometry the
    /// same way a real transport does (constructor argument from
    /// whoever owns the `Backing`).
    base: *mut u8,
    page_size: usize,
}

impl DstTransport {
    pub fn new(
        stripes: Stripes,
        counts: Rc<CallCounts>,
        cfg: Rc<MockSimConfig>,
        base: *mut u8,
        page_size: usize,
    ) -> Self {
        Self {
            stripes,
            counts,
            cfg,
            base,
            page_size,
        }
    }

    async fn do_bulk_get(&self, _req: &TestReq, src: BulkRef, dst: PageRef) -> Result<(), Error> {
        // Pull delay and (optional) fault decision up front; this
        // keeps the PRNG draws deterministic across re-orderings of
        // independent tasks.
        let delay = draw_delay(&self.cfg);
        let fault = draw_fault(&self.cfg);

        let page_size = self.page_size;
        let page_no = src.offset / page_size as u64;
        // Track concurrent in-flight for the single-flight invariant
        // via an RAII guard so that a `bulk_get` future dropped
        // mid-flight (the pool's leader-drop / subscriber-takeover
        // path, which cancels the in-flight I/O) is correctly
        // un-counted. A manual decrement at the end would leak the
        // count on cancellation and spuriously report two concurrent
        // `bulk_get`s when the next leader re-issues for the page.
        let _inflight = InflightGuard::enter(&self.counts, src.stripe, page_no);
        yield_n(delay).await;
        if fault {
            return Err(Error::from("dst: injected transport fault"));
        }

        // Copy stripe bytes into the destination page.
        let stripes = self.stripes.borrow();
        let bytes = stripes
            .get(&src.stripe)
            .expect("DstTransport: stripe not configured");
        let start = src.offset as usize;
        let end = start + src.len as usize;
        assert!(end <= bytes.len(), "DstTransport: src out of range");

        // SAFETY: dst is a pool-owned page within the registered
        // backing; src is a Vec<u8> owned by `stripes`. Both ranges
        // are valid for the duration of this call.
        unsafe {
            let dst_ptr = self
                .base
                .add(dst.page_idx as usize * page_size + dst.offset as usize);
            std::ptr::copy_nonoverlapping(bytes.as_ptr().add(start), dst_ptr, src.len as usize);
        }

        self.counts.bulk_get.set(self.counts.bulk_get.get() + 1);
        let mut by_page = self.counts.bulk_get_by_page.borrow_mut();
        *by_page.entry((src.stripe, page_no)).or_insert(0) += 1;
        Ok(())
    }
}

/// RAII tracker for concurrent `bulk_get`s against one logical page.
/// Increments the in-flight count (and bumps the observed max) on
/// `enter`, decrements on drop. Drop-based decrement is what makes
/// the single-flight invariant robust to the pool dropping a leader
/// future mid-I/O: that cancels the I/O, so the page is no longer
/// in-flight even though `do_bulk_get` never ran to completion.
struct InflightGuard<'a> {
    counts: &'a CallCounts,
    key: StripeKey,
    page_no: u64,
}

impl<'a> InflightGuard<'a> {
    fn enter(counts: &'a CallCounts, key: StripeKey, page_no: u64) -> Self {
        let mut inflight = counts.bulk_get_inflight.borrow_mut();
        let entry = inflight.entry((key, page_no)).or_insert(0);
        *entry += 1;
        let cur = *entry;
        let mut max = counts.bulk_get_max_inflight.borrow_mut();
        let m = max.entry((key, page_no)).or_insert(0);
        if cur > *m {
            *m = cur;
        }
        Self {
            counts,
            key,
            page_no,
        }
    }
}

impl<'a> Drop for InflightGuard<'a> {
    fn drop(&mut self) {
        let mut inflight = self.counts.bulk_get_inflight.borrow_mut();
        if let Some(e) = inflight.get_mut(&(self.key, self.page_no)) {
            *e = e.saturating_sub(1);
        }
    }
}

/// Single-page stream returned by `DstTransport::bulk_get`. Pool
/// always issues a one-element `dsts`; the stream yields that page
/// when the underlying future resolves `Ok`, or surfaces the error.
pub struct DstBulkStream<'a> {
    fut: Option<Pin<Box<dyn Future<Output = Result<(), Error>> + 'a>>>,
    page: PageRef,
}

impl<'a> PageStream for DstBulkStream<'a> {
    fn poll_next(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<Option<Result<PageRef, Error>>> {
        // SAFETY: structural pin projection; `fut` is the only
        // polled field and we never move out of it.
        let this = unsafe { self.as_mut().get_unchecked_mut() };
        let Some(fut) = this.fut.as_mut() else {
            return Poll::Ready(None);
        };
        match fut.as_mut().poll(cx) {
            Poll::Pending => Poll::Pending,
            Poll::Ready(Ok(())) => {
                this.fut = None;
                Poll::Ready(Some(Ok(this.page)))
            }
            Poll::Ready(Err(e)) => {
                this.fut = None;
                Poll::Ready(Some(Err(e)))
            }
        }
    }
}

impl Transport<TestReq> for DstTransport {
    type Stream<'a> = DstBulkStream<'a>;

    fn bulk_get<'a>(
        &'a self,
        req: &'a TestReq,
        src: BulkRef,
        dsts: &'a [PageRef],
    ) -> Self::Stream<'a> {
        assert_eq!(dsts.len(), 1, "Pool issues single-page bulk_get");
        let page = dsts[0];
        DstBulkStream {
            fut: Some(Box::pin(self.do_bulk_get(req, src, page))),
            page,
        }
    }
}

/// Blockstore mock with a configurable hit rate. On a miss
/// (probability `1 - hit_rate/100`) returns `Ok(false)` and the
/// pool falls through to `Transport::bulk_get`. On a hit, copies
/// the canonical stripe bytes into the destination page and
/// returns `Ok(true)`, exercising the fast-path branch in
/// `Pool::fetch_page` that *skips* `bulk_get` and the tee.
pub struct DstBlockStore {
    counts: Rc<CallCounts>,
    stripes: Stripes,
    cfg: Rc<MockSimConfig>,
    base: Cell<Option<*mut u8>>,
    page_size: Cell<usize>,
    /// `0` = miss-only (the original v1 behavior); `100` = always
    /// hit; intermediate values inject hits probabilistically.
    hit_rate: Cell<u32>,
}

impl DstBlockStore {
    pub fn new(counts: Rc<CallCounts>, stripes: Stripes, cfg: Rc<MockSimConfig>) -> Self {
        Self {
            counts,
            stripes,
            cfg,
            base: Cell::new(None),
            page_size: Cell::new(0),
            hit_rate: Cell::new(0),
        }
    }

    pub fn set_hit_rate(&self, pct: u32) {
        self.hit_rate.set(pct.min(100));
    }
}

impl BlockStore for DstBlockStore {
    fn register_pages(&self, backing: &Backing) -> Result<(), Error> {
        self.base.set(Some(backing.base));
        self.page_size.set(backing.page_size);
        Ok(())
    }

    async fn read_page(
        &self,
        key: StripeKey,
        stripe_off: u64,
        dst: PageRef,
    ) -> Result<bool, Error> {
        let delay = draw_delay(&self.cfg);
        let hit = self.hit_rate.get() > 0
            && with_sim(|s| s.rng.gen_ratio(self.hit_rate.get().min(100), 100));
        yield_n(delay).await;
        self.counts.read_page.set(self.counts.read_page.get() + 1);
        if !hit {
            return Ok(false);
        }

        // Hit: copy oracle bytes for this page into `dst`. Mirrors
        // what a real on-disk cache would do.
        let page_size = self.page_size.get();
        let stripes = self.stripes.borrow();
        let bytes = stripes
            .get(&key)
            .expect("DstBlockStore: stripe not configured");
        let start = stripe_off as usize;
        let end = start + page_size;
        assert!(end <= bytes.len(), "DstBlockStore: stripe_off out of range");
        let base = self.base.get().expect("register_pages must run first");
        // SAFETY: dst is a pool-owned page within the registered
        // backing; oracle bytes outlive this call.
        unsafe {
            let dst_ptr = base.add(dst.page_idx as usize * page_size + dst.offset as usize);
            std::ptr::copy_nonoverlapping(bytes.as_ptr().add(start), dst_ptr, page_size);
        }
        Ok(true)
    }

    async fn write_page(
        &self,
        _key: StripeKey,
        _stripe_off: u64,
        _page: PageRef,
    ) -> Result<(), Error> {
        let delay = draw_delay(&self.cfg);
        yield_n(delay).await;
        self.counts.write_page.set(self.counts.write_page.get() + 1);
        Ok(())
    }
}
