# Racer end-to-end tests

## Native GantryRacer integration

Run from the repository root on a capable Linux host:

```sh
make e2e-gantry-racer-build
make e2e-gantry-racer
```

The build target builds `bin/gantry`, builds the real Rust `racer-dataplane`
with `cargo build --locked` (debug profile), and warms the Go e2e build cache.
The test target uses those binaries and runs `TestGantryRacerStriped`,
`TestGantryRacerAuthorization`, `TestGantryRacerRecovery`, and
`TestGantryRacerCorruption` separately, each with an external 60-second hard
deadline and `go test -timeout 50s -count 1 -v`. Builds are outside those
deadlines. Rerun the build target after changing either binary or test sources.

### Prerequisites

- Go matching `go.mod`, Rust 1.96.0, a C toolchain, `ar`, Make, Perl, and
  libibverbs development headers. On Ubuntu the native build dependencies are
  `build-essential perl libibverbs-dev`. OpenSSL and protoc are vendored.
- Linux with usable io_uring, at least two allowed physical cores on the first
  NUMA node, and enough RAM and locked memory for three real dataplanes plus
  Gantry, containerd, and the test payload. Each fixture dataplane uses a
  512 MiB slab and four registered buffers. An unlimited memlock limit avoids
  the usual small shell limit; where authorized, raise it for the current shell
  with `sudo -n prlimit --pid $$ --memlock=unlimited:unlimited`.
- `containerd`, `ip` (iproute2), `unshare`, `mount`, `findmnt` (util-linux),
  GNU `timeout`, and passwordless `sudo -n`. The privileged subprocess must be
  allowed to create mount, network, and PID namespaces, create device nodes,
  and bind-mount its private `/dev`. It brings up only namespace-local loopback
  and starts a private containerd content store.
- Workspace-local ext4 scratch space. The default is
  `$PWD/tmp/gantry-racer`; an existing `TMPDIR` or `GANTRY_RACER_TMPDIR` override
  must be an absolute directory inside this checkout, on ext4. All fixture
  backing files live there.

Missing prerequisites fail the target. The suite starts the actual Rust daemon
and exercises io_uring; it has no environmental skip or simulated fallback.

To use already-built artifacts explicitly, including a release-profile daemon:

```sh
TMPDIR="$PWD/tmp/gantry-racer" \
RACER_DATAPLANE_BINARY="$PWD/bin/racer-dataplane" \
GANTRY_BINARY="$PWD/bin/gantry" \
make e2e-gantry-racer
```

Both binary overrides must be absolute executable paths. On a kTLS-capable host
(Linux >= 6.14 and an eligible OpenSSL build), require real peer-page sendfile:

```sh
RACER_REQUIRE_KTLS=1 make e2e-gantry-racer
```

`Striped` fails if it does not observe at least one page of kTLS sendfile bytes
when this flag is set. Without it, the test still requires real peer transfers
and Gantry splice/tee verification, and logs the observed sendfile byte count.

### CI

The `Racer Rust and Cross-language Tests` job in `.github/workflows/ci.yaml`
runs both targets on its existing Ubuntu 24.04 native Racer runner. It reuses
the locked Cargo debug cache, raises memlock, uses workspace ext4 scratch, and
requires the runner's containerd and namespace/mount capabilities. It runs on
every configured CI event, so Gantry core, Racer SDK, Rust, API, and
`e2e/racer` changes are covered without a path filter. The separate
`gantry-e2e.yaml` kind workflow includes those paths as well. CI does not assume
kTLS support from the Ubuntu runner label; the verbose Striped result records
the observed offload coverage.
