# Racer runtime performance: essential Prometheus queries

This guide covers the metrics currently emitted by Racer's Rust dataplane and
Go controller, with optional Gantry measurements for the client-facing path.
It describes existing instrumentation, not proposed metric names or alert rules.
Source references were checked against revision `e97421ef`; controller-runtime
behavior below is for the pinned **v0.25.1** dependency (`go.mod:97`).

## Scraping and query conventions

| Target | Metrics endpoint | Important setup detail |
| --- | --- | --- |
| Dataplane | HTTP `/metrics`, standalone default `127.0.0.1:9090` | `RACER_DIAGNOSTICS_LISTEN` controls the address. Controller-managed pods bind their pod IP on named port `diagnostics`, normally 9090, or 9091 if the peer port is 9090. |
| Controller | HTTP `/metrics`, default `:8080` | `RACER_METRICS_ADDRESS` controls the address; `0` disables it. The Deployment exposes named port `metrics`, but the existing Service exposes only HTTPS 8443. |
| Gantry, in Racer mode | HTTP `/metrics`, default `:9095` | Optional, separate target. These metrics are not emitted by the Rust dataplane. |

Scrape each process once, preferably by pod discovery. Include controller
followers even though they are intentionally unready for control traffic; do not
change control-Service readiness filtering to make metrics discoverable. The
metrics listeners are plain HTTP without authentication in the current serving
paths, so restrict their network exposure. Dataplane diagnostics start only after
initial membership preparation, recovery, and snapshot refresh: a startup failure
can mean no endpoint, not an exported readiness value of zero.

Sources: `cmd/racer-dataplane/src/config.rs:167`,
`internal/racer/workload.go:150`, `internal/racer/workload.go:190`,
`internal/racer/config.go:72`, `internal/racer/manager.go:142`,
`deploy/racer/controller.yaml.tmpl:37`, `deploy/racer/controller.yaml.tmpl:65`,
`cmd/racer-controller/README.md:240`, `cmd/gantry/agent_readiness.go:24`,
`internal/gantry/config/config.go:511`, `cmd/racer-dataplane/src/app.rs:537`.

For all examples:

- Replace `job="racer-dataplane"`, `job="racer-controller"`, and `job="gantry"`
  with your actual scrape jobs. These names are examples, not installed defaults.
- Scope selectors to **one installation**. Add cluster/namespace selectors and
  preserve those labels in aggregations when comparing multiple installations.
- `instance` identifies the scraped process. Dataplane metrics have no intrinsic
  labels: pod, node, namespace, and instance labels must come from scraping.
  Its exporter already aggregates workers; it cannot provide per-worker views.
- Counters use `rate(...[5m])` before aggregation to handle process resets.
  Use a window with several scrapes; lengthen it for sparse controller activity.
  Gauges are graphed directly, not with `rate`.
- Ratios with no traffic are intentionally undefined. Do not clamp denominators
  to 1 or turn missing targets into healthy zero-valued series.
- Histogram queries below use classic `_bucket` series. Preserve `le` when
  aggregating buckets. If your pipeline stores only native histograms, adapt the
  controller queries to the base histogram family, without `le`.

The dataplane exports its fixed counters and gauges even when zero. Controller
reconcile series appear when controllers start on the leader; followers can have
process metrics without reconcile series. Gantry's labeled mirror/origin series
are created lazily. Missing series therefore need interpretation, not just zero
filling. Sources: `cmd/racer-dataplane/src/telemetry/metrics.rs:195`,
`cmd/racer-dataplane/src/telemetry/metrics.rs:266`,
`cmd/gantry/agent_racer_metrics.go:25`, and controller-runtime
`pkg/internal/controller/controller.go:253`.

## 1. Establish availability before interpreting performance

**Scrape status for both Racer components:**

```promql
up{job=~"racer-(dataplane|controller)"}
```

`0` means a failed scrape; `1` means the metrics endpoint answered, not that Racer
is ready. A target removed from discovery eventually disappears entirely, so also
compare target inventory with the expected pods.

**Dataplane readiness, per process:**

```promql
racer_ready{job="racer-dataplane"}
```

Readiness includes worker/resource state, valid credentials, and usable local
admission. `racer_live` is weaker: it remains 1 while Starting, Degraded, or
Draining. An expiring identity can explain readiness loss:

```promql
racer_identity_expires_at_seconds{job="racer-dataplane"} - time()
```

