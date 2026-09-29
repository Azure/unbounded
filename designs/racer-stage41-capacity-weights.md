# Stage41: capability-weighted ownership

## Research checkpoint

Inspected controller `0c23d0e93338979d41b9f78568ea08f7587fa0bc`,
dataplane `5e1a4554f8034fef676d5ca91315da80646e5b71`, and operator
`d3f82a0fb3be3b2bcfbb4a57b640acf73e2a7795`, not the concurrent parent's
new protocol. Worktree base is the exact deployed dataplane revision. The
controller's `internal/racer` tree is identical there. Read the stage28
eleven-node list, latest stage40 result, repository AGENTS.md, and
`~/design.md:70-74` after inspecting implementation and test assertions.

Existing supported configuration is sufficient: administrator Node annotation
`racer.unbounded-cloud.io/shares: "1"` overrides the authenticated enrollment
proposal, which remains 4. See controller `internal/racer/membership.go:43-69`
and `bootstrap.go:152-207`; the authenticated proposal/override assertions are
in `server_test.go:236-285`. No new operator/controller/registration feature,
forked DaemonSet, security bypass, node exclusion, or image build is needed.

The default remains 4 for the other 1489 nodes. All 1500 retain membership and
local pulls. The radix-18 relay graph uses sorted node positions and count, not
shares (`cmd/racer-dataplane/src/topology/graph.rs:11-36`). Weighted placement
uses cost/shares (`topology/placement.rs:64-110`). Owner relief must not be
reported as equivalent relay relief or a fourfold throughput improvement.

No live configuration, NIC setting, concurrency, process affinity, or image has
been changed. No subagent tool is available in this session.

## Decision

**Reuse the existing Node annotation; do not implement or release a new binary.**
All eleven UID/resourceVersion-guarded patches passed live Kubernetes server
admission dry-run. `designs/racer-stage41-preflight.json` records them and the
1500/1500 Ready/Available status of all three workload DaemonSets. Current
controller/operator image inspection also confirmed 0c23/d3. The cache UID is
still `79f749cf-6c6c-4e26-928f-2057ca9b4279`.

This configuration is ready for an **owner-load relief evaluation on the current
pod network**, not an underlay qualification or a forecast that all nodes can
saturate their NICs. The capacity model does not justify deploying an image or
repeating the failed underlay experiment. No configuration was applied here.

### Exact control/security contract

- `RACER_SHARES` is a scalar per-process enrollment proposal, default 4
  (`cmd/racer-dataplane/src/config.rs:200-207`, `app.rs:308`,
  `control/enrollment.rs:372`). The shared runtime ConfigMap cannot distinguish
  eleven Nodes. CPU limits and cache-size quotas do not supply owner shares.
- The explicit administrator Node annotation takes precedence, including at
  later reenrollment. Do not edit `enrolled-shares` or `last-admitted-member`.
  Positive u32 shares are accepted; zero is invalid, not a receiver-only mode
  (`internal/racer/membership.go:43-69`, `bootstrap.go:173-202`).
- Enrollment checks TokenReview audience, live Pod UID, ServiceAccount/workload,
  assigned Node UID, and CSR possession. Endpoint selection still requires the
  managed DaemonSet owner UID (`bootstrap.go:30-114,152-207`,
  `membership.go:108-153`). An independently forked DaemonSet is not equivalent.
- Node annotation changes trigger topology reconciliation, and content/membership
  counters advance only through normal publication (`events.go:63-82`,
  `publications.go:101-160`). Old accepted values survive invalid updates, and
  UID-bound history survives controller restart (`membership.go:190-207`).
- Precision about "signed membership": this release serves canonical,
hash/version-checked membership over authenticated HTTPS/mTLS; the snapshot
  JSON is **not a separately signed membership envelope**. Signed node
  certificates and signed peer forwarding remain intact. Snapshot authorization
  and delivery are at `server.go:422-513`; dataplane membership/hash/replay checks
  are at `control/snapshot.rs:170-228`; signed monotonic route validation is at
  `topology/paths.rs:30-78`. No new signature or parent protocol is introduced.
- Existing RBAC grants controller Node get/list/watch/patch. Runtime-write
  admission restricts controller ConfigMap/Secret writes, not administrator Node
  annotations (`deploy/racer/rbac.yaml.tmpl:20-29`,
  `create-restriction.yaml.tmpl:9-24`). Actual administrator authorization and
  server dry-run passed for every target; this is not inferred from RBAC alone.

