# Racer full-stack e2e test

Run from the repository root on a Linux Docker host with Go, Docker, kind,
kubectl, and make installed. Build the images first, outside the test timeout:

```sh
for component in unbounded-operator racer-controller racer-dataplane gantry; do
  docker build -f "images/$component/Containerfile" --build-arg VERSION=e2e \
    -t "docker.io/library/$component:e2e" . || break
done

make e2e-racer
```

The test uses these local images and creates an isolated kind cluster with two
workers. Setup and reads share a seven-minute deadline, leaving time for diagnostics
and cleanup within the ten-minute test timeout. Commands have a two-minute
deadline; workload waits are capped at 90 seconds. The host
must support Racer's io_uring and direct-I/O requirements. The kind nodes must
be able to reach HTTP fixtures on the Docker network's IPv4 host gateway.

The test deploys the real unbounded-operator using its rendered manifests and
creates a single `ClusterCache` named `gantry`. The operator and Racer controller
provision Racer's initialization, identity, configuration, and workloads. A
test Gantry DaemonSet shares the node's Racer socket directory and runs in Racer
mode with the same UID as the dataplane.

The acceptance check is a digest-pinned containerd image pull and unpack from an
empty content namespace. Its only registry endpoint is a recording proxy to
Gantry; there is no direct upstream fallback. The fixture includes a compressed
layer larger than Racer's 16 MiB page size. The test verifies:

- Racer readiness before and after the pull.
- Complete manifest, config, and layer delivery with `Gantry-Mirrored: 1`.
- Origin requests for every object.
- Exact SHA-256 digests and sizes in containerd's content store.

This preserves the cold origin-backed path through Gantry, the Go SDK, the
deployed Rust dataplane, and Gantry's origin adapter.

## Peer reuse and bounded recovery

After the containerd pull, the same cluster exercises the deployed Rust peer
transport using identities, cache keys, and membership provisioned by the real Go
controller. A separate immutable blob has two full 16 MiB pages and a short final
page. The fixture selects a digest whose three pages rank the serving worker
first under Racer's equal-share placement. This makes the source deterministic
without changing placement, readiness, or controller behavior.

The test warms all three pages on that worker, then reads through the second:

1. Read page zero and require exactly one new `racer_peer_hits_total`, no new
   reader origin fill, and no new fixture GET for any page.
2. Insert a narrowly scoped `iptables` TCP-reset rule inside the serving kind
   node for the reader pod's traffic to the serving pod's peer port. Read page
   one, which is still cold on the reader. Require completion within 30 seconds,
   exactly one origin fill and corresponding origin range GET, no peer hit, and
   a positive packet count on the interruption rule. This verifies that the read
   actually tried the unavailable peer before falling back.
3. Remove the rule and read the still-cold short final page. Require a new peer
   hit with no new origin GET or fill, proving peer service resumes after healing.

Every warm/read validates HTTP 206, the pinned ETag, the exact Content-Range,
length, and every returned byte. The peer phases use kind's `curl` against Racer's
real client Unix socket, with Gantry's deployed Go origin adapter still serving
origin requests. They use explicit `If-Match` pins rather than SDK bootstrap reads:
unpinned admission can legitimately fetch fresh metadata and page zero. Fixture
HEAD requests are counted separately and allowed; they cannot mask an origin
data GET. No request retries hide a failed read; curl has a 25-second limit and
its process has a 30-second context deadline.

The interruption rule is removed on failure as well as success. Metrics snapshots
and packet counters are retained alongside normal diagnostics. The host needs
capacity for three kind containers and enough free inotify instances for their
systemd/kubelet processes. `Failed to create control group inotify object: Too many
open files` during kind startup indicates host inotify exhaustion, before Racer is
deployed.

## Populated cache deletion and recreation

The final phase deletes `ClusterCache/gantry` through Kubernetes while an origin
fill is outstanding, then recreates the same name with a different API-assigned
UID. The real controller and Rust HTTPS control poll perform every transition.
The test:

1. Warms an immutable object on both nodes and verifies repeated pinned reads
   reuse it without additional origin GETs.
2. Starts a cold read whose origin sends a prefix and holds the remaining body.
   Deletion must cancel it before the ordinary request timeout and remove both
   client sockets. The held client cannot complete successfully.
3. Recreates the cache and polls pinned HEAD requests for replacement socket
   readiness. Hard links retained to the old sockets must reject promptly, and
   the replacement sockets must have different inodes.
4. Releases the old origin handler only after replacement readiness, attempting
   its late body write. With origin data GETs denied but HEAD still available,
   reads of both the warmed and held objects must fail on both nodes and must
   attempt origin. Old cached bytes cannot satisfy the replacement UID.
5. Enables origin data again, checks exact bytes, ETag, and Content-Range, and
   verifies the replacement pages become reusable without additional GETs.

Pod UIDs and restart counts must remain unchanged throughout this phase, so a
restart cannot substitute for live cache retirement. The fixture keeps immutable
digest identities; origin request accounting distinguishes a genuine fresh fill
from old byte reuse even though the correct object bytes are identical. No
metrics thresholds or fixed sleeps are used. Release is idempotent and the held
handler also exits on test cancellation. The existing `make e2e-racer` target
includes this phase without additional tools or images.

## Live key and issuer rotation

Between peer recovery and cache recreation, the same two live Rust dataplanes
exercise Go-controller-driven rotation. A supported `racer` Deployment override
sets a two-minute leaf TTL, one-minute preparation, and two-minute retention.
The normal rotation interval stays at 24 hours so earlier accounting checks cannot
race activation. The test advances only `rotation.json`'s scheduling deadline
with a resourceVersion precondition; the real Go reconciler generates and
publishes all prepared, active, retiring, and pruned keys and issuer roots.
No credential bundle is constructed by the test.