This is seconds until expiry. Zero/uninitialized or stale identity values need
correlation with readiness; the update path does not explicitly clear the old
expiry when identity lookup fails. Sources:
`cmd/racer-dataplane/src/app_health.rs:13`,
`cmd/racer-dataplane/src/telemetry/health.rs:38`.

**Controller leadership:**

```promql
leader_election_master_status{job="racer-controller",name="racer-controller"}
```

Expect one leader per installation in steady state. Leadership is not control-API
readiness. Followers still serve metrics. Sources:
`internal/racer/manager.go:145`; controller-runtime
`pkg/metrics/leaderelection.go:28`, `pkg/metrics/server/server.go:182`.

## 2. Dataplane traffic, failures, and pressure

### Client exchange rate and unsuccessful exits

**Observed client exchanges per second, per dataplane:**

```promql
sum by (instance) (rate(racer_requests_total{job="racer-dataplane"}[5m]))
```

This counts UDS client exchanges after header reception, before parsing. It is
not image pulls, peer RPCs, open connections, or diagnostic requests. Errors
before the observation starts are outside this counter.

**Unsuccessful client exits per second:**

```promql
sum by (instance) (rate(racer_request_errors_total{job="racer-dataplane"}[5m]))
```

Errors include rejection, cancellation, abandonment, and failed/truncated delivery.
Even a fully delivered error response is an unsuccessful exchange. Requests count
starts while errors count exits: dividing these rates gives only an approximate
error-exit/admission ratio, not a completed-request failure probability. It can
exceed 1 after admissions drop while older work fails.

**Overload events per second:**

```promql
sum by (instance) (rate(racer_overloads_total{job="racer-dataplane"}[5m]))
```

This counts at most one reported overload per observed client exchange, not all
internal capacity pressure or every HTTP 503. Correlate with the active gauges
below and Gantry admission metrics rather than treating it as a saturation ratio.
Sources: `cmd/racer-dataplane/src/client/listener.rs:635`,
`cmd/racer-dataplane/src/client/response.rs:153`,
`cmd/racer-dataplane/src/telemetry/metrics.rs:164`.

### Page-source activity

Graph these four series together, with one legend per source:

```promql
sum by (instance) (rate(racer_memory_hits_total{job="racer-dataplane"}[5m]))
```

```promql
sum by (instance) (rate(racer_disk_hits_total{job="racer-dataplane"}[5m]))
```

```promql
sum by (instance) (rate(racer_peer_hits_total{job="racer-dataplane"}[5m]))
```

```promql
sum by (instance) (rate(racer_origin_fills_total{job="racer-dataplane"}[5m]))
```

Units are instrumented page-source events/second, **not bytes/second**. They help
compare cold and warm workloads, but are not an exhaustive partition of requests:
one request can touch many pages, coalesced results can avoid another increment,
and peer-serving work also contributes. Do not divide by request count and call
the result a cache hit ratio, or multiply by page size to estimate bandwidth.
Peer and disk hits can be ciphertext-only service; they do not universally prove
local plaintext verification, durable publication, or successful client delivery.
Sources: `cmd/racer-dataplane/src/read/fill.rs:339`,
`cmd/racer-dataplane/src/read/fill.rs:598`,
`cmd/racer-dataplane/src/read/fill.rs:762`.

### In-flight work and persistence

Graph the following individually, per `instance`:

| Query | Meaning |
| --- | --- |
| `racer_active_requests{job="racer-dataplane"}` | Observed client exchanges still processing or delivering, not open sockets. |
| `racer_active_fills{job="racer-dataplane"}` | Elected page acquisitions, including local work, retained through completion fencing; not coalesced waiters. |
| `racer_active_deliveries{job="racer-dataplane"}` | Attached delivery reader leases, not all waiting requests. |
| `racer_pending_disk_writes{job="racer-dataplane"}` | Accepted entries in the writer's pending map, including queued/in-progress persistence. |

Look for sustained activity with falling completions/source activity and rising
errors. These are activity counts, not utilization percentages: exported limits
and queue-wait distributions are missing. In particular, pending disk writes can
be zero while abandoned kernel writes are still outstanding.

**Successful disk index publications per second:**

```promql
sum by (instance) (rate(racer_disk_publications_total{job="racer-dataplane"}[5m]))
```

**Accepted dirty-copy discard events per second:**

```promql
sum by (instance) (rate(racer_dirty_discards_total{job="racer-dataplane"}[5m]))
```

