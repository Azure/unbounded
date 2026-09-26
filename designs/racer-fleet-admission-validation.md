# Peer receive admission versus retained local Fill cache

Base: `6466ff93fd31c6864bd6a2a436dde32ee334ae5c`. Branch and worktree:
`racer-fix-fleet-admission`, `tmp/racer-fix-fleet-admission`. Date: 2026-09-26.

## One confirmed bug

HTTP peer receive allocated its ciphertext buffer with an immediate reservation
failure, even when that worker owned disposable local cache pages or queued writes
(`cmd/racer-dataplane/src/peer/transfer.rs:319-327` at the base). Production Fill
already reclaims these owners before rejecting its own allocation
(`cmd/racer-dataplane/src/read/fill.rs:69-79`). A node doing both local reads and
transit can therefore send a signed overload instead of forwarding a valid page.
Relay dispatch converts the receive error to a signed overload
(`cmd/racer-dataplane/src/peer/server.rs:313-320`). Gantry reports early failures as
503, or aborts an already started body
(`internal/gantry/mirror/racer.go:135-147,159-176`).

`Transfers::receive_buffer` now discards unsubmitted writes and incrementally evicts
idle cache entries until the actual ciphertext reservation succeeds or there are
no disposable entries (`cmd/racer-dataplane/src/peer/transfer.rs:127-159`). The
application supplies the same worker's memory and writer
(`cmd/racer-dataplane/src/app.rs:631-633`). Each pass removes an entry, so reclamation
is bounded by existing cache capacity. Independent plaintext/ciphertext owners
remain protected by the existing idle predicate
(`cmd/racer-dataplane/src/memory/cache.rs:98-111,149-152`). There is no added wait,
wire retry, resource-limit increase, or persistent-format change.

This fixes a demonstrated avoidable receive-admission failure. Live tracing proves
the failing receive boundary, but does not measure the reclaimable fraction of the
live quota. It does not prove that this fix resolves all fleet overloads.

## Settled read-only AKS evidence

All cluster commands explicitly used `joolshev-scale-test`, request timeout 20s,
and subprocess timeout at most 30s. The three-node probe had a 110s alarm and 120s
outer cap. Traces used existing hostPID net-node pods, 24s remote bounds, 22s remote
alarms, and temporary uprobes with automatic detach. No AKS configuration or durable
state was changed. No host sysctl was changed and no new kind cluster was created.

The settled probe is retained in the deployment worktree:
`tmp/racer-v2-cluster-validation/tmp/racer-6466-fleet-admission-settled.json` and
`.md` (paths relative to the original repository). All sampled images matched
`6466ff93`; manifest/config reads succeeded. Results:

- Zero of three complete images; ten successful layers, 13 premature EOFs, one
  layer HTTP 503.
- Stable sampled pod UIDs/restarts and process counters; background full-pull error
  deltas 10, 10, and 11, with no new full successes.
- Prometheus: 1500/1500 scrape coverage, 82 cumulative successes on 78 pods,
  4,319,844 cumulative errors, approximately 1298.7 errors/s over five minutes.
  The rate window can overlap rollout; it is not a controlled before/after result.

Affected benchmark image:
`loadgen.invalid/benchmark/image@sha256:2aa1e8745512f4f76499b8b9a5543642630d3f1120668d8636278537d9aa53a6`.
Loadgen remains one pull per node, four concurrent layers, eight layers with
64 MiB base and 20 percent size jitter.

### Fresh failed nonce and downstream receive boundary

An explicit GET for layer
`sha256:2115d07a4bfcddb9d1c4322c22fda03d1f24032560ae938b8c8d224eef408c1d`
returned HTTP 200, length 59,757,056, then EOF at 16,777,216 bytes in 0.04775s.
Page-1 nonce `s4ozhXD/gbphv7yIDzgnww==` tried:

1. `10.242.125.73`, returning downstream overload from
   `80d587ad-66b1-45a4-b82d-74e2333a33a6`.
2. `10.242.226.73`, returning overload.
3. `10.241.35.82`, returning overload, with zero delegated attempts left.

Evidence in this worktree: `tmp/unbounded-net-node-2wqt8-wire.json:1-140`.
`tmp/map.json` confirms sampled peer images at the base. The downstream relay is
`racer-dataplane-2k4cn`, `10.241.155.74`, node
`aks-ddv5-17198779-vmss0000d5`.

On that relay, simultaneous packet/uprobe tracing identified three receive
ciphertext reservation failures. The caller resolves to `WireBuffer::for_cache`
at ELF return address `0x160b67`, called by `Transfers::exchange_planned` at
`0x1637fd`. `tmp/unbounded-net-node-82j5x-disasm.log` verifies reserve entry
`0x1503e0`, overload branch `0x150ba4`, and the ELF mapping's 0x1000 file-offset
difference. Actual uprobe offsets were `0x14f3e0` and `0x14fba4`.
The trace also observed 3295 reserve-fill ciphertext failures, which include
repeated local admission checks and are not 3295 distinct failed requests.

