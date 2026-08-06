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

## Operation state machine

```text
Pending MachineOperation
        |
        v
Validate parameters
        |
        +-- missing URL/digest or non-HTTPS URL ---------> Failed
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

The daemon reads `spec.parameters["downloadURL"]` and
`spec.parameters["sha256"]` from the `MachineOperation`, resolves the current
binary target, and calls `agentbinary.SecureInstallAndSwitch`. Logs and errors
omit URL query and fragment data.

`SecureInstallAndSwitch` performs the upgrade as one logical operation:

1. Require an HTTPS URL and an exact compressed-archive SHA-256.
2. Download the tarball within the configured size bound.
3. Require the archive to contain only the exact `unbounded-agent` entry.
4. Bound decompression and atomically install the inactive slot.
5. Run `unbounded-agent version` against the staged binary without exposing output.
6. Protect the running binary through `LastGoodPath` before replacing an inactive slot.
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
| Missing `downloadURL` or `sha256` | `Failed`, `InvalidParameters` | No link changes. |
| Non-HTTPS URL or digest mismatch | `Failed`, `InvalidParameters` or `ExecutionFailed` | No current link change. |
| Download or extraction failure | `Failed`, `ExecutionFailed` | No link changes after failure. |
| Empty archive entry | `Failed`, `ExecutionFailed` | No link changes after failure. |
| Staged binary fails `version` | `Failed`, `ExecutionFailed` | Current and last-good remain unchanged. |
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
