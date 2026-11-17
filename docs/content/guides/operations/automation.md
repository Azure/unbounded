---
title: "Automation Patterns"
description: "Automate machine lifecycle operations with the MachineOperation API, label selectors, watches, and GitOps or CI workflows."
weight: 4
---

The `MachineOperation` API is the automation surface for day-2 work on Unbounded
machines. Instead of logging into a host to power it off, reboot it, upgrade its
agent, or replace it, you create a cluster-scoped resource that describes the
work, and the component that owns that boundary carries it out. This page shows
how to drive that API from scripts, controllers, GitOps pipelines, and CI jobs,
and how to decide what to automate and what to keep manual.

The mental model is simple. A `Machine` holds desired state for a host and its
node. A `MachineOperation` is a job: it records one action, runs once, reports a
terminal phase, and is then retained or garbage collected according to its TTL.
A selector-based operation is a job that agents or controllers resolve into
targets themselves, rather than a long-lived desired state you edit over time.
Keep that distinction in mind and most automation questions answer themselves.

## Desired State, Controllers, and Idempotency

### Declarative Inputs and Reconciliation

The durable inputs you manage declaratively are `Machine` resources, their
configuration through `MachineConfiguration` and `MachineConfigurationVersion`,
and the credentials those machines use. Controllers reconcile those objects
toward their declared state continuously, which is why reapplying the same
manifest is safe. For example, a `MachineConfiguration` whose
`spec.machineSelector` matches a `Machine` assigns that configuration to the
machine, and a machine that declares `spec.isolation` changes mode at its next
repave rather than in the middle of a running workload.

`MachineOperation` participates in that model at a lower level. It is input for
one execution, not an instruction that runs forever. When you write
automation, treat machine and configuration objects as state to converge, and
operations as events to submit. Mixing the two, for example keeping a
`HostPowerOff` manifest in an auto-synced directory that prunes finished
objects, produces repeated destructive work rather than convergence.

There is one reconciliation rule worth knowing when you generate operations from
templates. The API server accepts updates to the spec of an operation that is
not a `HostAllocate` or `HostRelease`, but a completed or failed operation is
never re-executed. A terminal operation is only retained or deleted. If you need
to run the same action again, create a new `MachineOperation` with a new name
instead of editing the old one.

### Operations Are Records of Work

A `MachineOperation` is cluster-scoped, has the short name `mop`, and carries a
status subresource. Its `spec.operationKind` selects the action from a fixed
enum. The column output is deliberately small so you can scan a fleet quickly:
the machine reference, the operation kind, the phase, and the age. Everything
else you need for diagnosis lives in `status`, including a human-readable
message, timestamps, the machine generation that was observed, and per-target
detail when a controller resolves more than one machine.

The phases are `Pending`, `InProgress`, `Complete`, and `Failed`. `Complete` and
`Failed` are terminal. A new operation starts with an empty phase and becomes
`InProgress` when an executor picks it up, but a controller can leave it
`Pending` while it waits for an older conflicting host operation to finish.
That is normal queueing, not a stall, and the status message names the operation
being waited on.

### Idempotency in Practice

Several guarantees make retries safe. The API server rejects a `HostAllocate` or
`HostRelease` whose spec changed after creation, so an allocation transition
cannot be silently repointed at a different machine or allocation. A host
operation that is already `InProgress` is not executed a second time by the
external power controller unless the provider marks the action replay-safe, so
a controller restart does not power-cycle a machine twice. An agent executes a
selector-based operation only while its phase is empty or `Pending`, and only
when its local labels match.

What is not idempotent is the act of creating work. Two operation objects with
different names are two jobs, even if they target the same machine and kind.
Generate names that are unique per run, such as
`<machine>-<kind>-<timestamp>`, and make your automation check for an existing
operation before creating another one when duplicate submissions are possible.
The convenience commands for individual actions already build names this way.

## Targeting a Single Machine

For one machine, set `spec.machineRef` to the `Machine` name and
`spec.operationKind` to the action. The following manifest reboots the node stack
on `worker-01` and keeps the completed record for five minutes:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: worker-01-node-reboot-20261117-120000
spec:
  machineRef: worker-01
  operationKind: NodeReboot
  ttlSecondsAfterFinished: 300
