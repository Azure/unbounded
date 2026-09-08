// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Storage-engine DST property tests.

use proptest::prelude::*;

use crate::storage::oracle::Oracle;
use crate::storage::workload::{
    ClientSpec, Op, Outcome, RunReport, Workload, run_workload, workload_strategy,
    workload_strategy_no_faults_multi_client,
};

proptest! {
    #![proptest_config(ProptestConfig {
        cases: 128,
        ..ProptestConfig::default()
    })]

    /// Invariant: bounded termination.
    /// `run_workload` returning `Ok` implies the executor neither
    /// deadlocked nor exhausted its step budget. Re-asserting it
    /// explicitly pins the contract.
    #[test]
    fn invariant_bounded_termination(seed in any::<u64>(), w in workload_strategy()) {
        let report = run_workload(seed, w).expect("run completed without deadlock or budget exhaustion");
        prop_assert!(report.steps > 0);
    }

    /// Invariant: every `ReadHit` returns bytes that the workload
    /// previously wrote for that `(key, offset)`. Catches silent
    /// data corruption, mis-routed reads, and cross-page bleed.
    #[test]
    fn invariant_hit_bytes_were_written(seed in any::<u64>(), w in workload_strategy()) {
        // Rebuild the oracle alongside the run so the assertion is
        // independent of the harness internals.
        let oracle = Oracle::new();
        for c in &w.clients {
            for op in &c.ops {
                if let Op::Write { key_idx, off_idx, payload_seed } = op {
                    oracle.record_write(
                        w.key(*key_idx),
                        w.offset(*off_idx),
                        w.payload(*key_idx, *off_idx, *payload_seed),
                    );
                }
            }
        }
        let faults_enabled = w.io_fault_rate > 0 || w.read_corrupt_rate > 0;
        let report = run_workload(seed, w).expect("run completed");
        for (i, o) in report.outcomes.iter().enumerate() {
            match o {
                Outcome::ReadHit { key, offset, bytes } => {
                    prop_assert!(
                        oracle.allows_read(*key, *offset, bytes),
                        "client op {} returned a hit with bytes not previously written for ({:?}, {})",
                        i, key.0, offset,
                    );
                }
                Outcome::Err(e) => {
                    prop_assert!(
                        faults_enabled,
                        "op {} produced an error under happy-path config: {}", i, e,
                    );
                }
                _ => {}
            }
        }
    }

    /// Invariant: read corruption never produces a false hit. The
    /// sim device may flip the first byte of any successful read
    /// (data pages, btree internals, anything the engine reads via
    /// the `BlockDevice` trait); the engine's per-page xxh3
    /// checksum must convert a corrupted data-page read into a
    /// miss. A `ReadHit` whose bytes do not match what the
    /// workload wrote would be a silent data corruption bug.
    ///
    /// We only assert the false-hit property here. Whether the
    /// engine's `checksum_misses` counter fires depends on which
    /// device read happened to be corrupted (data path vs btree
    /// internals); pinning that down belongs in a more targeted
    /// test, not this proptest.
    #[test]
    fn invariant_no_false_hit_under_corruption(seed in any::<u64>(), w in workload_strategy()) {
        let oracle = Oracle::new();
        for c in &w.clients {
            for op in &c.ops {
                if let Op::Write { key_idx, off_idx, payload_seed } = op {
                    oracle.record_write(
                        w.key(*key_idx),
                        w.offset(*off_idx),
                        w.payload(*key_idx, *off_idx, *payload_seed),
                    );
                }
            }
        }
        let report = run_workload(seed, w).expect("run completed");
        for (i, o) in report.outcomes.iter().enumerate() {
            if let Outcome::ReadHit { key, offset, bytes } = o {
                prop_assert!(
                    oracle.allows_read(*key, *offset, bytes),
                    "op {} returned a corrupted hit for ({:?}, {}); first byte {:#x}",
                    i, key.0, offset, bytes.first().copied().unwrap_or(0),
                );
            }
        }
    }

    /// Invariant: a read for a `(key, offset)` the workload never
    /// wrote must miss. This is the dual of the byte-correctness
    /// invariant and catches false-positive hit reports.
    #[test]
    fn invariant_unwritten_keys_miss(seed in any::<u64>(), w in workload_strategy()) {
        let oracle = Oracle::new();
        for c in &w.clients {
            for op in &c.ops {
                if let Op::Write { key_idx, off_idx, payload_seed } = op {
                    oracle.record_write(
                        w.key(*key_idx),
                        w.offset(*off_idx),
                        w.payload(*key_idx, *off_idx, *payload_seed),
                    );
                }
            }
        }
        let report = run_workload(seed, w).expect("run completed");
        for (i, o) in report.outcomes.iter().enumerate() {
            if let Outcome::ReadHit { key, offset, .. } = o {
                prop_assert!(
                    oracle.was_written(*key, *offset),
                    "op {} returned a hit for never-written ({:?}, {})",
                    i, key.0, offset,
                );
            }
        }
    }

    /// Invariant: snapshot accounting is self-consistent.
    /// - `hits + misses` cannot exceed the number of `read_page`
    ///   calls in the workload.
    /// - `admitted + rejected_by_filter` cannot exceed the number
    ///   of `write_page` calls in the workload (singleflight
    ///   followers and write-I/O errors contribute the slack).
    /// - The gap between `resident_pages` and `btree_entries` is
    ///   bounded even under fault injection. With no faults the
    ///   counters must agree exactly: a regression that orphans
    ///   cache entries on overwrite (no `replace-LBA` reclaim in
    ///   `write_page`) breaks the equality immediately. Under
    ///   faults two paths can desynchronize them:
    ///   1. A failed eviction batch. `evict_if_over_watermark`
    ///      pops up to `EVICT_SWEEP_TARGET` victims from the LRU
    ///      and then attempts a batched btree delete; if that
    ///      `apply_batch` fails (surfaced as a `device_io_errors`
    ///      bump from the btree's underlying writes) the victims
    ///      are gone from the LRU but still live in the btree, so
    ///      `btree_entries` exceeds `resident_pages` by up to
    ///      `EVICT_SWEEP_TARGET` per failure.
    ///   2. A corrupted btree-internal read of the prior-LBA
    ///      probe in the mutator's `process_batch`. A flipped
    ///      byte that makes `btree::lookup` return `Ok(None)`
    ///      when a prior entry existed causes the engine to skip
    ///      `retire_range(old)`; the new LBA is admitted to both
    ///      sides but the old LBA stays in the LRU and `reverse`
    ///      map even though its btree key was overwritten by the
    ///      new insert. That orphans one LRU entry per
    ///      corruption, so `resident_pages` can exceed
    ///      `btree_entries` by up to `device_corruptions_injected`.
    ///      A corruption that hits the path-copy descent inside
    ///      `apply_batch` itself aborts the commit (the engine's
    ///      `apply_node` surfaces `Decoded::Empty` as
    ///      `Error::Corrupt` so a subtree is never silently
    ///      dropped from the new tree), so it doesn't contribute
    ///      to the gap.
    ///   The data-write failure paths in `write_page_from` either
    ///   rewind both sides or touch neither, so they don't
    ///   contribute. `pending_free_len` is added as a small
    ///   additional slack: it tracks LBAs already detached from
    ///   the LRU whose btree key was replaced by a newer LBA, so
    ///   on its own it doesn't unbalance the counters, but it
    ///   gives defensive headroom for transient bookkeeping
    ///   races without weakening the bound to uselessness.
    #[test]
    fn invariant_snapshot_accounting(seed in any::<u64>(), w in workload_strategy()) {
        // Mirrors the hardcoded sweep target in
        // `StorageEngine::evict_if_over_watermark`; each failed
        // eviction batch can orphan at most this many LRU entries
        // while leaving them resident in the btree.
        const EVICT_SWEEP_TARGET: i64 = 8;

        let mut writes = 0u64;
        let mut reads = 0u64;
        for c in &w.clients {
            for op in &c.ops {
                match op {
                    Op::Write { .. } => writes += 1,
                    Op::Read { .. } => reads += 1,
                }
            }
        }
        let faults_enabled = w.io_fault_rate > 0 || w.read_corrupt_rate > 0;
        let report = run_workload(seed, w).expect("run completed");
        prop_assert!(
            report.hits + report.misses <= reads,
            "hits ({}) + misses ({}) > read calls ({})",
            report.hits, report.misses, reads,
        );
        prop_assert!(
            report.admitted + report.rejected_by_filter <= writes,
            "admitted ({}) + rejected ({}) > write calls ({})",
            report.admitted, report.rejected_by_filter, writes,
        );

        let resident = report.resident_pages as i64;
        let btree = report.btree_entries as i64;
        let diff = (resident - btree).abs();
        let bound = EVICT_SWEEP_TARGET * report.device_io_errors as i64
            + report.device_corruptions_injected as i64
            + report.pending_free_len as i64;
        prop_assert!(
            diff <= bound,
            "|resident_pages ({}) - btree_entries ({})| = {} exceeds bound {} \
             (device_io_errors={}, device_corruptions_injected={}, pending_free_len={}); \
             LRU and index diverged beyond what failed evictions and corrupted btree \
             reads can explain",
            resident, btree, diff, bound,
            report.device_io_errors, report.device_corruptions_injected,
            report.pending_free_len,
        );

        if !faults_enabled {
            // Strictest sub-assertion: with no fault injection
            // the bound collapses to zero (no device_io_errors,
            // pending_free drains opportunistically), so the
            // counters must agree exactly. Asserted separately so
            // a regression that breaks the happy path produces a
            // crisp shrink message instead of a "diff <= 0" form.
            prop_assert_eq!(
                report.resident_pages, report.btree_entries,
                "resident_pages ({}) != btree_entries ({}); LRU and index disagree \
                 under fault-free config",
                report.resident_pages, report.btree_entries,
            );
        }
    }

    /// Invariant: a clean restart preserves correctness.
    /// After the engine is dropped and reopened on the same backing
    /// device, replaying read-only over every `(key, offset)` in
    /// the grid must satisfy two contracts:
    /// 1. Every post-restart `ReadHit` returns bytes the workload
    ///    previously wrote for that key. The engine is free to
    ///    miss (eviction / rebuild best-effort) but never to
    ///    return wrong bytes.
    /// 2. No post-restart `Err`. Replay runs with faults and
    ///    corruption disabled, so any I/O error indicates the
    ///    rebuild path itself misbehaved.
    /// We do NOT assert that pre-restart hits remain hits after
    /// restart: the design permits eviction and partial rebuild,
    /// so admitted-and-cached pages may legitimately miss after
    /// reopen.
    #[test]
    fn invariant_restart_preserves_correctness(seed in any::<u64>(), w in workload_strategy()) {
        let oracle = Oracle::new();
        for c in &w.clients {
            for op in &c.ops {
                if let Op::Write { key_idx, off_idx, payload_seed } = op {
                    oracle.record_write(
                        w.key(*key_idx),
                        w.offset(*off_idx),
                        w.payload(*key_idx, *off_idx, *payload_seed),
                    );
                }
            }
        }
        let restart_was_requested = w.restart_after;
        let key_count = w.key_count.max(1) as u64;
        let off_count = w.offset_count.max(1) as u64;
        let report = run_workload(seed, w).expect("run completed");
        if !restart_was_requested {
            // Nothing more to check; replay did not run.
            prop_assert!(!report.restart_performed);
            prop_assert_eq!(report.post_restart_hits, 0);
            prop_assert_eq!(report.post_restart_misses, 0);
            prop_assert_eq!(report.post_restart_errors, 0);
            return Ok(());
        }
        // If restart_after was true, the pre-restart open may
        // still have aborted under fault injection, in which case
        // restart_performed is false. That's a no-op path; nothing
        // to assert about replay outcomes.
        if !report.restart_performed {
            prop_assert_eq!(report.post_restart_hits, 0);
            prop_assert_eq!(report.post_restart_misses, 0);
            prop_assert_eq!(report.post_restart_errors, 0);
            return Ok(());
        }
        prop_assert_eq!(
            report.post_restart_errors, 0,
            "restart replay produced {} error(s) with faults disabled",
            report.post_restart_errors,
        );
        let grid = key_count * off_count;
        prop_assert_eq!(
            report.post_restart_hits + report.post_restart_misses, grid,
            "post-restart replay should visit every grid cell exactly once: hits={} misses={} grid={}",
            report.post_restart_hits, report.post_restart_misses, grid,
        );
        // Spot-check: the bytes-were-written invariant already
        // covers every ReadHit in `report.outcomes`, including the
        // appended replay outcomes, but re-asserting here keeps
        // the restart invariant self-contained and gives a clearer
        // shrink message when this is the failure mode.
        for (i, o) in report.outcomes.iter().enumerate() {
            if let Outcome::ReadHit { key, offset, bytes } = o {
                prop_assert!(
                    oracle.allows_read(*key, *offset, bytes),
                    "op {} (incl. restart replay) returned a hit with bytes not previously written for ({:?}, {})",
                    i, key.0, offset,
                );
            }
        }
    }

    /// Invariant: when the workload routes through `LocalStorage`
    /// over `num_disks >= 2`, the `disk_for` hash actually spreads
    /// device writes across more than one disk under any
    /// non-trivial workload. A regression that ignored either the
    /// stripe key or the page index (collapsing the hash to a
    /// constant) would funnel every write to disk 0; this catches
    /// that without depending on the routing function's own unit
    /// tests.
    ///
    /// Scoped tightly: requires `num_disks >= 2` and at least eight
    /// device writes in aggregate. Below that threshold a perfectly
    /// healthy hash can legitimately land every write on a single
    /// disk by chance, and we'd be testing the PRNG rather than
    /// the router.
    #[test]
    fn invariant_multidisk_routing_diverse(seed in any::<u64>(), w in workload_strategy()) {
        let report = run_workload(seed, w).expect("run completed");
        // Scope guards: this invariant is only meaningful when the
        // router has more than one disk and the workload issued
        // enough device writes that a healthy hash should land on
        // at least two of them. Use early returns instead of
        // `prop_assume!` so workloads outside the scope do not
        // count against proptest's global reject budget; otherwise
        // the strategy's mix of `num_disks=1` and short op
        // sequences exhausts the budget long before 128 cases run.
        if report.num_disks_used < 2 || report.device_writes < 8 {
            return Ok(());
        }
        let touched = report
            .device_writes_per_disk
            .iter()
            .filter(|n| **n > 0)
            .count();
        prop_assert!(
            touched >= 2,
            "all {} device writes routed to a single disk across {} disks: {:?}",
            report.device_writes, report.num_disks_used, report.device_writes_per_disk,
        );
    }

    /// Invariant: per-disk residency never exceeds the per-disk
    /// page budget. The SIEVE LRU is supposed to evict at ~90%
    /// utilization; a regression that fired the watermark too
    /// late (or never) would let `resident_pages` overshoot the
    /// configured `device_pages` on some disk. We assert the
    /// hard upper bound (full capacity), not the watermark
    /// itself, because the watermark is a policy knob the test
    /// should not pin.
    #[test]
    fn invariant_resident_pages_within_capacity(seed in any::<u64>(), w in workload_strategy()) {
        let report = run_workload(seed, w).expect("run completed");
        for (i, n) in report.resident_pages_per_disk.iter().enumerate() {
            prop_assert!(
                (*n as u64) <= report.device_pages,
                "disk {} resident_pages={} exceeds device_pages={}",
                i, n, report.device_pages,
            );
        }
    }

    /// Invariant: at end-of-run quiescence the deferred-reclaim
    /// queue is bounded. Every LBA pushed onto `pending_free`
    /// (either by an overwrite-while-pinned or an eviction-while-
    /// pinned) is reclaimed opportunistically on subsequent
    /// writer ops; entries that survive the run reflect a
    /// retire-then-no-more-writes tail and stay bounded by the
    /// number of writes the workload issued. A regression that
    /// dropped the drain path entirely would let the queue grow
    /// unboundedly with the workload's read fanout, not just
    /// its writes.
    ///
    /// Skipped under fault injection: a failing write or a
    /// checksum-driven retry can leave entries on the queue
    /// when the surrounding op returns an error without
    /// completing the drain handshake.
    #[test]
    fn invariant_pending_free_drains(seed in any::<u64>(), w in workload_strategy()) {
        let faults_enabled = w.io_fault_rate > 0 || w.read_corrupt_rate > 0;
        let mut writes = 0usize;
        for c in &w.clients {
            for op in &c.ops {
                if matches!(op, Op::Write { .. }) {
                    writes += 1;
                }
            }
        }
        let report = run_workload(seed, w).expect("run completed");
        if faults_enabled {
            return Ok(());
        }
        prop_assert!(
            report.pending_free_len <= writes,
            "pending_free queue len={} exceeds bound (writes={}): drain path may be stuck",
            report.pending_free_len, writes,
        );
    }

    /// Invariant: per-disk `max_inflight` is always within
    /// sane bounds. The engine never issues more than a
    /// handful of independent device ops at once (the mutator
    /// is a single consumer, writers serialize through
    /// singleflight per key, reads can fan out but are
    /// bounded by the number of live client tasks), so a
    /// runaway value would indicate either a counter
    /// regression in `SimBlockDevice` or a path that lost
    /// track of its `await` boundaries. The hard upper bound
    /// is generous (`clients + 4` slack for the mutator and
    /// supervisor tasks); the value of this invariant is
    /// shape-checking, not pinning a tight number.
    ///
    /// A stronger "overlap actually happens" check would have
    /// to be a sweep-level invariant (proptest has no
    /// sweep-end hook), so the dedicated `smoke_concurrent_
    /// reads_overlap` test below carries that property
    /// explicitly with a hand-crafted multi-client read
    /// workload that the engine cannot serialize.
    #[test]
    fn invariant_max_inflight_bounded(seed in any::<u64>(), w in workload_strategy()) {
        let client_count = w.clients.len() as u32;
        let report = run_workload(seed, w).expect("run completed");
        let bound = client_count + 4;
        for (i, n) in report.max_inflight_per_disk.iter().enumerate() {
            prop_assert!(
                *n <= bound,
                "disk {} max_inflight={} exceeds plausible bound {} (clients={})",
                i, n, bound, client_count,
            );
        }
    }

    /// Invariant: every engine's mutator queue drains at
    /// shutdown. The harness closes each queue after the last
    /// client task finishes and then joins `run_mutator`; once
    /// joined the queue must be empty. A non-zero `pending_len`
    /// would mean the consumer exited while requests were still
    /// buffered, which would lose committed btree state and
    /// leak `done` notifications back to producers.
    #[test]
    fn invariant_mutator_drains_at_shutdown(
        seed in any::<u64>(), w in workload_strategy(),
    ) {
        let report = run_workload(seed, w).expect("run completed");
        // `mutator_pending_per_disk` is empty only when bootstrap
        // failed and no engines were opened; in that case there
        // is nothing to drain.
        for (i, n) in report.mutator_pending_per_disk.iter().enumerate() {
            prop_assert_eq!(
                *n, 0,
                "engine {} exited with {} mutator request(s) still buffered after shutdown",
                i, n,
            );
        }
    }

    /// Invariant: no committed write is silently lost when
    /// multiple clients write the same `(key, offset)`
    /// concurrently.
    ///
    /// The harness already enforces "no wrong bytes on a hit"
    /// via [`invariant_hit_bytes_were_written`] and the oracle.
    /// What this check adds is an end-of-run sanity probe: a
    /// regression that let the mutator drop one of two coalesced
    /// inserts (singleflight follower vs leader) without
    /// committing either would manifest as the engine reporting
    /// `admitted > 0` while every read missed and `btree_entries`
    /// stayed at zero. That precise shape is what we look for
    /// here. We don't directly cross-reference per-write commit
    /// receipts because the mutator does not surface per-request
    /// commit counters to the harness today; documenting the gap
    /// (see comment) keeps the intent explicit for the next
    /// reader.
    ///
    /// Scoped tightly to happy-path runs: under fault injection
    /// admissions can legitimately survive without a backing
    /// btree entry (the data-page write failed after admission
    /// bumped its counter), so this exact shape is only a bug
    /// in the fault-free regime.
    #[test]
    fn invariant_no_lost_writes_under_concurrent_writers(
        seed in any::<u64>(), w in workload_strategy_no_faults_multi_client(),
    ) {
        // Strategy already pins `io_fault_rate == 0`,
        // `read_corrupt_rate == 0`, and `clients.len() >= 2`, so we
        // don't need a `prop_assume!` gate here. Doing the filtering
        // in the strategy keeps soak runs (high `PROPTEST_CASES`)
        // from tripping `max_global_rejects`.
        debug_assert_eq!(w.io_fault_rate, 0);
        debug_assert_eq!(w.read_corrupt_rate, 0);
        debug_assert!(w.clients.len() >= 2);
        let report = run_workload(seed, w).expect("run completed");
        if report.admitted == 0 {
            return Ok(());
        }
        // At least one admitted write must have produced an
        // observable btree entry. (Coalesced followers don't
        // each bump `btree_entries`, so we cannot equate the
        // two counts; we only assert the non-zero floor.)
        prop_assert!(
            report.btree_entries >= 1,
            "{} admissions but zero btree entries: every write was lost between \
             admission and commit (admitted={}, evictions={}, write_io_errors={})",
            report.admitted, report.admitted, report.evictions, report.write_io_errors,
        );
    }
}

