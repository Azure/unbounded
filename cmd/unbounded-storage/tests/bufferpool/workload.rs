// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Workload model, proptest strategies, and the `run_workload`
//! driver that ties the executor, mocks, and `Pool` together.

use std::cell::RefCell;
use std::collections::HashMap;
use std::rc::Rc;

use proptest::collection::vec;
use proptest::prelude::*;
use unbounded_storage::bufferpool::{
    BufferPool, Error, PageGuard, PipelinedRead, Pool, PoolConfig, ReadStream, StripeKey,
    StripePlan, WindowedRead,
};
use unbounded_storage::memory::Backing;

use crate::bufferpool::mocks::{
    CallCounts, DstBlockStore, DstTransport, MockSimConfig, Stripes, TestReq,
};
use crate::framework::executor::{Executor, RunError};

// ---------------------------------------------------------------------------
// Workload model.
// ---------------------------------------------------------------------------

/// Tunables for a single DST run. Sizes are kept small so proptest
/// can shrink quickly and so each case stays well under a second.
#[derive(Clone, Debug)]
pub struct Workload {
    pub page_size: usize,
    pub page_count: usize,
    /// Upper bound on per-I/O pend count. `0` means I/Os complete
    /// on first poll (still produces non-trivial interleaving via
    /// the executor's random task selection).
    pub max_io_delay: u32,
    /// Per-I/O fault probability in `[0, 100]`. `0` disables faults
    /// (the happy-path regime); positive values exercise the
    /// leader-error / `ParkOutcome::Error` paths in `pool.rs`.
    pub io_fault_rate: u32,
    /// Per-page probability in `[0, 100]` that
    /// `BlockStore::read_page` returns `Ok(true)` (cache hit). `0`
    /// is the miss-only regime where every fetch goes through the
    /// transport.
    pub cache_hit_rate: u32,
    /// `PoolConfig::max_concurrent_streams`. Defaults set this high
    /// enough to never trigger; the strategy occasionally chooses a
    /// small value so `Pool::read` returns `Error::StreamLimit` and
    /// the `invariant_stream_limit_bounds` property can verify the
    /// reject path doesn't leak state.
    pub max_concurrent_streams: usize,
    /// `PoolConfig::max_inflight_pages`: the global speculative
    /// prefetch budget shared by every `WindowedRead`. Bounds how
    /// many pages may be fetched strictly ahead of any stream's
    /// cursor at once. Only the windowed read path consumes it.
    pub max_inflight_pages: usize,
    /// Distinct stripes the workload may reference. Each client's
    /// `key_idx` indexes into this set modulo its length.
    pub key_count: u8,
    pub clients: Vec<ClientSpec>,
    /// Multi-stripe pipelined readers driven through
    /// `Pool::read_pipelined`. Each entry is an ordered plan of
    /// per-stripe slices delivered as one in-order page stream that
    /// pipelines fetches across stripe boundaries. Kept separate from
    /// `clients` so the single-stripe invariants that reason about
    /// `max_concurrent_streams` against `clients.len()` stay valid;
    /// the shared `workload_strategy` leaves this empty and a
    /// dedicated strategy/proptest exercises the pipelined path.
    pub pipelines: Vec<PipelineSpec>,
}

/// One slice of a pipelined read plan: a byte range within the stripe
/// identified by `key_idx`. `offset`/`len` are normalized against the
/// stripe length in `run_workload`, exactly like [`ClientSpec`].
#[derive(Clone, Debug)]
pub struct PlanSliceSpec {
    pub key_idx: u8,
    pub offset: u64,
    pub len: u64,
}

/// A single pipelined reader: an ordered list of plan slices plus an
/// optional mid-stream cancellation (same semantics as
/// [`ClientSpec::cancel_after`], counted in delivered pages across the
/// whole plan).
#[derive(Clone, Debug)]
pub struct PipelineSpec {
    pub cancel_after: Option<u32>,
    pub slices: Vec<PlanSliceSpec>,
}