```

Apply it and watch the operation:

```bash
kubectl apply -f worker-01-node-reboot.yaml
kubectl get mop worker-01-node-reboot-20261117-120000 -w
```

The same request is available without a file:

```bash
kubectl unbounded machine operation create worker-01-node-reboot \
  --kind NodeReboot --machine worker-01 --wait
```

`--wait` streams progress and exits nonzero if the operation reaches `Failed`.
You can wait on an operation that already exists with
`kubectl unbounded machine operation wait <name>`, and both commands accept
`--timeout`. The CLI accepts the eight node, agent, and host kinds through
`--kind`. `HostAllocate` and `HostRelease` are internal allocation transitions
and are not created with this command. The convenience commands
(`node-reboot`, `host-hard-reboot`, `host-soft-reboot`, `power-off`, `power-on`,
`agent-upgrade`, `agent-reset`, and `replace`) add the right confirmation
prompts, parameters, and defaults for their action.

Parameters are passed as string key-value pairs. They are operation-specific;
`AgentUpgrade` requires `downloadURL` and accepts an optional `sha256` digest,
while operations that need no parameters omit `spec.parameters` entirely. The
following table lists the kinds and the component that executes them, which is
the part that determines whether a selector is allowed.

| Operation | Executor | Selector supported |
| --- | --- | --- |
| `NodeReboot` | in-host agent | yes |
| `AgentUpgrade` | in-host agent | yes |
| `AgentReset` | in-host agent | yes |
| `HostSoftReboot` | in-host agent | yes |
| `HostHardReboot` | provider controller or metalman | provider dependent |
| `HostPowerOff` | provider controller or metalman | provider dependent |
| `HostPowerOn` | provider controller or metalman | provider dependent |
| `HostReplace` | provider controller or metalman | provider dependent |
| `HostAllocate` | allocation controller | no |
| `HostRelease` | allocation controller | no |

`ttlSecondsAfterFinished` is optional and must be zero or greater. Zero keeps a
terminal record indefinitely, and omitting the field does the same. Convenience
commands for host operations default to 300 seconds so repeated manual runs do
not accumulate. Name
the operation after the machine and action when you generate manifests by hand;
the CLI builds names in the form `<machine>-<action>-<UTC timestamp>` and trims
them to the DNS subdomain limit.

## Targeting Multiple Machines with machineSelector

Labels are the grouping mechanism for fleet automation. Put labels that express
a maintenance group, pool, site, hardware class, or role on your `Machine`
objects, then select that group with `spec.machineSelector`. A selector is a
standard Kubernetes label selector, so it supports `matchLabels` for equality
and `matchExpressions` for `In`, `NotIn`, `Exists`, and `DoesNotExist`. The
following operation scopes an agent upgrade to machines labeled `pool=gpu` in
the `lab` site:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: gpu-pool-agent-upgrade-20261117
spec:
  machineSelector:
    matchLabels:
      pool: gpu
    matchExpressions:
      - key: unbounded-cloud.io/site
        operator: In
        values: ["lab"]
  operationKind: AgentUpgrade
  parameters:
    downloadURL: https://example.com/unbounded-agent-linux-amd64.tar.gz
  ttlSecondsAfterFinished: 600
```

Create the same request from the CLI with `--selector`, which takes the same
syntax as `kubectl` label selection:

```bash
kubectl unbounded machine operation create gpu-pool-agent-upgrade \
  --kind AgentUpgrade --selector 'pool=gpu,unbounded-cloud.io/site=lab' \
  --param downloadURL=https://example.com/unbounded-agent-linux-amd64.tar.gz \
  --wait
```

`--machine` and `--selector` are mutually exclusive; the CLI requires exactly
one of them. Creating the operation is the apply step. Watching is the same as
for a single machine:

```bash
kubectl get mop gpu-pool-agent-upgrade -w
kubectl describe mop gpu-pool-agent-upgrade
```

Selector support depends on the executor, not on the API. The in-host agent
handles `NodeReboot`, `AgentUpgrade`, `AgentReset`, and `HostSoftReboot`, and it
evaluates `spec.machineSelector` itself. It matches the selector against its
Node labels first and falls back to the Machine labels when the Node object does
not exist yet. Each agent makes that decision independently, and there is no
per-machine child resource on this path: the operation status is a shared
summary written by the agent that executes it. Use a selector when a shared
record is enough, and confirm the effect on the machines themselves. When you
need a result per machine, create one operation per machine.

