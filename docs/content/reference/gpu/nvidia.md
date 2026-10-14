---
title: "NVIDIA GPU Support"
weight: 1
description: "How the unbounded-agent discovers, configures, and exposes NVIDIA GPUs to Kubernetes workloads."
---

The unbounded-agent automatically detects NVIDIA GPUs on the host, forwards the
driver's userspace libraries into the nspawn container, generates a CDI
specification, and configures containerd so that GPU workloads can be scheduled on the node.

## Prerequisites

Before the agent can expose GPUs, the **host VM** must have:

1. **NVIDIA kernel drivers loaded.** `/dev/nvidia*` device nodes must exist.
2. **NVIDIA userspace libraries installed.** `ldconfig -p` must list
   `libcuda`, `libnvidia-ml`, and related libraries.
3. **Fabric Manager running** (host-managed NVSwitch systems only). H100, H200, and
   multi-GPU ND A100 systems require the `nvidia-fabricmanager` service.
   Without it, CUDA initialization fails with error 802
   (`SYSTEM_DRIVER_MISMATCH`).
   On GB200 NVL systems, Fabric Manager and NVLSM run on the NVLink switch
   trays instead; installing compute-host Fabric Manager does not enable IMEX.
4. **Persistence mode enabled.** `nvidia-smi -pm 1` keeps the driver loaded
   between GPU process invocations and avoids cold-start latency.

The GPU OCI images (`agent-ubuntu2404-nvidia`, `agent-ubuntu2604-nvidia`,
`agent-azlinux3-nvidia`) include the NVIDIA Container Toolkit (`nvidia-ctk`,
`nvidia-container-runtime`) but **not** the kernel driver or userspace
libraries. Those come from the host.

## How It Works

The agent's GPU support spans two bootstrap phases: **rootfs** (nspawn
configuration) and **node-start** (runtime setup inside the container). The
full sequence is:

![NVIDIA GPU architecture: Host devices and libraries bind-mounted into nspawn container running nvidia-ctk, containerd with nvidia runtime, and kubelet](../../../img/gpu-nvidia-host-container.svg)

1. **Device discovery.** Scans `/dev` for NVIDIA device nodes (per-GPU,
   control, UVM, capability, and DRI render nodes). If none are found, the
   entire GPU pipeline is skipped.
2. **Library discovery.** Queries `ldconfig -p` on the host and filters for
   NVIDIA libraries matching the host architecture. Deduplicates parent
   directories and assigns each a numbered mount index.
3. **nspawn configuration.** Writes device bind-mounts, read-only library
   bind-mounts at `/run/host-nvidia/<index>/`, and `DeviceAllow` cgroup
   entries into the nspawn and systemd service override configs.
4. **NVIDIA setup.** After the nspawn boots, creates symlinks in the
   container's multiarch library directory pointing into `/run/host-nvidia/`,
   then prepares `/run/nvidia/driver` for NVIDIA container tooling and device
   plugins. The driver root contains copied 64-bit NVIDIA and VDPAU libraries,
   `nvidia-smi` when present on the host, optional matching i386 libraries,
   and multiarch compatibility symlinks. The agent then runs `ldconfig` and
   `nvidia-ctk cdi generate` to produce the CDI spec at
   `/etc/cdi/nvidia.yaml`. Most CDI hooks are disabled to avoid interference
   with the nspawn environment.
5. **containerd runtime configuration.** Writes a containerd drop-in that
   enables CDI support and registers the `nvidia` runtime class.
6. **kubelet GPU advertisement.** Once kubelet starts and a user-deployed
   NVIDIA device plugin is running, GPUs are registered as `nvidia.com/gpu`
   extended resources on the node.

## Multi-node NVLink with IMEX

IMEX is disabled by default. Unbounded does not install or repair NVIDIA
drivers or IMEX userspace. Enable exposure only after the host-image pipeline
has applied and tested a qualified profile:

