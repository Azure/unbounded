---
title: "NVSwitch Host Image"
weight: 3
description: "The host-ubuntu2404-nvidia Metalman machine image for NVIDIA HGX systems with NVSwitch: contents, boot order, Secure Boot, GPUDirect Storage, build, and smoke tests."
---

`host-ubuntu2404-nvidia` is a Metalman machine image for bare-metal NVIDIA HGX
systems with NVSwitch, including Blackwell (NVLink 5) systems that need the
NVLink Subnet Manager (NVLSM). It is Ubuntu 24.04 for amd64 only.

It starts from the same Ubuntu cloud image release as `host-ubuntu2404` and
adds the GPU driver, Fabric Manager, and RDMA user space. The
`host-azurelinux3-nvidia` image has the GPU driver but no Fabric Manager, so it
does not work on NVSwitch systems.

## Contents

| Component | Package | Version |
|---|---|---|
| Kernel | `linux-generic` (Ubuntu 24.04 GA kernel, 6.8) | Newest when the image is built; held |
| GPU driver, open kernel modules | `nvidia-headless-open` | 595.71.05 |
| Fabric Manager | `nvidia-fabricmanager` | 595.71.05 |
| NVSwitch Configuration and Query library | `libnvidia-nscq` | 595.71.05 |
| NVSwitch Device Monitoring library | `libnvsdm` | 595.71.05 |
| NVLink Subnet Manager | `nvlsm` | 2025.10.12-1; held |
| RDMA | `rdma-core`, `ibverbs-providers`, `ibverbs-utils`, `infiniband-diags`, `perftest` | Ubuntu 24.04 |
| ConnectX drivers | `mlx5_core`, `mlx5_ib`, `ib_umad` from the Ubuntu kernel | Kernel version |

The driver packages come from NVIDIA's CUDA apt repository for Ubuntu 24.04.
NVLSM 2025.10.12 is the version that NVIDIA's `nvlink5` 595.71.05 package
requires. DKMS builds the GPU kernel modules, including `nvidia_peermem`,
for the image's kernel during the image build. The machine does not compile
anything on first boot.

The `nvidia-driver-pinning-595.71.05` package pins every driver, Fabric
Manager, NSCQ, and NVSDM package to 595.71.05, so `apt upgrade` cannot mix
versions. The kernel metapackages and `nvlsm` are held. To move to a newer
kernel or driver, build a new image and reinstall the machine.

The image does not include the CUDA toolkit, the NVIDIA Container Toolkit,
IMEX, `nvidia-fs`, or DOCA-OFED. Workload containers bring CUDA. The agent's
`agent-*-nvidia` rootfs images bring the container toolkit. IMEX is only for
multi-node NVLink domains, and its unit is masked.

The image keeps the Metalman contracts of `host-ubuntu2404`: `/disk/disk.img.gz`
is a GPT disk with an EFI system partition and an ext4 root file system, the
root file system has netplan and cloud-init with the NoCloud datasource, and
the ESP boots through shim at `EFI/BOOT/BOOTX64.EFI`.

## Boot order

1. `systemd-modules-load.service` loads `nvidia`, `nvidia_modeset`,
   `nvidia_uvm`, `nvidia_drm`, and `ib_umad`. udev loads `mlx5_core` and
   `mlx5_ib` for the ConnectX devices. The GPU driver is not in the initramfs.
2. `nvidia-persistenced.service` and `nvidia-persistence-mode.service` keep
   the driver initialized. The agent checks for `nvidia-persistence-mode.service`.
3. `nvidia-fabricmanager.service` starts after the GPU units and
   `network-online.target`. On NVLink 5 systems its start script waits for the
   NVLink management ports to come up, starts NVLSM, and then starts Fabric
   Manager. This can take several minutes. A drop-in at
   `/etc/systemd/system/nvidia-fabricmanager.service.d/10-unbounded.conf`
   adds the ordering, loads `ib_umad` and `mlx5_ib` before start, restarts the
   service on failure, and skips it on machines with no GPU bound to the
   driver.
4. cloud-init installs the unbounded-agent, which starts containerd and
   kubelet.

