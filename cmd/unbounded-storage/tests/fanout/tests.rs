// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

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
use unbounded_storage::fanout::{FetchChannel, FetchEvent, FetchService};
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
/// drained, all pool pages must be free or idle-cached with no active
/// inflight entries. Bufferpool DST owns the lower-level stripe-fetch
/// state transitions; fanout only checks the owner pin lifetime and
/// release contract.
fn assert_no_pin_leak(report: &RunReport) -> Result<(), TestCaseError> {
    prop_assert_eq!(
        report.free_pages_at_end + report.cached_pages_at_end,
        report.total_pool_pages,
        "pages leaked: free={} cached={} expected {}",
        report.free_pages_at_end,
        report.cached_pages_at_end,
        report.total_pool_pages,
    );
    prop_assert_eq!(
        report.active_inflight_entries_at_end,
        0,
        "active inflight entries leaked: {}",
        report.active_inflight_entries_at_end,
    );
    Ok(())
}

/// Invariant: fetch errors only occur under fault injection.
///
/// With `io_fault_rate == 0` the owner read path never surfaces an error
/// to the coordinator, so every fetch must resolve `Ok`. This now also
/// guards the page-pressure path: under `tight_pool` the owner handles
/// transient `Error::Busy` internally, so `Busy` must never leak out as a
/// `FetchOutcome::Err`. A happy-path run producing a `FetchOutcome::Err`
/// is itself a bug.
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
            PendingTransport::new(base, page_size, payload.clone()),
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
    let mut stream = channel.fetch(req, 0, page_size as u64).expect("stream");
    let noop = unbounded_storage::runtime::noop_waker();
    let mut cx = Context::from_waker(&noop);
    let mut event = Box::pin(stream.next_event());

    assert!(event.as_mut().poll(&mut cx).is_pending());
    let wakes_after_fetch = wakes.load(Ordering::SeqCst);

    assert!(service.progress(), "admitting the queued fetch is progress");
    assert_eq!(
        wakes.load(Ordering::SeqCst),
        wakes_after_fetch + 1,
        "first in-flight poll must wake the configured shard waker",
    );

    assert!(
        !service.progress(),
        "a still-pending fetch without command admission or completion must not report busy",
    );
    assert_eq!(
        wakes.load(Ordering::SeqCst),
        wakes_after_fetch + 2,
        "subsequent pending poll must still use the configured shard waker",
    );

    assert!(service.progress(), "the final fetch completion is progress");
    let page = match event.as_mut().poll(&mut cx) {
        Poll::Ready(Ok(unbounded_storage::fanout::FetchEvent::Page(page))) => page,
        other => panic!("fetch did not resolve after completion: {other:?}"),
    };
    assert_eq!(page.loc.len, page_size as u32);

    drop(event);

    let wakes_before_release = wakes.load(Ordering::SeqCst);
    stream.release(page.pin_token);
    assert_eq!(
        wakes.load(Ordering::SeqCst),
        wakes_before_release + 1,
        "pin release must wake the owner service promptly",
    );
    assert!(service.progress(), "release command is progress");
    assert_eq!(pool.free_pages() + pool.cached_pages(), page_count);
    assert_eq!(pool.active_inflight_entries(), 0);
}