Prose drift found, not used as authority: `designs/racer-control-plane.md:147-153`
describes accepted history as process-local, whereas deployed
`membership.go:190-207` restores UID-bound Node annotations. No behavior was
changed to follow that stale description. `~/design.md:70-74` calls for default-4
capability shares with an environment proposal; existing code already supplies
that plus the administrator override. `~/design/AGENTS.md` does not exist;
repository AGENTS.md was read.

## Actual catalog placement, not the proportional expectation

Inputs: retained stage16 origin verification (all 512 manifests and blob
descriptors), stage40 recovery membership 4974/sequence4975, stage40 Node UID
inventory, and the stage28 eleven-node cohort. Loadgen208 uses a shuffled complete
catalog pass (`cmd/racer-loadgen/catalog.go:77-97` at 2082700d), not the parent's
newer popularity profile. Gantry b8 maps the blob SHA256 directly to the cache key
(`internal/gantry/racer/racer.go:40-47`). The cache UID is part of slot hashing.

`hack/scripts/racer-capacity-forecast.py` reproduces the deployed SHA256 framing,
20-bit slot, and Q32.32 exponential HRW comparison. It checks all three deployed
slot/ranking golden vectors and four cost vectors
(`topology/hash.rs:11-20`, `placement.rs:365-396`). The optimized ranking retains
all eleven changed candidates plus healthy top-three and cutoff ties. It does
not approximate placement with `11/1500` or floating-point logarithms.

The **4096 unique layers, 274,439,398,400 bytes, 18,417 pages, 18,249 distinct
slots** yield these primary-owner bytes. Last pages use actual sizes, not 16 MiB
rounding. These are assigned layer bytes, not measured disk occupancy or served
bytes. Manifest/config payload and metadata request counts are omitted from the
bandwidth model (the former are only 1,105,920 bytes per catalog pass).

| ddsv6 suffix | Primary bytes, shares4 | Primary bytes, shares1 | Pages, before -> after | Owner Gbit/s at 586 GB/s, before -> after | Transit Gbit/s after |
| --- | ---: | ---: | ---: | ---: | ---: |
| 00000h | 135708672 | 50331648 | 10 -> 3 | 2.317 -> 0.859 | 6.058 |
| 00002i | 151949824 | 34819584 | 11 -> 3 | 2.594 -> 0.594 | 4.678 |
| 00003z | 190529536 | 63029760 | 14 -> 5 | 3.252 -> 1.076 | 4.502 |
| 00005f | 252459520 | 83998720 | 17 -> 6 | 4.310 -> 1.434 | 4.599 |
| 000066 | 198722560 | 93999616 | 14 -> 6 | 3.392 -> 1.605 | 5.912 |
| 00006d | 145172992 | 57576960 | 10 -> 4 | 2.478 -> 0.983 | 5.824 |
| 000078 | 202808832 | 106393600 | 13 -> 7 | 3.462 -> 1.816 | 4.809 |
| 0000b1 | 134217728 | 67108864 | 8 -> 4 | 2.291 -> 1.146 | 5.791 |
| 0000bl | 272494592 | 100663296 | 18 -> 6 | 4.652 -> 1.718 | 5.144 |
| 0000cp | 197098496 | 33554432 | 12 -> 2 | 3.365 -> 0.573 | 4.700 |
| 0000d9 | 134977536 | 28619776 | 9 -> 2 | 2.304 -> 0.489 | 5.843 |

Exact cohort primary bytes: **2,016,140,288 -> 720,096,256**, a **64.2834%**
reduction. Only **1,296,044,032 bytes (0.472251% of the catalog)** change primary
owner. All other owners gain or retain assigned bytes; none lose. Cohort top-three
candidate bytes decrease from **6,197,919,232 -> 1,876,171,264**; this is candidate
coverage, not a promise that three physical replicas are present.

In the infinite-key proportional model, total shares go 6000 -> 5967, cohort
fraction 44/6000 -> 11/5967, a 74.86% decrease. The real catalog is small enough
that the exact 64.28% result is materially different. The healthy cohort gains
only about 0.476% in aggregate assigned bytes, but individual page-sized gains
are uneven. There is no global fourfold speedup.

