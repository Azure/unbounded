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

Lifecycle hooks are a supported part of the first release, not a fallback to be
added later. They exist for side effects at fixed points that the other
mechanisms do not cover, such as sending lifecycle telemetry, collecting
diagnostics before a reset, registering the node with an external system after
it joins, or migrating a consumer's own files after an upgrade. No consumer
uses one yet: every AKS Flex Node customization maps to configuration, an
extension or a provider (section 5). They are in the first contract anyway, so
that a consumer with such a need never has to go back to linking `pkg/agent`,
which goal G3 rules out, or wait for a new configuration `apiVersion`. Most
hooks should use the `Ignore` failure policy, so a broken hook cannot block the
node.

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
| `agent.upgrade.before` | Before the new binary is staged, in the daemon or the `agent-upgrade` command | current version, operation name if any | `Fail` |
| `agent.upgrade.after` | In the daemon that publishes the upgrade result | old and new version, operation name if any | `Ignore` |
| `agent.reset.before` | Before reset begins | trigger (`CLI`, `Operation` or `Goal`) | `Ignore` |
| `agent.reset.after` | After reset, before the agent directory, with its plugins, and the daemon unit are removed | trigger, leftovers | `Ignore` |

The order of events in each lifecycle:

| Lifecycle | Events, in order |
|---|---|
| Bootstrap ([lifecycles, section 1](05-lifecycles.md#1-bootstrap-start)) | `agent.install.before`, `host.prepare.before`, `host.prepare.after`, `node.provision.before`, `node.provision.after`, `node.start.before`, `node.start.after`, `agent.install.after` |
| Repave ([lifecycles, section 2](05-lifecycles.md#2-repave)) | `node.repave.before`, `node.provision.before` (new), `node.provision.after` (new), `node.stop.before` (old), `node.stop.after` (old), `node.start.before` (new), `node.start.after` (new), `node.repave.after` |
| NodeReboot ([lifecycles, section 3](05-lifecycles.md#3-nodereboot)) | `node.restart.before`, `node.restart.after` |
| HostOSReboot ([lifecycles, section 4](05-lifecycles.md#4-hostosreboot)) | None: the host reboots, and systemd starts the node at boot |
| AgentUpgrade ([lifecycles, section 5](05-lifecycles.md#5-agentupgrade)) | `agent.upgrade.before` in the old daemon or the CLI, `agent.upgrade.after` in the new daemon |
| Reset ([lifecycles, section 6](05-lifecycles.md#6-reset)) | `agent.reset.before`, `node.stop.before` and `node.stop.after` if a node is running, `agent.reset.after` |

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
    "units": [
      {
        "name": "node-problem-detector.service",
        "start": "AfterKubelet",
        "template": true,
        "content": "[Service]\nExecStart=/usr/local/bin/node-problem-detector --hostname-override={{.NodeName}} --apiserver-override=\"{{.APIServer}}?inClusterConfig=false&auth={{.KubeletKubeconfigPath}}\" --config.system-log-monitor=/etc/npd/kernel-monitor.json\n"
      }
    ]
  },
  "host": {
    "units": [ { "name": "static-routes.service", "content": "...", "orderBeforeNode": true } ],
    "files": [ { "path": "/etc/aks-flex-node/static-routes.sh", "mode": "0755", "content": "..." } ]
  },
  "sharedHostPaths": [ { "hostPath": "/opt/aks-flex-node/bin", "nodePath": "/opt/aks-flex-node/bin", "readOnly": true } ],
  "nodeDependencies": { "requires": ["systemd-udev-settle.service", "himdsd.service"] },
  "agentDependencies": { "after": ["himdsd.service"] },
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
- A node file or unit with `"template": true` is rendered as a Go
  `text/template` when the slot's bundle is rendered, with only these
  variables, a contract like the LocalDNS template variables
  ([CLI and configuration, section 2](01-cli-and-configuration.md#2-configuration)):

  | Variable | Value |
  |---|---|
  | `NodeName` | The Kubernetes Node name, `machine.nodeName` |
  | `MachineName` | The Machine's name, `machine.name` |
  | `APIServer` | The API server URL this provision uses: `cluster.apiServer`, unless the `credentials.bootstrap` provider replaced it |
  | `ClusterDNS` | The cluster DNS service IP, `cluster.clusterDNS` |
  | `KubeletKubeconfigPath` | kubelet's kubeconfig, as a path inside the node, which differs between runtimes |

  - None of the variables is a secret, so a rendered file never holds one.
  - Only `text/template`'s built-in functions are available. Without the
    flag, content is used exactly as written, and inside a template a literal
    `{{` is written `{{"{{"}}`.
  - The rendered content is saved in the slot's manifest and counted in its
    digest. Restarts and reboots do not render it again; a changed value
    reaches the node at its next provision.
  - Templates apply to node content only. Host files and units are installed
    at bootstrap, and again only at a provision after the distribution
    changed (section 2.1), not at every goal change, so a templated value that
    a goal changed would leave them stale.
  - When the configuration loads, in `start`, `preflight` and the daemon, each
    template is rendered with placeholder values, so a misspelled variable
    fails with the extension, the file or unit, and the name, before anything
    is installed.
- `agentDependencies.after` lists host units the agent's own daemon should
  start after, such as an identity service its credentials depend on. v2
  writes a drop-in for `unbounded-agent-daemon.service` with `After=` and
  `Wants=` on each. It never uses `Requires=`: a broken or stopped dependency
  must not stop the agent, which may be needed to reset the host. `preflight`
  warns about a listed unit that does not exist.
- An extension's node files and units may not use a path or unit name that the
  agent renders itself or that a runtime owns, such as
  `/etc/containerd/config.toml` or `kubelet.service`. Validation rejects them.
- An extension host file or unit that already exists on the host, and that the
  agent did not create, fails preflight. The agent never overwrites host files
  it does not own.
- `cleanup` declares state the extension's workloads create ([host runtime cleanup, section 13](07-host-runtime-cleanup.md#13-extension-cleanup-declarations)).
- Everything an extension creates is recorded in state and removed by reset.
- Which extensions exist is an install-time setting: adding or removing one
  needs a reset and a new install ([CLI and configuration, section 2](01-cli-and-configuration.md#2-configuration)). Their
  content can change with the agent, as part of the distribution (section 2.1).

This replaces the hardcoded `systemd-nspawn@` dependencies in v1's assets and
in Flex, such as Flex's static routes with
`RequiredBy=systemd-nspawn@.service`.

### 2.1 Distributions

A consumer's *distribution* is its plugin executables and its extension
definitions. They are versioned together, and they change only with the agent,
so a consumer release can ship a new Node Problem Detector, a fixed static
route script or a new plugin without a reinstall. AKS Flex Node can do this
today, because it downloads Node Problem Detector at every node start
(`pkg/daemon/start.go:53-55` at Flex `2c149e991`); without distributions, v2
would take that away.

```json
{
  "apiVersion": "agent.unbounded-cloud.io/v1alpha1",
  "kind": "DistributionManifest",
  "plugins": [
    { "name": "aks", "url": "https://example.com/aks-flex-node-plugin", "sha256": "4b1e...07" }
  ],
  "extensions": [ { "name": "node-problem-detector", "artifacts": [], "node": {}, "host": {} } ]
}
```

- At first install, the distribution is the configuration file's plugins and
  extensions, or, under an image agent runtime, the image's manifest at
  `/usr/share/unbounded-agent/distribution.json`, where `url` is a path in the
  image ([agent runtime, section 2](12-agent-runtime.md#2-images)).
- An AgentUpgrade can carry a new one: under the `host` agent runtime as a manifest
  named by the `distributionManifestURL` and `distributionManifestSHA256`
  parameters, under an image agent runtime inside the new image. Without one, the
  running distribution is kept ([lifecycles, section 5](05-lifecycles.md#5-agentupgrade)).
- A new distribution must name exactly the plugins and extensions that are
  installed, and must not widen what they may do. It may change plugin
  executables and extension artifacts, files, units and templates. It may not
  change plugin permissions, credentials, environment or timeouts, hooks, or
  an extension's `sharedHostPaths`, `agentDependencies` or `cleanup`. A
  manifest that does is rejected before the switch.
- The distribution is staged beside the new agent and switched with it, so a
  rollback restores both.
- Extension content takes effect at the next provision, host and node content
  alike. An AgentUpgrade therefore changes nothing on the host but the agent
  and its plugin store, and a rollback has nothing else to undo.

## 3. Plugin protocol

A plugin is a compiled executable owned by the consumer. The agent runs it
with its configured arguments and the method name, a JSON request on stdin, and
reads a JSON response from stdout. A non-zero exit or an `error` object means
failure. stderr goes to the agent log, size-bounded. Secrets travel only on
stdin or as declared credentials, never in arguments or the environment.

```json
{
  "apiVersion": "plugin.agent.unbounded-cloud.io/v1alpha1",
  "kind": "Request",
  "method": "goal.get",
  "context": { "machineName": "m-01", "nodeName": "m-01", "nodeRuntime": "host", "agentRuntime": "host", "agentVersion": "..." },
  "payload": {
    "node": { "exists": true, "uid": "...", "labels": {}, "taints": [] },
    "applied": { "goalVersion": "..." }
  }
}
```

| Method | Request payload | Response payload |
|---|---|---|
| `describe` | None | `{protocolVersions[], methods[]}` |
| `goal.register` | `{machineName, nodeName, nodeRuntime, agentRuntime, agentVersion, kubernetesVersion, provisioningFormat}` | None |
| `goal.get` | Node observation, applied state | `{version, settings, delete}`. `settings` holds goal settings only ([CLI and configuration, section 2](01-cli-and-configuration.md#2-configuration)). `delete` is `true` when the machine is being removed and the node must be reset. |
| `status.report` | `{phase, reason, message, appliedVersion, nodeRuntime, agentRuntime, attempts, leftovers}` | None |
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
the agent has no handler for is ignored, as in v1 ([control and state, section 6](04-control-and-state.md#6-machineoperation-handling)); it
does not change the node's phase. The built-in `machina` status reporter maps
these phases onto the Machine's conditions rather than its phase
([control and state, section 1](04-control-and-state.md#1-control-plane-integration)).

Operational rules:

- **`describe`** is called when `start`, `daemon`, `preflight` or
  `reset` starts. If a plugin configured as a provider does not list a
  method the configuration uses, startup fails, except for `reset` (below).
  For the daemon this happens, for all plugins in parallel, before it reports ready
  ([control and state, section 5](04-control-and-state.md#5-daemon-reconcile)), so a plugin set that does not fit a newly
  upgraded binary keeps the daemon from reporting ready, and the recovery unit
  rolls both back ([lifecycles, section 5](05-lifecycles.md#5-agentupgrade)).
- **Timeouts** are set per method in `plugins[].timeouts`. The defaults are 10
  seconds for `describe` and 60 seconds for every other method. A hook's
  timeout is set only by its `hooks[].timeout`, which also defaults to 60
  seconds. On a timeout or cancellation the plugin gets SIGTERM, then SIGKILL
  after 5 seconds.
- **Output limits:** a response larger than 1 MiB on stdout is a failure. Only
  the first 64 KiB of stderr is logged.
- **What a plugin sees is the same under every agent runtime:** the request on
  stdin and the response on stdout, a clean environment, `STATE_DIRECTORY` and
  `CACHE_DIRECTORY`, which persist across calls and upgrades, and
  `CREDENTIALS_DIRECTORY`, which holds its declared credentials. How each call
  runs, and how far it is sandboxed, depends on the agent runtime: below for
  the `host` agent runtime, and in [agent runtime, section 6](12-agent-runtime.md#6-plugins) for the image agent runtimes, where each
  call is a child process inside the agent's container, run from the image,
  with no file system sandbox.
- **Under the `host` agent runtime, one transient unit per call.** The agent starts each
  call with `systemd-run --pipe --wait --collect` as
  `unbounded-agent-plugin-<name>-<id>.service`, which runs the plugin
  executable from the plugin store directly. A call costs 50 to 70 ms on the
  host matrix's systemd versions (validation task V8,
  [testing, section 7](11-testing.md#7-validation-tasks)).
- **Pinned executables.** Under the `host` agent runtime the agent runs only an
  executable whose SHA-256 matches the distribution: the configuration's
  `sha256` at first install, or the distribution manifest after an upgrade. It
  checks the digest when it stages the executable into the store, and again
  before each call. The store and every parent directory are owned by root and
  not writable by group or others, so replacing an executable between the
  check and the run already needs root on the host. The executable must be a
  statically linked ELF file. A dynamically linked one would load libraries
  from the host that nothing pins, and a `#!` script would run an interpreter
  that nothing pins. `preflight` and `start` refuse either. A Go plugin built
  with `CGO_ENABLED=0` is static, as this repository's own release builds are
  (`.goreleaser.yml:34-35`). Under an image agent runtime the image digest pins the
  plugins, and the image's libraries are pinned with them. A plugin used as
  kubelet's exec credential runs in the node, so it must be statically linked
  under every agent runtime ([agent runtime, section 6.1](12-agent-runtime.md#61-kubelets-exec-credential)).
- **The plugin store**, under the `host` agent runtime only, is
  `/opt/unbounded/agent/plugins/`. It holds the plugin set beside each of the
  `blue` and `green` binaries, each with the manifest of its plugins' names and
  digests. It must not be under a path that plugins cannot see: a unit cannot
  run an executable inside `InaccessiblePaths=` (V8).
- **Under the `host` agent runtime, sandboxed and unprivileged by default.** Each call's
  unit applies these settings. A plugin asks for more only through
  `plugins[].permissions`, which is an install-time setting
  ([CLI and configuration, section 2](01-cli-and-configuration.md#2-configuration)):

  | | Default | Declared with |
  |---|---|---|
  | User | A dynamic user of the plugin's own, `DynamicUser=yes` with `User=unbounded-plugin-<name>`, so it keeps the same UID across calls | `root: true` runs it as root, with every other setting unchanged |
  | Capabilities | None: an empty `CapabilityBoundingSet=` and `NoNewPrivileges=yes` | `capabilities`, granted as bounding and ambient capabilities |
  | File system | The OS read-only (`ProtectSystem=strict`), no home directories (`ProtectHome=yes`), a private `/tmp` and no devices (`PrivateDevices=yes`). The agent's and the node's private paths are inaccessible: `/etc/unbounded`, `/var/lib/unbounded`, `/etc/kubernetes`, `/var/lib/kubelet` and `/var/lib/machines`. | `paths`: `read` makes a path readable that would otherwise be inaccessible, and, with `root: true`, `write` makes it writable. Neither changes file permissions: a dynamic user can write only its own directories, and read only what any user may read (V8). |
  | Its own state | A state directory and a cache directory that persist across calls and upgrades, `StateDirectory=` and `CacheDirectory=` named `unbounded-agent-plugins/<name>`, for token caches and the like | Always present |
  | Secrets | None | `credentials`: each file is passed with `LoadCredential=` and read from `$CREDENTIALS_DIRECTORY`, which works for a dynamic user although the file is root's alone (V8) |
  | Other processes | Hidden (`ProtectProc=invisible`) | Not configurable |
  | Network | Allowed, because providers call cloud and cluster APIs | Not configurable |
  | Run time | `RuntimeMaxSec=` set to the call's timeout, and `TimeoutStopSec=5s`. Every process the call started ends with the unit, including one that daemonized (V8). | `plugins[].timeouts` and `hooks[].timeout` |

  An identity that only root can use, such as Azure Arc's managed identity,
  whose challenge key under `/var/opt/azcmagent/tokens` only root may read,
  needs `root: true` under every agent runtime. Under an image agent runtime a plugin runs
  as an unprivileged UID of its own with only its declared capabilities,
  unless it declares `root: true`, and `permissions.paths` does not apply
  ([agent runtime, section 6](12-agent-runtime.md#6-plugins)).

- **A clean environment.** The plugin's environment holds only `PATH`, set to
  `/usr/sbin:/usr/bin:/sbin:/bin`, `LANG=C.UTF-8`, `STATE_DIRECTORY`,
  `CACHE_DIRECTORY` and `CREDENTIALS_DIRECTORY`, the other variables systemd
  sets for the unit under the `host` agent runtime, and the literal values in
  `plugins[].env`, such as proxy settings, which cannot set `PATH` or any `LD_`
  name. Nothing from the agent's own environment passes through (V8).
- **An audit log.** Every call writes one line to the agent's log: the plugin,
  the method, the SHA-256 or image digest that was run, the unit name under
  the `host` agent runtime, the user, the granted capabilities and writable paths, the
  duration, the exit status and the response size. It never includes the request or the response, which can hold
  secrets.
- **Reset never fails because of a plugin.** During `reset` and other
  resets, `describe` failures, provider failures and hook failures are logged
  and reset continues. This is why the reset events default to `Ignore`.
- **Plugins change only with the agent.** Under the `host` agent runtime, `start` fills
  the running set at first install, and after that only an AgentUpgrade changes
  it, by staging a new distribution beside the new agent or copying the
  running one there (section 2.1). Under an image agent runtime the plugins are in the
  image, so they change exactly when the image does. Switching to the new agent, or recovery restoring the old one,
  therefore always switches agent and plugins together
  ([lifecycles, section 5](05-lifecycles.md#5-agentupgrade)). A plugin's name, permissions, credentials,
  environment, timeouts and hooks stay install-time settings, so an upgrade
  replaces a plugin's code but never widens what it may do.

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
| ARM Machine backend: create, get, ETag, status | `goal` and `status` plugin providers. `goal.register` pushes the local goal as Flex's `EnsureMachine` does, and the daemon records the Machine's ETag as applied on its first check whose settings match ([control and state, section 1](04-control-and-state.md#1-control-plane-integration)). |
| Reset on a deleting taint plus a missing ARM Machine | `goal.get` sees the Node's taints and returns `delete: true` |
| Repave when the Node is missing and the goal is not applied, even after a daemon restart | Built in: a missing Node, or one with a new UID, counts as deleted ([control and state, section 5](04-control-and-state.md#5-daemon-reconcile)) |
| Fresh bootstrap data before a repave | `credentials.bootstrap` provider, called before every provision |
| Kubelet exec credential, binary copied into the rootfs | `credentials.kubelet.type: exec` with `exec.plugin: aks`: the agent copies the plugin into the node and refreshes the running node's copy after an upgrade, as Flex does, and the service principal secret is a `secretEnv` from the plugin's `credentials` ([CLI and configuration, section 2](01-cli-and-configuration.md#2-configuration)) |
| Daemon exec credential, host path rewritten | `credentials.daemon.type: plugin`; Flex's plugin implements `credentials.cluster` |
| ARM access with Arc's managed identity, whose challenge key only root may read | A plugin with `root: true` and no capabilities (section 3) |
| CSR group `aks-flex-node-daemons` | `credentials.daemon.csr.group` |
| Static routes with `RequiredBy=systemd-nspawn@.service` | Host extension unit with `orderBeforeNode` |
| Daemon unit ordered after `himdsd.service` when Arc is enabled | `agentDependencies.after` |
| Host preparation steps copied from unbounded (`pkg/daemon/start.go:16-28`), which leave out unbounded's Docker `iptables: false` step and the masking of the host's containerd and kubelet (Azure/AKSFlexNode#423) | Built-in host preparation, the same for every consumer. It masks the host's kubelet but leaves the host's containerd running, because the port checks already catch a real conflict and, on ACL, the host's containerd holds ACL's Arc agent image and runs the `containerd` agent runtime's agent ([host changes, section 3](06-host-changes.md#3-host-preparation-and-its-reversal)). An adopted host gains the missing steps, and adoption warns which host services will be masked ([migrating from v1, section 9](10-migration-from-v1.md#9-hosts-built-by-other-pkgagent-consumers)). |
| `additionalRequiredServices` and the udev-settle drop-in | `nodeDependencies.requires` |
| NPD written into the rootfs and started with `systemd-run`, its unit rendered with the node name, API server and kubeconfig, at the version Flex's release names | Node extension: an artifact plus a templated unit with `start: AfterKubelet`, using `NodeName`, `APIServer` and `KubeletKubeconfigPath`. A Flex release changes it through its distribution (section 2.1). |
| Arc and NPD preflight checks | `preflight.check`, plus automatic reachability checks for extension artifacts |
| `maxPods`, reserved resources, forced labels | `kubelet.configuration` and `kubelet.labels` |
| AKS LocalDNS template | `components.localDNS.corefileTemplate` plus the variable contract |
| `machineOperationMode`: `auto`, the default, which checks for the MachineOperation CRD at start, and `disable` | `control.operations.enabled`: `true`, which handles operations once the CRD exists, and `false` ([control and state, section 6](04-control-and-state.md#6-machineoperation-handling)) |
| ARM-backed MachineOperations, planned (`docs/design/agent-and-aks.md:16`) | Not yet: operations come only from the MachineOperation CRD ([overview, section 3](README.md#3-open-questions)) |
| Startup gate around its reconcilers | Built in ([control and state, section 5](04-control-and-state.md#5-daemon-reconcile)) |
| Own blue/green upgrade, signal file, recovery unit and unit names | Built in ([lifecycles, section 5](05-lifecycles.md#5-agentupgrade)) |
| The container install on Azure Container Linux (Azure/AKSFlexNode#370): the agent image run by the host's containerd, `host-exec`, upgrade by `oci://` image | The `containerd` agent runtime, built in, with Flex's image built from the agent image ([agent runtime](12-agent-runtime.md)) |
| Host-root move; `uninstall.sh` that hardcodes unbounded names | Not needed: `reset` plus the documented list of what v2 creates ([host changes, section 1](06-host-changes.md#1-what-v2-creates-on-the-host)) |
