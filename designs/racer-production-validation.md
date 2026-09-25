# Racer production validation

Source audit: 2026-09-25, baseline `6938c593..e975e41b` plus pending integration
changes. Paths below are relative to `cmd/racer-dataplane`. This document describes
inspected implementation and test assertions, not an independent suite execution.
The integration owner records final commands/results against the settled commits.

## Production-component read fixtures

`tests/production_dataplane.rs:369-629` manually assembles the real read graph:
coordinator, fill, origin HTTP over Unix sockets, page crypto, memory, direct-I/O
storage, dispatcher, parser, and response writer. It polls the real reactor and
crypto engine. The client side uses accepted socket pairs; the fixture does not
launch `Application`, its client listeners, or the executable.

The six tests assert:

| Scenario | Assertions | Source |
| --- | --- | --- |
| Multipage bootstrap and cache reuse | Initial unpinned fetch, pinned remainder, exact bytes from memory and forced disk with origin offline | `tests/production_dataplane.rs:707-762` |
| Zero-TTL metadata | HEAD revalidation, new version, old pinned version still available offline | `tests/production_dataplane.rs:765-808` |
| Expired metadata and old pins | New length/version, missing pin returns 412, old-version disk suffix remains exact | `tests/production_dataplane.rs:811-854` |
| Bounded page memory | Stream larger than page budget, dirty flush, complete offline reread | `tests/production_dataplane.rs:857-886` |
| Overlapping zero-TTL callers | Concurrent callers share one origin flight; later caller revalidates | `tests/production_dataplane.rs:889-949` |
| Backpressured reader | Fast reader completes while slow reader holds delivery; cancellation releases reactor/pipe/flight resources and preserves cache bytes | `tests/production_dataplane.rs:952-1043` |

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

1. Fix multiworker membership retirement: every worker holds two references, but
   cleanup requires global `Arc::strong_count == 2` (`src/app.rs:910-930`). This
   ownership-based inference predicts bounded-table exhaustion under version churn
   (`src/peer.rs:41-54`). Add multiworker churn coverage with and without retained
   old-version requests.
2. Exercise deployed Go SDK reads through the binary's actual client listeners,
   populated-cache control-driven publication/removal, and restart with persisted
   payload. The component/lifecycle fixtures above establish narrower contracts.
   The repository's Go control server is still a scaffold:
   `internal/racer/server.go:34-42` (repository-relative) returns pending errors for
   TLS/start/readiness, and its handlers always return unavailable (`55-66`). The
   Rust TLS fixture therefore does not establish actual Go-controller interoperability.
3. Record real native-provider execution separately from fallback/no-device checks,
   then validate multi-host RNIC operation and fault behavior in the deployment
   environment.

Final formatting, feature/build checks, complete suites, and test execution records
belong to the integration owner. Historical totals in the coordination document
must not be treated as proof that later pending edits passed.
