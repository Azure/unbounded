// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Workload model, proptest strategy, and the `run_workload` driver
//! for the cross-shard fan-out DST.
//!
//! The driver models a single owner shard: one real [`Pool`] over a
//! heap backing, driven by an owner-side [`FetchService`] that holds
//! pages pinned across the cross-shard round-trip. Coordinator clients
//! issue [`FetchChannel::fetch`] requests; on each reply they read the
//! owner's backing memory at the returned [`PageLoc`] offsets (the DST
//! stand-in for the coordinator's `SEND_ZC` source read, which cannot
//! run under the simulator), verify the bytes against the oracle, then
//! [`FetchChannel::release`] the pin. An optional `hold` delay between
//! reply and read widens the window during which the owner must keep
//! the pages pinned, stressing pin lifetime against concurrent fetches.
//!
//! Tasks spawned on the seeded executor:
//!   - one SERVICE task driving [`FetchService::poll_once`] until the
//!     stop flag is set and no fetch is in flight, then `mark_dead`;
//!   - one SUPERVISOR task that flips the stop flag once every client
//!     has finished (every `release` is already enqueued by then, so
//!     the service drains them in its final `poll_once`);
//!   - one COORDINATOR client task per [`ClientSpec`].
//!
//! [`Pool`]: unbounded_storage::bufferpool::Pool
//! [`FetchService`]: unbounded_storage::fanout::FetchService
//! [`FetchChannel::fetch`]: unbounded_storage::fanout::FetchChannel::fetch
//! [`FetchChannel::release`]: unbounded_storage::fanout::FetchChannel::release
//! [`PageLoc`]: unbounded_storage::fanout::PageLoc

use std::cell::{Cell, RefCell};
use std::collections::HashMap;
use std::future::{Future, poll_fn};
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll, RawWaker, RawWakerVTable, Waker};

use proptest::collection::vec;
use proptest::prelude::*;
use unbounded_storage::bufferpool::{Error as PoolError, Pool, PoolConfig, StripeKey};
use unbounded_storage::fanout::{FetchChannel, FetchService};
use unbounded_storage::memory::Backing;
use unbounded_storage::storage::StripeReq;

use crate::fanout::mocks::{CallCounts, FanoutBlockStore, FanoutTransport, MockSimConfig, Stripes};
use crate::framework::executor::{Executor, RunError, yield_n, yield_once};

// ---------------------------------------------------------------------------
// Workload model.
// ---------------------------------------------------------------------------

/// Tunables for a single DST run. Stripe geometry (`stripe_pages` *
/// `page_size`) is deliberately separate from the pool's page count:
/// the pool is sized in `run_workload` to hold every distinct stripe
/// pinned at once plus slack, so concurrent distinct-key fetches can
/// never deadlock on free-page exhaustion.
#[derive(Clone, Debug)]
pub struct Workload {
    pub page_size: usize,
    /// Pages spanned by one full stripe. The stripe byte length is
    /// `page_size * stripe_pages`.
    pub stripe_pages: usize,
    /// Distinct stripe keys the workload may reference; each fetch's
    /// `key_idx` indexes into this set modulo its length.
    pub key_count: u8,
    /// Upper bound on per-I/O pend count in the mocks.
    pub max_io_delay: u32,
    /// Per-I/O fault probability in `[0, 100]`. `0` is the happy path;
    /// positive values exercise the fetch-error path (which must drop
    /// its partial guards without leaking pins).
    pub io_fault_rate: u32,
    /// Per-page blockstore hit probability in `[0, 100]`. Drives the
    /// owner read path through both the disk-hit and transport-fetch
    /// branches.
    pub cache_hit_rate: u32,
    /// If true, size the owner pool to exactly one stripe
    /// (`stripe_pages` pages) instead of the generous "hold every
    /// distinct stripe at once" sizing. This forces free-page pressure:
    /// concurrent fetches for distinct keys cannot all be pinned, so the
    /// owner read path must return `Error::Busy` (fail-fast, never park
    /// on the free list) and coordinators must retry until a holder
    /// releases. Exercises the cross-shard deadlock-avoidance path.
    pub tight_pool: bool,
    pub clients: Vec<ClientSpec>,
    /// If true, after the run completes (the service task has dropped
    /// the receiver), issue one more `fetch` on a surviving channel
    /// clone and record whether it errored. A send after the service
    /// is gone must resolve with an error rather than parking forever.
    pub probe_shutdown: bool,
}

