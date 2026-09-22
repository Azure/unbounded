# Racer correctness scripts

Run from the repository root against a current production `racer-dataplane`
binary. Prerequisites are Go (for the loopback control fixture), Python 3.8+,
the Rust/native build prerequisites in
[the dataplane README](../../../cmd/racer-dataplane/README.md), Linux io_uring,
workspace-local ext4 scratch space, sufficient inherited locked memory, and
distinct physical I/O and compute cores. Apply external memory/runtime limits.
Each output directory must be new. These commands do not change host settings.

## Commands

```sh
export TMPDIR="$PWD/tmp"
test -d "$TMPDIR"
timeout --kill-after=5s 180s cargo build --manifest-path cmd/racer-dataplane/Cargo.toml --release --locked --bin racer-dataplane -j 2
PYTHONPYCACHEPREFIX="$TMPDIR/__pycache__" python3 -m py_compile e2e/racer/scripts/probe.py
ruff check --no-cache e2e/racer/scripts
PYTHONDONTWRITEBYTECODE=1 timeout 30s python3 -m unittest discover -s e2e/racer/scripts -v
timeout --kill-after=5s 120s go test ./hack/release ./e2e/racer/scripts/controlfixture -count=1

timeout --kill-after=15s 90s python3 e2e/racer/scripts/probe.py idle-close --binary cmd/racer-dataplane/target/release/racer-dataplane --output tmp/mtls-idle-close
timeout --kill-after=15s 150s python3 e2e/racer/scripts/probe.py fanout --binary cmd/racer-dataplane/target/release/racer-dataplane --output tmp/mtls-fanout
timeout --kill-after=15s 90s python3 e2e/racer/scripts/probe.py hot-cache --binary cmd/racer-dataplane/target/release/racer-dataplane --output tmp/mtls-hot-cache
timeout --kill-after=15s 90s python3 e2e/racer/scripts/probe.py churn --binary cmd/racer-dataplane/target/release/racer-dataplane --output tmp/mtls-churn
```

The script builds its Go fixture into the output directory. `--build-timeout`
(default 600 seconds) bounds each Go/Cargo build; the external deadline bounds
the complete scenario, including builds. SIGTERM unwinds daemon and fixture
cleanup. SIGKILL cannot run Python cleanup. Single-daemon probes bind peer TLS
on `127.0.0.1:9443`, so run them sequentially. Use `--cpus` for optional taskset
placement and `--peer-cpus` for the second physical-owner daemon.

## Enrollment and mTLS

Every run creates a private `credentials/` directory and starts the
[`controlfixture`](controlfixture/main.go) server on an ephemeral loopback port.
The fixture creates a random, in-memory CA and control-plane key. It writes the
public `bundle.json` trust projection (`version`, `generation`, `active` root
DER SHA-256, PEM `certificates`) and generates a separate random enrollment token
for each registered daemon. Credential files are mode 0600, directories 0700;
the CA/server private keys are never written to disk.

The production daemon generates its own leaf key and CSR, authenticates the
fixture's control-plane DNS and URI SANs, and enrolls through HTTPS `/v3/enroll`.
The fixture checks the token, Pod registration, boot ID, CSR signature, and
universe/node/Pod URI before issuing a short-lived leaf. Control requests use
that client certificate over mTLS. The fixture decodes the shared Go ProtoJSON
schema and sends protobuf `ControlCommand` messages with the exact snapshot,
digest, revision, boot incarnation, and Pod identity. Prepare/receive/activate/
retire phases advance from subscriber feedback; a new digest starts at prepare.
Explicit Content-Length supports the large physical-owner exports.

The captured environment sets `RACER_TLS_TRUST_DIR`, `RACER_ENROLL_URL`,
`RACER_CONTROL_SERVER_NAME=localhost`, `RACER_CONTROL_TOKEN_FILE`,
`RACER_POD_NAMESPACE`, `RACER_POD_NAME`, `RACER_POD_UID`,
`RACER_TRUST_PROOF_URL`, and an HTTPS `RACER_CONTROL_PLANE_URL`. Inherited
`RACER_*` settings are removed. No signing bundles or pre-issued daemon keys
are used. Each daemon must report an activated configuration and generation-1
TLS credentials installed by all workers before requests start.

