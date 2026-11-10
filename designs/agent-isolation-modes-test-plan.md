# Agent isolation modes: test plan

Companion to [agent-isolation-modes.md](agent-isolation-modes.md). This plan
exercises the agent in each isolation mode on a real QEMU VM joined to a Kind
control plane, and produces a report of every scenario and its outcome. Unit
tests, golden tests and the backend conformance suite already cover the code
against fakes; nothing in them runs a real systemd, containerd, kubelet or
runc. This plan does.

## 1. What is tested

| Question | Scenarios |
|---|---|
| Does a host bootstrapped with no isolation setting behave exactly as before (nspawn)? | D01, D02 |
| Does every agent and host operation work in each mode? | D02, N01, X01, C01 |
| Does `unbounded-agent reset` on the host remove the node stack of each mode, and leave no reprovision handoff behind? | R01, R02, R03, R04 |
| Can a node change mode by declaring it and repaving, and back, keeping its identity? | T01 |
| Does Machine status report the applied mode and the change, and does drift appear, clear and report a refusal? | T02 |
| Does a change that cannot go ahead leave the node in its mode, and is it left alone afterward? | T03 |
| Does a failed in-place change roll back to the source and restore the shared host configuration, in both directions? | T04 |
| Does the experimental container mode run on its target host? | A01 |
| Do GPUs work in none mode? | G01 |

This plan covers the nspawn, none and experimental container modes. A host
that runs or changes to container opts in to it
(`--experimental-container-isolation` at bootstrap); the harness does that for
every container scenario and every suite that changes a node to container.

## 2. Environment

- One Kind cluster (control plane and machina controller, reached from the VM
  over a bridge) and one QEMU VM per scenario, booted from `HOST_BASE_OS`
  (default `ubuntu2404`). The harness is `hack/agent/e2e-kind/`, the same one
  CI uses for the lifecycle suite.
- This plan is run by hand, with `run-local.sh` or `isolation_plan.py`. The
  isolation suites are not added to the CI e2e workflow: each scenario builds a
  cluster and boots a VM, and the whole plan is too expensive to run on every
  change. Run it when the agent's isolation code changes. The nightly workflow
  `.github/workflows/agent-isolation-nightly.yaml` runs T01
  (`isolation-matrix`) and T03 (`isolation-rollback`) every night.
- Host needs: `docker`, `kind` v0.29 or newer, `kubectl`, `go`,
  `qemu-system-x86_64`, `qemu-img`, `genisoimage`, `iptables`, passwordless
  `sudo`, and read/write access to `/dev/kvm`.
- Each scenario starts from a fresh VM disk and a clean Machine and Node, so a
  failure cannot poison the next scenario.

## 3. Scenarios

Each scenario is one e2e suite, run with the stated `ISOLATION_MODE`.
"Unset" means no `--isolation` flag is passed, which must give nspawn.

| ID | Scenario | Start mode | e2e suite | Requires |
|---|---|---|---|---|
| D01 | Default flow with no isolation set: existing lifecycle | unset | `lifecycle` | - |
| D02 | Default (no flag): all operations | unset | `isolation-operations` | - |
| N01 | Explicit nspawn: all operations | nspawn | `isolation-operations` | - |
| X01 | none: all operations | none | `isolation-operations` | - |
| C01 | container: all operations | container | `isolation-operations` | - |
| R01 | Default (no flag): reset on the host | unset | `isolation-reset` | - |
| R02 | none: reset on the host | none | `isolation-reset` | - |
| R04 | container: reset on the host | container | `isolation-reset` | - |
| R03 | Reset after a reprovision mode change | unset | `isolation-reset-after-reprovision` | - |
| T01 | Every ordered pair of modes | unset | `isolation-matrix` | - |
| T02 | Machine status and drift across mode changes | unset | `isolation-drift` | - |
| T03 | Mode change that cannot go ahead | unset | `isolation-rollback` | - |
| T04 | Failed in-place change: rollback and config restore | container | `isolation-rollback-in-place` | - |
| A01 | container on Azure Container Linux | container | `isolation-operations` | `host-os:acl` |
| G01 | GPU workloads in none mode | none | none | GPU hardware |

