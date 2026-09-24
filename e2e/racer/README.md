# Racer end-to-end tests

## Native GantryRacer integration

Run from the repository root on a capable Linux host:

```sh
make e2e-gantry-racer-build
make e2e-gantry-racer
```

The build target builds `bin/gantry` and `bin/racer-loadgen`, builds the real
Rust `racer-dataplane` with `cargo build --locked` (debug profile), and warms the Go e2e build cache.
The test target uses those binaries and runs six suites: `TestGantryRacerStriped`,
`TestGantryRacerAuthorization`, `TestGantryRacerRecovery`,
`TestGantryRacerCorruption`, `TestGantryRacerContainerImage`, and
`TestGantryRacerGeneration` separately, each
with an external 60-second hard
deadline and `go test -timeout 50s -count 1 -v`. Builds are outside those
deadlines. Rerun the build target after changing binary or test sources.

`ContainerImage` starts separate loadgen registry and puller processes against
one real Gantry/Racer pair. It verifies manifest, config, and two 64 MiB layers,
exact cold origin range counts, repeated pulls with the registry offline,
completed splice streams, zero tee counters, and zero Gantry fallback. Payload
verification belongs to the downstream reader; Gantry's `completed` metric does
not claim OCI digest verification. `Striped` independently verifies
multi-node page distribution and peer transport. See the
[loadgen guide](../../cmd/racer-loadgen/README.md#container-image-mode) and
[cluster example](examples/container-image-loadgen.yaml) for benchmarks.

### Explicit generation recovery fixture

`TestGantryRacerGeneration` exercises three real dataplanes: warm corrupt origin
bytes, repair the origin at the same digest URL, prove warm old-generation reads
still return the old bytes, increment the cache generation, await activation on
every dataplane, then verify repaired bytes and offline warm reuse with zero
Gantry fallback. It complements `TestGantryRacerCorruption`, which exercises two
peers and containerd's normal `Client.Pull` path against a real content store.

`Corruption` serves incorrect same-length layer bytes under the correct advertised
OCI digest. Racer computes a self-consistent admission CRC for those bytes, and
Gantry completes HTTP forwarding through both nodes with positive splice and zero
tee counters. Containerd must reject specifically with an unexpected commit
digest (`FailedPrecondition`), with neither the expected nor incorrect digest
committed and no image published. Repairing the origin alone leaves warmed bad
bytes intact. After explicit generation activation on every dataplane, the test
demonstrates the retained-ingest retry behavior below, aborts only that idle failed
ingest, and repeats the same normal pull in the same namespace. It checks the
committed bytes and then reads through Racer on both peers with origins offline.
The valid image config is fetched through containerd's resolver before the pulls,
so layer rejection cannot cancel a sibling config fetch and introduce an unrelated
fallback. Manifest and layer fetching use the normal pull path.

The native `gantryFixture` helpers for follow-on tests are:

```go
// Publish owned bytes atomically at an existing path, retaining the digest URL.
err := f.setOriginObject(path, gantryObject{
    data: goodBytes, mediaType: "application/octet-stream", corrupt: false,
})
// Check err before continuing. In-flight origin requests retain their old bytes.
before := f.originRequestCount("GET", path)

// Increment each named existing volume once across all nodes, then block until
// every real dataplane has activated the resulting configuration revision.
revision := f.bumpCacheGeneration("gantry")
```

For tests that need to separate publication from activation, use
`revision, err := f.advanceCacheGeneration("gantry")`, check the error, then call
`f.awaitCacheRevision(revision)`. The returned value is a **configuration
revision**, not a cache generation. Multiple volume IDs are supported; invalid
selections fail without publishing any node's update. Wait for each bump before
issuing another. Publication wakes authenticated control long polls and assigns
new snapshot digests/cursors without mutating previously offered snapshots.

Activation checks each dataplane's `/status`: exact `activeRevision` and
`candidateRevision`, `localState: applied`, all workers activated, `ready: true`,
and no rejection. A desired-state offer, cursor, or `/readyz` alone is not an
activation barrier. `waitCacheRevision(ctx, revision) error` provides a
caller-controlled deadline; the assertion wrapper has a ten-second deadline
and reports the last failing node/status.

`originRequestCount(method, path)` counts attempts across all fixture origins,
including offline failures. An empty method or path is a wildcard. Use GET counts
for payload refetch assertions, keeping HEAD authorization/metadata traffic
separate. Origin replacement and count inspection are safe during concurrent
requests; replacement copies the caller's byte slice.

In a real cluster, the equivalent operator action is an explicit patch of the
existing P2PCache, for example when its current generation is 1:

```sh
kubectl patch p2pcache gantry --type=json -p='[{"op":"test","path":"/spec/cacheGeneration","value":1},{"op":"replace","path":"/spec/cacheGeneration","value":2}]'
```

Use the actual current and monotonically increasing next values. If the JSON
Patch test fails, reread rather than overwriting a concurrent update. Repair the
origin before advancing the generation. Wait for P2PCache `Ready=True`, with
`status.observedGeneration` and the Ready condition's `observedGeneration`
matching the new `metadata.generation`, and all desired participants ready
(nonzero desired count). Every serving dataplane must activate the resulting
configuration. Kubernetes resource generation, `spec.cacheGeneration`, and
dataplane configuration revision are different values. The native fixture checks
activation directly; it does not run a Kubernetes P2PCache controller.

This is cache-wide logical invalidation for the P2PCache, not immediate physical
deletion of old slab data or removal of containerd ingests. Generation recovery
is explicit; a missing commit observation is not proof of corruption and does
not trigger automatic global eviction. See the
[operator recovery guide](../../docs/content/guides/gantry.md#recover-from-known-incorrect-cached-content).

Containerd recovery also needs attention to its retained failed ingest. In the
native campaign (containerd daemon 2.2.1, Go client 2.3.5), a normal pull leaves
`layer-<digest>` at offset/total 1,179,648 after digest rejection. Retrying after
generation activation recommits those bytes and fails with the same bad digest,
without issuing another layer GET. The test asserts that behavior rather than
hiding it behind a fresh `content.WriteBlob` reference. Its lease keeps the
campaign's content and ingest available across the sequential attempts.
These are tested runtime/client observations, not a universal cleanup or retry
contract for every containerd version or consumer.

For operator recovery, finish the failed pull and ensure there is no active writer
before explicitly aborting only the known failed ingest in its original containerd
namespace, keeping concurrent retries stopped. In the test, acquiring and closing
that exact writer confirms it is idle; after abort, the same image/ingest reference
fetches the repaired layer and commits successfully. The consumer namespace is
separate from Gantry's local stores, and
subsequent offline reads must increment Racer's `completed` counter. Drain/finish
pre-bump requests before asserting post-bump reads: activation does not
retroactively change an already-started stream.

The corruption is injected at origin before CRC admission; this test does not
claim peer transfer or disk CRC validation was removed. Racer retains peer CRC64
validation and background disk scrubbing, while file-backed foreground hits are
not rehashed. A self-consistent admission CRC is not an OCI digest proof.
`cmd/racer-dataplane/tests/http/peer_recovery.rs` separately checks malformed or
missing peer CRCs and altered metadata, rejection without cache publication,
healthy refetch, and subsequent cache reuse. Generic HTTP consumers must verify
their own content; the SDK's `StreamVerified` remains available.

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
RACER_LOADGEN_BINARY="$PWD/bin/racer-loadgen" \
make e2e-gantry-racer
```

All binary overrides must be absolute executable paths. On a kTLS-capable host
(Linux >= 6.14 and an eligible OpenSSL build), require real peer-page sendfile:

```sh
RACER_REQUIRE_KTLS=1 make e2e-gantry-racer
```

`Striped` fails if it does not observe at least one page of kTLS sendfile bytes
when this flag is set. Without it, the test still requires real peer transfers
and Gantry splice forwarding with zero tee counters, and logs the observed
sendfile byte count.

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
