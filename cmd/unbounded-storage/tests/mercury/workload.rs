// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! DST workload model and driver for Mercury's bulk-get RPC. The
//! workload itself is purely declarative (and serializable) so the
//! same value can flow through proptest, regression tests, and
//! recovery tests without any non-determinism leaking in. All
//! randomness still flows through the framework's `with_sim`; this
//! file only describes *what* to drive, not *how* to schedule.
//!
//! Reference-bytes rule (kept in lock-step with `mocks.rs::make_pair`):
//! peer `p` exposes `vec![p.0 as u8; peer_data_len]` as its canonical
//! payload. Both the destination buffer and the oracle are derived
//! from this single rule; if `mocks.rs` changes it, this file and
//! `oracle.rs` must change too.

#![allow(dead_code)]

use std::cell::RefCell;
use std::collections::HashMap;
use std::rc::Rc;

use proptest::collection::vec;
use proptest::prelude::*;
use serde::{Deserialize, Serialize};

use unbounded_storage::bufferpool::{BulkRef, Error as BpError, PageRef, StripeKey, Transport};
use unbounded_storage::mercury::PeerId;

use crate::framework::executor::Executor;
use crate::mercury::mocks::{MercuryCounters, MercurySimCfg, MockReq, make_pair};

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

/// Canonical page size used by `run_workload`. Mirrors the value
/// `mocks::make_pair` uses to size its destination buffer
/// (`page_size * 64`); changing one without the other breaks the
/// `page_idx`/`page_offset` bounds enforced by the strategy below.
pub(crate) const DST_PAGE_SIZE: u32 = 4096;

/// Number of pages the destination buffer in `make_pair` is sized
/// for. Matches the `* 64` in `mocks.rs`.
pub(crate) const DST_PAGE_COUNT: u32 = 64;

/// Default seed for the framework executor inside `run_workload`.
/// The executor's PRNG drives task scheduling and the mocks' fault
/// rolls; the workload itself is a pure data input that does not
/// otherwise touch the PRNG.
const RUN_SEED: u64 = 0xC0FFEE_u64;

// ---------------------------------------------------------------------------
// Workload model
// ---------------------------------------------------------------------------

/// A complete, replayable Mercury DST scenario. Self-contained: given
/// only a `Workload`, `run_workload` builds the executor, mocks, and
/// task set deterministically.
#[derive(Clone, Debug, Serialize, Deserialize)]
pub(crate) struct Workload {
    pub clients: Vec<ClientSpec>,
    pub peers: Vec<PeerSpec>,
    pub ops: Vec<Op>,
    pub cfg_seed: MercurySimCfgSeed,
}

/// A logical client. Per-client transports are constructed inside
/// `run_workload`; the `id` is for diagnostics only.
#[derive(Clone, Debug, Serialize, Deserialize)]
pub(crate) struct ClientSpec {
    pub id: u32,
}

/// A peer the workload may target. `data_len` controls how many bytes
/// `make_pair` allocates for that peer's reference payload.
#[derive(Clone, Debug, Serialize, Deserialize)]
pub(crate) struct PeerSpec {
    pub id: PeerIdSer,
    pub data_len: u32,
}

/// A single bulk_get operation. All fields are rebound by
/// `run_workload` against the actual peer/client tables, so the raw
/// values in the strategy can be unconstrained relative to the
/// workload's other fields.
#[derive(Clone, Debug, Serialize, Deserialize)]
pub(crate) struct Op {
    pub client_idx: u32,
    pub peer_idx: u32,
    pub stripe_key_seed: u8,
    pub stripe_off: u32,
    pub len: u32,
    pub page_idx: u32,
    pub page_offset: u32,
}

/// Integer-only seed for `MercurySimCfg`. The `_x10000` rates are
/// "parts per ten thousand" so the workload stays cleanly
/// serializable; the conversion to `f64` happens in
/// `MercurySimCfg::from_seed`.
#[derive(Clone, Copy, Debug, Serialize, Deserialize)]
pub(crate) struct MercurySimCfgSeed {
    pub min_latency_yields: u32,
    pub max_latency_yields: u32,
    pub forward_fault_rate_x10000: u32,
    pub short_read_rate_x10000: u32,
    pub peer_disconnect_rate_x10000: u32,
    pub capacity: u32,
}

