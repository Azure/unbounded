// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Property tests for the cross-shard fan-out path. Each invariant is
//! a small `assert_*` helper returning `Result<(), TestCaseError>`, so
//! shrinking output stays legible. A single `proptest!` block drives
//! `run_workload` once per case and dispatches to every invariant.

use std::pin::Pin;
use std::rc::Rc;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::task::{Context, Poll, Wake, Waker};

use proptest::prelude::*;
use unbounded_storage::bufferpool::{
    BlockStore, BulkRef, Error, PageRef, PageStream, Pool, PoolConfig, Req, StripeKey, Transport,
};
use unbounded_storage::fanout::{FetchChannel, FetchService};
use unbounded_storage::memory::Backing;
use unbounded_storage::storage::StripeReq;

use crate::fanout::workload::{FetchOutcome, RunReport, run_workload, workload_strategy};

proptest! {
    #![proptest_config(ProptestConfig {
        // Keep CI runtime modest; bump locally (or via
        // `PROPTEST_CASES`) for soak runs.
        cases: 256,
        ..ProptestConfig::default()
    })]

    #[test]
    fn fanout_invariants(seed in any::<u64>(), w in workload_strategy()) {
        let report = run_workload(seed, w).expect("run completed without deadlock or budget exhaustion");
        assert_bytes_match(&report)?;
        assert_pagelocs_cover(&report)?;
        assert_no_pin_leak(&report)?;
        assert_faults_only_with_injection(&report)?;
        assert_shutdown_send_errors(&report)?;
        assert_busy_only_under_pressure(&report)?;
    }
}

/// Invariant: zero-copy byte correctness across the round-trip.
///
/// For every successful fetch, the bytes read from the owner backing
/// at the returned `PageLoc`s - after the `hold` delay, while the owner
/// still holds the pages pinned - must equal the oracle slice. This is
/// the core guarantee: the owner's pin keeps the source pages valid and
/// unmodified from reply until release, so the coordinator's `SEND_ZC`
/// (modeled here as a direct memory read) observes correct bytes.
fn assert_bytes_match(report: &RunReport) -> Result<(), TestCaseError> {
    for (i, o) in report.outcomes.iter().enumerate() {
        if let FetchOutcome::Ok { got, expected, .. } = o {
            prop_assert_eq!(got, expected, "fetch {} bytes mismatch", i);
        }
    }
    Ok(())
}

/// Invariant: the reply's `PageLoc`s exactly cover the requested range
/// and stay within the owner backing.
///
/// The concatenated `PageLoc` lengths must equal the requested byte
/// length, each loc must be non-empty (every requested fetch has
/// `len >= 1`), and `page_byte_offset + len` must fit inside the pool's
/// backing. A loc pointing outside the backing would mean the
/// coordinator reads out of bounds during `SEND_ZC`.
fn assert_pagelocs_cover(report: &RunReport) -> Result<(), TestCaseError> {
    let backing_bytes = (report.total_pool_pages * report.page_size) as u64;
    for (i, o) in report.outcomes.iter().enumerate() {
        if let FetchOutcome::Ok { page_locs, len, .. } = o {
            let covered: u64 = page_locs.iter().map(|(_, l)| *l as u64).sum();
            prop_assert_eq!(
                covered,
                *len,
                "fetch {} pagelocs cover {} bytes, expected {}",
                i,
                covered,
                len,
            );
            for (off, l) in page_locs {
                prop_assert!(*l > 0, "fetch {} has an empty PageLoc", i);
                prop_assert!(
                    off + *l as u64 <= backing_bytes,
                    "fetch {} PageLoc {}+{} exceeds backing {}",
                    i,
                    off,
                    l,
                    backing_bytes,
                );
            }
        }
    }
    Ok(())
}

