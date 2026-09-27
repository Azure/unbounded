# Acquisition progress after pre-forward rejection

## Result and scope

Base: `5585464b5f6c4112fa7c7fd655c214e762518767`.
Branch/worktree: `racer-fix-acquisition-progress`, `tmp/racer-fix-acquisition-progress`.
Affected image: **ghcr.io/azure/racer-dataplane**. The image-default release build
uses `images/racer-dataplane/Containerfile:18-31`.

Implemented one causal correction: preserve proven unused acquisition delegation
after transit rejects an attempt before forwarding, then let its existing owner
retry under its original budgets. The full production SDK reproduction fails with
receipt emission disabled and passes with it enabled, including repeated pressure,
warm and cold eight-layer reads. **All-1500-node health is not established by this
local result.** The parent owns rollout, settled c1/layer4 acceptance on all nodes,
and subsequent stock-64 validation. No image was pushed or deployed here.

## Live failure chain

Ignored evidence is under this worktree's `tmp/` directory. Every remote command
used explicit `joolshev-scale-test`, a 20-second kubectl request timeout, a 30-second
subprocess timeout, and a 27-second remote timeout. Captures persist selected signed
header fields, not credentials or packet bodies.

The supplied fleet baseline remains
`../racer-v2-cluster-validation/tmp/racer-558546-c1-stable-retry1-diagnostic-report.md:13-59`:
1500 active/ready clients, 1500 error nodes, 454 nodes with positive success,
3.158333 successes/s and 893.570833 errors/s. Lines 569-574 record unchanged
restarts and only 13/24 successful sampled layers. No reduced-load acceptance is
substituted for this baseline.

1. At 16:38:04 UTC, the first capture on `unbounded-net-node-p2279`, colocated with
   `racer-dataplane-9rdsr` and `gantry-ff4wk`, reproduced HTTP 200 ending at exactly
   16,777,216/59,757,056 bytes for layer
   `2115d07a4bfcddb9d1c4322c22fda03d1f24032560ae938b8c8d224eef408c1d`.
   The derived first-capture chain was subsequently superseded by another capture;
   the complete independently retained chain and instrumented evidence below are
   the reproducible artifacts for the terminal-resource attribution.
2. `tmp/terminal-chain.json` extracts request `4T7mprVmrzXMnLz/31cfwg==` from
   `tmp/tagged-unbounded-net-node-c8z4v.json`. Its PAGE2 second candidate reaches
   relay `0378e429-6e94-492b-8423-525ecd51d453` at 16:43:42.733 UTC. The live
   reservation probe reports Relay **8/8** at 42.736939. The signed response is
   `overloaded`, bound to that exact original signed request head. Its PAGE1
   second candidate fails the same reservation at 42.780896 and returns the same
   signed outcome. Canonical signed-head SHA-256 bindings join requests/responses;
   request IDs join the resource probe, and these two times distinguish the pages.
3. The simultaneous ingress capture, summarized during the session, shows the
   complete PAGE1 candidate sequence: attempts at 42.515, 42.774, and 43.035 UTC,
   all returning overload, with delegated credits **3, 2, 0**. PAGE2 also exhausts
   all three candidates. Other requests in the same capture receive successful
   pages from the same candidate set. This is transient admission loss, not proof
   of persistent content absence. The instrumented capture remains available;
   the ingress convenience filename was replaced by the later capture below.
4. The probe is not inferred from an error counter. `tmp/inspect-unbounded-net-node-t7ssg.json`
   verifies the 558546 executable's LOAD offset difference of 0x1000.
   `tmp/disassemble-unbounded-net-node-t7ssg.json:973-991` shows the Relay class
   argument 10, the `Admission::reserve_inner` call, and its error branch at
   virtual address 0x2ae929. The uprobe uses file offset **0x2ad929**. It reads
   the request scope and admission counters without modifying them. The complete
   trace contains 632 reservation failures, all at 8/8. Broad ID matches include
   unrelated pages/outcomes; only the exact chain above is causal attribution.
5. `tmp/capture-unbounded-net-node-p2279-1790528398.json` retains a later foreground
   failure: HTTP 200, exactly 16 MiB, 0.441 seconds, and request
   `DV13t6iU59nlDEln97dACQ==` with captured second/third PAGE1 overloads. Its first
   attempt predates capture. A following request successfully fetches PAGE1 from
   the third candidate. This foreground capture is separate from the instrumented
   terminal chain; neither is a full-image success claim.

## Mechanism and correction

The SDK opens a single pinned continuation for remaining pages and preserves late
errors (`pkg/racersdk/value.go:96-175`). Gantry aborts a failed body after exposing
earlier bytes (`internal/gantry/mirror/racer.go:129-151`). The sliding window lends
eight attempts and sixteen links per admitted page from one original ingress
budget (`cmd/racer-dataplane/src/read/range_stream.rs:185-201`).