/// Serializable mirror of `unbounded_storage::mercury::PeerId`.
/// `PeerId` itself does not derive serde, so we wrap a plain `u64`
/// for the workload model and convert on the way in.
#[derive(Clone, Copy, Debug, Serialize, Deserialize, PartialEq, Eq, Hash)]
pub(crate) struct PeerIdSer(pub u64);

impl PeerIdSer {
    pub fn to_peer(self) -> PeerId {
        PeerId(self.0)
    }
}

impl MercurySimCfgSeed {
    /// Build a fresh `MercurySimCfg` (with zeroed counters) from this
    /// seed. The returned `Rc` is the canonical handle the mocks
    /// share for the duration of one `run_workload` invocation.
    pub fn build(&self) -> Rc<MercurySimCfg> {
        let lo = self.min_latency_yields;
        let hi = self.max_latency_yields.max(lo);
        // The mock's `bulk_get` blocks until `current_in_flight <
        // capacity`; a `capacity` of zero means that gate never opens
        // once a single op enters in-flight, so we floor at 1. The
        // workload seed is left untouched so the persisted form still
        // round-trips losslessly.
        let capacity = self.capacity.max(1);
        Rc::new(MercurySimCfg {
            min_latency_yields: lo,
            max_latency_yields: hi,
            forward_fault_rate: rate_from_x10000(self.forward_fault_rate_x10000),
            short_read_rate: rate_from_x10000(self.short_read_rate_x10000),
            peer_disconnect_rate: rate_from_x10000(self.peer_disconnect_rate_x10000),
            capacity,
            counters: Rc::new(RefCell::new(MercuryCounters::default())),
        })
    }
}

fn rate_from_x10000(x: u32) -> f64 {
    (x.min(10_000) as f64) / 10_000.0
}

/// Free-function form so callers that already have a seed don't have
/// to pull `MercurySimCfgSeed::build` into scope. Mirrors the spec's
/// `MercurySimCfg::from_seed(&MercurySimCfgSeed)` signature.
pub(crate) fn cfg_from_seed(seed: &MercurySimCfgSeed) -> Rc<MercurySimCfg> {
    seed.build()
}

// ---------------------------------------------------------------------------
// Outcome
// ---------------------------------------------------------------------------

/// Per-op classification. Maps the `Result<(), BpError>` returned by
/// `Transport::bulk_get` onto a small, serializable enum the oracle
/// and invariant tests can match on without re-doing the string
/// inspection. The mapping uses the `Display` impl of the wrapped
/// `HgError` because `BpError::Transport` erases the concrete type
/// behind `Arc<dyn Error>`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) enum OpResult {
    Ok,
    ForwardErr,
    ShortReadErr,
    AddrLookupErr,
    OtherErr(String),
}

/// What `run_workload` hands back. `op_results` is indexed in the
/// same order as `Workload::ops`; tasks may complete in any order at
/// runtime but the slot for op `i` is always written by op `i`.
pub(crate) struct WorkloadOutcome {
    pub op_results: Vec<OpResult>,
    pub counters: MercuryCounters,
    pub dst_buffer: Vec<u8>,
    /// Same per-peer reference bytes the mocks generate, snapshotted
    /// so the oracle can compute expected ranges without re-deriving
    /// the rule. Keyed by the real `PeerId` (not the serializable
    /// wrapper).
    pub peer_data: HashMap<PeerId, Vec<u8>>,
}

// ---------------------------------------------------------------------------
// run_workload
// ---------------------------------------------------------------------------

