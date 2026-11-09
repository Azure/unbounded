# Agent Isolation Modes

Status: proposed. This document is both the design and the execution plan for
sub-agents. Section 11 contains self-contained work packages.

## 1. Summary

The Unbounded agent turns a host into a Kubernetes worker. Today it always does
this by unpacking an OS image into a systemd-nspawn machine (`kube1` or
`kube2`) and running containerd, kubelet, CNI and LocalDNS inside it. This
design makes that choice pluggable.

The agent always remains a host process. An **isolation mode** decides only
where the node stack (containerd, kubelet, CNI, CoreDNS) runs.

| Mode        | Node stack runs                                      | Status        |
|-------------|------------------------------------------------------|---------------|
| `nspawn`    | In a systemd-nspawn machine (today's behavior)       | Default       |
| `none`      | Directly on the host as agent-owned systemd units    | New           |
| `container` | In a privileged OCI container (for Azure Container Linux) | New, provisional |

## 1.1 Delivery scope

This design covers three modes: `nspawn` (existing), `none` (bare host) and
`container` (Azure Container Linux). WP0 to WP5 are in scope.

## 2. Goals and non-goals

Goals:

- One `Backend` interface that every machine operation goes through.
- A coherent story for every operation (bootstrap, start, stop, soft reboot,
  repave, reset, delete, upgrade, exec, logs, GPU, LocalDNS) in every mode.
- No behavior change for existing `nspawn` hosts. Hosts with no recorded mode
  are `nspawn`.
- A supported way to switch modes on a live host.
- Documentation that describes each mode and how to choose and change it.

Non-goals:

- Changing the existing `nspawn` repave semantics.
- Auto-detecting a mode. The configured mode is always explicit.
- Time or effort estimates (per `AGENTS.md`).

## 3. Verification notes

Code is authoritative (`AGENTS.md`). Facts below come from an exploration pass
whose line numbers were approximate; WP0 step 1 re-verified them (see
"Citation verification" at the end of this section). Verified at the pre-change
commit: the files named in section 4 exist, and `nodeOperator` is defined at
`cmd/agent/internal/daemon/nodeoperator.go:37` with a single nspawn
implementation. There are 35 `MachineRun(` references in Go sources (the
exploration estimated about 40).

Known doc and code drift to fix while touching these files:

- `hack/smoke-metalman.py:68` cited `cmd/agent/internal/goalstates/constants.go`.
  The file is `pkg/agent/goalstates/constants.go`. The comment is fixed on this
  branch.
- `pkg/agent/phases/rootfs/os.go` hard-codes Ubuntu Noble apt sources
  (`archSources`, `sources-amd64.list`, `sources-arm64.list`), and
  `rootfs/provision.go:36` applies `ConfigureOS` to every rootfs, including the
  Azure Linux 3 default images (`goalstates/constants.go:182-183`). Not changed
  by this work.
- Deprecated constants `DaemonBinaryPath` and `NSpawnLifecycleBinaryPath` in
  `pkg/agent/goalstates/constants.go` (lines 34 and 44 at the pre-change
  commit) are superseded by `hostroot`.

### Citation verification

WP0 step 1 checked every file, line and symbol citation in this document
against the pre-change commit (`design/agent-isolation-modes`, which is `main`
plus the design documents). All file paths exist, except the deliberately wrong
`cmd/agent/internal/goalstates/constants.go` quoted above. All symbols exist:
`nodeOperator` (line 37), `machinectlRunner`, `NSpawnMachineKube1/Kube2`,
`ResolveNSpawnConfig`, `resetResources`, `NSpawnLifecycleConfig`,
`ProvisioningFormat`, and the hidden `nspawn-lifecycle` command. The `MachineRun(`
count of 35 is exact. The reset order in section 9 matches `resetResources`.
Corrections made: `FindActiveMachine` is a method of `nodeOperator`, not a
function; `host/assets/99-*` also matches `99-kubernetes-sysctl.conf`, which is
not a machined guard, so WP3 names the two guard files instead; section 4 is
the pre-change layout, and WP0 moved the nspawn items it lists into
`pkg/agent/isolation/nspawn`.

## 4. Current coupling (as explored)

This section describes the layout at the pre-change commit. After WP0 the
nspawn-specific items below live in `pkg/agent/isolation/nspawn`.

Agent code lives in `cmd/agent/internal/{cmd,daemon,bootstrap,attest,installstate}`
and `pkg/agent/{goalstates,phases/{host,rootfs,nodestart,nodestop,reset},nspawnlifecycle,config,hostroot,agentbinary}`.
Shared helpers are in `internal/{executil,hostroot,provision}`.

Coupling points to abstract:

- `MachineDir` file placement: `filepath.Join(MachineDir, ...)` in
  `pkg/agent/phases/rootfs/*` and `pkg/agent/phases/nodestart/*`.
- Command execution inside the machine: `internal/executil/exec.go`
  `MachineRun` (`systemd-run --machine=X`).
- Machine lifecycle: `pkg/agent/phases/nodestart/nspawn.go`
  (`machinectlRunner`), `pkg/agent/phases/reset/{machine,nspawn}.go`,
  `pkg/agent/phases/nodestop/stop.go`.
- Slot constants `NSpawnMachineKube1/Kube2` and `ResolveNSpawnConfig` in
  `pkg/agent/goalstates/{constants,resolve}.go`.
- Rendered nspawn config: `pkg/agent/phases/rootfs/nspawn.go` and assets
  `nspawn.conf`, `service-override.conf`.
- Host phases: `pkg/agent/phases/host/{os,preflight_host,preflight_existing_deployment}.go`
  (installs `systemd-container`, checks `machinectl`).
- Lifecycle helper: `pkg/agent/nspawnlifecycle/lifecycle.go` and the hidden
  `nspawn-lifecycle` command in `cmd/agent/internal/cmd/nspawn_lifecycle.go`.
- Daemon seam: `nodeOperator` (`cmd/agent/internal/daemon/nodeoperator.go`),
  used by reboot, repave, reset, upgrade.
- Active machine discovery: the `nodeOperator` method `FindActiveMachine`,
  keyed on `/etc/unbounded/agent/<m>-applied-config.json`
  (`goalstates.AppliedConfigPath`).
- Uninstall: `internal/provision/assets/unbounded-agent-uninstall.sh`
  hard-coded `machinectl`, `/etc/systemd/nspawn`, `/var/lib/machines`. Nothing
  ran it; it is removed, and `unbounded-agent reset` is the way to remove a
  node in every mode.
- Generated config and vendor data: `internal/provision/agent_config.go`,
  `images/netboot/assets/vendor-data.tmpl`, `hack/smoke-metalman.py`.

There is no isolation field today. The nearest existing knobs are
`config.NSpawnLifecycleConfig` and `ProvisioningFormat`.

## 5. Design invariants

1. **Agent on host.** The agent and its daemon run on the host in every mode.
2. **Host-global state is mode independent.** The agent owns nftables flush,
   the LocalDNS dummy interface and nft rules, WireGuard keys, sysctls and
   bpffs mounts in every mode. Backends only declare ordering dependencies.
3. **Shared host network and same-path contract.** In `nspawn`, `none` and
   `container`, pods and the node stack share the host network namespace, and
   these paths resolve identically for every consumer: `/run/containerd`,
   `/etc/containerd/certs.d`, `/var/lib/kubelet`, `/opt/cni`, bpffs.
   unbounded-net-node, gantry and their charts must not change per mode.
4. **Nspawn is unchanged.** WP0 must be behavior preserving. Existing golden
   files and unit tests are the safety net and must not be edited to pass.
5. **Explicit mode.** The default is `nspawn`. Preflight fails fast with a
   suggested mode instead of switching silently.
6. **Rollback stability.** Units already written on existing hosts keep working
   across agent rollbacks. `nspawn-lifecycle` and `NSpawnLifecycleConfig` are
   not renamed or removed.

## 6. Configuration and API

### 6.1 Machine spec

Add to `MachineSpec` in `api/machina/v1alpha3/machine_types.go` an optional
field:

```go
// Isolation declares where the node stack should run. Empty declares nothing:
// a new node uses nspawn and a running node keeps its mode.
// +kubebuilder:validation:Enum=nspawn;none;container
// +optional
Isolation IsolationMode `json:"isolation,omitempty"`
```

`Machine.status` gains `isolation` (the mode the agent reports as
applied) and `isolationChange` (the agent's last attempt to change the mode:
`targetMode`, `phase` of `InProgress`, `Succeeded` or `Failed`, `strategy` of
`InPlace` or `Reprovision`, `message` and `lastTransitionTime`).

It is additive within `v1alpha3`. No API version bump. Regenerate with
`make generate`; never edit CRD YAML by hand.

`spec.isolation` is the operator's declaration. The agent never writes `spec`,
so no RBAC beyond the existing `machines/status` permissions is needed.

### 6.2 Agent config

Add `Isolation` to `AgentConfig` (`pkg/agent/config/config.go`). Empty means
`nspawn`. Validation rejects unknown values. Add `--isolation` to the agent
bootstrap flags and to `kubectl unbounded` manual bootstrap
(`cmd/kubectl-unbounded/app/machine_manual_bootstrap.go`).

### 6.3 Applied config

The applied-config file records `isolation`, `slot` and the owning backend. A
missing `isolation` reads as `nspawn`. `FindActiveMachine` must handle both old
and new files.

An nspawn node keeps `<slot>-applied-config.json`. A node in any other mode
stores it as `<mode>-<slot>-applied-config.json`
(`goalstates.AppliedConfigName`), a name agents that predate the mode never
read, so such an agent finds no node and refuses to operate it instead of
running nspawn operations against it. The name and the recorded mode must
agree, or discovery fails closed.

### 6.4 Version skew

- The agent rejects any unrecognized `isolation` value with a clear error. An
  agent that predates the key does not reject it: the config is decoded with
  `encoding/json` (`cmd/agent/internal/cmd/config.go`), which ignores unknown
  keys, so that agent runs the node as `nspawn`.
- An agent upgrade and the daemon recovery script ask the candidate (or the
  last-good binary) `unbounded-agent supports-isolation <mode>` for every
  non-nspawn mode whose applied config exists, and refuse a binary that fails
  it. An agent that predates the subcommand fails it.
- The netboot vendor-data template ships in the netboot image and is rendered
  by whichever metalman serves it, so it references no field a metalman may
  lack, and templates render from a map so an unknown field yields no value
  instead of failing the render. The machined guards it writes are removed by
  the agent at bootstrap in modes without an nspawn machine.
- Machina passes `spec.isolation` through to generated agent config without
  checking the agent version. No release number is compiled into the code; the
  operator pins an agent version that supports the requested mode.

### 6.5 Drift

`spec.isolation` declares intent; it does not move the node. The controller
compares it with `status.isolation` and `status.isolationChange` and
sets the `IsolationDrift` condition:

| Reason | Status | When |
|---|---|---|
| `NoModeDeclared` | `False` | `spec.isolation` is empty |
| `AppliedModeMatches` | `False` | the declared mode runs |
| `AppliedModeDiffers` | `True` | a different mode is declared; it applies at the next repave |
| `ChangeInProgress` | `True` | `status.isolationChange` is `InProgress` for the declared mode |
| `ChangeFailed` | `True` | `status.isolationChange` is `Failed` for the declared mode |

The condition is absent until the agent has reported a mode. The mode changes
when the node is next repaved (section 10).

### 6.6 Implementation status

- 6.4: implemented without a version gate. `provision.BuildAgentConfig` passes
  `spec.isolation` through and no release number is compiled into the code.
  Because an older agent ignores the key (above), pin a supporting agent
  version before setting a non-default mode.
- 6.5: the controller sets `IsolationDrift` as above from `spec.isolation`,
  `status.isolation` (which the agent daemon writes at startup and after
  a change) and `status.isolationChange`. The agent never writes `spec`.

## 7. Backend interface

New package `pkg/agent/isolation`. Names below are a starting shape. WP0 may
adjust signatures, but must keep the responsibilities.

```go
type Mode string

const (
    ModeNSpawn    Mode = "nspawn"
    ModeNone      Mode = "none"
    ModeContainer Mode = "container"
)

// Backend owns where the node stack runs.
type Backend interface {
    Mode() Mode

    // Requirements declares host requirements checked by preflight before
    // anything is mutated, and the ordering dependencies on host-global units.
    Requirements() Requirements

    // Prepare materializes a node root for a slot (or version, for none):
    // unpack/install artifacts and render backend config.
    Prepare(ctx context.Context, n NodeSpec) (NodeRoot, error)

    Start(ctx context.Context, r SlotRef) error
    Stop(ctx context.Context, r SlotRef) error
    // Restart restarts the node stack only, never the host (soft reboot).
    Restart(ctx context.Context, r SlotRef) error
    // Destroy removes the node root and backend config for a slot.
    Destroy(ctx context.Context, r SlotRef) error

    Status(ctx context.Context, r SlotRef) (Status, error)

    // Exec runs a command in the node stack (replaces MachineRun).
    Exec(ctx context.Context, r SlotRef, cmd ExecSpec) (ExecResult, error)
    // Logs streams logs for a unit in the node stack.
    Logs(ctx context.Context, r SlotRef, opts LogOptions) (io.ReadCloser, error)
}

// SlotRef names the slot an operation acts on. SlotName is a SlotRef with
// no filesystem view; every NodeRoot is a SlotRef.
type SlotRef interface {
    Slot() string
}

// NodeRoot resolves paths into the node stack's filesystem view.
type NodeRoot interface {
    SlotRef
    // Path maps an in-node absolute path to its path as seen by the agent.
    Path(inNode string) string
}
```

`DirRoot` (a directory on the host, used by nspawn) refuses an empty, relative
or host-root directory: `NewDirRoot` returns an error and `Path` panics. An
empty directory must never mean "the host root". A caller that only has a
slot passes a `SlotName`, and a backend that needs a filesystem view builds
its own root from the slot.

Phases receive a `NodeRoot` and call `Path` and `Backend.Exec`. They never
reference `MachineDir`, `machinectl` or `systemd-run --machine`.

**Grants.** GPU, device and extra mount requests (`goalstates/{nvidia,amd,hostdevices,hostpaths}.go`)
become a backend-neutral `[]Grant` (kinds: mount, device, library path). Each
backend applies them its own way:

- `nspawn`: render `.nspawn` binds and `DeviceAllow`.
- `container`: render OCI mounts, devices and cgroup device rules.
- `none`: nothing to apply (host is native).

**Mode composition table.** There is no init-time registry. One table,
`cmd/agent/internal/cmd/modes.go`, wires each mode: its backend factory, the
daemon's node operator for it (built with that backend factory) and its
bootstrap stage plan. A mode without a complete entry is one the agent cannot
operate, and the daemon and bootstrap refuse it before touching the host.
Dependencies are injected through structs (`daemon.Run` receives the mode
operators, `agentStages` holds the plan); no package-level function variable
exists to be swapped by tests. Tests use a fake backend.

**nodeOperator.** The daemon's `nodeOperator` is a dispatcher. Host-level
operations (finding the active machine, reset, stopping the daemon, boot ID,
host reboot, agent upgrade staging, the host root move) go to a host
operator whatever the mode. Node operations (lifecycle migration, restart,
repave) go to the `ModeOperator` of the mode the active machine was applied
with. Each mode implements `ModeOperator` in full and never embeds another
mode's operator, so a method added to it is a compile error in a mode that
lacks it instead of a silent fallback to nspawn. A mode with no operator is
an error, never a fallback. The dispatcher stays injected through the daemon
run options so daemon tests keep faking it.

**Adding a mode.** A mode is added by: its `isolation.Mode` constant and
`ParseMode` case, its backend package, its `ModeOperator` in the daemon, its
`stagePlan` in `cmd/agent/internal/cmd`, and one entry in the composition
table. The dispatcher, bootstrap and start do not change.

## 8. Per-mode design

### 8.1 `nspawn` (default)

Exactly today's behavior behind the interface: OCI unpack to
`/var/lib/machines/<slot>`, `.nspawn` and service override rendering,
`machinectl` start/stop, `systemd-run --machine` exec, slots `kube1`/`kube2`,
blue/green repave, `nspawn-lifecycle` hooks, per-machine bpffs bind.

Requirements: `systemd-container` (`systemd-nspawn`, `machinectl`), writable
`/etc` and `/var`.

### 8.2 `none`

Everything runs directly on the host.

- Install: containerd, kubelet, CNI plugins and CoreDNS come from the same
  release artifacts the agent downloads today, installed into agent-owned
  versioned directories such as `/opt/unbounded/node/<version>/`, with a
  `current` symlink. No distro packages.
- Units: agent-owned systemd units for containerd, kubelet and CoreDNS.
- Safety: bootstrap (`EnsureHostClean`, not only the optional `preflight`
  subcommand) refuses a host with a kubelet or containerd it does not own
  (unit, binary or running process). A unit masked to `/dev/null`, which an
  nspawn host leaves, is not one. containerd runs with the agent-owned
  `/etc/unbounded/containerd/config.toml`, never the distro's
  `/etc/containerd/config.toml`.
- Requirements: writable `/etc`, `/opt`, `/var`. `/usr` is never modified, so
  a read-only `/usr` is fine.
- Network and paths: host native. Pods share the host netns trivially.
- bpffs: no per-machine bpffs. The host `/sys/fs/bpf` is used directly. This is
  a documented difference for eBPF CNIs such as Cilium.
- Exec: runs on the host as the unit's environment. Logs: `journalctl` for the
  agent-owned units.
- Upgrade: stop units, end the pods (the shims of the agent's containerd and
  the pod cgroups) before host networking is cleaned, flip `current`, write the
  new units, start units, wait for kubelet Ready, and on failure flip back and
  start the previous version with its own units, configuration and host
  networking. The node is briefly down. A soft reboot restarts the units and
  leaves the pods running.
- CNI plugins are versioned with the rest (`<version>/cni/bin`) and linked one
  by one into `/opt/cni/bin` through `current`, so plugins cluster add-ons
  install there (unbounded-net's IPAM plugin) keep working.
- Crash safety: an upgrade journal (`none-upgrade-journal.json` in the agent
  config directory) records the version being replaced before anything
  changes, and the saved configuration is kept until the applied config is
  persisted. The daemon finishes an interrupted upgrade at its next start:
  rolled back, or committed if it had succeeded.

**Status (WP1).** Implemented end to end: bootstrap, start, soft reboot,
upgrade with rollback, reset, `exec`, `logs` and preflight. Decisions: the
applied config is recorded under the fixed slot `kube1`, as
`none-kube1-applied-config.json` (the
version directory, named `<kubernetes version>-<digest of other artifact
versions>`, is the unit of upgrade); same-path files are written by the existing
nodestart phases through a goal-state `RootOverride`, and binaries land in
`<base>/<version>/bin` through the same rootfs installers.

GPU: every grant is honored or refused by `Prepare`, never ignored. Device
nodes and identity mounts are checked for existence, NVIDIA library and helper
directories are satisfied by the host view, and a mount at another path or a
library-path grant is an error. On a host with NVIDIA devices `Prepare`
requires the NVIDIA Container Toolkit on the host, at `/usr/bin` or at the
paths in the agent config `NvidiaToolkit` (valid only for `none`). Before the
units start on `Start`, `Restart` and `Upgrade` (and its rollback) the agent
selects CDI mode and regenerates `/etc/cdi/nvidia.yaml` as nspawn does. Reset
removes the spec and leaves the toolkit's `config.toml` in CDI mode, which is
accepted.

Unverified: everything against a real systemd and containerd, kubelet Ready
beyond "unit active and credentials present", pods surviving a plain unit stop,
the GPU path on a real GPU host (including `nvidia-ctk cdi generate
--driver-root /` on the distros in use), and a read-only `/usr`.

### 8.3 `container` (Azure Container Linux)

Provisional until the node facts in section 13 are collected.

- Target host: Azure Container Linux (derived from Flatcar). Read-only `/usr`
  (dm-verity), enforcing SELinux, signed UKI with Secure Boot, image-based
  updates through AKS. Public docs do not cover runtimes, kernel modules or
  mount propagation.
- Image: reuse the `images/agent-*` rootfs images with systemd as PID 1 (an
  entrypoint is added to the image family if needed).
- Launch: the agent reuses the OCI unpack step, generates an OCI runtime bundle
  (`config.json`) over the unpacked rootfs, and runs it with `runc`
  supplied as an agent-downloaded artifact under `/opt/unbounded`. An
  agent-owned systemd unit supervises the container. The host's docker and
  containerd are not used.
- Slots: `kube1`/`kube2` with root at `/var/lib/unbounded/containers/<slot>`.
- Privileges: privileged, shared host netns and PID as needed, host bind mounts
  for the same-path contract (invariant 3), `rshared` propagation for
  `/var/lib/kubelet`, device and cgroup rules from grants.
- SELinux: when enforcing, set `process.selinuxLabel` to `spc_t`. A preflight
  check verifies label support. Provisional.
- Requirements: writable `/etc`, `/opt`, `/var`, a loadable `runc`, cgroup
  v2, required kernel modules available to the container.
- Provisioning: existing `ignition` `ProvisioningFormat`. No new format.
- Lifecycle hooks: a new hidden generic `node-lifecycle` subcommand with the
  same pre-start, post-start and reconcile phases. Shared logic lives in
  `pkg/agent`. `nspawn-lifecycle` is untouched.
- Exec: via `runc exec` (or equivalent) behind `Backend.Exec`. Logs: container
  journal via `journalctl -M` equivalent or `runc exec journalctl`.

**Status (WP2, provisional).** Implemented end to end: the OCI bundle and
supervising unit, bootstrap, start, soft reboot, blue/green upgrade with
rollback, reset, `exec`, `logs`, preflight, GPU wiring through the
`node-lifecycle` hooks, and uninstall. The runtime is `runc`, downloaded to
`/opt/unbounded/runtime/runc` in this mode only.
Decisions: paths under the bind-mounted same-path directories are written on the
host side, since a bind mount hides anything written below it in the rootfs;
both slots share those directories, so a repave resets the shared kubelet state
and a rollback restarts the old slot against the new shared files (saved and
restored byte for byte, see section 10.4); the applied config moves to the new
slot only after it is healthy; the node-lifecycle post-start hook asks the
runtime, not `systemctl is-active`, because the unit is still activating while
`ExecStartPost` runs (an inference from systemd semantics, not observed).

Unverified assumptions, all still open until the node facts in section 13 are
collected:
- `spc_t` is accepted by the node policy.
- `rshared` on `/var/lib/kubelet` propagates through the host root mount, and
  pod mounts propagate to the host.
- `SIGRTMIN+3` through `runc` stops systemd as PID 1.
- The capability, device and mount set in `config.json` is enough for kubelet,
  containerd and the CNI.
- `/proc/<pid>/root` of a containerized process resolves to the bundle rootfs
  (used by the bind-address ownership preflight).
- The runtime path and the `/lib/modules` read-only bind are right for the
  image.
- The privileged container, the `/run/host-nvidia*` binds and the CDI hooks
  work on Azure Container Linux.

## 9. Operation matrix

| Operation | `nspawn` | `none` | `container` |
|---|---|---|---|
| Preflight | `systemd-container`, nspawn runtime | no unmanaged kubelet/containerd, writable `/etc` `/opt` `/var` | runtime artifact, cgroup v2, SELinux label, modules |
| Bootstrap | host prep, unpack rootfs to slot, render nspawn config, start machine, start stack inside | host prep, install versioned artifacts, write units, start units | host prep, unpack rootfs to slot, generate OCI bundle, write unit, start container, start stack inside |
| Start | `machinectl start` then in-machine units | start host units in order | start container unit then in-container units |
| Stop | stop kubelet and containerd in machine, stop machine | stop host units | stop in-container units, stop container |
| Soft reboot (`NodeReboot`) | restart machine unit (re-runs regenerate-config and post-start) | restart containerd then kubelet, wait Ready | restart container unit (re-runs hooks) |
| Repave / upgrade | blue/green into alternate slot, cleanup old | symlink flip with automatic rollback, brief downtime | blue/green into alternate slot, cleanup old |
| Reset / delete | stop slots, remove config, remove machine, clean bpffs, then host-global cleanup | stop units, remove versioned dirs and units, host-global cleanup | stop container, remove bundle/rootfs/unit, host-global cleanup |
| Health | machine reachable, kubelet healthy, LocalDNS | unit active, kubelet healthy, LocalDNS | container running, kubelet healthy, LocalDNS |
| Exec / logs | `Backend.Exec` via `systemd-run --machine`; journal of machine | host exec; `journalctl` | `runc exec`; container journal |
| GPU / devices | binds and `DeviceAllow` | native | OCI mounts and devices |
| LocalDNS | CoreDNS in machine, host network unit | CoreDNS host unit, same network unit | CoreDNS in container, same network unit |
| Mode change | declare `spec.isolation`, delete the Node; repave switches in place to `container` | repave resets and bootstraps again (reprovision) | declare `spec.isolation`, delete the Node; repave switches in place to `nspawn` |
| Agent binary upgrade | unchanged (nspawn independent) | unchanged | unchanged |
| Uninstall script | reads mode from applied-config, mode-specific cleanup | same | same |

The order of reset steps in `daemon/reset.go` (`resetResources`) is preserved:
daemon unit, stop, WireGuard keys, config, root, bpffs, network and routes,
bootstrap unit, artifacts, systemd reload.

## 10. Changing the mode

### 10.1 Trigger

There is no operation for it. The operator declares the mode in
`spec.isolation` and the change is applied by the node's next repave, the
existing OnDelete repave: the daemon repaves when the Node object is deleted
(`cmd/agent/internal/daemon/controller_node.go`, `ReconcileRepave`). The repave
builds the desired configuration from the Machine, which now carries the
declared mode, and `hasDrift` compares the mode like any other field. When the
mode differs, the repave changes it, together with anything else the repave
applies (a new Kubernetes version, labels). Editing `spec.isolation` alone only
reports drift. The agent never drains: the operator cordons and drains the node
before deleting it, as for any repave.

An empty `spec.isolation` declares nothing, so a repave never changes the mode
of a node whose Machine does not declare one.

### 10.2 Supported strategies

The strategy follows from the pair; the operator does not choose it.

| From / To | `nspawn` | `container` | `none` |
|---|---|---|---|
| `nspawn` | n/a | in-place | reprovision |
| `container` | in-place | n/a | reprovision |
| `none` | reprovision | reprovision | n/a |

Reprovision is reset, then bootstrap in the new mode. Declaring the mode that
already runs changes nothing.

### 10.3 In-place steps (`nspawn` <-> `container`)

1. Preflight the target backend. Fail before mutating anything.
2. Download artifacts for the target.
3. Save the shared host configuration (`confsnap`).
4. Prepare the target backend in the alternate slot.
5. Stop the source node stack.
6. Clean up host networking as repave does today.
7. Start the target. Issue fresh kubelet credentials through the normal
   bootstrap and attestation flow. No credentials are copied. The Node name,
   labels and taints are kept.
8. Persist new applied-config (mode, slot, owning backend).
9. Wait for the target to come up on the host within a bounded timeout.
10. On success, destroy the source slot, remove its applied-config and the saved
    configuration, and record `Succeeded` in `status.isolationChange`.
11. On failure, stop the target, restore the saved configuration, restart the
    source slot and record `Failed` in `status.isolationChange` with the cause
    and `rolled back to <mode>`. The source slot is retained until step 9 passes.

A failed change is not retried on its own, so a bad declaration cannot cause
repeated outages: the node keeps the mode it had and `IsolationDrift` reports
`ChangeFailed`. The next trigger (deleting the Node again, or a configuration
change) starts a new attempt.

### 10.4 State

Pod data and the image store are re-pulled. Host-global state (invariant 2)
is untouched.

**Status.** Implemented for both strategies, driven from the repave.

- *In place (nspawn <-> container).* `migrate.RunInPlace` with real hooks:
  the target mode's host preflight, the repave's download resolution and archive
  staging, prepare in the alternate slot, the repave's network cleanup, persist
  the target's applied config, and the repave's local kubelet check within five
  minutes. Rollback removes the target's record and restores the source's.
  Success removes the source and updates `status.isolation`. Kubelet
  credentials are fresh because the target boots a new kubelet from the
  configuration's bootstrap token, as a repave does; nothing is copied.
- *Crash safety.* `migrate.Journal` is a write-ahead step file at
  `/etc/unbounded/agent/isolation-migration.json`. The daemon resolves a leftover
  journal at start (before the source stopped: drop the target; once it may have
  stopped: roll back; `awaiting-ready`: wait again; `ready`: finish cleanup) and
  records the outcome in `status.isolationChange`. While both slots have an
  applied config, the journal decides which one is active.
- *Reprovision (any pair, required for `none`).* The reset removes the daemon,
  so the daemon stages `/var/lib/unbounded-agent-reprovision` (binary copy,
  target config, journal) and starts `unbounded-agent-reprovision.service`,
  whose hidden `isolation-reprovision` command runs the existing reset, then the
  target mode's bootstrap, retried by systemd up to
  `daemon.ReprovisionMaxAttempts` runs. The daemon that bootstrap installs
  records the outcome once the applied config is in the target mode.
- *Shared configuration.* `confsnap` saves, byte for byte, the host files both
  stacks write at the same paths before an in-place change, a `none` upgrade
  and a container upgrade, and restores them before the source starts again on
  any failure. It never overwrites a complete snapshot and syncs what it saved
  before writing its manifest. After a restart mid-change the snapshot is
  restored for an in-place change (through `migrate.Journal`) and for a `none`
  upgrade (through the none upgrade journal); a container upgrade restores it
  only within the same process.
- *Preflight and tokens.* The `none` target's static preflight runs before the
  reset and the ownership half after. An attested host records its attestation
  URL at bootstrap and attests again on reprovision; any other host is checked
  for a still-valid bootstrap token before the reset. An invalid token cannot
  be renewed from the agent: the operator must renew it or bootstrap with
  attestation. A change to nspawn is refused up front when the host lacks the
  nspawn runtime, because a change installs nothing.
- *Gaps.* After a reprovision that never bootstraps, `status.isolationChange` is
  recorded `Failed` only once an agent next runs on the host.

### 10.5 Future: in-place upgrades without deleting the Node

A mode change, like any repave, replaces the node stack and needs the Node
deleted. Upgrades that swap only containerd and kubelet on the host, without
deleting the Node, are not built. The `none` backend is the extension point: it
installs each version in `/opt/unbounded/node/<version>` and switches an atomic
`current` link, with automatic rollback. A future trigger other than Node
deletion could flip that link directly. The mode change does not depend on it
and the repave stays the only trigger today.

## 11. Work packages

Each work package (WP) is written to be handed to one sub-agent. Rules for all
WPs:

- Read `AGENTS.md` first. No em-dashes, American English, no time estimates.
- Run `make fmt` before finishing. Do not hand-format. Run `make lint` and the
  relevant `make test`.
- Never hand-edit generated files. Use `make generate`.
- Do not remove `_test.go` files or `Test*` functions. Ask first.
- Do not cross `cmd/` package boundaries. Shared code goes in `internal/` or
  `pkg/`.
- Add tests for new behavior (success, failure, edge cases).
- Stay inside the owned files. If a change outside them is needed, stop and
  report instead of editing.

Dependency graph: WP0 -> (WP1, WP2, WP3 in parallel after WP0 interface lands)
-> WP4 -> WP5.

### WP0: Extract the interface (behavior preserving)

Depends on: nothing. Blocks: all others.

Owns: `pkg/agent/isolation/` (new), `pkg/agent/isolation/nspawn/` (new, code
moved from the nspawn-specific files), and edits to
`pkg/agent/phases/{rootfs,nodestart,nodestop,reset,host}/`,
`internal/executil/exec.go`, `pkg/agent/goalstates/{constants,resolve,rootfs,nodestart}.go`,
`cmd/agent/internal/daemon/{nodeoperator,daemon,reset}.go`,
`cmd/agent/internal/cmd/{bootstrap,start,reset}.go`.

Steps:

1. Re-verify section 4 against the code. Record any discrepancy in a short
   note appended to this document before proceeding. Done; see "Citation
   verification" in section 3.
2. Define `Backend`, `NodeRoot`, `Grant`, `Requirements`, `ExecSpec`, `Status`
   in `pkg/agent/isolation`, plus a fake backend for tests, and the mode
   composition table in the agent.
3. Move all nspawn-specific code behind an `nspawn` backend: rendering
   (`rootfs/nspawn.go` and assets), `machinectlRunner`, start/stop/recovery,
   `reset` machine handling, `MachineRun` call sites (35).
4. Replace `filepath.Join(MachineDir, ...)` in phases with `NodeRoot.Path`.
5. Convert GPU, device and mount goal states to `[]Grant`; the nspawn backend
   renders them to the same `.nspawn` and `DeviceAllow` output as before.
6. Make `nodeOperator` dispatch node operations to the mode's operator, chosen
   from the applied config; a missing mode is `nspawn`.
7. Keep `kube1`/`kube2`, `nspawn-lifecycle` and `NSpawnLifecycleConfig` as is.

Acceptance:

- All existing unit tests and golden files pass unmodified
  (`rootfs/testdata/**`, nodestart, reset, daemon tests).
- `rg 'MachineDir|MachineRun|machinectl'` outside `pkg/agent/isolation/nspawn/`
  and `pkg/agent/phases/host/preflight*` returns no hits, or each remaining hit
  is documented as intentional. Remaining hits in Go sources (tests excluded),
  all intentional:
  - `goalstates/{rootfs,nodestart,resolve}.go`: the `MachineDir` field is the
    nspawn backend's root directory in the goal state, kept so the nspawn
    output is unchanged; `noderoot.go` is the adapter that turns it into a
    `NodeRoot`.
  - `phases/rootfs/preflight_local.go`: `nspawnMachineProvisioningChecker` is
    nspawn-only preflight by nature (it inspects the machine directory), the
    rootfs counterpart of the host machine checks, so it is acceptable here and
    only runs for the nspawn mode.
  - `phases/nodestart/preflight_api_server.go`: derives the blue/green slot name
    from the root directory for the bootstrap preflight.
  - `phases/host/os.go` and `isolation/isolation.go`: comments only.
  - `nspawnlifecycle/lifecycle.go`: the `nspawn-lifecycle` hook is untouched
    (invariant 6).
  - `cmd/kubectl-unbounded/app/machine_soft_reboot.go`: help text that names
    `machinectl shell kube1` for nspawn hosts.
- `make lint` and `make test` pass.
- The Python QEMU e2e for the default mode passes, if runnable. Otherwise
  report that it was not run.

### WP1: `none` backend

Depends on: WP0. Parallel with WP2, WP3.

Owns: `pkg/agent/isolation/none/` (new), host unit templates and assets for it,
and new preflight checks in `pkg/agent/phases/host/` for the `none`
requirements.

Steps: implement `Backend` per section 8.2: versioned install, `current`
symlink, agent-owned units, unmanaged kubelet/containerd detection, restart,
symlink-flip upgrade with automatic rollback, destroy, status, exec, logs.
Wire CoreDNS as a host unit and keep the LocalDNS network unit unchanged.
Host-global phases (nftables, sysctls, WireGuard) must not be reimplemented;
reuse them.

Acceptance:

- Conformance suite (WP0 provides the harness; WP1 adds the `none` runner)
  passes: prepare, start, stop, exec, cleanup, idempotence, failure recovery.
- Golden tests for rendered units.
- Preflight refuses an unmanaged kubelet or containerd and a read-only `/etc`.
- Upgrade rollback test: failed kubelet Ready flips `current` back.

### WP2: `container` backend

Depends on: WP0 and the Azure Container Linux node facts (section 13). Parallel
with WP1, WP3.

Owns: `pkg/agent/isolation/container/` (new), OCI bundle generation,
`node-lifecycle` hidden command in `cmd/agent/internal/cmd/`, shared lifecycle
code in `pkg/agent/` (new package, for example `pkg/agent/nodelifecycle`),
changes to `images/agent-*` needed for systemd as PID 1, and the runtime
artifact (`runc`) download wiring.

Steps: implement `Backend` per section 8.3. Reuse the OCI unpack code from
`pkg/agent/phases/rootfs/oci`. Apply grants as OCI mounts and devices. Set the
SELinux label when enforcing. Supervise through an agent-owned systemd unit.

Acceptance:

- Conformance suite passes with the `container` runner (can use a stub runtime
  in unit tests).
- Golden tests for the generated `config.json` and unit.
- `nspawn-lifecycle` is byte-for-byte unchanged.
- The design section 8.3 is updated with the verified node facts, and anything
  still unverified stays labeled provisional.

### WP3: API, config, applied-config, mode change

Depends on: WP0. Parallel with WP1, WP2 (stubs for backends are fine).

Owns: `api/machina/v1alpha3/machine_types.go`,
generated CRDs and deepcopy via `make generate`,
`pkg/agent/config/config.go`, applied-config read/write,
`cmd/agent/internal/daemon/controller_node.go` (the repave), machina
agent-config generation (`internal/provision/agent_config.go` and machina
controller), metalman passthrough, `kubectl unbounded` manual bootstrap flag.

Steps:

1. Add `spec.isolation`, `status.isolation` and
   `status.isolationChange`. Run `make generate`.
2. Add `Isolation` to `AgentConfig` with validation. Unknown values rejected.
3. Applied-config records mode, slot, owning backend. Tolerate old files.
4. Pass `spec.isolation` into generated agent config (section 6.4).
5. Drift reporting in Machine status (section 6.5).
6. Implement the change orchestrator (section 10.3) on top of the `Backend`
   interface, including automatic rollback, and call it from the repave when the
   declared mode differs from the applied one.
7. Make nspawn-only host guards conditional: the `systemd-container` install
   in `host/os.go`, the machined restart guards
   (`host/assets/99-unbounded-needrestart.conf` and
   `99-unbounded-no-restart-systemd.conf`, applied by `host/apt.go`), and the
   `images/netboot/assets/vendor-data.tmpl` machined rules.

Acceptance:

- Existing Machines without the field produce unchanged generated output
  (golden comparison).
- Mode change tests with fake backends: success, preflight failure (no
  mutation), target failure (rollback to source), no retry of a failed change,
  and an empty declaration (the mode is kept).
- Config tests: `spec.isolation` reaches the generated agent config, and an
  unset field leaves the output unchanged.

### WP4: `exec`, `logs` and lifecycle

Depends on: WP0, plus WP1 and WP2 for their `Exec`/`Logs` implementations.

Owns: new agent subcommands in `cmd/agent/internal/cmd/` (`exec`, `logs`),
`kubectl unbounded` help text referencing machine access.

Steps: add backend-agnostic `exec` and `logs` subcommands. The uninstall
script, which nothing ran, is removed; `unbounded-agent reset` removes a node
in every mode: it always runs the idempotent nspawn teardown and adds a mode's
teardown only where the host has that mode's own units or directory.

Acceptance: command tests per backend with fakes.

### WP5: Docs and e2e

Depends on: WP1, WP2, WP3, WP4 (docs can be drafted early, finalized last).

Owns: `docs/content/**` (public docs only), `hack/agent/e2e-kind/*`,
`hack/smoke-metalman.py`.

Docs (`docs/` is public site documentation only, not design material):

- Split `docs/content/reference/agent/nspawn.md` into an isolation overview
  (comparison table, "choosing a mode"), one page per mode (`nspawn`, `none`,
  `container`), and a page on changing the mode.
- Add per-mode sections to `reference/gpu/{nvidia,amd}.md`,
  `reference/agent/{_index,configuration}.md`, `guides/agent.md`,
  `guides/operations/_index.md`, `concepts/overview.md`, `reference/cli.md`.
- Document `spec.isolation`, `status.isolationChange`, `exec` and `logs`.
- Replace `machinectl shell` recipes with `exec`/`logs`; keep the nspawn-specific
  recipes on the nspawn page only.
- Update the diagrams under `docs/static/img/` or note them as nspawn-only.

E2E:

- Parameterize `e2e.py` by mode. Default-mode tests unchanged.
- Add `none` and `container` bootstrap, soft reboot, upgrade and reset tests.
- Add mode change tests: declare `spec.isolation`, delete the Node, and check
  `nspawn` to `container` and back (in place), `none` (reprovision), the
  rollback case, and that a failed change is not retried.

Acceptance: docs build; every mode page covers requirements, bootstrap, each
operation in section 9, GPU, LocalDNS, limits and troubleshooting; links valid;
`make lint` passes; e2e results reported honestly, including anything not run.

## 12. Testing strategy

- Unit: a fake `Backend` for phase and daemon tests; golden files for rendered
  units and configs (existing pattern in `rootfs/nspawn_render_test.go`).
- Conformance: one shared Go suite run against every backend (prepare, start,
  stop, restart, exec, destroy, idempotence, failure recovery).
- Mode change: fake-backend tests for every row of section 10.2 and the
  rollback path.
- E2E: the Python QEMU suite parameterized by mode (WP5).

## 13. Open items and prerequisites

Status: WP2 was implemented without these facts; see the status note in
section 8.3. Every item below is still open. Remaining gap beyond these facts:
nothing has run on a real host or in the QEMU e2e.

1. **Azure Container Linux node facts (blocks WP2).** Run on a live node and
   record the output: `uname -r`, `containerd --version`, `runc --version`,
   `getenforce`, `modprobe wireguard && lsmod | grep wireguard`,
   `ls /lib/modules`, `systemd-sysext status`, `which systemd-nspawn machinectl`,
   `mount | grep -E ' / | /usr '`. Until then section 8.3 is provisional.
2. Whether Azure Container Linux accepts user-supplied Ignition when AKS
   provisions the node, and whether a sysext is a viable alternative delivery
   path to a container. Unverified.
3. Whether `systemd-nspawn` and `machinectl` ship on Azure Container Linux. If
   they do, `nspawn` may also work there; this does not change the plan.
4. Public-doc facts about Azure Container Linux were partly inferred from
   Flatcar. Only the lineage, read-only `/usr` with dm-verity, weekly
   image-based updates, signed UKI with Secure Boot, and enforcing SELinux are
   confirmed from Microsoft documentation.
5. WP0 step 1 re-verified all code citations in this document (section 3).
6. Approving this document is the approval required by `AGENTS.md` for the
   cross-package refactor in WP0.
