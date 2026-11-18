---
title: "Load vLLM Models from Racer"
weight: 11
description: "Load model weights from a Racer cache straight into GPU memory with vLLM."
---

{{< callout type="warning" >}}
Racer is pre-release. Its APIs, flags, and on-disk formats may change without a
migration path. See the [Racer reference]({{< relref "reference/racer" >}}).
{{< /callout >}}

This guide loads vLLM model weights from a Racer cache. The `racer` load format
reads each safetensors file through the local Racer dataplane. When GPU mode is
on, the dataplane writes the bytes straight into GPU memory. Otherwise the
client falls back to host memory, then to a plain stream.

This helps most when many vLLM replicas start at once. Each page of the
weights comes from the source only a few times, and the replicas share the
pages among themselves instead of each downloading the full model.

## Before You Start

- A ClusterCache whose origin holds the model files. The
  [Azure Blob guide]({{< relref "guides/azure-blob-cache" >}}) shows how to make
  one. This guide uses a cache named `models`, a bucket named `llama`, and the
  prefix `Llama-3.1-8B-Instruct`.
- NVIDIA nodes set up by the unbounded agent.

## 1. Check GPU Mode

GPU mode is on by default. The `racer` image is built with CUDA, and
`RACER_ENABLE_GPU` is `auto`. The dataplane does not touch the GPUs until the
first GPU read, so vLLM's first load turns GPU mode on. After a load, check a
dataplane log on the GPU node:

```bash
kubectl -n unbounded-system logs <dataplane-pod> | grep 'stage=gpu state='
```

`state=enabled` means GPU mode is on. `state=disabled` gives the reason.
`state=standby` means no GPU read has come in yet.

To make the dataplane set up the GPUs at startup and fail when it finds none,
so a broken setup is not silent, set `RACER_ENABLE_GPU` to `true`:

```bash
kubectl -n unbounded-system patch configmap racer-settings \
  --type merge -p '{"data":{"RACER_ENABLE_GPU":"true"}}'
```

The image also has NVIDIA's cuFile library and a `cufile.json` for GPUDirect
Storage. Without GPUDirect Storage on the host, disk reads go through host
memory.

