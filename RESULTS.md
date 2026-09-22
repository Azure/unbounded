# Racer deployment results

## Baseline

Cluster: `joolshev-scale-test`, 1,500 nodes. Branch: `feat/racer-operator-integration`.
Initial deployed commit/image tag: `d53886cd99ef1af6949275f537c9077f72ec89bd`.
Prometheus evaluation: **2026-09-22 00:02:23.589 UTC**, using the preceding five minutes.

### Deployment verification

- Operator-managed Racer dataplane: **1,500/1,500 Ready**, every node covered, zero container restarts.
- Independently deployed racer-loadgen DaemonSet: **1,500/1,500 Ready**, every node covered, zero container restarts.
- All **3,000** Racer/loadgen Prometheus targets had five successful samples in the measurement window.
- All 1,500 loadgen nodes completed successful downloads.
- All dataplanes reported configuration epoch 5, with no changes during the window.
- Storage quarantines: zero. Peer circuits: 35,976 closed, none open or half-open.
- Existing net and machina workloads remained Ready on their pinned v0.8.0 images.

### Observed measurements

| Metric | Baseline |
| --- | ---: |
| Successful 1 GB object downloads | 190.43/s |
| Failed object attempts | 76.43/s |
| Failed object-attempt fraction | **28.64%** |
| Successful-object goodput | 190.43 GB/s (1.523 Tb/s) |
| Received payload, including partial failed attempts | 214.62 GB/s (1.717 Tb/s) |
| Successful-object mean latency | 6.09 s |
| Successful-object p50, histogram estimate | 5.39 s |
| Successful-object p95, histogram estimate | 24.52 s |
| Client HTTP requests | 51,863/s |
| Peer HTTP requests | 55,077/s |
| Client payload response rate | 214.68 GB/s |
| Peer payload response rate | 229.34 GB/s |
| Upstream origin page requests | 39.91/s |
| Upstream peer page requests | 54,935.23/s |
| Local page disk-hit lookups | 51,381.13/s |
| Page miss lookups | 54,974.15/s |
| Coalesced page lookups | 107.95/s |
| Prometheus RSS | 3,927,814,144 bytes |

GB and Tb are decimal. Transport was HTTP only, with no RDMA traffic.
These measurements describe the configured workload, not maximum capacity.
Client and peer traffic represent different legs and must not be added as goodput.
Page misses can be served by peers rather than the origin.

**The baseline is not an error-free result.** Sampled failures were HTTP 503 page responses,
persisting after configuration convergence. The 28.64% figure measures failed full-object
attempts, not failed page requests. The cause was not established by the baseline measurement.

### Workload

Each dataplane uses the operator low resource profile: three CPUs, 2 GiB memory,
a 10 GiB host-backed slab, and eight transient buffers. Each loadgen pod continuously
downloads one full object at a time, with four parallel 4 MiB page reads, a two-minute
attempt timeout, decimal 1 GB objects, a shared deterministic 512 GB dataset, and Zipf
exponent 1. No automatic page retries hide errors. Loadgen readiness is independent
of download success.

Endpoint: `http://racer-loadgen-volume.unbounded-system.svc`.
Prometheus uses existing annotation-based discovery at a 60-second scrape interval.
Racer metrics are on port 9090 and loadgen metrics on port 8080.
Racer control plane has two running replicas; only its elected leader is Ready by design.

Received bytes include partial failed downloads (`cmd/racer-loadgen/load.go:52`).
Successful goodput is successful downloads per second multiplied by 1,000,000,000 bytes.
Latency queries filter `result="success"`; quantiles interpolate coarse histogram buckets.

## Initial deployment action audit

