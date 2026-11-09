# Gantry cluster bench

Tooling to measure synchronized large-image pulls with gantry on the
`joolshev-rdma` AKS cluster (150 x Standard_D64ds_v6 workers, 142 Ready ones
labeled `cluster-workload=rdma`). Everything is plain bash + kubectl plus one
Go helper (`hack/cmd/gantry-cluster-bench`).

| file | purpose |
|---|---|
| `lib.sh` | shared settings (namespace, origin node, KUBECONFIG) and DaemonSet helpers |
| `registry.sh` | in-cluster origin registry (`registry:3`) and synthetic image generation |
| `prep.sh` | writes `certs.d/<origin>/hosts.toml` (gantry mirror first, origin as fallback) |
| `clean.sh` | removes bench images and gantry leases, prunes content, reports disk free |
| `run.sh` | synchronized `crictl pull` on all target nodes with 1s sampling |
| `node.sh` | the script the bench DaemonSet pods run (all modes) |
| `build-gantry-image.sh` | builds gantry from any source tree and pushes it to the origin |
| `deploy.sh` / `undeploy.sh` | helm install / uninstall of `deploy/gantry/chart` |
| `values-default.yaml` | gantry default (p2p) mode |
| `values-cachemesh.yaml` | cachemesh mode overlay (layer over values-default.yaml) |
| `meshstate.sh` | one-line cachemesh state of every agent (ready, leaders, members, term, seq) |

Results and scratch files go to `tmp/gantry-cluster-bench/` at the repo root
(gitignored).

## Cluster facts the scripts rely on

- containerd and kubelet run inside a systemd-nspawn machine. Pods see that
  machine's view: PID 1 of a `hostPID` pod is the machine's systemd, so
  `nsenter -t 1 -m -u -n -p` reaches `/run/containerd/containerd.sock`,
  `/etc/containerd/certs.d`, `crictl` and `ctr` (all at their normal paths;
  there is no `/var/lib/machines` to deal with). `hostPath` volumes also land
  in the machine root.
- Primary NIC is `eth0` (MANA, 30Gb/s); the OS/containerd disk is `nvme0n1`
  (the other NVMe drives belong to racer, do not touch them).
- Every node has `certs.d/_default/hosts.toml` -> `http://127.0.0.1:5000`
  without a `server`. A registry without its own `certs.d` directory therefore
  fails to pull while gantry is down. Run `prep.sh apply` after adding nodes.
- When gantry is not running, the mirror dial is refused immediately and
  containerd falls back to the origin `server` (verified), which is how the
  gantry image itself is pulled.

## Origin registry

- Runs as `gantry-bench/origin-registry` on `joolshev-rdma-w0007` (quarantine
  node, not a pull target), `hostNetwork`, plain HTTP on **`10.224.16.40:5005`**
  (`./registry.sh addr`). Storage: `/var/lib/gantry-bench-registry` on the node
  root fs. Deletes are enabled.
- Bench images: `bench/g40:run1..run4` (4 layers x 10GiB, 42949683200 bytes
  each) and `bench/smoke:smoke1` (4 x 256MiB). Layers are uncompressed tars
  (`application/vnd.oci.image.layer.v1.tar`) holding one file of ChaCha8
  output seeded by `repo:tag`, so they are incompressible and every tag has
  unique digests. Each 40GB image takes about 60s to generate (four layers in
  parallel at about 190MB/s each).

```bash
cd hack/gantry-cluster-bench
./registry.sh up               # idempotent
./registry.sh install-tool     # copy the helper into the registry pod
./registry.sh gen run4         # 4 x 10Gi; or: gen <tag> <layers> <size> <repo>
./registry.sh ls; ./registry.sh df
./registry.sh delete run1 && ./registry.sh gc   # free space (never during a push)
```

Disk on w0007: about 81GB left with four 40GB images. `run1` and `run2` were
pulled directly from the origin by w0000-w0003 during validation (since
cleaned); `run3` and `run4` have never been pulled. Each pulled 40GB
image needs about 43GB on every worker (content store plus snapshot is about
86GB, since unpacking duplicates the data), so two un-cleaned runs nearly fill
a 247GB worker. **Always run `clean.sh` between runs.**

## Runbook

All commands run from `hack/gantry-cluster-bench/` with
`KUBECONFIG=~/.kube/config.rdma` (the default in `lib.sh`).

1. One-time preparation (already done):

   ```bash
   ./registry.sh up && ./registry.sh install-tool
   ./prep.sh apply                 # all 142 Ready rdma nodes
   ```

2. Build and push a gantry image from a worktree:

   ```bash
   IMG=$(./build-gantry-image.sh ~/code/unbounded/.worktrees/<tree> <tag>)
   echo "$IMG"                     # 10.224.16.40:5005/gantry:<tag>
   ```

3. Deploy gantry (default p2p mode) and wait for 142 Ready agents:

   ```bash
   ./deploy.sh values-default.yaml "$IMG"
   ```

   The agent limits are raised to 32 CPU / 48Gi (both modes) so that a 40GB fan-out is not
   throttled. The chart has no pod annotations, so `run.sh` reads each agent's
   `:9095/metrics` through the API server proxy instead of Prometheus.

4. Clean, then run (use a tag no node has pulled before):

   ```bash
   DROP_CACHES=1 ./clean.sh
   ./run.sh gantry-default-run1 10.224.16.40:5005/bench/g40:run1
   ```