Publication is not fsync durability. Discards include cleanup, retirement,
cancellation, and failures; enqueue rejection or skipped optional persistence is
not a discard. These counters do not form an exact persistence success ratio.
`racer_effective_payload_bytes` and `racer_disk_page_index_capacity` are startup
capacity, not occupancy. `racer_segment_tail_bytes` is storage geometry, not
current fragmentation or free space.

**Corrupt-copy misses per second:**

```promql
sum by (instance) (rate(racer_corrupt_misses_total{job="racer-dataplane"}[5m]))
```

This captures instrumented rejected corrupt copies, not ordinary cache misses,
all I/O errors, or missing keys. Investigate increases alongside source activity
and errors. Sources: `cmd/racer-dataplane/src/read/flight.rs:1083`,
`cmd/racer-dataplane/src/memory/delivery.rs:109`,
`cmd/racer-dataplane/src/store/writer.rs:188`,
`cmd/racer-dataplane/src/store/tests.rs:646`,
`cmd/racer-dataplane/src/app.rs:907`,
`cmd/racer-dataplane/src/read/fill.rs:897`.

## 3. Controller reconciliation and runtime cost

Racer has two controllers, `racer-topology` and `racer-keyring`. Each has one
worker and maps events to a singleton request. Metrics here come from
controller-runtime, not Racer-specific collectors.

### Reconcile rate, errors, and latency

**Completed reconciles per second, split by outcome:**

```promql
sum by (controller, result) (
  rate(controller_runtime_reconcile_total{job="racer-controller"}[5m])
)
```

Results are `success`, `error`, `requeue`, and `requeue_after`.
`requeue_after` is not failure: it includes normal keyring rotation scheduling
and handled Kubernetes conflicts. Consequently, `success / total` would
misrepresent healthy keyring operation.

**Reconcile error fraction:**

```promql
sum by (controller) (
  rate(controller_runtime_reconcile_errors_total{job="racer-controller"}[5m])
)
/
sum by (controller) (
  rate(controller_runtime_reconcile_total{job="racer-controller"}[5m])
)
```

Multiply by 100 for percent. Terminal errors are included but handled conflicts
are not. Inspect `controller_runtime_terminal_reconcile_errors_total` separately
during cancellation/failover; a terminal error does not request error-backoff
retry. Likewise, `workqueue_retries_total` includes healthy delayed scheduling
and is not a failure counter.

**p95 completed reconcile duration, seconds:**

```promql
histogram_quantile(
  0.95,
  sum by (controller, le) (
    rate(controller_runtime_reconcile_time_seconds_bucket{job="racer-controller"}[5m])
  )
)
```

This includes catalog-gate waiting and reconcile handling but excludes workqueue
waiting. There is no outcome label, so successes and failures share a distribution.
Classic finite buckets end at 60 seconds and cannot resolve longer tails. Check
observation volume with `increase(controller_runtime_reconcile_time_seconds_count[5m])`
(using the same job selector); sparse rotations do not yield stable percentiles.
An unfinished reconcile has not contributed a duration observation yet.

Sources: `internal/racer/topology_controller.go:39`,
`internal/racer/topology_controller.go:213`, `internal/racer/keyring_controller.go:78`,
`internal/racer/rotation_reconcile.go:104`; controller-runtime
`pkg/internal/controller/metrics/metrics.go:27`,
`pkg/internal/controller/controller.go:464`.

### Queue waiting and blocked work

**Instantaneous worker occupancy, per process/controller:**

```promql
controller_runtime_active_workers{job="racer-controller"}
/
controller_runtime_max_concurrent_reconciles{job="racer-controller"}
```

The configured maximum is one for each Racer controller. Occupancy is not CPU
utilization: waiting on the API or catalog gate occupies the worker too.

**Age of the oldest in-progress queue item, seconds:**

```promql
max by (instance, controller) (
  workqueue_longest_running_processor_seconds{job="racer-controller"}
)
```

This can expose stuck work before a histogram observation arrives. Racer does
not configure the controller-runtime reconcile-timeout guardrail, so
`controller_runtime_reconcile_timeouts_total` is not a stuck-work detector here.

**Ready backlog, summed across priority labels:**

```promql
sum by (instance, controller) (workqueue_depth{job="racer-controller"})
```

**p95 ready-queue wait, seconds:**

```promql
histogram_quantile(
  0.95,
  sum by (controller, le) (
    rate(workqueue_queue_duration_seconds_bucket{job="racer-controller"}[5m])
  )
)
```