`python3 hack/agent/e2e-kind/isolation_plan.py plan` prints this table from the
machine-readable catalog, which is the source the runner uses.

### 3.1 Operations suite (D02, N01, X01, C01)

In order: bootstrap and join, mode check, host root layout, controller health,
node config, persisted agent config, kube-proxy, Machine self-registration,
Machine status, preflight, `exec` and `logs`, workload and DNS, NodeReboot,
host-driven AgentUpgrade, operation AgentUpgrade, AgentUpgrade rollback,
workload, repave by MachineConfiguration version, workload, host reboot,
mode and status check, AgentReset, delete Machine, reinstall on the same
disk, join, mode check, `exec`, workload.

Mode-specific expectations the steps assert:

| Operation | nspawn | none | container |
|---|---|---|---|
| Node stack | `systemd-nspawn@kubeN` machine | `unbounded-containerd.service`, `unbounded-kubelet.service`, `/opt/unbounded/node/<version>` | one active `unbounded-node-kubeN.service`, its rootfs, bundle and `/run/bpffs/kubeN` |
| exec / logs | `unbounded-agent exec` into the machine | `exec` on the host | `exec` through `runc exec` |
| NodeReboot | node stack restarts | node stack restarts, host boot ID unchanged | container restarts, host boot ID unchanged |
| Repave | other slot becomes active, bpffs is fresh (D01) | `current` link flips to a new version | other slot becomes active, bpffs is fresh |
| AgentReset | no machine left | no units, no `/opt/unbounded/node` | no slot rootfs, bundle, unit or bpffs |
| Preflight | `nspawn-runtime` check runs | `none-backend` runs | `container-backend` runs |

### 3.2 Reset on the host (R01 to R04)

`unbounded-agent reset`, run on the host over SSH as an operator does without
the control plane, must leave a clean host: the node stack of the mode is gone
(as for AgentReset in 3.1), the agent's files and units are gone, and no
reprovision handoff is left: no `unbounded-agent-reprovision.service` unit, and
no staged agent or target configuration (which carries the bootstrap token)
under `/var/lib/unbounded-agent-reprovision`.

- R01, R02 and R04: bootstrap in nspawn, none or container, run a workload,
  reset on the host.
- R03: bootstrap in nspawn, change to none by reprovision, check no handoff is
  left once the change has succeeded, then reset on the host.

### 3.3 Transitions (T01 to T04)

A mode change is declared the way an operator does it: the harness patches
`spec.isolation` on the Machine, checks that nothing moves and `IsolationDrift`
is `True` with `AppliedModeDiffers`, deletes the Node to trigger the repave, and
waits for `status.isolationChange`. A change between nspawn and container is
made in place: the target is built in the other slot beside the source. none
has no slot to prepare a target in beside the source, so every change to or
from none is a reprovision: the host is reset and bootstrapped again in the new
mode. A reprovision happens only
when the Machine carries the annotation `unbounded-cloud.io/allow-reprovision:
"true"`, so the harness sets it before it deletes the Node and removes it once
the change is done. The suites assert the strategy the pair calls for. After
every change they assert: `status.isolationChange` is `Succeeded`, same Node
name, labels and taints, the new mode recorded in the applied config and in
`status.isolation`, `IsolationDrift` `False` with `AppliedModeMatches`, a
workload and DNS run, `exec` works, and no reprovision handoff is left.

- T01: every ordered pair of nspawn, none and container: nspawn and container
  in place, any pair with none by reprovision. Each stop also runs a soft reboot and a host reboot, and none runs its
  preflight.