The dataplane gets the NVIDIA driver libraries without requesting a GPU. This
is on by default (`RACER_NVIDIA: env` in `racer-settings`). See
[GPU reads]({{< relref "reference/racer" >}}#gpu-reads).

## 2. Deploy vLLM

Add the client socket label and the cache annotation to the vLLM pod:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: vllm
  labels:
    racer.unbounded-cloud.io/client-socket: enabled
  annotations:
    racer.unbounded-cloud.io/client-cache: models
spec:
  containers:
    - name: vllm
      image: <registry>/racer-vllm:<tag>
      command:
        - sh
        - -c
        - |
          racer-vllm-fetch racer://models/llama/Llama-3.1-8B-Instruct /models/llama &&
          exec vllm serve /models/llama --load-format racer
      volumeMounts:
        - name: model-config
          mountPath: /models
      resources:
        limits:
          nvidia.com/gpu: 1
  volumes:
    - name: model-config
      emptyDir: {}
```

The webhook mounts `/run/racer/models/client` read-only into the container and
sets `RACER_SOCKET` to the socket in it. To limit the mount to some containers,
add `racer.unbounded-cloud.io/client-containers: vllm`. Init containers get
the mount too, so `racer-vllm-fetch` can run in an init container that writes
to the shared `emptyDir`.

## 3. Install racer-vllm

Release and nightly builds publish the image `racer-vllm` next to the other
images, at the same registry and tag. It is `vllm/vllm-openai:v0.33.0` with
both packages installed, so it needs no changes. `racer-vllm` supports vLLM
v0.30.0 and v0.33.0 or later.

To use another vLLM image, install both Python packages into it. They are not
on PyPI. GitHub releases include their wheels and sdists (`racer_client-*` and
`racer_vllm-*`) as assets. Releases made before the packages were added do not.
Set `VERSION` to a release tag and point pip at it:

```bash
VERSION=vX.Y.Z
pip install --no-index \
  --find-links "https://github.com/Azure/unbounded/releases/expanded_assets/${VERSION}" \
  racer-vllm
```

`--find-links` reads the asset links from the release page. `--no-index` keeps
pip off PyPI, so a package there with the same name cannot be installed by
mistake. This works because `racer-vllm` needs only `racer-client`, and
`racer-client` needs nothing else.

You can also install the wheels by URL. The file names carry the package
version, not the release tag:

```bash
pip install --no-index \
  "https://github.com/Azure/unbounded/releases/download/${VERSION}/racer_client-0.2.0-py3-none-any.whl" \
  "https://github.com/Azure/unbounded/releases/download/${VERSION}/racer_vllm-0.2.0-py3-none-any.whl"
```

To build an image without network access, download the wheels first and use
`--find-links <dir>`.

From a checkout:

```bash
pip install ./clients/python/racer ./clients/python/racer-vllm
```

`make racer-python-dist` builds wheels for both packages into
`tmp/racer-python-dist`.

`racer-vllm` registers the `racer` load format with vLLM through a plugin entry
point. No other code change is needed.

## 4. Run

vLLM reads the model config and tokenizer before it loads weights, so loading
takes two steps:

1. `racer-vllm-fetch racer://<cache>/<bucket>/<prefix> <dir>` copies
   `config.json`, the tokenizer files, and the safetensors index into `<dir>`.
   It also writes `racer-model.json`, which records the `racer://` location and
   the ETag of each fetched file and each weight file.
   The prefix is the directory that holds `model.safetensors.index.json` and
   the safetensors files. It prints the weight layout (`regular` or
   `sharded_state`), the number of weight files and their total size.
2. `vllm serve <dir> --load-format racer` reads the weights through Racer, at
   the ETags in `racer-model.json`. If the model changed at the origin since
   the fetch, or changes during the load, the load fails with
   `ModelChangedError`. It also fails if the checkpoint layout changed, for
   example an index was added after a single-file fetch, because a weight file
   it would read has no ETag in `racer-model.json`. Run `racer-vllm-fetch`
   again to pick up the new version.

After each load, vLLM's log shows one `racer: loaded ...` line with the file
count, bytes, time, throughput, the number of reads in each delivery mode
(`gpu`, `host`, `stream`), and any tensors the load skipped. When the
dataplane refuses a mode, the client logs a warning with the reason once, then
retries the mode after `mode_retry_seconds` (default 300) or when the
dataplane restarts.

With tensor parallelism, each rank reads every weight file of a regular
checkpoint. Ranks on one node share the dataplane, so repeat reads come from
its cache, not the origin, but they still cost client, dataplane and PCIe
work: a 475.24 GiB checkpoint at `tensor_parallel_size=4` read 1.9 TiB on the
node while each rank kept 80.77 GiB. The loader logs a warning with these
numbers for regular checkpoints with a tensor parallel size above 1. To have
each rank read only its own files, save a vLLM `sharded_state` checkpoint
(`examples/features/sharded_state/save_sharded_state_offline.py` in vLLM) and
pass `--model-loader-extra-config '{"sharded_state": true}'`.
`racer-vllm-fetch` records every rank's files; pass `--pattern` if the
checkpoint uses a custom file name pattern.

With speculative decoding, vLLM loads the draft model with the target's load
config unless `--speculative-config` sets `draft_load_config`. The loader
recognizes the draft load and reads only the tensors the draft uses: it skips
those the model marks unused with `is_unused_checkpoint_weight` (vLLM 0.33 and
later), reads only the `mtp.*` tensors for DeepSeek-V4 DSpark drafters, and
applies the `tensor_include` and `tensor_exclude` options. When no rule
applies and the draft reads the target's checkpoint, it logs a warning with
the bytes it reads again.

Some models, such as DeepSeek-V4 and GLM, keep references to the weight
tensors they are given while they load. If such a model runs out of GPU
memory during the load, set `tensor_device` to `cpu` in
`--model-loader-extra-config`. The loader then hands vLLM CPU tensors, as
vLLM's default loader does, at the cost of `gpu-read`.

See the `racer-vllm` README for all loader options, such as `staging_bytes`.

The first load fills the cache from the origin. Later loads on any node read
from the cache. On a cold cache the loader also submits a prefetch ticket for
the weight files, so the dataplane fills pages in the background while vLLM
reads them. This uses the dataplane's prefetch concurrency and needs no extra
GPU memory. To turn it off, set `prefetch` to false in
`--model-loader-extra-config`.

## Benchmarking and Tuning

`hack/scripts/racer-vllm-bench.sh` measures weight loads over a matrix of
loader settings. For each combination of `concurrency`, `staging_bytes`, and
`use_vmm`, it starts `vllm serve <dir> --load-format racer`, waits until every
rank logs its `racer: loaded ...` line, stops vLLM, and appends one row to a
CSV file. vLLM is stopped right after the load, so KV cache setup and CUDA
graph capture are not part of the numbers.

### Run the Benchmark

Run the script in a vLLM pod set up as in steps 2 and 3, with as many GPUs as
the tensor parallel size. Give the pod a command that keeps it idle, such as
`sleep infinity`, so the benchmark's vLLM is the only one on its GPUs. Fetch
the model first:

```bash
kubectl cp hack/scripts/racer-vllm-bench.sh vllm:/models/racer-vllm-bench.sh
kubectl exec vllm -- \
  racer-vllm-fetch racer://models/llama/Llama-3.1-8B-Instruct /models/llama
kubectl exec vllm -- env \
  TP=1 OUT=/models/bench/results.csv \
  RANGE_WINDOW_PAGES=4 GPU_STAGING_BYTES=268435456 \
  RACER_METRICS_URL=http://<dataplane-pod-ip>:9090/metrics \
  bash /models/racer-vllm-bench.sh /models/llama --max-model-len 4096
kubectl cp vllm:/models/bench ./bench
```

Arguments after the model directory go to `vllm serve`. Pass `--port` if
something in the pod already listens on port 8000. Set the tensor parallel
size with `TP`, not `--tensor-parallel-size`, so the script knows how many
load lines to wait for. With `{"tensor_device": "cpu"}` in `EXTRA_CONFIG`,
`use_vmm` has no effect, so set `USE_VMM` to one value. Run
`racer-vllm-bench.sh --help` for every variable.

| Variable | Default | Meaning |
|---|---|---|
| `TP` | `1` | Tensor parallel size. |
| `RANKS` | `TP` | Load lines to wait for. Use the tensor parallel size times the pipeline parallel size, and double it to include a draft model that also loads through Racer. |
| `CONCURRENCY` | `2 4 8` | `concurrency` values. |
| `STAGING_BYTES` | `256M 512M 1G` | `staging_bytes` values. `K`, `M`, and `G` are binary units. |
| `USE_VMM` | `true false` | `use_vmm` values. |
| `EXTRA_CONFIG` | `{}` | JSON object merged into every `--model-loader-extra-config`, such as `{"sharded_state": true}`. The matrix values win. |
| `REPEAT` | `1` | Runs of each combination. |
| `WARMUP` | `1` | Runs of the first combination before the matrix, recorded with `phase` `warmup`. |
| `TIMEOUT` | `3600` | Seconds to wait for one load. |
| `PAUSE` | `5` | Seconds to wait after vLLM stops, so its GPU memory is freed. |
| `OUT` | `racer-vllm-bench.csv` | CSV file. Rows are appended, so runs with other dataplane settings can share it. |
| `LOG_DIR` | `OUT` without `.csv`, plus `-logs` | Per-run directories. |
| `RACER_METRICS_URL` | none | The dataplane's `/metrics` URL. Without it, the `dp_*` columns are empty. |
| `LABEL` | none | Free-form text for the `label` column. |
| `RANGE_WINDOW_PAGES`, `GPU_STAGING_BYTES` | none | The dataplane's `RACER_RANGE_WINDOW_PAGES` and `RACER_GPU_STAGING_BYTES`, written to each row. They are labels only. |

For `RACER_METRICS_URL`, use the IP of the `racer-*` dataplane pod on the vLLM
pod's node (`kubectl -n unbounded-system get pods -o wide --field-selector
spec.nodeName=<node>`). The dataplane serves metrics on port 9090 of its pod
IP; see [Diagnostics and metrics]({{< relref "reference/racer" >}}#diagnostics-and-metrics).
Leave it unset if a network policy blocks the port.

### Change Dataplane Settings

`RACER_RANGE_WINDOW_PAGES` and `RACER_GPU_STAGING_BYTES` are dataplane
settings, so the script cannot change them from the vLLM pod. Change them in
`racer-settings` between invocations, wait for the dataplane to restart, and
run the script again with labels that match:

```bash
kubectl -n unbounded-system patch configmap racer-settings --type merge \
  -p '{"data":{"RACER_RANGE_WINDOW_PAGES":"8","RACER_GPU_STAGING_BYTES":"1073741824"}}'
kubectl -n unbounded-system rollout status daemonset/racer
```

A restarted dataplane pod may have a new IP, so check `RACER_METRICS_URL`. It
also starts with an empty memory cache, so keep `WARMUP` at 1 or more. Each
I/O worker's share of `RACER_PLAINTEXT_BYTES` and `RACER_CIPHERTEXT_BYTES`
must still hold the window plus one page; see
[GPU reads]({{< relref "reference/racer" >}}#gpu-reads).

### Columns

| Column | Meaning |
|---|---|
| `started` | UTC start time of the run. |
| `label`, `range_window_pages`, `gpu_staging_bytes` | The `LABEL`, `RANGE_WINDOW_PAGES`, and `GPU_STAGING_BYTES` labels. |
| `phase` | `warmup` or `measure`. Compare `measure` rows only. |
| `concurrency`, `staging_bytes`, `use_vmm` | The loader settings of the run. `staging_bytes` is in bytes. |
| `status` | `ok`, `exited` if vLLM exited before every rank loaded, or `timeout`. The end of `vllm.log` is printed for the last two. |
| `wall_s` | Seconds from starting `vllm serve` to the last load line, including vLLM startup. |
| `ranks` | Workers that logged a load, from the first `RANKS` load lines. A worker's draft load adds to its time and data rather than counting as another rank. |
| `load_s` | The slowest rank's load time, summed over its load lines. |
| `total_gib` | Tensor data that all ranks read. Each rank reads every file of a regular checkpoint, so this is the checkpoint size times the number of ranks. With `sharded_state`, it is the checkpoint size. |
| `agg_gibps` | `total_gib` divided by `load_s`. |
| `min_rank_gibps`, `max_rank_gibps` | The slowest and fastest rank's throughput: its data over its load time. |
| `reads` | Range reads by all ranks. |
| `gpu_gib`, `host_gib`, `stream_gib` | Data by delivery mode: the delivery mix. |
| `peak_gpu_mib` | Highest `memory.used` that `nvidia-smi` reported for any one GPU of the pod, sampled every 500 ms until the last load line. It includes memory other processes hold, such as the dataplane's GPU staging. |
| `peak_host_mib` | Highest memory use of the pod's container (cgroup `memory.current`), sampled every 500 ms until the last load line. It includes page cache, but not the dataplane pod's memory. |
| `dp_host_reads` | Successful `host-read` requests (`racer_host_read_request_nanoseconds_count`). |
| `dp_host_read_ms`, `dp_host_first_page_ms` | Mean `host-read` time to the final frame and to the first page, in milliseconds. |
| `dp_gpu_reads` | `gpu-read` requests imported on a GPU (`racer_gpu_read_requests_total{outcome="gpu"}`). |
| `dp_gpu_read_ms`, `dp_gpu_first_page_ms` | The same means for `gpu-read`. Empty when the dataplane has no `racer_gpu_read_request_nanoseconds_*` series. |
| `dp_gpu_read_refused` | `gpu-read` requests with any other outcome, such as `not_enabled` or `busy`. |
| `dp_gpu_pages_gpu_decrypt`, `dp_gpu_pages_cpu_decrypt` | Pages written to GPU memory that the GPU or the CPU decrypted. |
| `dp_cufile_staged_pages` | Pages cuFile read from disk straight into GPU staging. |
| `dp_cufile_failures` | cuFile reads or registrations that failed. Those pages went through host memory. |
| `dp_gpu_staging_exhausted` | Disk reads that found no free GPU staging slot and went through host memory. |
| `log_dir` | The run's directory, with `vllm.log`, the memory samples, and the metrics scrapes. |

The `dp_*` columns are the change in dataplane counters between a scrape just
before vLLM starts and one just after the last load line. They count the
whole node, so other pods reading through the same dataplane add to them.

### Baseline

A validation run loaded a 475.24 GiB checkpoint with a tensor parallel size of
4, `concurrency` 2, and `use_vmm` false at about 11.93 GiB/s aggregate and
3.01 to 3.48 GiB/s per rank. Its other settings were not recorded.

### Tuning Table

Record measured `measure` rows here. Cells marked "to be measured" have no
result yet. Window pages is `RACER_RANGE_WINDOW_PAGES` and GPU staging is
`RACER_GPU_STAGING_BYTES`; their defaults are 4 pages and 256 MiB.

| `concurrency` | `staging_bytes` | `use_vmm` | Window pages | GPU staging | Aggregate GiB/s | Per-rank GiB/s | Peak GPU MiB | Delivery mix |
|---|---|---|---|---|---|---|---|---|
| 2 | not recorded | false | not recorded | not recorded | 11.93 | 3.01 to 3.48 | not recorded | not recorded |
| 2 | 256 MiB | true | 4 | 256 MiB | to be measured | to be measured | to be measured | to be measured |
| 4 | 256 MiB | true | 4 | 256 MiB | to be measured | to be measured | to be measured | to be measured |
| 8 | 256 MiB | true | 4 | 256 MiB | to be measured | to be measured | to be measured | to be measured |
| 4 | 512 MiB | true | 4 | 256 MiB | to be measured | to be measured | to be measured | to be measured |
| 4 | 1 GiB | true | 4 | 256 MiB | to be measured | to be measured | to be measured | to be measured |
| 8 | 1 GiB | true | 4 | 256 MiB | to be measured | to be measured | to be measured | to be measured |
| 4 | 256 MiB | false | 4 | 256 MiB | to be measured | to be measured | to be measured | to be measured |
| 4 | 1 GiB | true | 8 | 256 MiB | to be measured | to be measured | to be measured | to be measured |
| 4 | 1 GiB | true | 8 | 1 GiB | to be measured | to be measured | to be measured | to be measured |
| 8 | 1 GiB | true | 8 | 1 GiB | to be measured | to be measured | to be measured | to be measured |

### Harmful Combinations

- **Large `concurrency` with small `staging_bytes`.** The loader reads up to
  `staging_bytes` per request (a larger tensor gets a read of its own) and
  keeps `concurrency` requests in flight per rank. Small buffers mean many
  small reads, and each pays a fixed cost for the request and its first page
  (`dp_host_first_page_ms`, `dp_gpu_first_page_ms`). More of them in flight
  share the same dataplane windows and GPU staging slots. Look for `reads`
  going up without `agg_gibps` following. Each rank also holds `concurrency`
  staging buffers of `staging_bytes` each, so 8 buffers of 1 GiB cost 8 GiB
  per rank (`peak_gpu_mib`, or `peak_host_mib` with `tensor_device` `cpu`).
- **`use_vmm` false on nodes where VMM works.** The loader then stages in
  torch memory, which cannot be exported, so reads use `host-read` into a
  pinned host buffer and a copy to the GPU instead of `gpu-read`. The data
  moves from `gpu_gib` to `host_gib`, and each rank can hold up to
  `concurrency` pinned host buffers of `staging_bytes` (`peak_host_mib`). With
  `use_vmm` true the loader falls back to torch memory by itself, with a
  warning, when exportable GPU memory is not available, so leave it on.
- **`RACER_GPU_STAGING_BYTES` smaller than the slots needed.** The dataplane
  splits it into slots of about one page. Two or more slots are kept for GPU
  decryption, and the rest hold pages that cuFile reads from local disk. A GPU
  read fetches up to `RACER_RANGE_WINDOW_PAGES` pages ahead, so a GPU needs
  roughly `concurrency` times the window in free slots to keep every disk
  read on cuFile. With fewer, `dp_gpu_staging_exhausted` grows and pages go
  through host memory. Below two slots (about 34 MiB) the CPU decrypts every
  page (`dp_gpu_pages_cpu_decrypt`). Raising the window without raising
  `RACER_GPU_STAGING_BYTES` makes this worse.
