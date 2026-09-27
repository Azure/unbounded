# Bounded continuation of failed pages

Base: `4400d46905687759876f9b1e1598c7d57b81f4f0`.
Branch/worktree: `racer-fix-continuation-convergence`,
`tmp/racer-fix-continuation-convergence`.
Affected image: **ghcr.io/azure/racer-dataplane**.

## Causal finding

The continuation stream prematurely treats exhaustion of a page's partition as
exhaustion of the logical call. Original ingress credits remain unused. This is
a demonstrated terminal mechanism, not an assertion that every fleet EOF has
the same cause or that this change establishes all-1500 health.

At the base, `cmd/racer-dataplane/src/read/range_stream.rs:190-195` partitions
at most eight attempts and sixteen links into each page. Completed children
return only unused credits at lines 261-269, but a failed front page terminates
the stream at lines 235-239. No attempt is made to use the still-owned ingress
credits. `read/serve.rs:48-49` allocates 32 attempts and 96 links to the call.
`read/candidates.rs:407-465` conservatively reserves route/delegation credits and
refunds only verified non-submission. These security debits are correct and remain
unchanged. Repeated legitimate rejections can exhaust a small page partition well
before its original call deadline or budget.

Paths here and below are relative to `cmd/racer-dataplane/src/` unless prefixed.

## Fresh live evidence

Ignored evidence is retained under this worktree's `tmp/`. All live commands used
explicit `joolshev-scale-test`, kubectl request timeout 20 seconds, local subprocess
timeouts at most 30 seconds, and remote hard timeout 26 seconds. Captures retain
selected headers/bindings and counters, not credentials or payloads.

The supplied parent report
`../racer-v2-cluster-validation/tmp/racer-4400d4-c1-stable-retry1-diagnostic-report.md:13-18,50-53,568-570`
records 1500 error nodes, 22.0625 successes/s, 457.204164 errors/s, and 15/24 exact
layers passing. Its companion `exact.json` and `accept.json` identify c1, four
concurrent layers, eight verified layers, and the expected `4400d469` deployment.

Fresh captures confirm that deployment and add internal terminal evidence:

- `tmp/tagged-unbounded-net-node-rsq6l-1790533974.json` records layer
  `2115d07a4bfcddb9d1c4322c22fda03d1f24032560ae938b8c8d224eef408c1d` through
  Gantry `10.240.102.61`, HTTP 200, **16,777,216 / 59,757,056 bytes**.
  Foreground EOF is at Unix `1790533961.9995086`. Signed request
  **s6wsJW9bvDGldWdDzi+5/g==**, page 1, reaches **0 attempts / 0 links** at
  `1790533961.9989731`. Its page 2 receives a signed page response, while page 1
  encounters multiple downstream and adjacent non-submission receipts. The joined
  headers and terminal probes are in the companion `-terminal.json`.
- A second probe reads the stream budget at its error termination branch.
  `tmp/tagged-unbounded-net-node-rsq6l-1790534381.json-terminal.json` joins request
  **wawBJlFHu9XP4w4csMx7Ug==** to layer `a266e641...`, page 3: the page hits
  **1 attempt / 0 links**, then the stream terminates with **17 attempts / 64
  links** still owned. Other complete joined traces terminate with 12/60 and 12/55.
- `tmp/tagged-unbounded-net-node-rsq6l-1790534608.json-terminal.json` joins layer
  `2115d07a...` request **kJY7nZirdFVApo0oDI+DIA==**, page 2: **2/0** at the
  child terminal, then **22/76** at stream termination, 150 microseconds later.
  This request is background traffic, not the foreground request in that capture.
  The foreground stops at 32 MiB; a nearby stream probe has 24/80 but its headers
  were not captured, so that correlation alone is not treated as proof.

Probe offsets were derived from the running `4400d469` executable, not reused from
the previous image: `tmp/inspect-unbounded-net-node-{t7ssg,rsq6l}-*.json` records its
ELF executable mapping (virtual minus file offset `0x1000`). Disassembly is in
`tmp/disassemble-unbounded-net-node-t7ssg-1790533904.json:1790-1920,2790-3057`:
zero-attempt test virtual `0x26ef6a`, hop exhaustion `0x26ef7b`, stream terminal
`0x2701dd`. The request closure has scope/budget at +32/+40; the stream has
budget/scope at +256/+280. Budget attempt/link fields are +16/+23. Probes join the
16-byte request ID with signed-header bindings. Capture does not reconstruct every
fragmented response or prove why every downstream unavailable was emitted.

`tmp/canonical-routes.json` computes canonical routes from the live 1500 node IDs.
It shows substantial convergence (up to 223 source routes through one transit
vertex for a page-1 candidate). This is a topology calculation, not a measurement
of simultaneous traffic or proof that changing route tie-breaking would fix EOF.
Route selection is therefore not changed by this fix.

Cleanup records `tmp/cleanup-unbounded-net-node-{t7ssg,rsq6l}-*.json` show no
bpftrace processes and empty uprobe registrations. No cluster rollout or
configuration mutation was performed.

## Correction and bounds

- `read/range_stream.rs:169`: stop extending the speculative window while an
  already-completed failed page needs progress. Preserve pending and successful
  sibling pages rather than restarting the range.
