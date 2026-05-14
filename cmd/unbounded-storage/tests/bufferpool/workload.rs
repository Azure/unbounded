// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Workload model, proptest strategies, and the `run_workload`
//! driver that ties the executor, mocks, and `Pool` together.

use std::cell::RefCell;
use std::collections::HashMap;
use std::rc::Rc;

use proptest::collection::vec;
use proptest::prelude::*;
use unbounded_storage::bufferpool::{Backing, BufferPool, Pool, PoolConfig, StripeKey};

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
    /// Distinct stripes the workload may reference. Each client's
    /// `key_idx` indexes into this set modulo its length.
    pub key_count: u8,
    pub clients: Vec<ClientSpec>,
}

#[derive(Clone, Debug)]
pub struct ClientSpec {
    pub key_idx: u8,
    /// Byte offset within the stripe.
    pub offset: u64,
    /// Length in bytes.
    pub len: u64,
    /// If `Some(k)`, the client drops its `ReadStream` after
    /// successfully receiving `k` pages (a value of `0` drops it
    /// immediately after `Pool::read` returns). Models a consumer
    /// that abandons mid-iteration; exercises the drain paths in
    /// `decrement_stream`, `release_guard`, and the leader-error
    /// recycle.
    pub cancel_after: Option<u32>,
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
    let key_count = 1u8..=3;
    let clients = vec(client_strategy(), 1..=8);

    (
        page_size,
        page_count,
        max_io_delay,
        io_fault_rate,
        cache_hit_rate,
        max_concurrent_streams,
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
                key_count,
                clients,
            )| Workload {
                page_size,
                page_count,
                max_io_delay,
                io_fault_rate,
                cache_hit_rate,
                max_concurrent_streams,
                key_count,
                clients,
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
    (0u8..=255u8, 0u64..=4096, 1u64..=4096, cancel_after).prop_map(
        |(key_idx, offset, len, cancel_after)| ClientSpec {
            key_idx,
            offset,
            len,
            cancel_after,
        },
    )
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
    let transport = DstTransport::new(stripes.clone(), counts.clone(), mock_cfg.clone());
    let blockstore = DstBlockStore::new(counts.clone(), stripes.clone(), mock_cfg.clone());
    blockstore.set_hit_rate(w.cache_hit_rate);

    let pool = Rc::new(
        Pool::new(
            PoolConfig {
                max_concurrent_streams: w.max_concurrent_streams,
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

        exec.spawn(async move {
            let req = TestReq { key };
            let stream = match p.read(&req, offset, len).await {
                Ok(s) => s,
                Err(e) => {
                    outcomes
                        .borrow_mut()
                        .push(ClientOutcome::ReadErr(format!("{e}")));
                    return;
                }
            };
            let _ = cid;
            // Cancel-before-first-page: drop the stream immediately.
            // This exercises the "stream dropped while the slot is
            // Idle and no fetch was issued" path in
            // `decrement_stream`.
            if let Some(0) = cancel_after {
                drop(stream);
                outcomes.borrow_mut().push(ClientOutcome::Cancelled {
                    got: Vec::new(),
                    expected,
                    pages_read: 0,
                });
                return;
            }
            let mut stream = stream;
            let mut got = Vec::with_capacity(len as usize);
            let mut pages_read: u32 = 0;
            // Set inside the `Some(Ok(_))` arm when the cancel
            // condition fires. We can't `drop(stream)` inside the
            // match arm because the `PageGuard` returned by
            // `next_page` borrows `stream` until the arm ends.
            let mut should_cancel = false;
            loop {
                match stream.next_page().await {
                    Some(Ok(g)) => {
                        got.extend_from_slice(g.as_slice());
                        // Drop `g` (the PageGuard) at the end of
                        // this match arm so the page is recyclable
                        // before we test the cancel condition.
                        drop(g);
                        pages_read += 1;
                        if cancel_after.map(|k| pages_read >= k).unwrap_or(false) {
                            should_cancel = true;
                        }
                    }
                    Some(Err(e)) => {
                        outcomes
                            .borrow_mut()
                            .push(ClientOutcome::FetchErr(format!("{e}")));
                        return;
                    }
                    None => break,
                }
                if should_cancel {
                    drop(stream);
                    outcomes.borrow_mut().push(ClientOutcome::Cancelled {
                        got,
                        expected,
                        pages_read,
                    });
                    return;
                }
            }
            outcomes
                .borrow_mut()
                .push(ClientOutcome::Ok { got, expected });
        });
    }

    // Step budget: generous upper bound. Per page we expect O(delay)
    // pends; across all clients * pages we add a healthy slack. The
    // fault-rate term covers leader re-leads after a transport error
    // recycles the slot and a new caller leads again.
    let max_pages_per_client = (stripe_len / w.page_size as u64) + 1;
    let fault_slack = 1 + w.io_fault_rate as u64 / 5;
    let step_budget =
        64 + (normalized.len() as u64) * max_pages_per_client * (w.max_io_delay as u64 + 4) * 16
            * fault_slack;

    exec.run(step_budget)?;

    let free_pages_at_end = pool.free_pages();
    let inflight_entries_at_end = pool.inflight_entries();

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
