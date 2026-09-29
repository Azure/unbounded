# Gantry and Racer path tuning

## Capacity is a pipeline, not one connection count

Gantry exports effective SDK pool and queue limits alongside occupancy. Compare
`gantry_racer_sdk_{bulk,metadata,small_object}_limit` with the corresponding active
gauges, and queue limits with queue depths. Zero configuration selects SDK defaults;
the metrics report those effective values.

Do not raise SDK admission to compensate for worker pipe or memory contention.
Racer divides node budgets among worker pairs. For each worker with C connections,
ingress capacity is C - min(C/8, 2) - C/4 (integer division). Peer ingress shares
that capacity. Pipes limit simultaneous deliveries independently; prefetched pages
also consume the worker's plaintext budget. Budget origin connections per worker
against Gantry's shared origin listener and callback limits, including idle sockets.
There is no negotiated cross-process capacity protocol, so these are deployment
budgets, not automatically enforced equalities.

The mixed-load harness now includes the production HTTP wrapper and defaults to
16 pipes, 256 MiB ciphertext, a 30-second worker request deadline, and a 10-second
reader stall deadline. Explicit `RACER_PERF_PIPES`, `RACER_PERF_CIPHERTEXT_BYTES`,
`RACER_PERF_REQUEST_TIMEOUT_MS`, `RACER_PERF_READER_STALL_TIMEOUT_MS`,
`RACER_PERF_MAX_THREADS`, and `RACER_PERF_CLIENT_CONNECTIONS` override these values
and are recorded in the run's dataplane configuration. Compare production defaults
before increasing budgets. One worker pair remains the harness baseline; test
multiple pairs explicitly rather than extrapolating single-worker results.

## Delivery

Gantry uses SDK WriteToHTTP without hijacking the connection. Subscription frames
are validated before copying their payload through 32 KiB scratch; this path does
not invoke the destination's ReaderFrom or splice the framed SDK socket, even for
plain HTTP (`internal/gantry/mirror/racer.go:125-135`,
`pkg/racersdk/http_transfer.go:8-14`, `pkg/racersdk/value.go:217-335`). Get receives
ahead with at most two request-local payload buffers, reduced to one by page/byte
credits, and reuses storage only after release (`pkg/racersdk/ordered.go:11-133`).
Worker pipes are pooled only when empty. After the first
backpressure pipe drain on a page, worker delivery switches to direct sends while
preserving connection/page admission through the reactor completion fence.
`racer_delivery_pipe_drains_total` and `racer_delivery_direct_bytes_total` expose
this behavior. These changes remove specific overhead, not a measured throughput
guarantee; compare throughput, CPU, and small-object latency under mixed load.

## Transport controls

Gantry exposes the following YAML settings, matching `--racer-...` flags and
`GANTRY_RACER_...` environment variables:

| Setting | Default | Purpose |
| --- | --- | --- |
| `racer_max_conn_age` | `5m` | Jittered rotation of reusable connections; subscriptions already use fresh connections |
| `racer_idle_conn_timeout` | `20s` | Retire idle SDK sockets before the worker's default 30-second idle deadline |
| `racer_dial_timeout` | `5s` | Bound UDS connection establishment |
| `racer_body_read_timeout` | `60s` | Bound an active body read or bounded socket transfer, not total object lifetime |
| `racer_page_window` | `0` | Default subscription page credits; zero selects two, explicit range 1..64 |
| `racer_prefetch_bootstrap` | `false` | Unused compatibility setting; no bootstrap or continuation client requests |
| `racer_http_max_connections` | `512` | Bound accepted public mirror connections |

The public Racer mirror also has a 30-second HTTP idle timeout and a 32 KiB
configured header limit. SDK payload reads and release writes have bounded I/O
deadlines; the caller context bounds the entire Value. A stalled arbitrary
destination Write cannot be interrupted by SDK cancellation, but cleanup joins
the receiver and drops page buffers without waiting for that Write. Only separately
bounded 32 KiB copy scratch remains with the writer. Tune these bounds together,
not as interchangeable total request deadlines (`pkg/racersdk/subscription.go:565-650`,
`pkg/racersdk/ordered.go:76-98`, `pkg/racersdk/value.go:217-335`).

Each subscription uses one POST/connection, regardless of page credits. Ordered
Rust acquisition overlaps across stable page owners within the smaller of page
credits and `RACER_RANGE_WINDOW_PAGES` (default two), including delivered but
unreleased pages. Prefetch can admit work while sending when credits, window,
budgets, and fair arbitration with unordered selections permit. More credits do
not enlarge Get's two-buffer SDK limit or automatically raise Rust payload/pipe
capacity. Whole-page validation remains an integrity boundary
(`cmd/racer-dataplane/src/read/range_stream.rs:259-355,628-658`,
`cmd/racer-dataplane/src/read/subscription.rs:483-535`).

## Isolation and remaining architectural limits

- The worker origin pool reserves one endpoint slot from GETs when its endpoint
  limit exceeds one, and metadata can bypass queued GETs. This does not reserve
  global outbound admission or Gantry origin listener connections. At an endpoint
  limit of one, there is no reserved slot.
- Manifests and already-statted small ranged objects use SDK small-object admission.
  Unknown-size full blobs still use bulk admission; an extra HEAD is not introduced
  merely to classify them. Worker delivery pipes and memory remain shared.
- Accepted UDS connections still use sequential HTTP/1 exchanges on their assigned
  ingress worker. Rotation bounds stickiness; it is not byte-load-aware scheduling.
  Batching ingress handoffs reduces lock acquisitions, but page-owner dispatch and
  metadata-owner coordination still exist.
- Effective SDK limit gauges make budget mismatches visible, but do not negotiate
  limits with Racer. There is still no full phase-latency or per-worker load profile.
- Origin callback buffers retain at most 64 cleared 32 KiB buffers between uses.
  Active callbacks still own their buffers until reads return. TLS decryption,
  strict framing validation, per-chunk flushes, and bounded page origin GETs remain.
- Token refresh coalesces matching in-flight challenges per registry and protects
  newer tokens from late rejections. Distinct scopes serialize behind the current
  refresh; the cache remains one token per registry. The initiating request's
  cancellation can fail its shared refresh. Delegated credentials remain separate.
- The public connection cap bounds accepted sockets, not the kernel listen backlog.
  SDK admission and worker resource failures can still return overload responses.

These are targeted fixes and mitigations for all audit categories, not a claim
that all possible bottlenecks have been eliminated. Evaluate remaining limits
with production-shaped traffic before larger protocol or scheduling changes.

## Validation

The affected SDK, mirror, origin, configuration, Gantry, and load-generator Go
packages passed their complete test suites. Race-enabled SDK, origin, and mirror
suites passed. The Rust dataplane library suite passed 813 tests with four ignored.
Validation also exposed an existing containerd range-test accounting assertion
that ran before the upstream handler finished; it now synchronizes with handler
completion without weakening the exact-byte assertion.

The release performance harness builds with the production HTTP wrapper and
production-sized pipe budget. Historical results that omitted that wrapper or
used 72 pipes are not a before/after baseline for this implementation.
No new throughput result is claimed: the run guard rejected execution while other
Rust builds were active on this host, including after a bounded wait. Run on a
quiet host before attributing a throughput or latency change to these commits.
