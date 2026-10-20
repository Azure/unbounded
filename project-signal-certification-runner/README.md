# Project Signal certification runner

First-party, opt-in image build for the
[fifteen certification scripts](../project-signal-certification/README.md).
The target is **native `linux/amd64` with B200/SM100 CUDA support**. This recipe
does not use or derive from a MAI diagnostics image.

**Qualification boundary:** native build and registry publication are separate
from isolated post-build runtime validation, image approval, and Dapple/hardware
qualification. A software-only build check does not qualify a workload. Do not
use an ARM builder, emulation, shared-storage cleanup, or Dapple as a workaround.

## Locked inputs

[`dependencies.lock.json`](dependencies.lock.json) records the official CUDA
base digest, public upstream source commits/archive checksums, Python wheel
versions, and build epoch. [`requirements.lock`](requirements.lock) names the
complete Python wheel set by exact official URL and SHA-256; installation uses
`--no-index --require-hashes`, binary wheels only, and `pip check`. Wheel hashes
come from official publisher metadata; large wheel payloads have not been
downloaded or installed by the offline source checks.

| Component | Locked version/reference |
| --- | --- |
| Official CUDA devel / Ubuntu | `12.8.1-devel-ubuntu24.04`; single-platform amd64 manifest digest in the lock |
| Python / PyTorch | Snapshot-provided Python 3.12; `torch 2.8.0+cu128` and its exact 25-wheel closure |
| NCCL runtime / tests | `nvidia-nccl-cu12 2.27.3`; [nccl-tests v2.17.8](https://github.com/NVIDIA/nccl-tests/commit/81463c58d093dba403c28c28cd050ea948e9e1ba) |
| NVBandwidth | [v0.10.0](https://github.com/NVIDIA/nvbandwidth/commit/82fc4e8c6afa0babb8687793678f615b3b8d793e) |
| GPU Burn | [0d19c94dcb2e9b6858b7586a4c327674cd67113b](https://github.com/wilicc/gpu-burn/commit/0d19c94dcb2e9b6858b7586a4c327674cd67113b) |
| argparse, configured without further fetches | [v3.2](https://github.com/p-ranav/argparse/commit/3eda91b2e1ce7d569f84ba295507c4cd8fd96910) |

OS packages come only from the signed Ubuntu noble `main`/`universe` snapshot
`20261015T000000Z`, including its updates and security suites. APT still verifies
Ubuntu signatures and package hashes. The three signed Release files are also
checked against [`ubuntu-InRelease.sha256`](ubuntu-InRelease.sha256).
`Check-Valid-Until: no` permits this historical snapshot; it does not disable
signature verification. Mutable Ubuntu/CUDA package repositories are removed
from the build configuration.

[`script-contract.json`](script-contract.json) binds the recipe to all fifteen
regular `0755` script files at
`483c4704dc4bc265267a3b0037b2df3f833964e0`. This is the integrated script-source
ancestor, not the later commit containing this ledger. It also records the retained
requirements and architecture evidence hashes. Those receipts identify
requirements and the target architecture, **not old-image capabilities**.
The scripts are not copied into the image or modified by this build.

Pinned inputs make dependency selection auditable. Bit-for-bit image
reproducibility has not been measured; a local image ID is not a registry
manifest digest. Upstream licenses remain with the installed sources/wheels;
source license copies are retained under
`/usr/share/licenses/project-signal-certification-runner/`.

NVBandwidth's unmodified CMake calls `git describe --always --tags`. The fetcher
joins genuinely fetched, commit-verified Git objects to the SHA-256-verified
archive and rejects tracked-content differences. It does not invent a commit
or revision string. Its argparse dependency is fetched and hash-checked
before configuration; CMake is fully disconnected from additional downloads.
CUDA driver stubs are used only for linking, never in runtime loader paths.

## Verified script integration corrections

Eight script files differ from
`2fe83fb6da7ccf0bf1e3cde3e89832a4f02dce1b`; the other seven retain identical
bytes and executable modes. No workload substitution, policy, precision,
threshold, per-case duration, supervision or result-bound changes.

GPU Burn standalone execution now applies the same fail-closed quantitative
measurement validation as the four-component `gpu-burn` execution. A zero
process exit and qualitative `GPU N: OK` footer cannot pass unless one completed
record contains positive iterations and Gflop/s for every assigned GPU.
Component coordination, thresholds, inputs, parser rules and result files are
unchanged.

Hardware preflight ignores regular sysfs metadata files such as
`bonding_masters` when enumerating NIC directories; every physical NIC and its
declared speed policy remain required. fio's time-based write cases now receive
a separate, command-timeout-bounded full-file CRC32C verification pass, because
their write phase consumes the case runtime. Integrity errors remain typed
failures, and incomplete verification cannot pass.

BF16 and FP8 GEMM check incremental active CUDA allocator bytes against the
unchanged workload plan, using post-initialization, post-reset allocated and
reserved baselines. Active allocation above the plan is a typed `Fail`.
Incoherent allocator measurements remain `Inconclusive`. Reserved segment
peaks, including allocator rounding and cached slack, remain evidence but do
not represent workload bytes and do not determine the verdict. Native
precision, actual operators, correctness limits and timing remain unchanged.

NVBandwidth's two latency cases now accept their **exact**
[v0.10.0 descriptions](https://github.com/NVIDIA/nvbandwidth/blob/82fc4e8c6afa0babb8687793678f615b3b8d793e/testcases.cpp#L1042-L1084)
under the upstream `bandwidth_*` JSON fields. Unknown descriptions/units,
wrong cases, missing cells, conflicting metrics and invalid status still fail
closed; the legacy unit-description form remains supported.

Both NCCL scripts recognize the pinned `2.27.3` channel format:
decimal NVML device IDs, not hexadecimal PCI IDs. Every rank still has to
report the selected PCI identity, rank/count and CUDA ordinal through
[`Init COMPLETE`](https://github.com/NVIDIA/nccl/blob/72d2432094d6ae36abd6e511c3a16a2d052dbf94/src/init.cc#L1467-L1470).
The resulting rank-to-NVML mapping must be complete and unique; channel
endpoints must match it. Ring completeness, direction, topology and transport
checks are unchanged. Measurements add `channel_device_identifier` with
`nvml-index` or `packed-pci`. Other versions retain the pre-existing packed-PCI
contract; there is no numeric-base guessing or blanket compatibility fallback.
NVLink topology accepts ANSI SGR formatting but rejects unsupported control
sequences. Both collective parsers reconstruct a single exact 13-field row
split by NCCL INFO output; incomplete/extra fragments, mismatched parameters,
warnings, errors and unverified transport still cannot pass.

[`tests/test_vendor_contracts.py`](tests/test_vendor_contracts.py) reproduces
the original failures and checks these guards using source-derived synthetic
values plus byte-identical retained outputs from certification rows 07, 08,
11 and 12. Whitespace-sensitive stdout fixtures are losslessly gzip-wrapped;
asserted SHA-256 identities cover their original decompressed bytes. These offline
regressions do not qualify a new image build or a native rerun.
They do not reclassify retained live failures or blocked results.

## Workload contract

| Scripts / workloads | Image-provided software | Still required from the authorized caller/runtime |
| --- | --- | --- |
| Identity/full-node, including both MPI variants | Bash, core utilities, OpenMPI, SSH client and literal `/usr/sbin/sshd` | Assigned GPUs, NVIDIA runtime/driver and `nvidia-smi`; MPI peers, mounts and runtime SSH authorization |
| Four-component burn canary | Real compiled `gpu_burn` and `compare.fatbin`, `dd`, SHA-256 and kernel-log tools | Preserve GPU/CPU/memory/storage components, quantitative measurements, declared durations, `/work`/`/results`, telemetry permissions |
| Hardware preflight | Python stdlib, `lscpu`, `lsblk`, `ip` | Approved host proc/sysfs views and exact caller hardware/NIC policy |
| BF16 GEMM, FP8 GEMM, softmax determinism | CUDA-enabled PyTorch, native BF16 capability API, actual E4M3FN `_scaled_mm`, required Python dependencies | Native GPU/API correctness, memory/time bounds, identity and caller limits; imports are not workload validation |
| NVBandwidth | Compiled upstream `nvbandwidth` with its JSON interface | Assigned GPUs/topology, exact cases, sizes, samples and caller limits |
| Local NVLink and standalone backend NCCL | Compiled upstream `all_reduce_perf`, matching NCCL headers/runtime and CUDA | Exact local GPUs, sizes/data checks and explicit transport; Socket/bond0 is not an IB or multi-node qualification |
| CPU STREAM-like diagnostic | C11/OpenMP compiler/runtime, `numactl`, `lscpu` | Approved CPU/NUMA bindings and host memory, scratch permissions and caller policy |
| fio storage | Actual `fio` 3.x with CRC32C and `findmnt` | Exact approved ext4 scratch source/mount and private files; no raw-device substitution |
| Standalone health | Python decimal/JSON/POSIX support and `journalctl` | Current-boot kernel journal and GPU telemetry; explicit C2C/temperature modes and all conditional caller limits |

The image starts no services and contains no SSH host private keys or
`authorized_keys`. MPI workers generate fresh host keys at runtime using the
unchanged scripts. The image does not supply credentials, policy defaults,
thresholds, profiles, a publisher, or runtime integration.

The fixed `certification` account/group resolves UID/GID **65532**, with an
empty `/home/certification` and `/bin/bash` shell. It has no usable password,
SSH keys, or baked authorization. This fixes OpenSSH's `No user exists for uid
65532` failure when the runtime selects that identity, without changing the
image's default user, entrypoint, or working directory. The home remains
readable with a read-only root; any writable home/scratch must be supplied by
the caller. The frozen MPI worker roles still use `/root/.ssh/authorized_keys`,
`/run` and fresh host-key generation; the new account does **not** make those
roles compatible with a read-only, capability-free non-root runtime or grant
SSH access. Their runtime integration remains a separate gate.

NumPy, the `file` utility and clang are not requirements of the frozen fifteen
scripts or this image validator; the PyTorch import's optional NumPy warning is
not a missing required import. No convenience dependency is added for a probe.
CPU STREAM still requires a caller-selected real C11/OpenMP compiler; the
image retains GCC/cc and its existing compilation closure, as well as nvcc.
The retained Dapple probe's `+C` heredoc defect left OpenMP and subsequent
checks **unverified**, not failed.

Keep its working directory **`/opt/gpu-burn`**: the frozen burn argv loads the
real `compare.fatbin` relative to that directory. No image entrypoint rewrites
the selected script or its arguments. Do not replace the quantitative burn
with final `OK` lines or a different smoke test.

The CUDA/NCCL runtime loader is configured in the image, not by caller
`TOOL_LIBRARY_PATH` or `LD_LIBRARY_PATH`. Offline validation inspects
NVBandwidth/NCCL/burn resolution with both variables unset. Only host-injected
driver libraries may be absent in that no-GPU check. Actual NVIDIA runtime
injection and native functional execution remain separate gates.

## Validate and build

From the repository root:

```bash
python3 project-signal-certification-runner/build.py check
python3 -m unittest discover \
  -s project-signal-certification-runner/tests -v
```

These checks need only the local Python standard library. They verify frozen
script bytes/modes, build-input locks, safe source extraction, and fail-closed
build orchestration with controlled engine responses and source-derived MPI
version fixtures. Identity regressions use controlled NSS/SSH responses,
including the observed UID 65532/SSH exit 255 error; they are not native runtime
evidence. They do not build an image or run a workload.

Ubuntu `openmpi-bin 4.1.6-7ubuntu2` resolves `mpirun` to `orterun`.
The [upstream version branch](https://github.com/open-mpi/ompi/blob/v4.1.6/orte/orted/orted_submit.c#L275-L295)
prints `Open MPI` when invoked as `mpirun`, but `OpenRTE` as `orterun`.
The image check accepts either structured vendor/version header for the invoked
executable, never a vendor substring alone. Empty/malformed output, another MPI
vendor, or a nonzero exit still fails. The launcher exit code and raw stdout/
stderr are emitted before checking the banner, so later build failures retain
the native version evidence; timeouts retain their partial output.

Only after approving a **native Linux amd64 engine and its available disk
capacity**, select its existing Docker context or Podman connection and run:

```bash
python3 project-signal-certification-runner/build.py build \
  --engine docker \
  --output-directory "$HOME/certification-image-evidence/$(date -u +%Y%m%dT%H%M%SZ)"
```

The build requires network access to the pinned official inputs and substantial
space for CUDA/PyTorch wheels, layers and compiler outputs. No measured build
size or capacity guarantee is available yet. The driver checks the engine's
reported OS/architecture **before pulling/building**, rejects non-amd64 engines
and does not enable emulation. Establishing a genuinely native, authorized
builder remains the caller's responsibility. It uses only a `localhost/...`
tag and never pushes an image.

A successful build must also pass the embedded no-GPU software check in an
isolated container: no network, GPU request, added capabilities, writable root,
or privileged mode. It checks exact Python distributions, PyTorch APIs/SM100
build targets without CUDA initialization, POSIX supervision support, fio
version/CRC32C, compiler/OpenMP linkage, MPI/SSH/journal tools, default-loader
resolution, and the real burn kernel file. It executes no GPU, MPI, CPU-stress,
storage-stress, or certification-script workload.

The recipe additionally checks real passwd/group resolution, the home/shell,
and `ssh -V` as UID/GID 65532 with no supplementary groups, an empty capability
bounding set and no-new-privileges. That **in-build** check still has the
builder's writable root/network and is not an isolated hardened-runtime pass.
`build.py build` separately requires the same identity check with
`--user=65532:65532` and all the isolation flags above, after the full software
check. A software check as root alone cannot satisfy this identity gate.

The new evidence directory contains `build.log`, `offline-image-check.log`
(only if reached), `offline-identity-check.log` (only if reached), `image-id`
(only if built), and `result.json`. Failures return
nonzero and retain the failing stage; an unavailable engine is not reported as
a built or validated image. Installed OS package versions and resolved runtime
paths are emitted by the image check when it actually runs.

Publication needs a separately approved registry, identity and image review.
This repository adds no automatic build/publish pipeline and grants no live
execution, deployment, merge, or hardware-certification approval.