/// Invariant: no owner pin leak at quiescence.
///
/// After every client has released its pins and the service has
/// drained, all pool pages must be back on the free list. Bufferpool DST
/// owns the lower-level inflight stripe-fetch accounting; fanout only
/// checks the owner pin lifetime and release contract.
fn assert_no_pin_leak(report: &RunReport) -> Result<(), TestCaseError> {
    prop_assert_eq!(
        report.free_pages_at_end,
        report.total_pool_pages,
        "pages leaked: free={} expected {}",
        report.free_pages_at_end,
        report.total_pool_pages,
    );
    Ok(())
}

/// Invariant: fetch errors only occur under fault injection.
///
/// With `io_fault_rate == 0` the owner read path never surfaces an error
/// to the coordinator, so every fetch must resolve `Ok`. This now also
/// guards the page-pressure path: under `tight_pool` the owner returns
/// `Error::Busy` when it cannot pin, but coordinators retry until they
/// succeed, so `Busy` must never leak out as a `FetchOutcome::Err`. A
/// happy-path run producing a `FetchOutcome::Err` is itself a bug.
fn assert_faults_only_with_injection(report: &RunReport) -> Result<(), TestCaseError> {
    if report.io_fault_rate == 0 {
        for (i, o) in report.outcomes.iter().enumerate() {
            if let FetchOutcome::Err(e) = o {
                prop_assert!(false, "fetch {} errored ({}) with io_fault_rate=0", i, e);
            }
        }
    }
    Ok(())
}

/// Invariant: a fetch after service shutdown errors rather than parks.
///
/// When the probe ran, the service task had already returned and
/// dropped the receiver, so the surviving channel clone's `fetch` must
/// resolve with an error (the framework would otherwise hang, which the
/// probe's bounded spin would have turned into a panic).
fn assert_shutdown_send_errors(report: &RunReport) -> Result<(), TestCaseError> {
    if let Some(errored) = report.post_shutdown_send_errored {
        prop_assert!(errored, "post-shutdown fetch unexpectedly succeeded");
    }
    Ok(())
}

/// Invariant: `Error::Busy` back-pressure only arises under page
/// pressure.
///
/// The owner read path returns `Busy` (and coordinators retry) only when
/// the pool cannot pin a fetch's head page non-blockingly. The generous
/// pool sizing holds every distinct stripe at once, so it must never
/// produce a retry; any `busy_retries` there would mean the fail-fast
/// path fired when there was capacity. A positive count is expected only
/// under `tight_pool`.
fn assert_busy_only_under_pressure(report: &RunReport) -> Result<(), TestCaseError> {
    if !report.tight_pool {
        prop_assert_eq!(
            report.busy_retries,
            0,
            "{} Busy retries on a generously sized pool",
            report.busy_retries,
        );
    }
    Ok(())
}

#[test]
fn progress_polls_inflight_fetches_with_configured_waker() {
    let page_size = 64;
    let page_count = 1;
    let backing = heap_backing(page_size, page_count);
    let base = backing.base;
    let payload: Vec<u8> = (0..page_size).map(|i| i as u8).collect();
    let pool = Rc::new(
        Pool::new(
            PoolConfig {
                max_concurrent_streams: 4,
                max_inflight_pages: 1,
            },
            backing,
            PendingTransport {
                base,
                payload: payload.clone(),
            },
            MissBlockStore,
        )
        .expect("pool"),
    );
    let (channel, rx) = FetchChannel::new();
    let wakes = Arc::new(AtomicUsize::new(0));
    let waker: Waker = Arc::new(CountWaker {
        wakes: wakes.clone(),
    })
    .into();
    let mut service = FetchService::new(pool.clone(), rx, page_size, waker);

    let req = StripeReq::new(StripeKey([9u8; 32]));
    let mut fetch = Box::pin(channel.fetch(req, 0, page_size as u64));
    let noop = unbounded_storage::runtime::noop_waker();
    let mut cx = Context::from_waker(&noop);

    assert!(fetch.as_mut().poll(&mut cx).is_pending());

    assert!(service.progress(), "admitting the queued fetch is progress");
    assert_eq!(
        wakes.load(Ordering::SeqCst),
        1,
        "first in-flight poll must wake the configured shard waker",
    );

    assert!(
        !service.progress(),
        "a still-pending fetch without command admission or completion must not report busy",
    );
    assert_eq!(
        wakes.load(Ordering::SeqCst),
        2,
        "subsequent pending poll must still use the configured shard waker",
    );

    assert!(service.progress(), "the final fetch completion is progress");
    let reply = match fetch.as_mut().poll(&mut cx) {
        Poll::Ready(Ok(reply)) => reply,
        other => panic!("fetch did not resolve after completion: {other:?}"),
    };
    assert_eq!(reply.pages.len(), 1);
    assert_eq!(reply.pages[0].len, page_size as u32);

    channel.release(reply.pin_token);
    assert!(service.progress(), "release command is progress");
    assert_eq!(pool.free_pages(), page_count);
}

