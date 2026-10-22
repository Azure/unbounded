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

## 1. Turn On GPU Mode

Build the dataplane image with CUDA support and push it to the registry the
operator uses:

```bash
podman build --build-arg RACER_NATIVE_CUDA=true \
  -t <registry>/racer-dataplane:<tag> -f images/racer-dataplane/Containerfile .
```

GPU mode is `auto` by default. To make the dataplane fail when it finds no GPU,
set it to `true`:

```bash
kubectl -n unbounded-system patch configmap racer-dataplane-config \
  --type merge -p '{"data":{"RACER_ENABLE_GPU":"true"}}'
```

The dataplane gets the NVIDIA driver libraries without requesting a GPU. This
is on by default (`RACER_DATAPLANE_NVIDIA: env` in `racer-config`). See
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
      image: <your vLLM image with racer-vllm installed>
      command:
        - vllm
        - serve
        - racer://models/llama/Llama-3.1-8B-Instruct
        - --load-format
        - racer
      resources:
        limits:
          nvidia.com/gpu: 1
```

The webhook mounts `/run/racer/models/client` read-only into the container and
sets `RACER_SOCKET` to the socket in it. To limit the mount to some containers,
add `racer.unbounded-cloud.io/client-containers: vllm`.

## 3. Install racer-vllm

Install both Python packages into the vLLM image:

```bash
pip install ./clients/python/racer ./clients/python/racer-vllm
```

`racer-vllm` registers the `racer` load format with vLLM through a plugin entry
point. No other code change is needed.

## 4. Run

Pass `--load-format racer` and a model path of the form
`racer://<cache>/<bucket>/<prefix>`. The prefix is the directory that holds
`model.safetensors.index.json` and the safetensors files. See the `racer-vllm`
README for how it finds the model config and tokenizer.

The first load fills the cache from the origin. Later loads on any node read
from the cache.
