---
title: "Load vLLM Models from Racer"
weight: 11
description: "Load model weights from a Racer cache straight into GPU memory with vLLM."
---

{{< callout type="warning" >}}
Racer is pre-release. Its APIs, flags, and on-disk formats may change without a
migration path. See the [Racer reference]({{< relref "reference/racer" >}}).
{{< /callout >}}

This guide serves a vLLM model whose weights live in a Racer cache. The
`racer-vllm` plugin adds the `racer` load format. It reads each weight file
through the node's Racer dataplane, which writes the bytes straight into GPU
memory when it can, and otherwise into host memory.

This helps most when many vLLM replicas start at once. Each page of the
weights comes from the origin only a few times, and replicas share the pages
among themselves instead of each downloading the full model.

{{< callout type="note" >}}
This guide describes the `racer-vllm` command (`materialize`, `import`,
`prepare`, `warm`, `inspect`). Check that your release includes it. On older
releases only `racer-vllm-fetch` and the `racer-model.json` flow exist;
`racer-vllm-fetch` remains as a deprecated alias of `racer-vllm materialize`.
{{< /callout >}}

## Prerequisites

- **A cache.** A ClusterCache with an Azure Blob origin that holds the model.
  The [Azure Blob guide]({{< relref "guides/azure-blob-cache" >}}) shows how
  to make one. The examples use a cache named `models-hillsboro`.
- **Identity.** The dataplane reads the origin as the `racer` ServiceAccount.
  Its managed identity needs a federated credential for
  `system:serviceaccount:unbounded-system:racer` and the
  `Storage Blob Data Reader` role on the container. Importing weights needs a
  separate identity that can write blobs.
- **A plugin image.** Either the `racer-vllm` convenience image, which is a
  vLLM image with both Python packages installed, or the small
  `racer-vllm-plugin` image that holds the packages under `/opt/racer/python`
  and is mounted into your own vLLM image. Imports run from the plugin image,
  which also carries the Azure SDK for `racer-vllm import`.