#[derive(Clone, Debug)]
pub struct ClientSpec {
    pub key_idx: u8,
    /// Byte offset within the stripe.
    pub offset: u64,
    /// Length in bytes.
    pub len: u64,
    /// If `Some(k)`, the client drops its stream after
    /// successfully receiving `k` pages (a value of `0` drops it
    /// immediately after the stream is admitted). Models a consumer
    /// that abandons mid-iteration; exercises the drain paths in
    /// `decrement_stream`, `release_guard`, and the leader-error
    /// recycle.
    pub cancel_after: Option<u32>,
    /// Read path selector. `None` uses the plain one-page-at-a-time
    /// `BufferPool::read` stream; `Some(w)` uses the prefetching
    /// `Pool::read_windowed` reader with window depth `w` (clamped
    /// internally to `[1, max_inflight_pages + 1]`). Both deliver
    /// `PageGuard`s strictly in cursor order, so every byte/accounting
    /// invariant must hold identically across the two paths.
    pub window: Option<usize>,
}

impl Workload {
    fn stripe_len(&self) -> usize {
        self.page_size * self.page_count
    }

    fn key(&self, idx: u8) -> StripeKey {
        // Map an index to a 32-byte key by repeating the byte; this
        // is unique per `idx` (mod `key_count`) which is all the
        // pool cares about.
        let b = idx % self.key_count.max(1);
        StripeKey([b; 32])
    }

    /// Returns a deterministic byte pattern for the stripe identified
    /// by `key`. Used both to populate the transport and as the oracle.
    fn stripe_bytes(&self, key: StripeKey) -> Vec<u8> {
        let len = self.stripe_len();
        let seed = key.0[0];
        let mut out = vec![0u8; len];
        for (i, b) in out.iter_mut().enumerate() {
            *b = (i as u8).wrapping_add(seed);
        }
        out
    }
}

// ---------------------------------------------------------------------------
// Unified read surface over both stream types.
// ---------------------------------------------------------------------------

/// Erases the difference between the plain [`ReadStream`] and the
/// prefetching [`WindowedRead`] so the client driver can consume
/// either through one `next_page` loop. Both yield `PageGuard`s in
/// cursor order, one at a time, with identical observable bytes; the
/// only difference is internal prefetch depth.
enum AnyStream<'p> {
    Plain(ReadStream<'p>),
    Windowed(WindowedRead<'p>),
    Pipelined(PipelinedRead<'p>),
}

impl<'p> AnyStream<'p> {
    async fn next_page(&mut self) -> Option<Result<PageGuard<'_>, Error>> {
        match self {
            AnyStream::Plain(s) => s.next_page().await,
            AnyStream::Windowed(s) => s.next_page().await,
            AnyStream::Pipelined(s) => s.next_page().await,
        }
    }
}

/// Drive one [`AnyStream`] to completion (or to a mid-stream
/// cancellation after `cancel_after` delivered pages) and classify the
/// result. Shared by the single-stripe client tasks and the pipelined
/// reader tasks so both go through identical in-order consumption and
/// outcome accounting.
async fn consume<'p>(
    mut stream: AnyStream<'p>,
    expected: Vec<u8>,
    cancel_after: Option<u32>,
) -> ClientOutcome {
    // Cancel-before-first-page: drop the stream immediately. Exercises
    // the "dropped while the slot is Idle and no fetch was issued"
    // path in the readers' `Drop`.
    if let Some(0) = cancel_after {
        drop(stream);
        return ClientOutcome::Cancelled {
            got: Vec::new(),
            expected,
            pages_read: 0,
        };
    }
    let mut got = Vec::with_capacity(expected.len());
    let mut pages_read: u32 = 0;
    // Set inside the `Some(Ok(_))` arm when the cancel condition fires.
    // We can't `drop(stream)` inside the match arm because the
    // `PageGuard` returned by `next_page` borrows `stream` until the
    // arm ends.
    let mut should_cancel = false;
    loop {
        match stream.next_page().await {
            Some(Ok(g)) => {
                got.extend_from_slice(g.as_slice());
                drop(g);
                pages_read += 1;
                if cancel_after.map(|k| pages_read >= k).unwrap_or(false) {
                    should_cancel = true;
                }
            }
            Some(Err(e)) => {
                return ClientOutcome::FetchErr(format!("{e}"));
            }
            None => break,
        }
        if should_cancel {
            drop(stream);
            return ClientOutcome::Cancelled {
                got,
                expected,
                pages_read,
            };
        }
    }
    ClientOutcome::Ok { got, expected }
}

// ---------------------------------------------------------------------------
// Proptest strategy.
// ---------------------------------------------------------------------------

