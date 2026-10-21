# racer-object

Read S3 objects and Azure block blobs through Racer. The origin holds upstream
credentials; the sidecar serves trusted pod-local clients without authentication.
See the [Azure Blob guide](../../docs/content/guides/azure-blob-cache.md) for setup, and the
[reference](../../docs/content/reference/racer-object.md) for flags, deployment
requirements, request limits, and the security model.

## Example

Use the same `--cache` on the origin and sidecar. Each cache serves exactly one
bucket or container: the origin takes `--bucket` and forbids requests for any
other bucket, and the sidecar forwards `/<bucket>/<key>` requests unchanged.
This pairs an Azure Blob origin with the Azure Blob client API:

```sh
racer-object origin --cache=racer-object \
  --provider=azure --account-url=https://ACCOUNT.blob.core.windows.net \
  --azure-credential=default --bucket=example-container
racer-object sidecar --cache=racer-object \
  --api=azure --listen=127.0.0.1:8080
```

Run these as separate processes with their Racer sockets mounted. The origin
provider (`--provider`) and sidecar API (`--api`) are independent; both default
to `s3`. **Use a new cache when changing bucket, account, provider, or endpoint**;
the cache key includes only the bucket, key, and version, so reusing a cache
can serve stale data. Manifests for both are in
[`deploy/racer-object`](../../deploy/racer-object).

## Diagnostics

Both `origin` and `sidecar` accept `--debug-listen`. Its default is empty,
which disables the diagnostic listener. For local access, explicitly use
`--debug-listen=127.0.0.1:6060`.

The separate HTTP listener uses an explicit mux exposing `/debug/pprof/`
(including CPU `profile`, heap, goroutine, cmdline, symbol, and trace) and
`/healthz`. Health only indicates that diagnostics is serving, not that the
origin, cache, or sidecar is ready. Diagnostic routes are not added to the
sidecar object listener. A bind failure prevents startup; the diagnostic
listener closes when the main server exits, including startup errors and
context cancellation. Active profiles can be interrupted on exit.

These endpoints have **no authentication or TLS**. Profiles and command lines
can reveal sensitive process information, and profiling consumes resources.
Allow only trusted diagnostic access. `--debug-listen=:6060` is supported for
pod-IP access through a Kubernetes API pod proxy, but binds all interfaces:
the proxy's authentication does not protect direct access to the pod port.
Restrict direct access with network policy or equivalent controls, avoid public
Services, and disable the listener after profiling. Use distinct ports if
origin and sidecar share a pod network namespace.

Capture a CPU profile during a representative steady workload through your
trusted diagnostic connection, for example from a local listener:

```sh
timeout --signal=TERM --kill-after=10s 45s curl --fail --max-time 40 \
  -o racer-object-cpu.pprof 'http://127.0.0.1:6060/debug/pprof/profile?seconds=30'
timeout --signal=TERM --kill-after=10s 60s go tool pprof -top racer-object-cpu.pprof
```

Capture each process separately and retain the workload concurrency, object
size, verification setting, throughput, CPU usage, and binary revision with
the profile. Profiling enables measurement; it does not change object handling
or imply a particular CPU bottleneck.
