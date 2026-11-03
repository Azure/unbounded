# Unbounded Agent v2: Control and State

Part of the [Unbounded Agent v2 design](README.md). The README has the status,
repository baseline, citation conventions and terms.

## 1. Control plane integration

```go
package control

type Goal struct {
	// Version is opaque: a MachineConfigurationVersion name, an ARM ETag or a
	// digest of the configuration file. It is compared for equality only.
	Version string
	// Settings holds only goal settings (CLI and configuration doc,
	// section 2). Install-time settings never come from a goal.
	Settings *config.GoalSettings
	// Delete is true when the machine is being removed and the node must be
	// reset.
	Delete bool
}

type Observation struct {
	Node    *NodeObservation // nil when the Node object does not exist
	Applied *state.Applied
}

type GoalSource interface {
	Register(ctx context.Context, r Registration) error
	Get(ctx context.Context, obs Observation) (*Goal, error)
	Trigger() Trigger // an event source, or a poll interval
}

type StatusReporter interface {
	Report(ctx context.Context, s Status) error
}

// OperationSource is where MachineOperations come from. The MachineOperation
// CRD is the only implementation (section 6); it is an interface so that
// another source, such as operations from AKS's ARM API, can be added later
// without changing the handlers.
type OperationSource interface {
	Pending(ctx context.Context) ([]Operation, error) // this machine's operations still to run
	Start(ctx context.Context, op Operation, message string) error
	Finish(ctx context.Context, op Operation, r OperationResult) error
	// Restored returns results recorded before a restart, such as an
	// AgentUpgrade's, for the daemon to finish once it is ready.
	Restored(ctx context.Context) ([]PendingResult, error)
	Trigger() Trigger
}
```

Built-in providers:

- `machina`: registers the Machine, builds the goal settings as v1 does, and
  writes Machine status. v1 starts from the running node's applied
  configuration and overlays the template of the MachineConfigurationVersion
  that the Machine's `spec.configurationRef` names: the Kubernetes version,
  node labels, taints, agent image and LocalDNS settings
  (`cmd/agent/internal/daemon/controller_node.go:75-105,143-167`). Without a
  `configurationRef`, the goal settings are the applied ones. Other edits to
  the Machine's spec never change the goal. On the `host` runtime, the
  template's agent image is ignored with a logged warning and left out of
  drift: it is an nspawn root filesystem image, which means nothing without
  nspawn, and the API requires one in every version
  (`api/machina/v1alpha3/machineconfiguration_types.go:137-140`). Counting it
  as drift would repave, and wipe, a `host` node for nothing. The template's
  LocalDNS settings still apply.
