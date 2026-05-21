// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Oracle for Mercury's DST workload. Given a `Workload` and the
//! `WorkloadOutcome` produced by `run_workload`, this module audits a
//! handful of invariants:
//!
//! 1. Outcome shape - per-op result and destination buffer sizes line
//!    up with the workload and the canonical page geometry.
//! 2. In-flight quiescence - no RPC is left dangling and the peak
//!    in-flight count respects the configured capacity.
//! 3. Counter consistency - started/completed/bytes counters agree
//!    with the per-op outcomes.
//! 4. Bytes correctness - for every `OpResult::Ok`, the destination
//!    range contains the peer's reference byte (`peer.id.0 as u8`)
//!    across the full effective length the workload's `spawn_op`
//!    derives. Non-Ok ops are deliberately not inspected: the
//!    production contract leaves the destination undefined on
//!    failure.
//! 5. Saturating-fault contradictions - 100% forward-fault or 100%
//!    peer-disconnect must surface a matching error for every op; an
//!    `Ok` under those configs is an `UnexpectedSuccess`.
//!
//! The oracle never panics on its own (except via `assert_consistent`,
//! which is a thin wrapper for proptest call sites); the `check_*`
//! helpers always return `Result` so `recovery.rs` can compose them.

#![allow(dead_code)]

use super::mocks::MercuryCounters;
use super::workload::{DST_PAGE_COUNT, DST_PAGE_SIZE, Op, OpResult, Workload, WorkloadOutcome};

// ---------------------------------------------------------------------------
// Public types
// ---------------------------------------------------------------------------