#[derive(Clone, Debug)]
pub struct ClientSpec {
    pub fetches: Vec<FetchSpec>,
}

/// One coordinator fetch: a byte range within stripe `key_idx`, plus a
/// `hold` delay (extra `yield_once` pends between reply and read) that
/// widens the pin-lifetime window.
#[derive(Clone, Debug)]
pub struct FetchSpec {
    pub key_idx: u8,
    pub offset: u64,
    pub len: u64,
    pub hold: u32,
}

impl Workload {
    fn stripe_len(&self) -> usize {
        self.page_size * self.stripe_pages
    }

    fn key(&self, idx: u8) -> StripeKey {
        let b = idx % self.key_count.max(1);
        StripeKey([b; 32])
    }

    /// Deterministic byte pattern for the stripe identified by `key`.
    /// Used both to populate the mocks and as the read oracle.
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
// Proptest strategy.
// ---------------------------------------------------------------------------

pub fn workload_strategy() -> impl Strategy<Value = Workload> {
    let page_size = prop_oneof![Just(64usize), Just(128), Just(256), Just(512)];
    let stripe_pages = 1usize..=4;
    let key_count = 1u8..=3;
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
    let clients = vec(client_strategy(), 1..=6);
    let probe_shutdown = any::<bool>();
    // Bias toward the generous-pool path so the page-pressure path is a
    // sizable minority of cases without crowding out the rest.
    let tight_pool = prop_oneof![2 => Just(false), 1 => Just(true)];

    (
        page_size,
        stripe_pages,
        key_count,
        max_io_delay,
        io_fault_rate,
        cache_hit_rate,
        clients,
        probe_shutdown,
        tight_pool,
    )
        .prop_map(
            |(
                page_size,
                stripe_pages,
                key_count,
                max_io_delay,
                io_fault_rate,
                cache_hit_rate,
                clients,
                probe_shutdown,
                tight_pool,
            )| Workload {
                page_size,
                stripe_pages,
                key_count,
                max_io_delay,
                io_fault_rate,
                cache_hit_rate,
                tight_pool,
                clients,
                probe_shutdown,
            },
        )
}

fn client_strategy() -> impl Strategy<Value = ClientSpec> {
    vec(fetch_strategy(), 1..=6).prop_map(|fetches| ClientSpec { fetches })
}

fn fetch_strategy() -> impl Strategy<Value = FetchSpec> {
    // Offsets / lengths are normalized against the stripe length in
    // `run_workload`, so the strategy stays independent of geometry.
    (0u8..=255u8, 0u64..=4096, 1u64..=4096, 0u32..=4).prop_map(|(key_idx, offset, len, hold)| {
        FetchSpec {
            key_idx,
            offset,
            len,
            hold,
        }
    })
}

// ---------------------------------------------------------------------------
// Outcomes and report.
// ---------------------------------------------------------------------------

/// Outcome of a single coordinator fetch.
#[derive(Clone, Debug)]
#[allow(dead_code)]
pub enum FetchOutcome {
    /// `fetch` resolved `Ok`; `got` is the concatenation of bytes read
    /// from the owner backing at each returned `PageLoc`, `expected` is
    /// the oracle slice, `page_locs` are the `(page_byte_offset, len)`
    /// pairs, and `len` is the requested byte length.
    Ok {
        got: Vec<u8>,
        expected: Vec<u8>,
        page_locs: Vec<(u64, u32)>,
        len: u64,
    },
    /// `fetch` resolved `Err` (an injected I/O fault propagated through
    /// the owner read path).
    Err(String),
}

/// What the harness reports back to the property tests.
#[derive(Debug)]
#[allow(dead_code)]
pub struct RunReport {
    pub outcomes: Vec<FetchOutcome>,
    pub free_pages_at_end: usize,
    pub total_pool_pages: usize,
    pub page_size: usize,
    pub io_fault_rate: u32,
    /// True if this run used the one-stripe `tight_pool` sizing.
    pub tight_pool: bool,
    /// Number of `Error::Busy` re-dispatches across all coordinators.
    /// Positive only under page pressure; proves the fail-fast + retry
    /// path ran.
    pub busy_retries: u64,
    pub bulk_get_calls: u32,
    pub read_page_calls: u32,
    pub read_hit_calls: u32,
    /// `Some(true)` if the post-shutdown probe fetch errored (expected
    /// once the service is gone), `Some(false)` if it unexpectedly
    /// succeeded, `None` if no probe ran.
    pub post_shutdown_send_errored: Option<bool>,
    pub steps: u64,
}

// ---------------------------------------------------------------------------
// Driver.
// ---------------------------------------------------------------------------

type OwnerPool = Pool<FanoutTransport, FanoutBlockStore, StripeReq>;

/// Drive `w` under `seed`. Returns the report so callers can assert
/// invariants. Panics only on framework setup errors that are not
/// "test failures" the caller should shrink against.
pub fn run_workload(seed: u64, w: Workload) -> Result<RunReport, RunError> {
    let stripe_len = w.stripe_len() as u64;
    let key_count = w.key_count.max(1);

    // Size the pool. The generous default holds every distinct stripe
    // pinned at once plus one stripe of slack, so concurrent distinct-key
    // fetches never contend for pages. The `tight_pool` variant instead
    // sizes the pool to exactly one stripe: the minimum that still
    // guarantees any single fetch fits (a fetch pins at most one full
    // stripe), so forward progress is preserved while distinct-key
    // concurrency is forced through the `Error::Busy` fail-fast + retry
    // path. Sizing below one stripe would wedge a lone fetch and is
    // therefore never generated.
    let total_pool_pages = if w.tight_pool {
        w.stripe_pages.max(1)
    } else {
        w.stripe_pages * (key_count as usize + 1)
    };

    let backing = heap_backing(w.page_size, total_pool_pages);
    let pool_base_v = backing.base as usize;

    // Oracle + mock stripe map (same bytes).
    let stripes: Stripes = Rc::new(RefCell::new(HashMap::new()));
    let mut oracle: HashMap<StripeKey, Vec<u8>> = HashMap::new();
    for idx in 0..key_count {
        let k = w.key(idx);
        let bytes = w.stripe_bytes(k);
        oracle.insert(k, bytes.clone());
        stripes.borrow_mut().insert(k, bytes);
    }

    let counts = Rc::new(CallCounts::default());
    let mock_cfg = MockSimConfig::new();
    mock_cfg.max_io_delay.set(w.max_io_delay);
    mock_cfg.io_fault_rate.set(w.io_fault_rate);
    mock_cfg.cache_hit_rate.set(w.cache_hit_rate);

    let transport = FanoutTransport::new(
        stripes.clone(),
        counts.clone(),
        mock_cfg.clone(),
        backing.base,
        backing.page_size,
    );
    let blockstore = FanoutBlockStore::new(counts.clone(), stripes.clone(), mock_cfg.clone());

    let pool: Rc<OwnerPool> = Rc::new(
        Pool::new(
            PoolConfig {
                // read_owned admits one stream per fetch; keep the limit
                // well above the concurrent fetch count.
                max_concurrent_streams: 1024,
                max_inflight_pages: w.stripe_pages.max(1),
            },
            backing,
            transport,
            blockstore,
        )
        .expect("Pool::new must succeed for a well-formed workload"),
    );

    let (channel, rx) = FetchChannel::new();
    // The service is driven below via `poll_once` against each task's own
    // poll Context, so the stored progress() waker is never used here.
    let mut service = FetchService::new(
        pool.clone(),
        rx,
        w.page_size,
        unbounded_storage::runtime::noop_waker(),
    );

    let mut exec = Executor::new(seed);

    let outcomes: Rc<RefCell<Vec<FetchOutcome>>> = Rc::new(RefCell::new(Vec::new()));
    let pending_clients: Rc<Cell<usize>> = Rc::new(Cell::new(w.clients.len()));
    let stop: Rc<Cell<bool>> = Rc::new(Cell::new(false));
    // Counts how many times a coordinator re-dispatched after the owner
    // returned `Error::Busy`. Non-zero proves the page-pressure path was
    // actually exercised; it is reported, not asserted, since a tight
    // run with no key contention may legitimately never hit it.
    let busy_retries: Rc<Cell<u64>> = Rc::new(Cell::new(0));

    // SERVICE task: cooperatively drive the owner FetchService. The
    // loop body is a `poll_fn` so `poll_once` runs against this task's
    // own poll Context (the mocks' `yield_n` re-wakes through it). On
    // stop, one final `poll_once` has already drained every queued
    // `Release` (clients enqueue release before finishing, and the
    // supervisor only flips stop once all clients are done).
    {
        let stop = stop.clone();
        exec.spawn(async move {
            loop {
                poll_fn(|cx| {
                    service.poll_once(cx);
                    Poll::Ready(())
                })
                .await;

                if stop.get() && !service.has_inflight() {
                    service.mark_dead();
                    return;
                }
                yield_once().await;
            }
        });
    }

    // SUPERVISOR task: flip stop once every client has finished.
    {
        let stop = stop.clone();
        let pending_clients = pending_clients.clone();
        exec.spawn(async move {
            while pending_clients.get() > 0 {
                yield_once().await;
            }
            stop.set(true);
        });
    }

    // COORDINATOR client tasks: one per ClientSpec. Each runs its fetch
    // list serially over a clone of the channel; concurrency comes from
    // multiple clients in flight at once.
    for c in w.clients.iter().cloned() {
        let channel = channel.clone();
        let outcomes = outcomes.clone();
        let pending_clients = pending_clients.clone();
        let busy_retries = busy_retries.clone();
        let w = w.clone();
        exec.spawn(async move {
            for f in &c.fetches {
                let key = w.key(f.key_idx);
                let offset = f.offset % stripe_len;
                let max_len = stripe_len - offset;
                let len = ((f.len - 1) % max_len) + 1;
                let expected = {
                    let stripes = stripes_for(&w, key);
                    stripes[offset as usize..(offset + len) as usize].to_vec()
                };

                // Retry-on-Busy loop, mirroring the coordinator's
                // re-dispatch in `http_serve::stream_body`. `Error::Busy`
                // is transient owner back-pressure (no free page for the
                // non-blocking head alloc); yield so a holder can release,
                // then re-issue. Any other error is a real fault and is
                // recorded. A genuine stall is bounded by the executor
                // step budget, which surfaces as `RunError`.
                let reply = loop {
                    let req = StripeReq::new(key);
                    match channel.fetch(req, offset, len).await {
                        Ok(reply) => break Some(reply),
                        Err(PoolError::Busy) => {
                            busy_retries.set(busy_retries.get() + 1);
                            yield_once().await;
                            continue;
                        }
                        Err(e) => {
                            outcomes
                                .borrow_mut()
                                .push(FetchOutcome::Err(format!("{e}")));
                            break None;
                        }
                    }
                };
                let Some(reply) = reply else {
                    continue;
                };

                // Hold the pin across a delay before reading, modeling
                // the in-flight SEND_ZC duration. The owner must keep
                // these pages valid until the matching release below.
                yield_n(f.hold).await;
                let mut got = Vec::with_capacity(len as usize);
                let mut page_locs = Vec::with_capacity(reply.pages.len());
                for ploc in &reply.pages {
                    // SAFETY: the owner service holds these pages pinned
                    // in its backing until we release the token below;
                    // `pool_base_v` is the live backing base and the loc
                    // is in range.
                    let bytes = unsafe {
                        std::slice::from_raw_parts(
                            (pool_base_v as *const u8).add(ploc.page_byte_offset as usize),
                            ploc.len as usize,
                        )
                    };
                    got.extend_from_slice(bytes);
                    page_locs.push((ploc.page_byte_offset, ploc.len));
                }
                let pin_token = reply.pin_token;
                outcomes.borrow_mut().push(FetchOutcome::Ok {
                    got,
                    expected,
                    page_locs,
                    len,
                });
                // Release only after the read so the pin-lifetime
                // assertion (bytes valid across the round-trip + hold)
                // is meaningful.
                channel.release(pin_token);
            }
            pending_clients.set(pending_clients.get() - 1);
        });
    }

    // Step budget: per fetch we expect O(delay + hold) pends across up
    // to `stripe_pages` page I/Os, inflated by the fault retry term and
    // a generous yields-per-await factor for the random interleaving.
    let total_fetches: u64 = w.clients.iter().map(|c| c.fetches.len() as u64).sum();
    let max_hold = w
        .clients
        .iter()
        .flat_map(|c| c.fetches.iter())
        .map(|f| f.hold as u64)
        .max()
        .unwrap_or(0);
    let step_budget = 4096
        + total_fetches
            * (w.max_io_delay as u64 + max_hold + 8)
            * 64
            * (1 + w.io_fault_rate as u64 / 4)
            * (w.stripe_pages as u64)
        // Under a tight pool, distinct-key fetches serialize through the
        // single stripe of capacity: in the worst case each fetch retries
        // once per still-pending fetch before a holder releases, an
        // O(total_fetches^2) term. The constant per-retry pend factor
        // keeps this principled rather than an open-ended cap, so a
        // genuine livelock still trips the budget.
        + if w.tight_pool {
            total_fetches * total_fetches * 64
        } else {
            0
        };

    exec.run(step_budget)?;

    let free_pages_at_end = pool.free_pages();

    // Service-shutdown probe: the service task has returned and dropped
    // the receiver, so a fetch on a surviving channel clone must error
    // rather than park forever.
    let post_shutdown_send_errored = if w.probe_shutdown {
        let key = w.key(0);
        let req = StripeReq::new(key);
        let len = w.page_size.min(stripe_len as usize) as u64;
        let res = block_on_local(channel.fetch(req, 0, len));
        Some(res.is_err())
    } else {
        None
    };

    let outcomes = Rc::try_unwrap(outcomes)
        .map_err(|_| RunError::Deadlock)
        .expect("all tasks completed; outcomes Rc must be unique")
        .into_inner();

    Ok(RunReport {
        outcomes,
        free_pages_at_end,
        total_pool_pages,
        page_size: w.page_size,
        io_fault_rate: w.io_fault_rate,
        tight_pool: w.tight_pool,
        busy_retries: busy_retries.get(),
        bulk_get_calls: counts.bulk_get.get(),
        read_page_calls: counts.read_page.get(),
        read_hit_calls: counts.read_hit.get(),
        post_shutdown_send_errored,
        steps: exec.last_steps(),
    })
}

/// Reconstruct the oracle stripe bytes for `key`. The map built in
/// `run_workload` is consumed by the mocks, so client tasks recompute
/// the deterministic pattern instead of sharing a borrow.
fn stripes_for(w: &Workload, key: StripeKey) -> Vec<u8> {
    w.stripe_bytes(key)
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
        keepalive: std::sync::Arc::new(owner),
    }
}

/// Minimal noop-waker `block_on` for the post-shutdown probe, which
/// runs after the executor returns and so cannot use the framework's
/// scheduler. The probed fetch resolves in a single poll (the send
/// fails immediately once the receiver is gone), so the spin budget is
/// generous insurance, not a hot loop.
fn block_on_local<F: Future>(fut: F) -> F::Output {
    fn raw() -> RawWaker {
        RawWaker::new(std::ptr::null(), &VTABLE)
    }
    static VTABLE: RawWakerVTable = RawWakerVTable::new(|_| raw(), |_| {}, |_| {}, |_| {});
    // SAFETY: the vtable functions are no-ops or return the same
    // vtable, so the waker can be cloned and dropped freely.
    let waker = unsafe { Waker::from_raw(raw()) };
    let mut cx = Context::from_waker(&waker);
    let mut fut = fut;
    // SAFETY: the future is owned here and never moved after pinning.
    let mut fut = unsafe { Pin::new_unchecked(&mut fut) };
    for _ in 0..1_000_000 {
        if let Poll::Ready(v) = fut.as_mut().poll(&mut cx) {
            return v;
        }
        std::thread::yield_now();
    }
    panic!("block_on_local: probe future did not complete within spin budget");
}
