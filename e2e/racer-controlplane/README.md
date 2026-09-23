# Rust control-plane production-binary campaign

Run the explicit `e2e`-tagged campaign from the repository root:

```sh
export RACER_CONTROLPLANE_BINARY="$PWD/cmd/racer-controlplane/target/debug/racer-controlplane"
export RACER_DATAPLANE_BINARY="$PWD/bin/racer-dataplane"
export KUBEBUILDER_ASSETS=/home/azureuser/code/unbounded/bin/envtest/k8s/1.37.0-linux-amd64
export TMPDIR="$PWD/tmp/live"
export RACER_LIVE_SOCKET_ROOT=/home/azureuser/code/unbounded/tmp/s
bash hack/scripts/racer-controlplane-live.sh
```

Create the two scratch directories first. The socket root must be an existing
workspace directory whose absolute path is at most 36 bytes: production validates
that even a maximum-length cache name fits a Unix socket address. Build the current
Rust CP with `cargo build --locked --manifest-path cmd/racer-controlplane/Cargo.toml`.
The daemon must support the v4 desired-state protocol.

## Prerequisites

- Linux with usable io_uring, sufficient locked memory and physical CPU cores,
  and workspace-local ext4 scratch space for real slab files.
- Passwordless `sudo`, `unshare`, `mount`, `ip`, and `setpriv`. The test reexecutes
  in a private network namespace, then gives each daemon a private bind mount of
  its socket directory. Test code, API processes and Racer processes run as the
  invoking UID after namespace setup. Host routes and mounts are not changed.
- Explicit absolute executable paths and real envtest assets. Missing settings
  skip locally but fail when `RACER_REQUIRE_LIVE=1` or `CI` is set. Set
  `RACER_LIVE_API_LOG=1` for API startup diagnostics.

Every run preserves CP/DP logs, binary SHA-256 identities, and timestamped
public TLS/status/metrics observations in `$TMPDIR/racer-live-artifacts-*`.
Credentials remain in automatically cleaned test directories. The runner saves
complete output to `$TMPDIR/racer-live-run-*.log`. To inspect TLS transitions and
error counter deltas:

```sh
python3 hack/scripts/racer-live-observations.py \
  "$TMPDIR/racer-live-artifacts-<run>/observations.jsonl"
```

The harness enables `RACER_HTTP_DIAGNOSTICS=1` on dataplanes to log typed
transport failures. See `designs/racer-live-failure.md` for the resolved
peer-connection retirement regression and diagnostic context.

## Exercised contract

Two Rust control-plane subprocesses compete for the actual API Lease. Three
production dataplanes enroll using API-issued Pod-bound service-account tokens.
The installed, generated CRDs enforce API validation. Node and Pod statuses and
workload ownership are seeded because envtest has no kubelet or workload
controllers. A replacement CP Pod is created only after the production CP deletes
its old-boot Pod. A byte-preserving TCP relay supplies Service routing; TLS ends
in the real CP. Public trust ConfigMap bytes are atomically projected to daemon
files, as a kubelet would project them.

Assertions cover bootstrap identities, rejection of certificate-free control
requests, independently converged participants while one selected daemon is
absent, grow/shrink and shard-count changes, rejected runtime capacity, invalid
last-good policy, equivalent quantities, storage/topology independence, real watch
publication, stale resourceVersion conflicts, crash failover and durable content
retention, CP boot replacement, and verified SDK reads with encrypted peer
exchanges across overlap, issuer switch, and old-root retirement.
After retirement, fresh SDK reads are verified and a dataplane is restarted with
a new Pod UID, the retained slab inode, and an invalid creation-size environment;
the persisted policy must be reacknowledged without recreating storage.

The CLI uses explicit `--leaf-lifetime=120s --clock-skew=1s`. Retirement still
requires actual TLS proofs and persisted expiry watermarks. No private CA state,
proof records, configuration responses, or clocks are supplied by the harness.
Any SDK traffic error fails the campaign; after a traffic failure it continues
observing retirement so the report distinguishes traffic continuity from CA
progress. Failure output includes process logs and public/local TLS status,
without dumping private CA keys or service-account tokens.

## Measured run: September 23, 2026

The final verified run passed with **3,518 continuous verified SDK reads** across
CA generations 1 through 4, full old-root retirement, CP crash failover, independent
convergence, and storage resize/restart. Its log is
`tmp/live/racer-live-run-7xgD7B.log`; the campaign passed in 246.42 seconds.
The dataplane peer keepalive retirement fix resolved the earlier traffic failures.

This run establishes the campaign's correctness assertions. It does not establish
10,000-node first-byte tail compliance, hardware throughput, or RDMA performance.
The retirement audit records those boundaries. Use `GOTOOLCHAIN=go1.26.6` for the
repository's Go formatting/lint checks.
