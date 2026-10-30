# Unbounded Agent v2: Lifecycles

Part of the [Unbounded Agent v2 design](README.md). The README has the status,
repository baseline, citation conventions and terms.

## 1. Bootstrap (`v2 start`)

```mermaid
sequenceDiagram
  autonumber
  participant Op as cloud-init, Ignition or operator
  participant CLI as v2 start
  participant St as state
  participant Px as providers and hooks
  participant HP as hostprep
  participant NM as nodemanager
  participant RT as NodeRuntime
  participant SD as systemd
  Op->>CLI: start with config
  CLI->>CLI: log experimental warning, load and validate config, select runtime
  CLI->>St: acquire install lock, run the install check
  alt already complete
    CLI->>NM: verify node and daemon, repair daemon if needed, ignore other differences in the file
  else fresh or resume
    CLI->>Px: hook agent.install.before
    CLI->>Px: hook host.prepare.before
    CLI->>HP: common tasks, runtime requirements, extension host units
    CLI->>Px: hook host.prepare.after
    CLI->>Px: credentials.bootstrap
    CLI->>Px: goal.register, status.report Provisioning
    CLI->>NM: Ensure
    NM->>RT: Inspect
    NM->>NM: resolve NodeSpec, cache artifacts, render the bundle for slot a
    NM->>Px: hook node.provision.before, unless a is already running
    NM->>RT: Provision a and save its manifest, unless a is already running
    NM->>Px: hook node.provision.after, with resumed true if a was already running
    NM->>Px: hook node.start.before, unless a is already running
    NM->>NM: write slot a's host units
    NM->>RT: Start a, which enables it
    NM->>RT: Access a, wait for readiness
    NM->>St: save applied state from a's saved manifest, with an empty goal version
    NM->>Px: hook node.start.after
    CLI->>SD: install and start unbounded-agent-v2-daemon
    CLI->>Px: hook agent.install.after
    CLI->>St: mark install complete
  end
```

## 2. Repave

```mermaid
sequenceDiagram
  autonumber
  participant D as daemon
  participant NM as nodemanager
  participant St as node state
  participant Px as providers and hooks
  participant RT as NodeRuntime
  D->>St: sees the Node deleted with drift, saves Pending
  D->>NM: Apply goal
  NM->>Px: hook node.repave.before, credentials.bootstrap
  NM->>NM: resolve NodeSpec, cache artifacts, render the bundle for the new slot
  NM->>St: record the new slot and its bundle digest in Pending
  NM->>Px: hook node.provision.before new
  NM->>RT: Provision new and save its manifest
  NM->>Px: hook node.provision.after new
  NM->>Px: hook node.stop.before old
  NM->>RT: Stop old, which disables it
  NM->>Px: hook node.stop.after old
  NM->>NM: host network reset, then write the new slot's host units
  NM->>Px: hook node.start.before new
  NM->>RT: Start new, which enables it
  NM->>RT: Access new, wait for readiness
  NM->>Px: hook node.start.after new
  NM->>St: commit the applied goal and digest, clear Pending
  NM->>RT: Destroy old
  NM->>Px: hook node.repave.after, status.report Provisioning
```

"Old" is the slot that was running before the repave, and "new" is the other
slot. After the first repave, the old slot is `b`.

