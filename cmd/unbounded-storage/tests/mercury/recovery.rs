// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Hand-rolled regression scenarios for Mercury's DST workload. Each
//! test constructs a `Workload` deterministically (no proptest) so a
//! failure here points at exactly one bug. These cases exercise
//! corners that the random proptest in `tests.rs` only hits rarely
//! (or by construction, never).

#![allow(dead_code)]

use crate::mercury::oracle::{ViolationKind, audit};
use crate::mercury::workload::{
    ClientSpec, DST_PAGE_COUNT, Op, OpResult, PeerIdSer, PeerSpec, Workload,
    deterministic_workload, empty_cfg_seed, run_workload,
};

/// Replace every op's `page_idx` with its position in the workload
/// so destination ranges stay disjoint. Mirrors the rebinding that
/// `workload_strategy` does for proptest cases; hand-rolled
/// workloads in this file rely on it for the same reason (the
/// oracle's per-op byte check assumes the previous op did not
/// already overwrite the same destination range).
fn assign_unique_pages(ops: &mut [Op]) {
    debug_assert!(ops.len() <= DST_PAGE_COUNT as usize);
    for (i, op) in ops.iter_mut().enumerate() {
        op.page_idx = (i as u32) % DST_PAGE_COUNT;
    }
}

#[test]
fn recovery_capacity_one_serializes_ops() {
    let mut wl = deterministic_workload(0x11);
    wl.cfg_seed = empty_cfg_seed();
    wl.cfg_seed.capacity = 1;

    // 16 ops, one peer, one client, disjoint dst pages.
    wl.peers = vec![PeerSpec {
        id: PeerIdSer(3),
        data_len: 4096,
    }];
    wl.clients = vec![ClientSpec { id: 0 }];
    wl.ops = (0..16u32)
        .map(|i| Op {
            client_idx: 0,
            peer_idx: 0,
            stripe_key_seed: i as u8,
            stripe_off: 0,
            len: 64,
            page_idx: i,
            page_offset: 0,
        })
        .collect();
    assign_unique_pages(&mut wl.ops);

    let outcome = run_workload(&wl);
    assert_eq!(outcome.op_results.len(), 16);
    assert!(
        outcome.counters.peak_in_flight <= 1,
        "capacity = 1 must serialize ops; peak_in_flight = {}",
        outcome.counters.peak_in_flight
    );
    audit(&wl, &outcome).expect("clean audit for capacity = 1");
}

#[test]
fn recovery_full_fault_then_drain() {
    let mut wl = deterministic_workload(0x22);
    wl.cfg_seed = empty_cfg_seed();
    wl.cfg_seed.forward_fault_rate_x10000 = 10_000;

    wl.peers = vec![PeerSpec {
        id: PeerIdSer(5),
        data_len: 4096,
    }];
    wl.clients = vec![ClientSpec { id: 0 }];
    wl.ops = (0..8u32)
        .map(|i| Op {
            client_idx: 0,
            peer_idx: 0,
            stripe_key_seed: i as u8,
            stripe_off: 0,
            len: 32,
            page_idx: i,
            page_offset: 0,
        })
        .collect();
    assign_unique_pages(&mut wl.ops);

    let outcome = run_workload(&wl);
    assert_eq!(outcome.op_results.len(), 8);
    for (i, r) in outcome.op_results.iter().enumerate() {
        assert_eq!(*r, OpResult::ForwardErr, "op {i} expected ForwardErr");
    }
    assert_eq!(outcome.counters.current_in_flight, 0);
    audit(&wl, &outcome).expect("clean audit for 100% forward fault");
}

