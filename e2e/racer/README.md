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