5. Switch to cachemesh: fill in `values-cachemesh.yaml`, then

   ```bash
   ./undeploy.sh                   # gantry leases and identities are reset
   DROP_CACHES=1 ./clean.sh
   ./deploy.sh values-default.yaml "$IMG" -f values-cachemesh.yaml
   ./run.sh gantry-cachemesh-run2 10.224.16.40:5005/bench/g40:run2
   ```

   `values-cachemesh.yaml` leaves `maxNeighbors` at the agent default of 32
   (the multi-hop relay grid, mode C in `designs/gantry-cachemesh-benchmark.md`).
   Add `--set gantry.cachemesh.maxNeighbors=256` for the 1-D direct grid
   (mode B).

   The cachemesh chunk files live in the agent pod's `emptyDir`, so
   `undeploy.sh` removes them; `clean.sh` does not need to.

   Use a different image tag per run: the origin registry is the only place
   bench images live, and a reused digest might still be cached somewhere.
   `clean.sh` removes leases, but gantry also keeps in-memory provider
   records, so undeploy/deploy between modes.

6. Compare:

   ```bash
   go run ./hack/cmd/gantry-cluster-bench compare \
     tmp/gantry-cluster-bench/gantry-default-run1 tmp/gantry-cluster-bench/gantry-cachemesh-run2
   ```

7. Leave the cluster clean: `./undeploy.sh && ./clean.sh`.

## run.sh

`run.sh <label> <image-ref>` with env `NODES` (`"4"` = first 4 Ready nodes, or
names), `START_DELAY` (120s), `MAX_WAIT` (3600s per pull), `TAIL_SECONDS`
(15), `INTERVAL` (1s), `FORCE=1` to overwrite a label.

1. Looks up the image digest and size in the origin.
2. Starts `bench-origin-sampler` on w0007 and `bench-pull` on every target
   node (privileged, hostPID, hostNetwork, image `ubuntu:24.04`, already
   cached on all nodes) with a shared `START_AT` epoch. Node clocks are NTP
   synchronized.
3. Snapshots gantry metrics and origin `eth0` tx bytes, waits for every pod
   to print `DONE`, snapshots again, collects logs, and deletes both
   DaemonSets.
4. Runs `gantry-cluster-bench summarize`.

Raw output in `tmp/gantry-cluster-bench/<label>/`:

- `meta.env`: label, image, digest, bytes, `START_AT`, target nodes, gantry image.
- `pods/<node>.log`: `RESULT <node> <start_ns> <end_ns> <rc>`, `PULLOUT` lines
  (crictl output), and once per interval
  `SAMPLE <node> <ts_ns> <rx> <tx> <disk_wr_sectors> <cpu_busy> <cpu_total> <mem_total_kb> <mem_avail_kb> <gantry_ticks> <gantry_rss_kb> <containerd_ticks> <containerd_rss_kb>`
  (`-` when a process is absent), and
  `MEMSAMPLE <node> <ts_ns> <disk_rd_sectors> <cg_current> <cg_anon> <cg_file> <cg_active_file> <cg_inactive_file> <cg_file_dirty> <cg_file_writeback>`
  with the gantry container's cgroup v2 `memory.current` and `memory.stat`
  values in bytes. The working set (`container_memory_working_set_bytes`) is
  `cg_current - cg_inactive_file`.
- `origin.log`: the same SAMPLE lines for w0007.
- `origin-pre.txt` / `origin-post.txt`: `rx tx` bytes of w0007 `eth0`.
- `metrics-pre/`, `metrics-post/`: one Prometheus text file per gantry pod.
- `summary.md`, `summary.json`, `completion.csv` (seconds since first start,
  nodes completed).

The summary reports pull duration distribution, makespan, success counts,
aggregate throughput (image bytes x succeeded / makespan), per-node MB/s, origin
egress (bytes and multiple of image size), NIC rx/tx average and peak, disk
writes and reads, node CPU and memory, gantry and containerd CPU and RSS, the
gantry container's working set, anon memory, and page cache (peak, and at the
end of the idle tail) (each as min, p50,
p90, p99, and max across nodes), and the sum across pods of every changed
`gantry_*_total` counter and any `gantry_cachemesh_*` series. Rates use the
node's own pull window; disk writes include the tail.

## Direct-origin baseline (no gantry)

Measured on 2026-11-08 with `bench/g40` (42.95GB):

| run | nodes | pull s (min / p50 / max) | makespan | origin egress | origin tx peak |
|---|---|---|---|---|---|
| `direct-g40-1node` | 1 | 94.7 | 94.7s | 43.0GB | 2.2GB/s |
| `direct-g40-4node` | 4 | 102.7 / 110.3 / 124.3 | 124.3s | 172.0GB | 3.7GB/s (NIC cap) |

On one node the download finishes after about 26s (about 2.1GB/s rx), and the
remaining about 69s is containerd unpacking the layers (about 600MB/s of disk
writes). The unpack time is a floor that gantry cannot remove. A pull writes
about 64GB within the tail and uses about 86GB of disk once finished. Containerd uses
about 1 core and about 90MiB of RSS.

## Safety notes

- Never run a 142-node pull without gantry. Direct-origin pulls are capped by
  the origin NIC (about 3.6GB/s), so 142 x 43GB would take about 30 minutes of
  saturated origin egress.
- Do not touch `unbounded-system/racer`, `default/bench-*`, `monitoring/*`.
- If `run.sh` is killed, delete leftovers with
  `kubectl -n gantry-bench delete ds -l gantry-cluster-bench=true`.
- `registry.sh gc` while an image is being generated can delete its blobs.