/// Defaults tuned for the basic invariant tests: a handful of pages,
/// 1-8 concurrent clients, short reads. Override as new invariant
/// suites grow.
pub fn workload_strategy() -> impl Strategy<Value = Workload> {
    // Powers of two in [64, 1024]. Small to keep test runtime down.
    let page_size = prop_oneof![Just(64usize), Just(128), Just(256), Just(512), Just(1024)];
    let page_count = 1usize..=8;
    let max_io_delay = 0u32..=3;
    // Most cases are fault-free; a meaningful minority injects
    // transport/blockstore errors to exercise the leader-error and
    // recycle-after-Error paths. Keep the upper bound modest so
    // happy-path coverage doesn't get drowned out.
    let io_fault_rate = prop_oneof![
        9 => Just(0u32),
        1 => 1u32..=30,
    ];
    let cache_hit_rate = prop_oneof![
        6 => Just(0u32),    // miss-only is the most interesting regime
        3 => 1u32..=80,     // mixed hits / misses
        1 => Just(100u32),  // all hits: tee never runs
    ];
    // Default to a limit well above the client count so most cases
    // exercise the happy path; a meaningful minority drops it low
    // enough that some `Pool::read` calls must be rejected with
    // `Error::StreamLimit`. Combined with `clients in 1..=8`, a
    // limit of 1..=4 reliably produces rejects without dominating
    // coverage.
    let max_concurrent_streams = prop_oneof![
        8 => Just(1024usize),
        2 => 1usize..=4,
    ];
    // Global prefetch budget for the windowed path. Kept modest so
    // speculation stays bounded relative to `page_count in 1..=8`,
    // which keeps free-list contention (and thus schedule coverage
    // of the head-of-line / no-deadlock paths) meaningful without
    // blowing the step budget.
    let max_inflight_pages = 1usize..=8;
    let key_count = 1u8..=3;
    let clients = vec(client_strategy(), 1..=8);

    (
        page_size,
        page_count,
        max_io_delay,
        io_fault_rate,
        cache_hit_rate,
        max_concurrent_streams,
        max_inflight_pages,
        key_count,
        clients,
    )
        .prop_map(
            |(
                page_size,
                page_count,
                max_io_delay,
                io_fault_rate,
                cache_hit_rate,
                max_concurrent_streams,
                max_inflight_pages,
                key_count,
                clients,
            )| Workload {
                page_size,
                page_count,
                max_io_delay,
                io_fault_rate,
                cache_hit_rate,
                max_concurrent_streams,
                max_inflight_pages,
                key_count,
                clients,
                // The shared strategy never generates pipelined
                // readers: the single-stripe invariants reason about
                // `max_concurrent_streams` against `clients.len()`, and
                // pipelined readers admit one stream per active slice
                // which would invalidate that accounting. The pipelined
                // path has its own strategy and proptest below.
                pipelines: Vec::new(),
            },
        )
}

fn client_strategy() -> impl Strategy<Value = ClientSpec> {
    // Offsets / lengths are normalized against the stripe length in
    // `run_workload` so we don't have to thread the workload size
    // through the strategy. Most clients read to EOF; a meaningful
    // minority cancel mid-iteration so the drain paths get covered
    // proportionally to the happy path.
    let cancel_after = prop_oneof![
        7 => Just(None),
        3 => (0u32..=16).prop_map(Some),
    ];
    // Roughly half the clients use the windowed reader so every
    // existing invariant exercises both read paths. Window depths
    // span 1 (degenerate, head-only) through 5 (deeper than most
    // short reads) to cover the speculative refill and budget-cap
    // branches in `WindowedRead`.
    let window = prop_oneof![
        5 => Just(None),
        5 => (1usize..=5).prop_map(Some),
    ];
    (0u8..=255u8, 0u64..=4096, 1u64..=4096, cancel_after, window).prop_map(
        |(key_idx, offset, len, cancel_after, window)| ClientSpec {
            key_idx,
            offset,
            len,
            cancel_after,
            window,
        },
    )
}