/// Drive a workload to completion. Constructs a fresh executor, mocks,
/// and per-client transport set, spawns one task per op, runs to
/// quiescence, and returns the outcome. Panics on `RunError` (deadlock
/// or step-budget exhaustion) so the regression suite surfaces those
/// as test failures instead of silent hangs.
pub(crate) fn run_workload(wl: &Workload) -> WorkloadOutcome {
    // Empty workload is a valid input; return a fully-zeroed outcome
    // without touching the executor so we don't depend on `make_pair`
    // tolerating an empty peer list.
    if wl.ops.is_empty() || wl.peers.is_empty() || wl.clients.is_empty() {
        return WorkloadOutcome {
            op_results: Vec::new(),
            counters: MercuryCounters::default(),
            dst_buffer: Vec::new(),
            peer_data: HashMap::new(),
        };
    }

    let cfg = wl.cfg_seed.build();

    // Build the canonical peer set. We deduplicate by id (last
    // `data_len` wins) to mirror the HashMap shape `make_pair`
    // produces; the strategy already keeps peers small but a
    // hand-rolled `Workload` could have duplicates.
    let mut peer_data_len: HashMap<PeerId, u32> = HashMap::new();
    let mut peer_order: Vec<PeerId> = Vec::with_capacity(wl.peers.len());
    for p in &wl.peers {
        let id = p.id.to_peer();
        if !peer_data_len.contains_key(&id) {
            peer_order.push(id);
        }
        peer_data_len.insert(id, p.data_len.max(1));
    }

    // `make_pair` allocates a single `peer_data_len` for every peer;
    // we use the max here and cap each op's `(stripe_off, len)` against
    // its own peer's `data_len` below.
    let alloc_len = peer_data_len.values().copied().max().unwrap_or(1) as usize;
    let (_transports, _source, dst_buffer) = make_pair(
        Rc::clone(&cfg),
        peer_order.clone(),
        DST_PAGE_SIZE,
        alloc_len,
    );

    // Snapshot the per-peer reference bytes for the oracle. We
    // generate them ourselves rather than reach into `make_pair`'s
    // internals so the oracle has a stable view even if the mock
    // later starts mutating peer data.
    let mut peer_data: HashMap<PeerId, Vec<u8>> = HashMap::new();
    for p in &peer_order {
        // `make_pair` uses `peer_data_len` (the function arg) for
        // every peer, so the per-peer bytes are all `alloc_len` long;
        // we record that length so the oracle does the same range
        // arithmetic.
        peer_data.insert(*p, vec![p.0 as u8; alloc_len]);
    }

    let mut exec = Executor::new(RUN_SEED);
    let slots: Rc<RefCell<Vec<Option<OpResult>>>> = Rc::new(RefCell::new(vec![None; wl.ops.len()]));

    // The mock counts every in-flight task (including those parked in
    // the capacity-wait loop) against `current_in_flight`. If
    // `ops.len() > capacity` and every op fires concurrently, the
    // tasks past the cap deadlock waiting for a count that can never
    // drop. We sidestep that pathology by spawning ops in batches of
    // size `capacity` and draining each batch to quiescence before
    // spawning the next. The same `Executor` is reused so the PRNG
    // continues to evolve across batches and the schedule remains
    // fully deterministic.
    let batch_size = (wl.cfg_seed.capacity.max(1) as usize)
        .min(wl.ops.len())
        .max(1);

    for chunk_start in (0..wl.ops.len()).step_by(batch_size) {
        let chunk_end = (chunk_start + batch_size).min(wl.ops.len());
        for i in chunk_start..chunk_end {
            let op = &wl.ops[i];
            spawn_op(
                &mut exec,
                i,
                op,
                &peer_order,
                &peer_data_len,
                &cfg,
                &dst_buffer,
                Rc::clone(&slots),
            );
        }

        // Step budget per batch. Each op consumes O(latency) yields
        // plus capacity-wait yields proportional to the in-flight
        // peak (which is bounded by `batch_size`). `batch_size^2 *
        // latency * 64 + 4096` covers the worst case with comfortable
        // slack.
        let n = (chunk_end - chunk_start) as u64;
        let lat = wl
            .cfg_seed
            .max_latency_yields
            .max(wl.cfg_seed.min_latency_yields) as u64
            + 1;
        let step_budget = 4_096 + n * n * lat * 64;
        exec.run(step_budget)
            .expect("mercury workload batch exceeded step budget or deadlocked");
    }

    let counters = std::mem::take(&mut *cfg.counters.borrow_mut());
    let dst_snapshot = dst_buffer.borrow().clone();
    let op_results = Rc::try_unwrap(slots)
        .map_err(|_| ())
        .expect("all op tasks completed; slots Rc must be unique")
        .into_inner()
        .into_iter()
        .map(|s| s.expect("every op task wrote its slot"))
        .collect();

    WorkloadOutcome {
        op_results,
        counters,
        dst_buffer: dst_snapshot,
        peer_data,
    }
}

