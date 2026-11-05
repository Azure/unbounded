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
| Can a node change mode by declaring it and repaving, and back, keeping its identity? | T01 |
| Does Machine status report the applied mode and the change, and does drift appear, clear and report a failure? | T02 |
| Does a failed mode change put the node and its shared host configuration back, and is it left alone afterwards? | T03 |
| Does the uninstall script remove the node stack of each mode? | U01, U02, U03 |
| Does container mode work on its intended platform? | A01 |
| Do GPUs work in none and container mode? | G01 |

## 2. Environment

- One Kind cluster (control plane and machina controller, reached from the VM
  over a bridge) and one QEMU VM per scenario, booted from `HOST_BASE_OS`
  (default `ubuntu2404`). The harness is `hack/agent/e2e-kind/`, the same one
  CI uses for the lifecycle suite.
- This plan is run by hand, with `run-local.sh` or `isolation_plan.py`. The
  isolation suites are not added to the CI e2e workflow: each scenario builds a
  cluster and boots a VM, and the whole plan is too expensive to run on every
  change. Run it when the agent's isolation code changes.
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
| U01 | Default (no flag): uninstall script | unset | `isolation-uninstall` | - |
| U02 | none: uninstall script | none | `isolation-uninstall` | - |
| U03 | container: uninstall script | container | `isolation-uninstall` | - |
| T01 | Every ordered pair of modes | unset | `isolation-matrix` | - |
| T02 | Machine status and drift across mode changes | unset | `isolation-drift` | - |
| T03 | Failed mode change: rollback and config restore | container | `isolation-rollback` | - |
| A01 | container on Azure Container Linux | container | `isolation-operations` | `HOST_BASE_OS=acl` |
| G01 | GPU workloads in none and container mode | none | none | GPU hardware |

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
| Node stack | `systemd-nspawn@kubeN` machine | `unbounded-containerd.service`, `unbounded-kubelet.service`, `/opt/unbounded/node/<version>` | one `unbounded-node-<slot>.service`, root `/var/lib/unbounded/containers/<slot>` |
| exec / logs | `unbounded-agent exec` into the machine | `exec` on the host | `exec` into the container |
| NodeReboot | node stack restarts | node stack restarts, host boot ID unchanged | node stack restarts |
| Repave | other slot becomes active, bpffs is fresh (D01) | `current` link flips to a new version | other slot becomes active |
| AgentReset | no machine left | no units, no `/opt/unbounded/node` | no container root |
| Preflight | `nspawn-runtime` check runs | `none-backend` runs | `container-backend` runs |

### 3.2 Transitions (T01 to T03)

A mode change is declared the way an operator does it: the harness patches
`spec.isolation` on the Machine, checks that nothing moves and `IsolationDrift`
is `True` with `AppliedModeDiffers`, deletes the Node to trigger the repave, and
waits for `status.isolationChange`. Any pair that involves none is a reprovision;
nspawn and container move in place. The suites assert the strategy the pair calls
for. After every change they assert: `status.isolationChange` is `Succeeded`,
same Node name, labels and taints, the new mode recorded in the applied config
and in `status.isolation`, `IsolationDrift` `False` with
`AppliedModeMatches`, a workload and DNS run, and `exec` works.

- T01: nspawn to container (in place), container to none, none to container,
  container to nspawn (in place), nspawn to none, none to nspawn. All six ordered
  pairs. The stops reached by reprovision to none and back to nspawn also run a
  soft reboot and a host reboot, and none runs its preflight.
- T02: `IsolationDrift` in every state. An empty `spec.isolation` is
  `NoModeDeclared`. A declared unapplied mode is `AppliedModeDiffers` while
  nothing moves, and `AppliedModeMatches` once it is set back. A failed change is
  `ChangeFailed`. A change that succeeds ends at `AppliedModeMatches`; T01 asserts
  that after every change, so T02 makes none.
- T03: starts in container mode, the only mode that keeps its configuration at
  the shared host paths. Declaring the applied mode moves nothing and records no
  change. A change to nspawn is refused before the node is touched while the host
  lacks the nspawn runtime. An in-place container to nspawn change is failed
  after the source stopped; the source runs again, the recorded mode is
  unchanged, `status.isolationChange` is `Failed` with a rollback, nothing
  retries it, and a digest of the shared host configuration (containerd, kubelet,
  CNI, PKI) is identical before and after.

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

- container mode is provisional. On `ubuntu2404` C01, T01 and T03 exercise the
  runc runtime, the OCI bundle and the unit, not the Azure Container Linux
  assumptions (SELinux label, PID 1 shutdown, mount propagation). A01 is the
  scenario that checks those, and needs an ACL image.
- G01 needs GPU passthrough. It is listed so the report shows the gap.
- Rollback of a failed reprovision has no harness fault and is covered by unit
  tests only.
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
python3 hack/agent/e2e-kind/isolation_plan.py run --exclude-tag provisional
```

`--no-up` reuses a cluster and bridge you created; `--keep-env` leaves the
cluster, bridge and VM for inspection. `render <results.json> --html`
regenerates the report from saved results.
