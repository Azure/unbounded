# Telemetry integration

`Telemetry::default()` is side-effect-free: no sockets, threads, executors, or
kernel resources. The app owner must integrate these explicit hooks:

1. On the designated I/O worker, call
   `telemetry.attach_io(runtime.reactor.clone(), runtime.admission.clone())`
   before accepting data work. This initializes the supplied worker reactor and
   reserves five `ControlProgress` slots plus 340 KiB of `RequestContext` memory.
   Attachment failure is a startup failure. It does not bind or start serving.
2. Retain and poll `telemetry.serve(config.diagnostics_listen, &scope)` as an
   ordinary local worker task. First poll binds; surface bind/accept failures.
   No internal executor or thread drives the future. Continue the worker's
   regular reactor completion/timer drive. A shared `Rc<Telemetry>` can be moved
   into an owned async task to obtain the app's usual `'static` task shape.
3. Alternatively retain `Rc<DiagnosticIo::attach(reactor, admission)?>` and call
   `serve_with_io(address, io, &scope)`. For prebound sockets or port-zero tests,
   transfer a `TcpListener` to `serve_listener_with_io(listener, io, &scope)`.
   Each attachment supports one serving listener at a time.
4. Publish `health.observe(Resources { ... })` from actual startup/progress
   checks aggregated over all required workers, then `health.transition(Ready)`.
   All five usability bits, a future credential expiry, and a future observation
   expiry are required. Refresh before `observed_until`; expiry automatically
   yields degraded/not-ready without another callback. Map credential wall-clock
   expiration conservatively into monotonic time and update on renewal/revocation.
   `Starting`, `Degraded`, and `Draining` are live but not ready. Transition to
   `Draining` before shutdown and `Stopped` when unusable; both are irreversible
   toward `Ready`. A stopped attached `Admission` overrides ready observations.
5. Keep diagnostics polled during data drain. Cancel its scope when shutting down
   the listener, drop/finish the task, then drive the worker reactor's completion
   fence before releasing attachment. Abandoned I/O retains buffers, reservations,
   and connection gauges through completion.

`Metrics`, `Health`, and `Tracing` are cheap cloneable shared handles. Initialize
node-wide metrics with `Metrics::for_workers` and assign one returned handle per
worker before wrapping its `Telemetry` in `Rc`. Clones retain that worker's event
shard; clone `Health` and `Tracing` for shared state. Only the designated worker
attaches/binds diagnostics.
`metrics.record(Event, amount)` has fixed saturating counters;
`metrics.lease(Gauge)` returns a resource-lifetime gauge guard. Keep guards with
the actual owned resource, not just its waiting future. No request or cache labels
are accepted. `tracing.event(RequestId, Option<AttemptId>, Stage)` stores at most
128 typed correlation records. No text, headers, keys, credentials, formatting
callbacks, or logging sinks can be attached. Trace records are not HTTP output.

## Dataplane metrics

The application allocates a fixed metrics registry at startup and gives each
worker a separate 64-byte-aligned event counter block. Its client listeners, page
fills, and store readers/writers clone that worker's handle. Recording touches
only that block; reads sum all blocks with saturation. The registry retains counts
after worker handles are dropped. No registration or locks occur on event writes.

Gauges remain node-wide, with each atomic on a separate 64-byte cache line. This
preserves exact aggregate lease overflow rejection, cross-thread guard release,
additive capacity totals, and replacement values for credentials and checkpoints.
Active-resource gauges still share writes between workers of the same resource
class; sharding them would require a separate aggregate overflow protocol.
Scrapes use relaxed atomic observations, not a coherent multi-worker snapshot.

Scrape GET `/metrics` on the existing
diagnostics listener. Every series is label-free and process-wide; Prometheus can
attach target labels. Counters reset on process restart and saturate at `u64::MAX`.

| Metric | Meaning |
| --- | --- |
| `racer_requests_total` | Client UDS exchanges admitted after header reception, including rejected parsed heads. Idle connections and header I/O failures are excluded. |
| `racer_request_errors_total` | Admitted exchanges that did not finish successful response delivery: protocol/read errors, cancellation, timeout, truncated delivery, or abandonment. |
| `racer_active_requests` | Admitted client exchanges still processing or delivering a response. |
| `racer_memory_hits_total` | Successful page acquisitions/copy-only reads from memory or pending dirty ciphertext. |
| `racer_disk_hits_total` | Successful page acquisitions/copy-only reads from disk. |
| `racer_peer_hits_total` | Successfully validated and published peer page fills. |
| `racer_origin_fills_total` | Successfully encrypted and published origin pages, including bootstrap page zero. |
| `racer_active_fills` | Elected page acquisitions retained through their operation completion fence, including local disk/decrypt work. Coalesced waiters are excluded. |
| `racer_overloads_total` | Admitted client exchanges that encountered `Overloaded`, at most once per exchange. Optional persistence skips and internal recovered pressure are excluded. |
| `racer_corrupt_misses_total` | Disk or peer copies rejected for corrupt framing, identity, or AEAD validation. Missing keys and ordinary I/O misses are excluded. |
| `racer_dirty_discards_total` | Accepted queued dirty copies discarded before persistence, including reclamation, retirement, cancellation, and persistence failure. Declined enqueue attempts are excluded. |

Page-source counters measure actual successful source work, including peer-serving
copy-only reads. They do not count coalesced waiter deliveries again and are not
request counts: HEAD requests need no page and ranges may need many pages. Empty
origin bootstrap responses do not fill a page. `racer_active_requests` tracks the
exchange task lifetime; `racer_active_fills` remains set after a waiting request
disappears until retained fill work is fenced.

The existing `racer_live`, `racer_ready`, and `racer_diagnostic_*` series remain
available. Diagnostics do not increment client request counters.

## Bounds and runtime integration constraint

HTTP/1.1 GET `/healthz`, `/readyz`, `/metrics`, and `/debug/failures` are served.
Responses close the connection, have exact Content-Length, and never echo input. Heads are capped
at 1 KiB/16 headers, responses at 64 KiB, active connections at four, and exchange
time at two seconds. Bodies, transfer encoding, duplicate framing, and unsupported
methods are rejected. Unknown paths and query strings produce fixed 404s.

Diagnostics use their startup-reserved memory/control capacity instead of the
ordinary Connection, Plaintext, or Ciphertext pools, and remain operational after
ordinary admission stops. However, the current reactor `submit` still charges
each submission to shared `RequestContext` memory and caps all in-flight entries
at `queue_entries`. It has no reserved-control submission API. The runtime/app
integrator must preserve at least five submission slots and submission-bookkeeping
headroom from data work (or provide a reactor reserved-control API) to guarantee
diagnostic progress under *complete* shared reactor/memory saturation. Telemetry
cannot enforce that global ceiling from its owned files. It does not create a
second reactor to conceal this constraint. Reactor and Admission arguments must
come from the same worker; Reactor currently exposes no provenance check.

## Internal failure diagnostics

GET `/debug/failures` returns the latest 128 internal failure records, oldest
first, across the node's workers. It uses the existing diagnostic listener and
access controls. `total` counts recorded internal failures since process startup;
`retained` and `sequence` reveal ring overwrites. Snapshot reads do not clear it.
This is diagnostic text, not a Prometheus metric or a stable public wire schema.

Records include worker, wall-clock Unix milliseconds, typed stage/error, and
request/attempt IDs when available. Page acquisition records include the page
number; client continuation/write records include complete slice bytes sent and
advertised body bytes. `ClientWrite.sent` excludes any partial current slice.
Candidate records preserve remote statuses and remaining retry/link credits.
Peer records distinguish routing, checkout, authentication, response heads,
receive admission/body, decoding, local serving, and relay failures before the
existing error mappings collapse them.

Admission records capture the rejected resource, worker used/limit, requested
amount, and cache used/fair limit when cache fairness rejected the reservation.
They have no request ID because the quota API does not receive request context;
use worker/time/sequence with surrounding correlated stage records. These record
individual reserve failures, including those later recovered by reclamation or
optional persistence skips. They are not terminal request counts. Cache-entry
table saturation uses a separate `CacheEntries` detail.

No object key, ETag, origin context, credential, header, or payload is accepted by
the record API. Failure recording uses a fixed ring and a short node-local mutex;
formatting and HTTP I/O happen after releasing it. Successes add no records.
Collect snapshots promptly from requesting nodes and peer candidates/relays while
reproducing a failure. A missing or overwritten record is not evidence of success.

## Focused checks

Run `cargo test --lib telemetry::`. If unrelated concurrent app changes block the
crate, `bash src/telemetry/check-component.sh` compiles these same telemetry tests
with production reactor, admission, deadline, and model modules against built
dependencies. Raw TCP tests drive the real io_uring reactor on the calling test
thread. No socket test is silently skipped when io_uring is unavailable.
Phase 3 adds `racer_active_deliveries`, a fixed gauge held by each `ReaderLease`.
It counts actual attached delivery pipes, including completion-owned leases after
ingress cancellation. It is not a count of queued requests or a sampled peak.