/// Smoke scenario: hand-tuned workload that exercises the write -> read
/// roundtrip path under modest delay, with no faults.
#[test]
fn smoke() {
    let w = Workload {
        page_size: 4096,
        device_pages: 64,
        max_io_delay: 2,
        io_fault_rate: 0,
        read_corrupt_rate: 0,
        key_count: 2,
        offset_count: 2,
        clients: vec![
            ClientSpec {
                ops: vec![
                    Op::Write {
                        key_idx: 0,
                        off_idx: 0,
                        payload_seed: 1,
                    },
                    Op::Write {
                        key_idx: 0,
                        off_idx: 0,
                        payload_seed: 1,
                    },
                    Op::Read {
                        key_idx: 0,
                        off_idx: 0,
                    },
                ],
            },
            ClientSpec {
                ops: vec![
                    Op::Write {
                        key_idx: 1,
                        off_idx: 1,
                        payload_seed: 9,
                    },
                    Op::Write {
                        key_idx: 1,
                        off_idx: 1,
                        payload_seed: 9,
                    },
                    Op::Read {
                        key_idx: 1,
                        off_idx: 1,
                    },
                ],
            },
        ],
        restart_after: false,
        num_disks: 1,
    };
    let report: RunReport = run_workload(0xC0FFEE, w).expect("smoke run");
    // At least one of the two reads should hit; admission filter
    // requires the second write before anything lands.
    let hits: usize = report
        .outcomes
        .iter()
        .filter(|o| matches!(o, Outcome::ReadHit { .. }))
        .count();
    assert!(
        hits >= 1,
        "expected at least one hit after double-write: {:?}",
        report.outcomes
    );
}

