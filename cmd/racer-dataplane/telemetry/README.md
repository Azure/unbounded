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
