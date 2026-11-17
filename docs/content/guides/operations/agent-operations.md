---
title: "Agent Operations"
description: "Stage, activate, verify, and roll back host agent upgrades, and remove an agent installation safely with AgentReset."
weight: 3
---

The `unbounded-agent` is the process on the host that builds the node, watches
`MachineOperation` resources addressed to its `Machine`, and performs the work
that only the host itself can do. Two operation kinds act on the agent
installation: `AgentUpgrade` replaces the agent binary, and `AgentReset` removes
the agent and everything it installed, including the node stack. Both are
executed by the running daemon, so they need a reachable API server and a
healthy agent, and neither needs a provider API, a BMC, or SSH for the action
itself.

The mental model is a pair of switches with very different cost. An upgrade is
designed to be reversible: the new binary is staged in an inactive slot, the
previous binary stays available as the last-good fallback, and a daemon that
fails to start is rolled back automatically. A reset is one-way: it stops and
removes the node stack, deletes the machine root filesystems, clears the host
networking that the agent added, and removes the agent's units and files. After
a reset the host is no longer managed by the cluster, and the only way forward
is to bootstrap it again.

The two operations are also the boundary between repair and reinstallation.
Reach for an upgrade when the host and node are healthy but the agent binary is
old, or when a release fixes a defect in the agent itself. Reach for a reset
when the installation is inconsistent, when bootstrap refuses to continue
because the desired identity changed, or when you are retiring the host. If the
daemon is down and the problem is not a bad agent binary, neither operation can
run; use a host operation such as a power cycle, or repair the host over SSH.

| Operation | Target | Effect | Reversible |
| --- | --- | --- | --- |
| `AgentUpgrade` | Host-side agent binary | The new binary is staged in the inactive slot and the daemon restarts from it. | Yes, through the last-good slot and the recovery unit. |
| `AgentReset` | Agent installation and node stack | The agent, both nspawn machines, the network state it added, and its units and files are removed. | No. Bootstrap the host again. |

Both kinds are created as cluster-scoped `MachineOperation` resources with short
name `mop`, and both accept `spec.machineRef` or `spec.machineSelector`. The
plugin requires exactly one of the two; in a manifest you write by hand, set
exactly one as well. An operation with neither targets nothing, and the agent
uses `machineRef` when both are present. The agent is the executor in both
cases; within the cluster, it is the only component that changes its own binary
or removes itself. The general lifecycle of an operation, including creation,
status fields, and cleanup, is covered in
[Machine operations]({{< relref "guides/operations" >}}).

## AgentUpgrade

`AgentUpgrade` replaces the host-side `unbounded-agent` executable. It does not
touch the Kubernetes version, the node root filesystem, the container runtime,
the kubelet configuration, or the network dataplane. A host that runs a healthy
node before the upgrade runs the same node after it, with a different agent
binary managing it. That narrow scope is what makes the operation safe to run
during normal cluster life, and also what makes it the wrong tool for changing
anything about the node itself.

### What an Upgrade Changes

The agent is a single executable with a blue-green layout under
`/opt/unbounded/agent/bin`. An upgrade downloads a release archive, extracts
only the root `unbounded-agent` member, verifies that the result can execute,
and switches the `unbounded-agent-current` symlink to the new slot. The daemon
unit runs that symlink, so the next daemon start uses the new binary. The
previous binary is preserved in the other slot and recorded as the last-good
target, which is the rollback source if the new daemon cannot start.

Everything outside that layout is left alone: the rootfs under
`/var/lib/machines`, the node's applied configuration, the WireGuard keys, and
the node's own services. An upgrade therefore does not restart the node, does
not interrupt pods, and does not change the Node's readiness. The only visible
interruption is the daemon restart, during which the host does not reconcile new
operations.

### When to Use It

Use an upgrade when you are moving to a newer agent release, when a release
contains a security or reliability fix that is worth applying without reprovisioning
the host, or when you need to bring a fleet back under a known agent version.
Because the operation is per host and reversible, it is also the correct first
step when a new release needs validation on a small number of machines before a
wider rollout.

Do not use it to change the Kubernetes version, the node image, the kubelet
arguments, or the isolation mode. Those are node or installation changes, not
binary changes. A node upgrade is driven by deleting the Node and letting the
agent rebuild the node stack, as described in
[Node operations]({{< relref "guides/operations/node-operations" >}}). Changing
the machine name, API server, rootfs image, or recorded isolation mode requires a
reset followed by a fresh bootstrap, because bootstrap refuses a host whose
recorded installation intent no longer matches the request.

### Prerequisites

An upgrade runs on the host, so the operator workstation needs only API access.
The host needs everything the daemon needs, plus a reachable release archive.

- The `Machine` exists and its `unbounded-agent` daemon is running and connected
  to the API server. If the daemon is down, the operation stays `Pending` until
  it returns.
