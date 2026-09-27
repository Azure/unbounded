# Executable restart integration tests

`process_restart.rs` executes Cargo's actual `racer-dataplane` binary, including
environment parsing, CPU discovery, TLS enrollment, projected key loading,
application startup, readiness, UDS listeners, encrypted O_DIRECT storage, signal
handling, and shutdown. It supplies a TLS controller fixture and a raw HTTP/UDS
origin adapter. Client requests use the production UDS wire protocol.

Run from the repository root on Linux with io_uring, O_DIRECT/STATX_DIOALIGN, at
least one usable CPU, and permission to create a private mount namespace. The
tests are explicitly ignored in ordinary unprivileged Cargo runs; selecting them
fails visibly if prerequisites are missing. The `Racer Rust Suite` CI job selects
this suite explicitly in a privileged restart step after the complete Rust suite.
On x86-64, run the same bounded system service locally. Cargo builds as your normal
user and its runner executes just this suite as root:

```sh
mkdir -p tmp
sudo systemd-run --wait --pipe --collect \
  --property="User=$(id -un)" --property="WorkingDirectory=$PWD" \
  --property=MemoryMax=12G --property=MemorySwapMax=0 \
  --property=RuntimeMaxSec=5min --property=TimeoutStopSec=30s \
  --setenv="PATH=$PATH" --setenv="HOME=$HOME" --setenv="TMPDIR=$PWD/tmp" \
  --setenv="CARGO_TARGET_DIR=${CARGO_TARGET_DIR:-$PWD/bin/racer-cargo}" \
  --setenv='CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_RUNNER=sudo -n' \
  bash "$PWD/hack/scripts/memory-safe-run.sh" -- \
  cargo test --locked --manifest-path cmd/racer-dataplane/Cargo.toml \
  --all-features --test process_restart -j 2 -- \
  --ignored --test-threads=1 --nocapture
```

The command verifies a 12 GiB memory limit and no swap before launching Cargo.
It reuses the CI job's `bin/racer-cargo` artifacts; export `CARGO_TARGET_DIR` to
reuse a different worktree-local build directory. On other architectures, use the
corresponding Cargo target runner variable. Only `process_restart` is selected;
other ignored suites are not enabled.

Each process has one I/O/crypto worker pair, a 256 MiB sparse slab, 64 MiB
plaintext, 160 MiB ciphertext (including write staging), and 64 MiB dirty budgets.
Tests run serially and transfer a 32 MiB-plus-113-byte object. All temporary files,
keys, logs, and sockets are under this crate's `target/restart-*` directories and
are removed by fixture guards. A child-only private mount maps its scratch runtime
directory over `/run`; the host's `/run` is untouched. Identity and slabs persist
between incarnations, while runtime socket directories are fresh by fixture
design. The deployed DaemonSet instead uses a hostPath for `/run/racer`
(`internal/racer/workload_controller.go:167,176-177`), so persistent stale-socket
restart recovery is not covered here. This matters after SIGKILL because startup
deliberately rejects existing sockets it does not own
(`src/client/transition.rs:176-177`). This suite proves persisted identity/slab
recovery once fresh listener paths are available, not unattended recovery from a
crash with stale hostPath sockets.

## Assertions

- **Graceful SIGTERM:** bootstrap page zero, fetch a pinned remainder spanning
  another full page and a short tail, verify every byte, and require successful
  process exit and client socket cleanup. Decode the resulting checkpoint and
  require all three page mappings under the projected page key. Launch a new PID
  with the same identity/slabs, reject origin requests, and verify the complete
  pinned object, exactly three disk hits, and zero origin calls/fills. The
  persisted certificate is freshly issued after token reauthentication; its
  verified cluster and Node UID remain the same. Each process enrolls twice,
  during pre-worker bootstrap and control-worker startup, so the two incarnations
  require four enrollments. Readiness requires completed pending identity cleanup.
- **Interrupted SIGKILL:** deliver and verify page zero, wait for real slab
  allocation, then pause the origin after sending the first chunk of another
  page. Kill and reap the process without graceful shutdown. Require no published
  checkpoint, launch a fresh PID with the same identity/slabs, and verify every
  byte of the pinned object. Require safe misses and the exact three pinned origin
  page refetches. A subsequent cross-page read succeeds with origin disabled.
  The same fresh-issuance, authenticated binding, and enrollment-count checks
  apply after SIGKILL; a valid persisted certificate never bypasses startup
  token authentication.

Readiness, client I/O, origin I/O, and process-exit waits are bounded. Process
guards kill and reap children on failure and print their captured logs. This is
process-loss coverage, not power-loss durability: checkpoint publication does not
fsync files or directories, and uncheckpointed slabs are not scanned
(`designs/racer-store-protocol.md:86-93`).
