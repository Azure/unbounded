# Gantry cluster bench

Tools to measure many nodes pulling one large image at the same moment, with
gantry in default (p2p) mode or in Racer mode. Everything is plain bash and
kubectl, plus one Go helper (`hack/cmd/gantry-cluster-bench`).

Design and background: `designs/racer-containerd-store.md`.

| file | purpose |
|---|---|
| `lib.sh` | shared settings and DaemonSet helpers |
| `registry.sh` | in-cluster origin registry (`registry:3`) and synthetic images |
| `prep.sh` | writes `certs.d/<origin>/hosts.toml` (gantry mirror first, origin as fallback) |
| `clean.sh` | removes bench images and gantry leases, prunes content, reports free disk |
| `run.sh` | synchronized `crictl pull` on all target nodes with 1s sampling |
| `node.sh` | the script the bench DaemonSet pods run (all modes) |
| `build-gantry-image.sh` | builds gantry from a source tree and pushes it to the origin |
| `deploy.sh` / `undeploy.sh` | switch the operator-managed gantry between bench modes |
| `manifests/clustercache-racer.yaml` | `ClusterCache/gantry` with `persistence: Racer` |
| `manifests/clustercache-adapter.yaml` | `ClusterCache/gantry` with `persistence: Adapter` |

Results and scratch files go to `tmp/gantry-cluster-bench/` at the repo root
(gitignored).

## Settings

All scripts use the current kubeconfig context. Set these in the environment:

| variable | default | meaning |
|---|---|---|
| `REGISTRY_NODE` | none, required | node that runs the origin registry; keep it out of the targets |
| `REGISTRY_PORT` | `5005` | origin registry port (host network, plain HTTP) |
| `ORIGIN` | `<REGISTRY_NODE IP>:<REGISTRY_PORT>` | override the origin address |
| `TARGET_SELECTOR` | empty (all nodes) | label selector for pull target nodes |
| `NODES` | all targets | `N` for the first N Ready targets, or node names |
| `BENCH_NS` | `gantry-bench` | namespace for the registry and bench pods |
| `GANTRY_NS` | `unbounded-system` | namespace of the `gantry` and `racer` DaemonSets |
| `TOOLS_IMAGE` | `docker.io/library/ubuntu:24.04` | image for bench pods; pre-pull it |
| `WORK_DIR` | `<repo>/tmp/gantry-cluster-bench` | results |

Example: `export REGISTRY_NODE=mycluster-w0007 TARGET_SELECTOR=cluster-workload=bench`.

## Modes

Gantry is installed by the unbounded operator. The bench never installs it.
`deploy.sh <mode> [image]` switches the installed gantry:

| mode | ClusterCache/gantry | what it measures |
|---|---|---|
| `default` | deleted | gantry p2p (DHT, containerd store) |
| `racer` | `persistence: Racer` | Racer keeps pages in memory and on its slab disks |
| `adapter` | `persistence: Adapter` | Racer keeps pages in memory only; containerd's content store is the only disk copy |

`deploy.sh` also:

- adds the origin to `ConfigMap/gantry-config` `upstream_registries` (the
  original is saved and `undeploy.sh` restores it), and applies
  `GANTRY_SET` (default `peer_fetch_timeout=15m transfer_max_concurrent_serves=10`),
- writes key `gantry-cluster-bench.yaml` of
  `ConfigMap/unbounded-component-overrides` to raise the agent limits
  (`GANTRY_CPU_LIMIT=32`, `GANTRY_MEMORY_LIMIT=48Gi`) and, if given, set the
  gantry image,
- waits until the `gantry` DaemonSet has `GANTRY_RACER_ENABLED` set as the
  mode needs, then waits for the `racer` and `gantry` rollouts.

The `racer` DaemonSet is not changed. Racer runs where the operator places it,
and in Racer mode gantry follows that placement.

## Node requirements

- Every target node needs `certs.d/<origin>/hosts.toml` with the gantry
  mirror first and the origin as fallback. `prep.sh apply` writes it. Run it
  again after adding nodes.
- When gantry is down, the mirror dial is refused and containerd falls back
  to the origin `server`. This is how the gantry image itself is pulled.
- Set `NIC` and `DISK` for `run.sh` if the primary NIC is not `eth0` or the
  containerd disk is not `nvme0n1`.

## Origin registry

The origin is `registry:3` on `REGISTRY_NODE` with host networking. Storage is
`/var/lib/gantry-bench-registry` on the node. Deletes are enabled.

Bench images are uncompressed tar layers holding ChaCha8 output seeded by
`repo:tag`, so they do not compress and every tag has unique digests.

