# Project Signal certification scripts

This directory contains hardware-check scripts for GPU nodes. The checks cover
GPU identity and health, GPU computation, local GPU communication, CPU/memory
bandwidth, and scratch-disk I/O.

[Project Signal](https://github.com/Azure/unbounded) runs these checks as workflow
steps and collects their results. Each script supplies the test logic; Project
Signal supplies the scheduling, workflow execution, result collection, and
cleanup. This is a script library, not an installer or a ready-to-run test suite.

## How the scripts are used

1. **Choose a check.** Pick a script from the catalog below and supply its required
   inputs, such as expected hardware, duration, and pass/fail limits.
2. **Make the script available.** `make signal-manifests` verifies each script
   against the locked contract and renders the 14 substantive entries into an
   immutable Kubernetes ConfigMap.
3. **Select it in Project Signal.** A Profile or Run names the ConfigMap entry,
   the matching workflow, and an approved runner image containing the tools the
   script needs. Project Signal saves a fixed copy of the selected script for
   the Run and executes that copy.
4. **Collect the result.** Diagnostic steps write a verdict, reason, and
   measurements under `/results`; Project Signal collects and combines them.

`aks-flex-dev` maintains the scripts and the separate
[first-party runner image definition](../project-signal-certification-runner/README.md).
[Azure/unbounded](https://github.com/Azure/unbounded) maintains Project Signal
itself. The image definition is not a built or approved image. Image publication
and approval, run profiles, publishing, and deployment are separate gates.

All 14 substantive scripts are connected to upstream WorkflowTemplates and
example Profiles. Check the [integration status](#upstream-integration-status)
before configuring a run.
The [consumption details](#consumption) below explain the exact ConfigMap fields
and script-copy behavior.

## Script catalog

The first five scripts use the identity, full-node, and burn workflow families.
The remaining ten use the one-script WorkflowTemplate with explicit
hardware-specific policy.

`R` in the table means the three result files: `/results/verdict`,
`/results/reason`, and `/results/measurements.txt`.
The ten new diagnostics also keep separate audit logs, limit measurements to
**16 KiB**, and support `--describe` to list their tools, inputs, and timing
without running a check. The original five do **not** support that no-work flag.
"Existing" describes an upstream source interface, not an installed script.
"Integrated" means the repository has an explicit Workflow/Profile surface. It
does not mean the runner image, policy, cluster deployment, or hardware result
has been approved.

| Entrypoint in `scripts/` | Intended Argo step/test boundary | Runtime tools/devices | Explicit inputs | Outputs | Consumption |
| --- | --- | --- | --- | --- | --- |
| [`gpu-identity-example.sh`](scripts/gpu-identity-example.sh) | Existing `gpu-identity` identity/count example | Bash, `nvidia-smi`, assigned NVIDIA GPUs | Expected GPU product/count | R; termination summary | Existing; identity/count only |
| [`gpu-identity-mpi.sh`](scripts/gpu-identity-mpi.sh) | Existing `gpu-identity` + `mpi-backend` | Identity tools, Open MPI, SSH client/server | Identity guards; role; positive GPU count; script mode | R for stage; MPI role exit status | Existing script-mode hooks |
| [`gpu-full-node.sh`](scripts/gpu-full-node.sh) | Existing stage body for `preflight`, `gpu-health`, `health-after` | Bash, `nvidia-smi`, assigned GPUs | GPU product/count, observed capacity/allocatable, stage name | R; stage events | Existing baseline/inline path |
| [`gpu-full-node-mpi.sh`](scripts/gpu-full-node-mpi.sh) | Existing full-node stages + `mpi-rank-access` | Full-node tools, Open MPI, SSH client/server | Full-node guards; role; positive GPU count; script mode | R for stage; local-rank query/role status | Existing script-mode hooks |
| [`gpu-burn-canary.sh`](scripts/gpu-burn-canary.sh) | Existing coupled `gpu-burn`, `cpu-stress`, `memory-integrity`, `storage-integrity` | Bash, GPU telemetry, kernel log access, runtime-provided `gpu_burn`, host tools, `/work` | Existing GPU/capacity guards, burn duration/sample interval, power tolerance, CPU workers, memory/storage sizes, component | R; component verdict/reason/measurements; stage events | Existing four-container protocol; standalone and component execution require complete quantitative GPU measurements |
| [`hardware-preflight.sh`](scripts/hardware-preflight.sh) | Separate hardware inventory task | Python stdlib, `nvidia-smi`, `lscpu`, `ip`, `lsblk`, Linux proc/sysfs | Exact hardware policy; each physical NIC has explicit minimum, expected-unavailable or inventory-only speed policy; snapshot duration 0 | R + audit; unevaluated speed health is explicit | Integrated; host-view and unavailable-speed evidence required |
| [`gpu-gemm-bf16.sh`](scripts/gpu-gemm-bf16.sh) | Separate BF16 GEMM task | CUDA-capable PyTorch and assigned GPUs | GPU identity/selection, matrix dimensions, memory budget, duration/iterations, correctness tolerances, optional throughput floor | R + correctness/throughput audit | Integrated; explicit site policy required |
| [`gpu-gemm-fp8.sh`](scripts/gpu-gemm-fp8.sh) | Separate FP8 GEMM task | PyTorch with actual E4M3FN `_scaled_mm` support and capable GPUs | GPU identity/selection, matrix/memory/time bounds, tolerances, optional throughput floor; fixed unit scales/BF16 output | R + correctness/throughput audit | Integrated; explicit site policy required |
| [`softmax-determinism.sh`](scripts/softmax-determinism.sh) | Separate repeated identical-input softmax task | CUDA-capable PyTorch and all assigned GPUs | GPU identity/selection, declared dtypes/shapes, seed, duration/iterations, memory budget | R + per-GPU/dtype determinism evidence | Integrated; explicit site policy required |
| [`nvbandwidth.sh`](scripts/nvbandwidth.sh) | Separate declared host/device/local bandwidth and latency task | Runtime-provided `nvbandwidth`, GPU inventory tools, assigned GPUs | Explicit cases, sizes/iterations/time, GPU guards, optional per-case limits | R + exact-case command evidence | Integrated; explicit site policy required |
| [`nccl-nvlink.sh`](scripts/nccl-nvlink.sh) | Separate local NVLink all-reduce task | Runtime-provided `all_reduce_perf`, NCCL/CUDA, declared local GPU topology | Exact all-reduce sizes/iterations/data checks, GPU selection, explicit local transport, optional per-size floors | R + correctness/transport evidence | Integrated; explicit site policy required |
| [`nccl-backend.sh`](scripts/nccl-backend.sh) | Separate explicitly configured backend all-reduce task | `all_reduce_perf`, NCCL/CUDA, declared local GPUs and IB/Socket interface | Exact sizes/iterations/warmup, selected GPUs, explicit backend/interface/HCA ports, optional per-size floors | R + correctness/backend evidence | Integrated; single host/channel, not multi-node proof |
| [`cpu-stream.sh`](scripts/cpu-stream.sh) | Separate CPU/NUMA memory bandwidth task | C/OpenMP compiler, `numactl`, `lscpu`, host memory | Explicit CPU/node bindings and identity, array/memory budget, alpha/tolerances, duration/minimum iterations, optional operation floors | R + copy/scale/add/triad evidence | Integrated; own STREAM-like source, not external STREAM |
| [`fio-storage.sh`](scripts/fio-storage.sh) | Separate sequential/random read/write task | `fio` 3.x, `findmnt`, approved mounted scratch filesystem | Exact mount/source, scratch/file size, block/jobs/depth/engine/direct/seed, per-case duration, optional case limits | R + fio JSON/CRC32C evidence | Integrated; private approved scratch files only |
| [`gpu-health-monitor.sh`](scripts/gpu-health-monitor.sh) | Separate standalone health snapshot/monitor task | `nvidia-smi` NVLink reads, C2C reads or per-GPU `-q` mode evidence, readable current-boot kernel journal | Snapshot/monitor mode, explicit `GPU_HEALTH_C2C_MODE=required\|disabled` and `GPU_HEALTH_TEMPERATURE_MODE=enforce\|observe`, duration/interval, GPU guards, conditional caller-supplied limits, freshness/window, exact link IDs/counter labels | R + Xid/ECC/link/clock/power, exact temperature observations and explicit temperature/C2C policy | Integrated; observe mode is functional validation, not temperature health certification |

## Upstream integration status

This repository integrates the prior workflow concepts from
[`Azure/unbounded@282603184ddcafed12911ed0a14b091702eb3bf4`](https://github.com/Azure/unbounded/commit/282603184ddcafed12911ed0a14b091702eb3bf4)
without removing the current recovery receipt gate. The substantive ConfigMap
contains exactly 14 scripts, excluding the public identity example. Each
CertificationRun still snapshots and executes one selected script, while the
full-node and MPI scripts retain their real multi-stage or child-resource
boundaries.

The checked-in Profiles intentionally contain replacement markers rather than
universal hardware or performance policy. Deployment acceptance, a native
digest-pinned runner, Dapple execution authorization, and a valid signed
recovery receipt remain separate requirements.

Offline/stub checks validate script behavior, not hardware. They do not establish
hardware qualification, hhdiag compatibility, or Dapple equivalence.

## Consumption

An authorized publisher reads a reviewed commit of this library and creates a
ConfigMap in the controller's configured `--script-namespace`, putting each
script's exact text, including newlines, in `data[<script-filename>]`. Publishing,
namespace/RBAC configuration, and deployment are separate responsibilities.

Profiles/Runs select `spec.script.name` and `spec.script.key`, the matching
`spec.workflow.name`, and an approved digest-pinned `spec.runnerImage`
(`image@sha256:<digest>`). Digest syntax alone does not establish availability or
approval. No profile examples or image digests are supplied here.

Admission snapshots **only the selected decoded `data[key]` and its SHA-256**
into a Run-owned immutable ConfigMap. Other keys, helper scripts, source labels,
and source metadata are not implicitly copied. The stage and MPI pods read that
snapshot; both MPI Launcher and Worker mount the same snapshot read-only at
`/scripts`. Each selected key must therefore be self-contained, with dependencies
already present in its approved runner image.

## New diagnostic contract

Inspect a new entrypoint before configuring a run, without loading CUDA or
starting any workload:

```bash
/bin/bash project-signal-certification/scripts/hardware-preflight.sh --describe
```

The ten new scripts accept no normal positional arguments. They require explicit
expected hardware/selection, duration or iteration policy, scratch location, and
command timeout. Optional performance floors are checked only when supplied;
descriptions specify where an explicit empty limit map is required. No GB200
counts, device names, bandwidth floors, or thermal/power limits are universal
defaults. JSON schemas reject unknown policy fields.
Required runtime dependencies are supplied separately, never downloaded here.
The new scripts require Python stdlib and POSIX `waitid`/`WNOWAIT` supervision:
owned leaders stay unreaped until group termination is complete, preventing
signals to recycled process-group IDs. API readiness is checked before launch;
inherited `SIGCHLD` auto-reaping cannot bypass that pin. Descriptions document
the precise per-script names and units.

The normal result location is `/results`; `PROJECT_SIGNAL_RESULTS_DIR` can
redirect **new-script** outputs for isolated testing. A writable, non-symlink
directory is required. Each script creates only a private temporary child of the
explicit `SCRATCH_ROOT`, stops its owned processes on timeout/interruption, and
removes its own scratch while preserving audit evidence. `fio-storage.sh` checks
the approved mount, initializes and verifies its own files, and never accepts a
raw-device target. Each time-based write case is followed by a full-file CRC32C
verification pass bounded by `COMMAND_TIMEOUT_SECONDS`; the declared write-case
duration is unchanged. No script publishes environment variables or credentials.

Measured correctness or configured-limit violations are a typed `Fail` with
exit status 0; missing tools/inputs/devices, unsupported formats, lost
observability, and infrastructure errors are `Inconclusive` with nonzero status.
The new scripts emit `Pass`, `Fail`, or `Inconclusive`, rather than manufacturing
`Suspect` or substituting a weak test when a dependency is absent. Kernel/memory
budget failures are not hardware passes. Audit logs are separate from bounded
measurements; loss or overflow of required evidence cannot silently pass.

GPU GEMM/softmax use visible CUDA ordinals in `GPU_DEVICES`, inheriting
`CUDA_VISIBLE_DEVICES`. Each sweep covers every assigned GPU (and each declared
softmax dtype) **sequentially**, not concurrent multi-GPU saturation. `ITERATIONS`
caps complete sweeps; `DURATION_SECONDS` bounds when another sweep may start,
not a minimum stress duration. The mandatory first/final sweep may overrun that
window; `COMMAND_TIMEOUT_SECONDS` bounds the whole worker including setup and
CPU reference calculation. Softmax cannot Pass without two complete sweeps and
bit-identical same-dtype outputs across devices and repeats.

BF16 requires native CUDA BF16; FP8 executes E4M3FN `_scaled_mm` with unit FP32
scales and BF16 output, never cast-to-BF16 GEMM as a substitute. Explicit
correctness tolerances compare against CPU FP64 references of the actual
quantized inputs and do not relax softmax determinism. Operator timing includes
cold samples, without hidden warmup or discarded results; optional TFLOPS floors
apply to every device/sample. GEMM allocator checks charge only peak active allocation above the
post-initialization, post-reset baseline to the workload plan. An active
allocation overrun is a typed `Fail`; inability to obtain coherent allocator
measurements remains `Inconclusive`. Absolute allocated and reserved peaks,
baselines and increments remain in the audit log. Reserved segments are
evidence only because allocator segment rounding and cached slack are not
workload bytes.
CPU STREAM compiles readable first-party source
embedded in the selected shell key.

NVBandwidth/NCCL instead index `GPU_DEVICES` into the full `nvidia-smi` inventory
sorted by PCI bus ID, then construct UUID-based child CUDA visibility; they do
not inherit a CUDA visibility mask. They run separately provided standard tools
with explicit cases/collective settings and strict output/path evidence.
Their `DURATION_SECONDS` is a maximum benchmark-phase budget, not sustained
stress time; the shared `COMMAND_TIMEOUT_SECONDS` also includes preflight.
Both NCCL scripts use one host/process/thread/channel, float32/sum and fixed
Ring/Simple. Backend `IB` or `Socket` and exact interfaces/ports must be explicit;
fallback or incomplete observed paths cannot Pass. This is not multi-node,
external-wire, full-fabric/peak-bandwidth, or unconditional GDR proof.
NVBandwidth's host/device latency case measures SM host-memory access, not
separate bidirectional DMA latencies. Approved native versions/formats still
need target validation.

Hardware preflight keeps **every physical NIC** in its exact inventory, including
interfaces whose speed is unavailable. Each `EXPECTED_HARDWARE_JSON.nics[]` uses
exactly one speed representation:

| Speed policy | Required fields | Meaning |
| --- | --- | --- |
| Legacy minimum | `speed_min_mbps` (positive integer) | Retains the existing minimum-speed check |
| Tagged minimum | `speed: {kind: minimum, min_mbps: ...}` | Same check with an explicit discriminator |
| Expected unavailable | `speed: {kind: unavailable, evidence_sha256: ..., approved_unavailability_class: ...}` | Requires the exact declared native error class and observation hash |
| Inventory only | `speed: {kind: inventory_only, reason: ..., approval_reference: ...}` | Retains the observation but explicitly does **not** evaluate speed health |

The supported unavailable selectors are `errno:EINVAL` and `errno:EOPNOTSUPP`.
They are technical matching rules, **not site approvals**. The evidence hash is
SHA-256 of the recorded `speed_observation` serialized with Python
`json.dumps(..., sort_keys=True, separators=(',', ':'), ensure_ascii=True)`;
it includes the exact sysfs source path. Unknown formats, missing data,
permission/tool errors and unapproved roles are not expected unavailability.
Unexpected numeric availability or a changed declared observation fails the
expectation. Minimum failures remain typed `Fail`; unreadable/unclassified
evidence is `Inconclusive`. Inventory-only success is not a speed-health Pass.
No unavailable interface may be omitted to make the inventory pass. NIC
state/MTU and HCA state/link-layer/rate remain independent checks.

Preflight also requires actual `MemTotal`, each HCA's `link_layer`, complete
successful `ip` JSON and valid `lsblk` JSON from the execution view. Missing
fields are not inferred. `HOST_SYSFS_ROOT` does not redirect `ip` or `lsblk`;
the authorized runner must provide the intended host/device view separately.

GPU health uses the documented read-only
[`nvidia-smi nvlink` and `c2c` `-s`/`-e` commands](https://docs.nvidia.com/deploy/nvidia-smi/index.html),
with readable current-boot kernel history and an explicit
`GPU_HEALTH_C2C_MODE=required|disabled`; there is **no default or auto-detection**.
In `required` mode, both link kinds retain nonempty `HEALTH_LINKS_JSON` policies
and `HEALTH_LIMITS_JSON` requires `max_c2c_total` and `max_c2c_delta`.
In `disabled` mode, omit the `c2c` policy and those two limits; dummy C2C thresholds
are neither needed nor accepted. Each selected GPU must instead report exactly
`GPU C2C Mode: Disabled` or `GPU C2C Mode: Not Supported` in a successful
`nvidia-smi -i <index> -q` response with matching GPU PCI identity and UUID, at
every sample. Enabled C2C fails; generic `N/A`, missing/ambiguous evidence and
command errors remain non-passing. Only C2C link/counter evaluation is skipped:
measurements explicitly record not-applicable status and observed mode, and the
success reason is `DeclaredGPUHealthCheckedC2CNotApplicable`. All NVLink, Xid,
ECC, clock, power and temperature observability remain required.

`GPU_HEALTH_TEMPERATURE_MODE=enforce|observe` is also mandatory, with **no
default**. Existing callers must explicitly choose `enforce` to retain the
positive `HEALTH_LIMITS_JSON.max_temperature_c` ceiling: a sample strictly above
it fails. `observe` **forbids** that field, but still requires valid temperature
evidence from every selected GPU at every sample. It retains each exact native
temperature token as a string in measurements (`temperature_sample`, `gpu`,
`temperature_c`) and records `temperature_mode` and
`temperature_health_evaluated: false`. The reason and measurements explicitly
state `functional-validation observation only; no temperature health claim`.
Temperature alone cannot fail this functional-validation mode; missing, invalid
or errored telemetry remains `Inconclusive`, never Pass. All other policies,
limits, result bounds and cleanup remain unchanged.

`HEALTH_LINKS_JSON` declares exact link IDs, states and error/observation-counter
labels for every applicable link kind. For example, NVLink5 traffic-byte counters
must not be mistaken for errors. Unknown, missing, duplicated, reset or
unsupported applicable telemetry cannot pass. Driver support and exact output
schemas still require target validation; no fallback is guessed.

Link counters/rates and their existing explicit total/delta limits accept finite
nonnegative canonical integers, leading-digit decimals and scientific notation
such as `0`, `0.0`, `15e-255` and `1.2E+3`. Leading signs/dots/zero padding,
NaN/Inf, locale commas, units and trailing junk are rejected. Python stdlib
`decimal` preserves exact values, sums, deltas and numeric JSON limit tokens;
link numbers are serialized as decimal strings, not rounded binary floats.
No threshold is invented or relaxed: a tiny positive value still exceeds zero.
Arithmetic outside decimal's representable range or a 4 MiB digit span is
explicitly `Inconclusive`, never a rounded/underflowed Pass. ECC/Xid counts,
C2C applicability and all other health policy remain unchanged.

Health is a standalone check with caller-supplied policy and its own verdict;
it does not depend on a monitoring service. Dapple hardware validation has not
been run. The authorized caller supplies approved limits for every enforced
check and the native link-counter classification. Temperature `observe` removes
only its ceiling; missing limits for other checks remain an input blocker, not
permission to invent thresholds or silently observe instead.

These are first-party implementations, not copies of Mango hhdiag, Polaris, or
third-party benchmark source. Existing legacy payload bytes and behavior are
preserved separately. The added files and README alone do not install an Argo
step, expand an upstream required test plan, or authorize a hardware run.

## Entrypoints

The baseline payloads are [identity](scripts/gpu-identity-example.sh),
[full-node](scripts/gpu-full-node.sh), and [burn](scripts/gpu-burn-canary.sh).
The [MPI identity](scripts/gpu-identity-mpi.sh) and
[MPI full-node](scripts/gpu-full-node-mpi.sh) variants prepend role dispatch to
the unchanged baseline bodies. The identity example checks identity/count only;
it is not an organization-reviewed hardware qualification diagnostic.

| Caller | Exact invocation |
| --- | --- |
| Normal stage | `/bin/bash /scripts/<selected-key>` |
| MPI Launcher | `/bin/bash /scripts/<selected-key> mpi-launcher <gpu-count>` |
| MPI Worker | `/bin/bash /scripts/<selected-key> mpi-worker` |
| Full-node MPI rank, invoked by its launcher | `/bin/bash /scripts/<selected-key> mpi-rank` |

For new identity/full-node MPI selections, use the corresponding MPI key and
`spec.parameters.mpi-payload-mode: script` with the matching upstream runtime.
The GPU count must be a positive integer. Identity retains its one-rank
`/bin/true` backend check; full-node dispatches the local-rank GPU query.
MPI-role dispatch precedes baseline initialization/result writes and propagates
role failures. It does not introduce new result files or test IDs.

Absent/empty mode retains upstream `legacy-inline`. Already-admitted Runs with
an empty test plan retain their inline compatibility path and receive no new
script arguments, including on retry. Unsupported modes fail closed. Burn does
not use this MPI mode; its existing component interface is unchanged.

## Compatible scripts and results

Use the existing built-in workflow names and required test IDs:

| `spec.workflow.name` | Required test IDs |
| --- | --- |
| `project-signal-gpu-certification` | `gpu-identity`, `mpi-backend` |
| `project-signal-gpu-full-node-certification` | `preflight`, `gpu-health`, `mpi-rank-access`, `health-after` |
| `project-signal-gpu-burn-canary` | `gpu-burn`, `cpu-stress`, `memory-integrity`, `storage-integrity` |

A completed diagnostic writes `/results/verdict` (`Pass`, `Fail`, `Suspect`, or
`Inconclusive`), `/results/reason`, and bounded `/results/measurements.txt`.
It exits successfully even for a hardware `Fail`, preserving a typed diagnostic
result instead of converting it into an orchestration error. Infrastructure and
MPI-role failures must instead propagate their failing exit status. Upstream
owns aggregation, required coverage, attempt/node/script-digest provenance, and
cleanup; missing results or a passing final stage cannot erase earlier failure.

Full-node stages use `PROJECT_SIGNAL_STAGE_NAME` (`preflight`, `workload`,
`health-after`) and the existing `PROJECT_SIGNAL_STAGE` event format. Burn's
matching upstream four-container ContainerSet supplies
`PROJECT_SIGNAL_BURN_COMPONENT` and shares the ready/stop/completion protocol.
Each burn component writes `/results/<component>-verdict`, `-reason`, and
`-measurements.txt` in addition to the group's root outputs. These are coupled
tests with shared safety and foreground cleanup, not four independent jobs.

When authoring a compatible payload, preserve strict Bash variable/pipeline
handling, explicitly finalize diagnostic verdicts, and fail closed on missing
inputs or dependencies. Keep measurements bounded rather than embedding raw
logs; retain role exit propagation, reviewed guards, and existing result/event
formats. Do not invent test IDs or claim new coverage without a matching
upstream runtime change. GPU, MPI, SSH, and stress-tool dependencies must be
provided separately in the reviewed runner image.

The original identity/full-node/burn scripts retain the approved decoded
[`Azure/unbounded@98bdf4d0703f98c2c867ed22d561929a79f47996` script keys](https://github.com/Azure/unbounded/blob/98bdf4d0703f98c2c867ed22d561929a79f47996/deploy/signal/03-scripts.yaml.tmpl);
the MPI variants retain those baseline bodies. The ten additional diagnostics
are new first-party implementations here, not imported benchmark-suite bodies.
[LICENSE](LICENSE) and [NOTICE](NOTICE) preserve the imported upstream licensing
and attribution notices.