The current default is a priority queue. Depth and waiting exclude scheduled
backoff/rotation delay; duplicate events coalesce. With singleton work, low depth
does not prove spare capacity. Sources: `internal/racer/manager.go:142`;
controller-runtime `pkg/controller/controller.go:271`,
`pkg/controller/priorityqueue/metrics.go:92`,
`pkg/internal/metrics/workqueue.go:46`.

### Kubernetes API and Go process pressure

**Outbound Kubernetes API attempts per second, including failures:**

```promql
sum by (method, code) (
  rate(rest_client_requests_total{job="racer-controller"}[5m])
)
```

`code` is an HTTP status or `<error>` for no response. Inspect 409 conflicts,
429 throttling, 5xx responses, and transport errors separately. The count includes
retry attempts and has no endpoint/resource label; it cannot isolate TokenReview
or measure inbound bootstrap/snapshot traffic. Sources: controller-runtime
`pkg/metrics/client_go_adapter.go:125`; client-go v0.37.0
`rest/with_retry.go:238`.

| Query, per process | Unit and use |
| --- | --- |
| `rate(process_cpu_seconds_total{job="racer-controller"}[5m])` | CPU cores consumed, not percent of a configured CPU limit. |
| `process_resident_memory_bytes{job="racer-controller"}` | RSS bytes; compare with container memory and limits separately. |
| `go_memstats_heap_alloc_bytes{job="racer-controller"}` | Currently allocated Go heap bytes. |
| `rate(go_memstats_alloc_bytes_total{job="racer-controller"}[5m])` | Heap allocation bytes/second, useful for spotting allocation churn. |
| `go_goroutines{job="racer-controller"}` | Goroutine count; inspect trends under stable load. |
| `rate(go_gc_duration_seconds_sum{job="racer-controller"}[5m])` | GC pause seconds/second, not total GC CPU. |

These cover the whole controller process, including HTTPS serving, not just
reconciliation. Do not aggregate GC summary quantiles across replicas. Collector
registration: controller-runtime `pkg/internal/controller/metrics/metrics.go:94`;
definitions: client_golang v1.24.1 `prometheus/process_collector.go:68`,
`prometheus/go_collector.go:220`.

## 4. Optional: client-facing throughput and latency through Gantry

The Rust exporter cannot answer these questions directly. When Gantry is using
Racer, scrape its metrics in addition to Racer's.

**Downstream GET body throughput, MiB/s:**

```promql
sum(rate(gantry_racer_mirror_bytes_total{job="gantry",method="GET"}[5m])) / 1024 / 1024
```

Bytes are accepted by the response writer, including partial/error bodies, and
credited when the handler exits. This is not continuously sampled network
bandwidth or verified image goodput; long streams make short windows bursty.

**p95 GET handler duration, seconds:**

```promql
histogram_quantile(
  0.95,
  sum by (le) (
    rate(gantry_racer_mirror_duration_seconds_bucket{job="gantry",method="GET"}[5m])
  )
)
```

This includes downstream writes/final flush, errors, and aborts. It is neither
dataplane-only latency nor whole-image pull latency. Buckets are coarse and the
histogram has no status/outcome label. Inspect outcomes alongside it:

```promql
sum by (status, outcome) (
  rate(gantry_racer_mirror_requests_total{job="gantry",method="GET"}[5m])
)
```

`outcome="complete"` can include a successfully delivered HTTP error response;
`status="200"` or `"206"` can still have `outcome="aborted"`.

**SDK admission pressure:** graph
`gantry_racer_sdk_queue_depth{job="gantry"}` per process, then inspect full-queue
rejections:

```promql
sum by (instance) (rate(gantry_racer_sdk_queue_rejections_total{job="gantry"}[5m]))
```

Split queue depth using `gantry_racer_sdk_bulk_queue_depth`,
`gantry_racer_sdk_metadata_queue_depth`, and
`gantry_racer_sdk_small_object_queue_depth`. These describe Gantry SDK admission,
not Rust worker queues. `gantry_racer_sdk_queue_timeouts_total` counts configured
queue-timer expiry, not every caller cancellation. Sources:
`cmd/gantry/agent_racer_metrics.go:25`, `cmd/gantry/agent_racer_metrics.go:83`,
`internal/gantry/mirror/racer_io.go:33`, `pkg/racersdk/response_conn.go:135`.