#[test]
fn cached_head_page_is_emitted_before_later_miss_completes() {
    let page_size = 64;
    let page_count = 2;
    let backing = heap_backing(page_size, page_count);
    let base = backing.base;
    let payload: Vec<u8> = (0..page_size * page_count).map(|i| i as u8).collect();
    let pool = Rc::new(
        Pool::new(
            PoolConfig {
                max_concurrent_streams: 4,
                max_inflight_pages: 1,
            },
            backing,
            PendingTransport::new(base, page_size, payload),
            MissBlockStore,
        )
        .expect("pool"),
    );
    let (channel, rx) = FetchChannel::new();
    let waker = unbounded_storage::runtime::noop_waker();
    let mut service = FetchService::new(pool.clone(), rx, page_size, waker.clone());
    let mut cx = Context::from_waker(&waker);
    let req = StripeReq::new(StripeKey([8u8; 32]));

    let mut warm = channel
        .fetch(req.clone(), 0, page_size as u64)
        .expect("warm stream");
    let warm_page = poll_page_event(&mut service, &mut warm, &mut cx);
    warm.release(warm_page.pin_token);
    assert!(service.progress(), "release command is progress");
    drain_stream_done(&mut service, &mut warm, &mut cx);
    drop(warm);
    assert_eq!(pool.cached_pages(), 1, "warm page should be cached");

    let mut stream = channel
        .fetch(req, 0, (page_size * page_count) as u64)
        .expect("fetch stream");
    let mut first = Box::pin(stream.next_event());
    assert!(first.as_mut().poll(&mut cx).is_pending());
    assert!(service.progress(), "admission should emit cached head page");
    let page = match first.as_mut().poll(&mut cx) {
        Poll::Ready(Ok(FetchEvent::Page(page))) => page,
        other => panic!("cached head page was not emitted immediately: {other:?}"),
    };
    assert_eq!(page.ordinal, 0);
    let pin_token = page.pin_token;
    drop(first);
    stream.release(pin_token);

    let mut second = Box::pin(stream.next_event());
    assert!(second.as_mut().poll(&mut cx).is_pending());
    assert!(service.progress(), "release command is progress");
    assert!(second.as_mut().poll(&mut cx).is_pending());
    drop(second);

    let second_page = poll_page_event(&mut service, &mut stream, &mut cx);
    assert_eq!(second_page.ordinal, 1);
    stream.release(second_page.pin_token);
    assert!(service.progress(), "release command is progress");
    drain_stream_done(&mut service, &mut stream, &mut cx);
    assert_eq!(pool.free_pages() + pool.cached_pages(), page_count);
    assert_eq!(pool.active_inflight_entries(), 0);
}

#[test]
fn cached_later_page_is_emitted_before_head_miss_completes() {
    let page_size = 64;
    let page_count = 3;
    let backing = heap_backing(page_size, page_count);
    let base = backing.base;
    let payload: Vec<u8> = (0..page_size * 2).map(|i| i as u8).collect();
    let pool = Rc::new(
        Pool::new(
            PoolConfig {
                max_concurrent_streams: 4,
                max_inflight_pages: 1,
            },
            backing,
            PendingTransport::new(base, page_size, payload),
            MissBlockStore,
        )
        .expect("pool"),
    );
    let (channel, rx) = FetchChannel::new();
    let waker = unbounded_storage::runtime::noop_waker();
    let mut service = FetchService::new(pool.clone(), rx, page_size, waker.clone());
    let mut cx = Context::from_waker(&waker);
    let req = StripeReq::new(StripeKey([0x81; 32]));

    let mut warm = channel
        .fetch(req.clone(), page_size as u64, page_size as u64)
        .expect("warm stream");
    let warm_page = poll_page_event(&mut service, &mut warm, &mut cx);
    warm.release(warm_page.pin_token);
    assert!(service.progress(), "release command is progress");
    drain_stream_done(&mut service, &mut warm, &mut cx);
    drop(warm);
    assert_eq!(pool.cached_pages(), 1, "later page should be cached");

    let mut stream = channel
        .fetch(req, 0, (page_size * 2) as u64)
        .expect("fetch stream");
    let mut first = Box::pin(stream.next_event());
    assert!(first.as_mut().poll(&mut cx).is_pending());
    assert!(service.progress(), "admission should poll the fetch");
    let page = match first.as_mut().poll(&mut cx) {
        Poll::Ready(Ok(FetchEvent::Page(page))) => page,
        other => panic!("cached later page was not emitted behind pending head: {other:?}"),
    };
    assert_eq!(page.ordinal, 1);
    let pin_token = page.pin_token;
    drop(first);
    stream.release(pin_token);

    let page0 = poll_page_event(&mut service, &mut stream, &mut cx);
    assert_eq!(page0.ordinal, 0);
    stream.release(page0.pin_token);
    assert!(service.progress(), "release commands are progress");
    drain_stream_done(&mut service, &mut stream, &mut cx);
    assert_eq!(pool.free_pages() + pool.cached_pages(), page_count);
    assert_eq!(pool.active_inflight_entries(), 0);
}

