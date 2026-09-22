# DST migration and retained boundaries

The artifact adapter is the complete-journal boundary. Baseline, native, and
scale runners retain executable/source metadata and logs, but do not promote
every selected test to exact replay. The inventory in each run identifies the
actual selectors. Passing the required artifact matrix establishes its declared
cells, not parity with all legacy fixtures.

## Retained fixtures and retirement gates

| Fixture | Owner | Reason retained | Required evidence before retirement |
| --- | --- | --- | --- |
| `tests/storage/recovery.rs` allocator `Sim` and `Model` | dataplane/storage | Independent logical snapshots, both retained checkpoint roots, and finite crash cases exceed the cluster actor's durable-object witness. | Run identical operation/crash transcripts against both models; preserve `assert_recovery` checks of both roots and all finite cases before replacing the environment. |
| `tests/runtime/scenarios.rs` targeted fixtures | dataplane/runtime | Custom placement, physical workers, and fault histories have assertions beyond the canonical artifact topology. | Port each scenario with its original path and resource assertions, exact success/failure replay, and an explicit independent replacement for every changed routing obligation. |
| `simulation::World` unmanaged progression | dataplane/environment | Component and hybrid tests still use explicit progression or direct completion calls. | Classify each caller, preserve effect/completion separation, and demonstrate managed adapter parity before deleting progression helpers. |
| `tests/security/negotiation.rs` native/hybrid regressions | dataplane/negotiation | Queue pressure, authenticated replacement, stale messages, and real I/O have coverage beyond the confirmation actor and admission mutant. | Retain required selectors; migrate only with equivalent wire, capacity, identity, and ownership assertions. Native I/O validation remains a separate tier. |
| Ignored large-cluster selectors in `tests/runtime/cluster.rs` | dataplane/runtime | The bounded scale profile runs 16 real modeled nodes, not the default 1024-node topology. These entries lack complete journals. | Measure the larger topology within its declared resource budget and add streaming replay before advertising large-scale exact replay. |
| Native io_uring, provider, and real-thread fixtures | dataplane/platform | Simulation cannot execute kernel/provider ordering or host-thread behavior. | Keep capability-gated native checks. The runner currently requires io_uring; hardware/provider and deployment coverage must be requested and reported separately. |
| Subscriber and Go controller integration | dataplane/control | Excluded from this implementation by user direction. | A separately approved control/data-boundary project is required; prepared configuration publication is not controller decision coverage. |

No retained test or engine is removed by this migration. Source files above are
relative to `cmd/racer-dataplane/`. In particular, allocator
`assert_recovery` visits every retained checkpoint and compares it with the
independent snapshot map; a successful cluster GET cannot replace that check.

## Remaining model and campaign limits

- Actor templates compose their documented concurrent operations. Inputs select
  one template plus follow-up actions; arbitrary actor combinations are rejected.
  Nightly sampling varies independent seeds across all passing templates, not an
  unrestricted action/configuration cross product or coverage-weighted search.
- Cross-domain phase permutations retain bounded fairness and next-turn CQ
  delivery. Timers and delayed peer notifications still have declared turn/FIFO
  ordering; this is not enumeration of all enabled kernel events.
- Shared-worker coverage exercises common listeners, publication, and flight
  ownership with worker-local caches. It does not combine a whole-process crash
  with pending operations on every worker.
- Wall offsets and bounded directional queues have conformance and integration
  coverage. A required component cell verifies signed authentication windows and
  monotonic nonce expiry across wall steps. In-flight HTTP authentication-expiry
  overlap and every transport policy cross product remain outside the required
  cells. Half-close/reset have an exact-replayed environment contract, not yet a
  full HTTP recovery actor.
- Targeted fixture oracle declarations explain substitutions. They do not prove
  that every caller executes its replacement, nor supply an independent logical
  placement model for every custom topology.
- The reducer preserves named failure identity and typed path witnesses as an
  ordered subsequence, including repetitions. Normalized request identities do
  not establish a complete causal graph. It does not reduce arbitrary actor
  internals/object sizes or minimize scheduling prefixes.
- All six initial semantic mutant families have paired controls and exact replay.
  Some exercise production components directly; their evidence is not relabeled
  as end-to-end HTTP, kernel, or deployment coverage.

## Verification snapshot

Runs use a 23,000,000,000-byte process-tree cgroup limit, no swap, and explicit
build/suite/campaign deadlines. Retained artifacts are ignored local outputs.

| Tier | Verified result | Artifact directory |
| --- | --- | --- |
| PR baseline | 94 test executions passed across five groups | `dst/artifacts/stream-baseline` |
| Required artifacts | 28 cells passed their outcome/witness gates and fresh-process exact replay | `dst/artifacts/stream-policies` |
| Nightly sampler | 26 required plus 13 sampled cells passed and exact-replayed | `dst/artifacts/nightly-matrix` |
| Native | Required kernel capability, negotiation, and remaining library: 376 test executions passed | `dst/artifacts/native-capabilities-owned` |
| Bounded scale/formats | Four gates passed at 16 nodes, seed 19 | `dst/artifacts/scale-contract-correction` |

These runs were performed at their respective incremental revisions; each
bundle retains its executable hash and source metadata. Hosted workflow syntax
was checked locally; execution on GitHub runners has not been observed here.
Rust formatting passed. Repository-wide `make fmt` was attempted but its
golangci-lint binary, built with Go 1.26, fails while loading Go 1.27 code.
