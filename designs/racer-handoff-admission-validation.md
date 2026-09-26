# Bounded peer admission after settled 05c52 validation

Base: `05c52a46e67e10740cd5c2f075fe17d6535dc652`.
Worktree/branch: `tmp/racer-fix-handoff-admission` / `racer-fix-handoff-admission`.
One new read-only investigator: OpenCode session
`ses_f2359e92bffePxdM2MoCS5ZMRm`. One bug and one commit.

## Settled baseline and actual failure

The immediate 07:35 UTC post-rollout report had manifest 503s on all three
samples. The fresh 07:37 UTC sample instead completed manifest/config on all
three nodes, 18 layers, five premature EOFs, and one layer 503. No complete probe
image passed. Direct background full-pull deltas were errors 11/11/9 and
successes 1/0/0, with stable sampled UIDs/restarts and process starts. Evidence:
`../racer-v2-cluster-validation/tmp/racer-handoff-settled-fresh.{json,md}`.
This does not establish a persistent manifest regression from the handoff.

This worktree's ignored `tmp/discovery.json` records dataplane, Gantry, and
loadgen each 1500/1500 ready/updated/available. Images were dataplane `05c52a46`,
Gantry `4b898044`, and loadgen `05a5703c`. The fresh sample checked loadgen
concurrency 1, layer concurrency 4, eight layers, and verification enabled.

All AKS commands explicitly selected `joolshev-scale-test`, request timeout 20s,
subprocess timeout 30s, and outer caps at most 120s. Captures used existing
net-node pods, selected HTTP headers, and temporary automatically detached
uprobes. No AKS configuration, host sysctl, or durable state was changed.
No credentials or page payloads were retained. No kind cluster was created.

The decisive paired capture is:

- `tmp/unbounded-net-node-5ghsp-bounded-probes-wire.json`
- `tmp/unbounded-net-node-wpf62-bounded-probes-wire.json`
- `tmp/match-failures.py` reconstructs the matching events.

It contains three sequential explicit layer-3 GETs: one complete 59,679,232-byte
success, then 503s in 55.7 ms and 11.3 ms. Layer digest:
`sha256:8952e5cc686eb8a0d53fe830df7804a1ed34e72073da0e867d3072c4f042ed8b`.
The manifest digest is
`sha256:2aa1e8745512f4f76499b8b9a5543642630d3f1120668d8636278537d9aa53a6`.

For the first failed GET:

1. Relay event 246, Unix time 1790409259.563892: request
   `haN8FOqDZDs/8nwNTqothQ==`, attempt `+R8SEs99F1Rn0q+C8Bj8dA==`, page zero,
   ingress `10.243.138.76:49872` to relay `10.244.204.78:8082`.
2. At 1790409259.5649688, the relay's per-endpoint checkout rejects
   `10.243.228.75:8082`, with active=2 and limit=2.
3. Event 247 at 1790409259.5654364 returns signed `overloaded`, binding
   `elQZKEMX4Q+iTzoXDI8QT9mMlcOeCGOmZHI2g0/nigc=`.

The next failed GET repeats the chain in events 248/249: request
`qZssfl00YiGmXEezrCOYbA==`, attempt `FMu+3ioF5J1Dmk50vfn0Bg==`, endpoint
rejection at 1790409259.620832, overload at 1790409259.6215231. Other candidate
attempts also return overload. This proves a failed local admission step on the
explicit GET path, not the cause of every fleet error.

The actual live ELF was inspected before probing. In
`tmp/unbounded-net-node-wpf62-checkout.log`, virtual address `0x202434` compares
active RCX to the per-endpoint limit at RDX; the uprobe file offset is `0x201434`.
Endpoint pointer/length came from the inspected checkout/Endpoint layout.
Times use each node's own wall/monotonic calibration. Captures can drop/reorder
packets; extracted signed fields are not independently signature-verified.

Other captures observed relay-slot, connection, and ciphertext pressure.
The cross-worker mailbox and path-search probes did not record saturation in
their sampled windows. These are scoped observations, not fleet-wide exclusions.
The investigator's short-page reservation and bootstrap-follower hypotheses
were not established as causes and are not implemented here.

## One authorized behavior change