`Recover` after a crash follows the table in [control and state, section 4](04-control-and-state.md#4-state). In addition, a
provisioned slot that is neither enabled nor pending is destroyed.

When a repave fails, it is retried, matching v1:

- Retries never stop. They use controller-runtime's default per-item backoff,
  as v1 does: v1 configures no rate limiter of its own
  (`pkg/agent/daemon/setup.go:45-50`). Unlike v1, they also continue after a
  daemon or host restart, because `Pending` was saved when the deletion was
  seen.
- Every attempt reads the goal again, as v1 does
  (`cmd/agent/internal/daemon/controller_node.go:43`). A goal change also
  queues an attempt at once, so a corrected goal does not wait out the backoff.
- Each failed attempt is reported as `Failed` with reason `RepaveFailed`, the
  attempt count and the last error (`Pending.Attempts`, `Pending.LastError`).
- There is no rollback. The old slot stays stopped and intact until a later
  attempt succeeds, and is destroyed only then. Starting the old node again is
  not useful: the control plane deleted its Node to approve the repave, and its
  workloads are already gone.
- If stopping the old slot fails, for example with `ErrNodeBusy` or
  `ErrMountsRemain` on `host`, the attempt ends there and the new slot is not
  started. The node stays down until a retry succeeds, as in v1.

If the goal changes while a repave is in progress:

- Before the new slot has been started, the next attempt replaces `Pending`
  and provisions the new slot again from the new goal. If the new goal no
  longer has drift, the attempt clears `Pending` and records the goal version
  as applied, as for a deletion with no drift.
- Once the new slot has been started, the repave finishes and commits the goal
  it started with. The newer goal is then ordinary drift and needs another Node
  deletion ([control and state, section 5](04-control-and-state.md#5-daemon-reconcile)).

## 3. NodeReboot

```mermaid
sequenceDiagram
  participant D as daemon
  participant NM as nodemanager
  participant RT as NodeRuntime
  participant Px as hooks and status
  D->>Px: MarkInProgress
  D->>NM: Reboot
  NM->>Px: hook node.restart.before
  NM->>RT: Restart the enabled slot
  NM->>RT: Access, wait for readiness
  NM->>Px: hook node.restart.after
  D->>Px: Finish Complete or Failed
```

`Restart` per runtime:

- `nspawn-container`: `systemctl restart systemd-nspawn@<m>`, which runs the
  hooks again, matching v1
  (`pkg/agent/phases/nodestart/nspawn_lifecycle.go:36-40`).
- `host`: Terminate and Unmount ([host runtime cleanup](07-host-runtime-cleanup.md), sections 6 and 7), then start
  the node units again. Starting them runs the prerequisites and the pre-start
  boot hook again first ([node runtime, section 4](03-node-runtime.md#4-host-boot-and-boot-units)). The pre-start boot hook gives
  kubelet a new boot ID, so the Node shows the same reboot signals as on
  `nspawn-container` ([node runtime, section 5.2](03-node-runtime.md#52-host)).

There is no host network reset on reboot, matching v1's `RestartNode`
(`cmd/agent/internal/daemon/nodeoperator.go:224-229`).

## 4. AgentUpgrade

```mermaid
sequenceDiagram
  participant D as daemon (old)
  participant AB as agentbinary
  participant SD as systemd
  participant N as daemon (new)
  D->>D: install lock, binary activation lock, MarkInProgress
  D->>D: hook agent.upgrade.before
  D->>AB: download, verify sha256, install into the inactive blue or green binary
  AB->>AB: candidate must answer version and v2 version
  AB->>AB: switch current link, keep last-good
  D->>D: write upgrade signal, re-exec current
  N->>N: startup gate publishes Complete from the signal
  N->>N: hook agent.upgrade.after
  SD-->>SD: on repeated failure the recovery unit restores last-good, whose daemon publishes Failed and fires agent.upgrade.after
```

This is v1's mechanism
(`cmd/agent/internal/daemon/controller_machineoperation.go:79-165`,
`pkg/agent/agentbinary/upgrade.go:193-273`) with two additions:

- A candidate binary is checked with `v2 version` as well as `version`
  (`pkg/agent/agentbinary/agentbinary.go:125`), so a binary without the `v2`
  command is rejected before the switch.
- The daemon unit runs `<current> v2 daemon`.

The `v2 agent-upgrade` command is the other way in. Host delivery automation
runs it on the host, from a separately staged candidate binary, without a
MachineOperation, as v1's hidden `agent-upgrade` command is run
(`cmd/agent/internal/cmd/agentupgrade.go:63-70`). It uses the same activation:
install into the inactive blue or green binary, switch the `current` link, restart the daemon
unit and wait until it is healthy, rolling back if it is not
(`pkg/agent/agentbinary/activation.go:86-246`). The `agent.upgrade.before` and
`agent.upgrade.after` events fire on this path too.

## 5. Reset

Reset is triggered by `v2 reset`, an `AgentReset` MachineOperation, or a goal
with `delete: true`.

```mermaid
sequenceDiagram
  participant T as trigger
  participant St as state
  participant Px as hooks
  participant RT as NodeRuntime
  participant HN as hostnet
  participant HP as hostprep
  T->>St: install lock, mark Resetting
  T->>Px: hook agent.reset.before
  T->>T: CLI only, stop and remove the daemon unit first
  T->>Px: hook node.stop.before, if a node is running
  T->>RT: Teardown, covering both slots and everything the runtime owns
  T->>Px: hook node.stop.after, if a node was running
  T->>HN: host network reset, teardown scope
  T->>HP: replay the undo log
  T->>St: remove node state, artifact cache, slots, any migration backup and /etc/unbounded/agent-v2
  T->>T: remove the agent binary directory, /opt/unbounded/agent
  T->>Px: hook agent.reset.after
  T->>St: remove the install record
  T->>T: operation or goal only, stop and remove the daemon unit last
```

The order of stopping the daemon matches v1. `v2 reset` stops the daemon first,
so it cannot act during the reset (`cmd/agent/internal/daemon/reset.go:29-31`).
When the daemon itself runs the reset, for an `AgentReset` operation or a goal
with `delete: true`, it stops its own unit last, after it has reported the
result.

The agent binary directory is removed because v1 cannot be installed alongside
v2 ([v1 compatibility, section 4](08-v1-compatibility.md#4-existing-packages-used-as-libraries)), so nothing else uses it. v1's reset removes it the same way
(`cmd/agent/internal/daemon/lifecycle.go:343-351`).

## 6. Migration from v1

A v1 host becomes a v2 host without replacing its node: v2 takes over the
running nspawn machine as its slot and replaces only the agent around it. The
lifecycle, its crash behavior and rollback are in
[migrating from v1](10-migration-from-v1.md).
