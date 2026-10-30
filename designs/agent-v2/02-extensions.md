# Unbounded Agent v2: Extensions

Part of the [Unbounded Agent v2 design](README.md). The README has the status,
repository baseline, citation conventions and terms.

## 1. Extension mechanisms

| Mechanism | Use it for | Can it replace built-in behavior? |
|---|---|---|
| Configuration | Kubelet settings, labels, taints, versions, the LocalDNS template, credential modes | Not applicable |
| Declarative extensions (section 2) | Extra node binaries, files and units; extra host units; ordering before node boot; cleanup declarations | No, additive only |
| Providers, through exec plugins (section 3) | Goal source, status reporter, bootstrap credentials, daemon credentials, extra preflight checks | Yes, a provider replaces one built-in implementation |
| Lifecycle hooks, through exec plugins (section 3) | Side effects at fixed points | No. Hooks observe, and can fail the operation. |

Use the first mechanism that fits, in this order. There are no in-process Go
plugins: consumers never link agent code.

### Lifecycle hook events

Events are named `<subject>.<action>.<phase>`:

- The subject is `agent`, `host` or `node`.
- The action is a lowercase word, such as `install`, `start` or `repave`. Dots
  only separate segments, so a future action that needs more than one word
  would use hyphens.
- The phase is `before` or `after`. Every action has both.

When events fire:

- A `before` event fires immediately before its step. If a hook with failure
  policy `Fail` fails, the step does not run and the operation fails.
- An `after` event fires when its step completes, whether it succeeded or
  not. The payload carries `result` (`Succeeded` or `Failed`) and `message`. If
  the step succeeded and a hook with policy `Fail` then fails, the operation is
  reported as failed, but the step is not rolled back.
- Hooks fire every time the agent runs a step, including on a retried
  operation, so hooks must be idempotent.
- On a resumed bootstrap, a step that is already done, such as provisioning a
  slot that is already running, is skipped. Its `before` event does not fire,
  but its `after` event fires again with `resumed: true` in the payload. A hook
  that failed on the earlier attempt is therefore retried, and the bootstrap is
  not marked complete until it succeeds.