Metalman, which owns bare-metal host operations, resolves selectors centrally
and records one entry in `status.targets` for each machine it adopts. Its
selectors are site-scoped: the selector must name exactly one
`unbounded-cloud.io/site`, and that site must be the metalman instance's site.
A selector with no site label, with two sites, or with a multi-value `In` on
the site key is rejected. Metalman also filters the result to machines it owns
and that are bare-metal Redfish targets, so a broad selector cannot make one
site's controller act on another site's hosts. Within a single operation it
advances a bounded number of targets concurrently, controlled by metalman's
`--operation-max-concurrent-machines` flag (10 by default).

The external power controller that handles cloud VMs does not expand selectors.
It only processes operations with `spec.machineRef` set, and it logs and returns
for selector operations. A `HostHardReboot`, `HostPowerOff`, `HostPowerOn`, or
`HostReplace` aimed at cloud VMs through a selector therefore stays with an
empty phase and no message, which is easy to misread as a stuck controller. If
a host operation must run against cloud VMs, create one operation per machine,
which is the subject of the next section. Provider support is a separate
constraint: an operation kind that the machine's provider does not implement
fails with `UnsupportedOperation` rather than being silently skipped.

## Batch Host Operations

Cloud host operations need one `MachineOperation` per machine. A batch is
therefore a loop that creates independent operations, not a single operation
with a list. The following loop submits a hard reboot for every machine in a
pool, using a unique name per machine:

```bash
for machine in $(kubectl get machines -l pool=gpu -o jsonpath='{.items[*].metadata.name}'); do
  kubectl unbounded machine operation create "${machine}-hard-reboot-$(date +%s)" \
    --kind HostHardReboot --machine "$machine" --ttl 600
done
```

The loop creates work quickly. Each operation is queued by its own controller,
and the external power controller serializes host operations per machine: if an
older host operation for the same `machineRef` is unfinished, the newer one
stays `Pending` with a message such as `waiting for older host operation
<machine>-host-hard-reboot-...`. Different machines are independent, so a batch
does not become a fleet-wide serial queue.

Decide where you want to block. Without `--wait`, the loop submits every
operation and returns immediately, which is the right choice when you want the
whole fleet to converge in parallel and will inspect results afterward. With
`--wait`, the loop blocks on each machine in turn, which turns a batch into a
rolling sequence. For a batch of host replacements, that serialization is often
what you want, because it limits how many hosts are offline at once. For a
power-off of idle capacity at the end of the day, it usually is not.

