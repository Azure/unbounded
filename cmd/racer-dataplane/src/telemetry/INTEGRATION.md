# Telemetry integration

`Telemetry::default()` is side-effect-free: no sockets, threads, executors, or
kernel resources. The app owner must integrate these explicit hooks:

1. On the designated I/O worker, call
   `telemetry.attach_io(runtime.reactor.clone(), runtime.admission.clone())`
   before accepting data work. This initializes the supplied worker reactor and
   reserves five `ControlProgress` slots plus 40 KiB of `RequestContext` memory.
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

`Metrics`, `Health`, and `Tracing` are cheap cloneable shared handles. If worker
telemetry should be node-wide, assign clones of these fields before wrapping each
worker's `Telemetry` in `Rc`. Only the designated worker attaches/binds diagnostics.
`metrics.record(Event, amount)` has fixed saturating counters;
`metrics.lease(Gauge)` returns a resource-lifetime gauge guard. Keep guards with
the actual owned resource, not just its waiting future. No request or cache labels
are accepted. `tracing.event(RequestId, Option<AttemptId>, Stage)` stores at most
128 typed correlation records. No text, headers, keys, credentials, formatting
callbacks, or logging sinks can be attached. Trace records are not HTTP output.

## Bounds and runtime integration constraint

Only HTTP/1.1 GET `/healthz`, `/readyz`, and `/metrics` are served. Responses close
the connection, have exact Content-Length, and never echo input. Heads are capped
at 1 KiB/16 headers, responses at 4 KiB, active connections at four, and exchange
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

## Focused checks

Run `cargo test --lib telemetry::`. If unrelated concurrent app changes block the
crate, `bash src/telemetry/check-component.sh` compiles these same telemetry tests
with production reactor, admission, deadline, and model modules against built
dependencies. Raw TCP tests drive the real io_uring reactor on the calling test
thread. No socket test is silently skipped when io_uring is unavailable.
