# Racer throughput architecture: Phase 2

## Authority and acceptance

This phase implements the approved progress and bounded-hot-path changes on
`racer-throughput-architecture`. The user-approved scope and canonical
`/home/azureuser/design.md:5-32,43-69` authorize cross-package changes: bounded
worker-local resources, shared fills, alternate routes, rotation, and circuit
breakers. Phase 1 is committed at `62ff37b5`. Parent integration owns merging.
No dependencies, destructive migrations, or test removals are planned.

## Inspected evidence

Paths below are relative to `cmd/racer-dataplane`.

- `src/read/fill.rs:487-513` reserves a generic network fill before local lookup;
  `src/store/reader.rs:110-155` separately owns padded staging and decoded bytes.
  `src/runtime/admission.rs:123-133` permits only a single-page fairness floor.
  These conflict with the required complete progress working set. Resolve in
  favor of source-specific admission, retaining global bounds.
- `src/memory/pool.rs:63-104` retains upper-bound reservations after exact-sized
  allocation. Its test at lines 231-248 explicitly expects an eight-byte charge
  for three bytes. Preserve lifetime/provenance assertions but require exact
  allocated-capacity accounting instead.
- `src/control/client.rs:362-365` bypasses all remote progress while publication
  is pending; lines 464-487 clone and validate the full publication repeatedly.
  Resolve in favor of independent bounded duties and prepare-once publication.
- Phase 1's strict disk diagnostic intentionally fails
  (`designs/racer-throughput-phase-1.md:145,159-163`). Convert its permanent
  assertion to successful verified disk progress, keeping the strict command.

## Reviewable implementation sequence

1. Commit this plan before source changes.
2. Admission: select pending/disk/network sources before reserving; atomically
   acquire each source's overlapping plaintext, staging, decoded ciphertext and
   optional dirty obligations before asynchronous I/O. Size the bounded fairness
   floor for the complete working set, without increasing global budgets.
   Shrink only uniquely owned reservations to actual live allocation capacity.
   Verify rollback, invalid shrink, multiple-cache progress, and small objects.
3. Connections: provide explicit ingress/outbound/control classes and bounded
   reserved progress capacity. Transfer charge ownership to socket/kernel
   operation lifetime, including cancellation. Expose this API for Phase 3's
   ingress distribution.
4. Failure progress: share worker-local LinkHealth between routing and actual
   immediate-hop transfers; report transport outcomes, bound half-open probes,
   prune retired members, and keep application misses/401/403 separate. Preserve
   request deadline, attempt/hop accounting and cancellation fences. Apply bounded
   endpoint breakers to origin and control. Add actual failed-hop alternate-route
   coverage rather than only candidate fallback.
5. Control: service projection reload, renewal, pending installation and polling
   independently with bounded turns/backoff. Retain immutable prepared pending
   state; newer authenticated publications may supersede blocked ones. Test key
   rotation and supersession while listener preparation is blocked.
6. Security/hot paths: validate immutable identity/trust epochs outside locks;
   retain signature/session checks; cache exact peer chains against trust epoch
   and time validity with bounded capacity. Use indexed key lookup where useful.
   Avoid committed cache-definition clones and poll health periodically.
7. Peer pools: binary-search membership endpoints, autonomously expire idle
   connections with bounded per-turn work. Correct related 24-hour activation
   cadence if the control implementation requires it; run `make fmt` for Go.
8. Verification and final documentation: `cargo fmt`, applicable focused tests,
   all-feature compilation/tests, explicitly selected real io_uring executable
   suite, strict multicache disk gate and strict small-object churn. Record exact
   commands/results, proven properties, Phase 3 APIs, and concrete blockers.

Before each incremental commit inspect `git status`, `git diff`, and
`git log --oneline -10`; stage only intended files. All scratch/build artifacts
stay in this worktree. Final status must be clean. Tests that formerly encoded
baseline failures must assert the fixes. Hardware/prerequisite failures are
reported as failures or blockers, never successful acceptance.

## Results

Implementation and exact verification results will be recorded here in the final
documentation commit.
