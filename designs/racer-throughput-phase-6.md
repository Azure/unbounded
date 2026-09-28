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

## Delivered implementation

- `topology/membership.rs` prepares the aligned rail-ID domain; `topology/rails.rs`
  hashes object/page with `racer/rail/v2`, independently of route and ETag. Every
  hop must carry the selected rail on the same fabric. Missing rails fall back to
  HTTP instead of changing the page's rail. Golden vectors and alternate/reverse
  route tests enforce this contract. A change to the published rail domain can
  change assignment; this is not a permanent placement guarantee across epochs.
- `app_native.rs` records pinned crypto NUMA locality and activates only matching,
  funded rails. Unknown production NUMA locality selects HTTP. Retained activation
  futures retry with bounded deadlines/backoff after mappings change or activation
  fails. Application refresh closes incompatible generations.
- `rdma/lifecycle.rs` reopens only after native destruction, final I/O lease release,
  and retained backend quota release. Old device handles carry a generation and
  cannot allocate a QP in a new generation. Failed fences retain quarantine;
  failed replenishment backs off. Tests hold both QP and region owners across close,
  inject failed stops, and prove old handles cannot create new capabilities.
- Native auto-selection requires the `rdma` feature and trusted physical
  associations. Explicit false and Node alignment disable remain authoritative.
  Shared admission counters are cache-line aligned.
- Final Phase 5 review added a Go-produced shared delta vector consumed by Rust,
  tamper/replay assertions, monotonic checkpoint sequences recovered from disk,
  correct alternating checkpoint slots after restart, and strict TLS buffered-body
  checks before connection reuse.

## Integrated verification

All test commands used `timeout --signal=TERM --kill-after=10s 300s`; Go also used
`-timeout=5m`. No external timeout fired. Commands below ran from the crate unless
they name Go or make targets, which ran from the worktree root.

| Command / group | Result |
| --- | --- |
| `cargo check --locked --all-features --all-targets -j2` | Passed |
| `cargo test --locked --all-features --lib -j2 -- --test-threads=2 --quiet` | 767 passed, 7 explicit ignores |
| `cargo test --locked --all-features --test process_restart -j2 -- --ignored --test-threads=1 --quiet` with `CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_RUNNER='sudo -n'` | 15 passed, actual io_uring/processes |
| `cargo test --locked --all-features --test production_dataplane --test client_origin_conformance -j2 -- --test-threads=2 --quiet` | 13 + 19 passed; 2 + 1 explicit ignores |
| `cargo test --locked --all-features --doc -j2 -- --test-threads=2 --quiet` | 32 passed |
| Final changed TLS group: `cargo test --locked --all-features --lib control::transport::tests -j2 -- --test-threads=2 --quiet` | 4 passed, including new buffered-trailing-body rejection |
| `go test -timeout=5m ./internal/racer/... ./deploy/racer ./pkg/racersdk ./internal/gantry/racer` | Passed |
| `go test -timeout=5m -race ./pkg/racersdk ./internal/gantry/racer ./internal/racer/...` | Passed |
| `go test -timeout=5m ./internal/operator/components/racer/...` | Passed |
| `cargo fmt` and scoped `make fmt` using golangci-lint v2.13.1 | Passed, Go lint reported zero issues |
| `cargo clippy --locked --all-features --all-targets -j2 -- -D warnings` | Failed: 94 library diagnostics, 119 including test diagnostics; style/type-size findings remain |

The complete library run preceded the final TLS guard/test; the focused group
verified that final change. Strict Clippy is not a passing gate and no blanket
suppression was added. See earlier phase reports for measured HTTP throughput;
these final correctness runs are not additional performance measurements.

`make -C native` compiled the real verbs adapter with `-Wall -Wextra -Werror`.
Calling its discovery ABI returned ABI 2 and **zero eligible ports**. Consequently
the real-provider transfer test was not run. Simulated fence/restart tests do not
establish NIC throughput, multi-host behavior, GB200 isolation, or NUMA bandwidth.

## Remaining limits against the canonical objective

These remain explicit gaps, not delivered throughput guarantees:

1. Worker page ownership stays stable and rail-independent. A worker activates a
   funded local subset; a page whose selected rail is elsewhere uses HTTP. There
   is no cross-worker native transfer dispatcher or guarantee that every selected
   rail has funded capacity on every page owner. Registered allocations are
   first-touched on pinned crypto workers, not enforced with a strict NUMA memory
   policy. Multi-rail hardware validation is required before claiming full GB200
   aligned-rail throughput.
2. QPs remain single-use and fenced. Native staging copies and authenticated setup
   rounds remain. No unmeasured pooled-QP or zero-copy speedup is claimed.
3. Five-second periodic checkpoints are best-effort cache recovery hints. Writes
   pause during a checkpoint; metadata validation/sorting and some snapshot work
   remain synchronous. Pressure or repeated checkpoint failure can extend crash
   loss indefinitely. There is no fsync or power-loss durability promise, nor an
   enforced small maximum number of lost pages under arbitrary load.
4. The 100k topology/ranking tests and small-cluster actual process gates do not
   establish sustained 100k-member update latency or distributed cluster goodput.
   Cold exact rendezvous still scans membership on cache misses.
5. Origin reception still uses owned io_uring buffers rather than an all-UDS splice
   pipeline. SDK direct-splice source connections close instead of reentering the
   ordinary HTTP pool. CRC is an additional pass rather than fused with AEAD.

Peer v4 requires coordinated upgrade; records write v2 and read v1/v2; negotiated
control deltas extend control v1 with full-snapshot fallback. These compatibility
requirements apply to the merged throughput branch as a whole.
