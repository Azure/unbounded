# Fleet layer connection-admission regression

Base: `ef461fdfac661d4ac43cebae1498a94b28ce837a`. Investigation performed on
2026-09-26 in worktree `tmp/racer-fix-fleet-layers`, branch
`racer-fix-fleet-layers`.

## Live evidence

Read-only tracing used context `joolshev-scale-test` and existing hostPID net-node
pods. Every executed cluster operation used `--request-timeout=20s`; remote
commands, subprocesses, and local command groups had explicit timeouts. No AKS
configuration or durable files were changed. Temporary uprobes detached on exit.

The exact benchmark manifest was
`sha256:2aa1e8745512f4f76499b8b9a5543642630d3f1120668d8636278537d9aa53a6`,
namespace `loadgen.invalid`, repository `benchmark/image`.

- On `aks-ddsv6-84072342-vmss00004b`, a fresh request for layer
  `sha256:2115d07a4bfcddb9d1c4322c22fda03d1f24032560ae938b8c8d224eef408c1d`
  returned HTTP 200 with Content-Length 59,757,056 but EOF at 16,777,216 bytes
  after 0.066 seconds. `tmp/trace-live.log` retains the result and concurrent
  flight failure counts. This was not a probe timeout.
- A subsequent passive capture during the same layer pull ended at 33,554,432
  bytes. Page 2's first attempt received a signed `overloaded` response from
  first-hop relay `10.242.88.74`; its second attempt received a signed
  `overloaded` from a downstream relay. The third attempt carried zero delegated
  attempts and received `unavailable`. See `tmp/trace-wire.log:1-336`.
- The first-hop relay is `aks-ddsv6-84072342-vmss00001z`, net-node
  `unbounded-net-node-f9sp7`, dataplane PID 2285134 during this sample. A four-second
  uprobe sample at the actual ELF file offset `0x24cd16` counted connection
  admission overload 441 times, relay admission overload 184 times, and one
  ciphertext overload. Resource discriminants are defined in
  `cmd/racer-dataplane/src/model/limits.rs:30-42`.
- A three-second follow-up recorded the caller address at `reserve_inner` entry
  and grouped overloads by caller. It found 37 outbound checkout failures, 280
  inbound accept failures, and 21 relay-slot failures. Subtracting the binary load
  base `0x5d134db23000` gave return addresses `0x313461` (HttpPool checkout),
  `0x312673` (ConnectionLease::from_accepted), and `0x2abe5d` (Relay::forward).
  Disassembly verified the outbound failure immediately after connection reserve.
- A bounded `/proc` snapshot on that relay found 23 outgoing peer sockets and 40
  incoming peer sockets including its listener. Of the outgoing sockets, 22 were
  established with empty send/receive queues. This suggests substantial idle
  occupancy, but empty kernel queues alone do not prove application-level idleness.

The capture decodes signed fields but does not independently verify signatures;
the application verifies them. Host-interface capture can duplicate packets, so
packet occurrence counts are not used as request counts. Uprobe aggregate samples
include background traffic, not only the explicit pull.

## Confirmed bug and focused fix

Completed pooled sockets retain connection reservations until reuse or expiration
(`cmd/racer-dataplane/src/http/pool.rs:118-145`). Before this change, checkout of a
different neighbor could return `Overloaded` while this same pool owned reclaimable
idle sockets. At a relay this becomes a signed overload response
(`cmd/racer-dataplane/src/peer/server.rs:314-330`), which consumes candidate retries
and can fail an already-started layer stream.

Checkout now reclaims one oldest completed idle lease when connection reservation
fails, then retries that reservation once
(`cmd/racer-dataplane/src/http/pool.rs:244-250,269-301`). It preserves the existing
global and per-endpoint limits, request deadline, active leases, and connecting
completion fences. It does not retry a wire request or increase capacity. The scan
is bounded by existing pool limits and runs only on connection admission pressure.