#[test]
fn newly_ready_later_page_is_emitted_while_head_is_pending() {
    let page_size = 64;
    let page_count = 4;
    let backing = heap_backing(page_size, page_count);
    let base = backing.base;
    let payload: Vec<u8> = (0..page_size * 2).map(|i| i as u8).collect();
    let transport = PendingTransport::new(base, page_size, payload)
        .with_pending_polls(0, 8)
        .with_pending_polls(page_size, 0);
    let pool = Rc::new(
        Pool::new(
            PoolConfig {
                max_concurrent_streams: 4,
                max_inflight_pages: 1,
            },
            backing,
            transport,
            MissBlockStore,
        )
        .expect("pool"),
    );
    let (channel, rx) = FetchChannel::new();
    let waker = unbounded_storage::runtime::noop_waker();
    let mut service = FetchService::new(pool.clone(), rx, page_size, waker.clone());
    let mut cx = Context::from_waker(&waker);
    let req = StripeReq::new(StripeKey([0x82; 32]));

    let mut waiting = channel
        .fetch(req.clone(), 0, (page_size * 2) as u64)
        .expect("waiting stream");
    let mut waiting_event = Box::pin(waiting.next_event());
    assert!(waiting_event.as_mut().poll(&mut cx).is_pending());
    assert!(service.progress(), "admission should leave head pending");
    assert!(waiting_event.as_mut().poll(&mut cx).is_pending());
    drop(waiting_event);

    let mut producer = channel
        .fetch(req, page_size as u64, page_size as u64)
        .expect("producer stream");
    let produced = poll_page_event(&mut service, &mut producer, &mut cx);
    producer.release(produced.pin_token);
    assert!(service.progress(), "producer release is progress");
    drain_stream_done(&mut service, &mut producer, &mut cx);

    let mut next = Box::pin(waiting.next_event());
    let page = match next.as_mut().poll(&mut cx) {
        Poll::Ready(Ok(FetchEvent::Page(page))) => page,
        Poll::Pending => {
            assert!(
                service.progress(),
                "ready later page should wake waiting fetch"
            );
            match next.as_mut().poll(&mut cx) {
                Poll::Ready(Ok(FetchEvent::Page(page))) => page,
                other => {
                    panic!("newly ready later page was not emitted behind pending head: {other:?}")
                }
            }
        }
        other => panic!("newly ready later page was not emitted behind pending head: {other:?}"),
    };
    assert_eq!(page.ordinal, 1);
    let pin_token = page.pin_token;
    drop(next);
    waiting.release(pin_token);

    let page0 = poll_page_event(&mut service, &mut waiting, &mut cx);
    assert_eq!(page0.ordinal, 0);
    waiting.release(page0.pin_token);
    assert!(service.progress(), "waiting releases are progress");
    drain_stream_done(&mut service, &mut waiting, &mut cx);
    assert_eq!(pool.free_pages() + pool.cached_pages(), page_count);
    assert_eq!(pool.active_inflight_entries(), 0);
}

#[test]
fn later_page_error_keeps_already_emitted_pin_until_release() {
    let page_size = 64;
    let page_count = 2;
    let backing = heap_backing(page_size, page_count);
    let base = backing.base;
    let payload: Vec<u8> = (0..page_size * 2).map(|i| i as u8).collect();
    let transport = PendingTransport::new(base, page_size, payload)
        .with_pending_polls(0, 0)
        .with_pending_polls(page_size, 0)
        .with_error_offset(page_size);
    let pool = Rc::new(
        Pool::new(
            PoolConfig {
                max_concurrent_streams: 4,
                max_inflight_pages: 2,
            },
            backing,
            transport,
            MissBlockStore,
        )
        .expect("pool"),
    );
    let (channel, rx) = FetchChannel::new();
    let waker = unbounded_storage::runtime::noop_waker();
    let mut service = FetchService::new(pool.clone(), rx, page_size, waker.clone());
    let mut cx = Context::from_waker(&waker);
    let req = StripeReq::new(StripeKey([0x83; 32]));

    let mut stream = channel
        .fetch(req, 0, (page_size * 2) as u64)
        .expect("fetch stream");
    let page0 = poll_page_event(&mut service, &mut stream, &mut cx);
    assert_eq!(page0.ordinal, 0);
    assert_eq!(
        pool.free_pages() + pool.cached_pages(),
        page_count - 1,
        "emitted page is pinned before the later error",
    );

    let mut event = Box::pin(stream.next_event());
    let mut got_error = false;
    for _ in 0..8 {
        match event.as_mut().poll(&mut cx) {
            Poll::Ready(Err(_)) => {
                got_error = true;
                break;
            }
            Poll::Ready(Ok(other)) => panic!("expected error after first page, got {other:?}"),
            Poll::Pending => {
                service.progress();
            }
        }
    }
    assert!(got_error, "expected later page error");
    assert_eq!(
        pool.free_pages() + pool.cached_pages(),
        page_count - 1,
        "later error must not release an already emitted page pin",
    );
    drop(event);

    stream.release(page0.pin_token);
    assert!(service.progress(), "explicit release is progress");
    assert_eq!(pool.free_pages() + pool.cached_pages(), page_count);
    assert_eq!(pool.active_inflight_entries(), 0);
}