1. Inspected git status/history/remotes, build workflow, deployment implementation, tests, and documentation.
2. Inspected the current Kubernetes context, all nodes, pools, labels, capacity, workloads, Site, overrides, and monitoring.
3. Queried baseline Prometheus targets and memory; verified existing pod discovery could scrape Racer and loadgen.
4. Pushed the existing branch normally to the same-named GitHub branch, without force, new commits, tags, releases, or PRs.
5. Dispatched and verified four linux/amd64 image builds:
   - [racer-controlplane](https://github.com/Azure/unbounded/actions/runs/35668606496)
   - [racer-dataplane](https://github.com/Azure/unbounded/actions/runs/35668608653)
   - [racer-loadgen](https://github.com/Azure/unbounded/actions/runs/35668610603)
   - [unbounded-operator](https://github.com/Azure/unbounded/actions/runs/35668612920)
6. Saved pre-deployment operator, Site, overrides, and Prometheus configuration snapshots, excluding Secret resources.
7. Added an override key pinning existing machina/net images to v0.8.0, preserving existing overrides.
8. Labeled one node per pool for a four-node canary.
9. Upgraded the operator to the built SHA image and verified rollout; its startup reconciled CRDs.
10. Added supported Racer overrides for control-plane placement/resources, dataplane rollout limits, scrape annotations, and canary selection.
11. Enabled Racer on Site `cluster`; the operator created its workloads, identities, and configuration state.
12. Applied the independent loadgen namespace/DaemonSet, local origin Service, and Racer volume Service with 1,500 slots.
13. Verified canary preflight, readiness, activation, successful downloads, and metric scraping.
14. Expanded loadgen origins to all nodes first, then expanded Racer.
15. Investigated slow control-plane convergence using bounded logs, status, metrics, and read-only rollout state inspection.
16. Applied control-plane overrides for TokenReview QPS 500/burst 600 and the existing AKS API FQDN. Convergence completed after its rollout.
17. Verified full node coverage, readiness, zero restarts, exact image digests, 1,500 endpoints per Service, and epoch convergence.
18. Removed the four temporary canary node labels.
19. Verified existing workload readiness/version pins and operator reconciliation conditions.
20. Queried throughput, error rates, success latency, cache/upstream behavior, epochs, quarantines, breakers, scrape health, and Prometheus memory; aligned queries to one timestamp.
21. Saved sampled 503 and control-plane logs and waited for a complete post-rollout five-minute window.
22. Investigated a transient preexisting Parca scrape timeout; it recovered without intervention.
23. Saved final configurations, node/image coverage, and exact timestamped PromQL results.

All potentially blocking external operations used explicit timeouts. No Azure management
operations or destructive GitHub operations were performed. Persistent rollout state,
signing identity, and slabs were not manually deleted or modified.

Operational artifacts are retained locally under `tmp/racer-deploy-20260921/` (gitignored),
including `metrics-20260922T000229Z.json`, manifests, before/after snapshots, sampled logs,
`ACTIONS.md`, and `ops.py`. Reproduce a report with:

```sh
timeout 90s python3 tmp/racer-deploy-20260921/ops.py report
timeout 90s python3 tmp/racer-deploy-20260921/ops.py pods
```

## Follow-up: HTTP 503 correctness

The requested follow-up focuses on fixing HTTP 503 failures, not performance tuning.
Investigations and verified fixes will be recorded here as iterations complete.

### Iteration 1: pressure correctness and failure attribution

Delegated independent dataplane and SDK investigations, then an independent patch review.
Live pre-fix sampling still showed 65.91 failed object attempts/s with all 1,500 nodes
on epoch 5. Performance tuning and unrelated SDK/origin changes were deferred.

Implemented two reproduced pressure-path defects:

- Early scheduler wakeups no longer exhaust the resource-poll allowance into an
  immediate Busy/503 before the timed retry budget elapses. Excess early attempts
  park until their retry deadline; caller/candidate deadlines and timed exhaustion
  remain enforced (`cmd/racer-dataplane/src/cache.rs:1267`, `:1500`).
- Terminal admission exhaustion is shared with waiting consumers rather than
  mistaken for producer cancellation and causing takeover/refetch. Shared admission
  is recognized before peer-validation recovery, so local checksum pressure cannot
  falsely penalize a peer (`cmd/racer-dataplane/src/cache.rs:1280`, `:1657`).

Added bounded-cardinality counters for emitted HTTP error reasons, typed pressure
causes, and handler stream aborts. They distinguish Busy, owner-unavailable, and
unavailable 503s without adding retries or changing the loadgen workload.

Validation before image build:

- Both original defects reproduced in deterministic tests before their fixes.
- Cache suite: 30 passed; subprocess-only helpers also passed through wrappers.
- Generated lifecycle campaigns: six passed with real io_uring required.
- Handler suite: 12 passed; HTTP attribution suite: 29 passed; metrics suite: six passed.
- All-target Rust check, Rust formatting (including included tests), and diff checks passed.
- Required `make fmt` was attempted: installed Go 1.27 conflicted with a Go 1.26-built
  linter. A Go 1.26.6 retry exceeded its 90-second bound. No Go source changes resulted.
- Independent review found a shared-admission dispatch regression; it was fixed and
  covered by a checksum-queue exhaustion regression before committing.
- Follow-up independent review confirmed that blocker resolved.
- The previously ignored full-page latency stress now passes. Enabled it as a
  regular regression with both HTTP-only and RDMA-enabled 32-node paths; the
  unchanged oracle rejects healthy 502/503 and validates exact response bytes
  (`cmd/racer-dataplane/tests/runtime/cluster.rs:609`). Both paths passed.
- A subsequent bounded Go 1.26.6 `make fmt` completed successfully with zero issues.

Fix commit: `13ff109e6b59e73ee564946444e8ef6dc82f4f4f`.
Dataplane image build: https://github.com/Azure/unbounded/actions/runs/35729592795.

Cluster validation of this iteration is pending. These fixes are not yet claimed
to eliminate the observed live 503s.

### Iteration 2: unblock validation and localize remaining pressure

- Built the iteration-1 dataplane image successfully. An attempted `OnDelete`
  override was rejected because the override merger retained `rollingUpdate`;
  no workload changed from that rejected override. Used a rolling update with
  `maxUnavailable: 1`, then 25 after the first replacement activated.
- The rollout stalled with 25 unready replacements and 1,475 serving nodes.
  Returned the rollout limit to one while investigating. Preserved all durable
  topology, forwarding history, and storage state.
- Delegated control-plane and Rust retirement investigations. Reproduced forward
  history backpressure with missing retirement acknowledgments; confirmed that
  valid acknowledgments allow replacement without discarding history.
- Found and fixed a control-plane liveness defect: inventory Pod/Node LISTs held
  the subscription mutex, blocking heartbeats while Kubernetes reads stalled.
  Reads now occur outside that mutex; topology identity and the current rollout
  decision are revalidated before transitions (`cmd/racer-controlplane/rollout.go:212`).
- Four blocked-LIST regression cases failed before the fix and passed afterward.
  Full control-plane tests, focused race tests, scoped lint/format, and independent
  review passed. API-server/cross-language integration prerequisites were unavailable.
- Added deterministic HTTP retirement-under-load and wire acknowledgment tests.
  These passed, including all 37 control tests; no Rust retirement defect was found.
- Added eight fixed `racer_dataplane_cache_resource_exhaustions_total{site}` series
  to distinguish local terminal resource exhaustion from propagated Busy responses.
  Seven pressure tests and seven metrics tests passed, along with formatting checks.
- No workload concurrency, retry budget, buffer count, or SDK retry policy changed.

The mixed-version rollout cannot establish the effect on cluster-wide 503s yet.

Iteration-2 commit: `d79c045970db0a05e89c4c619647d49d9a94703d`.
Both image builds succeeded: dataplane run
https://github.com/Azure/unbounded/actions/runs/35733654406 and control-plane run
https://github.com/Azure/unbounded/actions/runs/35733654464. Applied both images
through operator overrides. Readiness recovered from 1,475 to 1,496 shortly after
the new control plane started; this is rollout evidence, not a final 503 result.

### Iteration 3: prevent heartbeat deadline amplification

A bounded 1,500-recipient, 228-forward-history reproduction established repeated
ledger decoding under the heartbeat mutex and processing of already-canceled
requests. Added a validated cache keyed to exact durable state, with private copies
and invalidation after uncertain writes. Canceled lock waiters now exit before
refreshing acknowledgments or generating responses.

The local HTTP reproduction improved from 1,188 successful polls and 3,312 deadlines
to 4,500 successful polls and zero deadlines. These are diagnostic reproduction
results, not cluster performance claims. Full control-plane tests, focused race
tests, cache corruption/write-failure regressions, mandatory cancellation regression,
scoped formatting/lint, and independent review passed. Durable history limits and
collection rules are unchanged. This change supports completing the rollout needed
to validate the 503 fixes.

Iteration-3 commit: `d7e0cf46674f4730a39735b044b40aa98a6af168`. Control-plane
image build https://github.com/Azure/unbounded/actions/runs/35736288263 succeeded
and was deployed through the operator override. Replacement admission resumed;
readiness reached 1,499/1,500 while the one-at-a-time rollout continued.

### Iteration 4: break reciprocal receive-buffer starvation

A deterministic HTTP-only test reproduced a circular dependency: opposite cold
fetches held all eight receive buffers on both nodes, while their owner-side
fetches needed those same pools. No payload reached the independent origins before
the existing 320 ms allowance expired. The one-direction control returned eight
successful responses; opposing requests returned seven successes and nine 503s.

Peer receives now atomically leave one buffer per remaining canonical hop for
downstream work. Owner fetches need no reserve. This preserves the managed
eight-buffer pool, existing deadlines, retry budgets, and terminal error sharing.
The standalone daemon now rejects pools smaller than four before startup because
they cannot support the maximum three-hop reserve plus a receive slot. The README
documents this explicit compatibility restriction; managed preflight stays at eight.

Strict regressions now pass for direct and opposing three-hop cold routes, including
the minimum four-buffer configuration, with exact response bytes and no retries or
resource exhaustion. Cache, runtime, attribution, handler, buffer, configuration,
preflight, and historical HTTP/RDMA pressure tests passed. Independent review found
the small-pool compatibility issue, then confirmed its resolution. Live validation
of this additional fix is still pending; ordinary bounded overload can still return
Busy and is not claimed to be eliminated.

Iteration-4 commit: `cc0a20cd2f38518dab6e3f4d07518d86cbbe4281`.
Image build: https://github.com/Azure/unbounded/actions/runs/35740253797.
Broader bounded verification completed with 435 library tests, 10 dataplane binary
tests, four preflight tests, and 81 separate doctests passing. The all-target run
reached its external timeout during runtime coverage; remaining owning suites
completed in separate bounded runs. Real io_uring/TCP wrappers passed. External
Go/Rust fixtures, hardware RDMA, Soft-RoCE, privileged NUMA, and extended campaigns
were not enabled. No assertion failures were found.

The iteration-4 image build succeeded and its operator-managed rollout began.
The control plane now advances successive batches, rather than repeatedly timing
out the same prepare phase. Intermediate metrics remain mixed-version observations.

### Iteration 5: release durable checkpoint admission charges

A strict 10 GiB-slab reproduction exposed stale physical-capacity reservations:
after payload A became durable, its charge remained while overlapping payload B
was still publishing. Payload C was rejected despite sufficient allowance for B+C,
2,238 free payload extents, and no read pins. Charges previously retired only when
the entire allocator became idle, contrary to their documented checkpoint lifetime.

Checkpoint batches now capture their covered charge and release it only after
successful final-sync completion. Later admissions and other shards' reservations
remain charged. Failed or ambiguous writes retain their reservations through
quarantine. No filesystem headroom, slab format, or retry limit changed.

The previously failing reproduction passes. Shared-shard overlap, superseded values,
empty checkpoints, exact recovered bytes, and pre/post-effect failure tests passed;
the allocator suite passed 51 tests and the cache suite passed 40. Formatting and
independent review passed. This establishes false admission rejection in the
reproduction; its contribution to the live 503 rate remains unproven.

### Post-fix fleet validation: 503s remain unresolved

Iteration-5 commit: `ba1da0ec027926c48ee7e7c72999b403cbf6b98c`.
Image build: https://github.com/Azure/unbounded/actions/runs/35742503797.
All 1,500 dataplanes completed rollout on this image, digest
`sha256:5efbf5f5a56f54943eaef7951122469fb2031858fc9856f78d7d02977209ac03`.
The control plane runs `d7e0cf46674f4730a39735b044b40aa98a6af168`.
The independently deployed loadgen and its workload settings remain as recorded
above. Newer concurrent branch commits were preserved and were not implicitly
included in these pinned images.

The five-minute Prometheus window ending **2026-09-22 15:14:58.735 UTC**
had 1,500 dataplane and 1,500 loadgen targets up, all dataplanes at epoch 62,
zero epoch changes, and zero storage quarantines:

| Measurement | Post-fix observation |
|---|---:|
| Successful object attempts | 193.39/s |
| Failed object attempts | 87.65/s |
| Object-attempt failure fraction | **31.19%** |
| Client HTTP503 Busy responses | 166.54/s |
| Peer HTTP503 Busy responses | 47.51/s |
| Client/peer owner-unavailable or unavailable 503s | 0/s |
| Local terminal receive-buffer exhaustion | 116.24/s |
| Local terminal payload-admission exhaustion | 49.55/s |
| Other instrumented resource-exhaustion sites | 0/s |

Artifact: `tmp/racer-deploy-20260921/metrics-20260922T151506Z.json`.
Object failures are not equivalent to page-response failures; several concurrent
page requests and forwarding hops can fail within one object attempt. The original
28.64% object failure fraction and this 31.19% observation do **not** demonstrate
an improvement. The reproduced correctness defects are fixed, but the live 503
goal is **not resolved**.

Three bounded hotspot inspections found approximately 97.7 GB filesystem space
available, healthy workers at terminal epoch 62, no quarantine, and ongoing payload
progress. Over approximately 67 seconds, cgroup I/O pressure `some` occupied
76.0-84.7% and `full` 21.5-33.9% of elapsed time. Device-wide average read/write
times were approximately 14-35 ms. These observations support storage contention;
they neither prove every remaining rejection is necessary nor exclude a narrower
request-level bug. One dataplane had restarted once with exit code zero during
rollout and was Ready afterward; the loadgen had zero restarts.

Two additional deterministic probes retain strict checks:

- With eight buffers, a 10 GiB slab, four full-page clients per node plus opposing
  peer work, and 14/25/35 ms storage latency, every finite-burst HTTP206 response
  contained the exact 4 MiB payload and no resource exhaustion occurred.
- With 2,236 of 2,240 extents occupied, four of eight new payloads needed safe
  checkpoint reclamation. They admitted at 290/390/600 ms respectively. The
  latter two exceed the existing roughly 320 ms pressure allowance, although
  checkpoint generations continued advancing and all values eventually became
  durable. This allocator-only probe does not extend HTTP retry budgets.

Both probes passed bounded tests and independent review. They establish eventual
reclaim progress, not elimination of the fleet's failures. A targeted review also
confirmed ordinary network receive time does not consume extra resource retries.
The next discriminating evidence would be allocator rejection subcause and
checkpoint progress at terminal exhaustion, rather than treating every Busy as
another deadlock or masking it with retries.

Additional operational actions: applied each pinned image through the existing
operator override; increased rolling replacement concurrency from 25 to 100 after
successive batches converged; verified final node/image coverage; queried aligned
Prometheus windows; sampled only three pressure hotspots and the single restarted
pod's previous logs. No durable rollout history was cleared. Source commits from
concurrent agents were left intact.

### Iteration 6: distinguish live allocator rejection causes

Added 16 fixed metric series per dataplane for payload-admission rejection
attempts (`pending_limit`, `filesystem_headroom`, `extent_unavailable`), checkpoint
preparation/completion, checkpoint phase, and allocator pressure. Rejection
counters include repeated attempts and are not terminal503 counters. Gauges
reflect the latest allocator poll; short-lived phases can be missed. Dropping a
shard removes its gauge contribution while retaining counters. Admission policy,
retry budgets, checkpoint ordering, and the concurrently added disk-eviction
metrics are unchanged.

Validation: 53 allocator tests, nine metrics tests, 40 cache tests and the two
HTTP/I/O probes passed, including real-kernel helper wrappers. Two opt-in allocator
tests were not run. Independent review found no blocker. Scoped `make fmt` with
Go 1.26.6 completed with zero issues after the local build-space failure was
resolved; Racer formatting and diff checks also passed. This is diagnostic
instrumentation, not a claim that the remaining 503s are fixed.

### Iteration 7: prepare reusable extents before admission stalls

Diagnostic image `488d09d685df990748aef3725d43edb3de478af0` was built by
https://github.com/Azure/unbounded/actions/runs/35751676728 and deployed through
the operator to all 1,500 nodes. A stable five-minute measurement at Prometheus
timestamp `1790095948.544` had all 3,000 Racer/loadgen targets up, epoch 78 on
every dataplane, no epoch changes, and no quarantines. Object failures remained
29.85%; client Busy responses were 165.65/s and peer Busy responses 49.66/s.
Payload-admission exhaustion was 54.48/s and receive-buffer exhaustion 110.44/s.
Allocator rejection attempts were exclusively `extent_unavailable` (1.70 million/s,
including retries); filesystem-headroom and pending-limit rejections were zero.
Approximately 33,189 checkpoints completed per second. Three bounded hotspot
observations likewise showed continued checkpoint completion and extent recovery.

A strict full-slab, eight-buffer HTTP reproduction returned 503 under the 35 ms
simulated storage profile while waiting for two safe checkpoint rotations.
Completing reclamation before the same finite burst yielded exact HTTP206
responses. The resulting policy starts bounded reclamation before free extents
reach zero: for the default shard, a low watermark of 32 targets 64 free-or-retiring
4 MiB extents. This trades up to 256 MiB of payload residency for admission
headroom. It preserves durable-root and reader protection, existing victim rules,
request budgets, slab formats, and bounded Busy behavior under genuine pressure.

Maintenance receives one bounded poll opportunity before freezing a checkpoint,
including overlapping admissions. Review found and corrected both a bypassed
maintenance boundary and repeated-yield starvation. Regressions failed before
their fixes and now require checkpoint progress before capacity exhaustion.
Validation passed: 58 allocator tests, five HTTP/I/O probes including strict
35 ms success, and 40 cache tests with subprocess helpers. Two opt-in allocator
tests remain excluded. Independent re-review found no remaining concrete blocker.
Live improvement from this policy is pending deployment and measurement.
