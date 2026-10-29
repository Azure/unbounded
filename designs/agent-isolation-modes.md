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
| `microvm`   | In a micro VM                                        | Reserved, design note only |

## 1.1 Delivery scope

The first delivery covers three modes: `nspawn` (existing), `none` (bare host)
and `container` (Azure Container Linux). WP0 to WP5 are in scope. `microvm` is
out of scope for this delivery: the enum value is reserved, the agent rejects
it as not implemented, and WP6 (a design note, no code) is optional and may be
deferred until `microvm` work is actually planned.

## 2. Goals and non-goals

Goals:

- One `Backend` interface that every machine operation goes through.
- A coherent story for every operation (bootstrap, start, stop, soft reboot,
  repave, reset, delete, upgrade, exec, logs, GPU, LocalDNS) in every mode.
- No behavior change for existing `nspawn` hosts. Hosts with no recorded mode
  are `nspawn`.
- A supported way to switch modes on a live host.
- Documentation that describes each mode and how to choose and migrate.

Non-goals:

- Implementing `microvm`. Only the interface leaves room for it (section 12).
- Changing the existing `nspawn` repave semantics.
- Auto-detecting a mode. The configured mode is always explicit.
- Time or effort estimates (per `AGENTS.md`).

## 3. Verification notes

Code is authoritative (`AGENTS.md`). Facts below come from an exploration pass
whose line numbers were approximate. WP0 step 1 must re-verify every file and
symbol cited here before editing. Verified during planning: the files named in
section 4 exist, and `nodeOperator` is defined at
`cmd/agent/internal/daemon/nodeoperator.go:37` with a single nspawn
implementation. There are 35 `MachineRun(` references in Go sources (the
exploration estimated about 40).

Known doc and code drift to fix while touching these files:

- `hack/smoke-metalman.py:68` cites `cmd/agent/internal/goalstates/constants.go`.
  The file is `pkg/agent/goalstates/constants.go`.
- `pkg/agent/phases/rootfs/os.go` hard-codes Ubuntu Noble apt sources, even for
  Azure Linux rootfs images.
- Deprecated constants `DaemonBinaryPath` and `NSpawnLifecycleBinaryPath` in
  `pkg/agent/goalstates/constants.go` are superseded by `hostroot`.

## 4. Current coupling (as explored)

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
- Active machine discovery: `FindActiveMachine`, keyed on
  `/etc/unbounded/agent/<m>-applied-config.json`.
- Uninstall: `internal/provision/assets/unbounded-agent-uninstall.sh`
  hard-codes `machinectl`, `/etc/systemd/nspawn`, `/var/lib/machines`.
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
   `microvm` is the known exception.
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
// Isolation selects where the node stack runs. Defaults to nspawn.
// +kubebuilder:validation:Enum=nspawn;none;container;microvm
// +optional
Isolation IsolationMode `json:"isolation,omitempty"`
```

It is additive within `v1alpha3`. No API version bump. Regenerate with
`make generate`; never edit CRD YAML by hand. `microvm` is accepted by the enum
but rejected by the agent as not implemented.

### 6.2 Agent config

Add `Isolation` to `AgentConfig` (`pkg/agent/config/config.go`). Empty means
`nspawn`. Validation rejects unknown values. Add `--isolation` to the agent
bootstrap flags and to `kubectl unbounded` manual bootstrap
(`cmd/kubectl-unbounded/app/machine_manual_bootstrap.go`).

### 6.3 Applied config

The applied-config file records `isolation`, `slot` and the owning backend. A
missing `isolation` reads as `nspawn`. `FindActiveMachine` must handle both old
and new files.

### 6.4 Version skew

- The agent rejects any unrecognized `isolation` value with a clear error.
- Machina emits a non-default `isolation` into generated agent config only for
  agent versions known to support it, otherwise it sets a condition on the
  Machine explaining why.

### 6.5 Drift

If `spec.isolation` differs from the applied mode, Machine status reports
drift. Nothing moves until an `IsolationMigrate` MachineOperation (section 9).

## 7. Backend interface

New package `pkg/agent/isolation`. Names below are a starting shape. WP0 may
adjust signatures, but must keep the responsibilities.

```go
type Mode string