/// Spawn a single op as a future on `exec`. Pulled out of the main
/// loop so the batched-spawn path stays linear.
fn spawn_op(
    exec: &mut Executor,
    i: usize,
    op: &Op,
    peer_order: &[PeerId],
    peer_data_len: &HashMap<PeerId, u32>,
    cfg: &Rc<MercurySimCfg>,
    dst_buffer: &Rc<RefCell<Vec<u8>>>,
    slots: Rc<RefCell<Vec<Option<OpResult>>>>,
) {
    let peer_idx = (op.peer_idx as usize) % peer_order.len();
    let peer = peer_order[peer_idx];
    let peer_len = peer_data_len[&peer] as u64;

    // Bound the op against the peer's payload length and the
    // destination buffer's geometry so the mock's debug-asserts never
    // trip. `len` is guaranteed >= 1 by the strategy; we re-clamp
    // here for hand-rolled workloads.
    let raw_len = op.len.max(1).min(DST_PAGE_SIZE) as u64;
    let len = raw_len.min(peer_len) as u32;
    let len = len.max(1);
    let stripe_off = (op.stripe_off as u64) % peer_len.saturating_sub(len as u64).max(1);
    let stripe_off = if stripe_off + len as u64 > peer_len {
        0
    } else {
        stripe_off
    };

    let page_idx = op.page_idx % DST_PAGE_COUNT;
    let page_offset = op.page_offset % DST_PAGE_SIZE;
    let max_page_off = DST_PAGE_SIZE.saturating_sub(len);
    let page_offset = if max_page_off == 0 {
        0
    } else {
        page_offset % (max_page_off + 1)
    };
    debug_assert!(page_offset + len <= DST_PAGE_SIZE);

    let stripe_key = StripeKey([op.stripe_key_seed; 32]);
    let req = MockReq { key: stripe_key.0 };
    let bulk = BulkRef {
        stripe: stripe_key,
        offset: stripe_off,
        len,
    };
    let page = PageRef {
        page_idx,
        offset: page_offset,
        len,
    };

    let transport_for_task = build_transport(peer, cfg, dst_buffer);

    exec.spawn(async move {
        let res = transport_for_task.bulk_get(&req, bulk, page).await;
        let outcome = classify(&res);
        slots.borrow_mut()[i] = Some(outcome);
    });
}

/// Build a per-task `MockTransport` for `peer`, sharing the same
/// `cfg` (counters + faults) and `dst_buffer` as the rest of the
/// batch. The peer-bytes map is reconstructed from the canonical rule
/// `vec![peer.0 as u8; *]` since the public surface of `mocks.rs`
/// does not expose a way to re-use the one inside `make_pair`.
fn build_transport(
    peer: PeerId,
    cfg: &Rc<MercurySimCfg>,
    dst_buffer: &Rc<RefCell<Vec<u8>>>,
) -> crate::mercury::mocks::MockTransport {
    use crate::mercury::mocks::{MockTransport, PeerBytes};
    let peer_data_len = dst_buffer.borrow().len();
    let bytes_by_peer: PeerBytes = Rc::new(RefCell::new(HashMap::new()));
    bytes_by_peer
        .borrow_mut()
        .insert(peer, vec![peer.0 as u8; peer_data_len]);
    MockTransport::new(
        Rc::clone(cfg),
        bytes_by_peer,
        peer,
        DST_PAGE_SIZE,
        Rc::clone(dst_buffer),
    )
}

