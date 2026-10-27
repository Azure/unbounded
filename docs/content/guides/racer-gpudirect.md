---
title: "GPUDirect Model Loading with Racer"
weight: 12
description: "Set up nodes so Racer reads model weights from disk and from peers straight into GPU memory."
---

{{< callout type="warning" >}}
Racer is pre-release. Its APIs, flags, and on-disk formats may change without a
migration path. See the [Racer reference]({{< relref "reference/racer" >}}).
{{< /callout >}}

[Load vLLM Models from Racer]({{< relref "guides/racer-vllm" >}}) shows how to
load a model through Racer. This guide covers the node and dataplane setup that
lets those reads skip host memory, and how to check that they do.

{{< callout type="important" >}}
**TODO:** No automated test runs GPU reads on real GPUs. CI builds the CUDA
library without its GPU kernels and never loads it on a GPU. Validate every
path in this guide on your own hardware.
{{< /callout >}}

## How It Works

The client allocates GPU memory that can be shared, and passes it to the local
dataplane. For each page, the dataplane uses the first source that works:

1. **Plaintext held by another read.** Copied from host memory to the GPU.
2. **Local disk (GPUDirect Storage).** cuFile reads the encrypted page from
   disk into GPU memory. The GPU decrypts it.
3. **A peer over RDMA (GPUDirect RDMA).** The peer's NIC writes the encrypted
   page into GPU memory on this node. The GPU decrypts it.
4. **Anything else.** The page comes through host memory. Encrypted pages are
   still decrypted on the GPU. Pages fetched from the origin are decrypted on
   the CPU.

If the dataplane cannot use the client's GPU memory, the client falls back to
a host memory read, then to a plain stream. Loading still works, only slower.

Each fast path has its own needs:

| Path | Needs |
|------|-------|
| GPU decryption | A dataplane image built with CUDA. A GPU with compute capability 8.0 (Ampere) or newer. `RACER_GPU_STAGING_BYTES` of at least 34 MiB |
| GPUDirect Storage | GPU decryption. GPUDirect Storage on the host (`nvidia-fs` or PCIe P2PDMA). A disk and filesystem cuFile supports. Pages stored in record format v2 |
| GPUDirect RDMA | GPU decryption. Racer RDMA working between the two nodes. A NIC driver and GPU driver that support dma-buf. `RACER_GPU_LANDING_BYTES` of at least one page slot |

## Before You Start

- vLLM loads a model through Racer, as in the
  [vLLM guide]({{< relref "guides/racer-vllm" >}}).
- NVIDIA GPUs in `Default` compute mode. The dataplane skips GPUs in
  `EXCLUSIVE_PROCESS` or `PROHIBITED` mode, and reads to them use host memory.
- The NVIDIA driver is loaded on each host. See
  [NVIDIA GPUs]({{< relref "reference/gpu/nvidia" >}}).

## 1. Prepare the Nodes

### GPUDirect Storage

Install and load NVIDIA's `nvidia-fs` kernel module on each GPU host, or use a
kernel with PCIe P2PDMA support. Follow NVIDIA's GPUDirect Storage install
guide, and match the package to your driver. Then check the host:

```bash
lsmod | grep nvidia_fs
ls /dev/nvidia-fs*
```

NVIDIA's `gdscheck -p` tool also lists the storage types GPUDirect Storage
supports on the host.

The unbounded agent passes `/dev/nvidia-fs*` into the machine when the devices
exist. Neither the agent nor Racer installs them.

{{< callout type="important" >}}
**TODO:** The agent and the Azure Linux host image do not install or load
`nvidia-fs`, and no supported driver, kernel, or GPUDirect Storage versions are
documented. Racer ships no `cufile.json`, so cuFile uses its packaged default.
{{< /callout >}}

Raw devices are the simplest storage for cuFile. Select them with the Node
annotation, then restart the dataplane on that node:

```bash
kubectl annotate node <node> \
  racer.unbounded-cloud.io/block-devices='^nvme-<model>_<serial>$'
```

