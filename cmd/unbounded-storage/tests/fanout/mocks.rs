// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! DST-aware mocks for the owner-side `Pool` behind a `FetchService`.
//!
//! The owner pool is generic over `Transport<StripeReq>` and
//! `BlockStore`; these mocks supply both. Each async method routes its
//! "I/O latency" through [`yield_n`] with a per-call random count drawn
//! from the framework's [`SimState::rng`], and optionally returns a
//! synthetic error governed by [`MockSimConfig::io_fault_rate`]. The
//! blockstore optionally serves a page directly (cache hit) so the
//! owner read path is exercised through both the disk-hit branch and
//! the transport-fetch branch. All knobs live here rather than in the
//! framework so the framework stays project-agnostic.

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
use unbounded_storage::storage::StripeReq;

use crate::framework::executor::{with_sim, yield_n};

/// Owner-pool simulation knobs that ride alongside the framework's
/// [`SimState`]. Held behind an `Rc` so both mocks plus the workload
/// driver can share a single configuration instance.
#[derive(Default)]
pub struct MockSimConfig {
    /// Maximum number of `yield_once` pends an I/O mock emits before
    /// completing. The actual count per call is drawn from the PRNG.
    pub max_io_delay: Cell<u32>,
    /// Probability in `[0, 100]` that an I/O mock returns a synthetic
    /// error after its delay. `0` disables faults (happy path).
    pub io_fault_rate: Cell<u32>,
    /// Probability in `[0, 100]` that `read_page` serves the page from
    /// "disk" (a hit) instead of returning `Ok(false)` and falling
    /// through to the transport. `0` is the miss-only regime.
    pub cache_hit_rate: Cell<u32>,
}

impl MockSimConfig {
    pub fn new() -> Rc<Self> {
        Rc::new(Self::default())
    }
}

fn draw_delay(cfg: &MockSimConfig) -> u32 {
    let max = cfg.max_io_delay.get();
    if max == 0 {
        0
    } else {
        with_sim(|s| s.rng.gen_range(0..=max))
    }
}

fn draw_fault(cfg: &MockSimConfig) -> bool {
    let rate = cfg.io_fault_rate.get();
    rate > 0 && with_sim(|s| s.rng.gen_ratio(rate.min(100), 100))
}

/// Canonical bytes per stripe. Both mocks copy out of this map; the
/// workload uses the same bytes as its read oracle.
pub type Stripes = Rc<RefCell<HashMap<StripeKey, Vec<u8>>>>;

#[derive(Default)]
pub struct CallCounts {
    pub bulk_get: Cell<u32>,
    pub read_page: Cell<u32>,
    pub read_hit: Cell<u32>,
}

/// Copy `len` bytes of `key`'s stripe starting at `src_off` into the
/// pool page described by `dst`, relative to the registered `base`.
///
/// # Safety
/// `dst` is a pool-owned page within the registered backing and the
/// oracle bytes outlive the call; the caller guarantees `base` is the
/// live backing base and the source range is in bounds.
unsafe fn copy_stripe_into_page(
    stripes: &Stripes,
    base: *mut u8,
    page_size: usize,
    key: StripeKey,
    src_off: usize,
    len: usize,
    dst: PageRef,
) {
    let stripes = stripes.borrow();
    let bytes = stripes.get(&key).expect("stripe not configured");
    assert!(src_off + len <= bytes.len(), "src out of range");
    unsafe {
        let dst_ptr = base.add(dst.page_idx as usize * page_size + dst.offset as usize);
        std::ptr::copy_nonoverlapping(bytes.as_ptr().add(src_off), dst_ptr, len);
    }
}

pub struct FanoutTransport {
    stripes: Stripes,
    counts: Rc<CallCounts>,
    cfg: Rc<MockSimConfig>,
    base: *mut u8,
    page_size: usize,
}

impl FanoutTransport {
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

