# Racer correctness scripts

Run these scripts from the repository root against a current
`cmd/racer-dataplane/target/release/` or `bin/` binary. They exercise the real
kernel and production daemon. Use workspace-local ext4 scratch space, sufficient
inherited locked memory, distinct physical I/O and compute cores, and external
memory/runtime limits. Every probe output directory must be new.

`preflight.py` uses an existing Docker image (`ubuntu:noble` by default;
override with `RACER_PREFLIGHT_IMAGE`) with `--pull=never`. Its normal container
uses `seccomp=unconfined`, matching the operator in
`internal/operator/components/racer/resources.go`. It checks production pool/ring
startup, inherited and raised memlock, SYS_RESOURCE, CPU quota, memory limits,
ext4, physical cores, and required syscalls under Docker's default seccomp policy.
It asserts that the preflight leaves no slab or scratch file.

## Commands

These commands succeeded during migration on September 21, 2026:

```sh
PYTHONPYCACHEPREFIX="$PWD/tmp/__pycache__" python3 -m py_compile e2e/racer/scripts/preflight.py e2e/racer/scripts/probe.py
ruff check --no-cache e2e/racer/scripts
timeout 240s python3 e2e/racer/scripts/preflight.py cmd/racer-dataplane/target/release/racer-preflight tmp
timeout 90s python3 e2e/racer/scripts/probe.py idle-close --binary cmd/racer-dataplane/target/release/racer-dataplane --output tmp/phase3-scripts-idle-close
timeout 150s python3 e2e/racer/scripts/probe.py fanout --binary cmd/racer-dataplane/target/release/racer-dataplane --output tmp/phase3-scripts-fanout
timeout 90s python3 e2e/racer/scripts/probe.py hot-cache --binary cmd/racer-dataplane/target/release/racer-dataplane --output tmp/phase3-scripts-hot-cache
timeout 90s python3 e2e/racer/scripts/probe.py churn --binary cmd/racer-dataplane/target/release/racer-dataplane --output tmp/phase3-scripts-churn
```

The tested daemon SHA-256 was
`10fc50d2bfc4be47c80694a6eb0829bd21b3158bf295a17bc75b367a1bf6e08b`.
All ten preflight cases passed. An initial invocation with `timeout 180s`
encountered the script's 20-second Docker subprocess timeout during the CPU quota
case; the complete retry above passed without script changes. The host already
provided sufficient memlock (`ulimit -l` reported `12356396` KiB).

## Physical-owner Go exports

The shared schema is `api/racer/control.proto`. Export production fixtures using
the root Go module, into an existing empty directory:

```sh
mkdir tmp/phase3-scripts-snapshots
TMPDIR="$PWD/tmp" B02_EXPORT="$PWD/tmp/phase3-scripts-snapshots" timeout 240s go test ./cmd/racer-controlplane -run '^TestB02ProductionSnapshots$' -count=1
TMPDIR="$PWD/tmp" timeout 720s python3 e2e/racer/scripts/probe.py physical-owner --binary cmd/racer-dataplane/target/release/racer-dataplane --output tmp/phase3-scripts-physical-owner-v2 --snapshots tmp/phase3-scripts-snapshots --jobs 2
```

The Go export command passed. An initial `timeout 180s` invocation failed with
`undefined: _test.TestProvisionedNodeAdmission` during concurrent controller test
edits; retrying the command above succeeded.

**The full interleaved physical-owner probe passed.** Rust conformance validated
all 66 exported snapshots, then the two production daemons passed all eight
HEAD/GET requests across live-owner, local-owner, semantic-404, and stopped-owner
fallback phases. Exact response bytes, origin hits, peer/backend attempts,
zero exit codes, origin shutdown, and absence of leaked listeners were checked.
Artifacts are in `tmp/phase3-scripts-physical-owner-v2/`, including
`rust-consistency.log`, `results.json`, daemon logs, and metrics.

The initial run, retained in `tmp/phase3-scripts-physical-owner/`, failed before
daemon startup because the conformance test asserted algorithm 1 support.
The approved contract accepts only algorithm 2
(`cmd/racer-dataplane/src/control.rs:1806-1808`). The test was corrected to check
default/explicit algorithm 2 equivalence and algorithm 1 rejection at both the
routing and configuration preparation boundaries. A non-ignored regression also
checks rejection of 0, 1, 3, and `u32::MAX`. No tests were removed.

