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

## Iteration and diagnostics

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