fn classify(res: &Result<(), BpError>) -> OpResult {
    match res {
        Ok(()) => OpResult::Ok,
        Err(e) => match e {
            BpError::Transport(inner) => {
                let s = format!("{inner}");
                if s.contains("HG_Forward") {
                    OpResult::ForwardErr
                } else if s.contains("short read") {
                    OpResult::ShortReadErr
                } else if s.contains("HG_Addr_lookup") {
                    OpResult::AddrLookupErr
                } else {
                    OpResult::OtherErr(s)
                }
            }
            other => OpResult::OtherErr(format!("{other}")),
        },
    }
}

// ---------------------------------------------------------------------------
// Strategy
// ---------------------------------------------------------------------------

/// Single proptest strategy producing a valid workload. Bounds are
/// chosen so every generated value passes the in-flight clamps in
/// `run_workload` without that clamping being visible to the oracle:
/// the oracle re-applies the same arithmetic when computing expected
/// ranges.
///
/// Destination ranges are made **disjoint per op** by (a) capping the
/// workload at `DST_PAGE_COUNT` ops and (b) overriding each op's
/// `page_idx` to its position in the workload. Without this, two ops
/// can target overlapping `(page_idx, page_offset, len)` regions and
/// the last-writer-wins semantics of the destination buffer breaks
/// the oracle's per-op byte-equality check.
pub(crate) fn workload_strategy() -> impl Strategy<Value = Workload> {
    // Generate peers as a count plus a vector of `data_len` values;
    // the `PeerId` values themselves are assigned sequentially as
    // `0..n` so `wl.peers` is guaranteed to have unique `PeerId`s.
    // Distinct peer IDs are required by the oracle, which mods op
    // indices against `wl.peers.len()` without re-deduplicating; if
    // the generator produced duplicates here, that count would
    // diverge from `run_workload`'s internal dedup and the oracle
    // would report spurious mismatches. The concrete ID values do
    // not affect transport correctness, so fixing them to `0..n`
    // removes a degree of generator freedom with no test value.
    let peers = vec(peer_data_len_strategy(), 1..=4).prop_map(|data_lens| {
        data_lens
            .into_iter()
            .enumerate()
            .map(|(i, data_len)| PeerSpec {
                id: PeerIdSer(i as u64),
                data_len,
            })
            .collect::<Vec<_>>()
    });
    let clients = vec(client_strategy(), 1..=4);
    // Cap ops at `DST_PAGE_COUNT` so every op gets a unique destination
    // page when we rebind `page_idx` below.
    let ops = vec(op_strategy(), 1..=(DST_PAGE_COUNT as usize));
    let cfg_seed = cfg_seed_strategy();

    (peers, clients, ops, cfg_seed).prop_map(|(peers, clients, mut ops, cfg_seed)| {
        for (i, op) in ops.iter_mut().enumerate() {
            // Deterministic per-op page; `i < DST_PAGE_COUNT` by the
            // vec-length cap above, so this is also the unmodded value.
            op.page_idx = (i as u32) % DST_PAGE_COUNT;
        }
        Workload {
            clients,
            peers,
            ops,
            cfg_seed,
        }
    })
}

fn peer_data_len_strategy() -> impl Strategy<Value = u32> {
    // Powers of two between 4 KiB and 64 KiB. The strategy intentionally
    // uses concrete values so shrinking lands on round numbers.
    prop_oneof![
        Just(4 * 1024u32),
        Just(8 * 1024),
        Just(16 * 1024),
        Just(32 * 1024),
        Just(64 * 1024),
    ]
}

fn client_strategy() -> impl Strategy<Value = ClientSpec> {
    (0u32..=255u32).prop_map(|id| ClientSpec { id })
}