- A completed transient front-page failure can start the **same pinned page** via
  the existing WorkerDirectory and production Fill, using only the stream's
  still-owned credits. Each continuation first spends one original attempt,
  including failures that do no I/O. The child remains capped at eight attempts
  and sixteen links. Total ingress credits, route ceilings, deadline, origin
  authority, context, admission quotas, and signature rules are unchanged.
- `read/range_stream.rs:317`: only unavailable, hop exhaustion, overload and I/O
  failures qualify. Security/integrity/version/origin rejection/deadline/cancellation
  errors terminate. Lost or incomplete responses do not restore spent credits.
- Replacement futures remain in the existing bounded window across `next_slice`
  abandonment. Completed child credits are reunited once. Actual Fill operations
  retain their existing completion ownership. No new durable or distributed state.

The prior comment that every late error terminates the stream described the old
behavior. It is deliberately corrected, along with `racer-runtime-interfaces.md`,
to allow bounded continuation before emitting a terminal error. A preliminary
implementation that continued page 1 but prefetched page 3 ahead of failed page 2
still failed at 32 MiB (`tmp/continuation-debug.log`). Prioritizing already-failed
pages is necessary to avoid wasting the remaining original credits.

## Production old-fail/new-pass

`peer/production_stream_tests.rs:84` runs the actual Go 1.26.6 SDK against production
Coordinator, RangeStream, WorkerDirectory, Fill, elections, AEAD, signed TCP peer
servers and cut-through relays. Its origin adapter supplies bytes only.

The topology has **1500 members, six active peers**, at positions
`[3,0,1,18,19,2]`, including intersecting paths `3->0->1->18` and `19->1->0->2`.
Weighted membership keeps the three candidates active; ranking is recomputed by
production Placement. Inactive endpoints are unavailable. This does not model 1500
simultaneously active dataplanes or uniform fleet shares. The shared fixture's path
search work allowance is 150,000 for the larger graph, identical on both sides.

Each run reads manifest/config and three rounds of all eight full-sized layers,
four layers concurrently, exact length and SHA-256 checked. Layer sizes match the
live image (542,950,400 layer bytes); fixture content and manifest/config differ.
Every four-layer batch experiences 2200 ms of Relay-capacity contention, exceeding
the original child partition's retry horizon. Background 16 MiB production Fill
transfers cross the SDK routes throughout the run, including after each pressure
interval. Resource ceilings remain 8 Relay, 2 per-peer connections and bounded
worker page memory. Synthetic reservation pressure is explicit; it is not claimed
to reproduce the full fleet's natural arrival process.

- **Exact base:** `tmp/exact-base.log:104-193`. An exact source archive of
  `4400d469` with only the new test fixtures copied in fails **0/3 images,
  0/24 layers**, all at 16 MiB. The run stops after warm failure; no cold-base pass
  or count is claimed. The base production RangeStream and Fill are unchanged.
- **Fixed:** `tmp/continuation-final.log:5-30`: **6/6 images, 48/48 layers**,
  zero layer failures across warm/cold. **48 bounded page continuations per run**;
  **546 warm / 530 cold crossing transfers** complete. All final plaintext,
  ciphertext, dirty, Relay, Connection, Waiter and Flight charges are zero.
  Final verification after preserving the remainder of a locally failed range
  also passes (`tmp/continuation-verified.log`): 6/6 images, 48/48 layers,
  48 continuations per run, 532 warm / 540 cold crossing transfers.
- Disabling only continuation also fails (`tmp/continuation-old.log`), separating
  the correction from test topology, receipts or timeouts. The test never retries
  a failed SDK object or weakens digest/length assertions.

## Verification

Every test/build command has a process-group hard bound of 285 seconds. Per-command
arguments, status and elapsed time are retained as `tmp/*.result.json`.

- `full-final.log`: **587 library, 2 binary, 18 conformance, 11 production and
  31 doctests pass**, all features, release, serial. Ignored tests remain explicit.
- `read/range_stream.rs:393,490`: actual stream failure/cancellation/deadline tests
  and credit/error classification checks cover finite no-I/O retries, security and
  pin errors, exhausted budgets and unchanged deadlines. Existing tests cover
  out-of-order completion, one-time credit return, abandoned outstanding work,
  pin validation and ordered multi-page delivery.
- All existing ignored SDK stream scenarios pass: `sdk-existing-full.log` (5),
  `sdk-sliding.log` (2), `sdk-sustained.log` (3), `sdk-routing.log` (1).
- `intersecting.log`: all nine existing production intersecting-route tests pass.
- `go.log`: Go 1.26.6 SDK, Gantry Racer adapter and mirror tests pass.
- `fmt.log`: scoped `make fmt GO_PACKAGE_DIRS=pkg/racersdk
  GO_PACKAGE_PATTERNS=./pkg/racersdk/...`, Go 1.26.6, passes. Cargo formatting passes.
- `clippy.log`: all-feature/all-target release Clippy completes with existing
  warnings. `release-build.log`: locked image-default release build passes.

All-1500 acceptance remains outstanding. The parent owns publication, rollout and
the settled fleet acceptance run. This change fixes the demonstrated unused-budget
terminal mechanism; it does not claim recovery from permanent route failure,
unbounded sustained overload, exhausted total credits or expired deadlines.
