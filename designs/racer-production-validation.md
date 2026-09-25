# Racer production validation

Source audit: 2026-09-25, baseline `6938c593..e975e41b` plus pending integration
changes. Paths below are relative to `cmd/racer-dataplane`. This document describes
inspected implementation and test assertions. The production-component suite was
also executed as recorded below; broader lifecycle/native results remain the
integration owner's responsibility.

## Production-component read fixtures

`tests/production_dataplane.rs:368-663` manually assembles the real read graph:
coordinator, fill, origin HTTP over Unix sockets, page crypto, memory, direct-I/O
storage, dispatcher, parser, and response writer. It polls the real reactor and
crypto engine. The client side uses accepted socket pairs; the fixture does not
launch `Application`, its client listeners, or the executable.

The six tests assert:

| Scenario | Assertions | Source |
| --- | --- | --- |
| Multipage bootstrap and cache reuse | Initial unpinned fetch, five-page pinned remainder with two-page window, six persisted records, exact memory and disk bytes with origin offline | `tests/production_dataplane.rs:712` |
| Zero-TTL metadata | HEAD revalidation, new version, old pinned version still available offline | `tests/production_dataplane.rs:771` |
| Expired metadata and old pins | Nonzero expired observation, new length/version, missing pin returns 412, old-version disk suffix remains exact | `tests/production_dataplane.rs:817` |
| Bounded page memory | Eight-page stream with four plaintext pages and two dirty pages, dirty flush, offline reread of every actually persisted page | `tests/production_dataplane.rs:863` |
| Overlapping zero-TTL callers | Concurrent callers share one origin flight; later caller revalidates | `tests/production_dataplane.rs:908` |
| Backpressured reader | Fast reader completes while slow reader holds delivery; cancellation releases reactor/pipe/flight resources and preserves cache bytes | `tests/production_dataplane.rs:971` |

### Executed production-component verification

From `cmd/racer-dataplane`, on 2026-09-25:

```sh
rustfmt --edition 2024 --check tests/production_dataplane.rs
cargo test --test production_dataplane -- --nocapture
```

Formatting passed. All six tests passed, zero failed, zero ignored. These tests
run real crypto and O_DIRECT storage without read/storage/crypto success doubles.
The parent reports actual Go SDK conformance already passes; it was not rerun here.

### Production failure reported and corrected by parent

The initial application composition shared a `PAGE_BYTES + 16` HTTP body cap
between page traffic and client responses. Long-range tests failed with
`Err(InvalidRequest)` before a response head or pinned origin fetch. Exact original
repro from `cmd/racer-dataplane`:

```sh
cargo test --test production_dataplane bootstrap_then_pinned_remainder_over_three_pages_and_disk_hits_without_origin -- --exact --nocapture
```

The parent correction adds `HttpIo::for_clients` (`src/http/io.rs:139`) and uses
it in application composition (`src/app.rs:718`). Sending supports whole-object
ranges while receiving retains the page cap. The final fixture uses that same
production constructor (`tests/production_dataplane.rs:558`), not a test-only
relaxed codec. The test commit depends on the parent-owned production correction.
The original repro now exercises the fixed path.

### Harness corrections

- The real crypto engine is polled inline in the fixture, so debug-build page
  crypto can exceed a two-second delivery stall deadline while occupying the
  fixture executor. Delivery now uses the bounded 40-second request timeout
  (`tests/production_dataplane.rs:536`). This is not evidence of a production
  delivery failure; production has a paired crypto worker.
- A drained writer does not imply every fetched page persisted. Fill explicitly
  discards unsubmitted writes and may retry without dirty admission under pressure
  (`src/read/fill.rs:69-78`). The initial complete offline replay assertion was
  intermittently false for this reason, not established corruption. The full-cache
  test now has sufficient dirty capacity and asserts six records; the pressure
  test demands offline reads only for records actually published in the disk index.

No additional unresolved production failure was established by these six tests.

## Application lifecycle fixtures

`src/app_integration_tests.rs` includes real TLS enrollment and authenticated
publication polling. The latest assertions cover:

- Two worker objects: pending late driver blocks removal; rejected publication
  preserves state; committed removal rejects late memory and disk fills. The test
  drives stage/publication directly and substitutes a pending control future
  (`55-132`). It does not test a populated-cache update delivered by the server.
- Separate worker threads: real control startup and one complete two-shard shutdown
  checkpoint (`135-161`).