This is a loopback test authority. Its token/registration check substitutes for
Kubernetes TokenReview and Pod selection. The proof endpoint authenticates the
client but does not implement durable CA rotation/retirement. Those behaviors
belong to the control-plane PKI/rotation suites. Volume ingress and origin
traffic remain HTTP; physical-owner forwarding uses the dedicated mTLS peer
listeners. Local scenarios explicitly authorize no peers with
`peerEndpoints: {}`.

## Physical-owner Go exports

Export production fixtures from the root Go module into an empty directory:

```sh
mkdir tmp/mtls-snapshots
TMPDIR="$PWD/tmp" B02_EXPORT="$PWD/tmp/mtls-snapshots" timeout --kill-after=5s 240s go test ./cmd/racer-controlplane -run '^TestB02ProductionSnapshots$' -count=1
TMPDIR="$PWD/tmp" timeout --kill-after=15s 720s python3 e2e/racer/scripts/probe.py physical-owner --binary cmd/racer-dataplane/target/release/racer-dataplane --output tmp/mtls-physical-owner --snapshots tmp/mtls-snapshots --jobs 2
TMPDIR="$PWD/tmp" timeout --kill-after=15s 720s python3 e2e/racer/scripts/probe.py physical-owner --layout blocks --binary cmd/racer-dataplane/target/release/racer-dataplane --output tmp/mtls-physical-blocks --snapshots tmp/mtls-snapshots --jobs 2
```

The exporter supplies `manifest.json`, `a.json`, `b.json`, `files.json`, and the
complete snapshot/owner corpus. Interleaved mode uses `a.json` and `b.json`
(131072 slots), and runs Rust's `b02_go_placement_conformance` against the full
corpus. Blocks mode selects `p8-n2-historical-0.json` and
`p8-n2-historical-1.json` (A owns 0..3, B owns 4..7) from that same export and
runs `b03_go_snapshot_consistency`. The script runs Cargo from
`cmd/racer-dataplane` with `B02_EXPORT` and `B03_GO_SNAPSHOT` set.

Snapshots are consumed unchanged. Pod identities and management bind IPs come
from the Go manifest. This places the daemon's dedicated peer listeners on the
exact `127.0.0.2:9443` and `127.0.0.3:9443` addresses in Go's peer endpoints.
Keep those addresses, volume ingress `127.0.0.2:18881` and `127.0.0.3:18881`,
origin `127.0.0.1:18880`, and management `127.0.0.2:18890` and
`127.0.0.3:18891` free. Physical-owner probes must run sequentially.

Stopped-owner coverage waits 21 seconds after stopping A to exceed the peer
pool's 20-second reuse limit (`cmd/racer-dataplane/src/http_client.rs:394-401`).
It asserts fallback from fresh connection-refusal evidence. During migration,
the immediate request on a reused TLS connection returned 502; the TLS send/read
paths propagate errors without the plain-HTTP idle reconnect path
(`cmd/racer-dataplane/src/http_client.rs:1730-1747,1847-1861`). The wait does not
establish recovery from abrupt EOF on reused TLS sessions. Backend FIN/RST replay
remains covered separately by `idle-close`.

## Assertions and artifacts

| Probe | Required observations |
| --- | --- |
| `idle-close` | FIN/RST for HEAD and GET; exact 64 KiB bodies, fresh sockets and healthy reuse, 12 metadata plus 4 page attempts |
| `fanout` | 255 distinct idle origin sockets; cold progress after 0/1/5/31 seconds, warm reuse, 320 retained-generation sockets, retirement to 64 |
| `churn` | 2000 HEADs plus warmup on one upstream connection, exactly 2001 origin hits |
| `hot-cache` | Two origin requests total across 8000 warm HEAD/GET requests; exact checksum/bodies and metadata/page hit/miss counters |
| `physical-owner` | Go/Rust consistency, live-owner mTLS forwarding, local-owner and semantic controls (interleaved), stopped-owner fresh-connect fallback, exact bytes and peer/backend attempt attribution |

Successful probes assert clean daemon shutdown and origin cleanup; physical-owner
also checks for leaked ingress, peer, and management listeners. Artifacts include
`results.json` (commands, environment paths, binary hash, TLS readiness), daemon
logs, metrics, generated configuration, public trust projection, private fixture
registration/token files, fixture binary/build log/server log, and slabs. Token
contents are not logged. Failed runs retain partial artifacts. Physical-owner
also captures `rust-consistency.log`. There is no tracing or profiling mode.