The phase checks:

- Both Rust processes install the prepared Secret projection before activation.
  One pod's mount namespace temporarily pins a copy of that genuine projection;
  kubelet, token projection, and control polling continue normally. The other
  node installs activation while the delayed node demonstrably remains prepared.
- Warm data stays byte-correct. After activation, fresh admission to retired
  keys closes and the previously warm server must refill from origin. The delayed
  reader must return correct bytes through bounded origin fallback. Unmounting
  the pin exposes the newest real projection, and both nodes converge.
- Each live node persists a distinct controller-issued leaf signed by the newly
  activated root, with the two-minute TTL. Its running identity-expiry gauge must
  match that leaf. After the original certificates have actually expired, a
  still-cold reader must get a validated peer page with no origin GET/fill.
- The Go controller removes old shared keys, the public root, and private issuer
  material. Both Rust processes install the pruned generation and remain ready.
  Key IDs are compared as opaque bytes, with no assumed encoding.
- Twenty full-page pressure objects exceed the default 256 MiB node plaintext
  budget, distributed across worker buckets. Before rotation and after prune,
  reading the evicted target must increment the disk-hit counter exactly once
  without an origin GET/fill. Every page read checks exact bytes, HTTP 206, ETag,
  and Content-Range. Pod UIDs and restart counts must remain unchanged.

Before warming the target, the test waits for zero pending disk writes and records
the disk-publication counter. The isolated cold target fill must produce exactly
one new publication and return to zero pending writes within ten seconds before
pressure starts. The same precondition applies to its active-key refill. Publication
is counted only after completed slab I/O and successful index installation;
abandoned or retired writes cannot satisfy it. These are bounded observations,
not request retries or a sleep hoping that writeback has finished.

The rotation blob requires an exact public synthetic Authorization value for
both HEAD and GET. This exercises active origin-key sealing in page dispatch and
peer requests before and after rotation, with exact delivery to the Go origin
adapter. It does not establish cross-node credential decryption: both nodes are
ranked candidates, so the predecessor request is CopyOnly and deliberately never
opens Authorization. The server's origin-fill count must not change during that
probe. Cross-node Acquire credential decryption needs a noncandidate reader.

The fixed, unlabeled `racer_keyring_generation` and
`racer_identity_expires_at_seconds` gauges describe installed local credentials;
they are observations, not controller acknowledgments. Public certificate chains
are decoded in memory; private keys and bundles are not written to diagnostic
artifacts. Metrics snapshots are retained. The projection pin is removed during
failure cleanup as well as on success.

The pin uses a mode-0700 parent, validates the complete decoded bundle against the
expected controller bundle before/after copying and after mounting, and retries
copy races for at most eight seconds. Release removes both the bind mount and the
private copied material. Bundles never enter diagnostic artifacts. The fixed
`racer_pending_disk_writes` gauge counts accepted pending writes, and
`racer_disk_publications_total` counts completed index publications across workers.

Polling is one second, with 45-second projection bounds, 100-second renewal
bounds, and pruning bounded by the actual retirement deadline plus 15 seconds.
The existing 25-second curl / 30-second request bounds apply. Cached reads also
run while awaiting pruning. There are no fixed expiry sleeps, new clusters, or
additional image builds in the test; the seven-minute shared deadline and
ten-minute `make e2e-racer` timeout remain in force.

## Iteration and diagnostics

### CI coverage

The `Racer Full-stack Image Pull` job in `.github/workflows/ci.yaml` runs this
suite on every pull request to `main` or `release-*`, every merge group, release
branch pushes, and manual CI dispatches. It uses an Ubuntu 24.04 Docker host,
Go from `go.mod`, kind v0.28.0, and kubectl v1.33.1 to match the suite's node
image. Host io_uring and inotify limits are configured before cluster creation.

CI builds all four images above serially with `VERSION=e2e`, with a separate
GitHub Actions BuildKit cache per component, and loads them into the local Docker
daemon. The kind node image is pulled before the test. The complete job has a
60-minute budget; the test step has a 15-minute limit around the existing
ten-minute Go test timeout, allowing Go compilation and diagnostic upload.
The `racer-e2e-diagnostics` artifact retains the test output, pod/event logs,
port-forward logs, and peer metric/packet-counter logs for seven days. Only logs
are uploaded; the generated kubeconfig and manifests stay on the runner.

The separate `Racer Rust Suite` job also explicitly runs the real Go SDK/Rust
wire test with `make racer-sdk-conformance`. See the
[dataplane test instructions](../../cmd/racer-dataplane/README.md).

### Local debugging

- Rebuild changed components before rerunning the test; it always uses locally
  built `docker.io/library/*:e2e` images by default. For concurrent development,
  build all four images with a unique repository prefix and set
  `RACER_E2E_IMAGE_REGISTRY=docker.io/<unique-prefix>`; the `e2e` tag must also be
  the operator's build-time `VERSION`. This avoids overwriting another run's tags.
- `RACER_E2E_KEEP_CLUSTER=1` retains the cluster for debugging. Delete it with
  `kind delete cluster --name <name>` before another run on resource-limited hosts.
- Generated fixtures, kubeconfig, pod/event diagnostics, and port-forward logs
  stay in the reported `tmp/racer-e2e-*` directory, including after failures.
- Readiness checks recreate port-forward processes that exit while the remote
  listener is starting. Each attempt retains a
  `forward-<pod>-<port>-<sequence>-<attempt>.log`;
  an HTTP 200 is required within the readiness deadline.
- The cluster is deleted by default. No existing cluster is used.
