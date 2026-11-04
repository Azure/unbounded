# racer-object

Serve cached objects to pods through Racer with an S3 or Azure Blob client API.
The Racer dataplane reads the cache's Azure Blob container directly, using the
ClusterCache `spec.origin`. The sidecar holds no upstream credentials and
serves trusted pod-local clients without authentication.
See the [Azure Blob guide](../../docs/content/guides/azure-blob-cache.md) for setup, and the
[reference](../../docs/content/reference/racer-object.md) for flags, deployment
requirements, request limits, and the security model.

## Example

Set `--cache` to a ClusterCache with `spec.origin.azure`. The sidecar forwards
`/<bucket>/<key>` requests unchanged; Racer forbids any bucket other than the
cache's container. This serves the Azure Blob client API:

```sh
racer-object sidecar --cache=racer-object \
  --api=azure --listen=127.0.0.1:8080
```

Use `--api=s3` (the default) for S3 clients. Mount the cache's Racer client
socket directory into the sidecar container.

## Diagnostics

`sidecar` accepts `--debug-listen`. Its default is empty,
which disables the diagnostic listener. For local access, explicitly use
`--debug-listen=127.0.0.1:6060`.

The separate HTTP listener uses an explicit mux exposing `/debug/pprof/`
(including CPU `profile`, heap, goroutine, cmdline, symbol, and trace) and
`/healthz`. Health only indicates that diagnostics is serving, not that the
cache or sidecar is ready. Diagnostic routes are not added to the
sidecar object listener. A bind failure prevents startup; the diagnostic
listener closes when the main server exits, including startup errors and
context cancellation. Active profiles can be interrupted on exit.

These endpoints have **no authentication or TLS**. Profiles and command lines
can reveal sensitive process information, and profiling consumes resources.
Allow only trusted diagnostic access. `--debug-listen=:6060` is supported for
pod-IP access through a Kubernetes API pod proxy, but binds all interfaces:
the proxy's authentication does not protect direct access to the pod port.
Restrict direct access with network policy or equivalent controls, avoid public
Services, and disable the listener after profiling.

Capture a CPU profile during a representative steady workload through your
trusted diagnostic connection, for example from a local listener:

```sh
timeout --signal=TERM --kill-after=10s 45s curl --fail --max-time 40 \
  -o racer-object-cpu.pprof 'http://127.0.0.1:6060/debug/pprof/profile?seconds=30'
timeout --signal=TERM --kill-after=10s 60s go tool pprof -top racer-object-cpu.pprof
```

Retain the workload concurrency, object
size, verification setting, throughput, CPU usage, and binary revision with
the profile. Profiling enables measurement; it does not change object handling
or imply a particular CPU bottleneck.