At the base, `read/candidates.rs:360-397` spends a send attempt and partitions
delegation before I/O; `:208-275` visits the three ranked candidates once. A relay
can reject before that delegated acquisition ever reaches its destination, yet
the owner discards the allowance. Three transient failures can therefore end a
page while healthy copies remain available. Raising deadlines alone cannot help
that already terminal sequence.

All following abbreviated source paths are under `cmd/racer-dataplane/src/`:

- `peer/relay.rs:68-175` distinguishes a pre-forward rejection from an ambiguous
  downstream result. `peer/stream.rs:347-354` sets submission before polling the
  downstream send, conservatively covering partial writes and late errors.
- `security/protocol.rs:417` adds the signed zero-body `not-forwarded` outcome.
  `security/forwarding.rs:179-206,719-726` verifies receipt authority and exposes
  only original-requester reconciliation. Destination-originated receipts and
  paths containing the destination are rejected.
- `read/candidates.rs:209-282,385-434` retains the owned child allowance until
  completion and reunites it only on the verified receipt. Actual forward edges
  and the send attempt remain spent. Ranked candidates may be revisited only with
  remaining original credits, after the existing jittered backoff. Outcome history
  remains bounded to one candidate pass. No limits or deadlines increase.
- `designs/racer-peer-security.md` replaces the former no-receipt design rule with
  this explicit exception. Ordinary overload does not prove non-submission: it
  can arise after downstream I/O. It never refunds credits. Unknown outcomes are
  rejected by older decoders; mixed-version nodes fail closed and require upgrade
  to benefit. This is a closed-vocabulary wire extension, not a timeout increase.

## Causal reproduction and verification

`peer/production_stream_tests.rs:72` introduces
`sdk_sustained_full_images_retry_proven_unspent_acquisition`. Four real peer graphs
run production Fill, signed HTTP transit, AEAD, sliding delivery, and the actual
Go SDK. Placement excludes ingress for each layer page. Eight live benchmark layer
sizes total **542,950,400 bytes**, with four concurrent layer streams. Synthetic
manifest/config bytes differ from the live complete image's 542,952,560 bytes.

Each four-layer batch crosses actual 8/8 Relay admission pressure lasting 650 ms,
longer than the old three-candidate backoff sequence. Six pressure episodes are
asserted across three full eight-layer rounds, warm and cold. This models repeated
fleet-observed transient admission and verifies convergence through full layers;
it is four active peers, not 1500 active dataplanes or a fleet fairness proof.

- `tmp/sdk-old.log`: receipt emission alone disabled, restoring base overload
  behavior, fails all eight layer sizes across three rounds at 16 MiB. This is a
  controlled old-path comparison, not an exact-base checkout test.
- `tmp/sdk-new.log`, `tmp/sdk-final-retry.log`: receipt enabled, three complete
  warm and cold rounds pass exact byte counts/SHA-256 and zero final page, dirty,
  Relay, Connection, Waiter and Flight accounting.
- Owner regression at `read/candidates.rs:533` covers success after three rejects,
  eight-attempt exhaustion, link exhaustion, ordinary overload, lost response,
  corrupt signature, cancellation, deadline, abandonment, and zero output charges.
- `security/forwarding.rs:1345` tests exact binding, multi-hop receipt, replay,
  signature corruption, expiry, destination rejection, and no ordinary refund.
- `peer/stream.rs:41` uses real sockets to distinguish pre-checkout rejection from
  request-received/downstream-disconnect ambiguity, with final zero accounting.
- `tmp/full-final.log`: **584 library, 2 binary, 18 conformance, 11 production,
  31 doctests pass**, all features/release, serial. The earlier parallel suite had
  three unrelated persistence-test deadline failures; the final serial run passes.
- `tmp/sdk-existing.log`: seven existing SDK scenarios pass warm/cold.
  `tmp/sdk-sustained-compat.log`: both previous sustained SDK scenarios pass.
  `tmp/intersecting.log`: nine real 1500-member-route/intersecting tests pass.
  `tmp/crossing.log`: two crossing Fill tests, including corruption, pass.
- Go 1.26.6 SDK and Gantry adapter/mirror tests pass (`tmp/go.log`). Scoped
  `make fmt GO_PACKAGE_DIRS=pkg/racersdk GO_PACKAGE_PATTERNS=./pkg/racersdk/...`
  passes with compatible golangci-lint (`tmp/fmt.log`); fixture gofumpt passes.
- All-target/all-feature Clippy completes with existing warnings
  (`tmp/clippy-final.log`); image-default locked release build passes (`tmp/build.log`).
  Interrupted SDK/Clippy attempts hit host disk exhaustion. Only this worktree's
  disposable Cargo debug artifacts were reclaimed; both checks subsequently pass.

All test/build invocations had hard bounds below 300 seconds. Cleanup artifacts
`tmp/cleanup-unbounded-net-node-{c8z4v,p2279,t7ssg}.json` confirm no bpftrace processes
and empty uprobe registrations on every inspected host. No agents, additional
worktrees, main-checkout edits, push, AKS configuration changes, sysctl changes,
or durable-state deletion were performed.