```bash
cd hack/gantry-cluster-bench
./registry.sh up               # idempotent
./registry.sh install-tool     # copy the helper into the registry pod
./registry.sh gen run1         # 4 x 10Gi into bench/g40; or: gen <tag> <layers> <size> <repo>
./registry.sh ls; ./registry.sh df
./registry.sh delete run1 && ./registry.sh gc   # free space (never during a push)
```

A pulled 40GB image needs about 86GB on each worker (content store plus
snapshot). **Run `clean.sh` between runs.**

## Runbook

All commands run from `hack/gantry-cluster-bench/`.

1. One-time setup:

   ```bash
   ./registry.sh up && ./registry.sh install-tool
   ./prep.sh apply
   ```

2. Optional: build and push a gantry image from a source tree:

   ```bash
   IMG=$(./build-gantry-image.sh ../.. dev-1)
   ```

3. Pick a mode, clean, and run. Use a tag no node has pulled before:

   ```bash
   ./deploy.sh default "$IMG"
   DROP_CACHES=1 ./clean.sh
   ./registry.sh gen run1
   ./run.sh default-run1 "$(./registry.sh addr)/bench/g40:run1"

   ./deploy.sh adapter "$IMG"
   DROP_CACHES=1 ./clean.sh
   ./registry.sh gen run2
   ./run.sh adapter-run2 "$(./registry.sh addr)/bench/g40:run2"

   ./deploy.sh racer "$IMG"
   DROP_CACHES=1 ./clean.sh
   ./registry.sh gen run3
   ./run.sh racer-run3 "$(./registry.sh addr)/bench/g40:run3"
   ```

   Racer keeps cached pages after a run. A new tag gives new digests, so
   earlier pages are never hit. Switching modes recreates
   `ClusterCache/gantry`, which drops its data.

4. Compare:

   ```bash
   go run ./hack/cmd/gantry-cluster-bench compare \
     tmp/gantry-cluster-bench/default-run1 tmp/gantry-cluster-bench/adapter-run2 \
     tmp/gantry-cluster-bench/racer-run3
   ```

5. Clean up: `./undeploy.sh && ./clean.sh`. Delete images with
   `registry.sh delete` and `registry.sh gc`, then `registry.sh down`.

### Second wave

To check that an adapter cache serves from containerd's content store on
holder nodes instead of the origin, pull the same tag twice: first on a
subset (`NODES=...`), then on the rest. Origin egress in the second run should
be near 0.

## run.sh

`run.sh <label> <image-ref>` with env `NODES`, `START_DELAY` (120s),
`MAX_WAIT` (3600s per pull), `TAIL_SECONDS` (15), `INTERVAL` (1s), `NIC`,
`DISK`, `FORCE=1` to overwrite a label.

1. Looks up the image digest and size in the origin.
2. Starts `bench-origin-sampler` on `REGISTRY_NODE` and `bench-pull` on every
   target node (privileged, hostPID, hostNetwork) with a shared `START_AT`
   epoch. Node clocks must be NTP synchronized.
3. Snapshots gantry agent `/metrics` (`:9095`, through the API server proxy)
   and origin NIC bytes, waits for every pod to print `DONE`, snapshots again,
   collects logs, and deletes both DaemonSets.
4. Runs `gantry-cluster-bench summarize`.

Raw output in `tmp/gantry-cluster-bench/<label>/`:

- `meta.env`: label, image, digest, bytes, `START_AT`, targets, gantry image,
  bench mode, and the `ClusterCache/gantry` spec.
- `pods/<node>.log`: `RESULT <node> <start_ns> <end_ns> <rc>`, `PULLOUT` lines,
  and per interval `SAMPLE`, `MEMSAMPLE`, and `IOSAMPLE` lines (see
  `node.sh` for fields). `IOSAMPLE` has the gantry container's cgroup and
  process disk bytes, to check that gantry writes no image data.
- `origin.log`, `origin-pre.txt`, `origin-post.txt`: origin node samples.
- `metrics-pre/`, `metrics-post/`: one Prometheus text file per gantry pod.
- `summary.md`, `summary.json`, `completion.csv`.

The summary reports pull duration distribution, makespan, aggregate and
per-node throughput, origin egress (bytes and multiple of image size), NIC,
disk, CPU and memory of the node, gantry and containerd, and the deltas of
`gantry_*_total` counters and all `gantry_racer_*` series.

The node samples cover the gantry and containerd processes only. Racer
dataplane memory and disk are not sampled; read them from Racer metrics.

## Safety notes

- Do not run a large pull without gantry. Direct pulls are capped by the
  origin NIC.
- If `run.sh` is killed, delete leftovers with
  `kubectl -n gantry-bench delete ds -l gantry-cluster-bench=true`.
- `registry.sh gc` while an image is being generated can delete its blobs.
- `deploy.sh` edits operator ConfigMaps. Run `undeploy.sh` when done.
