# AgentUpgrade

`AgentUpgrade` replaces the host-side `unbounded-agent` daemon binary without
repaving the worker. The flow is driven by a `MachineOperation` and uses a
blue-green binary layout, a single JSON signal file, and systemd recovery to
publish success or rollback failure back to the operation.

## Goals

- Stage a downloaded agent binary before changing the daemon entrypoint.
- Keep the previous working daemon binary as last known good.
- Complete the `MachineOperation` only after the restarted daemon is healthy
  enough to publish startup state.
- Roll back automatically when systemd cannot keep the upgraded daemon running.
- Support host-driven binary activation when a Kubernetes `MachineOperation` is
  unavailable or intentionally not used.
- Keep the host activation logic reusable by other consumers, including AKS
  Flex, without depending on Kubernetes APIs or Unbounded command packages.
- Keep signal file structure in Go code instead of shell scripts.

## Host paths

The path set is represented by `goalstates.AgentUpgradePaths`.

| Field | Purpose |
|-------|---------|
| `BinaryPath` | Compatibility path, normally `/usr/local/bin/unbounded-agent`. |
| `BluePath` | First blue-green binary slot. |
| `GreenPath` | Second blue-green binary slot. |
| `CurrentPath` | Symlink used by the systemd daemon unit. |
| `LastGoodPath` | Symlink used by recovery to roll back. |
| `SignalPath` | Single JSON signal file for pending and failure state. |
| `CurrentTargetPath` | Resolved current binary target for one operation. |

`goalstates.ResolvedAgentUpgradePaths()` resolves environment overrides and
stores the resolved `CurrentPath` target in `CurrentTargetPath`. If
`CurrentPath` does not exist, the compatibility `BinaryPath` is used as the
current target. `NextTargetPath()` then chooses the inactive slot:

```text
current target == BluePath  -> next target = GreenPath
otherwise                  -> next target = BluePath
```

## Bootstrap state

Daemon enablement initializes the binary links before installing and starting
the systemd unit:

1. Resolve `CurrentPath`.
2. If it does not exist, seed it from the first executable of `BluePath`,
   `GreenPath`, then `BinaryPath`. When only `BinaryPath` exists, copy it into
   `BluePath` first and seed the links from the blue slot.
3. If `LastGoodPath` does not exist, point it to the resolved current target.
4. Point `BinaryPath` to `CurrentPath` unless the current target already is
   `BinaryPath`.

This preserves legacy installs while making the daemon run through
`CurrentPath` for future upgrades.

## Host-driven agent-upgrade command

A newer agent binary also exposes this hidden command:

```text
unbounded-agent agent-upgrade [--preflight]
```

The command is a host-driven alternative to the Kubernetes-driven
`AgentUpgrade` `MachineOperation`. It is useful for upgrading an existing
deployment that might not support the `AgentUpgrade` `MachineOperation` yet,
without reimaging the host or repaving the nspawn worker. It is intended for
host provisioning and management systems that have already
delivered a candidate binary, including AKS Flex node-side integration. The
candidate is invoked directly from its staging path:

```bash
/var/tmp/unbounded-agent-candidate agent-upgrade --preflight
/var/tmp/unbounded-agent-candidate agent-upgrade
```

The command does not download a release archive. The caller is responsible for
obtaining and authenticating the candidate before execution. The command uses
the executable that contains the command as the candidate, rather than
accepting another candidate path from a flag.

The host-driven command does not:

- Create or update a `MachineOperation`.
- Read `downloadURL` or `sha256` operation parameters.
- Write the AgentUpgrade operation signal file.
- Publish success or failure through Kubernetes.

It reports through command output, its exit status, and daemon service logs.
It refuses to proceed while the shared AgentUpgrade operation signal exists,
which covers the interval after a MachineOperation-driven daemon releases its
process-owned activation lock and before startup publishes success or rollback
failure.

The command remains named `agent-upgrade` to match the MachineOperation naming
convention, even though the activation primitive does not enforce semantic
version ordering and may also be used for reinstall, repair, or downgrade.

### Host activation flow

Without `--preflight`, the command performs one transactional activation:

1. Require sufficient host privileges and acquire an exclusive activation
   lock.