struct CountWaker {
    wakes: Arc<AtomicUsize>,
}

impl Wake for CountWaker {
    fn wake(self: Arc<Self>) {
        self.wake_by_ref();
    }

    fn wake_by_ref(self: &Arc<Self>) {
        self.wakes.fetch_add(1, Ordering::SeqCst);
    }
}

struct PendingTransport {
    base: *mut u8,
    payload: Vec<u8>,
}

impl Transport<StripeReq> for PendingTransport {
    type Stream<'a> = PendingStream<'a>;

    fn bulk_get<'a>(
        &'a self,
        _req: &'a StripeReq,
        src: BulkRef,
        dsts: &'a [PageRef],
    ) -> Self::Stream<'a> {
        assert_eq!(src.offset, 0);
        assert_eq!(dsts.len(), 1);
        PendingStream {
            base: self.base,
            payload: &self.payload,
            dst: dsts[0],
            pending_left: 2,
            delivered: false,
        }
    }
}

struct PendingStream<'a> {
    base: *mut u8,
    payload: &'a [u8],
    dst: PageRef,
    pending_left: u8,
    delivered: bool,
}

impl PageStream for PendingStream<'_> {
    fn poll_next(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<Option<Result<PageRef, Error>>> {
        if self.pending_left > 0 {
            self.pending_left -= 1;
            cx.waker().wake_by_ref();
            return Poll::Pending;
        }

        if self.delivered {
            return Poll::Ready(None);
        }

        let len = self.dst.len as usize;
        assert!(len <= self.payload.len());
        unsafe {
            let dst = self
                .base
                .add(self.dst.page_idx as usize * self.payload.len() + self.dst.offset as usize);
            std::ptr::copy_nonoverlapping(self.payload.as_ptr(), dst, len);
        }
        self.delivered = true;
        Poll::Ready(Some(Ok(self.dst)))
    }
}

struct MissBlockStore;

impl BlockStore for MissBlockStore {
    fn register_pages(&self, _backing: &Backing) -> Result<(), Error> {
        Ok(())
    }

    async fn read_page<R: Req + ?Sized>(
        &self,
        _req: &R,
        _stripe_off: u64,
        _dst: PageRef,
    ) -> Result<bool, Error> {
        Ok(false)
    }

    async fn write_page<R: Req + ?Sized>(
        &self,
        _req: &R,
        _stripe_off: u64,
        _page: PageRef,
    ) -> Result<(), Error> {
        Ok(())
    }
}

struct HeapOwner {
    ptr: *mut u8,
    layout: std::alloc::Layout,
}

unsafe impl Send for HeapOwner {}
unsafe impl Sync for HeapOwner {}

impl Drop for HeapOwner {
    fn drop(&mut self) {
        unsafe {
            std::alloc::dealloc(self.ptr, self.layout);
        }
    }
}

fn heap_backing(page_size: usize, page_count: usize) -> Backing {
    let layout = std::alloc::Layout::from_size_align(page_size * page_count, page_size)
        .expect("valid layout");
    let ptr = unsafe { std::alloc::alloc_zeroed(layout) };
    assert!(!ptr.is_null(), "heap_backing alloc failed");
    let owner = HeapOwner { ptr, layout };
    Backing {
        base: owner.ptr,
        page_size,
        page_count,
        keepalive: Arc::new(owner),
    }
}