- **NVIDIA nodes** set up by the unbounded agent. GPU mode is on by default
  (`RACER_ENABLE_GPU=auto`). See [GPU reads]({{< relref "reference/racer" >}}#gpu-reads).

Complete example manifests live in
[`deploy/racer/examples/models-weights/`](https://github.com/Azure/unbounded/tree/main/deploy/racer/examples/models-weights).

## 1. Import the Weights

`racer-vllm import` copies a Hugging Face snapshot into the origin container
and writes `racer-manifest.json` next to it:

```bash
racer-vllm import \
  --source /root/.cache/huggingface/hub/models--Qwen--Qwen3-8B/snapshots/<revision> \
  --dest https://<storage-account>.blob.core.windows.net/models/qwen/Qwen3-8B
```

The manifest lists every file with its size and SHA-256, and the parsed
safetensors header of each weight file. The command follows the snapshot's
links into `../../blobs`, uploads the files whose SHA-256 changed, and writes
the manifest last. When it replaces a model it deletes the old manifest
first, so a reader never sees a manifest that names a missing or partly
written blob. The source repo and revision come from the cache path; set
`--repo` and `--revision` to override them.

The upload uses the Azure SDK with `DefaultAzureCredential`, so in a pod it
authenticates with workload identity through `AZURE_CLIENT_ID`,
`AZURE_TENANT_ID`, and `AZURE_FEDERATED_TOKEN_FILE`. Outside the plugin
image, install the extra: `pip install 'racer-vllm[azure]'`.

The model's location in Racer is `racer://<cache>/<bucket>/<prefix>`. For an
Azure origin the bucket is the container, so the example above is
`racer://models-hillsboro/models/qwen/Qwen3-8B`.

See `import-job.yaml` in the examples for a Job that imports a snapshot
already on a node.

## 2. Serve

vLLM reads the model config and tokenizer before it loads weights, so those
small files must be on local disk. An init container runs
`racer-vllm materialize`, which copies config, tokenizer, chat template,
index, other small files, and `racer-manifest.json` into an `emptyDir`, checks
each against the manifest's SHA-256, and records every file's ETag and the
model's `racer://` URI in `racer-source.json`. The main container then runs
`vllm serve` on that directory with `--load-format racer`:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: qwen3-8b
  namespace: racer-models
spec:
  replicas: 1
  selector:
    matchLabels:
      app: qwen3-8b
  template:
    metadata:
      labels:
        app: qwen3-8b
        racer.unbounded-cloud.io/client-socket: enabled
      annotations:
        racer.unbounded-cloud.io/client-cache: models-hillsboro
    spec:
      initContainers:
        - name: plugin
          image: <registry>/racer-vllm-plugin:<tag>
          command: ["cp", "-a", "/opt/racer/python/.", "/plugin/"]
          volumeMounts:
            - name: plugin
              mountPath: /plugin
        - name: materialize
          image: <registry>/racer-vllm-plugin:<tag>
          command:
            - racer-vllm
            - materialize
            - racer://models-hillsboro/models/qwen/Qwen3-8B
            - /models/Qwen3-8B
          volumeMounts:
            - name: models
              mountPath: /models
      containers:
        - name: vllm
          image: vllm/vllm-openai:<vllm-version>
          env:
            - name: PYTHONPATH
              value: /opt/racer/python
          args:
            - /models/Qwen3-8B
            - --load-format
            - racer
            - --model-loader-extra-config
            - '{"uri": "racer://models-hillsboro/models/qwen/Qwen3-8B", "delivery": "auto"}'
          resources:
            limits:
              nvidia.com/gpu: 1
          volumeMounts:
            - name: models
              mountPath: /models
            - name: plugin
              mountPath: /opt/racer/python
              readOnly: true
      volumes:
        - name: models
          emptyDir: {}
        - name: plugin
          emptyDir: {}
```

The admission webhook mounts the client socket into every container,
including init containers, and sets `RACER_SOCKET`. To limit the mount, add
`racer.unbounded-cloud.io/client-containers: materialize,vllm`. The
`vllm/vllm-openai` image's entrypoint is `vllm serve`, so `args` starts with
the model directory.

With the `racer-vllm` convenience image, drop the `plugin` init container,
the `plugin` volume, and `PYTHONPATH`, and use that image for both
`materialize` and `vllm`.

The plugin registers the `racer` load format through vLLM's
`vllm.general_plugins` entry point. With `PYTHONPATH`, vLLM finds the entry
point from the package metadata under `/opt/racer/python`.

## 3. Optional: Prepare Sharded Artifacts

With tensor or expert parallelism, a regular checkpoint makes every rank read
and slice full tensors. `racer-vllm prepare` runs vLLM once, captures each
rank's weights after loading and before `process_weights_after_loading`, and
writes them as versioned artifacts:

```bash
racer-vllm prepare racer://models-hillsboro/models/glm/GLM-5 \
  --dest https://<storage-account>.blob.core.windows.net/models/glm/GLM-5 \
  --workdir /scratch/glm-prepare -- \
  --tensor-parallel-size 8 --enable-expert-parallel
```

Run it in an image that has vLLM, `racer-vllm[azure]`, and the GPUs the
layout needs. `--dest` is the model's own location in the origin, the same
URL given to `import`. The command materializes the model into
`--workdir`, builds a vLLM engine with the arguments after `--`, and uploads
each rank's artifact under `sharded/<fingerprint>/` with its
`racer-sharded.json` last. It then writes `racer-sharded-index.json`, which
lists the artifacts, and fails without uploading anything if the capture does
not cover every rank. The fingerprint covers what changes the per-rank
layout: the checkpoint manifest, the vLLM version, the parallel layout, and
the quantization and dtype settings. A loader with a different fingerprint does
not find matching artifacts. With `sharded: auto` it falls back to the full
checkpoint; with `sharded: require` it fails.

## 4. Optional: Warm the Cache

`racer-vllm warm` asks the dataplane to prefetch every weight file into the
cache, so the first replica does not pay for origin reads. Run it before a
rollout, for example from a Job on a node of the site:

```bash
racer-vllm warm racer://models-hillsboro/models/qwen/Qwen3-8B
```

It prints the resident bytes until the prefetch settles and exits nonzero if
any file failed or `--timeout` (default 3600 s) passed. Nothing is pinned; the
pages stay as long as the cache's normal eviction keeps them.

`racer-vllm inspect <racer://...>` prints the manifest summary, the sharded
artifacts listed in `racer-sharded-index.json` with how many ranks are
present, and whether the dataplane serves each file at the manifest's size.
It exits nonzero if a file is missing.

## Settings

Pass these in `--model-loader-extra-config` as one JSON object. All are
optional.

| Key | Default | Meaning |
|-----|---------|---------|
| `uri` | From `racer-source.json` | The `racer://` model location |
| `sharded` | `auto` | `auto` uses matching `sharded/<fingerprint>/` artifacts when present; `require` fails without them; `off` always reads the full checkpoint |
| `delivery` | `auto` | `auto` tries GPU delivery, then host; `gpu` fails rather than fall back; `host` never asks for GPU delivery |
| `staging_bytes` | 256 MiB | Size of each staging buffer |
| `max_staging_bytes` | 4 GiB | Cap on all staging memory |
| `concurrency` | `4` | Reads in flight |
| `clone` | `auto` | Copy each tensor out of staging: `auto` copies only when the tensor would otherwise alias reused staging memory; `always`; `never` |
| `verify` | `size` | `none`; `size` checks each file's length against the manifest; `full` also checks the length when no ETag is pinned and adds a SHA-256 over every byte, forces host delivery, and is rejected with `delivery: gpu` |
| `capture_dir` | Unset | Used by `prepare`; leave unset when serving |
| `role` | `target` | `draft` for a speculative-decoding draft model; it selects the draft's own sharded artifacts and checkpoint filter |
| `socket` | `RACER_SOCKET` | Client socket path |

The settings `use_vmm`, `pattern`, `mode_retry_seconds`, and `sharded_state`
from earlier releases are gone, and so is `racer-model.json`.

The loader follows vLLM's own weight filters: expert-parallel
`local_expert_ids`, `secondary_weights`, name prefixes, and the encoder-only
and unused-weight filters. It reads the file list from the manifest instead of
probing for part files, and it does not read the full checkpoint again for a
speculative-decoding draft model.

## Verify the Load

After each load, vLLM logs one `racer: loaded ...` line with the role, rank,
file count, bytes, time, throughput, and the number of reads in each delivery
mode (`gpu`, `host`, `stream`). On the dataplane, compare before and after a load:

```bash
kubectl get --raw "/api/v1/namespaces/unbounded-system/pods/<dataplane-pod>:9090/proxy/metrics" \
  | grep -E 'racer_delivery_bytes_total|racer_gpu_read_requests_total|lookup_(hits|misses)_total|racer_peer_hits_total'
```

- `racer_delivery_bytes_total` should grow by about the model size per
  replica.
- `racer_gpu_read_requests_total{outcome="gpu"}` counts GPU deliveries.
- The `racer_{plaintext,ciphertext,pending,disk_index}_lookup_hits_total`
  and matching `_lookup_misses_total` series show which local tier answered
  each page lookup: the plaintext and ciphertext memory caches, pending disk
  writes, and the disk index. There is no peer lookup series;
  `racer_peer_hits_total` counts pages received from peers.

## Troubleshooting

**The load used host delivery.** Check the dataplane log on the GPU node:

```bash
kubectl -n unbounded-system logs <dataplane-pod> | grep 'stage=gpu state='
```

`state=enabled` means GPU mode is on. `state=disabled` gives the reason, and
`state=standby` means no GPU read has come in yet.
`state=starting attempt=<n>` marks each attempt to turn GPU mode on.
`state=start-failed attempt=<n> reason=<reason> retry_in=<s>s` explains why a
GPU read got 501: reads get 501 until the backoff expires, and the first GPU
read after `retry_in` starts the next attempt. `state=recovered attempt=<n>`
means an attempt after a failure succeeded. With `delivery: auto` the
loader falls back to host delivery and logs the reason once. Set
`delivery: gpu` to make the fallback an error.

**Decrypt cost.** Every page is sealed, so the GPU or CPU decrypts each one on
the way in. On large checkpoints that work is real, and it is the price of
leaving encryption on everywhere.

**No RDMA.** Without RDMA, pages from peers arrive over HTTP, which is slower
than the local disk. Run `racer-vllm warm` before a rollout so the cache is
already full and replicas read from peers instead of the origin.

**Capacity.** A model larger than a node's cache share is evicted while it
loads. Raise the cache's `spec.priority`, add capacity, or use raw NVMe
devices (`racer.unbounded-cloud.io/block-devices`). Check
`racer_effective_payload_bytes` and `/debug/hardware` on the dataplane. See
[Raw-device storage]({{< relref "reference/racer" >}}#raw-device-storage).

**The model changed at the origin.** The loader reads each file at the ETag
that `materialize` recorded in `racer-source.json`. If the origin changed
since, the load fails with a message that says so; restart the pod so
`materialize` runs again.

## Compatibility

- The plugin is pure Python. It needs `racer-client`, which needs nothing
  else, so it works with any vLLM image whose Python can import it. The
  optional `azure` extra is needed only by `import` and `prepare`.
- It targets vLLM 0.30 and 0.33. It hooks the loader interface that vLLM
  exposes for custom load formats; other versions may work but are not
  tested.

## Install Without an Image

The packages are not on PyPI. GitHub releases include their wheels and sdists
(`racer_client-*` and `racer_vllm-*`):

```bash
VERSION=vX.Y.Z
pip install --no-index \
  --find-links "https://github.com/Azure/unbounded/releases/expanded_assets/${VERSION}" \
  racer-vllm
```

`--no-index` keeps pip off PyPI, so a package there with the same name cannot
be installed by mistake. From a checkout:

```bash
pip install ./clients/python/racer './clients/python/racer-vllm[azure]'
```

`make racer-python-dist` builds wheels for both packages into
`tmp/racer-python-dist`.