At the base, `src/http/pool.rs:188-189,220-223` explicitly required immediate
Overloaded on a busy neighbor; `:460-464` asserted that contract. Paths below are
relative to `cmd/racer-dataplane`. The user explicitly authorized **Implement
bounded peer wait** after that contract and the correlated failure were reported.
The historical fail-fast check documented in
`designs/racer-fleet-layer-validation.md:98-100` still describes the ordinary
checkout API, which remains tested.

`src/http/pool.rs:229-316` adds peer-only waiting for a selected neighbor's active
slot. The table is capped by queue_entries, with Waiter and RequestContext
admission. Completion/connecting-slot release wakes registered callers outside
the pool borrow; cancellation, drop, and closure reclaim the registration.
The worker tick invokes deadline waking through PeerServer
(`src/app.rs:1018`, `src/peer/server.rs:48-52`). No self-wake polling is used.
`src/peer/transfer.rs:198,309,378` uses it for challenge, handshake, and signed
exchange. The request keeps its original deadline and route attempt; waiting
does not retry a send, increase the two-slot limit, or mint acquisition credits.
Other admission dimensions still fail normally.

## Causal regression and coverage

`sdk_sliding_range_waits_for_busy_peer_slots` runs the actual Go SDK against four
production read/admission graphs and real Unix/TCP transports. Every layer page
is remotely ranked. All peers now have the observed two-slot endpoint limit;
ingress retains the prior seven-page ciphertext quota. Warm and cold runs verify
manifest, config, and eight complete layers totaling 542,950,400 layer bytes,
with the fleet's exact layer lengths but deterministic fixture bytes/digests.
The test asserts that peer waits occurred and that page/connection charges drain.

With only Transfers restored to the old checkout calls, the SDK test failed on
three layers truncated at 16 or 32 MiB. Restoring peer waiting passed both images.
`full_image_through_relay_waits_for_busy_neighbor` independently verifies eight
layers through signed two-link TCP forwarding, four concurrent callers, and two
relay outbound slots. Old checkout failed layer 2/page 0 with a non-page response;
peer waiting passed 536,871,048 bytes and asserted actual relay waiting.

Pool tests cover finite waiter capacity, fail-fast API compatibility, completion
waking without self-spin, cancellation, abandonment, closure, deadline expiry
without active release, and reactor fencing before abandoned connection reuse.
Parallel SDK opt-ins initially collided on fixture slab/output paths; adding the
peer-limit/mode discriminator fixed fixture isolation and both now pass together.

Boundaries: the SDK fixture uses real Fill, placement, signatures, crypto,
RangeStream, Responses, and worker-directory calls, with one worker per node and
a fixture origin adapter. It does not run Gantry or Application startup. The relay
fixture uses a synthetic page service. Existing Go SDK and Gantry mirror tests
pass separately. No new full-stack Gantry or 1500-node acceptance is claimed.
AKS recovery and native RDMA hardware behavior remain unverified.

## Checks and affected image

Every script/build/test had an explicit outer cap at most 300s.

- Release/all-feature suite: 561 library, two binary, 18 conformance, nine
  production-graph, and 31 doc tests passed. One additional fence test then passed
  in the six-test pool suite. Fifteen library opt-ins were skipped by default.
- Five full-image opt-ins and both SDK opt-ins passed separately.
- Cargo fmt/check, all-target/all-feature release Clippy, and diff checks passed.
  Clippy reports existing warnings, including preexisting pool nested-if patterns.
- Scoped make fmt/lint and actionlint passed with GOTOOLCHAIN=go1.26.6 and the
  existing project-local actionlint. The initial default Go 1.27 invocation was
  incompatible with the installed Go-1.26-built linter; selecting the module's
  toolchain resolved it. SDK and Gantry mirror Go tests passed.
- Production Containerfile build passed. Local image
  `racer-dataplane:handoff-admission`, OCI index
  `sha256:90e6b7399a1b4f774047ab949990aa8172b5fe340fd519d116c4370796d4921c`.
  It was built before commit with revision label `unknown`; subsequent changes
  were test-only fixture isolation/fence coverage.

Only **racer-dataplane** needs rebuilding from the resulting commit for a
revision-labeled publication. No push or AKS rollout is performed by this task.