## Routing and usable receiver capacity

The live override selects routing v3. Its hash distributes requests over all
equal-cost next hops and relays reselect at each hop
(`topology/paths.rs:305-335`, `equal_cost.rs:1-8,93-137`). Shares do not enter the
graph. The eleven retain 35 or 36 neighbors. All 1500 sources are included.

The offline model runs a destination-rooted BFS on the actual sorted-UID graph
and propagates expected uniform-source demand over every shortest next hop.
Destination demand is weighted by the actual catalog primary bytes above. This
models healthy-v3 primary hits, no retained local copies, no coalescing, no
failures/retries, and equal full-catalog demand per receiver. It is an expectation
over request/attempt hashes, **not actual flow telemetry** or a simulation of
admission/deadlines. Alternate candidates, local retained copies, partial reads,
and retries can change the traffic. The lookup really tries ordered candidates
(`read/candidates.rs:180-245`), so primary demand is the correct starting model,
not proof every request reaches its primary.

At the 586 GB/s retention target:

- Cohort owner TX: **34.417 -> 12.293 Gbit/s** total.
- Cohort relay payload TX (also relay RX): **57.778 -> 57.859 Gbit/s** total.
  Owner weighting barely changes transit; some targets increase slightly.
- Mean payload links: **2.550939 -> 2.550953**. The graph is unchanged; the tiny
  difference comes from which destinations own the moved pages.
- Post-change per-node owner+relay TX is **5.273-7.516 Gbit/s**. Relay alone is
  **4.502-6.058 Gbit/s**. Both already exceed the stated approximately 2 Gbit/s
  pod-network and 4 Gbit/s underlay capabilities. Treat those observations as
  operating envelopes, not proven immutable hardware ceilings: stage40 recorded
  4.424-5.061 Gbit/s MANA TX while useful image goodput still failed.
- A hypothetical equal 586/1500 GB/s local receiver asks for approximately
  **3.125 Gbit/s** useful bytes. Its required network RX is that remote portion
  plus transit, **7.626-9.183 Gbit/s** after reweighting. If RX and TX each had
  the same 2/4 Gbit/s usable envelopes, this primary-hit model's cohort-limited
  uniform fleet bound changes only **120.006 -> 127.624 GB/s** (2 Gbit/s), or
  **240.012 -> 255.247 GB/s** (4 Gbit/s): about **6.35%**, not 4x.
  At 12.5 Gbit/s it gives 750.039 -> 797.648 GB/s, but ignores all other limits.

Calling transit "unavoidable" means the share-only change does not remove its
forwarding obligation, not that every modeled byte must cross that node in a
real run. Link failures and alternate routes can shift transit, at the cost of
retries/latency; these are not a deterministic capacity reservation.

Those uniform-demand bounds are deliberately conditional and are **not forecasts
of the actual fleet aggregate**. Healthy nodes can pull much faster while the
eleven remain positive but slow; stage40's recovered pod C6 achieved 1500 positive
nodes and 568.413 GB/s without uniform per-node throughput. The stage40 underlay
C2 result was 459.876 GB/s with 1499 positive, not an accepted baseline.

For any node the capacity accounting to preserve is:

```
TX >= owner-serving + transit (+ retries, origin, control overhead)
RX >= remote-local-receive + transit (+ retries, fills, control overhead)
usable remote-local-receive <= max(0, RX_capacity - transit - other_RX)
```

Shares free owner TX and owner-related buffer contention, but do not directly
reserve receiver bandwidth or remove transit RX/TX. If its independent RX really
were 2 or 4 Gbit/s, a receiver with zero transit could at most obtain 0.25 or
0.5 GB/s of remote payload before overhead. TX-only evidence does not establish
that RX ceiling. With current modeled transit at the fleet target there is no
such spare capacity. Lowering local pull concurrency can reduce pressure, but
does not lower relay demand imposed by other nodes. No NIC reset, pacing,
reboot, or another failed canary is warranted by this calculation.

This owner-share setting is not multi-NIC striping: it leaves existing endpoints,
rail configuration, and transport selection untouched. It cannot turn all NICs
into usable aggregate bandwidth by itself.

## Ready configuration and guarded execution procedure

Desired state, **not applied**:

