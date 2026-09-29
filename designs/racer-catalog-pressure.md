# Catalog pressure: acquisition cancellation must not poison peer ingress

## Scope and conclusion

Read-only Stage 16 artifact analysis against `ae603961e2f2f329ee164b9a0f112bb70304ce66`.
The measured image was `aba97ad0`; its only successor through ae603 is buffer
checkout reuse in `runtime/admission.rs` and `peer/transfer.rs`. The main checkout
had independently advanced to `bf78b3b4`; it was not used to infer Stage 16 behavior.
No cluster requests, configuration changes, image deployments, or load runs were made.
The separate SIGSEGV investigation is out of scope.

**Proven source bug:** abandoning a page acquisition or metadata refresh cancels
its caller's cancellation domain. Peer callers inherit a worker-lifetime domain,
so one abandoned operation can permanently disable that worker's peer sessions.
Two focused regressions failed on ae603 with the parent becoming `Cancelled`.
This explains a persistent progress failure mechanism, not just reduced cache hit
rate. Historical artifacts support this mechanism but do not expose cancellation
state, so they cannot prove that it caused every failed pull or every TX collapse.

## Quantitative evidence

Sources are preserved under repository `tmp/`, not copied or overwritten here.
`racer-stage16-load{ctx128,cat512,recovery4}-per-node-complete.csv:1` defines the
full-node columns. Group below uses `tx_Gbps < 0.1`; means use all matching rows.

| Measure | Catalog256/6x4, ctx128 | Catalog512/6x4 | Recovery256/4x4 |
| --- | ---: | ---: | ---: |
| Verified GB/s | 534.4604 | 302.4591 | 490.2151 |
| Pull success | 98.9904% | 46.1778% | 99.4630% |
| Nodes below 0.1 Gbit/s TX | 0 | 140 | 2 |
| Mean TX of the same 140 catalog512-collapsed nodes, Gbit/s | 7.73985 | 0.01067 | 7.05600 |
| Mean dataplane CPU of those 140, cores | 3.36008 | 1.51347 | 2.92896 |

In catalog512 these 140 nodes still average 3.11919 Gbit/s RX and 0.22463
verified GB/s, with 41.9684% pull success. Thus they are not stopped processes;
local client consumption survives while external serving largely disappears.
The other 1,360 average 8.32767 Gbit/s TX and 46.7220% success. Failures are
fleet-wide, not confined to the low-TX group. Goodput's Pearson correlation with
dataplane working set is -0.173 and with TX is +0.068, not evidence of a simple
fleet memory-capacity or NIC-capacity ceiling.

The exact sampled five-minute counter differences are 169,497 successful pulls,
198,507 failed pulls, 90.660 TB verified and 139.661 TB received. The 49.001 TB
gap is 163.336 GB/s, or 35.09% of received bytes not credited to completed images
(boundary in-flight work is included; this is not an exact failed-byte counter).
Rate-derived success differs slightly from the sampled-counter ratio. Relative to
Stage15's 534.5999 GB/s baseline, useful throughput falls 43.42% while TX falls
only 3.37%. Source: `racer-stage16-loadcat512-raw.json`, queries at lines 8-20;
baseline `racer-stage15-load6-raw.json`, same keys.

The saved failure breakdown is **before**, not over, the accepted window:
00:15:45 five-minute rates show layer errors 654.358/s, layer cancellations
1,539.525/s, manifest errors 19.154/s, config errors 14.114/s. Layers are 95.16%
of non-cancellation request errors. See
`racer-stage16-failure-metrics-1790640945394972759.json:18-94`.
One layer error cancels sibling layer workers (`cmd/racer-loadgen/pull.go:196-215`),
and only whole verified images earn credit (`pull.go:173-180`). Neither metric
distinguishes every transport failure from size/hash errors; no exact corruption
attribution is possible from these labels.

At 00:20 the three 128-entry rings contain 308 plaintext admission failures and
76 other records: candidate unavailable/deadline/I/O, peer handshake/relay,
and client delivery cancellation/failure. On adsv5/0000ap the sequence ends with
candidate-budget exhaustion -> page 1 acquisition Unavailable -> FirstSlice
Unavailable. Source: `racer-stage16-remote-diagnostics-1790641200625725430.json:1488,1497,1506`.
These are overlapping stage records over roughly seconds, not independent pulls.
The healthy recovery rings have 350 admission records out of 384, so admission
ring occupancy alone is not a failure-rate measurement. Admission logs its first
failed reserve before idle reclamation can succeed (`runtime/admission.rs:412-423`,
`read/fill.rs:133-172`). Exported overloads count terminal client Overloaded,
not all admission failures (`telemetry/metrics.rs:224-245`).

For adsv5/0000ap, between the saved 00:10:31 and 00:20 snapshots, peer bootstraps
increase by **zero**, while peer hits increase 17,046, decrypt starts 18,156,
local direct-delivery bytes 237.404 GB, memory hits 265 and disk hits 55.
There are zero corrupt-miss increments and zero terminal-overload increments.
This supports loss of incoming peer work while outbound acquisition and local
delivery continue. Sources: the same records in
`racer-stage16-remote-diagnostics-1790640631098314159.json` and
`racer-stage16-remote-diagnostics-1790641200625725430.json:1505`.
It is not an exact accepted-window delta.

Catalog rollback alone left degraded service; process restart restored it
(`racer-stage16-results.md:43-49`). This is consistent with irreversible in-memory
cancellation, unlike a working set that merely exceeds cache capacity. Sequential
rollouts and the later host events limit causal attribution.

## Capacity and working-memory model

