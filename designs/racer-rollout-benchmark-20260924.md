# Racer rollout and Gantry benchmark: September 24, 2026

## Final verdict

The matched **4206056a** deployment completed the 1,500-client image comparison.
With four concurrent images and three concurrent layers per image, mean
**client-digest-verified layer goodput was 12.309973 Tb/s, or 1,433.069 images/s**.
The one-image/one-layer profile achieved **3.606380 Tb/s, or 419.838 images/s**,
versus the earlier same-concurrency image baseline of **1.120757 Tb/s**:
**3.217808x** the baseline mean.

This is a completed workload measurement with remaining defects, not a
zero-fallback Racer-only pass. The final 4/3 window averaged **1.184111 Gantry
fallbacks/s, 0.410127 aborted streams/s, and 0.102598 failed images/s**. Most
delivery was warm local cache traffic. Positive software-TLS file-byte counters
prove peer payload activity despite a confirmed TLS response-byte accounting bug.

**The historical raw-object regression is unresolved by this campaign.** Its
last saved goodput was **331.517364 versus 911.710996 GB/s**, 63.637889% lower.
Raw load was not rerun on 4206056a; the image workload cannot establish recovery.

## Evidence and deployed versions

Artifact paths below are relative to
[`tmp/racer-rollout-20260923/`](../tmp/racer-rollout-20260923/). Source citations
refer to commit **`4206056a4ec45a6d7d01eab7ffab245544f3a42b`** unless another
commit is named. Local artifacts are gitignored and must be retained separately.

| Evidence | Purpose |
| --- | --- |
| [FRESH-FAST-COMPLETE.md](../tmp/racer-rollout-20260923/FRESH-FAST-COMPLETE.md) | Cutover completion, cleanup, startup limitations |
| [FINAL4206-RESULTS.md](../tmp/racer-rollout-20260923/FINAL4206-RESULTS.md) | Detailed image execution record and remaining defects |
| [final4206-comparison.json](../tmp/racer-rollout-20260923/final4206-comparison.json) | Baseline, 1/1, and 4/3 arithmetic summaries |
| `final4206-{one-one,four-three}-window.json` and `-summary.json` | Saved queries, exact samples, coverage and diagnostics |
| [peer-fleet-4206056a-summary.json](../tmp/racer-rollout-20260923/peer-fleet-4206056a-summary.json), adjacent `-report.json` | All-node adjacency and process-owned socket proof |
| [peer-payload-4206056a-summary.json](../tmp/racer-rollout-20260923/peer-payload-4206056a-summary.json) | Later payload-era sample and restarted-node revalidation |

All five final images use the full SHA above. Successful build receipts are in
`fresh-fast-4206056a4ec45a6d7d01eab7ffab245544f3a42b.json:2-33`; the verifier
requires matching SHA, completed/success status and a successful build job
(`freshv1.py:519-531`).

