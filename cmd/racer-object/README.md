# racer-object

Read S3 objects and Azure block blobs through Racer. The origin holds upstream
credentials; the sidecar serves trusted pod-local clients without authentication.
See the [deployment guide](../../docs/content/guides/racer-object.md) for setup,
request limits, and security requirements.

## Azure Blob

Use the same `--volume` on the origin and sidecar:

```sh
racer-object origin --volume=racer-object --namespace=example-azure-store \
  --provider=azure --account-url=https://ACCOUNT.blob.core.windows.net \
  --azure-credential=default --bucket=example-container
racer-object sidecar --volume=racer-object --namespace=example-azure-store \
  --api=azure --bucket=example-container --listen=127.0.0.1:8080
```

Run these as separate processes with their Racer sockets mounted. `--bucket`
means an Azure container. `DefaultAzureCredential` supports environment, workload
identity, and managed identity credentials. Set `AZURE_CLIENT_ID` to select a
user-assigned identity; there is no client ID flag. The identity must be attached
or configured and have Storage Blob Data Reader access. Workload identity needs
real pod labels, a service account, federation, and a projected token setup, not
just environment variables. `--azure-credential=anonymous` is for public upstream
blobs. SAS and shared key origin authentication are not supported.

Use an anonymous local Azure SDK client with service URL
`http://127.0.0.1:8080`, not an account URL or `/ACCOUNT` path. Requests use
`GET` or `HEAD /container/blob`, with optional `versionid`. Only block blobs are
supported, not snapshots or the special `$root`, `$web`, and `$logs` containers.
`GET` supports one byte range; `x-ms-range` takes precedence over `Range`.
`HEAD` ignores ranges. `If-Match` and `If-None-Match` are supported. Date, lease,
CPK, and optional checksum requests are rejected. Responses omit `Last-Modified`
and user metadata.

The origin provider and sidecar API are independent. The default `--api=s3` also
works with an Azure origin. Keep the same stable upstream namespace on origin and
sidecar. Schema 1 cache keys are unchanged, so **change the namespace when changing
account, provider, or endpoint**. Incoming authorization headers are ignored, not
verified or forwarded. Keep the sidecar on trusted loopback; cache access is not
per-client authorization.

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