- No events fire when systemd starts the node at boot ([node runtime, section 1](03-node-runtime.md#1-node-runtime), rule 6).
- If a hook entry has no failure policy, each event uses its default from the
  table below, including when the entry matches through a wildcard. The
  default is `Fail` for steps that bring the node or agent up. It is `Ignore`
  for stopping and reset, and for the `after` events of repave and upgrade, so
  a broken hook can never block stopping or cleanup.

Every payload includes `event`, the concrete event name. The table lists the
fields specific to each event.

| Event | Fires | Payload | Default failure policy |
|---|---|---|---|
| `agent.install.before` | After the install check accepts a fresh or resumed install, before host preparation | runtime, install kind (`Fresh` or `Resume`) | `Fail` |
| `agent.install.after` | After the daemon is installed, before the install is marked complete | runtime, slot | `Fail` |
| `host.prepare.before` | Before host preparation | runtime | `Fail` |
| `host.prepare.after` | After host preparation | runtime | `Fail` |
| `node.provision.before` | Before `Provision(slot)` | slot, goal version | `Fail` |
| `node.provision.after` | After `Provision(slot)` | slot, goal version, bundle digest | `Fail` |
| `node.start.before` | Before `Start(slot)` | slot, goal version | `Fail` |
| `node.start.after` | After `Start(slot)` and the readiness wait. During a repave, before the commit. | slot, goal version, node name | `Fail` |
| `node.stop.before` | Before `Stop(slot)` | slot, reason (`Repave` or `Reset`) | `Ignore` |
| `node.stop.after` | After `Stop(slot)` | slot, reason | `Ignore` |
| `node.restart.before` | Before `Restart(slot)` for a NodeReboot | slot, operation name | `Fail` |
| `node.restart.after` | After `Restart(slot)` and the readiness wait | slot, operation name, node name | `Fail` |
| `node.repave.before` | Before a repave begins | old slot, new slot, applied and goal versions | `Fail` |
| `node.repave.after` | After the repave commits and the old slot is destroyed, or after an attempt fails | old slot, new slot, goal version | `Ignore` |
| `agent.upgrade.before` | Before the new binary is staged, in the daemon or the `v2 agent-upgrade` command | current version, operation name if any | `Fail` |
| `agent.upgrade.after` | In the daemon that publishes the upgrade result | old and new version, operation name if any | `Ignore` |
| `agent.reset.before` | Before reset begins | trigger (`CLI`, `Operation` or `Goal`) | `Ignore` |
| `agent.reset.after` | After reset, before the daemon unit is removed | trigger, leftovers | `Ignore` |

The order of events in each lifecycle:

| Lifecycle | Events, in order |
|---|---|
| Bootstrap ([lifecycles, section 1](05-lifecycles.md#1-bootstrap-v2-start)) | `agent.install.before`, `host.prepare.before`, `host.prepare.after`, `node.provision.before`, `node.provision.after`, `node.start.before`, `node.start.after`, `agent.install.after` |
| Repave ([lifecycles, section 2](05-lifecycles.md#2-repave)) | `node.repave.before`, `node.provision.before` (new), `node.provision.after` (new), `node.stop.before` (old), `node.stop.after` (old), `node.start.before` (new), `node.start.after` (new), `node.repave.after` |
| NodeReboot ([lifecycles, section 3](05-lifecycles.md#3-nodereboot)) | `node.restart.before`, `node.restart.after` |
| AgentUpgrade ([lifecycles, section 4](05-lifecycles.md#4-agentupgrade)) | `agent.upgrade.before` in the old daemon or the CLI, `agent.upgrade.after` in the new daemon |
| Reset ([lifecycles, section 5](05-lifecycles.md#5-reset)) | `agent.reset.before`, `node.stop.before` and `node.stop.after` if a node is running, `agent.reset.after` |

NodeReboot has its own `restart` pair rather than stop and start events,
because for `nspawn-container` a restart is a single unit restart.

During a repave, `node.start.after` for the new slot fires before the commit. A
hook that fails under `Fail` therefore leaves the repave pending, and it is
retried like any other failed attempt ([lifecycles, section 2](05-lifecycles.md#2-repave)).

### Subscribing to events

`hooks[].event` must have exactly three segments. Each segment is either a
known value or `*`, which matches any value for that whole segment. Examples:
`node.start.after`, `node.*.after`, `agent.upgrade.*`.

Validation rejects:

- partial wildcards such as `node.st*`, and unknown values, which catches
  typos;
- two entries for the same plugin that can match the same event, such as
  `node.start.after` and `node.*.after` for one plugin. Each plugin therefore
  receives each event at most once.

When entries for different plugins match one event, their hooks run one at a
time in the order they appear in the configuration.

## 2. Declarative extensions

An extension adds content to the node and the host. It cannot change what the
agent itself renders.

```json
{
  "name": "node-problem-detector",
  "artifacts": [
    {
      "name": "npd",
      "url": "https://example.invalid/npd.tar.gz",
      "sha256": "...",
      "extract": "bin/node-problem-detector",
      "nodePath": "/usr/local/bin/node-problem-detector",
      "mode": "0755"
    }
  ],
  "node": {
    "files": [ { "path": "/etc/npd/kernel-monitor.json", "mode": "0644", "content": "..." } ],
    "units": [ { "name": "node-problem-detector.service", "content": "...", "start": "AfterKubelet" } ]
  },
  "host": {
    "units": [ { "name": "static-routes.service", "content": "...", "orderBeforeNode": true } ],
    "files": [ { "path": "/etc/aks-flex-node/static-routes.sh", "mode": "0755", "content": "..." } ]
  },
  "sharedHostPaths": [ { "hostPath": "/opt/aks-flex-node/bin", "nodePath": "/opt/aks-flex-node/bin", "readOnly": true } ],
  "nodeDependencies": { "requires": ["systemd-udev-settle.service", "himdsd.service"] },
  "cleanup": {}
}
```

Rules:

- Extension artifacts go through the same artifact cache, offline sources and
  preflight reachability checks as built-in artifacts.
- `host.units` with `orderBeforeNode` run, and must succeed, before every start
  of the node. `nodeDependencies.requires` units must be active before the node
  starts. The runtime writes the systemd wiring for both ([node runtime, section 4](03-node-runtime.md#4-host-boot-and-boot-units));
  consumers never refer to runtime unit names.
- `sharedHostPaths` are host paths made visible inside the node: a bind mount
  for `nspawn-container`, the path itself for `host`. On `host`, `hostPath` must
  equal `nodePath`; validation rejects anything else.
- An extension's node files and units may not use a path or unit name that the
  agent renders itself or that a runtime owns, such as
  `/etc/containerd/config.toml` or `kubelet.service`. Validation rejects them.
- An extension host file or unit that already exists on the host, and that the
  agent did not create, fails preflight. The agent never overwrites host files
  it does not own.
- `cleanup` declares state the extension's workloads create ([host runtime cleanup, section 13](07-host-runtime-cleanup.md#13-extension-cleanup-declarations)).
- Everything an extension creates is recorded in state and removed by reset.
- Extensions are install-time settings: changing or removing one needs a reset
  and a new install ([CLI and configuration, section 2](01-cli-and-configuration.md#2-configuration)).

This replaces the hardcoded `systemd-nspawn@` dependencies in v1's assets and
in Flex, such as Flex's static routes with
`RequiredBy=systemd-nspawn@.service`.

## 3. Plugin protocol

A plugin is an executable owned by the consumer. The agent runs
`<command...> <method>` with a JSON request on stdin and reads a JSON response
from stdout. A non-zero exit or an `error` object means failure. stderr goes to
the agent log, size-bounded. Secrets travel only on stdin, never in arguments
or the environment.

```json
{
  "apiVersion": "plugin.agent.unbounded-cloud.io/v1alpha1",
  "kind": "Request",
  "method": "goal.get",
  "context": { "machineName": "m-01", "nodeName": "m-01", "runtime": "host", "agentVersion": "..." },
  "payload": {
    "node": { "exists": true, "uid": "...", "labels": {}, "taints": [] },
    "applied": { "goalVersion": "..." }
  }
}
```

| Method | Request payload | Response payload |
|---|---|---|
| `describe` | None | `{protocolVersions[], methods[]}` |
| `goal.register` | `{machineName, nodeName, runtime, agentVersion, kubernetesVersion, provisioningFormat}` | None |
| `goal.get` | Node observation, applied state | `{version, settings, delete}`. `settings` holds goal settings only ([CLI and configuration, section 2](01-cli-and-configuration.md#2-configuration)). `delete` is `true` when the machine is being removed and the node must be reset. |
| `status.report` | `{phase, reason, message, appliedVersion, runtime, attempts, leftovers}` | None |
| `credentials.bootstrap` | None | `{apiServer, caCertBase64, token}` |
| `credentials.cluster` | None | A Kubernetes `ExecCredential` |
| `preflight.check` | Configuration summary | `[{name, severity, message}]` |
| `hook` | The event payload, including the event name (section 1) | None. A plugin that handles hooks lists `hook` once in `describe`, whatever events it subscribes to. |

`status.report` phases:

| Phase | Meaning | Reasons |
|---|---|---|
| `Provisioning` | The node is being built, or has started but its Node object has not been seen yet | `NodeNotRegistered` after 10 minutes without a Node ([control and state, section 5](04-control-and-state.md#5-daemon-reconcile)) |
| `Ready` | The node is running and the daemon has seen its Node object | None |
| `Pending` | A repave is waiting for the Node to be deleted | None |
| `Reconciling` | A repave or reboot is in progress | None |
| `Failed` | The agent cannot make progress without a retry or an operator | `StateMismatch` ([control and state, section 4](04-control-and-state.md#4-state)), `StateCorrupt` ([control and state, section 4](04-control-and-state.md#4-state)), `RepaveFailed` ([lifecycles, section 2](05-lifecycles.md#2-repave)) |
| `Resetting` | A reset is in progress | None |

`attempts` is set while a repave is being retried, and `leftovers` lists
anything reset could not remove, as `{kind, id, reason}`. A MachineOperation
the agent cannot handle is failed on the operation itself, with reason
`UnsupportedOperation` ([control and state, section 6](04-control-and-state.md#6-machineoperation-handling)); it does not change the node's phase.

Operational rules:

- **`describe`** is called when `v2 start`, `v2 daemon` or `v2 preflight` starts.
  If a plugin configured as a provider does not list a method the
  configuration uses, startup fails.
- **Timeouts** are set per method in `plugins[].timeouts`. The defaults are 10
  seconds for `describe` and 60 seconds for every other method. A hook's
  timeout is set only by its `hooks[].timeout`, which also defaults to 60
  seconds. On a timeout or cancellation the plugin gets SIGTERM, then SIGKILL
  after 5 seconds.
- **Output limits:** a response larger than 1 MiB on stdout is a failure. Only
  the first 64 KiB of stderr is logged.
- **Ownership:** the plugin's executable path must be absolute. The file and
  every parent directory must be owned by root and not writable by group or
  others. Plugins run as root.
- **Reset never fails because of a plugin.** During `v2 reset` and other
  resets, `describe` failures, provider failures and hook failures are logged
  and reset continues. This is why the reset events default to `Ignore`.
- **Upgrades:** the agent never upgrades plugins. Compatibility is negotiated
  through `describe`.

Goal changes from a plugin goal source are picked up by polling plus Node
events ([control and state, section 1](04-control-and-state.md#1-control-plane-integration)).

## 4. Hooks and plugins

```go
package hooks

type Event string

type Runner interface {
	Fire(ctx context.Context, e Event, payload any) error
}
```

```go
package plugin

type Client interface {
	Describe(ctx context.Context) (*Description, error)
	Call(ctx context.Context, method string, req, resp any) error
}
```

## 5. Mapping AKS Flex Node customizations

| Flex today | v2 mechanism |
|---|---|
| Own configuration schema, translated with `ToAgentConfig` | Flex's bootstrap script renders an `AgentConfiguration`; dynamic values come from providers |
| ARM Machine backend: create, get, ETag, status | `goal` and `status` plugin providers |
| Reset on a deleting taint plus a missing ARM Machine | `goal.get` sees the Node's taints and returns `delete: true` |
| Fresh bootstrap data before a repave | `credentials.bootstrap` provider, called before every provision |
| Kubelet exec credential, binary copied into the rootfs | `credentials.kubelet.type: exec`; the runtime makes the command visible inside the node |
| Daemon exec credential, host path rewritten | `credentials.daemon.type: plugin`; Flex's plugin implements `credentials.cluster` |
| CSR group `aks-flex-node-daemons` | `credentials.daemon.csr.group` |
| Static routes with `RequiredBy=systemd-nspawn@.service` | Host extension unit with `orderBeforeNode` |
| `additionalRequiredServices` and the udev-settle drop-in | `nodeDependencies.requires` |
| NPD written into the rootfs and started with `systemd-run` | Node extension: an artifact plus a unit with `start: AfterKubelet` |
| Arc and NPD preflight checks | `preflight.check`, plus automatic reachability checks for extension artifacts |
| `maxPods`, reserved resources, forced labels | `kubelet.configuration` and `kubelet.labels` |
| AKS LocalDNS template | `components.localDNS.corefileTemplate` plus the variable contract |
| `machineOperationMode: disable` | `control.operations.enabled: false` |
| Startup gate around its reconcilers | Built in ([control and state, section 5](04-control-and-state.md#5-daemon-reconcile)) |
| Own blue/green upgrade, signal file, recovery unit and unit names | Built in ([lifecycles, section 4](05-lifecycles.md#4-agentupgrade)) |
| Host-root move; `uninstall.sh` that hardcodes unbounded names | Not needed: `v2 reset` plus the documented list of what v2 creates ([host changes, section 1](06-host-changes.md#1-what-v2-creates-on-the-host)) |