For controlled experiments with the optional Racer load generator, verified
whole-image goodput in decimal Gbit/s is:

```promql
8 * sum(rate(racer_loadgen_verified_bytes_total{job="racer-loadgen"}[5m])) / 1e9
```

This target is separate again. Bytes are credited only at complete successful
verified image pulls, and remain zero when verification is disabled. Sources:
`cmd/racer-loadgen/metrics.go:28`, `cmd/racer-loadgen/pull.go:140`.

## 5. Current coverage gaps and recommended additions

These are **missing instrumentation**, not additional queries that work today.

| Gap | What cannot currently be concluded | Useful addition |
| --- | --- | --- |
| Dataplane latency and throughput | No native request/fill/peer latency percentiles, time to first byte, or byte-based source bandwidth. Page events are not bandwidth. | Bounded-label duration histograms and bytes at explicit read/write/delivery boundaries. |
| Dataplane saturation detail | Node aggregates hide hot workers; no worker CPU, reactor/crypto queue wait, coalesced-waiter count, or used-versus-limit resource view. | Per-worker utilization and queue/slot occupancy with limits; queue-wait histograms. |
| Cache and storage occupancy | Capacity gauges do not show current memory/disk residency, eviction rate, physical I/O latency, or all outstanding kernel writes. | Used bytes/entries, eviction reasons, physical I/O bytes/latency, completion-owned write count. |
| Failure attribution | Dataplane request errors have no reason/status labels; source counters cannot identify a slow/failing peer. | Bounded error classes and transport/source-level success/failure and latency metrics, without digest/request-ID labels. |
| Controller control-API serving | No inbound bootstrap/snapshot rate, latency, admission wait/rejection, or long-poll population. Reconcile timing does not cover these handlers. | Endpoint/status counters, active requests/waiters, and separate handler/admission-wait histograms. |
| Controller publication and convergence | No publication age, propagation/acceptance lag, or separate catalog-gate wait measurement. Reconcile success does not prove fleet convergence. | Last-success timestamps, pending publication/acceptance counts, and gate-wait duration. |
| Kubernetes API latency | Default `rest_client_requests_total` provides status counts, not API latency or client-throttle wait. | Explicit opt-in registration of controller-runtime's REST duration/rate-limiter collectors if needed. |
| End-to-end user experience | Gantry handler bytes/latency do not prove completed verified image pulls or identify every phase of a pull. | Application-side pull outcome/duration/verified bytes, or the optional load generator for experiments. |

The complete dataplane exporter is in
`cmd/racer-dataplane/src/telemetry/metrics.rs:88` and
`cmd/racer-dataplane/src/telemetry/server.rs:352`. Controller control handlers are
in `internal/racer/server.go:139`; no metrics wrapper is installed there.
REST latency, size, and retry collectors are opt-in in controller-runtime
`pkg/metrics/client_go_adapter.go:125`, and Racer does not register them in
`internal/racer/manager.go:102`. Do not assume their existence merely because
their names appear in dependency source.

Use container/node monitoring for dataplane CPU, memory, throttling, network,
and physical disk I/O; the custom Rust endpoint does not install Go/process
collectors. Correlate those signals with the per-instance queries above, but do
not attribute shared node traffic to Racer without isolating it.

### Interpretation cautions from existing documentation

- `cmd/racer-dataplane/src/telemetry/INTEGRATION.md:70` describes peer fills as
  validated and published. Current ciphertext-only paths can count peer hits
  without local plaintext validation, and optional persistence may be skipped
  (`cmd/racer-dataplane/src/read/fill.rs:779`, `:962`). This guide uses the narrower
  observed source-work interpretation; the older wording is not a universal
  integrity or residency guarantee.
- Do not extend the resource-fencing wording in that document (`:39`) to every
  gauge: the pending-write test explicitly reaches zero pending entries while
  a kernel write remains (`cmd/racer-dataplane/src/store/tests.rs:646`).
- `racer_diagnostic_failures_total` counts requests to `/debug/failures`, not
  runtime failures (`cmd/racer-dataplane/src/telemetry/server.rs:325`). That bounded
  debug endpoint is useful during investigation but is not a failure-rate metric.

Start dashboards with availability, dataplane exchange/source rates and active
work, controller reconcile duration and oldest work, then add Gantry latency and
throughput when present. Compare equivalent cold/warm workloads and per-node
outliers before choosing alert thresholds; the counters alone do not establish
universal performance targets.