The following commands succeeded from `cmd/racer-dataplane/`:

```sh
rustfmt --edition 2024 tests/control/topology.rs
rustfmt --edition 2024 --check tests/control/topology.rs
cargo fmt --check
TMPDIR="$PWD/../../tmp" timeout 180s cargo test --release --locked --lib -j 2 topology:: -- --test-threads=2
TMPDIR="$PWD/../../tmp" B03_GO_SNAPSHOT="$PWD/../../tmp/phase3-scripts-snapshots/p8-n2-historical-1.json" timeout 60s cargo test --release --locked --lib -j 2 topology::physical_owner_proof::b03_go_snapshot_consistency -- --exact --ignored --nocapture --test-threads=1
```

The focused suite reported 9 passed and 2 fixture-dependent tests ignored. Both
ignored tests were then exercised successfully: B02 by the full probe above
(1 passed), and B03 by the historical Go export command (1 passed). No further
defects or blockers were observed in these checks.

The exporter supplies `manifest.json`, `a.json`, `b.json`, `files.json`, and the
complete snapshot/owner corpus. All snapshots are consumed unchanged. The Go
producer already emits required `peerEndpoints` scopes at
`cmd/racer-controlplane/topology.go:530-535`; no producer adaptation was needed.
The script runs Cargo from `cmd/racer-dataplane` and sets both `B02_EXPORT` and
`B03_GO_SNAPSHOT` for its child conformance test.

The default layout is interleaved (131072 slots). `--layout blocks` retains the
historical eight-slot scenario for an existing Go-produced block-layout export
(A owns 0..3, B owns 4..7). Do not pass the current interleaved convenience pair
as blocks. Keep origin `127.0.0.1:18880`, peer endpoints `127.0.0.2:18881` and
`127.0.0.3:18881`, and management ports `127.0.0.1:18890-18891` free.

## Signing and metadata

Each run creates a private `peer-keys/bundle.json` from the public RFC 8032 test
vector and supplies `RACER_PEER_KEYS_DIR`. The bundle has version 1, generation
1, an active public key, matching seed, and public verification keys. Every
generated volume explicitly includes `peerEndpoints: {}` to authorize no peers.

These probes consume watched local ProtoJSON snapshots, which production allows
to be unsigned; the peer bundle is still mandatory. They sanitize inherited
`RACER_*` variables before setting the captured daemon environment. No manual
signing exports are needed. HTTP control consumers elsewhere require a separate
`RACER_CONFIG_KEYS_DIR/bundle.json` with `version`, `generation`, `active`, and
`public`, without a seed, signed control data, and a Pod token supplied through
`RACER_CONTROL_TOKEN_FILE` (`cmd/racer-dataplane/src/control.rs:945-967`). The old
raw signing-key environment variables are unsupported.

Kubernetes fixtures use the centralized `racer.unbounded-cloud.io/` metadata
prefix (`internal/racer/metadata.go`). These loopback scripts do not generate
Kubernetes metadata. Cryptographic protocol domains are independent of that
prefix.

## Assertions and captured artifacts

| Probe | Required observations |
| --- | --- |
| `idle-close` | FIN/RST for HEAD and GET; exact 64 KiB bodies, fresh sockets and healthy reuse, 12 metadata plus 4 page attempts |
| `fanout` | 255 distinct idle origin sockets; cold progress after 0/1/5/31 seconds, warm reuse, 320 retained-generation sockets, retirement to 64 |
| `churn` | 2000 HEADs plus warmup on one upstream connection, exactly 2001 origin hits |
| `hot-cache` | Two origin requests total across 8000 warm HEAD/GET requests; exact checksum/bodies and metadata/page hit/miss counters |
| `physical-owner` | Go/Rust consistency, live-owner forwarding, local-owner and semantic controls, stopped-owner fallback, exact bytes and peer/backend attempt attribution |

Successful probes assert clean daemon shutdown and origin cleanup. Artifacts
include `results.json` with commands, daemon environment and binary hash,
daemon logs, metrics, generated configuration, signing bundle, and slab. Failures
retain partial artifacts; physical-owner also captures `rust-consistency.log`.
There is no tracing or profiling mode.