#[test]
fn cancel_releases_emitted_pin_without_explicit_release() {
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
            PendingTransport::new(base, page_size, payload).with_pending_polls(0, 0),
            MissBlockStore,
        )
        .expect("pool"),
    );
    let (channel, rx) = FetchChannel::new();
    let waker = unbounded_storage::runtime::noop_waker();
    let mut service = FetchService::new(pool.clone(), rx, page_size, waker.clone());
    let mut cx = Context::from_waker(&waker);
    let req = StripeReq::new(StripeKey([0x84; 32]));

    let mut stream = channel
        .fetch(req, 0, page_size as u64)
        .expect("fetch stream");
    let page = poll_page_event(&mut service, &mut stream, &mut cx);
    drop(stream);
    assert!(service.progress(), "drop should enqueue cancel");
    assert_eq!(
        pool.free_pages() + pool.cached_pages(),
        page_count,
        "cancel must release emitted pages that were never sent",
    );
    assert_eq!(pool.active_inflight_entries(), 0);

    channel.release(page.pin_token);
    assert!(service.progress(), "late explicit release is a no-op");
    assert_eq!(pool.free_pages() + pool.cached_pages(), page_count);
}

#[test]
fn cancel_retains_pin_marked_sending_until_release() {
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
            PendingTransport::new(base, page_size, payload).with_pending_polls(0, 0),
            MissBlockStore,
        )
        .expect("pool"),
    );
    let (channel, rx) = FetchChannel::new();
    let waker = unbounded_storage::runtime::noop_waker();
    let mut service = FetchService::new(pool.clone(), rx, page_size, waker.clone());
    let mut cx = Context::from_waker(&waker);
    let req = StripeReq::new(StripeKey([0x86; 32]));

    let mut stream = channel
        .fetch(req, 0, page_size as u64)
        .expect("fetch stream");
    let page = poll_page_event(&mut service, &mut stream, &mut cx);
    stream.sending(page.pin_token);
    assert!(service.progress(), "sending command is progress");

    drop(stream);
    assert!(service.progress(), "drop should enqueue cancel");
    assert_eq!(
        pool.free_pages() + pool.cached_pages(),
        0,
        "cancel must not release a pin already handed to SEND_ZC",
    );

    channel.release(page.pin_token);
    assert!(service.progress(), "completion release is progress");
    assert_eq!(pool.free_pages() + pool.cached_pages(), page_count);
    assert_eq!(pool.active_inflight_entries(), 0);
}

#[test]
fn transient_busy_waits_for_pin_release_instead_of_erroring() {
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
            PendingTransport::new(base, page_size, payload).with_pending_polls(0, 0),
            MissBlockStore,
        )
        .expect("pool"),
    );
    let (channel, rx) = FetchChannel::new();
    let waker = unbounded_storage::runtime::noop_waker();
    let mut service = FetchService::new(pool.clone(), rx, page_size, waker.clone());
    let mut cx = Context::from_waker(&waker);

    let mut pinned = channel
        .fetch(StripeReq::new(StripeKey([0x85; 32])), 0, page_size as u64)
        .expect("pinned stream");
    let pinned_page = poll_page_event(&mut service, &mut pinned, &mut cx);
    assert_eq!(pool.free_pages() + pool.cached_pages(), 0);

    let mut waiting = channel
        .fetch(StripeReq::new(StripeKey([0x86; 32])), 0, page_size as u64)
        .expect("waiting stream");
    let mut waiting_event = Box::pin(waiting.next_event());
    for _ in 0..8 {
        match waiting_event.as_mut().poll(&mut cx) {
            Poll::Ready(Ok(event)) => panic!("busy fetch unexpectedly emitted {event:?}"),
            Poll::Ready(Err(e)) => panic!("transient Busy leaked to coordinator: {e:?}"),
            Poll::Pending => {
                service.progress();
            }
        }
    }
    drop(waiting_event);

    pinned.release(pinned_page.pin_token);
    assert!(service.progress(), "pin release is progress");

    let waiting_page = poll_page_event(&mut service, &mut waiting, &mut cx);
    assert_eq!(waiting_page.ordinal, 0);
    waiting.release(waiting_page.pin_token);
    assert!(service.progress(), "waiting release is progress");
    drain_stream_done(&mut service, &mut waiting, &mut cx);
    assert_eq!(pool.free_pages() + pool.cached_pages(), page_count);
    assert_eq!(pool.active_inflight_entries(), 0);
}

