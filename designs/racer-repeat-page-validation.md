# Repeated PAGE1 overload: neighbor congestion feedback

## Scope and result

Base: `5e5b723cca84decde5e34b1f2264afd7aaee3dae`.
Branch/worktree: `racer-fix-repeat-page`, `tmp/racer-fix-repeat-page`.
Affected image: **ghcr.io/azure/racer-dataplane**.

Implemented and locally verified one causal correction: transit now remembers an
authenticated immediate neighbor's own overload briefly, so subsequent independent
arrivals can use an available incident edge instead of repeatedly hitting that
neighbor. Full healthy **all-1500-node acceptance is still outstanding**. The parent
owns image rollout and that acceptance run. No image was pushed or deployed here.

## Live evidence

All evidence below is in this worktree's ignored `tmp/` directory unless noted.
The supplied acceptance report remains the fleet baseline:
`../racer-v2-cluster-validation/tmp/racer-5e5b72-c1-stable-retry1-diagnostic-report.md:9-56`
records 1500 ready clients/dataplanes/Gantry pods, unchanged restart counters,
4.470833 successes/s and 884.9 errors/s. Its lines 109-119 record layer 0 stopping
at exactly 16,777,216 of 59,757,056 bytes. This investigation did not reduce clients.

The exact layer is
`sha256:2115d07a4bfcddb9d1c4322c22fda03d1f24032560ae938b8c8d224eef408c1d`.

1. `capture-unbounded-net-node-t7ssg.json` records request
   `9+b8KtOJ772Z7kq1qqX5tw==`, PAGE1, at 15:47:12.258 UTC. Its three candidate
   attempts returned signed overload at 12.280, 12.488, and 12.729. Candidates
   were `4d68f064-4d3e-4a5a-92f5-9b221a52458c`,
   `8a14c117-fbd8-49b6-88d4-8da6303f1070`, and
   `aede9a49-7e8d-4f78-9062-c2d3de968acd`; transferred acquisition credits were
   3, 2, and 0. This capture also has successful PAGE1 responses from the first
   and second candidates. The foreground layer completed with its exact digest.
2. `capture-unbounded-net-node-rjbfc.json` follows request
   `sjg4GeKff/sr9XATCXeKDw==` through upstream relay
   `0734f962-ba2a-4ace-8d7b-4073b14545f0` to
   `006b43c0-4004-40f6-b0df-5c6b677f8cae`, with an overload originating farther
   downstream at `06f63f3c-4a34-4537-8aa9-e328e4a6f605`. Both foreground GETs
   during this capture stopped at exactly one page. The fix deliberately does
   not treat that distant overload as proof the immediate neighbor is congested.
3. `tagged-final-unbounded-net-node-6659p.json` instruments the actual Relay
   reservation failure on `racer-dataplane-vqv4m`, node
   `aks-ddsv6-84072342-vmss0000df`, running base image 5e5b723c. It records
   **568 Relay rejections at used/limit 8/8**, with **126 exact layer PAGE1
   requests correlated to signed overload responses**. Correlation uses request
   IDs plus SHA-256 canonical signed-head request bindings, not just timestamps.
   Derived records are `tagged-final-unbounded-net-node-6659p.json-{matches,responses}.json`.
   Example `WAXV/JYzcvihbZJLvDGkSg==`, attempt `gwxDDlj1Z8HXDD8SOrW8dw==`,
   arrives at 16:11:58.6318 UTC and receives overload at 58.6573. The signed
   response originates at the immediate neighbor `006b43c0...` itself.
4. The simultaneous ingress capture `capture-final-unbounded-net-node-t7ssg.json`
   reproduces HTTP 200 with exactly 16,777,216/59,757,056 bytes in 0.378 seconds.
   PAGE1 request `otep1lVVDw658PbptlnyBQ==` terminates after its captured second
   and third candidates return overload; the first attempt predates capture.
   This request is not claimed to be the same request as the instrumented example.

Probe validation: `inspect-unbounded-net-node-rjbfc.json` records the executable
LOAD offset/address difference of 0x1000. The instruction at 0x2c200a is the
error branch after `Admission::reserve_inner` for ResourceClass::Relay, proved by
`disassemble-unbounded-net-node-6659p.json:1004-1024`. The uprobe uses file offset
0x2c100a. RequestScope and admission fields are read only; no payloads, credentials,
or raw packet bodies are retained. Initial resource-return probes were inconclusive
and are not evidence of absent pressure; the final call-site probe is authoritative.

### Causal distinction

The retained page is not persistently absent: both candidates serve it in the
captures. The demonstrated failure is **Relay admission**, not AEAD corruption,
origin rejection, version absence, or a deadline expiry. Candidate credits amplify
the loss: `cmd/racer-dataplane/src/read/candidates.rs:360-397` debits and partitions
them before sending, and lines 208-275 exhaust the ordered candidate sequence.
The prior correction only saw the sender's own endpoint slots. Base
`cmd/racer-dataplane/src/peer/relay.rs:66-93` had no response-derived congestion
state, so repeated arrivals continued to select an overloaded remote relay.