fn op_strategy() -> impl Strategy<Value = Op> {
    // `len` >= 1 enforced here so a hand-rolled `Op { len: 0, .. }` is
    // the only path that can sneak a zero-length read into
    // `run_workload`; the run_workload clamp also guards against it.
    //
    // `page_offset` and `len` are jointly constrained so the
    // destination range `[page_offset, page_offset + len)` lies
    // entirely inside a single `DST_PAGE_SIZE` page. Combined with the
    // unique per-op `page_idx` assigned in `workload_strategy`, this
    // guarantees op-disjoint destination ranges within a workload.
    //
    // `page_idx` here is a placeholder; `workload_strategy` overrides
    // it to the op's index. We still generate a value so a
    // hand-constructed `Op` outside the strategy has a sane default.
    (
        0u32..=15,                  // client_idx (modded against client count in run)
        0u32..=15,                  // peer_idx (modded against peer count in run)
        0u8..=255u8,                // stripe_key_seed
        0u32..=(64 * 1024 - 1),     // stripe_off (modded against peer_data_len)
        0u32..=(DST_PAGE_SIZE - 1), // page_offset
        1u32..=DST_PAGE_SIZE,       // raw_len, clamped to fit page below
    )
        .prop_map(
            |(client_idx, peer_idx, stripe_key_seed, stripe_off, page_offset, raw_len)| {
                // `DST_PAGE_SIZE - page_offset >= 1` since `page_offset
                // <= DST_PAGE_SIZE - 1`, so `len >= 1` is preserved.
                let len = raw_len.min(DST_PAGE_SIZE - page_offset).max(1);
                Op {
                    client_idx,
                    peer_idx,
                    stripe_key_seed,
                    stripe_off,
                    len,
                    page_idx: 0,
                    page_offset,
                }
            },
        )
}

fn cfg_seed_strategy() -> impl Strategy<Value = MercurySimCfgSeed> {
    let capacity = prop_oneof![
        Just(4u32),
        Just(8),
        Just(16),
        Just(32),
        Just(64),
        Just(u32::MAX),
    ];
    (
        0u32..=4,     // min_latency_yields
        0u32..=8,     // max_latency_yields
        0u32..=2_000, // forward_fault_rate_x10000
        0u32..=2_000, // short_read_rate_x10000
        0u32..=500,   // peer_disconnect_rate_x10000
        capacity,
    )
        .prop_map(
            |(
                min_l,
                max_l,
                forward_fault_rate_x10000,
                short_read_rate_x10000,
                peer_disconnect_rate_x10000,
                capacity,
            )| MercurySimCfgSeed {
                min_latency_yields: min_l,
                max_latency_yields: max_l.max(min_l),
                forward_fault_rate_x10000,
                short_read_rate_x10000,
                peer_disconnect_rate_x10000,
                capacity,
            },
        )
}

// ---------------------------------------------------------------------------
// Hand-rolled builders
// ---------------------------------------------------------------------------

/// `MercurySimCfgSeed` with no faults and unbounded capacity. Used by
/// `recovery.rs` to author scripted cases without re-deriving the
/// "everything off" config.
pub(crate) fn empty_cfg_seed() -> MercurySimCfgSeed {
    MercurySimCfgSeed {
        min_latency_yields: 0,
        max_latency_yields: 0,
        forward_fault_rate_x10000: 0,
        short_read_rate_x10000: 0,
        peer_disconnect_rate_x10000: 0,
        capacity: u32::MAX,
    }
}

