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

Cluster validation of this iteration is pending. These fixes are not yet claimed
to eliminate the observed live 503s.
