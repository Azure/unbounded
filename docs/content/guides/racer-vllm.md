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
images, at the same registry and tag. It is `vllm/vllm-openai:v0.25.1` with
both packages installed, so it needs no changes.

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
   the safetensors files.
2. `vllm serve <dir> --load-format racer` reads the weights through Racer, at
   the ETags in `racer-model.json`. If the model changed at the origin since
   the fetch, or changes during the load, the load fails with
   `ModelChangedError`. It also fails if the checkpoint layout changed, for
   example an index was added after a single-file fetch, because a weight file
   it would read has no ETag in `racer-model.json`. Run `racer-vllm-fetch`
   again to pick up the new version.

After each load, vLLM's log shows one `racer: loaded ...` line with the file
count, bytes, time, throughput, and the number of reads in each delivery mode
(`gpu`, `host`, `stream`). When the dataplane refuses a mode, the client logs a
warning with the reason once, then retries the mode after
`mode_retry_seconds` (default 300) or when the dataplane restarts.

With tensor parallelism, each rank reads every weight file of a regular
checkpoint. Ranks on one node share the dataplane, so repeat reads come from
its cache, not the origin. To have each rank read only its own files, save a
vLLM `sharded_state` checkpoint and pass
`--model-loader-extra-config '{"sharded_state": true}'`. `racer-vllm-fetch`
records every rank's files; pass `--pattern` if the checkpoint uses a custom
file name pattern.

See the `racer-vllm` README for loader options such as `staging_bytes`.

The first load fills the cache from the origin. Later loads on any node read
from the cache.
