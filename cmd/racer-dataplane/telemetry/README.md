# telemetry

Small diagnostic primitives used by Racer, without Racer IDs, metric names,
payloads, admission policy, or error types.

- `metrics!` declares fixed metric enums implementing `Metric`.
- `Metrics<C, G>::shards(n)` returns shared registry handles with fixed writer
  shards. Counter blocks and individual gauges are cache-line aligned. Counter
  updates and aggregation saturate; gauge increases wrap. Leases reject overflow
  and release on drop, retaining the registry after workers exit.
- `write_prometheus` writes directly to a caller-provided `fmt::Write`; reads are
  relaxed observations, not an atomic registry snapshot.
- `Ring<T, N>` retains the newest `N` copyable records with saturating sequence
  numbers and oldest-first iteration. Capacity must be positive. The caller owns
  synchronization: clone under a lock and format after releasing it.

Racer owns all application adapters, worker quota labels, failure payloads, and
request/body lifecycle helpers. This crate does not register dynamic metrics or
provide a general observability framework.

## Diagnostic server

`server::Server` serves an already-bound descriptor on a borrowed
`uring_runtime::reactor::Reactor`. The owner explicitly polls both the service
and reactor (at least every 10ms under queue pressure); no executor or background
thread is created. Startup submission capacity and an opaque memory charge are
provided by the caller's budget authority. Buffers retain that charge and the
handler's connection guard until the reactor completion fence, even when a
service future is abandoned. Drain the reactor before releasing worker resources.

The transport permits one listener per server, four concurrent connections,
1024 request bytes, 64KiB response buffers, and two-second connection deadlines.
`Scope::with_deadline` must narrow the caller's deadline while preserving its
cancellation and metadata. Only HTTP/1.1 GET with a single nonempty Host and no
body is accepted; connections always close after one response. Headers are
scrubbed before dispatch, never echoed. The handler receives only the path and a
bounded `fmt::Write`, selects text, Prometheus, not-found, or unavailable output,
and observes fixed transport events. Application routes, trusted response bodies,
health, metrics names, and connection admission remain with the caller.
