# Opt-in integrity diagnostics

`--diagnose-integrity` requires `--verify=true`. It compares each received HTTP
200 object against the deterministic catalog while retaining normal size/status
classification. Only exact-size digest failures emit page evidence. Offsets are
relative to the complete object, including tar metadata. Pages are 16 MiB; at
most eight mismatching page hash pairs are retained. Comparison continues after
the cap and reports the omitted count. No payload is retained or logged.

The default reader is unchanged. Diagnostic reads additionally regenerate bytes,
compare them, and calculate two page SHA-256 streams. Diagnostic throughput is not
a saturated-path baseline. Evidence localizes bytes, not the component responsible.

## Finite standalone runner

Compile the same-package test binary (choose an existing output directory):

```sh
timeout --signal=TERM --kill-after=10s 300s env GOTOOLCHAIN=go1.26.6 \
  go test -c -o ./tmp/racer-diagnostic.test ./cmd/racer-loadgen
```

Set `RACER_DIAGNOSTIC_CONFIG` to an explicit JSON configuration file. Example only:
replace content parameters and target with the deployed catalog and authorized
node-local Gantry endpoint. HTTP GETs can naturally fill or refresh cache state.

```json
{
  "Target": "http://127.0.0.1:5000",
  "Namespace": "loadgen.invalid",
  "Image": {
    "Repository": "benchmark/image",
    "Seed": "benchmark-v1",
    "Layers": 8,
    "LayerBytes": 67108864,
    "Jitter": 0.2
  },
  "ImageIndices": [0, 7],
  "Iterations": 2,
  "LayerConcurrency": 1,
  "StartupTimeout": "60s",
  "PullTimeout": "60s",
  "TotalTimeout": "240s"
}
```

Run only the explicitly selected test, with a clean diagnostic environment:

```sh
timeout --signal=TERM --kill-after=10s 300s \
  env RACER_DIAGNOSTIC_CONFIG=./tmp/diagnostic.json \
  ./tmp/racer-diagnostic.test -test.run='^TestRacerDiagnosticTarget$' \
  -test.timeout=5m -test.v
```

The runner opens no listeners and starts no origin or dataplane. It generates
only selected image indices, cycles through them in order with one image pull
active, and exits after the configured number of attempts or the first failure.
It does not access concurrency control files. Deadline or cancellation is failure,
not successful completion. SIGTERM cancels requests and closes idle connections.
Without the environment variable the test skips and makes no network requests.
Failures use sanitized production evidence, not raw URL-bearing error strings.
The parent operator retains responsibility for global pause/drain and recovery.