This confirms a preventable outbound connection failure at the traced boundary.
It does not establish that every live overload has this cause: incoming admission,
relay-slot capacity, and ciphertext pressure were also observed. The fix only
reclaims idle sockets belonging to the pool doing checkout; it does not reclaim
accepted sockets or another pool's idle sockets. Fleet recovery requires the
parent's rollout and post-rollout validation.

## Old-fail/new-pass verification

`cmd/racer-dataplane/src/peer/fleet_layer_tests.rs` uses three independently admitted
peer graphs, real TCP and io_uring, signed request/response forwarding, and a
two-link path through a relay. Twelve distinct neighbors first complete real HTTP
exchanges and retain idle connections. Four incoming layer requests then fill the
relay's 16-connection quota. Outbound progress requires reclaiming idle capacity.

Eight layers each contain four 16 MiB pages plus a 17-byte final page. Four layers
are read concurrently. Every response checks identity and metadata length,
authenticates/decrypts the ciphertext, and contributes to a complete layer SHA-256
and byte-count comparison. Final draining asserts zero connection, ciphertext,
and relay reservations.

Command, with a 115-second outer timeout:

```sh
cargo test --locked --release --all-features --lib \
  full_image_through_relay_reclaims_idle_neighbor_capacity -- --ignored --nocapture
```

- Restoring the base checkout statement fails on layer 0, page 0: the relay returns
  a non-page response. The separate pool regression fails with `Overloaded` too.
- Candidate: all eight layers, 40 pages, and 536,871,048 plaintext bytes pass.
  Final all-feature run completed in 2.64 seconds.
- Pool regression additionally checks that only one idle connection is reclaimed,
  same-neighbor concurrency still rejects, fully active capacity still rejects,
  and closing the pool releases the remaining reservations.

The deterministic regression isolates peer transport pressure; it uses a synthetic
page service and pre-established authenticated peer challenges, not Gantry,
placement, origin acquisition, or a 1,500-member topology. It is not an OCI
manifest/config test despite its full-image-shaped layer workload.

## Full-stack kind check

The retained cluster `racer-e2e-1790395612486835516` has one dataplane, so its
full-image test is a compatibility check rather than the causal peer reproduction.
The rebuilt candidate ran through `e2e/racer/full-image-kind.py` with concurrency
64, four layers per image, eight 64 MiB-base layers, 20 percent jitter, and complete
size/SHA-256 verification. The exact benchmark manifest above passed:

| Full images | Errors | Canceled | Complete layers | Verified body bytes |
| ---: | ---: | ---: | ---: | ---: |
| 64 | 0 | 0 | 512 | 34,748,963,840 |

Evidence: `tmp/racer-full-image-ksi6cueu/probe.log`, with arguments, pod image IDs,
and component logs alongside it. The harness restored Gantry configuration and
removed its probe resources. The retained kind worker's dataplane tag was then
restored to `racer-dataplane:full-image-candidate` (the preexisting ef461 build).

Only the production `racer-dataplane` image is affected. The local test image
`racer-dataplane:fleet-layers` has OCI index
`sha256:8b1839488de6b4adbe97b2883c368785c05063fda4261e2d95571271fdb8dcfc`.
Publication and AKS rollout remain parent-owned.

## Checks

- All-feature Rust library suite: 556 passed, nine ignored; the new opt-in full
  layer regression was separately run and passed.
- Binary tests: two passed. Client/origin conformance: 18 passed, one opt-in SDK
  test ignored. Release production-dataplane tests: nine passed. Doc tests: 31 passed.
- The combined debug test command hit its 115-second outer deadline during the
  production suite after library/binary/conformance passed. The complete production
  suite was rerun in release mode and passed in 9.09 seconds.
- `cargo fmt --all --check` and all-target/all-feature Clippy passed. Clippy reports
  existing warnings in the package; there were no diagnostics on the new regression.
- Scoped `make fmt` and Go lint for `./cmd/racer-loadgen` passed with repository-local
  golangci-lint 2.13.1. An initial 2.11.4 invocation could not decode Go 1.27 export
  data. Actionlint passed after installing the pinned tool locally.
- Production Containerfile image build and `git diff --check` passed. No existing
  tests were removed, no dependencies changed, and no source outside this worktree
  was edited.