/// Strategy for pipelined-reader workloads. Distinct from
/// [`workload_strategy`]: it generates no single-stripe `clients` and
/// instead populates `pipelines`, and pins `max_concurrent_streams`
/// high so the lazy per-slice admission never trips `StreamLimit`
/// (which would surface mid-plan as a `FetchErr`). This keeps the
/// pipelined invariants focused on ordering, accounting, single-flight,
/// and deadlock-freedom rather than admission backpressure.
pub fn pipelined_workload_strategy() -> impl Strategy<Value = Workload> {
    let page_size = prop_oneof![Just(64usize), Just(128), Just(256), Just(512)];
    let page_count = 1usize..=8;
    let max_io_delay = 0u32..=3;
    let io_fault_rate = prop_oneof![
        9 => Just(0u32),
        1 => 1u32..=30,
    ];
    let cache_hit_rate = prop_oneof![
        6 => Just(0u32),
        3 => 1u32..=80,
        1 => Just(100u32),
    ];
    let max_inflight_pages = 1usize..=8;
    let key_count = 1u8..=3;
    let pipelines = vec(pipeline_strategy(), 1..=4);

    (
        page_size,
        page_count,
        max_io_delay,
        io_fault_rate,
        cache_hit_rate,
        max_inflight_pages,
        key_count,
        pipelines,
    )
        .prop_map(
            |(
                page_size,
                page_count,
                max_io_delay,
                io_fault_rate,
                cache_hit_rate,
                max_inflight_pages,
                key_count,
                pipelines,
            )| Workload {
                page_size,
                page_count,
                max_io_delay,
                io_fault_rate,
                cache_hit_rate,
                // Pinned high: pipelined admission must not be
                // rejected, so every plan slice gets a stream.
                max_concurrent_streams: 1024,
                max_inflight_pages,
                key_count,
                clients: Vec::new(),
                pipelines,
            },
        )
}

fn plan_slice_strategy() -> impl Strategy<Value = PlanSliceSpec> {
    (0u8..=255u8, 0u64..=4096, 1u64..=4096)
        .prop_map(|(key_idx, offset, len)| PlanSliceSpec { key_idx, offset, len })
}

fn pipeline_strategy() -> impl Strategy<Value = PipelineSpec> {
    // Most plans run to completion; a meaningful minority cancel
    // mid-stream so the pipelined `Drop` cleanup path is covered. A
    // plan spans 1..=4 stripe slices so prefetch crosses stripe
    // boundaries.
    let cancel_after = prop_oneof![
        7 => Just(None),
        3 => (0u32..=16).prop_map(Some),
    ];
    (cancel_after, vec(plan_slice_strategy(), 1..=4))
        .prop_map(|(cancel_after, slices)| PipelineSpec { cancel_after, slices })
}

// ---------------------------------------------------------------------------
// Run a single workload.
// ---------------------------------------------------------------------------

/// Outcome of a single client task.
#[derive(Clone, Debug)]
#[allow(dead_code)]
pub enum ClientOutcome {
    /// Stream completed; `got` is the concatenation of all
    /// `PageGuard::as_slice()` outputs, `expected` is the oracle.
    Ok { got: Vec<u8>, expected: Vec<u8> },
    /// Client dropped its `ReadStream` after `pages_read` successful
    /// `next_page` calls. `got` is the concatenation of those page
    /// slices; `expected` is the full oracle slice (the assertion
    /// checks the prefix `expected[..got.len()]`).
    Cancelled {
        got: Vec<u8>,
        expected: Vec<u8>,
        pages_read: u32,
    },
    /// `Pool::read` returned an error (e.g. StreamLimit). Currently
    /// not produced by the default config but kept so future
    /// workloads can exercise it.
    ReadErr(String),
    /// A `next_page` call returned `Err`. Surfaces injected faults.
    FetchErr(String),
}

/// What the harness reports back to the property tests.
#[derive(Debug)]
#[allow(dead_code)]
pub struct RunReport {
    pub outcomes: Vec<ClientOutcome>,
    pub free_pages_at_end: usize,
    pub inflight_entries_at_end: usize,
    /// Global speculative-prefetch budget still reserved at
    /// quiescence. Must be `0`: every windowed prefetch reservation
    /// is balanced on consume or drop.
    pub prefetch_inflight_at_end: usize,
    pub bulk_get_calls: u32,
    pub bulk_get_by_page: HashMap<(StripeKey, u64), u32>,
    /// Max observed concurrent `bulk_get`s per `(key, page_no)`.
    /// Sequential re-issues (slot recycled, then refetched) only
    /// bump the *count*, not the max-inflight; this is the value
    /// the single-flight invariant asserts against.
    pub bulk_get_max_inflight: HashMap<(StripeKey, u64), u32>,
    pub steps: u64,
}