- `static`: the goal settings in the agent's copy of the configuration file,
  `/etc/unbounded/agent-v2/config.json`, are the goal; the version is their
  digest. The daemon watches the file. An operator changes the goal by editing
  its goal settings. Edits to install-time settings are ignored and logged as a
  warning ([CLI and configuration, section 2](01-cli-and-configuration.md#2-configuration)).
- `plugin`: delegates to a plugin ([extensions, section 3](02-extensions.md#3-plugin-protocol)). The daemon calls `goal.get` on
  every Node event and every `control.goal.pollInterval`, which defaults to 10
  minutes with up to 10% random jitter. The interval applies only to this
  source.

Bootstrap does not ask the goal source for a goal. It builds the first node
from the configuration file and records an empty goal version. The daemon
records the goal's version as applied on its first check whose goal settings
match the applied ones (section 5), usually seconds after it starts, so a goal
source learns straight away which version the node runs. v1 records a version
only when it handles a Node deletion
(`cmd/agent/internal/daemon/controller_node.go:43-68`).

The `machina` status reporter writes what v1 writes, at the same moments, and
adds two conditions and one status field. It never writes `status.phase`:
machina owns that field and branches on it, and a Machine in `Pending`,
`Failed`, `Ready` or no phase can go down machina's provisioning path
(`cmd/machina/machina/controller/machine_controller.go:276-303`).

| Machine field | Written | As in v1 |
|---|---|---|
| `AgentBootstrapped` condition | During bootstrap: `Running`, then `Succeeded`, or a failure reason. v2 adds two reasons: `NodeStartFailed` when the `host` runtime cannot start the node, where v1's `NSpawnFailed` would be misleading, and `RuntimeUnsupported` when the host fails a capability probe for the requested runtime ([node runtime, section 5.3](03-node-runtime.md#53-host-capabilities)). | Yes, plus the two reasons (`cmd/agent/internal/cmd/bootstrap.go:260-272`, `cmd/agent/internal/daemon/bootstrap_status.go:85-112`) |
| `RepavePending` condition and `status.configuration` | Whenever the daemon records a goal version: after a repave, and on any check whose goal settings match the applied ones (section 5) | The same fields and values (`controller_node.go:54-68,116-133`). v1 writes them only when it handles a Node deletion, so v2 can write them earlier, but only when nothing needs to change. |
| `status.observedProvisioningFormat` | When the daemon starts and registers the Machine | Yes (`cmd/agent/internal/daemon/daemon.go:327,340,350`) |
| `AgentHealthy` condition | `True` with reason `Healthy` while the daemon works normally; `False` with reason `NodeNotRegistered`, `RepaveFailed`, `StateMismatch`, `StateCorrupt` or `PackageOwnedNodePath`, with the attempt count and last error in the message where they apply | New |
| `RuntimeChangePending` condition | `True` when `spec.agent.runtime` differs from the runtime installed on the host, until a reinstall applies it ([CLI and configuration, section 2](01-cli-and-configuration.md#2-configuration)) | New |
| `status.agent.runtime` | The runtime installed on the host, when the daemon starts | New |

A requested runtime change is not a health problem, which is why it has its own
condition rather than a reason on `AgentHealthy`. The daemon can already update
Machine status (`deploy/machina/07-bootstrapper-rbac.yaml.tmpl:17-19`), so none
of this needs new permissions. The phases the status reporter receives
([extensions, section 3](02-extensions.md#3-plugin-protocol)), such as `Pending`, are not written to the Machine, so
`RepavePending` is never set to `True` by a goal that is only waiting for a Node
deletion. Provisioning does not apply `configurationRef` yet (issue #1039), so a
Machine whose referenced version differs from what its node was built from
would otherwise show a pending repave right after bootstrap. Nothing in this
repository reads `status.configuration` or `RepavePending` apart from the
agent and an e2e check after a repave (`hack/agent/e2e-kind/e2e.py:6097-6107`),
so writing them earlier when nothing differs affects no consumer.

## 2. Credentials

```go
package credentials

type Bootstrap struct {
	APIServer string
	CACert    []byte
	Token     string // empty when kubelet uses exec or kubeconfig auth
}

type BootstrapProvider interface {
	Bootstrap(ctx context.Context) (*Bootstrap, error)
}

type ClusterAuth interface {
	RESTConfig() *rest.Config
	Run(ctx context.Context) error // rotation
}
```

The `csr` daemon credential reuses `pkg/agent/daemoncred`. The kubelet's three
authentication modes (bootstrap token, kubeconfig, exec credential) are carried
over from v1 (`pkg/agent/phases/nodestart/kubelet.go:282-379`).

## 3. Node manager

```go
package nodemanager

type Manager interface {
	Ensure(ctx context.Context, cfg *config.AgentConfiguration) error // bootstrap on slot a
	Apply(ctx context.Context, g *control.Goal) error                 // repave into the other slot
	Reboot(ctx context.Context) error
	Reset(ctx context.Context, trigger ResetTrigger) error
	Recover(ctx context.Context) error // finish or clean up interrupted work
	// Observe reads the Node and the applied state, and calls the runtime's
	// Inspect. The daemon calls it on every reconcile.
	Observe(ctx context.Context) (control.Observation, error)
}

// Trigger is why the daemon is reconciling: NodeDeleted when it saw this
// node's Node object being deleted, NodeChanged when the Node's status
// changed, otherwise GoalChanged, Poll or Startup. Whether the Node counts as
// deleted also depends on the observation (section 5), not only on the
// trigger.
type Trigger string

type Action string // Steady, Wait, Repave, Reset

func Decide(t Trigger, obs control.Observation, g *control.Goal, drift func(applied, desired *config.GoalSettings) bool) Action
```

The drift function starts from v1's `hasDrift`
(`cmd/agent/internal/daemon/nodeoperator.go:140-193`). Fields that only matter
for one runtime, such as the nspawn OCI image, are contributed by that runtime.

## 4. State

```go
package state

type Applied struct {
	GoalVersion  string               // empty until the daemon first sees a goal with matching settings
	Settings     *config.GoalSettings // the goal settings the running node was built from
	BundleDigest string               // digest of that node's saved bundle manifest
	StartedAt    time.Time            // when that node started, for the registration timeout
	NodeUID      string               // UID of that node's Node object, once the daemon has seen it
	Repave       *Repave              // non-nil from the moment a repave is approved
}

type Repave struct {
	ApprovedAt   time.Time    // when the daemon saw the Node deleted
	Slot         runtime.Slot // empty until the new slot's bundle is rendered
	GoalVersion  string
	Settings     *config.GoalSettings
	BundleDigest string // empty until the new slot's bundle is rendered
	Attempts     int
	LastError    string
}
```

The daemon saves `Repave` as soon as it handles a Node deletion that needs a
repave, before it resolves the node spec or downloads anything. Once saved, a
repave survives any restart of the daemon or the host: `Recover` continues it,
and an operator never has to approve it again. v1 keeps the approval only in
memory, so a daemon restart before the repave finishes loses it
([v1 compatibility, section 2](08-v1-compatibility.md#2-behavior-changes-from-v1)).

A single `Applied` record replaces v1's per-slot applied-configuration files
([v1 compatibility, section 2](08-v1-compatibility.md#2-behavior-changes-from-v1)).

The record does not say which slot is enabled or running; only the runtime
knows that ([node runtime, section 1](03-node-runtime.md#1-node-runtime)). `Recover` compares the enabled slot, and its bundle
digest as reported by `Inspect`, with this record. It checks both the slot and
the digest, so a repave whose new bundle happens to match the old one is never
mistaken for no repave:

| Enabled slot | Meaning | `Recover` does |
|---|---|---|
| Not `Repave.Slot`, digest matches `Applied.BundleDigest` | No repave in progress, or one that never got as far as starting the new slot, including one approved but not yet rendered (`Repave.Slot` empty) | Continues or retries the approved repave, if any, from the beginning of its first unfinished step |
| `Repave.Slot`, digest matches `Repave.BundleDigest` | The new slot started, but the commit did not happen | Waits for readiness, then commits |
| None, and `Repave` is set | The old slot was stopped, but the new one was not started | Continues the approved repave from starting the new slot |
| Any other case | The record and the host disagree | Reports `StateMismatch` and changes nothing; the daemon runs in limited mode (section 5) |

The runtime kind is an install-time setting, so it lives in the install
record, not here.

### State files

The rules for every v2 state file: the install record, node state, the host
preparation undo log, the host runtime's ownership and active-slot records,
saved bundle manifests, and the migration record on a host migrated from v1.

- Each file has a `schemaVersion`, is written and synced to disk before it is
  used, and has a `.sha256` sidecar.
- New fields are only ever added, never renamed or removed, and older readers
  ignore fields they don't know. An upgrade can roll back to the `last-good`
  binary ([lifecycles, section 5](05-lifecycles.md#5-agentupgrade)), which must still be able to read what the newer binary
  wrote.
- If a file has an unknown `schemaVersion` or fails its checksum, the daemon
  makes no changes, reports `StateCorrupt` and runs in limited mode (section 5).
  `reset` still runs, using the conservative paths described in
  [host runtime cleanup, section 9](07-host-runtime-cleanup.md#9-teardown).
- The state files listed above, the configuration copy and daemon credentials
  under `/etc/unbounded/agent-v2/`, and every file the bundle marks as secret
  have mode 0600. The configuration copy can hold a bootstrap token. Slot
  binaries and the artifact cache keep the modes they need to run.

## 5. Daemon reconcile

```mermaid
flowchart TD
  T1["Node deleted, or Node status changed"] --> Q
  T2["Goal source events or poll timer"] --> Q
  T3["MachineOperation events"] --> Q
  T4["Startup"] --> Q
  Q[("Serialized queue, one worker")] --> G{"Startup gate open?"}
  G -- no --> RQ["Requeue"]
  G -- yes --> L{"Install lock acquired?"}
  L -- no --> RQ
  L -- yes --> P{"Approved repave in state?"}
  P -- yes --> RF["nodemanager.Recover: finish the interrupted repave"]
  P -- no --> K{"Request kind"}
  K -- operation --> OP["NodeReboot, AgentUpgrade, AgentReset or HostOSReboot handler"]
  K -- reconcile --> O["Observe Node, goal, applied state and runtime state"]
  O --> D{"Decide"}
  D -- Reset --> RS["Reset, then stop daemon"]
  D -- Repave --> AP["Apply goal"]
  D -- Wait --> W["status.report Pending"]
  D -- Steady --> S["status.report Ready, or Provisioning while the Node does not exist"]
```

The daemon reports ready to systemd, with `READY=1` (the daemon unit is
`Type=notify`), once its local checks pass: its state files are readable,
`Recover` has compared the host with the state (section 4), and every
configured plugin has answered `describe` with the methods the configuration
uses, the calls made in parallel ([extensions, section 3](02-extensions.md#3-plugin-protocol)). Right after an AgentUpgrade,
while the upgrade signal is pending, it must also register or read its own
Machine with one authenticated API call, which proves the new binary can reach
the API ([lifecycles, section 5](05-lifecycles.md#5-agentupgrade)). Readiness never waits for anything else:

- **Not for the API server on an ordinary start.** The daemon connects in the
  background and keeps retrying, so a control-plane outage never keeps it from
  starting. v1's daemon exits when it cannot register its Machine
  (`cmd/agent/internal/daemon/daemon.go:166-168`). During a long outage it
  fails at every start, its start limit trips, and the recovery unit swaps it
  for `last-good` (`unbounded-agent-daemon-recovery.sh:11,26`): a binary
  rollback for a fault the binary does not have.
- **Not for the caches.** On a large cluster they can take a long time to sync:
  the daemon, like v1's, caches every MachineOperation in the cluster (issue
  #1058). A slow sync must not fail an upgrade.

The startup gate opens once the daemon is ready, has reached the API server and
its caches have synced. It then publishes pending AgentUpgrade and HostOSReboot
results, as v1 publishes them when its daemon starts
(`cmd/agent/internal/daemon/daemon.go:170-176`), and starts taking requests from
the queue. An approved repave that `Recover` found is finished from the queue,
so a long repave never delays readiness either.

When `Recover` finds the state inconsistent or corrupt (`StateMismatch`,
`StateCorrupt`):

- **Right after an AgentUpgrade,** the daemon reports the reason on the
  Machine if it can and exits with a failure code instead of reporting ready.
  The new binary disagrees with a state the old one accepted, so it is the
  likely fault, and the recovery unit rolls it back.
- **On any other start,** the daemon reports ready in a limited mode. It shows
  `AgentHealthy=False` with the reason, makes no change to the node, and runs
  only AgentReset, which handles corrupt state with conservative paths
  ([host runtime cleanup, section 9](07-host-runtime-cleanup.md#9-teardown)), and AgentUpgrade, so a fixed binary can be shipped.
  Every other request waits. v1's daemon instead exits when it cannot make
  sense of the host's machines (`daemon.go:137-140`), which leaves only access
  to the host, or a HostReplace, to fix it.

`Decide` repaves only after this node's Node object has been deleted, which is
how an operator or a control plane approves a repave. v1 only reacts to a
deletion event it receives while running, and ignores creates and updates
(`cmd/agent/internal/daemon/controller.go:147-156`), so a Node deleted while
its daemon restarts is never acted on: the node keeps running with no Node
object, and whoever deleted it waits forever. v2 also notices a deletion it
did not see happen. The daemon saves the UID of the node's Node object in
`Applied.NodeUID` the first time it sees it, and on every check compares:

| What the daemon finds | Node deleted? |
|---|---|
| A deletion event for the Node | Yes |
| No Node, and a UID is saved | Yes: it was deleted while the daemon was not watching, for example while it restarted |
| A Node with a different UID | Yes: it was deleted and registered again while the daemon was not watching, for example around a host reboot, when kubelet registers again before the daemon starts *(inference: kubelet registers once at startup)* |
| A Node with the saved UID, or a Node and no saved UID | No. The daemon saves the UID if none is saved. |
| No Node, and no UID saved | No: the node has not registered yet |

| Node deleted? | Goal `delete` | Drift | Action |
|---|---|---|---|
| Any | `true` | Any | Reset |
| Yes | `false` | Yes | Repave: save `Repave`, then `Apply` |
| Yes | `false` | No | Steady |
| No | `false` | Yes | Wait: a repave is pending until the Node is deleted |
| No | `false` | No | Steady |

When the action is Steady, the daemon records the goal's version as applied if
it is not already, and the UID of the Node it sees, if any. v1 records a goal
version only when it handles a Node deletion
(`cmd/agent/internal/daemon/controller_node.go:54-62`), so v2 records it
sooner, but only when nothing needs to change (section 1). A repave starts a
new `Applied` record, whose `NodeUID` is empty until the new Node registers.

A Node deleted while the daemon is not running is acted on when the daemon
starts again. Stopping the daemon still keeps a node as it is, for example
while debugging, but only for as long as the daemon is stopped.

The reported phase is `Pending` while a repave waits for the Node to be
deleted. Otherwise it follows the Node: `Ready` while the Node exists, and
`Provisioning` while it does not.

"Record the goal version as applied" changes nothing on the running node. Goal
settings that the drift function ignores, such as artifact download sources,
are saved with the version and take effect at the next provision.

A node that never registers is reported but not repaved: with no Node seen,
no UID is saved, so a missing Node is never taken for a deletion. This matches
v1, where such a node also stays as it is. The
`NodeNotRegistered` status, counted from `Applied.StartedAt`, makes the problem
visible, including on the Machine as `AgentHealthy=False` (section 1). Recovery
is manual, as in v1: an `AgentReset` operation or `reset`, then `start`.

## 6. MachineOperation handling

Operations are read from the MachineOperation CRD, which is not installed on
every cluster: AKS clusters running AKS Flex Node often lack it. Flex detects
that at startup and then ignores operations
(`pkg/daemon/machineoperation_reconciler.go:58-77,114-126` at Flex
`2c149e991`). A watch on a type the API server does not know would instead
keep the manager from starting *(inference: controller-runtime fails to start
a source whose kind has no REST mapping)*. With `control.operations.enabled`
set, the default, the daemon therefore:

- checks discovery for the MachineOperation type when it starts. Every
  authenticated client may read discovery, so this needs no new permission
  *(inference: Kubernetes' default `system:discovery` binding)*;
- if the type is missing, runs without the watch, logs it once, reports it in
  `status`, and checks again every 10 minutes, with up to 10% random jitter;
- starts the watch as soon as the type appears, without a restart.

Every machina cluster has the CRD, so machina hosts behave as in v1.

The CRD is the only operation source (section 1). AKS Flex Node plans to take
operations from AKS's ARM API instead (`docs/design/agent-and-aks.md:16` at
Flex `2c149e991`), but AKS has not defined that contract, so v2 does not design
a plugin protocol for it yet ([overview, section 3](README.md#3-open-questions)).

`OperationKind` is a closed enum (`api/machina/v1alpha3/machineoperation_types.go:54`).
There are no custom kinds: consumers customize the agent through plugins,
hooks and declarative extensions ([extensions](02-extensions.md)) instead. Each kind is
executed by one component:

- The agent executes `NodeReboot`, `AgentUpgrade`, `AgentReset` and
  `HostOSReboot` (`cmd/agent/internal/daemon/controller_machineoperation.go:45-51`;
  [lifecycles, section 4](05-lifecycles.md#4-hostosreboot)).
- machine-ops-controller and metalman execute the other `Host*` kinds, and do
  not execute `HostOSReboot` (`internal/machineops/controller.go:382-391`,
  `internal/metalman/machineops/controller.go:1238-1245`).

v2 ignores an operation whose kind it has no handler for, as v1 does
(`pkg/agent/daemon/machineoperation.go:114-118,139-141`). Failing such
operations instead was considered and rejected:

- The kind list is closed by the CRD, so an agent only meets a kind it does not
  know when the CRD is newer than the agent.
- No naming rule identifies the agent's kinds: the agent executes
  `HostOSReboot`, a `Host*` kind.
- A selector operation has one status shared by every matching agent, and
  agents skip it once it is terminal
  (`pkg/agent/daemon/machineoperation.go:105-107`). One out-of-date agent
  failing it would fail it for every agent (issue #1040).

v2 also deletes expired operations, which v1 never does. When an operation
aimed at this Machine with `machineRef`, and run by this agent, has
`ttlSecondsAfterFinished` set, the daemon deletes it that many seconds after it
completes or fails, as metalman does
(`internal/metalman/machineops/controller.go:1128-1145`). Without it, a cluster
that runs neither machine-ops-controller nor metalman keeps agent operations
forever (issue #1043), and the daemon lists every operation each time it starts
to report `HostOSReboot` results (`cmd/agent/internal/daemon/hostosreboot.go:207-210`).
Selector operations are left to the controllers, so agents never race to delete
a shared operation. The daemon's role already allows deleting operations
(`deploy/machina/07-bootstrapper-rbac.yaml.tmpl:23-25`).

v2 implements both in its own copy of the reconciler, so `pkg/agent/daemon` is
unchanged for v1 and Flex.