const (
    ModeNSpawn    Mode = "nspawn"
    ModeNone      Mode = "none"
    ModeContainer Mode = "container"
    ModeMicroVM   Mode = "microvm"
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

    Start(ctx context.Context, r NodeRoot) error
    Stop(ctx context.Context, r NodeRoot) error
    // Restart restarts the node stack only, never the host (soft reboot).
    Restart(ctx context.Context, r NodeRoot) error
    // Destroy removes the node root and backend config for a slot.
    Destroy(ctx context.Context, r NodeRoot) error

    Status(ctx context.Context, r NodeRoot) (Status, error)

    // Exec runs a command in the node stack (replaces MachineRun).
    Exec(ctx context.Context, r NodeRoot, cmd ExecSpec) (ExecResult, error)
    // Logs streams logs for a unit in the node stack.
    Logs(ctx context.Context, r NodeRoot, opts LogOptions) (io.ReadCloser, error)
}

// NodeRoot resolves paths into the node stack's filesystem view.
type NodeRoot interface {
    Slot() string
    // Path maps an in-node absolute path to its path as seen by the agent.
    // For none this is the identity.
    Path(inNode string) string
}
```

Phases receive a `NodeRoot` and call `Path` and `Backend.Exec`. They never
reference `MachineDir`, `machinectl` or `systemd-run --machine`.

**Grants.** GPU, device and extra mount requests (`goalstates/{nvidia,amd,hostdevices,hostpaths}.go`)
become a backend-neutral `[]Grant` (kinds: mount, device, library path). Each
backend applies them its own way:

- `nspawn`: render `.nspawn` binds and `DeviceAllow`.
- `container`: render OCI mounts, devices and cgroup device rules.
- `none`: nothing to apply (host is native).
- `microvm`: documented gap (needs VFIO passthrough).

**Registry.** `isolation.New(mode, deps)` returns the backend. Unknown modes
error. Tests use a fake backend.

**nodeOperator.** The existing `nodeOperator` becomes a thin wrapper that
resolves the active backend from applied-config. It stays injected through
Daemon options so existing daemon tests keep faking it.

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
- Safety: preflight refuses to start if it finds a kubelet or containerd it does
  not own (unit, binary or running process).
- Requirements: writable `/etc`, `/opt`, `/var`. `/usr` need not be writable.
  Not supported on a read-only `/usr`; use `container`.
- Network and paths: host native. Pods share the host netns trivially.
- bpffs: no per-machine bpffs. The host `/sys/fs/bpf` is used directly. This is
  a documented difference for eBPF CNIs such as Cilium.
- Exec: runs on the host as the unit's environment. Logs: `journalctl` for the
  agent-owned units.
- Upgrade: drain, stop units, flip `current`, start units, wait for kubelet
  Ready, flip back automatically on failure. The node is briefly down.

### 8.3 `container` (Azure Container Linux)

Provisional until the node facts in section 13 are collected.

- Target host: Azure Container Linux (derived from Flatcar). Read-only `/usr`
  (dm-verity), enforcing SELinux, signed UKI with Secure Boot, image-based
  updates through AKS. Public docs do not cover runtimes, kernel modules or
  mount propagation.
- Image: reuse the `images/agent-*` rootfs images with systemd as PID 1 (an
  entrypoint is added to the image family if needed).
- Launch: the agent reuses the OCI unpack step, generates an OCI runtime bundle
  (`config.json`) over the unpacked rootfs, and runs it with `runc` or `crun`
  supplied as an agent-downloaded artifact under `/opt/unbounded`. An
  agent-owned systemd unit supervises the container. The host's docker and
  containerd are not used.
- Slots: `kube1`/`kube2` with root at `/var/lib/unbounded/containers/<slot>`.
- Privileges: privileged, shared host netns and PID as needed, host bind mounts
  for the same-path contract (invariant 3), `rshared` propagation for
  `/var/lib/kubelet`, device and cgroup rules from grants.
- SELinux: when enforcing, set `process.selinuxLabel` to `spc_t`. A preflight
  check verifies label support. Provisional.
- Requirements: writable `/etc`, `/opt`, `/var`, a loadable `runc`/`crun`, cgroup
  v2, required kernel modules available to the container.
- Provisioning: existing `ignition` `ProvisioningFormat`. No new format.
- Lifecycle hooks: a new hidden generic `node-lifecycle` subcommand with the
  same pre-start, post-start and reconcile phases. Shared logic lives in
  `pkg/agent`. `nspawn-lifecycle` is untouched.
- Exec: via `runc exec` (or equivalent) behind `Backend.Exec`. Logs: container
  journal via `journalctl -M` equivalent or `runc exec journalctl`.

### 8.4 `microvm` (reserved)

Only a design note (WP6). The interface must not preclude it. Known invariant
breaks: guest has its own network and filesystem (invariant 3 does not hold),
needs a guest kernel and a vsock control channel, GPU needs VFIO, and
host-global state (LocalDNS, WireGuard) must be bridged into the guest.

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
| Agent binary upgrade | unchanged (nspawn independent) | unchanged | unchanged |
| Uninstall script | reads mode from applied-config, mode-specific cleanup | same | same |

The order of reset steps in `daemon/reset.go` (`resetResources`) is preserved:
daemon unit, stop, WireGuard keys, config, root, bpffs, network and routes,
bootstrap unit, artifacts, systemd reload.

## 10. Migration

### 10.1 Trigger

New `MachineOperation` type `IsolationMigrate` (alongside `NodeReboot`) in
`api/machina/v1alpha3/machineoperation_types.go`, carrying `targetMode` and
`strategy` (`inPlace` or `reprovision`). Handled by the daemon controller
(`cmd/agent/internal/daemon/controller_machineoperation.go`). Editing
`spec.isolation` alone only reports drift.

### 10.2 Supported strategies

| From / To | `nspawn` | `container` | `none` | `microvm` |
|---|---|---|---|---|
| `nspawn` | n/a | in-place or reprovision | reprovision | reprovision (future) |
| `container` | in-place or reprovision | n/a | reprovision | reprovision (future) |
| `none` | reprovision | reprovision | n/a | reprovision (future) |

Reprovision is always available: drain, reset, bootstrap in the new mode.

### 10.3 In-place steps (`nspawn` <-> `container`)

1. Preflight the target backend. Fail before mutating anything.
2. Download artifacts for the target.
3. Prepare the target backend in the alternate slot.
4. Drain the node.
5. Stop the source node stack.
6. Clean up host networking as repave does today.
7. Start the target. Issue fresh kubelet credentials through the normal
   bootstrap and attestation flow. No credentials are copied. The Node name,
   labels and taints are kept.
8. Persist new applied-config (mode, slot, owning backend).
9. Wait for kubelet Ready within a bounded timeout.
10. On success, destroy the source slot and remove its applied-config.
11. On failure, stop the target, restart the source slot and report the failure
    in the operation status. The source slot is retained until step 9 passes.

Existing repave is not changed to adopt rollback in this plan.

### 10.4 State

Pod data and the image store are re-pulled. Host-global state (invariant 2)
is untouched.

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
-> WP4 -> WP5. WP6 is independent.

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
   note appended to this document before proceeding.
2. Define `Backend`, `NodeRoot`, `Grant`, `Requirements`, `ExecSpec`, `Status`
   in `pkg/agent/isolation`, plus a registry and a fake backend for tests.
3. Move all nspawn-specific code behind an `nspawn` backend: rendering
   (`rootfs/nspawn.go` and assets), `machinectlRunner`, start/stop/recovery,
   `reset` machine handling, `MachineRun` call sites (35).
4. Replace `filepath.Join(MachineDir, ...)` in phases with `NodeRoot.Path`.
5. Convert GPU, device and mount goal states to `[]Grant`; the nspawn backend
   renders them to the same `.nspawn` and `DeviceAllow` output as before.
6. Make `nodeOperator` resolve the backend from applied-config; a missing mode
   is `nspawn`.
7. Keep `kube1`/`kube2`, `nspawn-lifecycle` and `NSpawnLifecycleConfig` as is.

Acceptance:

- All existing unit tests and golden files pass unmodified
  (`rootfs/testdata/**`, nodestart, reset, daemon tests).
- `rg 'MachineDir|MachineRun|machinectl'` outside `pkg/agent/isolation/nspawn/`
  and `pkg/agent/phases/host/preflight*` returns no hits, or each remaining hit
  is documented as intentional.
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
artifact (`runc` or `crun`) download wiring.

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

### WP3: API, config, applied-config, migration

Depends on: WP0. Parallel with WP1, WP2 (stubs for backends are fine).

Owns: `api/machina/v1alpha3/{machine_types,machineoperation_types}.go`,
generated CRDs and deepcopy via `make generate`,
`pkg/agent/config/config.go`, applied-config read/write,
`cmd/agent/internal/daemon/controller_machineoperation.go`, a new
`IsolationMigrate` handler, machina agent-config generation
(`internal/provision/agent_config.go` and machina controller), metalman
passthrough, `kubectl unbounded` manual bootstrap flag.

Steps:

1. Add `spec.isolation` and the `IsolationMigrate` operation type. Run
   `make generate`.
2. Add `Isolation` to `AgentConfig` with validation. Unknown values rejected.
3. Applied-config records mode, slot, owning backend. Tolerate old files.
4. Machina gating by agent version and a Machine condition (section 6.4).
5. Drift reporting in Machine status.
6. Implement the migration orchestrator (section 10.3) on top of the
   `Backend` interface, including automatic rollback.
7. Make nspawn-only host guards conditional: the `systemd-container` install
   in `host/os.go`, the machined restart guards in `host/assets/99-*`, and the
   `images/netboot/assets/vendor-data.tmpl` machined rules.

Acceptance:

- Existing Machines without the field produce unchanged generated output
  (golden comparison).
- Migration tests with fake backends: success, preflight failure (no mutation),
  target failure (rollback to source), and unsupported pair (rejected).
- Version-skew tests: old agent version leads to a condition and no emitted
  mode.

### WP4: `exec`, `logs`, lifecycle and uninstall

Depends on: WP0, plus WP1 and WP2 for their `Exec`/`Logs` implementations.

Owns: new agent subcommands in `cmd/agent/internal/cmd/` (`exec`, `logs`),
`internal/provision/assets/unbounded-agent-uninstall.sh`,
`kubectl unbounded` help text referencing machine access.

Steps: add backend-agnostic `exec` and `logs` subcommands. Make the uninstall
script read the mode from applied-config and call mode-specific cleanup, with
a fallback to nspawn cleanup when no applied-config exists.

Acceptance: command tests per backend with fakes; the uninstall script has a
shell test for each mode and for a legacy host with no recorded mode.

### WP5: Docs and e2e

Depends on: WP1, WP2, WP3, WP4 (docs can be drafted early, finalized last).

Owns: `docs/content/**` (public docs only), `hack/agent/e2e-kind/*`,
`hack/smoke-metalman.py`.

Docs (`docs/` is public site documentation only, not design material):

- Split `docs/content/reference/agent/nspawn.md` into an isolation overview
  (comparison table, "choosing a mode"), one page per mode (`nspawn`, `none`,
  `container`, and a short "planned" `microvm` page), and a migration page.
- Add per-mode sections to `reference/gpu/{nvidia,amd}.md`,
  `reference/agent/{_index,configuration}.md`, `guides/agent.md`,
  `guides/operations/_index.md`, `concepts/overview.md`, `reference/cli.md`.
- Document `spec.isolation`, `IsolationMigrate`, `exec` and `logs`.
- Replace `machinectl shell` recipes with `exec`/`logs`; keep the nspawn-specific
  recipes on the nspawn page only.
- Update the diagrams under `docs/static/img/` or note them as nspawn-only.

E2E:

- Parameterize `e2e.py` by mode. Default-mode tests unchanged.
- Add `none` and `container` bootstrap, soft reboot, upgrade and reset tests.
- Add migration tests: `nspawn` to `container` and back, plus a reprovision
  case, including the rollback case.

Acceptance: docs build; every mode page covers requirements, bootstrap, each
operation in section 9, GPU, LocalDNS, limits and troubleshooting; links valid;
`make lint` passes; e2e results reported honestly, including anything not run.

### WP6: `microvm` design note (optional, deferred)

Depends on: nothing. Not part of the first delivery. Owns: a section appended to this document or a new
`designs/agent-isolation-microvm.md`.

Content: constraints from section 8.4, candidate hypervisors, the network and
filesystem model that replaces invariant 3, how LocalDNS and WireGuard cross
the boundary, GPU passthrough, and what the `Backend` interface would need to
add. No code.

## 12. Testing strategy

- Unit: a fake `Backend` for phase and daemon tests; golden files for rendered
  units and configs (existing pattern in `rootfs/nspawn_render_test.go`).
- Conformance: one shared Go suite run against every backend (prepare, start,
  stop, restart, exec, destroy, idempotence, failure recovery).
- Migration: fake-backend tests for every row of section 10.2 and the rollback
  path.
- E2E: the Python QEMU suite parameterized by mode (WP5).

## 13. Open items and prerequisites

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
5. WP0 step 1 re-verifies all code citations in this document.
6. Approving this document is the approval required by `AGENTS.md` for the
   cross-package refactor in WP0.
