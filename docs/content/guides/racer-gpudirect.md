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

## How It Works

The client allocates GPU memory that can be shared, and passes it to the local
dataplane with the GPU's UUID. For each page, the dataplane uses the first
source that works:

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
| GPU decryption | A GPU with compute capability 8.0 (Ampere) or newer. `RACER_GPU_STAGING_BYTES` of at least 34 MiB |
| GPUDirect Storage | GPU decryption. GPUDirect Storage on the host (`nvidia-fs` or PCIe P2PDMA). Slabs on a local NVMe filesystem cuFile supports. Pages stored in record format v2 |
| GPUDirect RDMA | GPU decryption. Racer RDMA working between the two nodes. A NIC driver and GPU driver that support dma-buf. `RACER_GPU_LANDING_BYTES` of at least one page slot |

{{< callout type="note" >}}
GPU decryption and GPUDirect Storage have been run on 8x B200 hosts with
Ubuntu 24.04, kernel 6.8, driver 595, CUDA 12.9, and `nvidia-fs` 2.25.7, with
file slabs on ext4. PCIe P2PDMA, raw-device slabs with cuFile, GPUDirect RDMA,
and reads from more than one GPU per pod have not been run on hardware yet.
Run `make racer-gpu-test` on a GPU host to check yours. See
[Testing on GPUs]({{< relref "reference/racer" >}}#testing-on-gpus).
{{< /callout >}}

## Before You Start

- vLLM loads a model through Racer, as in the
  [vLLM guide]({{< relref "guides/racer-vllm" >}}).
- NVIDIA GPUs in `Default` compute mode. The dataplane skips GPUs in
  `EXCLUSIVE_PROCESS` or `PROHIBITED` mode, and reads to them use host memory.
- The NVIDIA open kernel modules on each host. Both GPUDirect paths need them.
  See [NVIDIA GPUs]({{< relref "reference/gpu/nvidia" >}}).

## 1. Prepare the Nodes

### GPUDirect Storage

GPUDirect Storage needs one of these on the host:

- **`nvidia-fs`.** NVIDIA's kernel module. For local NVMe it also needs the
  NVMe driver patches from MLNX_OFED or DOCA. On Ubuntu Azure VMs, add `--gds`
  when you install the driver:

  ```bash
  ./hack/agent/azure-gpu-vm/gpu-vm.sh setup-nvidia --size Standard_ND96isr_H100_v5 --gds
  ```

- **PCIe P2PDMA.** Uses the kernel's own NVMe driver, with no `nvidia-fs`.
  Needs Linux 6.2 or newer, cuFile 1.13 or newer, and x86_64. Turn it on in
  the dataplane with `RACER_GPU_PCI_P2PDMA` in step 2.

[GPUDirect Storage and GPUDirect RDMA]({{< relref "reference/gpu/nvidia" >}}#gpudirect-storage-and-gpudirect-rdma)
lists the versions, kernel options, and what the agent can and cannot
install.

Check `nvidia-fs` on the host:

```bash
lsmod | grep nvidia_fs
ls -la /dev/nvidia-fs*
```

Load `nvidia-fs` before the agent starts the machine. The agent passes
`/dev/nvidia-fs*` into the machine only when they exist at start. If you load
it later, restart the machine.

### Slab Storage

cuFile reads only from files on ext4 or xfs on local NVMe. Racer has tested
file slabs on ext4. Mount ext4 with an explicit `data=ordered`, or cuFile
rejects the files:

```bash
mkfs.ext4 /dev/nvme0n1   # erases the drive
mkdir -p /mnt/racer-slabs
mount -o data=ordered /dev/nvme0n1 /mnt/racer-slabs
```

The dataplane keeps file slabs in `/var/lib/racer/slabs` on the node. Each IO
worker gets one slab file of `RACER_SLAB_BYTES` (1 GiB by default). Raise it so
the slabs hold your models. On agent nodes, `/var/lib/racer/slabs` is inside
the machine. Bind the mount into it with `AdditionalHostMounts` in the agent
config:

```json
{
  "AdditionalHostMounts": [
    { "Source": "/mnt/racer-slabs", "Target": "/var/lib/racer/slabs" }
  ]
}
```

See [nspawn machines]({{< relref "reference/agent/nspawn" >}}).

Raw-device slabs, set with the `racer.unbounded-cloud.io/block-devices` Node
annotation, have not been tested with cuFile. NVIDIA lists only ext4 and xfs.
If cuFile rejects a device, the dataplane logs `cufile-unsupported` for it once
and reads it through host memory. Other devices keep using cuFile. See
[Raw-device storage]({{< relref "reference/racer" >}}#raw-device-storage).

### GPUDirect RDMA

This path is optional. First set up Racer RDMA between the nodes, as in
[RDMA]({{< relref "reference/racer" >}}#rdma). Both nodes need the same
nonempty Site and a shared rail.

Racer exports GPU memory as a dma-buf and registers it with
`ibv_reg_dmabuf_mr`. It does not use `nvidia_peermem`. This needs:

- The NVIDIA open kernel modules, R515 (CUDA 11.7) or newer.
- Linux 5.12 or newer.
- rdma-core 34 or newer in the image. The dataplane image has rdma-core 50.
- A NIC driver that supports dma-buf, such as `mlx5`.

## 2. Configure the Dataplane

The released `racer-dataplane` image has the GPU decrypt kernels, cuFile, and a
`cufile.json`. GPU mode turns on by itself on nodes with a usable GPU, so you
do not need to build an image or change any setting.

To tune GPU mode, set the GPU keys in `racer-dataplane-config`. For example:

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
| `RACER_ENABLE_GPU` | `auto` | `auto` turns GPU mode on when the dataplane finds a usable GPU. `true` makes startup fail when GPU mode cannot start, so a broken setup is not silent. `false` turns it off |
| `RACER_GPU_STAGING_BYTES` | 256 MiB | GPU memory per GPU for encrypted pages, in slots of 16 MiB plus 4 KiB. Two slots are for decryption, the rest for cuFile reads. When all slots are busy, pages go through host memory |
| `RACER_GPU_LANDING_BYTES` | 256 MiB | GPU memory per GPU that peers' NICs write into. It is split across the rails. `0` turns GPUDirect RDMA off |
| `RACER_GPU_PCI_P2PDMA` | `false` | `true` tells cuFile to use PCIe P2PDMA for NVMe instead of `nvidia-fs` |
| `RACER_DATAPLANE_NVIDIA` | `env` | Keep `env` or `cdi`. `off` drops the NVIDIA libraries and the `/run/udev` mount cuFile needs |

The operator checks these values. If one is wrong, it keeps the current
DaemonSet and reports `ConfigMap racer-dataplane-config is invalid` with the
reason.

Do not set `CUFILE_ALLOW_COMPAT_MODE`. Racer turns off cuFile's compatibility
mode so that a host without GPUDirect Storage logs `cufile-disabled` and uses
Racer's own host memory path. With compatibility mode on, cuFile hides the
problem behind a slower fallback.

## 3. Load the Model

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
- **More than one GPU.** The client sends the GPU's UUID with each read, so the
  dataplane finds the right GPU even when the pod numbers its GPUs differently
  from the host. With tensor parallelism, each rank reads every file of a
  regular checkpoint. Use a `sharded_state` checkpoint so each rank reads only
  its own files.
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

## 4. Check the Fast Paths

### vLLM Log

After each load, vLLM logs one line with the reads in each delivery mode:

```text
racer: loaded 4 files, 14.96 GiB in 2.34s (6.39 GiB/s) from racer://models/llama/Llama-3.1-8B-Instruct; reads by delivery mode: gpu=4 (14.96 GiB) host=0 (0.00 GiB) stream=0 (0.00 GiB)
```

`host` or `stream` reads mean the dataplane refused GPU reads. The client logs
the reason once:

```text
racer: gpu-read is not available from <socket> (status 501: ...); falling back to host mode until a retry in 300s or a dataplane restart
```

### Dataplane Logs

Read the `stage=gpu` lines from the dataplane on the GPU node:

```bash
kubectl -n unbounded-system logs <dataplane-pod> | grep 'stage=gpu'
```

| Line | Meaning |
|------|---------|
| `stage=gpu ordinal=0 uuid=... usable=true` | One line per GPU found |
| `stage=gpu state=enabled usable=N decrypt=M staging_slots=S pci_p2pdma=B` | GPU mode is on. `decrypt=0` means the CPU decrypts every page. `pci_p2pdma` shows the `RACER_GPU_PCI_P2PDMA` setting |
| `stage=gpu state=disabled reason=not-built` | A custom image was built with `RACER_NATIVE_CUDA=false` |
| `stage=gpu state=disabled reason=...` | No usable GPU, or the CUDA library failed to load |
| `stage=gpu ordinal=N decrypt=unavailable` | The GPU cannot run the decrypt kernels |
| `stage=gpu state=cufile-disabled reason=...` | cuFile is off until the dataplane restarts. Disk pages go through host memory |
| `stage=gpu state=cufile-unsupported dev=M:N reason=...` | cuFile rejected one device. Its pages go through host memory |
| `stage=gpu state=import-skipped reason=unknown-gpu-uuid uuid=...` | The client's GPU is not one the dataplane can use |
| `stage=gpu-landing rail=R ordinal=N state=enabled slots=S` | GPUDirect RDMA is on for this rail and GPU |
| `stage=gpu-landing ... state=failed reason=...` | The GPU memory could not be allocated or registered with the NIC, often for lack of dma-buf support |
| `stage=gpu-landing state=disabled reason=too-small` | `RACER_GPU_LANDING_BYTES` is below one slot |

### Metrics

Read the counters from the dataplane's diagnostics port:

```bash
kubectl get --raw "/api/v1/namespaces/unbounded-system/pods/<dataplane-pod>:9090/proxy/metrics" \
  | grep -E 'racer_gpu|racer_rdma_pages_landed'
```

| Metric | What to look for |
|--------|------------------|
| `racer_gpu_enabled` | 1 when GPU mode is on |
| `racer_gpu_cufile_enabled` | 1 while cuFile reads are on |
| `racer_gpu_read_requests_total{outcome}` | `gpu` should grow. `wrong_device` means the client's GPU is not one the dataplane can use |
| `racer_gpu_read_pages_total{decrypt}` | Pages written to GPU memory. `decrypt="gpu"` should grow |
| `racer_gpu_direct_disk_reads_total` | Pages read from disk with cuFile |
| `racer_gpu_staging_exhausted_total` | Disk reads that found no free staging slot |
| `racer_gpu_cufile_unsupported_total` | Devices cuFile rejected |
| `racer_rdma_pages_landed_total` | Pages peers wrote straight into GPU memory |
| `racer_gpu_landing_slots{rail,ordinal}` | Landing slots for each GPU on each rail |
| `racer_gpu_landing_exhausted_total{rail,ordinal}` | Landing requests that found no free slot |

See [GPU metrics]({{< relref "reference/racer" >}}#gpu-metrics) for the full list.

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

## Troubleshooting

| Symptom | Cause | Fix |
|---------|-------|-----|
| Dataplane fails to start with `RACER_ENABLE_GPU=true` | No usable GPU, or a custom image built without CUDA | Check the `stage=gpu` lines. Use the released `racer-dataplane` image. Set the GPUs to `Default` compute mode |
| `decrypt=0` | The GPU is older than compute capability 8.0 | Use a newer GPU |
| `cufile-disabled` at startup | GPUDirect Storage is missing on the host, or cuFile could not turn on P2PDMA | Load `nvidia-fs` or set up P2PDMA, then restart the machine. With `RACER_GPU_PCI_P2PDMA=true`, check that the host meets the P2PDMA needs |
| `cufile-disabled` later | A cuFile driver error turned cuFile off | Read `/var/log/cufile/cufile.log` in the dataplane pod. Fix the cause, then restart the dataplane |
| `cufile-unsupported` | The device or filesystem is not one cuFile supports, or ext4 is not mounted with `data=ordered` | Use file slabs on ext4 or xfs, as in [Slab Storage](#slab-storage) |
| `racer_gpu_direct_disk_reads_total` stays at 0 | Pages came from the origin, from peers, or from memory. Pages are stored in record format v1. All staging slots were busy | Load the model a second time. If `racer_gpu_staging_exhausted_total` grows, raise `RACER_GPU_STAGING_BYTES` |
| `racer_rdma_pages_landed_total` stays at 0 | Different Site, no shared rail, or no free landing slot | Check the `stage=gpu-landing` lines and the RDMA setup. If `racer_gpu_landing_exhausted_total` grows, raise `RACER_GPU_LANDING_BYTES` |
| vLLM log shows `host` reads | The GPU is not usable by the dataplane, or the client memory is not shareable | Read the client warning. Check the GPU compute mode. Keep `use_vmm` on |
