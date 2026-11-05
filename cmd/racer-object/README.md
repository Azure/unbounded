# racer-object

Serve cached objects to pods through Racer with an S3 or Azure Blob client API.
The Racer dataplane reads the cache's Azure Blob container directly, using the
ClusterCache `spec.origin`. The sidecar holds no upstream credentials and
serves trusted pod-local clients without authentication.
See the [Azure Blob guide](../../docs/content/guides/azure-blob-cache.md) for setup, and the
[reference](../../docs/content/reference/racer-object.md) for flags, deployment
requirements, request limits, and the security model.

This directory is a standalone Rust crate (its own Cargo workspace). The
object identity (canonical JSON and its SHA-256 Racer key) matches the
in-tree library [`internal/racerobject`](../../internal/racerobject) byte for
byte, so both compute the same keys.

## Example

Set `--cache` to a ClusterCache with `spec.origin.azure`. The sidecar forwards
`/<bucket>/<key>` requests unchanged; Racer forbids any bucket other than the
cache's container. This serves the Azure Blob client API:

```sh
racer-object sidecar --cache=racer-object \
  --api=azure --listen=127.0.0.1:8080
```

Use `--api=s3` (the default) for S3 clients. Mount the cache's Racer client
socket directory into the sidecar container. The sidecar connects to
`/run/racer/<cache>/client/socket`.

Flags: `--cache` (required), `--api`,
`--listen` (loopback IP and port), `--request-timeout`, `--max-connections`,
`--workers` (see [Workers](#workers)), `--debug-listen`, and `--version`. `--flag=value`, `--flag value`, and a
single dash all work. Invalid arguments print `racer-object: <message>` and
exit with status 1.

## Build and test

```sh
cargo build --release --locked --manifest-path cmd/racer-object/Cargo.toml
cargo test --locked --manifest-path cmd/racer-object/Cargo.toml
cargo clippy --locked --manifest-path cmd/racer-object/Cargo.toml --all-targets -- -D warnings
```

Set `RACER_OBJECT_VERSION`, `RACER_OBJECT_GIT_COMMIT`, and
`RACER_OBJECT_BUILD_TIME` at build time to fill in `--version`.

Unit tests in each module cover the request rules, the Racer client socket
protocol (request bytes, response heads, and page frames), and response
building. Integration tests in `tests/` run the server over TCP with no Racer
listening, so they cover rejected requests, connection handling, workers, and
shutdown. Reads through a real Racer are covered by the kind-based suite in
[`e2e/racer`](../../e2e/racer).

## Design

- `src/object.rs`: request rules. Paths, queries, headers, conditions, ranges,
  error codes, and the object identity (canonical JSON, byte for byte the same
  as `internal/racerobject`, and its SHA-256 key).
- `src/racer.rs`: Racer client socket protocol. Metadata `HEAD` requests reuse
  at most four idle connections in total, shared by all workers. Reads are credit-paced
  subscriptions on their own connection, with frame checks.
- `src/server.rs`: a small HTTP/1.1 server with keep-alive and pipelining, and
  the request flow.
- `src/splice.rs`: the zero-copy body path.
- `src/debug.rs`: the optional `--debug-listen` listener.

Request flow:

- An unconditional `GET` sends one subscription to Racer with the client's
  range. It does not send a `HEAD` first. The sidecar checks that Racer chose
  the same bytes it would have chosen.
- `HEAD` and conditional `GET` requests (`If-Match`, `If-None-Match`) ask Racer
  for metadata first. A `GET` then reads that exact version (`If-Match` on the
  Racer request).
- Bodies up to 64 KiB are read into memory and sent with the response head in
  one `writev`. If Racer fails before the end, the client still gets an error
  status.
- Larger bodies move from the Racer socket to the client socket with
  `splice(2)` through a pipe (1 MiB when the kernel allows it). The data never
  enters user space. The last byte is held back until Racer confirms the read
  completed, so a cut-off read can never look complete. If the first Racer
  frame is broken, the client gets `502`. If Racer fails after the head was
  sent, the sidecar closes the connection early.
- Requests with a body get a response, then the connection closes.
- Request heads over 36 KiB get `431` and malformed requests get `400`, both
  as the S3 or Azure XML error, then the connection closes.
- When an S3 query has both a malformed parameter (`400`) and an unsupported
  one (`501`), the sidecar returns `400`.

### Why tokio and splice, not io_uring

- Pods normally run with the `RuntimeDefault` seccomp profile. The containerd
  and Docker default profiles block `io_uring_setup`, so io_uring is not
  available.
- io_uring does not run `IORING_OP_SPLICE` inline. The kernel hands it to an
  io-wq worker thread, so it saves no CPU compared to calling `splice(2)` from
  an epoll loop.

### Workers

`--workers=N` (1 to 256; default: usable CPUs, at most 4) starts N OS
threads, each running its own single-threaded tokio runtime (epoll) with its
own Racer idle connection pool; the process keeps at most four idle
connections in total, shared by all workers. There is no work stealing:

- The first worker accepts every connection and hands it to the worker
  serving the fewest connections (one cross-thread wakeup per connection,
  not per request). A shared listener or `SO_REUSEPORT` would let a few
  long-lived connections land on one worker, and `SO_REUSEPORT` would also
  split `--max-connections` per worker.
- `--max-connections` stays global: one semaphore bounds accepted client
  connections and another two bound concurrent Racer reads, all shared.
- SIGTERM or SIGINT stops every worker; each lets in-flight requests finish
  for up to 5 seconds. The debug listener runs on the first worker.
- With one worker the binary runs a plain current_thread runtime.

Separate single-threaded runtimes keep the one-thread cost per request and
per byte; a shared work-stealing runtime adds cross-thread wakeups and costs
more CPU per request. The cap of 4 bounds idle threads in pods without CPU
limits; set `--workers=1` for the smallest footprint.

## Diagnostics

`--debug-listen` is empty by default, which disables the diagnostics
listener. When set (for example `--debug-listen=127.0.0.1:6060`), it serves
only `GET /healthz`, which returns 200. Health only means the listener is up,
not that the cache is ready. A bind failure stops startup.

Profile the sidecar with `perf`; release builds keep line tables, and the
container image builds with frame pointers:

```sh
timeout --signal=INT 40s perf record -F 999 -g -p "$(pidof racer-object)" -- sleep 30
timeout --signal=TERM 60s perf report --stdio
```

The listener has **no authentication or TLS**. `--debug-listen=:6060` binds
all interfaces. Allow only trusted access.