> The committed Hillsboro profile `v2026.10.14-1` is currently blocked and is
> documentation/validation input only. Do not enable it until `aks-flex-dev`
> publishes a passing authenticated result and a reviewed canonical digest.
> The accepted producer contract is in `aks-flex-dev` commit
> `84bae02adb8265dc8fb7d65b3c958048e22f7732` under
> `labs/gb200-hillsboro/gpu-operator-ownership/`.

```bash
kubectl unbounded machine config create gb200-dra \
  --nvidia-imex-mode=DRA \
  --nvidia-host-profile=hillsboro-gb200-gpu-operator-ready \
  --nvidia-host-profile-version=v2026.10.14-1 \
  --nvidia-host-profile-digest=sha256:<canonical-qualification-digest>
```

The corresponding API field is:

```yaml
spec:
  template:
    agent:
      nvidia:
        imex:
          mode: DRA
          hostProfile: hillsboro-gb200-gpu-operator-ready
          hostProfileVersion: v2026.10.14-1
          hostProfileDigest: sha256:<canonical-qualification-digest>
```

This is a strict, read-only capability gate. Goal-state resolution occurs
before normal host preparation and fails unless all of the following are true:

- `/etc/unbounded/host-capabilities/nvidia-gpu-operator.json` records the
  expected schema, profile and version, contract/result versions, immutable
  image identity, authenticity attestation, OS, architecture, exact kernel and
  kernel-devel, driver branch and exact driver/userspace/package versions,
  driver-matched IMEX package, runtime/CDI versions and readiness, GPU count,
  local NVLink result, host service ownership, device prerequisites, and
  versioned acceptance result.
- The artifact is RFC 8785 canonical UTF-8 JSON with no BOM or trailing
  newline. Its required sidecar contains the lowercase SHA-256 digest, two
  spaces, `nvidia-gpu-operator.json`, and one newline. The configured digest
  binds the evidence content but remains separate from the required verified
  authenticity attestation.
- The running OS, architecture, kernel, NVIDIA version, and GPU count match
  the qualification evidence, and the NVIDIA driver is branch 580 or later.
- The preinstalled driver-matched IMEX package provides
  `/usr/bin/nvidia-imex`, `/usr/bin/nvidia-imex-ctl`, and
  `nvidia-imex.service`.
- The loaded driver exposes
  `/proc/driver/nvidia/capabilities/fabric-imex-mgmt` and registers the
  `nvidia-caps-imex-channels` character-device class in `/proc/devices`.
- The host `nvidia-imex.service` is inactive and masked. NVIDIA DRA, not host
  systemd, owns the daemon and channel lifecycle in this mode.
- `/dev/nvidia-caps` and `/dev/nvidia-caps-imex-channels` are already
  available.
- The authenticated acceptance evidence and a read-only runtime probe both
  report four GPUs with healthy local NVLink. Multi-node NVLink remains a
  post-join ComputeDomain and workload validation.

