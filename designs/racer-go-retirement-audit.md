# Racer Go control-plane retirement

> The native campaigns, SDK daemon test, and Python probes referenced below have
> since been retired. Current e2e coverage is the [single kind smoke test](../e2e/racer/README.md).
> The coverage and build commands below record the historical retirement milestone.

> Historical pre-1.0 audit. Versioned endpoints and deprecated Site-label
> fallback below describe that revision, not the Racer 1.0 contract.

> Historical audit of the Go-to-Rust retirement. Its placement-history, durable
> chunk/GC, version-4 CA, same-Pod ambiguity, and fleet-proof claims describe the
> implementation validated at that milestone, not the current stateless runtime.
> The [stateless contract](racer-rust-controlplane.md) supersedes those semantics.
> The results below must not be reused as validation of the stateless cutover.

## Final state

Retirement is authorized after the production-binary campaign and real API GC
coverage passed. The agreed capacity target is 10,000 nodes. Historical Go
100,000-participant tests are not an additional acceptance requirement.

Deleted through the patch workflow:

- `cmd/racer-controlplane/main.go` and `main_test.go`.
- All production and test files in `internal/racer-controlplane`.
- All production and test files in `internal/racer/pki`.
- The obsolete `ControlCommand` message in `api/racer/control.proto`; Go bindings
  regenerated with `go generate ./api/racer`. Rust uses build-time generation.

No Go imports of either deleted package remain. No Go or Rust code consumes
`ControlCommand`. `e2e/racer/scripts/controlfixture` was the last external wire
consumer; it now serves `DesiredState` at `/v4/config` with a received cursor,
long polling and local-state feedback independent of desired-state publication.
Its authenticated identity checks, tests, and Python probe wiring remain.

Keep `internal/racer/{metadata,site,p2pcache,cache_size,trust}.go` and their tests.
These are shared operator/SDK metadata, Site identity, socket, quantity and public
trust helpers, not a second control plane. The SDK's TLS fixture is a test-only
X.509 issuer signing the actual dataplane's CSR. E2E decodes Rust metadata version
4 and `private_key`, preserving Secret UID, root-material, rotation, traffic and
dataplane restart assertions. Public API bindings, CRDs and SDK/origin helpers
remain independent of implementation retirement.

## Retained behavior and replacement coverage

Rust test paths below are relative to `cmd/racer-controlplane/tests/`. Former Go
test paths are historical references available in Git history.

| Retained behavior | Former Go coverage | Current replacement |
| --- | --- | --- |
| Site/node/Pod identity, fallback, exclusion, socket safety, cache recreation/isolation | `membership`, `pod_identity`, `model`, `p2pcache`, `controller` tests | `core.rs` identity, normalized inventory, recreation and independent graph assertions; shared Go helper/operator tests; real campaign bootstrap and absence scenarios |
| Stable balanced placement and sparse 10,000-node snapshots | `topology`, `model` tests | `core.rs` placement history/restart/list-order and 10,000-member sparse snapshots; current Rust topology tests |
| Durable-before-publication, uncertain commit readback, captured RV, restart and fencing | `controller`, `envtest`, `coordination_harness` tests | `core.rs` commit transitions; `runtime.rs` watch/CAS/restart; real `record_store_real_kubernetes_api`; campaign real watch, stale RV rejection and crash takeover |
| Immutable chunk GC, staged write protection, interrupted collection and takeover | `state_gc` tests | `runtime.rs::chunk_gc_preserves_staging_recovers_interruption_and_fences_takeover` and `record_store_real_kubernetes_api` |
| Independent convergence, cursor/application separation, disconnects, long polls, bounded idle work | `protocol`, `server`, rollout/liveness/capacity/heartbeat tests | `runtime.rs` actual TLS cancellation and 10,000 router waiters; default distinct-TLS scenario with 24 nodes in three universes; real campaign selected-but-absent peer; dataplane v4 desired/activation/mixed-version tests |
| Exact quantities, invalid last-good policy, offer-bound status, storage/topology independence | storage policy/status/runtime/enrollment tests | `core.rs`, `runtime.rs` process-bound feedback; real campaign grow/shrink, shard changes, rejected capacity, equivalent quantities, invalid intent and retained slab restart |
| Durable CA issuance, lost/corrupt state, hostile CSR, canceled/stale leader, fresh TLS proof, expiry floor | PKI bootstrap/issuance/concurrency/proof/retirement tests | `security.rs` fault-injected persistence and actual TLS scenarios; `service.rs` Kubernetes adapter conflicts and takeover; actual campaign Pod-bound TokenReview, live roots 1 through 4 |
| Warm standby readiness, nonleader rejection, same-Pod boot ambiguity and UID absence | replica TLS, availability, node restart/retirement tests | `service.rs` warm standby and ambiguous-boot replacement scenarios; actual campaign two competing Rust processes, Lease expiry and CP Pod replacement |
| Continuous encrypted traffic through CA retirement | `rotation_harness_test.go` | `e2e/racer-controlplane/TestProductionBinaryCampaign`: actual two Rust CP and three DP processes, full expiry-gated root retirement, verified reads without retries hiding errors |
| CLI and native artifact identity | `cmd/racer-controlplane/main_test.go` | `service.rs` CLI and native binary version tests; real campaign bootstrap; Make/container version, commit and build-time smoke checks |
| SDK page interoperability and operator workload admission | SDK/operator/e2e tests | Retained Go suites with the actual page-striped dataplane; real-API operator tests; retained kind deployment suites |