/// Drive `w` under seed `seed`. Returns the report so callers can
/// assert invariants; the helper itself never panics on invariant
/// violations (so proptest can shrink). It does panic on framework
/// setup errors (e.g. allocation failure) because those aren't
/// "test failures" the caller should shrink against.
pub fn run_workload(seed: u64, w: Workload) -> Result<RunReport, RunError> {
    // Normalize clients so every (offset, len) is in-bounds for the
    // configured stripe size, and len >= 1. We do this here rather
    // than in the proptest strategy so the workload struct itself
    // remains independent of `page_size * page_count`.
    let stripe_len = w.stripe_len() as u64;
    let normalized: Vec<ClientSpec> = w
        .clients
        .iter()
        .map(|c| {
            let offset = c.offset % stripe_len;
            let max_len = stripe_len - offset;
            let len = ((c.len - 1) % max_len) + 1;
            ClientSpec {
                key_idx: c.key_idx,
                offset,
                len,
                cancel_after: c.cancel_after,
                window: c.window,
            }
        })
        .collect();

    // Backing.
    let backing = heap_backing(w.page_size, w.page_count);

    // Oracle + transport stripe map (same bytes).
    let stripes: Stripes = Rc::new(RefCell::new(HashMap::new()));
    let mut oracle: HashMap<StripeKey, Vec<u8>> = HashMap::new();
    for idx in 0..w.key_count.max(1) {
        let k = w.key(idx);
        let bytes = w.stripe_bytes(k);
        oracle.insert(k, bytes.clone());
        stripes.borrow_mut().insert(k, bytes);
    }

    let counts = Rc::new(CallCounts::default());
    let mock_cfg = MockSimConfig::new();
    mock_cfg.max_io_delay.set(w.max_io_delay);
    mock_cfg.io_fault_rate.set(w.io_fault_rate);
    let transport = DstTransport::new(
        stripes.clone(),
        counts.clone(),
        mock_cfg.clone(),
        backing.base,
        backing.page_size,
    );
    let blockstore = DstBlockStore::new(counts.clone(), stripes.clone(), mock_cfg.clone());
    blockstore.set_hit_rate(w.cache_hit_rate);

    let pool = Rc::new(
        Pool::new(
            PoolConfig {
                max_concurrent_streams: w.max_concurrent_streams,
                max_inflight_pages: w.max_inflight_pages,
            },
            backing,
            transport,
            blockstore,
        )
        .expect("Pool::new must succeed for a well-formed workload"),
    );

    // Set up executor and spawn one task per client.
    let mut exec = Executor::new(seed);

    let outcomes: Rc<RefCell<Vec<ClientOutcome>>> = Rc::new(RefCell::new(Vec::new()));

    for (cid, c) in normalized.iter().enumerate() {
        let p = pool.clone();
        let outcomes = outcomes.clone();
        let key = w.key(c.key_idx);
        let expected = oracle
            .get(&key)
            .map(|s| s[c.offset as usize..(c.offset + c.len) as usize].to_vec())
            .expect("oracle stripe must exist");
        let offset = c.offset;
        let len = c.len;
        let cancel_after = c.cancel_after;
        let window = c.window;

        exec.spawn(async move {
            let _ = cid;
            let req = TestReq { key };
            // Window `None` => plain `read`; `Some(w)` => prefetching
            // `read_windowed`. Both admit through the same path and
            // surface `StreamLimit` identically.
            let stream = match window {
                None => match p.read(&req, offset, len).await {
                    Ok(s) => AnyStream::Plain(s),
                    Err(e) => {
                        outcomes
                            .borrow_mut()
                            .push(ClientOutcome::ReadErr(format!("{e}")));
                        return;
                    }
                },
                Some(w) => match p.read_windowed(&req, offset, len, w) {
                    Ok(s) => AnyStream::Windowed(s),
                    Err(e) => {
                        outcomes
                            .borrow_mut()
                            .push(ClientOutcome::ReadErr(format!("{e}")));
                        return;
                    }
                },
            };
            let outcome = consume(stream, expected, cancel_after).await;
            outcomes.borrow_mut().push(outcome);
        });
    }

    // Spawn one task per pipelined reader. Each plan slice is
    // normalized against the stripe length exactly like a single-stripe
    // client; the per-client oracle is the in-order concatenation of
    // every slice's bytes.
    for pspec in &w.pipelines {
        let mut plan_norm: Vec<(StripeKey, u64, u64)> = Vec::new();
        let mut expected: Vec<u8> = Vec::new();
        for s in &pspec.slices {
            let key = w.key(s.key_idx);
            let offset = s.offset % stripe_len;
            let max_len = stripe_len - offset;
            let len = ((s.len - 1) % max_len) + 1;
            let bytes = oracle.get(&key).expect("oracle stripe must exist");
            expected.extend_from_slice(&bytes[offset as usize..(offset + len) as usize]);
            plan_norm.push((key, offset, len));
        }
        if plan_norm.is_empty() {
            continue;
        }
        let p = pool.clone();
        let outcomes = outcomes.clone();
        let cancel_after = pspec.cancel_after;

        exec.spawn(async move {
            let plans: Vec<StripePlan<TestReq>> = plan_norm
                .into_iter()
                .map(|(key, intra_offset, intra_len)| StripePlan {
                    req: TestReq { key },
                    intra_offset,
                    intra_len,
                })
                .collect();
            // `usize::MAX` window: the pool clamps it to
            // `max_inflight_pages + 1`, so the pipeline prefetches as
            // aggressively as the global budget allows.
            let stream = match p.read_pipelined(plans, usize::MAX) {
                Ok(s) => AnyStream::Pipelined(s),
                Err(e) => {
                    outcomes
                        .borrow_mut()
                        .push(ClientOutcome::ReadErr(format!("{e}")));
                    return;
                }
            };
            let outcome = consume(stream, expected, cancel_after).await;
            outcomes.borrow_mut().push(outcome);
        });
    }

    // Step budget: generous upper bound. Per page we expect O(delay)
    // pends; across all clients * pages we add a healthy slack. The
    // fault-rate term covers leader re-leads after a transport error
    // recycles the slot and a new caller leads again.
    let max_pages_per_client = (stripe_len / w.page_size as u64) + 1;
    let fault_slack = 1 + w.io_fault_rate as u64 / 5;
    let per_page_cost = (w.max_io_delay as u64 + 4) * 16 * fault_slack;
    // A pipelined reader may touch up to `slices * pages_per_stripe`
    // pages, so budget each by its (normalized) plan length.
    let pipeline_pages: u64 = w
        .pipelines
        .iter()
        .map(|p| p.slices.len() as u64 * max_pages_per_client)
        .sum();
    let step_budget = 64
        + (normalized.len() as u64) * max_pages_per_client * per_page_cost
        + pipeline_pages * per_page_cost;

    exec.run(step_budget)?;

    let free_pages_at_end = pool.free_pages();
    let inflight_entries_at_end = pool.inflight_entries();
    let prefetch_inflight_at_end = pool.prefetch_inflight();

    // Drain the outcomes vec; by this point all tasks have completed
    // so no other Rc references exist.
    let outcomes = Rc::try_unwrap(outcomes)
        .map_err(|_| RunError::Deadlock)
        .expect("all tasks completed; outcomes Rc must be unique")
        .into_inner();

    let bulk_get_by_page = counts.bulk_get_by_page.borrow().clone();
    let bulk_get_max_inflight = counts.bulk_get_max_inflight.borrow().clone();
    Ok(RunReport {
        outcomes,
        free_pages_at_end,
        inflight_entries_at_end,
        prefetch_inflight_at_end,
        bulk_get_calls: counts.bulk_get.get(),
        bulk_get_by_page,
        bulk_get_max_inflight,
        steps: exec.last_steps(),
    })
}

// ---------------------------------------------------------------------------
// Heap-backed `Backing` for tests.
// ---------------------------------------------------------------------------

struct HeapOwner {
    ptr: *mut u8,
    layout: std::alloc::Layout,
}

// SAFETY: tests are single-threaded; the allocation lives for the
// owner's lifetime and is only handed to the pool inside the same
// thread.
unsafe impl Send for HeapOwner {}
unsafe impl Sync for HeapOwner {}

impl Drop for HeapOwner {
    fn drop(&mut self) {
        // SAFETY: `layout` matches the original `alloc_zeroed`.
        unsafe {
            std::alloc::dealloc(self.ptr, self.layout);
        }
    }
}

fn heap_backing(page_size: usize, page_count: usize) -> Backing {
    let layout = std::alloc::Layout::from_size_align(page_size * page_count, page_size)
        .expect("valid layout");
    // SAFETY: layout has nonzero size and a power-of-two align.
    let ptr = unsafe { std::alloc::alloc_zeroed(layout) };
    assert!(!ptr.is_null(), "heap_backing alloc failed");
    let owner = HeapOwner { ptr, layout };
    Backing {
        base: owner.ptr,
        page_size,
        page_count,
        _own: Box::new(owner),
    }
}