The `aks-flex-dev` Hillsboro image pipeline owns package installation,
service state, acceptance testing, and the qualification marker. Unbounded
never uses the distro package manager for IMEX and never starts, stops,
enables, disables, or masks the host service. Missing or mismatched evidence
fails before Node registration, rather than joining as a CPU-only fallback.
See the
[GPU Operator and IMEX decision guide]({{< relref "reference/gpu/nvidia-gpu-operator" >}})
and NVIDIA's
[IMEX service installation guide](https://docs.nvidia.com/multi-node-nvlink-systems/imex-guide/gettingstarted.html)
for the package, daemon, configuration, and channel-device contract.

When enabled, the agent:

1. Validates the qualified image evidence and current runtime prerequisites.
2. Bind-mounts the preexisting `/dev/nvidia-caps` and
   `/dev/nvidia-caps-imex-channels` into nspawn and grants their named systemd
   device classes.
3. Labels the Node with the validated profile name/version and
   `nvidia.unbounded-cloud.io/imex-mode=DRA`, and records the full
   qualification digest in
   `nvidia.unbounded-cloud.io/host-profile-digest`.
4. Reports `Machine.status.conditions[type=NVIDIAIMEXPrepared]`.

The label and `NVIDIAIMEXPrepared=True, reason=HostQualified` condition mean
only that the prequalified host evidence is valid and the nspawn boundary is
exposed. They do **not** mean a ComputeDomain is ready or that an IMEX domain is
`UP`. Workloads must use a ready NVIDIA DRA ComputeDomain and its
ResourceClaims. Do not schedule MNNVL workloads from Node Ready, GPU count, or
the host-qualified label alone.

Qualification failure prevents Node registration. When bootstrap credentials
are already available, or during a repave, the Machine condition is set to
`False` with reason `HostQualificationFailed` and the exact mismatch in the
message. Attestation-based first boot can fail before cluster credentials
exist; in that case the local bootstrap error is authoritative and no Node is
registered.

Unbounded does not write `/etc/nvidia-imex/config.cfg`, choose channel IDs, or
assign node membership. The DRA controller derives membership from the
ComputeDomain, configures the daemon pods, and allocates the same workload
channel minor on every member.

NVIDIA DRA additionally requires Kubernetes 1.34.2 or later, a CDI-compatible
runtime, GPU Operator DRA operands, and matching IMEX packages on every domain
member. Follow NVIDIA's
[DRA installation guide](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/dra-intro-install.html)
and
[ComputeDomain security guidance](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/dra-cds.html#security).
Do not install the legacy `ClusterPolicy` device-plugin stack and the DRA
`GPUCluster` stack together. A standalone NVIDIA device-plugin deployment must
also be removed from DRA nodes before enabling `GPUCluster`; otherwise the same
GPUs can be advertised simultaneously through `nvidia.com/gpu` and DRA
`ResourceSlice` objects.

Unbounded validates and exposes the qualified host and nspawn boundary only.
It does not install GPU Operator, create `GPUCluster`, or deploy NVIDIA DRA
operands. The `aks-flex-dev` deployment definition owns those resources. For
example, the evaluated GPU Operator v26.7.2 and DRA driver v0.5.1
preinstalled-driver path uses:

```yaml
clusterPolicy:
  deployCR: false
gpuCluster:
  deployCR: true
driver:
  enabled: false
  nvidiaDriverCRD:
    enabled: true
    deployDefaultCR: false
draDriver:
  computeDomains:
    enabled: true
```

The resulting singleton `GPUCluster/gpu-cluster` causes GPU Operator to manage
the `nvidia-dra-driver-kubelet-plugin` DaemonSet,
`nvidia-dra-driver-controller` Deployment, ComputeDomain CRDs and controller,
ResourceClaim templates, workload-scoped IMEX channels, and the
`compute-domain-daemon.nvidia.com` and
`compute-domain-default-channel.nvidia.com` DeviceClasses. Keep that ownership
in GPU Operator; do not duplicate it in MachineConfiguration or workload
bootstrap scripts.

### Security model

DRA mode intentionally does not create or globally inject channel 0. IMEX
channel IDs are security-domain identifiers: the same workload must receive
the same minor number on every member, while unrelated tenants must receive
different channels. A world-accessible channel 0 collapses that isolation.
Namespace mutation rights over ComputeDomains and ResourceClaims are therefore
security-sensitive.

The legacy NVIDIA device plugin can globally inject only channel 0 using
`imex.channelIDs: [0]` and should set `imex.required: true`. That model applies
to every GPU workload and is not automatically configured by Unbounded.

### Disable and rollback

To disable exposure, remove `agent.nvidia.imex` from the
MachineConfiguration and repave the node. The regenerated nspawn configuration
omits the IMEX directory bind and named device permission. The host directory
is left in place because deleting it could race an allocated ComputeDomain.
The agent clears the `NVIDIAIMEXPrepared` condition and profile metadata. Host
packages and service state remain owned by the qualified image.
Delete workloads and ResourceClaims and allow the GPU Operator teardown hooks
to complete before repaving or applying a different host image.

## Prepared Driver Root

CDI generation is explicitly directed at the host-derived driver root:

```text
nvidia-ctk cdi generate \
  --driver-root=/run/nvidia/driver \
  --dev-root=/ \
  --output=/etc/cdi/nvidia.yaml
```

`--driver-root` makes `nvidia-ctk` discover userspace libraries, linker data,
and helper binaries from `/run/nvidia/driver` instead of the nspawn OCI
filesystem. This prevents the generated CDI specification from depending on
libraries in the image that may be absent or may not match the host kernel
driver. `--dev-root=/` is separate: NVIDIA device nodes are bind-mounted at
their normal paths in the nspawn machine, so device discovery remains rooted
at `/`.

The prepared root includes `etc/ld.so.cache` so `nvidia-ctk` can resolve the
copied libraries and their SONAME aliases. When the host provides
`nvidia-smi`, the agent also copies that host-matched binary to
`/run/nvidia/driver/usr/bin/nvidia-smi`. Because helper discovery is scoped by
`--driver-root`, this copy allows the CDI specification to expose a
version-compatible `nvidia-smi` to GPU containers. The binary is useful for
diagnostics and monitoring but is not required for compute-only CUDA access.

## Architecture Support

Library discovery is architecture-aware. The agent uses `runtime.GOARCH` to
select the correct multiarch library path and `ldconfig` filter:

| GOARCH | ldconfig tag | Library directory |
|---|---|---|
| `amd64` | `x86-64` | `/usr/lib/x86_64-linux-gnu` |
| `arm64` | `aarch64` | `/usr/lib/aarch64-linux-gnu` |

## GPU OCI Image

The GPU rootfs images (`images/agent-ubuntu2404-nvidia/Containerfile`,
`images/agent-ubuntu2604-nvidia/Containerfile`, and
`images/agent-azlinux3-nvidia/Containerfile`) extend the standard agent base
images with the NVIDIA Container Toolkit:

- `nvidia-container-runtime`: OCI runtime wrapper that injects GPU devices.
- `nvidia-ctk`: generates CDI specifications from discovered hardware.
- `libnvidia-container`: low-level library for GPU container setup.

The image does **not** include NVIDIA kernel drivers or userspace libraries.
Those are forwarded from the host at boot time via the bind-mount mechanism
described above.

## Troubleshooting

### CUDA error 802 (`SYSTEM_DRIVER_MISMATCH`)

This means Fabric Manager is not running. It is required for NVSwitch-based
multi-GPU systems (H100, H200, ND A100):

```bash
sudo systemctl status nvidia-fabricmanager
sudo systemctl start nvidia-fabricmanager
```

### `nvidia-ctk cdi generate` fails with `ERROR_LIBRARY_NOT_FOUND`

The host NVIDIA libraries are not visible inside the nspawn. Check that:

- The bind-mounts in the `.nspawn` config include the host library directory.
- The symlinks in the container's multiarch lib dir point to valid paths
  under `/run/host-nvidia/`.
- `ldconfig` was run after symlink creation.

### CDI spec has no device entries

Ensure the kernel driver is loaded (`lsmod | grep nvidia`) and that
`/dev/nvidia0` exists. If the driver is installed but not loaded, reboot the
host.

### CUDA error 800 and missing IMEX channels

CUDA error 800 (`operation not permitted`) with an MNNVL clique usually means
the workload cannot open an IMEX channel. Check:

```bash
test -c /dev/nvidia-caps-imex-channels/channel0
nvidia-imex-ctl -N
```

In DRA mode, inspect the ComputeDomain and ResourceClaim state rather than
starting `nvidia-imex.service` on the host. NVIDIA documents channel creation
and permissions in the
[IMEX channel guide](https://docs.nvidia.com/multi-node-nvlink-systems/imex-guide/imexchannels.html).