## Correction and contracts

- `cmd/racer-dataplane/src/security/forwarding.rs:158-165` exposes an overload hint
  only after descriptor, request-binding, signature and reverse-chain verification,
  and only for the authenticated responder itself.
- `cmd/racer-dataplane/src/peer/stream.rs:249-261` carries that verified hint with
  the ordinary streamed response. It does not inspect or decrypt transit payloads.
- `cmd/racer-dataplane/src/peer/relay.rs:73-105` combines current local slot pressure
  and unexpired immediate-neighbor feedback in the existing bounded route search.
  Lines 149-155 record the hint after verification. Lines 274-315 bound it to 36
  entries, scope it to membership, and expire it after 250 ms, matching the existing
  acquisition overload-backoff scale without extending any request deadline.
- `cmd/racer-dataplane/src/topology/ALGORITHM_V1.md:106-121` documents this internal
  design correction. Lexicographic shortest paths still apply in the available
  incident-edge subgraph. Placement, security, original signed failure, request
  and link credits, all resource ceilings, and completion fences are preserved.

No failed envelope is replayed at transit. A later independent request benefits
from the hint; an overloaded whole graph can still fail. Expiry and topology changes
restore eligibility. An overload from a distant responder does not poison a healthy
adjacent edge. This is a local congestion hint, not distributed node health.

## Verification

Every build/test invocation was bounded to at most 300 seconds.

- **Before/after production Fill proof:**
  `sustained_fills_avoid_repeated_remote_relay_overload`, introduced at
  `cmd/racer-dataplane/src/peer/intersecting_flow_tests.rs:162-166`, fails with
  `Unavailable` on the unmodified production base. The final regression also fails
  with only feedback recording disabled (`tmp/old-fail.log`). Restoring feedback
  passes (`tmp/intersecting.log`). Seven active peer graphs use actual 1500-member
  radix-18 routing, production signed forwarding, Fill, AEAD, eviction and page
  delivery. Two concurrent arrivals perform 12 rounds of full 16 MiB pages.
  A real signed probe establishes 8/8 downstream admission pressure before later
  requests. The fixture releases competing quota owners after 150 ms and checks
  zero final plaintext/ciphertext/connection/Relay/Flight/waiter accounting.
- **All nine intersecting-flow tests pass**, including the prior local-edge hotspot,
  reverse-capacity cases and incoming keepalive churn (`tmp/intersecting.log`).
- **SDK compatibility:** existing eight production SDK scenarios pass warm/cold
  (`tmp/sdk.log`). New five-peer remote-congestion scenario passes warm/cold, each
  three complete eight-layer rounds with four concurrent layers, exact byte counts
  and SHA-256 verification (`tmp/sdk-remote.log`). Layer bytes total 542,950,400 per
  round; fixture manifest/config differ from the live 542,952,560-byte image.
  **The new SDK case also passes with feedback disabled** (`tmp/sdk-remote-old.log`):
  it is compatibility coverage, not the old-fail/new-pass proof.
- **Full all-feature release suite:** 581 library, 2 binary, 18 conformance,
  11 production and 31 doctests pass (`tmp/final-full.log`). Expensive/hardware
  tests remain explicitly ignored by default; relevant ignored cases ran above.
- Security coverage checks responder attribution, non-overload rejection, signature,
  binding, lengths and replay. Congestion coverage checks expiry, capacity, changed
  membership and removed neighbors in the full library suite.
- Go 1.26.6 SDK and Gantry Racer adapter tests pass (`tmp/go.log`). Scoped
  `make fmt GO_PACKAGE_DIRS=pkg/racersdk GO_PACKAGE_PATTERNS=./pkg/racersdk/...`
  passes with Go 1.26.6 (`tmp/fmt.log`); no Go files changed. Cargo formatting and
  diff whitespace checks pass. Image-default release binary build passes (`tmp/build.log`).

## Limits and cleanup

The local proof reproduces the measured admission boundary using bounded competing
quota owners, not 1500 active dataplanes or a sustained fleet traffic generator.
The SDK compatibility fixture uses five peers; it does not prove all-node goodput.
No new kind cluster was created; the supplied multi-dataplane kind inotify limitation
was not bypassed. Post-fix fleet health and full-image acceptance remain unmeasured.

All kubectl calls used explicit `joolshev-scale-test`, `--request-timeout=20s`,
30-second subprocess bounds and shorter remote timeouts. Cleanup files
`tmp/cleanup-unbounded-net-node-{t7ssg,7kntc,rjbfc,6659p}.json` confirm no bpftrace
processes and empty uprobe registrations on every inspected host. No AKS config,
sysctl, durable data, or deployment mutations were performed.