| Component | Successful workflow | Earlier image-baseline SHA |
| --- | --- | --- |
| Control plane | [36018455915](https://github.com/Azure/unbounded/actions/runs/36018455915) | `bbefd642` |
| Dataplane | [36018460074](https://github.com/Azure/unbounded/actions/runs/36018460074) | `abea6cfe` |
| Loadgen and registry | [36018464199](https://github.com/Azure/unbounded/actions/runs/36018464199) | `b73713cd` |
| Operator | [36018468201](https://github.com/Azure/unbounded/actions/runs/36018468201) | `b73713cd` |
| Gantry | [36018472871](https://github.com/Azure/unbounded/actions/runs/36018472871) | `60f15272` |

Earlier tags come from the same artifact's `before` workload specs. Final image
tags/counts are in `fresh-fast-final-verification.json:2-34`; per-Pod imageIDs,
resources and restart states are retained in `final4206-pod-inventory-*.json`.
The digest table is also recorded in `FINAL4206-RESULTS.md:21-29`.

### Authorized cutover and final operating state

The parent followed the user's later **fast, all-at-once cleanup authorization**.
Old writers were stopped; 1,500 identity-checked cleanup receipts covered only
`cache.slab` and `cache-v5.slab`, retaining lock sidecars and unrelated data.
Trust/runtime objects were reset with UID/resourceVersion guards; version-1
trust and `/cache/cache-v1.slab` replaced the earlier state. The cleanup check
requires all 1,500 successful receipts (`fresh-fast.py:94-141`;
`FRESH-FAST-COMPLETE.md:15-38`).

By **15:36:48 UTC**, operator 1, CP 2, DP 1,500, Gantry 1,500 and registry 1 were
ready; image clients were deliberately held. All 1,500 Nodes reported fresh,
applied 50-GiB storage. Simultaneous enrollment accumulated **1,127 DP startup
restarts**; a sampled prior process repeatedly reported enrollment unavailable
and exited after the 90-second startup deadline. Convergence does not erase
that cold-start failure (`fresh-fast-storage-verification.json:2-7`).

Full client admission followed at 15:50:11, all ready by 15:50:45; the 4/3
profile was selected at 16:01:00 and ready by 16:02:19
(`FINAL4206-RESULTS.md:12-19`). Final recorded state is **CP 2; DP, Gantry and
loadgen 1,500 each**. The final client spec retains 4/3, matched image, Site/Linux
selectors, maxUnavailable 25 and maxSurge 0; all 1,500 are ready/available/updated
(`final4206-final-client-state-20260924T161356Z-e798e2.json:38-61,119-121,136-151`).
One additional DP exit brought the count to 1,128 in the inspected 16:04 snapshot;
later peer-proof restart observations are described separately below.

## Workload, metric contract, and windows

- **Image dataset:** 32 images, four unique 256-MiB layers each, 32 GiB total
  unique layer data. Each successful image represents **1,073,741,824 layer
  bytes**, plus separately consumed manifest/config bytes. Zipf exponent 1;
  five-minute image deadline; host-network clients use local Gantry.
- **Resources:** client limit 2 CPU/1 GiB, request 1 CPU/256 MiB, GOMAXPROCS=2;
  registry limit 4 GiB; managed DP eight buffers, 2-GiB memlock, 90-second startup.
  Specs: `final4206-pod-inventory-20260924T160044Z-b514e5.json:12-40,94-104`;
  `FRESH-FAST-COMPLETE.md:35-37`.
- **Primary goodput:** successful images/s x `2^30` bytes x 8. Decimal Tb/s and
  GB/s are used for rates; GiB denotes binary object sizes. Raw consumed bytes
  include failed attempts and metadata and are not successful layer goodput.
- **Verification:** loadgen checks exact size, EOF and SHA-256 for each object;
  success requires all layer tasks to finish (`cmd/racer-loadgen/image_load.go:152-245`).
  Tests reject corrupt/truncated/oversized bodies and count failed bytes separately
  (`cmd/racer-loadgen/image_test.go:222-285`). This measures downloads and validation,
  not container unpack/start or containerd commit throughput.

Every image range below contains **six one-minute evaluations of five-minute
rates**. Means are arithmetic means of those points; latency p95 is the mean of
histogram-quantile estimates, not a pooled-window p95.

| Image observation, September 24 UTC | Evaluations | Earliest lookback | Unix bounds |
| --- | --- | --- | --- |
| Earlier baseline, 1/1 | 14:57:07-15:02:07 | 14:52:07 | 1790261827-1790262127 |
| Final 4206056a, 1/1 | 15:55:44-16:00:44 | 15:50:44 | 1790265344-1790265644 |
| Final 4206056a, 4/3 | 16:07:36-16:12:36 | 16:02:36 | 1790266056-1790266356 |

Sources: `final4206-comparison.json:2-4,202-204,418-420`. The 4/3 lookback starts
after readiness. The 1/1 earliest lookback is one second before the recorded
full-client readiness time and overlaps a DP exit at 15:51:50; its rates continued
warming strongly. Earlier prose describes that range as washed out
(`FINAL4206-RESULTS.md:17-19,52-54`); the exact timestamps retain these qualifications.

## Image results

| Mean across saved evaluations | Earlier baseline 1/1 | Final 1/1 | Final 4/3 |
| --- | ---: | ---: | ---: |
| Verified layer goodput, Tb/s | 1.120757 | 3.606380 | **12.309973** |
| Verified images/s | 130.473260 | 419.837915 | **1,433.069446** |
| Client errors/s | 0.010498 | 0.001211 | 0.102598 |
| Successful-image mean latency, s | Not captured | 4.030070 | 4.188219 |
| Mean successful p95 estimate, s | 35.112340 | 21.332530 | 9.640129 |
| Gantry aborted streams/s | 0.013195 | 0 | 0.410127 |
| Gantry fallback/s, five-minute increase / 300 | 0.022913 | 0.021730 | **1.184111** |

Sources: `final4206-comparison.json:6-78,206-280,422-495`.
Final 4/3 successful goodput is **1,538.746601 GB/s**; raw consumed bytes average
**1,538.857339 GB/s** (`final4206-four-three-summary.json:5-20,13534-13542`).
Its last point is 12.471332 Tb/s / 1,451.854169 images/s.

```text
Final 1/1 / baseline = 3606.380230065592 / 1120.7567702509111
                    = 3.2178081148313815
Final 4/3 / baseline = 12309.972811084437 / 1120.7567702509111
                    = 10.983625651735768
Final 4/3 failed-attempt fraction
  = 100 * 0.10259761111111111 / (1433.0694464832122 + 0.10259761111111111)
  = 0.007158778426769166%
```

The **3.22x** result compares the same concurrency, but still includes changed
versions, cache history and removal of Gantry's verification tee. The **10.98x**
4/3 comparison additionally changes concurrency; neither is an isolated
steady-state transport speedup.

### Integrity, fallback, availability, and CPU qualifications

Final Gantry uses `obj.Stream` without inline SHA-256 verification
(`internal/gantry/mirror/racer.go:117-133`). Its `outcome="completed"` means full
response forwarding, **not digest verification or a containerd commit**. Tee
counters are expected to remain zero (`cmd/gantry/agent_racer.go:212-237`;
assertions in `cmd/gantry/agent_racer_metrics_test.go:45-94`). Final integrity
evidence comes from loadgen's digest checks. Earlier `verified` stream labels
must not be conflated with the final `completed` label.

In the 4/3 window, Gantry completed 8,598.088 streams/s and spliced
1,538.642 GB/s, but fallback lifetime increased **631 to 977**. Origin mirror
payload averaged **278.937 MB/s**, **0.01812548%** of all mirror bytes. Registry
full-GET bytes averaged 292.449 MB/s; range-byte and backend-page-fetch rates
were zero. These values establish a predominantly Racer-served workload with
real fallback, not a pure-Racer result (`final4206-four-three-summary.json:13623-13779`;
`final4206-comparison.json:537-586`). Exact fallback triggers remain unproven.

All 1,500 client nodes had successes, healthy unique scrapes and CPU coverage;
client/Gantry counters had zero resets. One DP scrape fell to 1,499 at one
evaluation; Gantry availability also briefly read 1,499, then recovered
(`final4206-four-three-summary.json:13525-13533,13670-13676,41877-41949`).
Mean fleet CPU: loadgen **1,672.262 cores**, Gantry **529.433**, DP **1,181.385**,
registry **0.317**. Loadgen averaged 1.115 cores/node with 9.36% aggregate
throttled-period fraction. Last per-node goodput ranged 1.933-12.670 Gb/s, median
9.377 Gb/s (`final4206-comparison.json:503-535,624-628`;
`FINAL4206-RESULTS.md:130-135`). This is not uniform per-node bandwidth.

## Peer topology and payload evidence

### Exact all-node graph proof

The independent actual-catalog oracle matched **all 1,500 Nodes** at revision
**366**, captured **15:38:53.375807-15:44:42.334851 UTC** across 15 batches.
The unchanged catalog retained **K25 x HS50**: 1,250 base roles, base degree 31,
bundles of one or two physical members. Actual physical degrees were exactly
**1,000 Nodes x 36 and 500 Nodes x 42**, with 57,000 directed edges per volume
and 114,000 across two volumes (`peer-fleet-4206056a-summary.json:2-50`).

A fresh 1,500-member choice would be K30 x HS50 with degree 36; substituting that
geometry would incorrectly reject the retained graph. Retention and balanced
role assignment are implemented in `cmd/racer-controlplane/src/product_topology.rs:14-73`;
tests require retained-profile vacancy repair and resizing outside valid bounds
(`tests/placement/product.rs:269-302`, relative to the CP crate).

All process-owned established sockets observed in that census lay inside the
configured graph. Maximum per **directional peer** was two incoming and two
outgoing, not two per node. This was metadata traffic before payload ramp;
page counters were zero. It does not prove payload-saturation socket limits
(`peer-fleet-4206056a-summary.json:51-97,154-174`;
`peer-fleet-4206056a-report.json:576`). Counts were not simultaneous, and socket
state alone does not prove TLS completion.

Later, **24 Nodes** sampled under the payload workload at **16:15:54-16:16:00**
had maxima **nine outgoing / four incoming sockets per peer**, with no
outside-graph edges. **Seven restarted Nodes** were recaptured at
**16:16:26-16:16:32** and matched the same oracle/revision. This complements the
earlier all-node proof; it is not a second full-fleet census or proof of a
universal nine-socket ceiling (`peer-payload-4206056a-summary.json:2-882,889-920`).

### Confirmed TLS byte-accounting gap: zero is not no peer payload

At 4206056a, `racer_dataplane_response_bytes_total{source="peer"}` omits TLS body
accounting: the TLS branch advances the body and continues without the traffic
measurement used by the plaintext path
(`cmd/racer-dataplane/src/http_server.rs:1408-1496`). Therefore the summaries'
zero `peer_payload_bytes` values **do not establish zero peer bytes**.

Independent software-TLS file accounting increments only after a successful
write (`src/http_server.rs:1451-1455`; `src/tls.rs:955-959`, relative to the DP crate).
The HTTP/TLS file-transfer test checks the exact received body and an exact
**192 KiB + 7** fallback-byte delta when kTLS is absent
(`cmd/racer-dataplane/tests/http/tls.rs:660-678`). This is file payload evidence,
not merely handshake or page-attempt evidence, although it is not unique verified
object goodput and does not cover every in-memory TLS payload path.

The parent reports **102.142 GB/s** software-TLS file payload during the 15:54
ramp; that point's exact query receipt is not identified in the indexed artifacts
here. Saved independent evidence at 15:57:52 records **39,024,552,197,272 cumulative
software-TLS file bytes**, with zero kTLS sendfile bytes
(`final4206-path-diagnostic-20260924T155752Z-a2cda5.json:73-109`). The final 4/3
window directly records **2.614300094 GB/s** software-TLS file bytes and **42.9574
HTTP peer-page fetches/s**, with zero RDMA page rate and zero kTLS sendfile rate
(`final4206-comparison.json:587-615`). Peer payload transfer is established.

The lower warm-window peer rate is compatible with mostly local cache delivery:
page disk-hit rate 25,788.5544/s and miss rate 42.9824/s imply
**99.8336049% page hits**, by ratio of mean rates
(`final4206-four-three-summary.json:14022-14050`). It does not turn the
12.31-Tb/s aggregate into peer-network throughput. Gantry splice likewise does
not establish kernel TLS offload.

`FINAL4206-RESULTS.md:137-140` calls the peer-byte discrepancy unresolved. The
implementation and exact-byte test above now establish the accounting omission
and the independent positive byte evidence; the metric itself remains unfixed.

## Historical raw-object comparison: retain the regression

These are **1-GB decimal objects**, 512-GB logical dataset, four object workers
and eight page workers, rather than the image workload. Goodput counts successful
full objects; the received-byte counter also includes partial failed downloads
(`cmd/racer-loadgen/load.go:50-89`; `load_test.go:223-268`).

| Saved raw capture | Evaluation window UTC | Points | Mean successful GB/s | Mean raw GB/s |
| --- | --- | ---: | ---: | ---: |
| `baseline.json` | Sep 23 21:39:36-22:39:36 | 61 | 911.710996 | 1,026.635610 |
| `stateless-converged-raw-20260924T023109Z-3f1c0d.json` | Sep 24 02:16:08-02:31:08 | 16 | 427.029427 | 563.539892 |
| `post-admission-check-20260924T034338Z-30091c.json` | Sep 24 03:38:36-03:43:36 | 6 | 331.517364 | 470.061771 |

All use one-minute evaluations/five-minute lookbacks. The pre-admission CP was
eb97d252 and DP b73713cd; the last check used CP bbefd642 and DP abea6cfe,
converged at 03:32:33 (`stateless-timeline.jsonl:19`). Its earliest lookback is
03:33:36. The first five pre-admission evaluations overlap time before the
02:15:45 cache-convergence transition. Unequal windows, versions and cache history
prevent attributing these differences to a single fix.

```text
Pre-admission drop = 100 * (1 - 427.02942725694817 / 911.7109958166313)
                   = 53.161755291274915%
Last raw drop      = 100 * (1 - 331.5173635793548 / 911.7109958166313)
                   = 63.63788907882915%
```

Baseline/post-admission failed attempts averaged 732.425409/548.977783 per second;
successful mean latency 3.003904/4.823596 seconds and p95 estimates
8.975681/22.095578 seconds. Both baseline and adjacent pre-admission captures
showed zero kTLS sendfile throughput; the last raw-check artifact has no kTLS query.
Sources: `baseline.json:266-1558,10654-10913`;
`post-admission-check-20260924T034338Z-30091c.json:7-246`;
`cutover-monitor-20260924T023119Z-a80f16.json:9566-9793`.

Immutable baseline SHA-256:
`94099b66a340e523b5bfcf791dfe2ff44689c04a1412c29dcca5b018aed08c51`.
The latest deployment uses a different workload and eight-buffer profile.
**No latest-version raw-object result exists here, so no raw-regression resolution
is claimed.**

## Final limitations and retained history

- The final image comparison is complete, with nonzero fallback, aborts and
  errors, a brief availability/scrape gap, warming differences, and startup
  enrollment failures. A matched raw retest and TLS response-byte metric fix
  remain follow-up work.
- Earlier Gantry readiness concentration, registry OOM/4-GiB mitigation, failed
  cold attempts and one-client diagnostics are historical, not final metrics.
  `warm-layers-20260924T0450Z/RESULT.json:1-46` proves sequential verification of
  128 layers/32 GiB with zero run-local fallback/abort deltas; it is not fleet
  throughput and predates the fresh-cache cutover.
- `FRESH-FAST-COMPLETE.md` records clients held at cutover completion;
  `FINAL4206-RESULTS.md` and the final client spec record their later release and
  completed measurements. Earlier pending/deployment notes do not supersede these.
- Final live verification again found operator 1/1, CP 2/2, registry 1/1, and
  DP, Gantry, and image clients each 1,500/1,500 ready. Image clients remain at
  four concurrent images and three concurrent layers per image.