Remote prepare/receive/transmit/retire barriers and durable rollout/catch-up
ledgers were intentionally replaced by independently converging v4 desired state.
The Rust tests remain; no current Rust tests were removed during retirement.
Ten thousand router waiters are structural concurrency coverage, not a throughput
claim for ten thousand distinct live TLS connections.

## Verified campaign evidence

The saved local log `tmp/live/racer-live-run-7xgD7B.log:3-37` records:

- Two CP processes electing a leader and independent convergence while a selected
  peer is absent.
- Storage applied/failed paths, invalid-intent last-good retention, topology watch
  publication, Lease-expiry failover and CP Pod replacement.
- CA generations 1, 2, 3 and 4, all three dataplanes installing retired trust,
  3,518 continuous verified SDK reads, and retained storage after DP restart.
- `PASS`, with the full campaign completing in 246.42 seconds.

This supersedes the earlier failed runs documented during development. The
coordinator also verified the real kube-apiserver RecordStore/GC scenario. The
campaign assertions in `e2e/racer-controlplane/campaign_test.go` remain unchanged
by retirement. Envtest supplies a real API server but no kubelet/controllers:
the harness seeds workload status, relays Service TCP routing, and projects public
trust bytes. Kind remains the separate operator/deployment integration gate.

## Build and CI after retirement

- `racer-go-test` covers shared Go APIs/helpers, SDK, operator and tools.
- `racer-crosslang-test` builds the actual dataplane for SDK interoperability.
  The separate object adapter was removed. SDK `Object` is an immutable HEAD
  snapshot whose reads use page GETs directly through the dataplane.
- `racer-controlplane-live-test` builds both Rust binaries and runs the explicit
  live campaign. The runner uses `-tags=e2e`, `-mod=readonly`, strict prerequisites
  and an external timeout. General Go tests do not accidentally run a campaign
  without its prerequisites; the explicit CI campaign still fails if they are absent.
- CI supplies envtest to Rust tests (including real API GC), runs the actual
  CP/DP campaign, then SDK interoperability, and uploads public logs/observations.
- `make e2e-racer-compile` compiles both kind suites and the native campaign.
- System OpenSSL CP build/runtime dependencies and separate native Cargo target
  paths remain. NOTICE is generated by the existing workflow, with 140 entries.

## Retirement verification

Go commands use `GOTOOLCHAIN=go1.26.6`. Retained Racer Go race suites, migrated
controlfixture tests, both e2e package compilations, Python probe tests, scoped Go
lint, shell syntax, YAML parsing and NOTICE check passed. A first Go run using the
long worktree TMPDIR failed Unix socket path validation; rerunning with short
project-local scratch passed without changing tests or path validation.

The final cutover does not rerun the already-passed full live campaign solely for
Go deletion/build-tag wiring. The final report separately records schema rebuilds
and any remaining environment/check limitations. Live hardware throughput and RDMA
performance are not claimed by these checks.

Post-schema checks rebuilt both Rust release binaries and passed SDK tests against
the rebuilt dataplane. The coordinator's final reported results are 33 CP tests
passing, including real envtest coverage, and 337 dataplane library tests,
14 binary tests and 73 doctests passing. The standby failure is resolved: the
service fixture now permits metadata fencing updates on immutable ConfigMaps
while rejecting payload changes (`tests/service.rs:279-290,1030-1075`).
`go list ./...` and `go mod tidy -diff` passed; no module changes were needed.

Placement conformance now consumes actual Rust compiler exports via
`cmd/racer-dataplane/tests/control/topology.rs:222-229` and
`cmd/racer-controlplane/tests/placement_export.rs`. The retired Go producer and
its stale test hooks are no longer required.

## Measurement boundaries and test duration

The default distinct-TLS scenario uses 24 nodes, three universes and 512 slots,
with a 12-second deadline and independent 20-second watchdog. The coordinator
reported a 0.32-second run. The full 10,000-node TLS stress scenario is ignored by
default (`tests/scale/distinct.rs:82-96,297-312`). These are distinct from the
structural 10,000-router-waiter and sparse-topology checks.

Earlier completed loopback stress measurements are recorded in
`tests/scale/distinct.rs:7-19`:

| Nodes / universes | Initial publication | Fleet publication bytes | Peak combined client/server RSS |
| --- | --- | --- | --- |
| 10,000 / 1 | 33.964 seconds | 12,375,143,725 | 1,062,880 KiB |
| 10,000 / 10 | 65.940 seconds | 15,149,268,618 | 1,288,972 KiB |

Both endpoints ran in the same process over loopback. These figures do not measure
enrollment, Kubernetes persistence, NIC throughput, or remote-client performance.
**First-byte tail compliance at 10,000 nodes remains unverified**: its measurement
was canceled. Completion times above do not establish compliance with the
production first-byte deadline. Slow-store fleet convergence and hardware/RDMA
performance are also not established by these results. No further scale testing
is part of this housekeeping pass.