    async fn do_bulk_get(&self, src: BulkRef, dst: PageRef) -> Result<(), Error> {
        let delay = draw_delay(&self.cfg);
        let fault = draw_fault(&self.cfg);
        yield_n(delay).await;
        if fault {
            return Err(Error::from("dst: injected transport fault"));
        }
        // SAFETY: see `copy_stripe_into_page`; `self.base` is the live
        // backing base bound at construction and `src` is in range.
        unsafe {
            copy_stripe_into_page(
                &self.stripes,
                self.base,
                self.page_size,
                src.stripe,
                src.offset as usize,
                src.len as usize,
                dst,
            );
        }
        self.counts.bulk_get.set(self.counts.bulk_get.get() + 1);
        Ok(())
    }
}

/// Single-page stream returned by `FanoutTransport::bulk_get`. The pool
/// always issues a one-element `dsts`.
pub struct FanoutBulkStream<'a> {
    fut: Option<Pin<Box<dyn Future<Output = Result<(), Error>> + 'a>>>,
    page: PageRef,
}

impl<'a> PageStream for FanoutBulkStream<'a> {
    fn poll_next(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<Option<Result<PageRef, Error>>> {
        // SAFETY: structural pin projection; `fut` is the only polled
        // field and we never move out of it.
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

impl Transport<StripeReq> for FanoutTransport {
    type Stream<'a> = FanoutBulkStream<'a>;

    fn bulk_get<'a>(
        &'a self,
        _req: &'a StripeReq,
        src: BulkRef,
        dsts: &'a [PageRef],
    ) -> Self::Stream<'a> {
        assert_eq!(dsts.len(), 1, "Pool issues single-page bulk_get");
        let page = dsts[0];
        FanoutBulkStream {
            fut: Some(Box::pin(self.do_bulk_get(src, page))),
            page,
        }
    }
}

/// Blockstore mock with a configurable hit rate. On a miss returns
/// `Ok(false)` and the pool falls through to the transport. On a hit
/// copies the canonical stripe bytes into `dst` and returns `Ok(true)`,
/// exercising the owner read path's disk-hit branch.
pub struct FanoutBlockStore {
    counts: Rc<CallCounts>,
    stripes: Stripes,
    cfg: Rc<MockSimConfig>,
    base: Cell<Option<*mut u8>>,
    page_size: Cell<usize>,
}

impl FanoutBlockStore {
    pub fn new(counts: Rc<CallCounts>, stripes: Stripes, cfg: Rc<MockSimConfig>) -> Self {
        Self {
            counts,
            stripes,
            cfg,
            base: Cell::new(None),
            page_size: Cell::new(0),
        }
    }
}

impl BlockStore for FanoutBlockStore {
    fn register_pages(&self, backing: &Backing) -> Result<(), Error> {
        self.base.set(Some(backing.base));
        self.page_size.set(backing.page_size);
        Ok(())
    }

    async fn read_page<R: Req + ?Sized>(
        &self,
        req: &R,
        stripe_off: u64,
        dst: PageRef,
    ) -> Result<bool, Error> {
        let key = req.key();
        let delay = draw_delay(&self.cfg);
        let hit = self.cfg.cache_hit_rate.get() > 0
            && with_sim(|s| s.rng.gen_ratio(self.cfg.cache_hit_rate.get().min(100), 100));
        yield_n(delay).await;
        self.counts.read_page.set(self.counts.read_page.get() + 1);
        if !hit {
            return Ok(false);
        }
        let page_size = self.page_size.get();
        let base = self.base.get().expect("register_pages must run first");
        // SAFETY: see `copy_stripe_into_page`; `dst` is pool-owned and
        // `stripe_off + page_size` is in range for a full page read.
        unsafe {
            copy_stripe_into_page(
                &self.stripes,
                base,
                page_size,
                key,
                stripe_off as usize,
                dst.len as usize,
                dst,
            );
        }
        self.counts.read_hit.set(self.counts.read_hit.get() + 1);
        Ok(true)
    }

    async fn write_page<R: Req + ?Sized>(
        &self,
        _req: &R,
        _stripe_off: u64,
        _page: PageRef,
    ) -> Result<(), Error> {
        let delay = draw_delay(&self.cfg);
        yield_n(delay).await;
        Ok(())
    }
}