/// Build a small but non-trivial workload from a `seed`. Identical
/// inputs always yield identical workloads (no PRNG); the `seed`
/// merely permutes the chosen ids and offsets.
pub(crate) fn deterministic_workload(seed: u64) -> Workload {
    let s = seed;
    let peer_count = 1 + (s % 3) as usize; // 1..=3 peers
    let client_count = 1 + ((s >> 8) % 3) as usize; // 1..=3 clients
    let op_count = 1 + ((s >> 16) % 8) as usize; // 1..=8 ops

    let peers: Vec<PeerSpec> = (0..peer_count as u64)
        .map(|i| PeerSpec {
            id: PeerIdSer(i + (s & 0xff)),
            data_len: 4096,
        })
        .collect();
    let clients: Vec<ClientSpec> = (0..client_count as u32)
        .map(|i| ClientSpec { id: i })
        .collect();

    let ops: Vec<Op> = (0..op_count as u64)
        .map(|i| Op {
            client_idx: (i % client_count as u64) as u32,
            peer_idx: (i % peer_count as u64) as u32,
            stripe_key_seed: ((i + s) & 0xff) as u8,
            stripe_off: ((i * 64) & 0xfff) as u32,
            len: 64 + (i as u32 % 64),
            page_idx: (i % DST_PAGE_COUNT as u64) as u32,
            page_offset: 0,
        })
        .collect();

    Workload {
        clients,
        peers,
        ops,
        cfg_seed: empty_cfg_seed(),
    }
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn run_empty_workload() {
        let wl = Workload {
            clients: Vec::new(),
            peers: Vec::new(),
            ops: Vec::new(),
            cfg_seed: empty_cfg_seed(),
        };
        let out = run_workload(&wl);
        assert!(out.op_results.is_empty());
        assert_eq!(out.counters.current_in_flight, 0);
        assert_eq!(out.counters.forwards_started, 0);
        assert!(out.dst_buffer.is_empty());
        assert!(out.peer_data.is_empty());
    }

    #[test]
    fn run_single_op_no_faults() {
        let wl = Workload {
            clients: vec![ClientSpec { id: 0 }],
            peers: vec![PeerSpec {
                id: PeerIdSer(7),
                data_len: 4096,
            }],
            ops: vec![Op {
                client_idx: 0,
                peer_idx: 0,
                stripe_key_seed: 1,
                stripe_off: 0,
                len: 64,
                page_idx: 1,
                page_offset: 0,
            }],
            cfg_seed: empty_cfg_seed(),
        };
        let out = run_workload(&wl);
        assert_eq!(out.op_results.len(), 1);
        assert_eq!(out.op_results[0], OpResult::Ok);
        assert_eq!(out.counters.forwards_completed_ok, 1);
        assert_eq!(out.counters.forwards_completed_err, 0);
        assert_eq!(out.counters.current_in_flight, 0);

        // Bytes copied are `[peer.0 as u8; len]` per the reference rule.
        let dst_off = (1usize) * (DST_PAGE_SIZE as usize);
        for b in &out.dst_buffer[dst_off..dst_off + 64] {
            assert_eq!(*b, 7u8);
        }
        assert_eq!(out.peer_data.get(&PeerId(7)).map(|v| v.len()), Some(4096));
    }

    #[test]
    fn run_with_high_fault_rate_does_not_panic() {
        let mut cfg = empty_cfg_seed();
        cfg.forward_fault_rate_x10000 = 10_000; // 100% forward fault
        let wl = Workload {
            clients: vec![ClientSpec { id: 0 }],
            peers: vec![PeerSpec {
                id: PeerIdSer(2),
                data_len: 4096,
            }],
            ops: (0..4)
                .map(|i| Op {
                    client_idx: 0,
                    peer_idx: 0,
                    stripe_key_seed: 9,
                    stripe_off: 0,
                    len: 32,
                    page_idx: i,
                    page_offset: 0,
                })
                .collect(),
            cfg_seed: cfg,
        };
        let out = run_workload(&wl);
        assert_eq!(out.op_results.len(), 4);
        for r in &out.op_results {
            assert_eq!(*r, OpResult::ForwardErr);
        }
        assert_eq!(out.counters.forwards_completed_err, 4);
        assert_eq!(out.counters.forwards_completed_ok, 0);
        assert_eq!(out.counters.current_in_flight, 0);
    }

    proptest! {
        #![proptest_config(ProptestConfig::with_cases(32))]

        #[test]
        fn workload_strategy_runs_without_panicking(wl in workload_strategy()) {
            let _ = run_workload(&wl);
        }
    }
}
