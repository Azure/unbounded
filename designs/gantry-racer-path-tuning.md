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

Gantry uses bounded HTTP ReaderFrom transfers without hijacking the connection.
Plain HTTP can use the Go TCP splice path; TLS and unsupported writers still copy.
The explicit SDK FDSink path and worker pipes request at most 64 KiB kernel capacity;
denied growth is not fatal. Worker pipes are pooled only when empty. After the first
backpressure pipe drain on a page, worker delivery switches to direct sends while
preserving connection/page admission through the reactor completion fence.
`racer_delivery_pipe_drains_total` and `racer_delivery_direct_bytes_total` expose
this behavior. These changes remove specific overhead, not a measured throughput
guarantee; compare throughput, CPU, and small-object latency under mixed load.