- T02: `IsolationDrift` in every state. An empty `spec.isolation` is
  `NoModeDeclared`. A declared unapplied mode is `AppliedModeDiffers` while
  nothing moves, and `AppliedModeMatches` once it is set back. A change refused
  for want of the reprovision opt-in is `ChangeRefused`. A change that succeeds
  ends at `AppliedModeMatches`; T01 asserts that after every change, so T02 makes
  none. `ChangeInProgress` turns into `ChangeStale` after two hours without a
  report from the agent; no scenario waits that long, so that rule is covered by
  the controller's unit tests and the harness's own expectation by its tests.
- T03: starts in nspawn. Declaring the applied mode moves nothing and records no
  change. Declaring none without the allow-reprovision annotation and deleting
  the Node is refused: `status.isolationChange` is `Failed` with reason
  `ReprovisionNotAllowed`, `IsolationDrift` is `ChangeRefused`, the repave goes
  ahead in nspawn and the Node comes back, no handoff is staged, nothing retries
  the change, and a workload runs once the spec is set back. Declaring
  container on a host that has not opted in is refused with reason
  `ExperimentalNotEnabled` (skipped on a host that has).
- T04: starts in container. A fault injected after the target is started makes
  each in-place change (container to nspawn, nspawn to container) fail: the
  source is restarted, `status.isolationChange` is `Failed`, the journal is
  gone, and the shared host configuration is byte for byte what it was before.

## 4. Evidence and report

For every scenario the runner records start and end time, per-step result and
duration, the mode in effect at each step, the full suite log, and after the
suite a JSON snapshot of: the Machine's `spec.isolation`,
`status.isolation` and `IsolationDrift` condition, the applied-config
mode of each slot, the node-stack units, the `none` current version, `machinectl`
output, daemon state and version, host boot ID and the Node line. A failed
scenario also keeps the harness diagnostics. Bootstrap tokens are redacted from
logs and the report.

The report (`report.md`, `report.html`, `results.json` under
`tmp/isolation-report/<run-id>/`) has: environment, a summary table, an
operation by mode coverage matrix, a mode transition matrix, and per-scenario
detail with the failing step and the end of its log.

Result meanings: PASS (every step passed), FAIL (a step failed), ERROR (the
harness could not run it: cluster or VM would not start), SKIP (not applicable
here, with the reason).

## 5. Applicability and limits

- G01 needs GPU passthrough. It is listed so the report shows the gap.
- A failed reprovision, including a handoff that gives up after its runs are
  used up and must remove its unit and staged configuration, has no harness
  fault and is covered by unit tests only. R03 and T01 check the handoff is gone
  after a reprovision that succeeded, and T03 that a refused change stages none.
- A01 needs an Azure Container Linux image (`HOST_BASE_OS=acl` and a gallery
  export or `HOST_IMAGE_PATH`); it is skipped otherwise. C01 on the default
  host OS exercises runc, the bundle and the unit, not the target host.
- T02 and T03 set `spec.isolation` on the Machine; they are exploratory for
  side effects on generated agent config.
- D01 contains nspawn-only checks (host and nspawn distro pair, bpffs isolation
  across repave) that other modes skip by design.
- No scenario here replaces `make test`; run that first.

## 6. Running it

```sh
# once: groups take effect on a new login, or wrap the command in `sg kvm -c`
PATH="$PWD/tmp/bin:$PATH" python3 hack/agent/e2e-kind/isolation_plan.py run
python3 hack/agent/e2e-kind/isolation_plan.py run --scenario D01 T01   # a selection
python3 hack/agent/e2e-kind/isolation_plan.py run --category transitions --fail-fast
python3 hack/agent/e2e-kind/isolation_plan.py run --exclude-tag transition
```

`--no-up` reuses a cluster and bridge you created; `--keep-env` leaves the
cluster, bridge and VM for inspection. `render <results.json> --html`
regenerates the report from saved results.