fn poll_page_event(
    service: &mut FetchService<PendingTransport, MissBlockStore>,
    stream: &mut unbounded_storage::fanout::FetchStream,
    cx: &mut Context<'_>,
) -> unbounded_storage::fanout::FetchPage {
    let mut event = Box::pin(stream.next_event());
    for _ in 0..16 {
        if let Poll::Ready(Ok(FetchEvent::Page(page))) = event.as_mut().poll(cx) {
            return page;
        }
        service.progress();
    }
    panic!("page event did not arrive");
}

fn drain_stream_done(
    service: &mut FetchService<PendingTransport, MissBlockStore>,
    stream: &mut unbounded_storage::fanout::FetchStream,
    cx: &mut Context<'_>,
) {
    let mut event = Box::pin(stream.next_event());
    for _ in 0..16 {
        if matches!(event.as_mut().poll(cx), Poll::Ready(Ok(FetchEvent::Done))) {
            return;
        }
        service.progress();
    }
    panic!("done event did not arrive");
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
    page_size: usize,
    payload: Vec<u8>,
    default_pending_polls: u8,
    pending_polls: Vec<(usize, u8)>,
    error_offsets: Vec<usize>,
}

impl PendingTransport {
    fn new(base: *mut u8, page_size: usize, payload: Vec<u8>) -> Self {
        Self {
            base,
            page_size,
            payload,
            default_pending_polls: 2,
            pending_polls: Vec::new(),
            error_offsets: Vec::new(),
        }
    }

    fn with_pending_polls(mut self, offset: usize, polls: u8) -> Self {
        self.pending_polls.push((offset, polls));
        self
    }

    fn with_error_offset(mut self, offset: usize) -> Self {
        self.error_offsets.push(offset);
        self
    }

    fn pending_polls_for(&self, offset: usize) -> u8 {
        self.pending_polls
            .iter()
            .find_map(|(off, polls)| (*off == offset).then_some(*polls))
            .unwrap_or(self.default_pending_polls)
    }
}

impl Transport<StripeReq> for PendingTransport {
    type Stream<'a> = PendingStream<'a>;

    fn bulk_get<'a>(
        &'a self,
        _req: &'a StripeReq,
        src: BulkRef,
        dsts: &'a [PageRef],
    ) -> Self::Stream<'a> {
        assert_eq!(dsts.len(), 1);
        let src_offset = src.offset as usize;
        let len = src.len as usize;
        assert!(src_offset + len <= self.payload.len());
        PendingStream {
            base: self.base,
            page_size: self.page_size,
            payload: &self.payload,
            src_offset,
            dst: dsts[0],
            pending_left: self.pending_polls_for(src_offset),
            error: self.error_offsets.contains(&src_offset),
            delivered: false,
        }
    }
}

struct PendingStream<'a> {
    base: *mut u8,
    page_size: usize,
    payload: &'a [u8],
    src_offset: usize,
    dst: PageRef,
    pending_left: u8,
    error: bool,
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

        if self.error {
            self.delivered = true;
            return Poll::Ready(Some(Err(Error::from("forced fanout transport error"))));
        }

        let len = self.dst.len as usize;
        assert!(self.src_offset + len <= self.payload.len());
        unsafe {
            let dst = self
                .base
                .add(self.dst.page_idx as usize * self.page_size + self.dst.offset as usize);
            std::ptr::copy_nonoverlapping(self.payload.as_ptr().add(self.src_offset), dst, len);
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