{{< callout type="warning" >}}
Racer overwrites the selected devices. See
[Raw-device storage]({{< relref "reference/racer" >}}#raw-device-storage).
{{< /callout >}}

File-backed slabs can also use cuFile if cuFile supports the host filesystem.
cuFile reads the filesystem type from the host's udev database.

{{< callout type="important" >}}
**TODO:** No slab filesystem has been tested with cuFile. List the supported
ones, or state that only raw devices are supported.
{{< /callout >}}

### GPUDirect RDMA

This path is optional. First set up Racer RDMA between the nodes, as in
[RDMA]({{< relref "reference/racer" >}}#rdma). Both nodes need the same
nonempty Site and a shared rail.

Racer exports GPU memory as a dma-buf and registers it with
`ibv_reg_dmabuf_mr`. It does not use `nvidia_peermem`. So the NIC driver,
rdma-core, the kernel, and the GPU driver must all support dma-buf. NVIDIA
supports dma-buf export with its open GPU kernel modules.

{{< callout type="important" >}}
**TODO:** No minimum driver, kernel, NIC, or firmware versions are documented
for GPUDirect RDMA.
{{< /callout >}}

## 2. Build the CUDA Dataplane Image

Build the dataplane with CUDA and push it under the name and tag the operator
uses (`<registry>/racer-dataplane:<tag>`):

```bash
podman build --build-arg RACER_NATIVE_CUDA=true \
  -t <registry>/racer-dataplane:<tag> -f images/racer-dataplane/Containerfile .
podman push <registry>/racer-dataplane:<tag>
```

This image adds the CUDA library with the GPU decrypt kernels, and NVIDIA's
cuFile library.

{{< callout type="important" >}}
**TODO:** Release and nightly builds do not publish a CUDA dataplane image, so
every user must build one.
{{< /callout >}}

## 3. Configure the Dataplane

Set the GPU keys in `racer-dataplane-config`:

```bash
kubectl -n unbounded-system patch configmap racer-dataplane-config --type merge -p '{
  "data": {
    "RACER_ENABLE_GPU": "true",
    "RACER_GPU_STAGING_BYTES": "1073741824",
    "RACER_GPU_LANDING_BYTES": "268435456"
  }
}'
```

The operator rolls the dataplane pods when this ConfigMap changes.

| Key | Default | Use |
|-----|---------|-----|
| `RACER_ENABLE_GPU` | `auto` | `true` makes startup fail when GPU mode cannot start. Use it so a broken setup is not silent |
| `RACER_GPU_STAGING_BYTES` | 256 MiB | GPU memory per GPU for encrypted pages, in slots of 16 MiB plus 4 KiB. Two slots are for decryption, the rest for cuFile reads. When all slots are busy, pages go through host memory |
| `RACER_GPU_LANDING_BYTES` | 256 MiB | GPU memory per GPU that peers' NICs write into. `0` turns GPUDirect RDMA off |
| `RACER_DATAPLANE_NVIDIA` | `env` | Keep `env` or `cdi`. `off` also removes the `/run/udev` mount, so cuFile cannot classify the slab devices |

The operator does not check these values. A bad value shows up only as a
dataplane startup failure.

Do not set `CUFILE_ALLOW_COMPAT_MODE`. Racer turns off cuFile's compatibility
mode so that a host without GPUDirect Storage logs `cufile-disabled` and uses
Racer's own host memory path. With compatibility mode on, cuFile hides the
problem behind a slower fallback.

## 4. Load the Model

Run vLLM as in the [vLLM guide]({{< relref "guides/racer-vllm" >}}). Keep these
points in mind:

- **Fill the cache first.** Pages fetched from the origin are decrypted on the
  CPU and never use cuFile. The first load fills the cache. Later loads use the
  fast paths. To fill the cache before vLLM starts, send a warm request for
  each file:

  ```python
  import racer

  client = racer.Client(cache="models")
  for name in files:  # the safetensors files of the model
      req = racer.object_request("llama", f"Llama-3.1-8B-Instruct/{name}")
      meta = client.stat(req, retain_seconds=3600)
      print(name, meta.retain_until_ms)  # None means Racer refused to warm it
  ```

  Warm stores each page on its primary owner, which may be another node. Only
  pages on the local disk can use cuFile. Pages on other nodes come over
  GPUDirect RDMA or through host memory. See
  [Warm and hard retention]({{< relref "reference/racer" >}}#warm-and-hard-retention).
- **Keep shareable GPU memory on.** The loader option `use_vmm` is `true` by
  default. If the loader cannot allocate shareable memory, it logs
  `racer: exportable GPU memory unavailable (...); using torch memory`, and
  every read uses host memory from then on.
- **Tune for fast disks.** A GPU read keeps its whole
  `RACER_RANGE_WINDOW_PAGES` window in flight, with cuFile where it can. Raise
  the window, raise `RACER_GPU_STAGING_BYTES` to hold every page in flight,
  and raise the loader's `concurrency`. Each IO worker's share of
  `RACER_PLAINTEXT_BYTES` and `RACER_CIPHERTEXT_BYTES` must still hold the
  window plus one page. On one NVMe drive, a window of 8, 1 GiB of staging,
  and `concurrency` of 8 loaded a 7B model in 2.34 seconds, against 2.44
  seconds with the defaults:

  ```bash
  vllm serve /models/llama --load-format racer \
    --model-loader-extra-config '{"concurrency": 8}'
  ```

{{< callout type="important" >}}
**TODO:** These gaps affect model loading:

- GPU reads have not been tested on a node with more than one GPU. The
  dataplane matches the client's memory to a GPU by device number. A pod that
  gets one GPU sees it as device 0, which may be a different number on the
  host. A mismatch falls back to host memory.
- `racer-vllm` does not split reads by tensor-parallel rank. Each vLLM worker
  reads every weight file.
- `racer-vllm` does not pin an object version across files. Each file is read
  at the version the origin has when the read starts.
- `racer-client` and `racer-vllm` are not published to PyPI, and there is no
  vLLM image with them installed.
{{< /callout >}}

## 5. Check the Fast Paths

### Dataplane Logs

Read the `stage=gpu` lines from the dataplane on the GPU node:

```bash
kubectl -n unbounded-system logs <dataplane-pod> | grep 'stage=gpu'
```

| Line | Meaning |
|------|---------|
| `stage=gpu ordinal=0 uuid=... usable=true` | One line per GPU found |
| `stage=gpu state=enabled usable=N decrypt=M staging_slots=S` | GPU mode is on. `decrypt=0` means the CPU decrypts every page |
| `stage=gpu state=disabled reason=not-built` | The image was built without CUDA |
| `stage=gpu state=disabled reason=...` | No usable GPU, or the CUDA library failed to load |
| `stage=gpu ordinal=N decrypt=unavailable` | The GPU cannot run the decrypt kernels |
| `stage=gpu state=cufile-disabled reason=...` | cuFile is off until the dataplane restarts. Disk pages go through host memory |
| `stage=gpu-landing rail=R ordinal=N state=enabled slots=S` | GPUDirect RDMA is on for this rail and GPU |
| `stage=gpu-landing ... state=failed reason=...` | The GPU memory could not be registered with the NIC, often for lack of dma-buf support |
| `stage=gpu-landing state=disabled reason=too-small` | `RACER_GPU_LANDING_BYTES` is below one slot |

### Metrics

Read the counters from the dataplane's diagnostics port:

```bash
kubectl get --raw "/api/v1/namespaces/unbounded-system/pods/<dataplane-pod>:9090/proxy/metrics" \
  | grep -E 'racer_gpu_direct|racer_rdma_pages_landed'
```

| Metric | Counts |
|--------|--------|
| `racer_gpu_direct_disk_reads_total` | Pages read from disk with cuFile |
| `racer_gpu_direct_disk_read_bytes_total` | Bytes read from disk with cuFile |
| `racer_rdma_pages_landed_total` | Pages peers wrote straight into GPU memory |

### Client

`read_into_gpu` returns the mode it used in `delivery`: `gpu`, `host`, or
`stream`:

```python
import racer
from racer import cuda

client = racer.Client(cache="models")
req = racer.object_request("llama", "Llama-3.1-8B-Instruct/model-00001-of-00004.safetensors")
meta = client.stat(req)
with cuda.GpuAllocation(meta.size) as alloc:
    print(client.read_into_gpu(req, alloc, if_match=meta.etag).delivery)
```

{{< callout type="important" >}}
**TODO:** These gaps make the fast paths hard to check:

- `racer-vllm` drops the delivery mode, and the client does not log a fallback.
  vLLM gives no sign of which mode it used.
- Only the three counters above are exported. Pages decrypted on the GPU,
  rejected pages, and staging slots that ran out are counted inside the
  dataplane but not exported. No metric shows whether cuFile is on.
- The client remembers a mode the dataplane does not support until the process
  exits. A dataplane that restarts with GPU mode fixed is not retried.
{{< /callout >}}

## Troubleshooting

| Symptom | Cause | Fix |
|---------|-------|-----|
| Dataplane fails to start with `RACER_ENABLE_GPU=true` | No usable GPU, or the image has no CUDA | Check the `stage=gpu` lines. Use the CUDA image. Set the GPUs to `Default` compute mode |
| `decrypt=0` | The image was built without the decrypt kernels, or the GPU is older than compute capability 8.0 | Rebuild with `RACER_NATIVE_CUDA=true`. Use a newer GPU |
| `cufile-disabled` at startup | GPUDirect Storage is missing on the host | Load `nvidia-fs` or set up P2PDMA, then restart the dataplane |
| `cufile-disabled` later | One cuFile driver or registration error turns cuFile off | Fix the cause, then restart the dataplane |
| `racer_gpu_direct_disk_reads_total` stays at 0 | Pages came from the origin, from peers, or from memory. Pages are stored in record format v1. All staging slots were busy | Load the model a second time. Raise `RACER_GPU_STAGING_BYTES` |
| `racer_rdma_pages_landed_total` stays at 0 | Different Site, no shared rail, no landing buffer on the read's GPU, or no free slot | Check the `stage=gpu-landing` lines and the RDMA setup |
| Client `delivery` is `host` | The GPU is not usable by the dataplane, or the client memory is not shareable | Check the GPU compute mode. Keep `use_vmm` on |

{{< callout type="important" >}}
**TODO:** GPUDirect RDMA has limits that can leave GPUs without it:

- Each rail gets at most one GPU's landing buffer, picked by NUMA node only.
  On a node with more GPUs than rails, the other GPUs never use it.
- Only the first peer asked for a page can write it into GPU memory.
- A page written into GPU memory is used by one read only. It is not cached.
- Landing buffers are allocated once and never resized or freed.
{{< /callout >}}