- The installation is complete. The upgrade path takes the installation
  mutation lock, so an agent that is mid-bootstrap defers the operation and
  retries rather than competing with the installation.
- The release archive is reachable from the host over HTTP or HTTPS, not from
  your workstation. A URL that works in your browser but is blocked by the
  host's egress rules fails the download.
- The archive is a gzip-compressed tar with a regular, non-empty file named
  `unbounded-agent` at its root. Files in subdirectories are ignored, and an
  archive whose member has a different name fails validation.
- The binary matches the host architecture. The agent verifies that the staged
  file runs, so a binary for the wrong architecture is refused before the switch.
- There is free space in `/opt/unbounded/agent/bin` for the new binary and the
  temporary archive under `/tmp`.
- No other host activation is in progress. A host-driven activation and an
  operation-driven upgrade share a lock, and a second activation waits.

### The Blue-Green Layout

The layout is fixed and does not depend on the release. The paths below are the
current installation root. Hosts installed by releases that predate the host
root still keep their files under `/usr/local` until the agent's migration
completes, and the agent resolves the layout through the host root rather than
assuming one path.

| Path | Role |
| --- | --- |
| `/opt/unbounded/agent/bin/unbounded-agent-current` | Symlink to the active slot. The daemon unit runs this path. |
| `/opt/unbounded/agent/bin/unbounded-agent-blue` | One binary slot. |
| `/opt/unbounded/agent/bin/unbounded-agent-green` | The other binary slot. |
| `/opt/unbounded/agent/bin/unbounded-agent-last-good` | Symlink to the binary that recovery restores. |
| `/opt/unbounded/agent/bin/unbounded-agent` | Compatibility symlink. On current installs it points at the current link, so running it runs the active binary. |
| `/etc/unbounded/agent/agent-upgrade-signal` | JSON record of an upgrade whose result has not been reported yet. |
| `/run/unbounded-agent-upgrade.lock` | Host activation lock shared by the daemon and the local activation command. |
| `/usr/local/bin` | Legacy root for hosts installed by older releases. The agent removes these files after migration. |

The active slot is never edited in place. The archive member is written to a
temporary file beside the target slot, given mode `0755`, flushed, and renamed
over the inactive slot only after the archive has been fully read. A failed
download, a bad digest, or a truncated archive therefore leaves both slots and
both links exactly as they were. The switch itself updates the last-good link
and then the current link, and the daemon restart is what actually changes the
running binary.

### Requesting an Upgrade

The canonical request is a `MachineOperation` manifest. The only required
parameter is `downloadURL`; `sha256` is optional but recommended whenever the
release publishes a digest. The following manifest upgrades one Machine:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: upgrade-worker-01-20261117
spec:
  machineRef: worker-01
  operationKind: AgentUpgrade
  parameters:
    downloadURL: https://releases.example.com/unbounded-agent-linux-amd64.tar.gz
    sha256: 4f3c1d0b6a2e8c9d7b5a1908e6f4c2b1d0a9e8f7c6b5a4938271605142332415
```

Apply the file and follow the object:

```bash
kubectl apply -f upgrade-worker-01.yaml
kubectl get mop upgrade-worker-01-20261117 -w
```

`spec.parameters` is a string map. The agent reads `downloadURL` and `sha256`
from it and ignores other keys, so a misspelled parameter is not an error unless
the required key itself is missing or empty. The digest may be 64 hexadecimal
characters with or without a `sha256:` prefix. An unparseable digest fails the
operation before any download is attempted. Omitting the digest disables the
explicit comparison, although gzip's own checksum still rejects a corrupted
archive.

The download URL must use `http` or `https`, include a host, omit user
information, and omit a fragment. A signed query string is allowed and is
redacted from the agent's log, but the value is stored in the operation's spec,
so anyone who can read the object can read the URL. Prefer a short-lived
signature over a long-lived credential. Redirects are followed up to ten hops,
and an HTTPS download may not redirect to HTTP.

The plugin provides a convenience command that builds the same object, generates
a name, attaches an owner reference to the Machine, and waits for the result:

```bash
kubectl unbounded machine agent-upgrade worker-01 \
  --download-url https://releases.example.com/unbounded-agent-linux-amd64.tar.gz \
  --sha256 4f3c1d0b6a2e8c9d7b5a1908e6f4c2b1d0a9e8f7c6b5a4938271605142332415