On first-hop relay `racer-dataplane-r6c8q`, `10.242.226.73`, another bounded trace
observed six receive failures and captured this request end to end at the relay:

- Background request nonce `0P95YSq8bA+LM5XOVeC9Mw==`, attempt
  `9BpitfJbx6nm5KJpcLzgJA==`, layer
  `983882d4a89fdc9200fbd273ba219d3740d989c82d6d6beabd55b68db267de87`, page 1.
- Path: source `803d8e65-de8b-41b1-8dcb-ca16244807f5`, relay
  `45915147-e2bd-4342-a0b1-902f3a21b9d3`, next hop
  `f9046a12-da31-4e59-b55e-8ab7fa6638d1`, candidate
  `7c52013c-0969-4a65-8580-b3098501d074`.
- At Unix time 1790405914.075995, a signed downstream page advertised 16,777,232
  ciphertext bytes. Receive admission failed at approximately 1790405914.076269.
  The socket closed at 1790405914.076313; at 1790405914.076882 the relay returned
  signed overload with the same binding
  `QIDHQgMivZH0B+ZbL4+5pSqGZCWQUSP0etnu2+BBShQ=`.

Evidence: `tmp/unbounded-net-node-6tcfq-wire.json:4995-5071`, request event 256,
and the file's admission/clock/caller fields. `tmp/summarize.py` performs the
wall/monotonic conversion. The explicit probe in this last window succeeded for
the earlier `2115...` layer; the correlated failure is a background request for a
different layer of the same benchmark image.

Uprobes record callers and timestamps, not request IDs. Correlation uses the
socket, selected signed fields, same response binding, and event ordering. Packet
capture may duplicate/drop/reorder packets, and the capture script does not itself
verify signatures. No credential or page payload is retained. These observations
localize a real failure boundary, not exclusive causality for every sampled error.

## Meaningful old-fail/new-pass regression

`full_image_real_fill_reclaims_relay_cache`
(`cmd/racer-dataplane/src/peer/fill_admission_tests.rs:374`) assembles three
independent worker admission graphs. Each has production Fill, Flights,
CandidatePolicy, Coordinator peer dispatch, crypto engine, cache, store reader and
writer, Requester, handshake, authenticated TCP transport, and reverse-path relay.
Local coalesced acquisitions elect one supplier and share the actual plaintext and
ciphertext allocations. Their completed pages fill the byte quota before remote
traffic begins. The test checks an independent busy page survives reclamation,
malformed lengths do not evict, and fully busy quota still rejects and releases.

The remote requests use Acquire, not a synthetic LocalPageService. They traverse a
forced source-relay-candidate path. First-ranked candidates acquire cold pages;
final tails use second-ranked candidates and execute a real copy-only predecessor
probe before origin. Assertions require one origin acquisition per cold page,
exact version/page, AEAD authentication, full lengths and SHA-256. The transferred
manifest is parsed and its config/eight-layer descriptors checked. Both concurrency
1 and 4 verify eight layers totaling 536,871,048 bytes, plus manifest/config, and
finish with zero plaintext, ciphertext, dirty, connection, and relay charges.

With the final regression and only the HTTP receive call restored to the base's
`WireBuffer::for_cache`, it fails: `real Fill layer page 0, received 0: non-page
response`. Restoring `receive_buffer` passes both concurrency settings. This
isolates the production receive change rather than merely testing its helper.

```sh
timeout 180s cargo test --manifest-path cmd/racer-dataplane/Cargo.toml \
  --locked --release --all-features --lib full_image_real_fill \
  -- --ignored --nocapture
```

Precise limits: origin bytes and the polling loop are fixtures; no Gantry, Go SDK,
containerd extraction, executable startup, or controller enrollment is involved.
Three real nodes use a three-member ranking, with forced two-hop ingress rather
than a naturally sparse 1500-member topology. Authentication challenges are
pre-established. Requests are page-by-page; this is not the SDK sliding range
stream or a full fleet load test. Real O_DIRECT slabs are opened, but writes stay
queued/disposable in this regression rather than proving disk persistence/recovery.
Native RDMA and the blocked multi-node kind environment are unverified.

## Final checks and handoff

- Release all-feature suite: 557 library, two binary, 18 conformance, nine
  production-graph, and 31 doc tests passed. Twelve library opt-ins ignored by the
  default run; the new regression and three previous full-layer regressions passed
  separately.
- Cargo fmt/check and all-feature/all-target Clippy passed. Clippy has existing
  warnings outside the new code.
- Scoped `make fmt`, `make lint`, and actionlint passed using repository-local
  golangci-lint 2.13.1 and `GO_PACKAGE_PATTERNS=./cmd/racer-loadgen`;
  `GO_PACKAGE_DIRS` was also scoped for fmt. Loadgen Go tests passed.
- Initial compile commands had 300s caps; remaining test/lint commands used
  120-300s caps. No indefinite polling, host tuning, AKS rollout, or push.

Only the **racer-dataplane** production image is affected. Rebuild from the resulting
commit for a parent-owned rollout, then repeat exact-image fleet verification.
This task has not demonstrated post-fix AKS full-image success.