#[test]
fn recovery_full_short_read_yields_short_read_errors() {
    let mut wl = deterministic_workload(0x33);
    wl.cfg_seed = empty_cfg_seed();
    wl.cfg_seed.short_read_rate_x10000 = 10_000;

    wl.peers = vec![PeerSpec {
        id: PeerIdSer(9),
        data_len: 4096,
    }];
    wl.clients = vec![ClientSpec { id: 0 }];
    wl.ops = (0..8u32)
        .map(|i| Op {
            client_idx: 0,
            peer_idx: 0,
            stripe_key_seed: i as u8,
            stripe_off: 0,
            // len > 1 so the short-read range `0..src.len` is non-empty
            // (the mock zero-clamps when src.len <= 1).
            len: 32,
            page_idx: i,
            page_offset: 0,
        })
        .collect();
    assign_unique_pages(&mut wl.ops);

    let outcome = run_workload(&wl);
    assert_eq!(outcome.op_results.len(), 8);
    for (i, r) in outcome.op_results.iter().enumerate() {
        assert_eq!(*r, OpResult::ShortReadErr, "op {i} expected ShortReadErr");
    }
    audit(&wl, &outcome).expect("clean audit for 100% short read");
}

#[test]
fn recovery_full_disconnect() {
    let mut wl = deterministic_workload(0x44);
    wl.cfg_seed = empty_cfg_seed();
    wl.cfg_seed.peer_disconnect_rate_x10000 = 10_000;

    wl.peers = vec![PeerSpec {
        id: PeerIdSer(11),
        data_len: 4096,
    }];
    wl.clients = vec![ClientSpec { id: 0 }];
    wl.ops = (0..8u32)
        .map(|i| Op {
            client_idx: 0,
            peer_idx: 0,
            stripe_key_seed: i as u8,
            stripe_off: 0,
            len: 32,
            page_idx: i,
            page_offset: 0,
        })
        .collect();
    assign_unique_pages(&mut wl.ops);

    let outcome = run_workload(&wl);
    assert_eq!(outcome.op_results.len(), 8);
    for (i, r) in outcome.op_results.iter().enumerate() {
        assert_eq!(*r, OpResult::AddrLookupErr, "op {i} expected AddrLookupErr");
    }
    audit(&wl, &outcome).expect("clean audit for 100% peer disconnect");
}

#[test]
fn recovery_mixed_faults_30pct() {
    let mut wl = deterministic_workload(0x55);
    wl.cfg_seed = empty_cfg_seed();
    wl.cfg_seed.forward_fault_rate_x10000 = 3_000;
    wl.cfg_seed.short_read_rate_x10000 = 3_000;
    wl.cfg_seed.peer_disconnect_rate_x10000 = 3_000;
    wl.cfg_seed.capacity = 8;

    wl.peers = vec![
        PeerSpec {
            id: PeerIdSer(1),
            data_len: 4096,
        },
        PeerSpec {
            id: PeerIdSer(2),
            data_len: 4096,
        },
    ];
    wl.clients = vec![ClientSpec { id: 0 }, ClientSpec { id: 1 }];
    wl.ops = (0..32u32)
        .map(|i| Op {
            client_idx: i % 2,
            peer_idx: i % 2,
            stripe_key_seed: i as u8,
            stripe_off: 0,
            len: 64,
            page_idx: i,
            page_offset: 0,
        })
        .collect();
    assign_unique_pages(&mut wl.ops);

    let outcome = run_workload(&wl);
    assert_eq!(outcome.op_results.len(), 32);
    audit(&wl, &outcome).expect("clean audit for mixed 30% faults");
}

#[test]
fn recovery_high_latency_does_not_change_outcome() {
    let mut wl = deterministic_workload(0x66);
    wl.cfg_seed = empty_cfg_seed();
    wl.cfg_seed.min_latency_yields = 20;
    wl.cfg_seed.max_latency_yields = 20;
    wl.cfg_seed.capacity = 4;

    wl.peers = vec![PeerSpec {
        id: PeerIdSer(13),
        data_len: 4096,
    }];
    wl.clients = vec![ClientSpec { id: 0 }];
    wl.ops = (0..8u32)
        .map(|i| Op {
            client_idx: 0,
            peer_idx: 0,
            stripe_key_seed: i as u8,
            stripe_off: 0,
            len: 64,
            page_idx: i,
            page_offset: 0,
        })
        .collect();
    assign_unique_pages(&mut wl.ops);

    let outcome = run_workload(&wl);
    assert_eq!(outcome.op_results.len(), 8);
    for (i, r) in outcome.op_results.iter().enumerate() {
        assert_eq!(*r, OpResult::Ok, "op {i} expected Ok under no faults");
    }
    assert!(
        outcome.counters.peak_in_flight <= wl.cfg_seed.capacity,
        "peak_in_flight = {} exceeds capacity = {}",
        outcome.counters.peak_in_flight,
        wl.cfg_seed.capacity
    );
    audit(&wl, &outcome).expect("clean audit for high-latency no-fault workload");
}