2. Resolve the executing candidate path and inspect the current binary layout.
3. Validate path safety and reject path collisions or unsafe aliases.
4. Verify the candidate by running its `version` command.
5. If the managed layout is not initialized, preserve the existing
   single-path daemon binary in one slot and establish `CurrentPath` and
   `LastGoodPath`.
6. Install the candidate atomically into the inactive slot. If the managed
   layout already exists, use the normal blue-green slot selection.
7. Preserve the active binary through `LastGoodPath` and atomically switch
   `CurrentPath` to the candidate.
8. Ensure the daemon service runs through `CurrentPath`, then reload the
   service manager.
9. Restart the daemon and wait for the caller-defined health check.
10. If restart or health verification fails, restore `CurrentPath` to
    `LastGoodPath`, restart the previous daemon, and return a failure.
11. Return success only after the candidate daemon passes its health check.

The command must be idempotent. Re-executing the active binary may repair
missing links or service configuration, but it must not unnecessarily replace
an identical active binary. Candidate and active binary content can be compared
by SHA-256 when determining whether a binary change is required.

The command must not be run from a path that has already destructively replaced
the only copy of the previous daemon binary. Host delivery must stage the
candidate separately so the previous executable remains available for
last-good initialization and rollback.

### Preflight

`--preflight` computes and displays the host activation plan without applying
it. A successful preflight reports at least:

- The candidate and currently active binary paths.
- Whether the managed layout needs initialization or repair.
- The inactive slot selected as the installation target.
- The planned current and last-good link changes.
- Whether daemon service configuration needs an update.
- The planned reload, restart, health check, and rollback actions.

Preflight may inspect files, links, binary digests, service configuration, and
service status. It must not:

- Create, replace, or remove binaries, links, directories, or lock files.
- Write service units or drop-ins.
- Reload, start, stop, or restart a service.
- Create an AgentUpgrade signal.

Preflight exits nonzero when it finds a condition that would block activation.
Because host state can change after preflight, the applying command repeats all
validation while holding the activation lock.

### Reusable activation library

The safety-sensitive host activation behavior lives in an exported package,
not under `cmd/agent/internal`. The hidden command is a thin adapter over that
library. Other node-side consumers can use the same implementation with their
own paths, service unit, and health definition.

The reusable boundary has two conceptual operations:

```go
PreflightHostDaemonActivation(ctx, options, serviceInspector) (ActivationPlan, error)
ActivateHostDaemon(ctx, log, options, service) (ActivationResult, error)
```

`ActivationOptions` supplies an explicit candidate path, binary layout, mode,
and lock path. The CLI adapter obtains the candidate with `os.Executable()`.
The service integration supplies operations to prepare the daemon entrypoint,
reload the service manager, restart the daemon, and wait for health.

The reusable library must not:

- Import `cmd/agent` packages.
- Depend on Kubernetes clients or `MachineOperation` types.
- Hard-code the Unbounded systemd unit or operation signal schema.
- Download or authenticate a candidate implicitly.

The Unbounded CLI supplies an adapter for
`unbounded-agent-daemon.service`. AKS Flex can supply its own service adapter
and binary paths while sharing installation, link switching, idempotency,
locking, and rollback behavior.

## MachineOperation state machine

```text
Pending MachineOperation
        |
        v
Acquire shared host activation lock
        |
        +-- lock busy -----------------------------------> Requeue as Pending
        |
        v
Validate parameters
        |
        +-- missing/invalid HTTP(S) URL -----------------> Failed
        |
        v
Mark InProgress
        |
        v
Resolve current target and inactive slot
        |
        v
Download, install, and preflight inactive slot
        |
        +-- download/install/preflight failure ----------> Failed
        |
        v
Switch last-good and current symlinks
        |
        v
Write pending JSON signal
        |
        v
Restart unbounded-agent-daemon.service
        |
        +-- restart command failure ---------------------> Failed, clear signal
        |
        v
Old process exits, new daemon starts
        |
        +-- startup succeeds ----------------------------> Complete, clear signal
        |
        +-- systemd recovery fires ----------------------> Failed, clear signal
```

## Staging and switching

The daemon first acquires the same host activation lock used by the direct
`unbounded-agent agent-upgrade` command. It holds the lock through archive
staging, link switching, pending signal creation, and the systemd restart
request. Lock contention leaves the operation Pending and requeues it instead
of failing it. Process termination during restart releases the lock, while the
pending signal prevents a direct host activation from entering the remaining
startup window.

