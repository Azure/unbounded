# GPU Racer benchmark checkpoint

Final measured canary results and safe paused state are in
[racer-gpu-benchmark-results-20261007.md](racer-gpu-benchmark-results-20261007.md).
The updates below are the deployment chronology, not the final live state.

## Update at 2026-10-07 18:25 UTC: both raw-device dataplanes Ready

The startup fix was cherry-picked onto clean original `racer-v2` as
`4bb2327cb5ef9a2ce317473f7b634f4b22893cd0`, preserving all newer commits.
Only that branch was pushed. No worktree branch was pushed or built.

| Image at tag `4bb2327cb5ef9a2ce317473f7b634f4b22893cd0` | Action run | Result at checkpoint |
|---|---|---|
| racer-dataplane | [37665144114](https://github.com/Azure/unbounded/actions/runs/37665144114) | Success; deployed |
| racer-loadgen | [37665148287](https://github.com/Azure/unbounded/actions/runs/37665148287) | Success; not deployed |
| racer-controller | [37665152916](https://github.com/Azure/unbounded/actions/runs/37665152916) | Success; not deployed |
| unbounded-operator | [37665157987](https://github.com/Azure/unbounded/actions/runs/37665157987) | Build and push still running |

All four runs have the exact head SHA above and target linux/amd64. Newer code
includes SDK and controller/operator changes, so a coherent set was requested.
The fixed Rust production diff is only NIC discovery; its control/object/peer
wire crates are unchanged from the deployed version. The SDK wire diff changes
comments only. This supports temporary use with the existing controller while
the full set finishes building; it is not a general mixed-version guarantee.

Disk identity and exclusive-read checks passed again at 18:14 UTC for all 15
devices, with zero writes and no visible process-scan failures. Diagnostic pods
were removed. The fixed image was selected through supported named-container
overrides and the pause gate removed. Both GPU pods became Ready with no restarts.

| Evidence | gpu-07-03 | gpu-07-13 |
|---|---|---|
| Pod | racer-dataplane-9n9vp | racer-dataplane-zm6vv |
| Startup raw devices | 7 | 8 |
| Open NVMe descriptors | nvme1n1 through nvme7n1 | nvme0n1 through nvme7n1 |
| Whole 64 MiB segments | 400,617 | 457,848 |
| I/O / crypto workers | 10 / 5 | 10 / 5 |
| `/readyz` | ready | ready |
| `/debug/failures` | total=0 | total=0 |
| Membership workers | 10/10, fully_applied=1 | 10/10, fully_applied=1 |

Both use dataplane digest
`sha256:cee310f151d16b61adbda8f5ef8c157b878745d60c6e7dd2e6c4a948642f154c`.
There were no device-skip or slab-file fallback logs. The excluded node03 disk
is absent from its open descriptors. Checkpoints live under the slab directory;
checkpoint files are not evidence of slab payload fallback.

Both report membership sequence 4/version 3 and hash
`3ae510e1c82ca6725c91b753f2f5e917eca491ebb413a0b143ed91eaae4eaa31`.
Hardware enrollment lists 13 usable ports with GIDs and NUMA locality on each
node. The admitted membership correctly uses only the explicit eight
`mlx5_00` through `mlx5_07` mappings, rails 0 through 7, with shared site
`racer-gpu-fabric-07`. Both processes hold eight unique uverbs device descriptors.
This establishes enrollment and open-device state, **not RDMA payload throughput**.
No benchmark traffic has run; disk publications and indexed payload are zero.

Two capacity limits matter for the benchmark: control-connection budget reduced
the initial 11 I/O workers to 10; checkpoint working memory is capped at 512 MiB
despite larger worst-case snapshot estimates, so oversized cuts are skipped.
All workers currently land on NUMA node 0 under the 16-thread cap. Do not call
this an all-core or balanced-NUMA saturation configuration.

An operator upgrade was attempted before its image tag existed. It failed with
ImagePullBackOff/NotFound, not a code crash. The known-good operator was restored
and rollout passed; serving controllers/dataplanes remained Ready. Operator and
controller remain at `593f517c95cdbd906eabf83be7a71b753e4efadd`. Additive updated
operator RBAC is applied. Wait for operator run success before reapplying the
rendered upgrade; do not repeat the failed pull unchanged. No durable identity
was changed. No load is running or awaiting recovery.

Read-only evidence is saved as worktree `tmp/gpu-07-*-startup.log`,
`tmp/gpu-07-*-fds.txt`, and membership/failure/metrics snapshots. Live resource
snapshots are `tmp/racer-active-{workloads,config,nodes}.json`.
`tmp/loadgen-paused.yaml` is prepared but **not applied**. It uses the built
loadgen, volume `racer-bench`, exact GPU affinity, 128 x 64 MiB objects, verification,
concurrency zero, and a projected concurrency control file. For the next phase,
first finish the matching control-plane upgrade and recheck readiness. Then apply
that manifest through gateway kubectl, require both origins Ready and applied
concurrency zero, capture baseline metrics, and run separately bounded load steps.
Pause with control ConfigMap concurrency zero and verify in-flight work drains.

## Update at 2026-10-07 17:13 UTC: controller ready, dataplanes paused

All 15 approved relative by-ID links were created and verified after repeated
identity and exclusive-read checks. Both provisioning pods were deleted. The
excluded gpu-07-03 nvme0n1 has no added link. No benchmark traffic has started.

ClusterVolume `racer-bench` exists. The vanilla operator is 1/1 Ready and Racer
controller is 3/3 Ready. All images remain tagged
`593f517c95cdbd906eabf83be7a71b753e4efadd`. Cluster identity is
`0f5e01d9-7c73-407c-9585-1453e8a5686b`; permanent identity objects are intact.

Supported configuration now includes:

- Exact GPU node-name affinity for both Racer DaemonSet names; both AKS nodes
  carry `racer.unbounded-cloud.io/exclude=true` for membership too.
- GPU taint toleration `nvidia.com/gpu=present:NoSchedule`.
- Shared site `racer-gpu-fabric-07`, eight explicit NICs `mlx5_00` through
  `mlx5_07`, port 1, rails 0 through 7 respectively on both nodes.
- Host networking, peer port 8082, diagnostics port 9090.
- Dataplane requests 4 CPUs/8 GiB; limits 16 CPUs/64 GiB; maximum 16 threads.
  Plaintext/ciphertext budgets are 2 GiB each, dirty/registered 1 GiB each,
  request contexts 256 MiB. Other settings use runtime defaults.

Precreating `racer-config` initially hit the operator's standalone-adoption guard.
Only that session-created ConfigMap was removed, under an unsatisfied scheduling
gate. The operator initialized its own configuration and identity, then the
network settings were patched through the supported ConfigMap. No permanent
identity object was deleted. The first zero-desired rollout was not accepted as
success; adding the GPU taint toleration produced two GPU dataplane pods.

Both dataplanes then failed before worker planning with `racer-dataplane: Io`.
A same-context diagnostic pod isolated the cause: `/sys/class/net/bonding_masters`
is a regular file. Runtime NIC discovery tried to read `device/numa_node` below
it, received ENOTDIR, and failed. CPU topology and cgroup reads passed. All
diagnostic pods were removed; no host services or security settings changed.

Worktree commit `f9b520f1d` fixes discovery to skip non-directory class entries
while following device symlinks. Its regression and all 11 affinity tests passed;
Rust formatting and focused `make fmt` passed. Cherry-pick onto original
`racer-v2` was refused because another task has staged SDK changes. Those changes
were not stashed, reset, unstaged, or committed. No replacement image was built.

To stop known startup crash loops, both DaemonSet overrides now include the
unsatisfied node selector `racer.unbounded-cloud.io/bootstrap-enabled=true`.
The live DaemonSet has desired/current zero. This is a deliberate pause, not a
successful dataplane deployment. Raw-device startup count and RDMA enrollment
remain **unverified**; do not start a benchmark. All nodes remain Ready and the
model-cache PV remains Bound.

Resume after the SDK owner clears the original index:

1. Cherry-pick `f9b520f1d` to original `racer-v2`; inspect current committed state,
   push only `refs/heads/racer-v2:refs/heads/racer-v2` with followTags disabled.
2. Dispatch `images.yaml --ref racer-v2` for `image=racer-dataplane`,
   `platforms=linux/amd64`; record the resulting original-branch SHA and digest.
3. Set that image through the named dataplane container in the supported
   workload overrides. Keep operator/controller images unchanged for this narrow
   runtime-only fix. Verify the rendered image before removing the pause gate.
4. Repeat disk identity/exclusive checks, remove the pause gate, require actual
   two-GPU readiness, then verify raw-device logs and open descriptors for 7+8
   devices and RDMA enrollment. Do not infer RDMA transfers from annotations.
5. Only then prepare direct loadgen for volume `racer-bench` at concurrency zero.

Session scripts and exact commands remain in the ignored worktree `tmp/` and
`tmp/gpu-benchmark-checkpoint.txt`. `tmp/racer-staged.json` is the initial staging
snapshot, not the final live overrides; it lacks the later toleration/pause gate.

## Update at 2026-10-07 17:01 UTC: privileged disk preflight passed

This update supersedes the outer-host mount-visibility blocker below. No raw
writes, device links, node selectors, or load were activated in this phase.

Two authorized node-pinned privileged diagnostic pods ran with hostPID and
read-only hostPath mounts of node `/` and `/dev`. Both succeeded and were deleted
by 17:01:13 UTC; a separate label query confirmed no diagnostic pods remained.
The image resolved to
`docker.io/library/python@sha256:34386ef0cb081344d7ec1c103ba398e6e9f64e9ab3a1509accc92a4e24a07258`.

The installed `kubectl-node_shell` 1.11.0 uses `nsenter --target 1` normally and
node root hostPath in `-x` mode. Our equivalent diagnostic pod confirmed that
both expose `kube1`, not the outer host: PID 1's root mount is
`/dev/sda2[/var/lib/machines/kube1]`. No outer-host proc handle was available.
No node SSH or isolation workaround was used.

For all 15 approved devices, the probe checked serial, major/minor, capacity,
read-only flag, holders, child partitions, every visible process mount table,
and visible raw-device file descriptors. There were no matches or proc access
failures. It opened each with **O_RDONLY | O_EXCL**, performed no payload I/O,
and confirmed a second exclusive open failed with **EBUSY**. All 17 sysfs stat
fields stayed unchanged over the ten-second observation; lifetime writes were
zero. Fresh `blkid -p` returned 2 (no detected signature) on all 15 disks.
The excluded disk's label/UUID and Bound PV remain unchanged.

The claim check is not limited to visible mounts. Linux 6.8
[`block/fops.c`](https://github.com/torvalds/linux/blob/v6.8/block/fops.c),
`file_to_blk_mode` and `blkdev_open`, pass an O_EXCL holder to
[`block/bdev.c`](https://github.com/torvalds/linux/blob/v6.8/block/bdev.c),
`bdev_open_by_dev` and `bd_prepare_to_claim`. These check the kernel block-device
holder without a mount-namespace filter. This excludes competing filesystem or
exclusive claims at the time of the check, including claims outside `kube1`.
The running kernel reports `6.8.0-110-generic`; the source reference is upstream
6.8, not a separately audited Ubuntu build. The live EBUSY check corroborates
exclusive-claim enforcement.

**Operational conclusion:** the user-approved disposable disks pass this
preflight. Outer-host mount visibility is no longer itself a blocker. It is
reasonable to proceed with narrow by-ID provisioning and Racer startup checks
under the existing approval. This is an operational judgment, not proof of
universal non-use: O_EXCL does not reject all nonexclusive raw opens, cannot
prove data has no value, and a released probe claim does not reserve a disk.
An unseen idle nonexclusive raw opener is not ruled out. There is no observed
evidence of such use. Racer must still obtain its own exclusive claims at
startup (`cmd/racer-dataplane/src/app/devices.rs:98-132`) and reject fallback.

### Exact approved by-ID basenames

These names derive from each device's live sysfs WWID. They are proposed links,
not existing entries. `/dev/disk/by-id` is absent inside both nodes.

| Node | Device | Approved basename |
|---|---|---|
| gpu-07-03 | nvme1n1 | nvme-eui.3634483058a115610025384e00000001 |
| gpu-07-03 | nvme2n1 | nvme-eui.3634483058a115670025384e00000001 |
| gpu-07-03 | nvme3n1 | nvme-eui.3634483058a097440025384e00000001 |
| gpu-07-03 | nvme4n1 | nvme-eui.3634483058a115420025384e00000001 |
| gpu-07-03 | nvme5n1 | nvme-eui.3634483058a097410025384e00000001 |
| gpu-07-03 | nvme6n1 | nvme-eui.3634483058a114950025384e00000001 |
| gpu-07-03 | nvme7n1 | nvme-eui.3634483058a115170025384e00000001 |
| gpu-07-13 | nvme0n1 | nvme-eui.3634483058a115300025384e00000001 |
| gpu-07-13 | nvme1n1 | nvme-eui.3634483058a115280025384e00000001 |
| gpu-07-13 | nvme2n1 | nvme-eui.3634483058a115210025384e00000001 |
| gpu-07-13 | nvme3n1 | nvme-eui.3634483058a115580025384e00000001 |
| gpu-07-13 | nvme4n1 | nvme-eui.3634483058a115590025384e00000001 |
| gpu-07-13 | nvme5n1 | nvme-eui.3634483058a115570025384e00000001 |
| gpu-07-13 | nvme6n1 | nvme-eui.3634483058a114940025384e00000001 |
| gpu-07-13 | nvme7n1 | nvme-eui.3634483058a115270025384e00000001 |

Next deployment phase: use a short-lived node-pinned privileged pod with node
`/dev` mounted writable only for creating directory entries. Recheck the exact
serial/WWID mapping above, reject conflicting existing links, then create
relative links `disk/by-id/NAME -> ../../nvmeNn1`. Do not create any link for
gpu-07-03 nvme0n1. Do not open devices for writing in the provisioning pod.
Use these exact node-specific annotation values only after links are verified:

```text
gpu-07-03:
^nvme-eui\.3634483058(a11561|a11567|a09744|a11542|a09741|a11495|a11517)0025384e00000001$
gpu-07-13:
^nvme-eui\.3634483058(a11530|a11528|a11521|a11558|a11559|a11557|a11494|a11527)0025384e00000001$
```

Keep the original sequencing below: GPU-only affinity and membership exclusion
before any ClusterVolume. Provisioning and activation commands were not run.
The reproducible probe is in worktree `tmp/privileged-disk-check.py`; per-node
evidence is in `tmp/racer-disk-check-gpu-07-03.log` and
`tmp/racer-disk-check-gpu-07-13.log`. Preserve those artifacts with the session.

## State at 2026-10-07 16:57 UTC

The vanilla operator is installed and Ready. The benchmark is blocked before raw
cache activation by missing outer-host usage checks. No throughput, latency,
correctness, disk-write, or RDMA-transfer results have been measured.

All commands used bounded external TERM timeouts. Kubernetes commands ran through
`ssh azureuser@52.188.113.212`. No GPU services, mounts, partitions, filesystems,
SSH services, firewall rules, or existing workloads were changed.

## Images and source

Image tag: `593f517c95cdbd906eabf83be7a71b753e4efadd`, registry `ghcr.io/azure`.
Only the original `racer-v2` branch was pushed. Builds used `images.yaml`,
`--ref racer-v2`, and `platforms=linux/amd64`.

| Image | Action run | Observed result |
|---|---|---|
| unbounded-operator | [37654861645](https://github.com/Azure/unbounded/actions/runs/37654861645) | Success |
| racer-controller | [37654865505](https://github.com/Azure/unbounded/actions/runs/37654865505) | Success |
| racer-dataplane | [37654869396](https://github.com/Azure/unbounded/actions/runs/37654869396) | Success |
| racer-loadgen | [37654873633](https://github.com/Azure/unbounded/actions/runs/37654873633) | Success |

The earlier dataplane build failed because the image omitted `http/tests`, which
Cargo needs to resolve its declared `pool` target. Commit `593f517c9` adds that
input and a regression assertion. Focused validation passed:

```sh
timeout --signal=TERM --kill-after=10s 300s env GOTOOLCHAIN=go1.26.6 \
  make fmt GO_PACKAGE_DIRS=./deploy/racer GO_PACKAGE_PATTERNS=./deploy/racer
timeout --signal=TERM --kill-after=10s 300s env GOTOOLCHAIN=go1.26.6 \
  go test -timeout=5m ./deploy/racer
```

The explicit Go version avoids the installed Go 1.26 linter's panic with the
default Go 1.27 toolchain. No application behavior was changed.

## Cluster changes

Applied the five vanilla templates in `deploy/unbounded-operator`, rendered with
the image above. The operator bootstrapped its CRDs. Startup initially failed
because API endpoint discovery received an IP, not an FQDN. Re-rendering with the
supported `APIServerEndpoint` input fixed startup:

`https://aks-customer-004-eus-5czzjqga.hcp.eastus.azmk8s.io:443`

This value came from the gateway's active kubeconfig server field. The normal
configuration-hash rollout succeeded. The running operator image digest is
`sha256:85f57a37f3a26659e08dc079b7d677317574d9be40522f5c134f3781af47d98f`.

There are no Sites, ClusterVolumes, Racer dataplanes, or load generators. All four
nodes remain Ready. Both AKS system nodes must remain outside Racer dataplane
placement when activation resumes.

## Disk inventory and safety gate

The user approved seven raw disks on gpu-07-03 and eight on gpu-07-13, subject to
final identity and usage checks. Each is 3,840,755,982,336 bytes, model
`SAMSUNG MZQL23T8HCLS-00A07`.

| Device | gpu-07-03 serial | gpu-07-13 serial |
|---|---|---|
| nvme0n1 | **S64HNN0XA11543: excluded** | S64HNN0XA11530 |
| nvme1n1 | S64HNN0XA11561 | S64HNN0XA11528 |
| nvme2n1 | S64HNN0XA11567 | S64HNN0XA11521 |
| nvme3n1 | S64HNN0XA09744 | S64HNN0XA11558 |
| nvme4n1 | S64HNN0XA11542 | S64HNN0XA11559 |
| nvme5n1 | S64HNN0XA09741 | S64HNN0XA11557 |
| nvme6n1 | S64HNN0XA11495 | S64HNN0XA11494 |
| nvme7n1 | S64HNN0XA11517 | S64HNN0XA11527 |

gpu-07-03 `/dev/nvme0n1` remains ext4, label `tau-model-cache`, UUID
`d55cd903-d42e-4625-82a3-d5a8c5b2bab3`. Its associated PV
`glm-model-cache-gpu-07-03` remains Bound with Retain policy. Do not alter either
the device or the existing local PV path.

Read-only checks through the existing privileged monitoring pods found no
signatures, holders, child partitions, read-only flags, or visible mounts on the
15 approved disks. Their sysfs counters showed zero writes since boot. These
checks are not proof that no outer-host process holds a raw device open.

Kubernetes runs inside systemd-nspawn `kube1`. Entering PID 1's mount namespace
still shows `/dev/sda2[/var/lib/machines/kube1]`, not the outer host. Outer-host
processes and mounts cannot be inspected through this proc namespace. Direct
gateway-to-node SSH timed out in the earlier phase. A bounded TCP relay through
the monitoring pod reached outer-host port 22, but both local and gateway
default `azureuser` identities were rejected with `Permission denied (publickey)`.
No authentication settings were changed.

No by-ID links or Racer disk selectors were created. Raw activation is held
until an authorized outer-host access path or host-side usage evidence is
available. Do not substitute the container mount table for that check.

## RDMA and resume

Both nodes expose active InfiniBand ports `mlx5_00` through `mlx5_07`, each
reporting 400 Gb/s and a nonzero GID at index 0. This is hardware inventory, not
evidence of Racer RDMA transfers. gpu-07-13's preexisting missing eighth GPU and
monitoring collector restarts were not changed.

After inspecting live state, resume in this order:

1. Complete outer-host disk usage checks and recheck exact identities. Never
   include gpu-07-03 nvme0n1.
2. Expose only approved stable by-ID entries inside each Kubernetes node.
3. Set exact anchored selectors, the shared Racer site label, and GPU-only
   supported workload affinity overrides before creating any ClusterVolume.
   Exclude both AKS nodes from membership too.
4. Create the cache volume and verify raw-device startup on all 15 disks. Reject
   slab-file fallback. Verify RDMA membership and usable ports.
5. Start direct Racer loadgen origins with concurrency zero. Run bounded phases
   with metrics snapshots and distinguish cold, warm, RDMA, and HTTP traffic.
6. Pause and drain traffic, save results, and preserve artifacts before removing
   the task worktree.

The active worktree is `.worktrees/racer-gpu-benchmark-20261007`. Its ignored
`tmp/gpu-benchmark-checkpoint.txt` records commands, failures, and phase state;
`tmp/node-inventory.sh` reproduces node-visible inventory. No benchmark load is
running or awaiting recovery.