```

`agent-upgrade` accepts the following flags. The common flags have the same
meaning as on the other operation shortcuts.

| Flag | Purpose |
| --- | --- |
| `--download-url` | Required. HTTP or HTTPS URL of the release tarball. |
| `--sha256` | Optional SHA-256 digest of the tarball. Omit only when you cannot obtain one. |
| `--operation-name` | Name for the `MachineOperation`; generated when omitted. |
| `--ttl` | Seconds recorded in `spec.ttlSecondsAfterFinished`; defaults to 300, and `0` leaves the field unset. It is the general operation cleanup field, not something the agent acts on. |
| `--wait` | Wait for a terminal phase; defaults to true. |
| `--timeout` | Bound the wait; `0` waits indefinitely. |
| `--kubeconfig` | Path to a kubeconfig file. |

The explicit form is available when you need a selector, a particular name, or
a dry run:

```bash
kubectl unbounded machine operation create upgrade-worker-01 \
  --kind AgentUpgrade \
  --machine worker-01 \
  --param downloadURL=https://releases.example.com/unbounded-agent-linux-amd64.tar.gz \
  --param sha256=4f3c1d0b6a2e8c9d7b5a1908e6f4c2b1d0a9e8f7c6b5a4938271605142332415 \
  --ttl 3600 --wait
```

### Selectors and Batching

A selector is useful for building a version of an operation once and applying it
to a maintenance group. The following manifest targets Machines labeled
`pool=gpu` in the `lab` site:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: upgrade-gpu-pool-20261117
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
    downloadURL: https://releases.example.com/unbounded-agent-linux-amd64.tar.gz
    sha256: 4f3c1d0b6a2e8c9d7b5a1908e6f4c2b1d0a9e8f7c6b5a4938271605142332415
```

The same request from the command line uses `--selector`, which accepts the
standard `kubectl` label selection syntax:

```bash
kubectl unbounded machine operation create upgrade-gpu-pool \
  --kind AgentUpgrade \
  --selector 'pool=gpu,unbounded-cloud.io/site=lab' \
  --param downloadURL=https://releases.example.com/unbounded-agent-linux-amd64.tar.gz \
  --wait
```

Each agent evaluates the selector against its own labels. It reads its Node's
labels first and falls back to the Machine's labels when the Node object does
not exist yet, which lets a selector match a Machine that has not joined. A
selector that names no site is fine for the agent, because every agent only
evaluates it against itself; there is no cross-site fan-out to authorize.

The result of a selector-based agent operation is not a per-machine report.
There are no child operations and no `status.targets` entries written by the
agent. Every matching agent sees the same object, and each one that decides to
execute it writes to the same status, so the terminal phase and message are a
shared summary produced by whichever agent wrote last. Several agents can pass
the pending check before any of them records `InProgress`, so more than one host
can act, and the status still reflects only the last writer. In practice, a
selector is a best-effort broadcast, not a batch job with one result per host.
For a fleet, create one operation per Machine and wait for each one, as shown in
[Automation patterns]({{< relref "guides/operations/automation" >}}).

{{< callout type="warning" >}}
Do not use a selector for `AgentReset`. A reset is destructive, and a shared
status cannot tell you which hosts were reset. Create one operation per named
Machine, confirm the target, and keep the record until you have checked the
host.
{{< /callout >}}

The practical batching rule is the same for upgrades: upgrade one Machine at a
time, stop at the first failure, and use the per-machine record to know exactly
where the rollout stopped. If you use a selector for an upgrade, scope it with a
site label and a maintenance label, and treat it as one broadcast for the
machines that match when an agent observes the operation. Removing the label
later does not retract the work from agents that already matched it.

### Phase Transitions

An upgrade moves through the same four phases as every other operation, with one
unusual interval: it stays `InProgress` while the daemon that staged it is
stopped and a new daemon starts in its place.

| Phase | Message or reason | What it means |
| --- | --- | --- |
| Empty or `Pending` | none | The operation exists and no agent has claimed it yet. A stopped daemon leaves it here. |
| `InProgress` | `staging upgraded host agent binary` | The agent holds the locks, has recorded the start time, and is downloading, verifying, and switching slots. |
| `InProgress` | same message | The new binary is staged and the daemon is restarting. The pending signal holds the operation name and the observed Machine generation. |
| `Complete` | reason `Succeeded`, message `AgentUpgrade completed` | A daemon running the new binary started, read the pending signal, and reported success. |
| `Failed` | reason `InvalidParameters` | `downloadURL` was missing or empty. Nothing was downloaded or changed. |
| `Failed` | reason `ExecutionFailed` | The download, digest check, archive extraction, binary validation, or isolation-mode check failed before the switch. |
| `Failed` | reason `DaemonFailed` | The new binary was activated but the daemon did not become healthy. Recovery rolled the current link back to last-good. |

The `Completed` condition mirrors the phase: it is `False` with reason
`InProgress` while the operation runs, `True` with reason `Succeeded` on
success, and `False` with the failure reason on failure.
`status.observedMachineGeneration` records the Machine generation that the
operation acted on, so you can tell whether the Machine changed while the
upgrade was in flight.

### Daemon Restart and Activation

The upgrade is staged by the daemon, but the daemon cannot call `systemctl
restart` on itself without risking its own cgroup being torn down mid-command.
Instead it stops its own controller, and the command layer either re-executes
the binary the unit now points at in the same process or exits cleanly and lets
the unit's `Restart=always` start it again. The sequence is:

1. Acquire the installation mutation lock and the host activation lock. If
   either is held, wait and retry instead of starting a second activation.
2. Mark the operation `InProgress` and record the start time.
3. Resolve the active slot through the current symlink and select the other
   slot as the target.
4. If the target slot currently holds the last-good binary, repoint last-good at
   the running binary first, so a failed overwrite cannot destroy the only
   rollback copy.
5. Download the archive to a temporary file, compare the digest when one was
   supplied, and extract only the root `unbounded-agent` member next to the
   target slot.
6. Run the staged binary's `version` command. Then ask it whether it can operate
   every non-nspawn isolation mode present on the host. A candidate that cannot
   is refused before any link changes.
7. Point last-good at the binary that was running, point current at the new
   slot, and write the pending signal with the operation name and the observed
   Machine generation.
8. Stop the controller so the process restarts from the new current link. The
   operation is deliberately left `InProgress`; the old process must not write a
   second result or stage the upgrade again.
9. The new daemon starts, publishes the pending signal as `Complete`, removes
   the signal file, and continues watching for operations. If it cannot start,
   the recovery unit rolls back and records a failure signal instead.

The recovery unit is a systemd `OnFailure` handler for the daemon, with a
seven-minute start timeout. It checks that a valid last-good binary exists,
refuses one that cannot operate an isolation mode present on the host, writes a
failure signal when a pending upgrade signal exists, points current at last-good,
clears the daemon's failure counter, and restarts it. If the restart command
fails, it waits out one start-limit window and tries once more. It then checks
that the running process really is the last-good executable. The next healthy
daemon publishes that failure signal as a `Failed` operation with reason
`DaemonFailed`.

### Drain and Workload Impact

An upgrade does not cordon, drain, or evict anything, because the node stack is
not restarted and the kubelet is not touched. Pods keep running across the
daemon restart. The host is briefly unable to accept new `MachineOperation`
work and unable to report status, but the Node itself stays as it was. For a
production node this is the least disruptive agent operation available, and it
does not require a maintenance window beyond the time needed to watch the
result.

The restart still has consequences worth planning for. A `NodeReboot` or a
`HostSoftReboot` created during the restart stays `Pending` and is picked up by
the new daemon. An operation that was `InProgress` in the old daemon is not
resumed, except for the upgrade that triggered the restart, which is reported
from the signal. If the daemon does not come back, node operations stop being
reconciled even though the node keeps serving traffic, and the recovery path
becomes the priority.

### Verifying the Upgrade

Start with the operation record. A successful upgrade ends in `Complete` with
the message `AgentUpgrade completed`, and `kubectl describe mop` shows a `True`
`Completed` condition with reason `Succeeded` and the observed Machine
generation:

```bash
kubectl get mop upgrade-worker-01-20261117
kubectl describe mop upgrade-worker-01-20261117
```

The `Machine` object is not the record for this operation. Its status tracks
bootstrap and node lifecycle, and an upgrade does not change it. Confirm the
binary on the host when you have access, and confirm the daemon is healthy:

```bash
readlink -f /opt/unbounded/agent/bin/unbounded-agent-current
/opt/unbounded/agent/bin/unbounded-agent version
systemctl status unbounded-agent-daemon --no-pager
journalctl -u unbounded-agent-daemon -n 50 --no-pager
```

The resolved current path should name either the blue or the green slot. The
version output should name the release you staged, and the daemon should be
active. The compatibility path, `/opt/unbounded/agent/bin/unbounded-agent`,
should run the same binary because it links to the current link.

On a host installed by an older release, the files may still live under
`/usr/local` immediately after the upgrade. The daemon migrates the host root on
start and restarts once more when the copy is verified, so an upgrade at the
same time as the migration can take two daemon starts. Check
`readlink -f /opt/unbounded/agent` to see whether the host root is a real
directory or still a link to the legacy layout. If the link remains and the
daemon is not moving it, the filesystem may be mounted `noexec`, and the host
stays on the legacy layout by design.

### Rollback and Recovery

Rollback is automatic after a bad activation, but it is not part of the
operation's critical section. The switch happens first, then the restart. If the
restart or the health check fails, the recovery unit restores the last-good
link and reports the failure through the signal file. You do not need to create
a rollback operation. The failed `AgentUpgrade` is the record, the reason is
`DaemonFailed`, and the message from the recovery script ends with the path it
rolled back to.

The agent does not compare version numbers. The archive you supply defines what
runs next, so a downgrade uses the same operation and the same validation. That
is intentional: a rollback release is just a release, and refusing it because
its version string is older would remove a recovery option. Keep the digest of
a known-good archive so you can redeploy it deliberately if a newer release
passes staging but behaves badly in production.

If the operation is stuck in `InProgress` and no daemon has reported, SSH to the
host and inspect the recovery path. Running the recovery unit again is the
supported way to retry the rollback after fixing the underlying problem:

```bash
sudo systemctl status unbounded-agent-daemon --no-pager
sudo systemctl status unbounded-agent-daemon-recovery --no-pager
sudo journalctl -u unbounded-agent-daemon -u unbounded-agent-daemon-recovery -n 100 --no-pager
sudo systemctl start unbounded-agent-daemon-recovery.service
```

Recovery refuses a last-good binary that cannot operate an isolation mode that
the host is using, and it refuses to proceed when no executable last-good binary
exists. In the first case, install a release that supports the mode. In the
second, the host needs a fresh agent binary delivered over SSH, either by
running the hidden `unbounded-agent agent-upgrade` command from the candidate
binary or by bootstrapping the host again.

### Failure Modes

The failure reasons are small enough to plan around. `InvalidParameters` means
the manifest is wrong, and nothing on the host changed. `ExecutionFailed` means
the host could not accept the candidate and the running daemon was not
restarted; read the message, because it distinguishes a URL scheme problem, a
digest mismatch, an archive without a root `unbounded-agent`, a binary that
cannot execute, and a candidate that cannot operate an isolation mode.
`DaemonFailed` means the candidate passed every check and still could not run as
a daemon; recovery has already put the previous binary back, so fix the release
and create a new operation.

Two less obvious failures are worth recognizing. An operation that stays
`Pending` forever usually means the daemon is not running, cannot reach the API
server, or the installation is not complete; check the host, not the operation.
An operation that stays `InProgress` with the staging message and no restart
usually means the daemon stopped between marking the operation and reporting the
result, or that the new daemon is crash-looping and recovery has not taken over
yet. Inspect the daemon and the signal file at
`/etc/unbounded/agent/agent-upgrade-signal`; a manual restart publishes a pending
signal when one was recorded, and the recovery unit handles a binary that will
not run. In both cases, do not delete an operation that has not reached a
terminal phase; a deleted object cannot be finished, and the agent treats a
missing object as nothing to report. Deleting the object does not cancel work
that has already started; it only removes the record of it.

### Safety Rules for Upgrades

Supply a digest from the release you intend to run. Without one, the only
integrity check is the archive's gzip checksum, which catches corruption but not
substitution. Keep the URL scoped to the host's network, and prefer a
short-lived signed URL over an embedded credential.

Upgrade a canary first, verify the version and the daemon health on the host,
and only then widen the rollout. Do not run upgrades and node rebuilds on the
same Machine at the same time: the installation lock serializes them, so one
waits, but the waiting operation can outlive the maintenance window you planned.
Do not delete an in-flight operation, and remember that the agent does not delete
its terminal operation records; delete the record yourself when you no longer
need the audit trail, or delete the Machine and let the owner reference
garbage-collect it.

## AgentReset

`AgentReset` removes the agent and the resources it manages from the host. It is
the inverse of bootstrap: the node stack is stopped and destroyed, the nspawn
machines and their root filesystems are deleted, the networking the agent added
is removed, and the agent's units, binaries, and configuration are cleared. When
the operation finishes, the host is a plain machine again, and the cluster has
no way to reach it until an agent is installed and bootstrapped.

### What Reset Removes

The teardown covers both node slots, `kube1` and `kube2`, regardless of which
one is active, and every artifact the agent installed under either host root. It
does not depend on the applied configuration being readable: if the configuration
is damaged, the reset still runs, because reset is the escape hatch for exactly
that situation.

| Area | Removed |
| --- | --- |
| Node stack | Both nspawn machines are stopped, force-terminated after a grace period if needed, unregistered from `machinectl`, and their directories under `/var/lib/machines` are deleted. |
| Node configuration | The per-machine `.nspawn` files, `systemd-nspawn@<name>.service.d` override directories, and configuration regeneration units are removed. |
| Agent units | The daemon unit, the recovery unit, and the first-boot bootstrap unit are disabled and removed, and the recovery script is removed. |
| Networking | WireGuard interfaces named `wg51820` through `wg51899`, the tunnel and overlay interfaces `geneve0`, `vxlan0`, `ipip0`, `unbounded0`, and `cbr0`, the LocalDNS dummy interface and nftables table, policy routing rules and tables, and the nftables flush unit and its rules file. |
| Keys | `/etc/wireguard/server.priv` and `/etc/wireguard/server.pub`. |
| Agent files | Binaries, links, helper programs, and other agent files under `/opt/unbounded/agent` and the legacy `/usr/local` layout, the installer scripts under `/usr/local`, plus `/etc/unbounded/agent` and leftover temporary configuration files. |
| Installation record | `/var/lib/unbounded/agent/install-state.json` is marked `resetting` before teardown and removed after the filesystem buffers are flushed. |