/// Sweep-complement to [`invariant_max_inflight_bounded`]: a
/// hand-crafted workload that the engine cannot serialize,
/// proving the `SimBlockDevice` inflight counter actually
/// observes overlap when overlap is geometrically forced.
///
/// Several clients each admit one distinct key and then issue many
/// reads for that key with a generous `max_io_delay`. Reads are not
/// singleflight-gated and (unlike writes) do not funnel through the
/// mutator, so the executor can interleave device reads from
/// independent client tasks freely; with per-op `yield_n(delay)` and
/// the executor's random task pick, at least one engine must see two
/// device ops in flight at once. If this test ever observes peak == 1,
/// `SimBlockDevice` has stopped yielding through `await` or
/// the engine has acquired a global I/O lock that defeats
/// the per-disk concurrency the rest of the harness assumes.
#[test]
fn smoke_concurrent_reads_overlap() {
    // Each client first writes one distinct key twice so admission
    // promotes it, then repeatedly reads that key. The per-client
    // prefix avoids relying on a separate primer client completing
    // before readers start; after the prefixes, reads cannot be
    // collapsed and are numerous enough to force overlap under the
    // deterministic executor.
    let mut clients = Vec::new();
    let key_count = 16u8;
    for k in 0..key_count {
        let mut ops = Vec::new();
        ops.push(Op::Write {
            key_idx: k,
            off_idx: 0,
            payload_seed: k,
        });
        ops.push(Op::Write {
            key_idx: k,
            off_idx: 0,
            payload_seed: k,
        });
        for _ in 0..16 {
            ops.push(Op::Read {
                key_idx: k,
                off_idx: 0,
            });
        }
        clients.push(ClientSpec { ops });
    }
    let w = Workload {
        page_size: 4096,
        device_pages: 256,
        max_io_delay: 32,
        io_fault_rate: 0,
        read_corrupt_rate: 0,
        key_count,
        offset_count: 1,
        clients,
        restart_after: false,
        num_disks: 1,
    };
    let report = run_workload(0xDEADBEEF, w).expect("smoke run");
    let peak = report
        .max_inflight_per_disk
        .iter()
        .copied()
        .max()
        .unwrap_or(0);
    assert!(
        peak >= 2,
        "engine never saw overlapping device ops despite 16 concurrent readers over 16 \
         admitted keys with max_io_delay=32: per-disk peaks={:?}, device_reads={}, \
         device_writes={}",
        report.max_inflight_per_disk,
        report.device_reads,
        report.device_writes,
    );
}