- Real two-pair WorkerGroup: held-key retirement/resume, joined worker threads, and
  exact checkpoint shard IDs `[0, 1]` (`173-216`). The TLS fixture publishes empty
  caches and the test installs key bundles directly.
- One worker: accepted crypto completion must precede checkpoint invalidation;
  the last key lease must precede destruction/resume; final reactor and crypto
  counts are zero (`454-590`).

Retirement uses a conservative full-node pause, including diagnostics. Its retained
checkpoint invalidation future is reactor-backed and must finish before the key
retirement loop (`src/app_retirement.rs:369-394`). This is narrower than claiming
all filesystem work is nonblocking: checkpoint publication still uses synchronous
filesystem operations and explicitly omits fsync durability
(`src/store/checkpoint.rs:1-4`, `142-200`); prepared listener setup also performs
synchronous filesystem operations (`src/client/transition.rs:110-223`).

Readiness combines all workers' expiring observations. Its listener observation
checks task presence and its membership observation checks publication-sequence
agreement (`src/app_health.rs:75-111`); it is not an end-to-end service probe.

## SDK and native validation boundaries

- The opt-in Go SDK conformance test builds the actual SDK with an overlay and
  exercises production HTTP parsing plus a real Go origin server
  (`tests/conformance/sdk.rs:21-123`). Its Rust responses are scripted; this is not
  a deployed SDK-to-application read test.
- Configured native activation reaches the paired service and falls back when the
  device/provider is unavailable (`src/app_native.rs:245-312`). The ignored
  ABI-v2 no-device test requires a loadable native adapter; feature compilation
  alone does not execute it.
- Signed peer fallback tests assert exact ciphertext and released registered-byte
  accounting (`src/peer/native_io.rs:104-114`, `373-381`).
- The ignored real-provider roundtrip requires explicit device/port/GID settings.
  It asserts one native completion, zero fallback, and exact ciphertext
  (`src/peer/native_io.rs:383-646`). A passing fallback test cannot stand in for this
  hardware test, and a single-host roundtrip cannot establish multi-host fault
  behavior.

## Resolved fixture socket-root handoff

`OriginClient::with_socket_root` provides a lexical absolute local root override
while the default remains `/run/racer` (`src/origin/client.rs:86-121`). Resolution
still derives the endpoint from an accepted cache definition (`186-193`), and
direct publications still pass the strict wire codec
(`src/control/snapshot.rs:97-99`). The fixture uses a retained directory-FD alias
for short Unix socket paths while asserting the canonical published endpoint
(`tests/production_dataplane.rs:483-492`). The earlier fixed-origin-root testability
blocker is resolved.

## Remaining acceptance gates

Final integration verification on 2026-09-25: `cargo test --locked --manifest-path
cmd/racer-dataplane/Cargo.toml --all-features --quiet` completed successfully with
492 library tests, 2 executable tests, 18 conformance tests, 6 production tests,
and 31 doctests passing. Six native tests and the separately exercised SDK test
are gated in the default run. All three `native_no_device` tests passed explicitly
with `LD_LIBRARY_PATH` pointing at the built native adapter. Default and all-feature
all-target checks and Rust formatting passed. The final routing-capacity change
also passed all three application composition/membership tests.

The required scoped `make fmt` attempt ran gofumpt, but golangci-lint failed because
its Go 1.26 build cannot analyze a Go 1.27 dependency. No Go changes resulted.

The membership-retirement audit finding is resolved by per-object structural
reference accounting in `update_memberships` (`src/app.rs:1139-1171`). Its two-worker
regression covers a retained request lease, churn beyond routing-table capacity,
and workers skipping an intermediate publication. This is component-level churn
coverage, not a deployed control-server churn test.

1. Exercise deployed Go SDK reads through the binary's actual client listeners,
   populated-cache control-driven publication/removal, and restart with persisted
   payload. The component/lifecycle fixtures above establish narrower contracts.
   The repository's Go control server is still a scaffold:
   `internal/racer/server.go:34-42` (repository-relative) returns pending errors for
   TLS/start/readiness, and its handlers always return unavailable (`55-66`). The
   Rust TLS fixture therefore does not establish actual Go-controller interoperability.
2. Record real native-provider execution separately from fallback/no-device checks,
   then validate multi-host RNIC operation and fault behavior in the deployment
   environment.

Final formatting, feature/build checks, complete suites, and test execution records
belong to the integration owner. Historical totals in the coordination document
must not be treated as proof that later pending edits passed.