Other isolation modes are not ignored. The nspawn teardown always runs, because
each of its steps is idempotent, and the teardown for a non-nspawn mode runs in
addition when the host has traces of that mode, such as its units or its layout
directory, even if the applied configuration is missing or damaged. In the none
mode this stops the node's units and removes the containerd and kubelet state and
the CNI configuration the agent wrote; in the container mode it stops and
destroys both slot containers and the shared node data the backend owns. Each
mode's tasks occupy the same positions in the sequence as the nspawn tasks they
replace.

Reset deliberately leaves some things in place. It does not delete the
`Machine` or the Kubernetes `Node`, does not detach or delete provider resources
or the host's OS disk, does not reinstall the operating system, and does not
remove the host-side artifact and container image caches under
`/var/lib/unbounded`. Those caches are not agent state; remove them yourself if
the host is being decommissioned and the disk will be reused outside the
cluster.

### When to Reset

Reset is the right operation when the installation, rather than the workload, is
the problem. Use it for permanent decommissioning, for handing a host back to
its owner, for recovering a host whose agent layout is inconsistent, and for
testing the bootstrap and preflight path from a clean state. It is also the
required step when bootstrap refuses to continue because the machine name, the
Kubernetes version, the rootfs image, the API server, or the recorded isolation
mode changed. Bootstrap reports that an explicit reset is required, because
building a second node stack beside the first is not a valid repair.

Do not use reset as a general-purpose reboot. If the node stack is stuck but the
agent is healthy, a `NodeReboot` restarts it in place and keeps the disk and the
agent. If the host is unreachable, a host power operation is the fallback. Reset
is for the case where you intend to lose the node's local state.

### Prerequisites

A remote reset needs only a running daemon and API access, because the daemon
performs the teardown itself. A local reset needs root on the host. In both
cases, the important prerequisite is a decision about the workloads:

- Cordon and drain the node first if you care about the pods. Neither the agent
  nor the operation drains or evicts anything.
- Accept that local data in the node's root filesystem is lost, including
  container images cached inside the nspawn machine, kubelet state, pod logs,
  and data written to paths inside the machine.
- Confirm that the `Machine` name is the host you mean to erase. A reset has no
  allocation launch guard, so it is not refused because the Machine is fenced or
  backed by an allocation. If you use the convenience command, the typed
  confirmation is the only confirmation step.
- Have SSH access ready if you intend to bring the host back. The reset removes
  the agent, so no cluster-side operation can run on that host afterward.
- Keep the operation record until you have verified the host, or you will lose
  the only cluster-side evidence of what was removed.

### Remote Reset with MachineOperation

The remote form creates a `MachineOperation` with kind `AgentReset`. It needs no
parameters. The following manifest resets one Machine:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: reset-worker-04-20261117
spec:
  machineRef: worker-04
  operationKind: AgentReset
