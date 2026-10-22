# Ubuntu 24.04 GB200 Metalman host image

This ARM64-only image supplies `/disk/disk.img.gz` for the existing Metalman
netboot installer. It does not replace the installer, deploy an agent, modify a
Machine, initiate HostReplace, or reboot a host.

The image family provides five pinned experiment profiles:

| Image | Profile | Kernel and NVIDIA stack |
|---|---|---|
| `host-ubuntu2404-gb200` | `canonical-control` | Canonical 7.0 64K kernel and signed precompiled server-open 580.178.04 modules |
| `host-ubuntu2404-gb200-nvidia-580167` | `canonical-nvidia-580167` | Canonical control kernel with matched NVIDIA repository open DKMS driver, GSP, Fabric Manager, and NSCQ 580.167.08 |
| `host-ubuntu2404-gb200-agentbaker` | `agentbaker-driver` | Signed `linux-azure-nvidia` 6.14 and AgentBaker's open DKMS 580.159.04 driver |
| `host-ubuntu2404-gb200-agentbaker-doca` | `agentbaker-doca` | AgentBaker driver profile plus DOCA-OFED 3.1.0 |
| `host-ubuntu2404-gb200-agentbaker-doca-imex` | `agentbaker-doca-imex` | DOCA profile plus IMEX and AgentBaker's GB200 NVIDIA module parameters |

Every profile includes matching Fabric Manager and NSCQ packages. MANA
Ethernet (`mana`) and RDMA (`mana_ib`) must come from the selected kernel.
The AgentBaker kernel builds `mana` into the kernel and retains `mana_ib` as a
module; the Canonical control ships both as modules.
Versions fail closed if unavailable; there is no unpinned package or
different-kernel fallback. The profiles intentionally isolate cumulative stack
deltas rather than changing all AgentBaker components at once.

The 580.167.08 profile is a driver-only diagnostic relative to the Canonical
control: it retains the same Ubuntu release, 7.0.0-1019 64K kernel, MANA
drivers, boot path, and services. NVIDIA lists 580.167.08 with CUDA 13.0.2 in
the public DGX GB200 1.3.10 host software matrix. That DGX release is not an
OEM firmware authorization for this rack; the profile exists only to test
whether the later 580.178.04 Blackwell NVLink/GSP changes are involved.

The image does not install a custom device-node wait service. Physical GB200
boot evidence shows that Fabric Manager creates the NVSwitch control node and
that the UVM nodes can materialize during the first successful driver health
query. Host preflight and nspawn configuration generation must materialize and
validate these lazy nodes before taking the authoritative device-node snapshot;
otherwise a reboot can leave the physical host CUDA-ready while omitting UVM
from the nspawn device bindings.

The image contains a GPT disk with an EFI partition, signed Ubuntu ARM64
shim/GRUB binaries, and an ext4 root with at least 5 GiB additional space.
Metalman replaces `50-cloud-init.yaml` with its MAC-matched static netplan and
injects its NoCloud datasource. Machine IDs and SSH host keys are generated
at first boot. The image enables Fabric Manager for the local NVSwitch fabric.
Only the explicit IMEX profile enables IMEX. No profile installs GPU Operator,
DRA, or multi-node NVLSM.

The experiment images do not install the previous persistence-mode retry
service. Persistence state is not a qualification gate and repeated
`nvidia-smi -pm 1` attempts would confound idle NVLink testing.

## Build and qualification

Dispatch `.github/workflows/images.yaml` with one of the image names above.
Every profile requires `platforms=linux/arm64` and uses a native
`ubuntu-24.04-arm` runner.

Publication follows rootfs checks and a bounded QEMU ARM64 UEFI boot check.
The checks cover the 64K kernel, matching open NVIDIA module version/vermagic,
the Canonical control's module signatures, Fabric Manager/NSCQ and GB200
topology data, MANA built-in/module layout, ARM64 EFI binaries, cloud-init,
networkd and SSH. The workflow retains `image-inputs.json`, compressed-disk
SHA256 and the boot log.
The OCI is tagged by source commit; activation must use its immutable registry
digest.

QEMU has no GB200 or physical MANA device. Passing these checks does **not**
qualify GPU execution, physical MANA traffic/RDMA, NVLink, Secure Boot
enrollment, or the rack's fabric-service configuration. A separately
authorized one-host canary is required before any fleet rollout.