The daemon reads `spec.parameters["downloadURL"]` and the optional
`spec.parameters["sha256"]` from the `MachineOperation`, resolves the current
binary target, and calls `agentbinary.InstallAndSwitchFromTarGz`.
Logs and errors omit URL query and fragment data.

`InstallAndSwitchFromTarGz` performs the upgrade as one logical operation:

1. Require an HTTP or HTTPS URL and verify the compressed-archive SHA-256 when provided.
2. Download the tarball within the configured size bound.
3. Require the archive to contain only the exact `unbounded-agent` entry.
4. Bound decompression and atomically install the inactive slot.
5. Run `unbounded-agent version` against the staged binary without exposing output.
6. If the inactive slot is last-good, protect the running binary through `LastGoodPath` before replacing it; otherwise defer the last-good update until candidate verification succeeds.
7. Atomically update `CurrentPath` to the staged binary.

Symlink replacement uses `renameio.Symlink` through `utilio`, so each link is
replaced atomically after parent directory creation.

## Signal file

The daemon uses one JSON file at `AgentUpgradePaths.SignalPath`. The signal is
managed through `agentUpgradeSignalOperator` and has the following shape:

```json
{
  "operationName": "upgrade-agent-on-worker-1",
  "observedMachineGeneration": 42,
  "failureMessage": "AgentUpgrade daemon failed after switching binary"
}
```

Pending success state sets `operationName` and
`observedMachineGeneration`. Failure state sets `operationName` and
`failureMessage`. The shared file is intentionally JSON-only. Invalid JSON is
treated as an error, not as a legacy fallback format.

## Successful startup

The daemon does not complete the `MachineOperation` before restart. After the
new daemon starts, startup calls `publishAndClearAgentUpgradeSignals`:

1. Read the shared signal file.
2. If it contains a failure `message`, mark the operation `Failed` with reason
   `DaemonFailed`, then clear the signal.
3. Otherwise, if it contains pending operation state, mark the operation
   `Complete` with reason `Succeeded`, then clear the signal.
4. If no signal exists, do nothing.

This makes a successful operation dependent on the restarted daemon becoming
healthy enough to reconcile operation status.

## Recovery path

The daemon unit runs through `CurrentPath`. A separate systemd recovery unit
runs when the upgraded daemon repeatedly fails. The recovery script:

1. Resolves `LastGoodPath`.
2. If the upgrade signal file exists, calls the last-good binary with the
   hidden `record-agent-upgrade-failure-signal` command.
3. Updates `CurrentPath` back to `LastGoodPath`.
4. Restarts the daemon unit.

The hidden command records the failure by reading the pending signal and
rewriting the same JSON file with the operation name and rollback message. The
shell script passes only the message; it does not know the JSON schema or the
signal path. The recovered daemon then publishes the failure through the normal
startup signal path.

## Failure cases

| Failure | Operation status | Binary state |
|---------|------------------|--------------|
| Missing `downloadURL` | `Failed`, `InvalidParameters` | No link changes. |
| Unsupported URL or digest mismatch | `Failed`, `InvalidParameters` or `ExecutionFailed` | No current link change. |
| Download or extraction failure | `Failed`, `ExecutionFailed` | Current remains unchanged. Last-good changes to current only when needed to protect an inactive slot that it referenced. |
| Empty archive entry | `Failed`, `ExecutionFailed` | Current remains unchanged. Last-good changes to current only when needed to protect an inactive slot that it referenced. |
| Staged binary fails `version` | `Failed`, `ExecutionFailed` | Current remains unchanged. A distinct last-good target remains unchanged. |
| Restart command fails | `Failed` | Signal is cleared. Links may already point to the staged binary. |
| Upgraded daemon fails under systemd | `Failed`, `DaemonFailed` | Recovery restores current to last-good. |

## Sequential upgrades

Each successful upgrade alternates blue and green slots:

```text
Initial: current -> legacy or blue
Upgrade 1: current -> blue,  last-good -> previous current
Upgrade 2: current -> green, last-good -> blue
Upgrade 3: current -> blue,  last-good -> green
```

Failed staging does not advance the current slot. The next successful upgrade
uses the inactive slot computed from the still-current target.
