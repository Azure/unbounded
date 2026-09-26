# Full-image local kind regression

Base: `a010f19f9f83cdb59d204cd1a2a115d366f11efa`. Validation performed on
2026-09-26 in the retained `kind-racer-e2e-1790395612486835516` cluster.
All Kubernetes commands selected its explicit kubeconfig. No AKS mutations.

## Cause and fix

The original port-forward experiment's zero completed images did not identify a
cause. A finite in-cluster batch reproduced an unexpected EOF with just one image
in 94 ms, eliminating both port-forward and the whole-pull timeout as necessary
causes. At concurrency 64 the original retained dataplane completed 40 images
and failed 24, with both truncated bodies and HTTP 503 responses.

Temporary tracing localized the failures to local admission, before the original
request deadline:

- The origin connection pool rejects exhausted capacity immediately
  (`cmd/racer-dataplane/src/http/pool.rs:188-223`). OriginClient propagated that
  error from pinned page acquisition, so an already-started client body truncated.
- Bootstrap's downloaded plaintext could fail ciphertext admission, which failed
  the coalesced page flight. Waiting on connection capacity alone still failed
  all 64 images in one batch at bootstrap encryption.
- Fresh bootstrap could fail its initial plaintext reservation before reaching
  Fill's reclaiming, deadline-bounded page admission. A new-seed batch exposed
  this after warm batches had succeeded.

OriginClient now waits for local connection admission within the existing read
driver and request scope (`src/origin/client.rs:102-124`, relative to
`cmd/racer-dataplane`). When bootstrap plaintext is unavailable, it discovers
metadata with HEAD and lets the coordinator acquire the pinned page through Fill
(`src/origin/client.rs:152-171`, `src/read/metadata.rs:801-809`). An already-downloaded
bootstrap page waits for ciphertext under the same scope
(`src/read/fill.rs:320-341`). These waits preserve resource limits, version pins,
and original deadlines. There is no HTTP response retry or SDK retry.

All three new Rust regression tests failed when the old admission behavior was
restored, then passed with the fix. They cover release, cancellation, expiration,
closed pools, exact returned bytes, and quota release.

## Final finite-batch acceptance

The checked-in `e2e/racer/full-image-kind.py` runs the actual loadgen puller in a
pod through a ClusterIP Gantry endpoint, with an in-cluster origin. Each batch has
64 full-image attempts and four concurrent layers per image. Every success
requires the manifest, config, and all eight layers to pass size and SHA-256
verification. Counts below are final, after all workers join, not live snapshots.

| Dataplane / route | Seed | Full successes | Full errors | Canceled | Body bytes |
| --- | --- | ---: | ---: | ---: | ---: |
| Rebuilt base / Gantry | benchmark-v1 | 34 | 30 | 0 | 25,509,380,514 |
| Clean candidate / Gantry | benchmark-v1 | 64 | 0 | 0 | 34,748,963,840 |
| Clean candidate / Gantry, fresh image | full-image-acceptance-cold | 64 | 0 | 0 | 30,223,342,592 |
| Direct-origin control | benchmark-v1 | 64 | 0 | 0 | 34,748,963,840 |

The benchmark manifest is
`sha256:2aa1e8745512f4f76499b8b9a5543642630d3f1120668d8636278537d9aa53a6`:
eight 64 MiB-base payload layers with 20% jitter, 542,952,560 full-image bytes.
The fresh image has the same shape and seed-dependent sizes, 472,239,728 bytes,
manifest `sha256:29d252394c0e7d21d0043c6352f04874c6cf3c34ac5d6c33707cb8966284399b`.
Each successful batch verified 512 complete layers and ended with zero in-flight
images. Failed-batch byte counts include partial objects.

Final harness evidence under this worktree's `tmp/`:

- Base: `racer-full-image-i3h6ntp7/probe.log`.
- Candidate benchmark: `racer-full-image-ew5bi2yd/probe.log`.
- Candidate fresh: `racer-full-image-ym4x1zii/probe.log`.
- Direct origin: `racer-full-image-tx67yd3h/probe.log`.
- Each directory also retains arguments, pod image identities, and component logs.
- Earlier diagnostic logs and candidate isolation experiments remain in `tmp/`.

Only the `racer-dataplane` production image needs rebuilding. The clean candidate
local OCI image index is
`sha256:61a389c1f92c0cce7c03c5be9551c0bb216de4f376bc2b00df6cea93f6e9f820`.
Gantry, controller, operator, and production loadgen images were not changed.
The regression probe is test-only. The retained cluster was left on the clean
candidate; temporary Gantry origin configuration and probe resources were cleaned
up after each final harness run.

## Checks

- Rust all-feature unit/binary/integration tests: 584 passed, nine ignored.
- Rust doc tests and `cargo fmt --all --check` passed.
- Normal all-target/all-feature Clippy completed with warnings. The stricter
  `-D warnings` invocation failed on existing lint patterns throughout the package;
  its output is retained in `tmp/rust-clippy.log`.
- Scoped `make fmt`, `make lint`, actionlint, and loadgen Go tests passed using the
  repository-local golangci-lint 2.13.1 compatible with the installed Go 1.27.1.
  Older installed linter builds failed before the compatible tool was selected.
- Each script has an outer deadline; the probe has a 115-second test timeout and
  a 120-second pod deadline. Builds are capped at 600 seconds; other subprocesses
  are capped at 120 seconds or less. No existing tests were removed.