```yaml
# On ONLY the eleven Nodes in the table (full names/UIDs in the forecast):
metadata:
  annotations:
    racer.unbounded-cloud.io/shares: "1"
# Other 1489 nodes retain their current effective shares=4.
# Shared racer-dataplane-config RACER_SHARES remains "4".
```

Run the preflight from the repository root, with no changes to workloads:

```sh
timeout --signal=TERM --kill-after=10s 300s \
  python3 hack/scripts/racer-capacity-preflight.py \
  designs/racer-stage41-forecast.json tmp/racer-stage41-preflight.json
```

The helper never applies patches. It validates each exact Node UID, absence of
an administrator override/exclusion, enrolled and last-admitted shares4, then
server-dry-runs a JSON Patch containing UID and resourceVersion tests plus the
single annotation. Refresh immediately before any later application. Do not
reuse stale resourceVersions or remove the concurrency guards on conflict. Apply
only the freshly reviewed `patch` for its recorded `node` with normal authenticated
`kubectl patch node --type=json`; no operator pause, DS/config mutation, pod roll,
token/certificate replacement, cache wipe, or admission bypass is needed.

Reconciliation is asynchronous and eleven writes are not atomic. Keep pulls on
all1500; allow normal publication convergence and bounded old memberships to
drain. Verify exact member UID set/endpoints unchanged, exactly eleven shares1,
1489 shares4, new membership hash/version, and no annotation diagnostics.
Check workload images/specs/readiness and all1500 positive verified rates; do not
silently drop the slow cohort from success metrics. Preserve `00007r`'s process
quarantine: this operation does not need to replace that process.

Rollback is the inverse annotation operation, **not exclusion**: for each applied
target refetch Node, test the recorded UID, fresh resourceVersion, and current
shares annotation `"1"`, then remove only
`/metadata/annotations/racer.unbounded-cloud.io~1shares`. All eleven originally
had no explicit override and enrolled4; removal therefore restores effective4.
If a concurrent administrator changed that field, stop that patch and report
the conflict rather than overwriting it. Preserve every other annotation/label.

Any later evaluation should remain on pod-network5e1/C6, with unchanged catalog,
budgets, images and all1500 clients. Compare complete-image success and verified
goodput per node, owner/relay byte telemetry if available, and receiver progress;
do not promote to underlay merely because shares were accepted.

## Minimal future contract if transit relief is required

No narrow operator/controller-only shares patch can make this graph
capacity-weighted. The existing alternative is the administrator owner-share
annotation above. Explicit separate relay capacity would require dataplane route
selection/admission semantics and compatibility work, so it was not implemented.
A future contract should separate owner weight from transit capacity; use only
controller-authorized UID-bound attributes, default existing behavior, retain
all-node receiver membership/local sockets, preserve signed hop/deadline budgets,
and define behavior for mixed versions and capacity changes. Do not overload
owner shares as "exclude from graph", invent a zero-share escape hatch, or add
untrusted local metrics to placement authority.

## Validation and artifacts

- Four focused Go tests passed at the exact deployed controller source: authenticated
  proposal/explicit precedence, annotation defaults, annotation rejection cases,
  and CAS conflict preserving history then retrying fresh shares.
- Offline placement/hash vectors and catalog conservation assertions passed.
  The second offline run corrected cutoff-tie handling and explicit graph
  self-edge removal; cohort results remained unchanged. No benchmark was rerun.
- All eleven server-side Node-patch dry-runs passed. The preflight's initial
  Python syntax error was fixed before any Kubernetes operation by that helper.
- Focused preflight tests cover dry-run-only operation, identity/policy drift,
  and admission rejection without emitting an apply plan.
- Required `make fmt`, scoped to the inspected controller package, ran gofumpt
  but its installed golangci-lint panicked: it was built with Go1.26 and a file
  requires Go1.27. No tracked Go file changed. No toolchain replacement or broad
  suite was attempted for this documentation/offline-tool-only change.
- `racer-stage41-forecast.json`: exact before/after placement and modeled demand.
- `racer-stage41-preflight.json`: admitted, unapplied guarded patches and workload
  status. ResourceVersions are historical evidence, not reusable execution locks.
- No application source change, release build, deployment, NIC canary, or new
  parent protocol. Parent concurrent request-path documents are untouched.