`racer-stage16-catalog-size.json:2-18` contains actual descriptor sums:
256 has 2,048 distinct layers, 9,214 layer pages and 127.934 GiB including
manifest/config. 512 has 4,096 distinct layers, 18,417 layer pages and 255.593 GiB.
Adding manifest/config gives 2,560 vs 5,120 objects, and 9,726 vs 19,441 pages.
Content generation depends on seed/index, not catalog size (`catalog.go:74-95`);
the compatibility assertions compare old manifests and actual layer bytes
(`catalog_test.go:21-42`). Saved origin verification also matched all old manifests.

The retained configuration has 4 GiB plaintext, 6 GiB ciphertext, 4,096 metadata
entries, four workers, 1 GiB slabs per worker, 64 MiB segments and two reserved
segments (`racer-stage16-before.json`, `config.data`). Node memory/metadata quotas
are divided among workers (`app.rs:229-249`), unlike slab bytes (`app.rs:604-609`).
Each worker therefore has 1 GiB plaintext (64 full pages), 1.5 GiB ciphertext
(95 full page-plus-tag allocations before other ciphertext uses), and 1,024
metadata/cache entries. Rings confirm 1,073,741,824-byte plaintext limits and
16,777,216-byte requests. Idle page bundles share admission with active work;
busy plaintext OR ciphertext protects the bundle (`memory/cache.rs:273-318,396-399`).
The two recycled allocations per worker retain charges and are reclaimed on
pressure (`runtime/admission.rs:83-125,412-453`), not an unbounded pool leak.

The metadata catalog crosses a concrete threshold: 2,560 objects fit nominally
inside 4,096, but 5,120 cannot all fit. FIFO descriptor eviction also removes
fresh pointers; fresh misses refresh, while pinned misses can recover attached
descriptors (`store/index.rs:251-323`, `read/metadata.rs:463-480,833-876`). This
predicts metadata churn, not necessarily origin-body churn or correctness failure.
The exact per-worker occupancy/eviction counts were not saved.

Disk geometry yields 14 usable segments x 3 full records x 4 workers = 168
full-page payloads = 2,818,572,288 bytes/node, matching the saved gauge. This is
configured capacity, **not occupied bytes** (`app.rs:918-940`,
`store/checkpoint_format.rs:53-69`). The 65,536 page-index ceiling is not the
physical byte capacity. Mean primary layer ownership grows 6.14 -> 12.28 pages
per node. Even a hypothetical full three-candidate copy set averages only
523.45 MiB/node at catalog512, below disk capacity, but rankings do not guarantee
three actual copies (`topology/placement.rs:113-118`, `read/candidates.rs:190-203`).
No claim of complete residency follows from the fleet's 0.725 origin fills/s.

Client page memory depends on concurrent unique pages, not total catalog bytes.
There are at most 24 active layer fetches per node at 6x4, plus bootstrap/config
work; dataplane range prefetch defaults to two pages (`config.rs:233`,
`read/range_stream.rs:279-342`). Peer serving, coalescing, ciphertext references,
worker skew and any SDK continuation window change the live set. The saved
snapshots lack live-vs-idle byte and per-worker page counts, so a precise peak
working-set estimate or a justified blanket memory increase is unavailable.
Host available memory stays >=16.293 GB, and mean dataplane working set rises
only 11.074 -> 11.450 GB (`racer-stage16-results.md:63-67`).

## Proven causal path and focused correction

The following line references describe ae603 before this fix:

1. `app.rs:1044,1350-1361`: existing and newly accepted peer connections clone the
   same worker `task_scope`. Its listener/readiness scopes are separate.
2. `peer/server.rs:280-288` and `read/serve.rs:359-360,417-440`: signed request
   deadlines narrow clones without isolating cancellation; local peer acquisition
   reaches Fill/MetadataService with that shared token.
3. `read/fill.rs:387,454-460` and `read/metadata.rs:512,524-529`: retained drivers
   clone that token and cancel it on abandonment. Parent `check()` then fails
   permanently (`runtime/deadline.rs:104-105,125-150`). Future handshakes fail
   before I/O (`security/connection.rs:264-275`).
4. Per-connection errors are consumed without killing the worker
   (`app.rs:1366-1373`), explaining why readiness and outbound/local work can
   survive. Restart creates new scopes. A capacity increase cannot un-cancel one.

Fix both driver sites to own independent cancellation scopes with the same
request ID/deadline. An operation-owned parent subscription forwards cancellation
one way, and is released with the driver. Accepted I/O/crypto still drains before
resource release. No error hiding, retries, deadline extensions, or quota changes.
The first local test run exposed cancellation-wake coalescing in an existing
test; it now consumes the parent wake before checking the independent origin
completion wake. Repeated cancel/register wakeups are avoided while draining.

Validation: both new parent-isolation tests fail on ae603, then pass with the
fix; all 32 fill integration tests and 11 metadata tests pass, including existing
crypto-fence, coalesced-reader, pressure-release and non-spinning assertions.
Rust formatting passes. `make fmt` was attempted; the installed Go linter panics
because it was built with Go 1.26 while packages require 1.27. No Go files changed.

## Acceptance, not a deployment claim

Keep current healthy256/4x4 unchanged. Before any catalog performance comparison,
the fixed build must survive abandonment/expiry under the same worker peer scope,
serve the next request without restart, and release driver/flight/context charges
only after real completions. A future matched catalog512 validation should require
>=99% completed verified pulls, all 1,500 nodes with positive verified bytes, no
new persistent near-zero-TX serving cohort, no corruption/reset/process failures,
and no post-pressure recovery requiring restart. Full-window stage counters and
live/idle per-worker occupancy are needed to apportion residual failures. Improved
owner fairness or restored 534 GB/s is not established by these local tests.