#[test]
fn recovery_single_op_minimal_workload() {
    let wl = Workload {
        clients: vec![ClientSpec { id: 0 }],
        peers: vec![PeerSpec {
            id: PeerIdSer(17),
            data_len: 4096,
        }],
        ops: vec![Op {
            client_idx: 0,
            peer_idx: 0,
            stripe_key_seed: 0,
            stripe_off: 0,
            len: 8,
            page_idx: 0,
            page_offset: 0,
        }],
        cfg_seed: empty_cfg_seed(),
    };

    let outcome = run_workload(&wl);
    assert_eq!(outcome.op_results.len(), 1);
    assert_eq!(outcome.op_results[0], OpResult::Ok);
    assert_eq!(outcome.counters.forwards_completed_ok, 1);
    assert_eq!(outcome.counters.current_in_flight, 0);
    audit(&wl, &outcome).expect("clean audit for minimal workload");
}

#[test]
fn recovery_capacity_zero_clamps_to_one() {
    // Lock in the clamp behavior in `MercurySimCfgSeed::build`: a
    // `capacity` of zero is silently floored to 1 so the mock's
    // capacity-wait loop can make progress. If someone removes the
    // floor, the executor will deadlock (step-budget exhaustion) and
    // `run_workload` will panic, which this test will surface.
    let mut wl = deterministic_workload(0x77);
    wl.cfg_seed = empty_cfg_seed();
    wl.cfg_seed.capacity = 0;

    wl.peers = vec![PeerSpec {
        id: PeerIdSer(19),
        data_len: 4096,
    }];
    wl.clients = vec![ClientSpec { id: 0 }];
    wl.ops = (0..4u32)
        .map(|i| Op {
            client_idx: 0,
            peer_idx: 0,
            stripe_key_seed: i as u8,
            stripe_off: 0,
            len: 32,
            page_idx: i,
            page_offset: 0,
        })
        .collect();
    assign_unique_pages(&mut wl.ops);

    let outcome = run_workload(&wl);
    assert_eq!(outcome.op_results.len(), 4);
    for (i, r) in outcome.op_results.iter().enumerate() {
        assert_eq!(*r, OpResult::Ok, "op {i} expected Ok with capacity clamp");
    }
    // The seed still reports capacity = 0; the oracle's counter
    // check compares `peak_in_flight` against that serialized value
    // and will complain. We mirror tests.rs and tolerate exactly
    // that single class of violation.
    assert_eq!(outcome.counters.current_in_flight, 0);
    assert_eq!(outcome.counters.forwards_completed_ok, 4);
    assert_eq!(outcome.counters.forwards_completed_err, 0);
    assert!(
        outcome.counters.peak_in_flight <= 1,
        "peak_in_flight = {} exceeds clamped capacity of 1",
        outcome.counters.peak_in_flight
    );
    match audit(&wl, &outcome) {
        Ok(()) => {}
        Err(violations) => {
            for v in &violations {
                assert!(
                    matches!(v.kind, ViolationKind::CountersInconsistent)
                        && v.message.contains("peak_in_flight")
                        && v.message.contains("exceeds configured capacity = 0"),
                    "unexpected violation under capacity=0: {:?} {}",
                    v.kind,
                    v.message
                );
            }
        }
    }
}