/// A single oracle violation. `message` is human-formatted (with the
/// observed vs expected values inline) so proptest's shrinker output
/// is self-explanatory.
#[derive(Debug, Clone)]
pub(crate) struct Violation {
    pub kind: ViolationKind,
    pub op_idx: Option<usize>,
    pub message: String,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum ViolationKind {
    BytesMismatch,
    CountersInconsistent,
    InFlightLeak,
    UnexpectedSuccess,
    OutcomeShapeMismatch,
}

// ---------------------------------------------------------------------------
// Top-level entry points
// ---------------------------------------------------------------------------

/// Run every invariant. Returns `Ok(())` iff none fired.
pub(crate) fn audit(wl: &Workload, outcome: &WorkloadOutcome) -> Result<(), Vec<Violation>> {
    let mut v: Vec<Violation> = Vec::new();

    // Shape first - subsequent checks index into op_results/dst_buffer
    // and assume they are well-formed.
    if let Err(shape) = check_outcome_shape(wl, outcome) {
        v.push(shape);
        // Without a sane shape the per-op checks are unsafe; bail.
        return Err(v);
    }

    if let Err(leak) = check_in_flight(outcome) {
        v.push(leak);
    }

    if let Err(mut counters) = check_counters(wl, outcome) {
        v.append(&mut counters);
    }

    for i in 0..wl.ops.len() {
        if outcome.op_results[i] == OpResult::Ok {
            if let Err(bytes) = check_op_bytes(wl, outcome, i) {
                v.push(bytes);
            }
        }
    }

    if let Err(mut sat) = check_saturating_faults(wl, outcome) {
        v.append(&mut sat);
    }

    if v.is_empty() { Ok(()) } else { Err(v) }
}

/// Proptest-friendly entrypoint: panics with a formatted summary of
/// every violation if the audit fails.
pub(crate) fn assert_consistent(wl: &Workload, outcome: &WorkloadOutcome) {
    if let Err(violations) = audit(wl, outcome) {
        panic!(
            "{} oracle violation(s):\n{}",
            violations.len(),
            violations
                .iter()
                .map(|v| format!("  - [{:?}] op={:?}: {}", v.kind, v.op_idx, v.message))
                .collect::<Vec<_>>()
                .join("\n")
        );
    }
}

// ---------------------------------------------------------------------------
// Individual checks
// ---------------------------------------------------------------------------

/// Verify the basic shape of the outcome: lengths, the dst buffer
/// size, and that every peer referenced by a successful op is keyed
/// in `peer_data`. Empty-workload short-circuit is honored
/// (run_workload returns an empty dst_buffer and empty peer_data
/// when there are no ops).
pub(crate) fn check_outcome_shape(
    wl: &Workload,
    outcome: &WorkloadOutcome,
) -> Result<(), Violation> {
    if outcome.op_results.len() != wl.ops.len() {
        return Err(Violation {
            kind: ViolationKind::OutcomeShapeMismatch,
            op_idx: None,
            message: format!(
                "op_results.len() = {}, expected {}",
                outcome.op_results.len(),
                wl.ops.len()
            ),
        });
    }

    let empty_short_circuit = wl.ops.is_empty() || wl.peers.is_empty() || wl.clients.is_empty();

    if empty_short_circuit {
        if !outcome.dst_buffer.is_empty() {
            return Err(Violation {
                kind: ViolationKind::OutcomeShapeMismatch,
                op_idx: None,
                message: format!(
                    "empty workload produced non-empty dst_buffer ({} bytes)",
                    outcome.dst_buffer.len()
                ),
            });
        }
        if !outcome.peer_data.is_empty() {
            return Err(Violation {
                kind: ViolationKind::OutcomeShapeMismatch,
                op_idx: None,
                message: format!(
                    "empty workload produced non-empty peer_data ({} entries)",
                    outcome.peer_data.len()
                ),
            });
        }
        return Ok(());
    }

    let expected_dst = (DST_PAGE_SIZE as usize) * (DST_PAGE_COUNT as usize);
    if outcome.dst_buffer.len() != expected_dst {
        return Err(Violation {
            kind: ViolationKind::OutcomeShapeMismatch,
            op_idx: None,
            message: format!(
                "dst_buffer.len() = {}, expected DST_PAGE_SIZE*DST_PAGE_COUNT = {}",
                outcome.dst_buffer.len(),
                expected_dst
            ),
        });
    }

    for (i, op) in wl.ops.iter().enumerate() {
        if outcome.op_results[i] != OpResult::Ok {
            continue;
        }
        let peer_idx = (op.peer_idx as usize) % wl.peers.len();
        let peer = wl.peers[peer_idx].id.to_peer();
        if !outcome.peer_data.contains_key(&peer) {
            return Err(Violation {
                kind: ViolationKind::OutcomeShapeMismatch,
                op_idx: Some(i),
                message: format!(
                    "Ok op references peer {:?} but peer_data has no entry for it",
                    peer
                ),
            });
        }
    }

    Ok(())
}

/// Verify `current_in_flight == 0` and that `peak_in_flight` respects
/// the configured capacity (when capacity is bounded).
pub(crate) fn check_in_flight(outcome: &WorkloadOutcome) -> Result<(), Violation> {
    if outcome.counters.current_in_flight != 0 {
        return Err(Violation {
            kind: ViolationKind::InFlightLeak,
            op_idx: None,
            message: format!(
                "current_in_flight = {}, expected 0",
                outcome.counters.current_in_flight
            ),
        });
    }
    Ok(())
}

/// Verify the global counters agree with the per-op outcomes. We
/// enforce:
///   - `forwards_started == ok + err` once quiescent.
///   - `forwards_completed_ok` matches the count of `OpResult::Ok`.
///   - `forwards_completed_err` matches the count of non-Ok results.
///   - `bytes_pushed` is in `[ok_count, ok_count * DST_PAGE_SIZE]`.
///   - `peak_in_flight <= capacity` when capacity is bounded.
pub(crate) fn check_counters(
    wl: &Workload,
    outcome: &WorkloadOutcome,
) -> Result<(), Vec<Violation>> {
    let c: &MercuryCounters = &outcome.counters;
    let mut v: Vec<Violation> = Vec::new();

    let ok_count = outcome
        .op_results
        .iter()
        .filter(|r| matches!(r, OpResult::Ok))
        .count() as u64;
    let err_count = outcome
        .op_results
        .iter()
        .filter(|r| !matches!(r, OpResult::Ok))
        .count() as u64;

    if c.current_in_flight == 0 && c.forwards_started != ok_count + err_count {
        v.push(Violation {
            kind: ViolationKind::CountersInconsistent,
            op_idx: None,
            message: format!(
                "forwards_started = {}, expected ok + err = {} + {} = {}",
                c.forwards_started,
                ok_count,
                err_count,
                ok_count + err_count
            ),
        });
    }

    if c.forwards_completed_ok != ok_count {
        v.push(Violation {
            kind: ViolationKind::CountersInconsistent,
            op_idx: None,
            message: format!(
                "forwards_completed_ok = {}, expected {} (OpResult::Ok count)",
                c.forwards_completed_ok, ok_count
            ),
        });
    }

    if c.forwards_completed_err != err_count {
        v.push(Violation {
            kind: ViolationKind::CountersInconsistent,
            op_idx: None,
            message: format!(
                "forwards_completed_err = {}, expected {} (non-Ok count)",
                c.forwards_completed_err, err_count
            ),
        });
    }

    let bytes_lo = ok_count;
    let bytes_hi = ok_count * (DST_PAGE_SIZE as u64);
    if c.bytes_pushed < bytes_lo || c.bytes_pushed > bytes_hi {
        v.push(Violation {
            kind: ViolationKind::CountersInconsistent,
            op_idx: None,
            message: format!(
                "bytes_pushed = {}, expected in [{}, {}] (ok_count = {})",
                c.bytes_pushed, bytes_lo, bytes_hi, ok_count
            ),
        });
    }

    let cap = wl.cfg_seed.capacity;
    if cap != u32::MAX && c.peak_in_flight > cap {
        v.push(Violation {
            kind: ViolationKind::CountersInconsistent,
            op_idx: None,
            message: format!(
                "peak_in_flight = {}, exceeds configured capacity = {}",
                c.peak_in_flight, cap
            ),
        });
    }

    if v.is_empty() { Ok(()) } else { Err(v) }
}

/// For an `OpResult::Ok` op, verify the destination slice in
/// `dst_buffer` contains the peer's reference byte across the full
/// effective length the workload's `spawn_op` derives. Returns the
/// first mismatching byte (and its expected/observed values) when
/// the slice is incorrect.
///
/// `op_idx` must be a valid index; the caller is expected to gate
/// this on the result already being `Ok`.
pub(crate) fn check_op_bytes(
    wl: &Workload,
    outcome: &WorkloadOutcome,
    op_idx: usize,
) -> Result<(), Violation> {
    let op = &wl.ops[op_idx];
    let peer_idx = (op.peer_idx as usize) % wl.peers.len();
    let peer_spec = &wl.peers[peer_idx];
    let peer = peer_spec.id.to_peer();

    let (dst_off, eff_len) = effective_range(op, peer_spec.data_len);
    let expected = peer.0 as u8;

    let buf = &outcome.dst_buffer;
    let end = dst_off + eff_len;
    if end > buf.len() {
        return Err(Violation {
            kind: ViolationKind::BytesMismatch,
            op_idx: Some(op_idx),
            message: format!(
                "destination range [{}, {}) out of bounds for dst_buffer len {}",
                dst_off,
                end,
                buf.len()
            ),
        });
    }

    for (i, b) in buf[dst_off..end].iter().enumerate() {
        if *b != expected {
            return Err(Violation {
                kind: ViolationKind::BytesMismatch,
                op_idx: Some(op_idx),
                message: format!(
                    "dst_buffer[{}] = {:#x}, expected {:#x} for peer {:?} (range {:?})",
                    dst_off + i,
                    *b,
                    expected,
                    peer,
                    dst_off..end
                ),
            });
        }
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// Saturating-fault contradictions
// ---------------------------------------------------------------------------

fn check_saturating_faults(wl: &Workload, outcome: &WorkloadOutcome) -> Result<(), Vec<Violation>> {
    let mut v: Vec<Violation> = Vec::new();

    let full_forward = wl.cfg_seed.forward_fault_rate_x10000 == 10_000;
    let full_disconnect = wl.cfg_seed.peer_disconnect_rate_x10000 == 10_000;

    // Peer-disconnect is rolled first in the mock, so it dominates
    // forward faults when both are 100%. Either way, an `Ok` outcome
    // contradicts the configuration.
    if full_forward || full_disconnect {
        for (i, r) in outcome.op_results.iter().enumerate() {
            if *r == OpResult::Ok {
                v.push(Violation {
                    kind: ViolationKind::UnexpectedSuccess,
                    op_idx: Some(i),
                    message: format!(
                        "op {} succeeded despite forward_fault_rate_x10000 = {}, peer_disconnect_rate_x10000 = {}",
                        i,
                        wl.cfg_seed.forward_fault_rate_x10000,
                        wl.cfg_seed.peer_disconnect_rate_x10000
                    ),
                });
            }
        }
    }

    // When only disconnect is 100%, every non-Ok must be
    // AddrLookupErr; under full_forward (without full_disconnect),
    // every non-Ok must be ForwardErr. Other terminal errors (e.g.
    // ShortReadErr) would imply the mock is misbehaving.
    if full_disconnect {
        for (i, r) in outcome.op_results.iter().enumerate() {
            match r {
                OpResult::Ok | OpResult::AddrLookupErr => {}
                other => v.push(Violation {
                    kind: ViolationKind::UnexpectedSuccess,
                    op_idx: Some(i),
                    message: format!(
                        "op {} produced {:?} despite peer_disconnect_rate_x10000 = 10000",
                        i, other
                    ),
                }),
            }
        }
    } else if full_forward {
        for (i, r) in outcome.op_results.iter().enumerate() {
            match r {
                OpResult::Ok | OpResult::ForwardErr => {}
                other => v.push(Violation {
                    kind: ViolationKind::UnexpectedSuccess,
                    op_idx: Some(i),
                    message: format!(
                        "op {} produced {:?} despite forward_fault_rate_x10000 = 10000",
                        i, other
                    ),
                }),
            }
        }
    }

    if v.is_empty() { Ok(()) } else { Err(v) }
}

// ---------------------------------------------------------------------------
// Effective-range derivation
// ---------------------------------------------------------------------------

/// Re-apply the clamps `spawn_op` performs on `(stripe_off, len,
/// page_idx, page_offset)` so the oracle can index the destination
/// buffer the same way the mock did. Mirrors `tests/mercury/workload.rs::spawn_op`;
/// if that function changes, this one must too.
fn effective_range(op: &Op, peer_data_len: u32) -> (usize, usize) {
    let peer_len = peer_data_len.max(1) as u64;
    let raw_len = op.len.max(1).min(DST_PAGE_SIZE) as u64;
    let len = raw_len.min(peer_len) as u32;
    let len = len.max(1);

    let page_idx = op.page_idx % DST_PAGE_COUNT;
    let page_offset = op.page_offset % DST_PAGE_SIZE;
    let max_page_off = DST_PAGE_SIZE.saturating_sub(len);
    let page_offset = if max_page_off == 0 {
        0
    } else {
        page_offset % (max_page_off + 1)
    };

    let dst_off = page_idx as usize * DST_PAGE_SIZE as usize + page_offset as usize;
    (dst_off, len as usize)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use crate::mercury::workload::{
        ClientSpec, Op, PeerIdSer, PeerSpec, Workload, deterministic_workload, empty_cfg_seed,
        run_workload,
    };

    #[test]
    fn audit_ok_for_no_op_workload() {
        let wl = Workload {
            clients: Vec::new(),
            peers: Vec::new(),
            ops: Vec::new(),
            cfg_seed: empty_cfg_seed(),
        };
        let outcome = run_workload(&wl);
        assert!(audit(&wl, &outcome).is_ok());
    }

    #[test]
    fn audit_ok_for_deterministic_workload() {
        let wl = deterministic_workload(0);
        let outcome = run_workload(&wl);
        match audit(&wl, &outcome) {
            Ok(()) => {}
            Err(violations) => {
                panic!("expected clean audit, got: {:#?}", violations);
            }
        }
    }

    #[test]
    fn audit_detects_bytes_mismatch_when_we_corrupt_outcome() {
        let wl = deterministic_workload(0);
        let mut outcome = run_workload(&wl);

        // Find an Ok op and corrupt its first destination byte to a
        // value the peer's reference byte cannot be.
        let (ok_idx, op) = wl
            .ops
            .iter()
            .enumerate()
            .find(|(i, _)| outcome.op_results[*i] == OpResult::Ok)
            .map(|(i, op)| (i, op.clone()))
            .expect("deterministic_workload(0) must produce at least one Ok op");
        let peer_idx = (op.peer_idx as usize) % wl.peers.len();
        let peer = wl.peers[peer_idx].id.to_peer();
        let (dst_off, _len) = effective_range(&op, wl.peers[peer_idx].data_len);
        outcome.dst_buffer[dst_off] = peer.0.wrapping_add(1) as u8;

        let violations = audit(&wl, &outcome).expect_err("corruption must surface a violation");
        let bytes_violations: Vec<_> = violations
            .iter()
            .filter(|v| v.kind == ViolationKind::BytesMismatch)
            .collect();
        assert_eq!(
            bytes_violations.len(),
            1,
            "expected exactly one BytesMismatch, got {:#?}",
            violations
        );
        assert_eq!(bytes_violations[0].op_idx, Some(ok_idx));
    }

    #[test]
    fn audit_detects_in_flight_leak() {
        let wl = deterministic_workload(0);
        let mut outcome = run_workload(&wl);
        outcome.counters.current_in_flight = 1;
        let violations = audit(&wl, &outcome).expect_err("in-flight leak must surface");
        assert!(
            violations
                .iter()
                .any(|v| v.kind == ViolationKind::InFlightLeak),
            "expected an InFlightLeak violation; got {:#?}",
            violations
        );
    }

    #[test]
    fn audit_detects_full_fault_workload_succeeding() {
        let mut cfg = empty_cfg_seed();
        cfg.forward_fault_rate_x10000 = 10_000;
        let wl = Workload {
            clients: vec![ClientSpec { id: 0 }],
            peers: vec![PeerSpec {
                id: PeerIdSer(2),
                data_len: 4096,
            }],
            ops: (0..3)
                .map(|i| Op {
                    client_idx: 0,
                    peer_idx: 0,
                    stripe_key_seed: 1,
                    stripe_off: 0,
                    len: 32,
                    page_idx: i,
                    page_offset: 0,
                })
                .collect(),
            cfg_seed: cfg,
        };
        let mut outcome = run_workload(&wl);
        // Every op should be ForwardErr; flip one to Ok to fake the
        // anomaly the oracle must catch.
        assert!(
            outcome
                .op_results
                .iter()
                .all(|r| matches!(r, OpResult::ForwardErr))
        );
        outcome.op_results[1] = OpResult::Ok;
        // Adjust counters so the only complaint is UnexpectedSuccess,
        // not a counter mismatch.
        outcome.counters.forwards_completed_ok = 1;
        outcome.counters.forwards_completed_err = outcome.counters.forwards_completed_err - 1;

        let violations =
            audit(&wl, &outcome).expect_err("flipped Ok under 100% forward fault must surface");
        assert!(
            violations
                .iter()
                .any(|v| v.kind == ViolationKind::UnexpectedSuccess && v.op_idx == Some(1)),
            "expected UnexpectedSuccess for op 1; got {:#?}",
            violations
        );
    }
}
