# racer-object

Serve cached objects to pods through Racer with an S3 or Azure Blob client API.
The Racer dataplane reads the cache's Azure Blob container directly, using the
ClusterCache `spec.origin`. The sidecar holds no upstream credentials and
serves trusted pod-local clients without authentication.
See the [Azure Blob guide](../../docs/content/guides/azure-blob-cache.md) for setup, and the
[reference](../../docs/content/reference/racer-object.md) for flags, deployment
requirements, request limits, and the security model.

This directory is a standalone Rust crate (its own Cargo workspace). The
request rules are a port of the Go library in
[`internal/racerobject`](../../internal/racerobject), which other Go code still
uses.

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

Flags are the same as the Go version: `--cache` (required), `--api`,
`--listen` (loopback IP and port), `--request-timeout`, `--max-connections`,
`--debug-listen`, and `--version`. `--flag=value`, `--flag value`, and a
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

Integration tests in `tests/` run the server over TCP against an in-process
fake Racer that speaks the client socket protocol and can inject failures.

## Design

- `src/object.rs`: request rules. Paths, queries, headers, conditions, ranges,
  error codes, and the object identity (canonical JSON, byte for byte the same
  as Go `encoding/json`, and its SHA-256 key).
- `src/racer.rs`: Racer client socket protocol. Metadata `HEAD` requests reuse
  a pool of at most four idle connections. Reads are credit-paced
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
  completed, so a cut-off read can never look complete. If Racer fails after
  the head was sent, the sidecar closes the connection early.

### Why tokio and splice, not io_uring

- Pods normally run with the `RuntimeDefault` seccomp profile. The containerd
  and Docker default profiles block `io_uring_setup`, so io_uring is not
  available.
- io_uring does not run `IORING_OP_SPLICE` inline. The kernel hands it to an
  io-wq worker thread, so it saves no CPU compared to calling `splice(2)` from
  an epoll loop.

The server uses a single-threaded tokio runtime (epoll). The work is mostly
system calls, and more threads cost more CPU for the same work.

### Measurements

Loopback, one shared fake Racer (Go), the sidecar process CPU only. 16
connections for 10 KiB objects, 8 for 1 MiB, 4 for 64 MiB. `rust-tN` is a
tokio runtime with N worker threads; `rust-t0` is the single-threaded runtime
that the binary uses.

| Workload | Go CPU | rust-t0 | rust-t1 | rust-t2 | rust-t4 |
|---|---|---|---|---|---|
| 10 KiB GET, CPU per request | 600-680 us | 89-93 us | 88-94 us | 99-106 us | 115-122 us |
| HEAD, CPU per request | 250-290 us | 46-51 us | 47-51 us | 53-65 us | 72-74 us |
| 1 MiB GET, CPU per GB | 1.8-1.9 s | 0.66-0.72 s | 0.67-0.85 s | 0.80-0.91 s | 0.89-0.96 s |
| 64 MiB GET, CPU per GB | 1.34-1.38 s | 0.58-0.65 s | 0.64-0.65 s | 0.81-0.84 s | 0.95-0.99 s |

With one thread the throughput matched the Go sidecar in these runs (about
7,000 to 10,000 small requests per second and 1.1 to 1.3 GB/s for large
objects); the fake Racer was the limit.

## Diagnostics

`--debug-listen` is empty by default, which disables the diagnostics
listener. When set (for example `--debug-listen=127.0.0.1:6060`), it serves
only `GET /healthz`, which returns 200. Health only means the listener is up,
not that the cache is ready. A bind failure stops startup.

The Go version also served `/debug/pprof/`. The Rust binary has no pprof.
Profile it with `perf` instead; release builds keep line tables, and the
container image builds with frame pointers:

```sh
timeout --signal=INT 40s perf record -F 999 -g -p "$(pidof racer-object)" -- sleep 30
timeout --signal=TERM 60s perf report --stdio
```

The listener has **no authentication or TLS**. `--debug-listen=:6060` binds
all interfaces. Allow only trusted access.

## Differences from the Go version

- Bodies up to 64 KiB that fail before the end get an error status. Go sent
  `200` and then cut the connection.
- A large body whose first Racer frame is broken gets `502`. Go sent `200`
  and then cut the connection.
- Unconditional `GET` skips the metadata request to Racer.
- Requests with a body get a response, then the connection closes.
- Request heads over 36 KiB (Go's limit) and malformed requests get the S3 or
  Azure XML error (`431` or `400`) instead of Go's plain text, then the
  connection closes.
- When an S3 query has both a malformed parameter (`400`) and an unsupported
  one (`501`), Go picked one at random. Rust always returns `400`.
- No pprof (see above).
