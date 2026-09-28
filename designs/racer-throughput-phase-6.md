# Racer throughput architecture: Phase 6

## Implementation plan

Continue directly on the throughput worktree, without subagents. Preserve the
existing pinned I/O/crypto pairs, exclusive socket ownership, immutable page
leases, single-use remote capabilities, and terminal DMA fences.

1. Prepare a canonical rail domain once per immutable membership. Hash object and
   page identity independently of route and ETag, keeping metadata/page-zero
   ownership compatible. A route missing the selected rail uses HTTP, never a
   different rail. Test alternate/reverse routes and heterogeneous mappings.
2. Select native rails compatible with each pinned worker pair's actual NUMA
   locality. Bound slot provisioning by funded capacity and assigned rails rather
   than activating every node rail on every worker. Preserve HTTP progress when
   native capacity or hardware is unavailable. Do not silently exceed registered
   memory or the thread cap.
3. Add generation-safe native restart after close. Reopening requires native
   teardown completion and release of every old slot lease. Retry provisioning
   and QP replenishment with bounded backoff, retaining failed-fence quarantine.
   Mapping changes must stop old capability creation before new activation.
4. Enable native auto-selection only when the build includes libibverbs and trusted
   physical associations are available. Explicit disable and the default-enabled
   Node alignment annotation remain authoritative. Report HTTP fallback honestly.
5. Keep shared counters cache-line aligned. Measure native setup/staging only with
   a valid provider; do not remove required control proofs or reuse QPs without
   protocol/lifetime evidence.
6. Run focused route/lifecycle tests including held old leases and failed fences,
   then the integrated Rust, Go, strict process, formatting and lint checks.
   Validate provider availability before hardware tests. Record unmeasured
   multi-host/GB200 throughput explicitly rather than treating simulation as proof.

## Review boundaries

The existing `topology/rails.rs` chooses from a route intersection, so alternate
paths can change a page's rail. `app_native.rs` activates every local rail on each
worker. `rdma/lifecycle.rs` closes permanently and retires replenishment failures.
These production paths are the primary changes, not a replacement transport.

Canonical authority remains `/home/azureuser/design.md`. Tests and edits use the
worktree's mandatory external timeout and formatting rules. Full final review
also closes Phase 5 interoperability and persistence-bound issues.