Every generated operation should carry its own retention value.
`--ttl 600` keeps records for ten minutes after their terminal phase, which is
enough to investigate a failure without leaving a permanent fleet of finished
objects. If the batch is part of a change record, use a longer TTL or leave it
unset and export the operation list before deleting it. Watch the batch while it
runs, then list failures by phase, as described in
[Monitoring Operations](#monitoring-operations).

## Scaling In and Out

### Scale In (Power Off)

Scaling in a set of machines is a scheduled `HostPowerOff` per machine. Use it
for off-hours capacity, lab hosts, or GPU pools that you only need during
training windows. The Machine and its Node registration are not removed: the
host is powered down, so the Node stops reporting and eventually becomes
`NotReady`, while the `Machine` remains in the cluster and can be powered back
on later.

```bash
for machine in $(kubectl get machines -l pool=batch -o jsonpath='{.items[*].metadata.name}'); do
  kubectl unbounded machine operation create "${machine}-poweroff-$(date +%s)" \
    --kind HostPowerOff --machine "$machine" --ttl 600
done
```

The convenience form is `kubectl unbounded machine power-off NAME`, which waits
for completion by default and accepts `--ttl` and `--timeout`. After the
operations finish, verify that the intended machines are still registered and
that nothing else was selected:

```bash
kubectl get machines
kubectl get machines -l pool=batch
kubectl get nodes
```

The Machines remain in the first two listings; the third shows the
corresponding Nodes as `NotReady` after their grace period. If a Machine is
missing, the selector or the machine list was wrong rather than the power
operation having removed it. If an operation is `Failed`, the provider or BMC
rejected the power request; fix provider credentials or capabilities before
retrying, because retrying the same operation object does nothing.

Run scale-in from a CronJob or your scheduler when the schedule is predictable.
The job needs a ServiceAccount that can create and watch `machineoperations`,
and it should generate unique operation names or check for an existing operation
before creating one. A suspended CronJob is the simplest way to pause a
schedule without deleting it.

### Scale Out (Power On)

Scale out is the inverse: one `HostPowerOn` per machine, followed by a wait for
the Node to rejoin. The host boots, the agent starts, and the node stack comes
back. Whether the same Node name returns depends on the Machine's declared node
name, which is `spec.kubernetes.nodeRef.name` when set and the Machine name
otherwise.

```bash
for machine in $(kubectl get machines -l pool=batch -o jsonpath='{.items[*].metadata.name}'); do
  kubectl unbounded machine operation create "${machine}-poweron-$(date +%s)" \
    --kind HostPowerOn --machine "$machine" --ttl 600
done
```

Monitor the rejoin rather than assuming that a `Complete` power operation means
a ready node. The node name is the Machine name unless
`spec.kubernetes.nodeRef.name` overrides it; this example waits for a node named
`worker-03`:

```bash
kubectl get nodes -w
kubectl wait --for=condition=Ready node/worker-03 --timeout=15m
```

`HostPowerOn` only reports that the provider or BMC accepted and completed the
power request. A node that stays `NotReady` after a successful power-on usually
has a bootstrap or configuration problem, so inspect the Machine's
`AgentBootstrapped`, `Provisioned`, and `RepavePending` conditions. If the
Machine reports `ConfigurationPending`, no configuration is bound to it yet and
the node will not converge.

## Rolling Upgrades

### Agent Upgrade

`AgentUpgrade` replaces the host-side `unbounded-agent` binary using a
blue-green slot layout. The agent downloads the tarball from `downloadURL`,
verifies `sha256` when supplied, extracts only the root `unbounded-agent`
binary, validates it, switches the current symlink, and restarts the daemon.
The previous binary is preserved as the last-good slot, and if the new binary
fails validation or the daemon fails to start, recovery rolls back to that slot
and reports the operation as `Failed` with reason `DaemonFailed`.

For a fleet, upgrade sequentially and stop at the first failure. This keeps the
blast radius to one host and leaves you with a working agent on every machine
you have already visited:

```bash
SHA256_DIGEST=""
for machine in $(kubectl get machines -l pool=workers -o jsonpath='{.items[*].metadata.name}'); do
  echo "upgrading ${machine}"
  kubectl unbounded machine agent-upgrade "$machine" \
    --download-url https://example.com/unbounded-agent-linux-amd64.tar.gz \
    --sha256 "$SHA256_DIGEST" --ttl 3600 || break
done
```

`--download-url` is required. `--sha256` is optional but recommended for
automation, because it turns a corrupted or substituted tarball into a failed
download instead of a bad binary. Set `SHA256_DIGEST` in the environment before
running the loop, or leave it empty to skip verification. The convenience
command waits for the operation by default, so the loop does not start the next
machine until the current one reaches a terminal phase. Omitting `--sha256` is
fine when you cannot compute the digest ahead of time, but record the digest you
used if your change process requires provenance.

The operation completes only after the restarted daemon reports back, so a
successful `Complete` means the new binary is running. A failure usually falls
into a small set of causes: a missing or invalid `downloadURL`, a download or
extraction error, a digest mismatch, a binary validation failure, or a daemon
crash after restart. Find failed upgrades by phase and read the message:

```bash
kubectl get mop -o jsonpath='{range .items[?(@.status.phase=="Failed")]}{.metadata.name}{"\t"}{.status.message}{"\n"}{end}'
```

A selector-based `AgentUpgrade` is supported by the agent path, but it should be
reserved for cases where a shared operation record is acceptable for the whole
group. For production fleets, the sequential loop gives you a clear stopping
point and a per-machine record. If you do use a selector, scope it tightly with
a site label and a maintenance label that you remove as machines are upgraded.

### Node Upgrade via Recreation

A node upgrade replaces the node stack and rootfs on an existing host without
replacing the host. The durable record of the desired version is the
`MachineConfigurationVersion` bound to the Machine. The trigger for applying it
is deletion of the Kubernetes `Node` object, which the agent watches; the agent
then repaves the node stack with the desired configuration and rejoins it. This
is the `OnDelete` update strategy, which is the default for a
`MachineConfiguration`.

The sequence for one node is cordon, drain, delete the Node, and wait. Set
`NODE_NAME` to the node you are upgrading:

```bash
NODE_NAME=worker-03
kubectl cordon "$NODE_NAME"
kubectl drain "$NODE_NAME" --ignore-daemonsets --delete-emptydir-data --timeout=10m
kubectl delete node "$NODE_NAME"
for _ in $(seq 1 120); do
  kubectl get node "$NODE_NAME" >/dev/null 2>&1 && break
  sleep 5
done
kubectl wait --for=condition=Ready "node/${NODE_NAME}" --timeout=20m
```

Drain before delete. Deleting the Node while pods are still assigned to it
terminates those pods abruptly instead of evicting them, ignores
PodDisruptionBudgets, and can lose data written to `emptyDir` volumes.
`--ignore-daemonsets` is required because DaemonSet pods cannot be evicted,
and `--delete-emptydir-data` acknowledges that pods using `emptyDir` will lose
that data. A drain that hangs is usually waiting on a PodDisruptionBudget or a
pod with no controller; resolve that before deleting anything.

The built-in node recreation path does not need CNI dataplane cleanup: the agent
creates a fresh nspawn node and joins it, so the built-in path has no
CNI-specific cleanup step. If you run a third-party CNI such as Cilium, check
that project's node removal guidance before deleting the Node, because its
agents and identities may need their own cleanup or may clean up automatically.

Upgrade one node at a time and wait for readiness before moving on. A loop that
issues all deletes at once turns a rolling upgrade into an outage:

```bash
for node in $(kubectl get nodes -l pool=workers -o jsonpath='{.items[*].metadata.name}'); do
  kubectl cordon "$node"
  kubectl drain "$node" --ignore-daemonsets --delete-emptydir-data --timeout=10m || break
  kubectl delete node "$node"
  for _ in $(seq 1 120); do
    kubectl get node "$node" >/dev/null 2>&1 && break
    sleep 5
  done
  kubectl wait --for=condition=Ready "node/${node}" --timeout=20m || break
done
```

The inner loop waits for the new Node object to register before the readiness
wait, because `kubectl wait` on a name that does not exist yet returns an error
instead of waiting. After ten minutes the loop stops polling and the readiness
wait reports the missing node.

After the node returns, check that the Machine reports the desired version. The
`RepavePending` condition is `True` while the desired configuration has not been
applied. With a pinned `spec.configurationRef.version`, it becomes `False` with
reason `Applied` once the running node reports that version. Set `MACHINE_NAME`
to the machine you upgraded:

```bash
MACHINE_NAME=worker-03
kubectl get machine "$MACHINE_NAME" -o jsonpath='{range .status.conditions[?(@.type=="RepavePending")]}{.status}{"\t"}{.reason}{"\n"}{end}'
```

On a repaved node the new `Node` object is normally schedulable; if yours comes
back with `SchedulingDisabled`, uncordon it explicitly.

### Host Replacement

`HostReplace` replaces the host itself: for a cloud VM the provider creates a
new VM with the required disks and agent configuration, and for bare metal
metalman rewrites the host image and configures first boot. Use it only when
the kernel, operating system, or hardware identity must change, not for a
Kubernetes version upgrade.

Replace hosts one at a time, wait for the operation, and then wait for the node:

```bash
for machine in $(kubectl get machines -l pool=workers -o jsonpath='{.items[*].metadata.name}'); do
  kubectl unbounded machine replace "$machine" --force --ttl 3600 || break
  for _ in $(seq 1 360); do
    kubectl get node "$machine" >/dev/null 2>&1 && break
    sleep 5
  done
  kubectl wait --for=condition=Ready "node/${machine}" --timeout=30m || break
done
```

`--force` suppresses the interactive confirmation and is the flag to use in
CI. The command waits for the operation by default. `HostReplace` reports
`Complete` when the host is ready, which for a PXE machine means the node exists,
not that the Kubernetes Node is `Ready`, so the explicit node wait matters.
Replacement is slow and produces boot media work, so a longer TTL than the
five-minute default is appropriate; 3600 seconds keeps the record long enough
to inspect after the node rejoins. If the node name differs from the machine
name, wait on the declared node name instead.

## Troubleshooting Escalation

When a machine misbehaves, escalate in order of increasing blast radius and
increasing loss of local state. Stop as soon as the machine is healthy. The
order below assumes you have already checked the Machine conditions, the node
status, and the operation history, because a `ConfigurationPending` or
`RepavePending` machine needs configuration work rather than a reboot.

1. `NodeReboot` restarts the node stack in place. Use it for a stuck kubelet or
   containerd, a wedged nspawn node, or a node stack that will not come back
   cleanly. The rootfs and node identity are preserved.
2. `HostSoftReboot` reboots the host operating system from inside the host when
   the agent still responds, and `HostHardReboot` power-cycles the host through
   the provider or BMC when the host or agent is unresponsive. The hard reboot
   is out of band, so it works when SSH does not. Use the soft reboot when the
   agent is healthy and you need a controlled OS restart, and escalate to the
   hard reboot when it is not.
3. Node recreation deletes the Node object and lets the agent repave the node
   stack with the desired configuration. Use it when the host boots but the node
   stack or rootfs is corrupt, or when you are applying a new configuration
   version. The host is untouched.
4. `HostReplace` replaces the entire host. Use it for a corrupted host OS, a
   failed in-place recovery, a required image change, or a hardware identity
   change. It is the most disruptive repair short of decommissioning.
5. `AgentReset` is not a repair step. It removes the agent and all managed
   resources from the host as the first stage of decommissioning. After a reset
   the machine must be bootstrapped again, usually with SSH access, so do not
   use it to fix a host you intend to keep running.

A stepwise example makes the tradeoff concrete. A node is `NotReady`, the
kubelet is unresponsive, and the Machine is `Ready`. Reboot the node:

```bash
kubectl unbounded machine node-reboot worker-03 --wait
kubectl wait --for=condition=Ready node/worker-03 --timeout=10m
```

If the node does not return and the agent is unreachable, power-cycle the host:

```bash
kubectl unbounded machine host-hard-reboot worker-03 --wait
kubectl get nodes
```

If the host returns but the node stack still fails to start, repave by deleting
the Node after draining it. If the host itself is corrupt, replace it. Keep the
failed operations until you have captured their messages, then let the TTL or an
explicit delete remove them.

## Monitoring Operations

### Watch All Operations

`kubectl get mop -w` is the live view. It prints one row per operation using
the printer columns the CRD defines: name, machine, operation, phase, and age.
Because operations are cluster-scoped, you do not need `--all-namespaces`. Use a
short watch timeout in scripts so a forgotten watch cannot run forever:

```bash
timeout 300 kubectl get mop -w
```

For a single operation, `kubectl get mop <name> -w` is quieter, and
`kubectl describe mop <name>` shows the message, timestamps, conditions, and
target details. During an incident, `describe` is usually more informative than
the watch because it preserves the current message rather than scrolling past
earlier phases.

### Filter by Phase

Custom resources do not support arbitrary field selectors on `status`, so use a
JSONPath filter or a custom column. To list failures with their messages:

```bash
kubectl get mop -o jsonpath='{range .items[?(@.status.phase=="Failed")]}{.metadata.name}{"\t"}{.spec.operationKind}{"\t"}{.status.message}{"\n"}{end}'
```

To list work that has not finished, including operations that are queued behind
another host operation:

```bash
kubectl get mop -o jsonpath='{range .items[?(@.status.phase=="Pending")]}{.metadata.name}{"\t"}{.spec.operationKind}{"\t"}{.status.message}{"\n"}{end}'
kubectl get mop -o jsonpath='{range .items[?(@.status.phase=="InProgress")]}{.metadata.name}{"\t"}{.spec.operationKind}{"\t"}{.status.message}{"\n"}{end}'
```

An empty phase means the operation has been accepted but no controller has
written status yet. A `Pending` operation with a message that names another
operation is queued, not broken. A selector operation with no phase and no
message may be one that the external power controller will never process, which
is why cloud host operations should use `machineRef`.

A compact custom-columns view is easier to scan than JSONPath when you are
looking at a whole fleet:

```bash
kubectl get mop -o custom-columns='NAME:.metadata.name,KIND:.spec.operationKind,MACHINE:.spec.machineRef,PHASE:.status.phase,TTL:.spec.ttlSecondsAfterFinished'
```

### Watch Conditions

Every operation carries a `Completed` condition in `status.conditions`. The
condition is `True` when the operation reaches `Complete` and `False` when it
reaches `Failed`, with a reason and message that mirror the phase. That makes it
a good readiness signal for automation that already uses `kubectl wait`:

```bash
kubectl wait --for=condition=Completed mop/worker-03-node-reboot-20261117-120000 --timeout=15m
```

Be aware of the asymmetry: `kubectl wait` returns when the condition becomes
`True`, so a failed operation makes it wait until the timeout rather than fail
immediately. The CLI wait command is better for CI because it returns a nonzero
exit as soon as the phase is `Failed`:

```bash
kubectl unbounded machine operation wait worker-03-node-reboot-20261117-120000 --timeout=15m
```

Machine conditions are the other half of the picture. `Provisioned`,
`SSHReachable`, `AgentBootstrapped`, `RepavePending`, and `IsolationDrift`
describe the host and node, not the job. A successful operation on a machine
with a false `AgentBootstrapped` condition does not mean the node is healthy.

### View Machine and Operation Status Together

List both resource types in one command to correlate a machine's phase with the
operations aimed at it:

```bash
kubectl get machines,mop
```

The machine table uses the Machine printer columns: name, host, phase,
Kubernetes version, and age. The operation table uses name, machine, operation,
phase, and age. The `host` column shows `spec.ssh.host` when the Machine declares
an SSH connection, which is a useful cross-check when a Machine name and an SSH
host name differ. For selector operations, `machine` is empty because the
operation carries a selector instead of a single reference. When the controller
resolves targets itself, such as metalman on a bare-metal site, `status.targets`
holds the per-machine view. For a metalman `HostPowerOff` across a site, that
view is:

```bash
OPERATION_NAME=site-a-poweroff-20261117
kubectl get mop "$OPERATION_NAME" \
  -o jsonpath='{range .status.targets[*]}{.machineRef}{"\t"}{.phase}{"\t"}{.message}{"\n"}{end}'
```

## Integrating with GitOps and CI

A `MachineOperation` manifest is ordinary YAML, so any GitOps controller can
apply it. The question is whether it should. Continuous reconciliation is
designed for durable desired state, while an operation is a one-shot event.
If a GitOps application automatically prunes resources and the operation has a
TTL, the deletion triggers another sync, which recreates the operation and runs
the action again. That is harmless for a read-only operation and dangerous for
`HostReplace` or `AgentReset`.

Choose one of two patterns. For declarative, reviewed changes, keep operation
manifests in a directory or application that is synced manually, or use sync
hooks and waves that run the operation once and do not self-heal it. For
imperative pipelines, let CI create the operation with a unique name, wait for
the terminal phase, archive the YAML, and let the TTL clean it up. Do not mix
the two by leaving an auto-synced operation manifest with a short TTL.

A CI step can rely on exit codes and validate the manifest before touching the
cluster. Given a reviewed manifest for the machine you are replacing, the step
looks like this:

```bash
mkdir -p artifacts
kubectl apply --dry-run=server -f worker-03-host-replace.yaml
kubectl apply -f worker-03-host-replace.yaml
kubectl unbounded machine operation wait worker-03-host-replace-20261117 --timeout=45m
kubectl get mop worker-03-host-replace-20261117 -o yaml > "artifacts/worker-03-host-replace.yaml"
```

`--dry-run=server` catches CRD validation errors such as an unsupported
`operationKind`, a missing `machineRef` on a `HostAllocate`, or a negative TTL.
Capturing the YAML before the TTL expires gives you the message and timestamps
for the change record. If your pipeline creates operations directly rather than
applying files, use `kubectl unbounded machine operation create` and its
`--dry-run`, `-o`, and `--wait` flags for the same effect.

Grant the pipeline's identity only what it needs. Creating and reading
operations requires `create`, `get`, `list`, and `watch` on `machineoperations`,
and reading the status subresource requires `get` on
`machineoperations/status`. Deleting expired records requires `delete`. Because
operations are cluster-scoped, the grant is a `ClusterRole` and
`ClusterRoleBinding`, not a namespaced role. Keep destructive kinds
(`HostReplace`, `AgentReset`) behind a separate pipeline or approval gate rather
than exposing them to every job.

Concurrency needs its own control. A CI system that retries a failed job can
submit the same destructive operation twice under different names. Use a
pipeline concurrency lock per machine or per site, and make the job check for an
existing non-terminal operation for the machine before creating another. The
`HostAllocate` and `HostRelease` spec immutability protects an allocation
transition from edits, but it does not prevent you from creating a second
operation object.

## Failure Handling

Reaching `Failed` is terminal. Nothing reopens a terminal operation: retrying
means fixing the cause and creating a new operation. How a failure is reached
differs by executor. Metalman retries a target's external action within the
operation up to `--operation-max-attempts` (three by default) before failing
that target, while the external power controller and the in-host agent record a
failure without retrying the operation. Your own automation should be equally
conservative: an automatic retry of a power cycle or a host replacement can turn
a transient provider error into repeated disruption. Read `status.message` and
the `Completed` condition reason first, then decide whether the failure is
permanent.

Failures that will not improve on retry include an unsupported operation for the
provider (`UnsupportedOperation`), a missing machine (`MachineNotFound`), an
invalid host reference (`InvalidHost` or `InvalidProviderRef`), a rejected
allocation target, and an invalid selector. Fix the manifest or the machine
instead of resubmitting. Failures that may be transient include provider rate
limits, BMC timeouts, download errors during an agent upgrade, and a machine
that is temporarily unreachable. Retry those with a new operation after a
backoff, and cap the number of attempts in your automation.

When a selector operation resolves multiple targets, failure is per target.
Metalman records phases and messages in `status.targets`, so some machines can
be `Complete` while others are `Failed`. Do not rerun the whole selector
blindly; select the failed machines by their labels or by the target list and
submit targeted operations. The same caution applies to a batch loop: track
which machine names succeeded so a retry does not repeat the entire batch.

Long-running provider operations can stall without failing. A target that
exceeds its expected provider duration is reported with the
`ProviderOperationStalled` condition, while the controller keeps polling and
does not count polling as a retry attempt. A stalled operation is a signal to
check the provider console or BMC, not necessarily to delete the operation.
Deleting an operation removes the record and stops reconciliation of that
object, but it does not undo an external action that already happened. Confirm
the actual machine power state before resubmitting.

## Automatic Cleanup

`spec.ttlSecondsAfterFinished` requests retention. When the field is set, the
controller that owns the operation deletes it that many seconds after
`status.completedAt`. The value must be zero or greater; zero means keep
indefinitely, and omitting the field also means keep indefinitely. Retention is
measured from the terminal timestamp, so cleanup happens close to the
configured time without a separate cleanup job.

Use shorter TTLs for high-frequency operations such as nightly power-off and
power-on, where a long history is noise: 300 to 900 seconds is usually enough to
catch a failure. Use longer TTLs for host operations you may need to audit, such
as replacement and reboots, and for metalman operations where per-target
progress is recorded. A multi-hour or multi-day retention window costs little
because operations are small objects, and it preserves the message and
timestamps that explain what happened.

{{< callout type="warning" >}}
In this codebase, TTL deletion is implemented by the shared external power
controller and by metalman. The in-host agent records completion but does not
delete `MachineOperation` objects. If your automation depends on records
disappearing for `NodeReboot`, `AgentUpgrade`, `AgentReset`, or `HostSoftReboot`,
delete them explicitly or prune them with your GitOps tool, and do not rely on
`ttlSecondsAfterFinished` alone.
{{< /callout >}}

For operations created by hand, `kubectl delete mop <name>` is enough. If you
create a batch from manifests, put a batch label on the objects and delete the
group in one request with `kubectl delete mop -l batch=nightly`. If you create
them with the CLI, they have no batch label, so list them, filter by the machine
name prefix, and delete the finished ones together. Keep the most recent
completed record for any machine whose history matters, so a later
investigation can compare a successful run with a failed one.

## See Also

- [Machine operations overview]({{< relref "guides/operations" >}})
- [Host operations]({{< relref "guides/operations/host-operations" >}})
- [Node operations]({{< relref "guides/operations/node-operations" >}})
- [Agent operations]({{< relref "guides/operations/agent-operations" >}})
- [`MachineOperation` CRD reference]({{< relref "reference/machina-crd" >}})
- [CLI reference]({{< relref "reference/cli" >}})

