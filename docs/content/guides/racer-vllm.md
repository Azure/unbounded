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

The CUDA image also installs NVIDIA's cuFile library (`libcufile-12-9`) for
GPUDirect Storage reads, because the NVIDIA container runtime injects only the
driver libraries. cuFile uses its packaged default configuration, except that
Racer turns off cuFile's compatibility mode: without GPUDirect Storage
(`nvidia-fs` or PCIe P2PDMA) on the host, disk reads use Racer's own host
memory path instead of cuFile's slower POSIX fallback. Set
`CUFILE_ALLOW_COMPAT_MODE=true` on the dataplane to keep cuFile's fallback.
cuFile reads the filesystem type from udev, so the dataplane mounts the host's
`/run/udev` read-only.

GPU mode is `auto` by default. To make the dataplane fail when it finds no GPU,
set it to `true`:

```bash
kubectl -n unbounded-system patch configmap racer-dataplane-config \
  --type merge -p '{"data":{"RACER_ENABLE_GPU":"true"}}'
```

The dataplane gets the NVIDIA driver libraries without requesting a GPU. This
is on by default (`RACER_DATAPLANE_NVIDIA: env` in `racer-dataplane-config`). See
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
add `racer.unbounded-cloud.io/client-containers: vllm`. The webhook mounts the
socket into init containers too, so `racer-vllm-fetch` can also run in an init
container that writes to the shared `emptyDir`.

## 3. Install racer-vllm

Install both Python packages into the vLLM image:

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
   `ModelChangedError`. Run `racer-vllm-fetch` again to pick up the new
   version.

After each load, vLLM's log shows one `racer: loaded ...` line with the file
count, bytes, time, throughput, and the number of reads in each delivery mode
(`gpu`, `host`, `stream`). When the dataplane refuses a mode, the client logs a
warning with the reason once, then retries the mode after
`mode_retry_seconds` (default 300) or when the dataplane restarts.

With tensor parallelism, each rank reads every weight file of a regular
checkpoint. Ranks on one node share the dataplane, so repeat reads come from
its cache, not the origin. To have each rank read only its own files, save a
vLLM `sharded_state` checkpoint and pass
`--model-loader-extra-config '{"sharded_state": true}'`.

See the `racer-vllm` README for loader options such as `staging_bytes`.

The first load fills the cache from the origin. Later loads on any node read
from the cache.

To read from disk and from peers straight into GPU memory, see
[GPUDirect Model Loading with Racer]({{< relref "guides/racer-gpudirect" >}}).