Until Fabric Manager finishes, CUDA calls fail with error 802
(`SYSTEM_DRIVER_MISMATCH`). The drop-in orders Fabric Manager before host
`containerd.service` and `kubelet.service`, which covers the `none` isolation
mode after a reboot. The agent itself does not wait for Fabric Manager. In the
`nspawn` and `container` modes, or on first boot, a CUDA program that starts
before Fabric Manager is ready gets error 802 and must be restarted.

## Secure Boot

shim, GRUB, and the kernel are signed by Canonical, so the image boots with
Secure Boot on. The NVIDIA kernel modules are not. DKMS signs them during the
image build with a key it creates for that build. The build deletes the
private key and keeps the certificate at
`/usr/share/unbounded/nvidia-dkms-signing.der`.

With Secure Boot on, the kernel refuses modules that are not signed by a
trusted key, and the GPUs do not come up. Do one of the following:

- Turn off Secure Boot in the firmware.
- Enroll the image's certificate. This needs the machine's console, for
  example the BMC's remote console:

  ```bash
  sudo mokutil --import /usr/share/unbounded/nvidia-dkms-signing.der
  sudo reboot
  # At the MokManager screen: Enroll MOK, Continue, Yes, enter the password.
  ```

  Each image build has its own key, so enroll again after reinstalling the
  machine with a different build.

Check the state:

```bash
mokutil --sb-state
modinfo -F signer nvidia
sudo dmesg | grep -iE 'lockdown|module verification|nvidia'
```

Ubuntu's archive has Canonical-signed NVIDIA modules
(`linux-modules-nvidia-595-server-open-*`), but they are a different 595
release than 595.71.05, so the image does not use them.

## GPUDirect Storage

GPUDirect Storage (cuFile) on local NVMe has two paths:

- **PCI P2PDMA (recommended).** Needs Linux 6.2 or later, the open kernel
  module, and cuFile 1.13 (CUDA 12.8) or later in the workload container. The
  image meets the host side and needs no `nvidia-fs`. cuFile uses P2PDMA only
  when asked: set `"use_pci_p2pdma": true` under `properties` in `cufile.json`,
  or for Racer set `RACER_GPU_PCI_P2PDMA=true` in `racer-settings`.
- **`nvidia-fs`.** Not installed. It needs `nvidia-fs-dkms` from the CUDA
  repository, which is unsigned like the GPU modules, and the NVMe patches from
  MLNX_OFED or DOCA for local NVMe.

For both paths:

- The NVMe drives must use ext4 or xfs.
- The IOMMU must be off or in pass-through mode. If it is on, add `iommu=pt`
  to `GRUB_CMDLINE_LINUX` in `/etc/default/grub`, run `sudo update-grub`, and
  reboot.
- Peer-to-peer transfers between a GPU and a drive under different PCIe root
  ports go through the CPU. The kernel allows this only for host bridges it
  knows. If cuFile falls back to compatibility mode, check
  `sudo dmesg | grep -i p2pdma`.

Check GPUDirect Storage from a CUDA container with `gdscheck -p`
(`/usr/local/cuda/gds/tools/gdscheck`).

## Build

```bash
docker buildx build --platform linux/amd64 \
  -f images/host-ubuntu2404-nvidia/Containerfile \
  -t ghcr.io/azure/host-ubuntu2404-nvidia:v1 --push .
```

Build arguments:

| Argument | Default | Notes |
|---|---|---|
| `KERNEL_PACKAGE` | `linux-generic` | `linux-generic-hwe-24.04` selects the HWE kernel. |
| `NVIDIA_DRIVER_VERSION` | `595.71.05` | Must have an `nvidia-driver-pinning-<version>` package in the CUDA repository. |
| `NVLSM_VERSION` | `2025.10.12-1` | Use the version that `nvlink5` requires for the driver version. |

The build checks the disk image with
`images/host-ubuntu2404-nvidia/assets/validate.sh` and fails if a check fails.
It checks the boot files on the ESP, the kernel and initramfs, the GPU and
RDMA kernel modules for the image's kernel, the Fabric Manager, NVLSM, and
RDMA binaries, the enabled units, the cloud-init and netplan files, and that
no DKMS private key, SSH host key, or machine ID is in the image. To run it on
a built image:

```bash
docker buildx build --platform linux/amd64 \
  -f images/host-ubuntu2404-nvidia/Containerfile \
  --output type=local,dest=out .
sh images/host-ubuntu2404-nvidia/assets/validate.sh out/disk/disk.img.gz
```

It needs `sfdisk`, `debugfs` (e2fsprogs), `mtools`, and `pigz` or `gzip`.

## Publish

GitHub Actions publishes the image to `ghcr.io/azure/host-ubuntu2404-nvidia`
for `linux/amd64` only:

- A tag `images/host-ubuntu2404-nvidia/<version>` publishes
  `ghcr.io/azure/host-ubuntu2404-nvidia:<version>`:

  ```bash
  git tag images/host-ubuntu2404-nvidia/v1.0.0
  git push origin images/host-ubuntu2404-nvidia/v1.0.0
  ```

- Running **Build Container Images** with `image=host-ubuntu2404-nvidia`
  publishes `ghcr.io/azure/host-ubuntu2404-nvidia:<commit-sha>`.
- Each project release publishes `ghcr.io/azure/host-ubuntu2404-nvidia:<release tag>`.

Use it in a Machine:

```yaml
spec:
  host:
    netboot:
      image: ghcr.io/azure/host-ubuntu2404-nvidia:v1.0.0
      architecture: amd64
```

## Smoke tests

Run these on the machine after it installs and joins the cluster.

### Driver

```bash
nvidia-smi
modinfo -F version nvidia          # 595.71.05
nvidia-smi --query-gpu=persistence_mode --format=csv
systemctl status nvidia-persistence-mode nvidia-persistenced
```

### NVSwitch and Fabric Manager

```bash
systemctl status nvidia-fabricmanager
journalctl -b -u nvidia-fabricmanager
nvidia-smi -q | grep -A 3 -i fabric   # State: Completed, Status: Success
nvidia-smi nvlink --status            # every link reports a speed
nvidia-smi topo -m                    # NV<n> between every GPU pair
```

If the service failed, its journal names the step. On NVLink 5 systems,
check that `ib_umad` is loaded (`lsmod | grep ib_umad`) and that `ibstat`
lists the NVLink management ports as `LinkUp`.

### CUDA

Run a pod that uses every GPU on the node and measures NVLink bandwidth.
Driver 595 supports CUDA 13.2 and earlier.

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: nvlink-smoke
spec:
  restartPolicy: Never
  nodeName: <node>
  containers:
  - name: cuda
    image: nvcr.io/nvidia/cuda:12.8.1-devel-ubuntu24.04
    command: ["bash", "-c"]
    args:
    - |
      set -e
      nvidia-smi
      apt-get update && apt-get install -y --no-install-recommends git cmake libboost-program-options-dev
      git clone --depth 1 https://github.com/NVIDIA/nvbandwidth.git /nvbandwidth
      cd /nvbandwidth && cmake . && make -j
      ./nvbandwidth -t device_to_device_memcpy_read_ce
    resources:
      limits:
        nvidia.com/gpu: 8
```

```bash
kubectl apply -f nvlink-smoke.yaml
kubectl logs -f nvlink-smoke
```

A CUDA error 802 means Fabric Manager is not ready. Low device-to-device
bandwidth means the GPUs are talking over PCIe instead of NVLink.

### RDMA

```bash
ibv_devices
ibv_devinfo | grep -E 'hca_id|state|link_layer'
ibstat
rdma link
```

Test bandwidth between two machines with `perftest`:

```bash
# On machine A:
ib_write_bw -d mlx5_0 --report_gbits
# On machine B:
ib_write_bw -d mlx5_0 --report_gbits <machine A address>
```

Ubuntu's `perftest` has no CUDA support, so it does not test GPUDirect RDMA.
Use `nccl-tests` across two nodes with `NCCL_DEBUG=INFO` and check that the
log shows `GDRDMA`.