```

Apply it and watch the phase:

```bash
kubectl apply -f reset-worker-04.yaml
kubectl get mop reset-worker-04-20261117 -w
```

The convenience command adds a typed confirmation prompt and defaults to
waiting for the result:

```bash
kubectl unbounded machine agent-reset worker-04
```

The prompt asks you to type the Machine name, and a non-interactive session is
refused unless you pass `--force`. The explicit create command does not prompt,
so use it only from automation that has its own confirmation step. A reset
issued this way still needs a healthy daemon to receive it; the advantage is
that the teardown needs no inbound SSH connection to the host.

### Selectors and Why Not

The API accepts `spec.machineSelector` for `AgentReset`, and the agent will
evaluate it like any other selector. The result is still a single shared status
written by the agents that execute it, with the last write winning and no
per-machine record. For a destructive operation that is not a useful trade. Use
`spec.machineRef` for reset, one object per host, and keep the record. If you
must reset a group, generate the manifests in a loop and apply them one at a
time, as described in
[Automation patterns]({{< relref "guides/operations/automation" >}}).

### Phase Transitions

Reset uses the same phases, but its terminal phase is recorded before the daemon
stops itself, so the record survives the process that created it.

| Phase | Message or reason | What it means |
| --- | --- | --- |
| Empty or `Pending` | none | The operation exists and no agent has claimed it. |
| `InProgress` | `resetting unbounded agent` | The agent holds the installation lock, has marked the installation record `resetting`, and stopped the recovery unit. |
| `InProgress` | same message | Teardown is running: machines, keys, configuration, networking, units, and files. |
| `Complete` | reason `Succeeded`, message `AgentReset completed` | Teardown finished and the result was written. The daemon stops immediately after this. |
| `Failed` | reason `ExecutionFailed` | A teardown step failed. The host is partially reset and the installation record still says `resetting`. |

`Complete` means the agent finished removing its resources and recorded the
result. It does not mean the Node object is gone, the workloads were drained, or
the host is ready to be reused. Those are your decisions and your next steps.

### Teardown Sequence

The order is fixed so that a failure leaves the host in a recognizable state and
so that systemd does not restart a half-removed agent. The agent performs the
steps in this order:

1. Take the installation lock and set the installation record to `resetting`.
2. Stop the recovery unit, so a daemon failure during teardown cannot trigger a
   rollback that resurrects a partially removed installation.
3. Disable and remove the daemon unit, the recovery unit, and the recovery
   script.
4. Stop both nspawn machines in parallel, waiting for a clean stop and
   force-terminating after the grace period.
5. Remove the WireGuard private and public keys.
6. Remove the nspawn configuration, override directories, and configuration
   regeneration units for both slots.
7. Remove both machine root filesystems and their `machinectl` registration,
   then unmount their private bpffs mounts under `/run/bpffs`.
8. Remove the LocalDNS unit, dummy interface, and nftables table; remove
   WireGuard and overlay interfaces; remove policy routing rules and routes.
9. Remove the nftables flush unit and its rules file, remove the first-boot
   bootstrap unit, remove the agent binaries and configuration under both roots,
   and reload systemd.
10. Flush the filesystems that held the agent's files, then remove the
    installation record.
11. In the `MachineOperation` path, write the terminal phase to the operation,
    then stop the daemon through systemd. The result is recorded first so the
    record survives the process that wrote it, and the daemon is the last thing
    to go. The local command stops the daemon before the teardown instead.

The durability step matters. Without it, a host could lose power after the
record was unlinked but before that change reached disk, and come back with a
stale installation record pointing at files that are already gone. The reset
forces the relevant filesystems to disk before it deletes the record, so the
removed files and the removed record become durable together.

### Destructive Effects and Drain

{{< callout type="warning" >}}
`AgentReset` destroys the node stack and the machine root filesystems. It does
not drain pods, evict workloads, detach volumes, or delete the Node object. If
the host holds data you need, copy it or move the workload before you reset.
{{< /callout >}}

The node goes away without a graceful Kubernetes shutdown. Pods that were
running on it are terminated when the machine is stopped, and the control plane
eventually marks the Node `NotReady` or removes it. Stateful workloads that rely
on a local volume inside the nspawn rootfs lose that data. DaemonSets and
static pods are not exempt, because the kubelet itself is being removed. This is
the same blast radius as deleting a node's disk, and it should be treated that
way in a change plan.

Drain the node before the reset when the workloads matter. The reset does not do
this for you:

```bash
kubectl drain worker-04 --ignore-daemonsets --delete-emptydir-data
```

The command evicts pods and marks the Node unschedulable. It does not protect
data on a local volume, and it cannot evict a pod that has no controller, so
check the output and handle those pods explicitly before you continue.

The host itself survives. The operating system, the kernel, the provider VM, and
the physical hardware are untouched. That makes reset cheaper than `HostReplace`
when the OS is healthy, and it makes reset the wrong choice when the OS or the
boot path is the thing that failed.

### Local Reset with the CLI

When the daemon is unreachable, or when you are already on the host, run the
reset directly. The command needs root because it removes systemd units and
machine images:

```bash
sudo /opt/unbounded/agent/bin/unbounded-agent reset
```

The compatibility path points at the current binary on current installations, so
this runs the active agent version. On a host that has not migrated from the
legacy layout, the binary is under `/usr/local/bin`, and
`sudo /usr/local/bin/unbounded-agent reset` performs the same teardown. You can
also use the current link,
`/opt/unbounded/agent/bin/unbounded-agent-current reset`, if the plain path has
been removed.

The command stops the daemon first, then runs the same resource teardown as the
remote operation, including both possible machine names. It is idempotent, so
running it on a host that is already partly or fully reset is safe and is the
correct response to an interrupted reset. It does not need the API server.

### Verifying a Reset

The operation record should be `Complete`. On the host, check that the agent's
units, machines, and network state are gone:

```bash
systemctl status unbounded-agent-daemon --no-pager
machinectl list
ls /var/lib/machines
ls /opt/unbounded/agent
ls /etc/unbounded/agent
ls /etc/wireguard
ip -brief link
ip rule
```

The daemon unit should be reported as not found, `machinectl list` should not
list `kube1` or `kube2`, and `/opt/unbounded/agent` should be empty or absent.
`/etc/unbounded/agent` should be absent. `/opt/unbounded` itself is intentionally
kept, because it is shared with staged artifacts. The WireGuard key files should
be gone, and no agent-managed WireGuard interface (`wg51820` through `wg51899`),
`geneve0`, `vxlan0`, `ipip0`, `unbounded0`, or `cbr0` interface should remain.
The installation record at `/var/lib/unbounded/agent/install-state.json` should
also be absent.

The Node object is not removed by the agent. Delete it from the cluster so the
control plane stops treating the host as a member, using the node name from the
Machine's `spec.kubernetes.nodeRef.name` when it overrides the Machine name:

```bash
kubectl delete node worker-04
```

### Failure Recovery and Idempotence

A reset that fails partway leaves the installation record in `resetting`. That
is deliberate: bootstrap reads the record, refuses to start, and reports that
the reset is incomplete and must be run again. Do not delete the record to get
past the message. Run the reset again, locally if the daemon is already gone.
Every teardown step tolerates absent files and already-stopped services, so a
second run finishes what the first one started.

A failed `AgentReset` operation is terminal. The agent does not retry it, and
because the daemon may already be stopped, do not count on delivering a second
remote operation. The correct recovery is SSH and the local command. Once the
local reset succeeds, the installation record is removed, and the host can be
bootstrapped again. If the machine removal step keeps failing because a machine
is still registered, confirm that the nspawn service is stopped and run
`machinectl remove <name>` before repeating the reset.

### After Reset

The host is unmanaged. To return it to the cluster, reboot it to get a clean
baseline and clear any interface or unit state that the teardown could not
unload, run preflight, and bootstrap it again. A host reset remotely can be
power cycled with a host operation if it is still reachable through its provider
or BMC; otherwise use the host's own console. The
[host operations]({{< relref "guides/operations/host-operations" >}}) page
covers power cycles and replacement, and
[preflight]({{< relref "reference/agent/preflight" >}}) covers the checks that
should pass before bootstrap.

If the host is being retired, delete the `Machine` object as well after the Node
is gone. The reset does not remove it, and a leftover Machine with no agent
continues to appear in inventory and in selectors, where it will never complete
an agent operation.

### Safety Rules for Reset

Treat reset as irreversible. Verify the Machine name, drain the workloads, and
confirm that every piece of local data on the node is expendable before you
start. Prefer the confirmed CLI command over a hand-written manifest for
interactive work, and require an explicit confirmation step in automation.
Never use a selector, and never include a reset in a loop that also upgrades or
reboots machines without a stop-on-failure rule. Deleting the operation does not
stop a teardown that has already started, so treat the object as a record, not
as abort control.

Keep the operation record until the host has been verified or bootstrapped
again. The agent does not delete its terminal operation records. A TTL may or
may not be acted on by another controller, so remove the record yourself when
you are done, or leave it in place if the operation is part of an incident.

## Choosing Between Them

The decision follows the layer that is actually broken. An upgrade changes only
the agent binary, so it is the right choice when the host, the node, and the
installation are all sound and the agent release is the thing that needs to
move. A reset changes the installation itself, so it is the right choice when
the agent layout is inconsistent, when the recorded installation intent must
change, or when the host is leaving the cluster. A node operation is the right
choice when the agent is healthy but the node stack is not, and a host operation
is the right choice when the daemon cannot run at all.

| Situation | Operation |
| --- | --- |
| A new agent release should run on a healthy host. | `AgentUpgrade` |
| A release must be rolled back to a known-good binary. | `AgentUpgrade` with the older archive and its digest. |
| The agent layout is inconsistent or partially removed. | `AgentReset`, then bootstrap. |
| The machine name, node version, rootfs image, API server, or isolation mode changed. | `AgentReset`, then bootstrap with the new intent. |
| The host is being decommissioned or handed back. | `AgentReset` after draining, then delete the Node and Machine. |
| The node stack is stuck but the agent is healthy. | `NodeReboot`, covered in [Node operations]({{< relref "guides/operations/node-operations" >}}). |
| The host is unreachable or the OS cannot boot. | A host power or replace operation, covered in [Host operations]({{< relref "guides/operations/host-operations" >}}). |

A rollout usually combines them in a fixed order. Upgrade one canary, check the
version and the daemon journal on the host, and widen the rollout only after the
canary has served traffic for a while. If the fleet is being rebuilt rather than
upgraded, drain a host, reset it, delete its Node, reboot it, run preflight, and
bootstrap it again before moving to the next one. Do not mix upgrades and resets
in the same unattended loop, because a reset removes the agent that the next
upgrade in the loop needs.

## Related

The following pages cover the surrounding machinery and the checks that make
these operations safe:

1. [Machine operations]({{< relref "guides/operations" >}}) explains the shared
   `MachineOperation` model, status fields, and the boundary between host, node,
   and agent work.
2. [Automation patterns]({{< relref "guides/operations/automation" >}}) shows
   selector loops, sequential rollouts, and failure handling for fleets.
3. [Node operations]({{< relref "guides/operations/node-operations" >}}) covers
   `NodeReboot` and node recreation when the node stack, not the agent, is the
   problem.
4. [Host operations]({{< relref "guides/operations/host-operations" >}}) covers
   power control and replacement when the daemon cannot act.
5. [Installing the agent]({{< relref "guides/agent" >}}) covers the host layout,
   bootstrap, and the recovery behavior that follows a reset.
6. [Preflight]({{< relref "reference/agent/preflight" >}}) lists the checks to run
   before bootstrapping a reset host.
7. [Provisioning format]({{< relref "reference/agent/provisioning-format" >}})
   describes the bootstrap input that a reset removes and a new bootstrap
   recreates.
8. [MachineOperation reference]({{< relref "reference/machina-crd" >}}) documents
   every field and status value used on this page.
