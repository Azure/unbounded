# Unbounded Agent v2: CLI and Configuration

Part of the [Unbounded Agent v2 design](README.md). The README has the status,
repository baseline, citation conventions and terms.

## 1. CLI

These are the commands of the v2 build, at the top level. The v1 build has only
v1's commands, and nothing in it reaches v2
([migrating from v1, section 2](10-migration-from-v1.md#2-release-plan-and-cutover)). In this design, a command name on its own, such as
`start`, means the v2 build's command, and v1's commands are always written as
v1's `start`.

| Command | Purpose |
|---|---|
| `start` | Bootstrap: install check, host preparation, provision, start the node, install the daemon |
| `preflight [--output json] [--ignore-errors]` | Read-only checks: built-in, runtime, extension and plugin checks |
| `daemon` | Long-running reconciler, run by `unbounded-agent-daemon.service` |
| `reset` | Remove the node, everything the runtime owns, extensions, host preparation and agent state |
| `status [--output json]` | New. Runtime, slots, applied goal version, node readiness, any migration and any cleanup leftovers |
| `migrate --rollback` | Put v1 back on a host that migrated, while the rollback window is open ([migrating from v1, section 8](10-migration-from-v1.md#8-rollback)). A v1 host migrates by installing this build, not with this command. |
| `migrate --adopt-machine --config <file> --previous-agent <file>` | Take over a machine another `pkg/agent` consumer built, without replacing its node, carrying out the handoff the previous-agent descriptor describes ([migrating from v1, section 9](10-migration-from-v1.md#9-hosts-built-by-other-pkgagent-consumers)) |
| `agent-upgrade [--preflight] [--distribution-manifest <url> --distribution-manifest-sha256 <hex>]` (hidden) | Activate a staged agent binary, and optionally a new distribution, without a MachineOperation, for automation under the `host` agent runtime. Under an image agent runtime it takes `--image oci://<repository> --digest <sha256>` instead ([agent runtime, section 7](12-agent-runtime.md#7-agentupgrade-by-image)). Running it with the v2 build on a v1 host migrates the host. ([lifecycles, section 5](05-lifecycles.md#5-agentupgrade)) |
| `boot-hook <pre-start\|post-start> <slot>`, `boot-hook localdns-network` (hidden) | Called by systemd around the node boot unit; runs the runtime's boot hook. Under an image agent runtime, also sets up LocalDNS's host network in place of the helper script the `host` agent runtime writes ([agent runtime, section 3](12-agent-runtime.md#3-running-on-the-host)). |
| `record-agent-upgrade-failure-signal` (hidden) | Used by the daemon recovery script |
| `recover-agent-upgrade` (hidden) | Under an image agent runtime, run by the recovery unit from the last-good image: selects that image again and restarts the daemon unit ([agent runtime, section 3](12-agent-runtime.md#3-running-on-the-host)) |
| `host-exec <tool> [args]` (hidden) | Under an image agent runtime, runs a host tool on the host, under `chroot /host`, and for mounts also in the host's mount namespace ([agent runtime, section 5](12-agent-runtime.md#5-reaching-the-host)) |
| `convert-config [--node-runtime <kind>]` (hidden) | Reads a v1 configuration on stdin and writes the v2 configuration it converts to ([v1 compatibility, section 5](08-v1-compatibility.md#5-converting-a-v1-configuration)), optionally for another node runtime. Used by operators preparing a configuration by hand for a host without a Machine. |
| `version [--output json]` | Prints the version, and which agent the binary is, as `agent: v2`. The v1 build prints `agent: v1`, and older v1 releases print no such line, which also means v1. The upgrade candidate check reads it ([lifecycles, section 5](05-lifecycles.md#5-agentupgrade)). |

During the experimental releases, every command of the v2 build logs this
warning as its first line of output, and `--help` says the same:

> This is the experimental v2 build of unbounded-agent: configuration, plugin
> protocol and on-host layout may change without notice; not supported for
> production use.

The first official v2 release drops the warning
([implementation, section 4](09-implementation.md#4-production-readiness)).

`status --output json` is a documented, versioned interface, so tests and
tools never read v2's state files. It reports:

- the install phase and install ID, the node runtime and the agent runtime, any migration in
  progress with its phase, and whether a migrated host's rollback window is
  still open;
- whether MachineOperations are handled, waiting for the CRD, or turned off;
- each slot's phase, whether it is enabled, and its bundle digest;
- the applied goal version and a digest of the applied goal settings;
- any approved repave in progress, with its attempt count and last error;
- the reported status phase and reason, and any cleanup leftovers.

Configuration is read from `--config` or `UNBOUNDED_AGENT_CONFIG_FILE`, the
variable cloud-init and Ignition already set
(`cmd/kubectl-unbounded/app/ignition.go:187`). v2 does not read configuration
from other environment variables.

## 2. Configuration

The configuration file is versioned. A file without `apiVersion` is treated as
v1 `UnboundedAgentConfig` JSON and converted ([v1 compatibility, section 5](08-v1-compatibility.md#5-converting-a-v1-configuration)), so existing machina
provisioning works unchanged.

```json
{
  "apiVersion": "agent.unbounded-cloud.io/v1alpha1",
  "kind": "AgentConfiguration",
  "machine": { "name": "m-01", "nodeName": "m-01", "provisioningFormat": "cloud-init" },
  "cluster": {
    "apiServer": "https://cluster.example:443",
    "caCertBase64": "...",
    "kubernetesVersion": "1.33.1",
    "clusterDNS": "10.0.0.10"
  },
  "kubelet": {
    "nodeIP": "",
    "labels": {},
    "registerWithTaints": [],
    "configuration": {},
    "imageCredentialProvider": null
  },
  "components": {
    "containerd": {},
    "runc": {},
    "cni": {},
    "gantry": {},
    "localDNS": { "corefileTemplate": "" }
  },
  "artifacts": { "downloads": {}, "offline": null },
  "nodeRuntime": {
    "type": "nspawn-container",
    "nspawnContainer": { "image": "", "additionalHostDevices": [], "additionalHostMounts": [] }
  },
  "agentRuntime": { "type": "host" },
  "credentials": {
    "bootstrap": { "type": "static", "static": { "token": "..." } },
    "kubelet": { "type": "bootstrap" },
    "daemon": { "type": "csr", "csr": { "group": "unbounded-agent-daemons" } }
  },
  "control": {
    "goal": { "type": "machina" },
    "status": { "type": "machina" },
    "operations": { "enabled": true }
  },
  "plugins": [
    {
      "name": "aks",
      "command": ["/opt/aks-flex-node/bin/aks-flex-node-plugin"],
      "sha256": "9f2c...e1",
      "permissions": {
        "paths": [
          { "path": "/etc/aks-flex-node/cluster.json", "access": "read" }
        ],
        "capabilities": [],
        "root": false
      },
      "credentials": [
        { "name": "sp-secret", "path": "/etc/aks-flex-node/credentials/sp-secret" }
      ],
      "env": { "HTTPS_PROXY": "http://proxy.example:3128" },
      "timeouts": { "goal.get": "60s" }
    }
  ],
  "hooks": [
    { "event": "node.repave.after", "plugin": "aks", "timeout": "30s", "failurePolicy": "Ignore" }
  ],
  "extensions": []
}
```

For the `host` node runtime, the `nodeRuntime` block looks like this instead:

```json
"nodeRuntime": {
  "type": "host",
  "host": {
    "stopGracePeriod": "30s",
    "killTimeout": "15s",
    "repave": { "retainContainerImages": false }
  }
}
```

For an image agent runtime ([agent runtime](12-agent-runtime.md)), the `agentRuntime` block names
the image the install starts from, by repository and digest, and for
`systemd-nspawn` also the root file system archive the first install unpacks:

```json
"agentRuntime": {
  "type": "systemd-nspawn",
  "image": "ghcr.io/example/aks-flex-node-agent@sha256:4b1e...07",
  "systemdNspawn": {
    "installArchive": { "url": "https://example.com/aks-flex-node-agent-rootfs.tar", "sha256": "7c0d...9a" }
  }
}
```

Enumerated fields:

| Field | Values |
|---|---|
| `nodeRuntime.type` | `host`, `nspawn-container`. Every provisioning path writes it explicitly from the Machine's `spec.agent.nodeRuntime` (below). It defaults to `nspawn-container` only for a file that does not set it, such as a v1 configuration from an older provisioner. |
| `agentRuntime.type` | `host`, `containerd`, `systemd-nspawn`. Every provisioning path writes it explicitly from the Machine's `spec.agent.agentRuntime` (below). It defaults to `host` only for a file that does not set it, such as a v1 configuration. `containerd` targets Azure Container Linux ([agent runtime, section 1](12-agent-runtime.md#1-agent-runtimes)). `containerd` and `systemd-nspawn` require `nodeRuntime.type: nspawn-container` ([agent runtime, section 1.1](12-agent-runtime.md#11-why-the-image-agent-runtimes-support-only-nspawn-container)). |
| `agentRuntime.image` | The install image: a fully qualified repository and `@sha256:` digest. Required for `containerd` and `systemd-nspawn`. |
| `agentRuntime.systemdNspawn.installArchive` | `{url, sha256}`: the root file system archive of the install image, an `https` URL and its lowercase hex SHA-256 ([agent runtime, section 8](12-agent-runtime.md#8-bootstrap-and-reset)). Required for `systemd-nspawn`. |
| `control.goal.type` | `machina`, `static` (the goal settings in the configuration file are the goal), `plugin` |
| `control.goal.pollInterval` | A duration. Default `10m`. Only for the `plugin` source ([control and state, section 1](04-control-and-state.md#1-control-plane-integration)). |
| `control.status.type` | `machina`, `none`, `plugin` |
| `control.operations.enabled` | `true` (default): handle MachineOperations through machina once the MachineOperation CRD exists, waiting without error while it does not ([control and state, section 6](04-control-and-state.md#6-machineoperation-handling)). `false`: ignore them. |
| `hooks[].failurePolicy` | `Fail`, `Ignore`. If omitted, each event's default ([extensions, section 1](02-extensions.md#1-extension-mechanisms)). |
| `plugins[].permissions.paths[].access` | `read`, `write` ([extensions, section 3](02-extensions.md#3-plugin-protocol)). Both control what the plugin's sandbox exposes under the `host` agent runtime, not file permissions: a plugin that is not `root` reads only what its user may read, such as world-readable files. `write` requires `root: true`. Secrets that only root may read reach a plugin through `credentials`. Under an image agent runtime there is no file system sandbox, so `paths` does not apply ([agent runtime, section 6](12-agent-runtime.md#6-plugins)). |
| `plugins[].permissions.capabilities` | Linux capability names, such as `CAP_NET_ADMIN`. Default none. |
| `plugins[].permissions.root` | `false` (default): the plugin runs as an unprivileged user of its own: a dynamic user, `unbounded-plugin-<name>`, under the `host` agent runtime, and a fixed UID under an image agent runtime. `true`: it runs as root, still limited to its declared capabilities. |
| `plugins[].credentials[]` | `{name, path}`: a file only root may read, such as a service principal secret, that the plugin reads from `$CREDENTIALS_DIRECTORY/<name>`. Under the `host` agent runtime it is passed with systemd's `LoadCredential=`; under an image agent runtime the daemon writes it into a private directory for the call. |

Credentials:

| Setting | Serves | Values |
|---|---|---|
| `credentials.bootstrap` | The token, API server and CA used for kubelet TLS bootstrapping and the daemon's first certificate request | `static` (from this file), `tpm` (metalman attestation), `plugin` (the `credentials.bootstrap` plugin method) |
| `credentials.kubelet` | How kubelet authenticates | `bootstrap` (TLS bootstrapping with the bootstrap token), `kubeconfig` (a supplied kubeconfig), `exec` (an exec credential plugin that kubelet runs itself) |
| `credentials.daemon` | How the daemon authenticates | `csr` (a CSR-issued client certificate, requested once with the bootstrap token), `kubeconfig`, `plugin` (the `credentials.cluster` plugin method) |

Kubelet's `exec` cannot use the plugin protocol: kubelet runs that executable
itself, through client-go's exec credential protocol. It is configured like
this:

```json
"kubelet": {
  "type": "exec",
  "exec": {
    "plugin": "aks",
    "args": ["token", "kubelogin", "--server-id", "6dae42f8-..."],
    "env": { "AAD_LOGIN_METHOD": "spn", "AAD_SERVICE_PRINCIPAL_CLIENT_ID": "..." },
    "secretEnv": [ { "name": "AAD_SERVICE_PRINCIPAL_CLIENT_SECRET", "credential": "sp-secret" } ]
  }
}
```

- `plugin` names a configured plugin whose executable kubelet runs. On
  `nspawn-container`, under every agent runtime, `Provision` copies it into the
  node at `/usr/local/libexec/unbounded/plugins/<name>`, and when an
  AgentUpgrade or a rollback changes the plugin, the daemon replaces the
  running node's copy when it starts, so the path in kubelet's kubeconfig never
  changes and kubelet runs the plugin of the running distribution
  ([agent runtime, section 6.1](12-agent-runtime.md#61-kubelets-exec-credential)). On the `host` node runtime, kubelet runs it from the
  plugin store. AKS Flex Node delivers its credential binary the same way
  today (`pkg/daemon/install.go:25-35`, `pkg/daemon/agent_upgrade.go:322,400`
  at Flex `2c149e991`). The plugin must be statically linked, because it runs
  with the node's libraries. `command`, an absolute host path, can be given
  instead of `plugin`, for an executable that is not a plugin; the runtime
  makes it visible in the node at the same path.
- `secretEnv` takes values from the plugin's `credentials`. They are written
  only into kubelet's kubeconfig, which the bundle marks as secret, as AKS Flex
  Node writes its service principal secret today
  (`pkg/config/adapter.go:132-134` at Flex `2c149e991`).
- A file the credential needs from the host, such as a client certificate, is
  made visible to the node with an extension's `sharedHostPaths`.
- kubelet runs the executable inside the node, outside the plugin sandbox, as
  root. It is pinned by the plugin store's checks, or by the image digest under
  an image agent runtime, not by the sandbox.

When the bootstrap credentials come from `tpm` or `plugin`, the non-empty
values they return override `cluster.apiServer` and `cluster.caCertBase64` for
that provision. They are never written back to the configuration or the
applied state. AKS Flex Node handles refreshed bootstrap data the same way.

`components.*` set component versions and settings. `artifacts.downloads` only
says where to download each artifact from, as a URL or base URL; it never sets
a version. A v1 download override's `Version` converts to the matching
`components` version ([v1 compatibility, section 5](08-v1-compatibility.md#5-converting-a-v1-configuration)).

Validation rules:

- `nodeRuntime.nspawnContainer` settings are rejected when `nodeRuntime.type`
  is `host`, and `nodeRuntime.host` settings are rejected for
  `nspawn-container`.
- For the `host` node runtime, `cgroupRoot` and `cgroupDriver` are reserved and
  rejected in `kubelet.configuration`, in addition to the fields v1 already
  reserves, including `authorization.mode`
  (`pkg/agent/config/config.go:604-623`).
- Extensions follow the rules in [extensions, section 2](02-extensions.md#2-declarative-extensions).
- An agent runtime is accepted only with a node runtime it supports:
  `containerd` and `systemd-nspawn` are rejected with `nodeRuntime.type: host`
  ([agent runtime, section 1.1](12-agent-runtime.md#11-why-the-image-agent-runtimes-support-only-nspawn-container)).
- Under the `host` agent runtime, every plugin has a `sha256`, the lowercase hex SHA-256
  of its executable. `command[0]` is where `start` reads the executable from at
  first install, and the rest of `command` are its arguments. Under an image
  agent runtime, the executable comes from the image's distribution manifest, which
  carries its SHA-256; `command[0]` names it there, and a `sha256` in the file,
  if given, must match the manifest. `env` holds literal values only and never
  secrets, which travel on stdin or through `credentials`
  ([extensions, section 3](02-extensions.md#3-plugin-protocol)). It cannot set `PATH` or any name
  starting with `LD_`.
- `plugins[].permissions.paths` cannot name anything under `/etc/unbounded`
  or `/var/lib/unbounded`, which hold the agent's credentials and state, and
  cannot grant `write` under `/opt/unbounded`, which holds the agent and plugin
  executables. `write` requires `root: true`.
- `plugins[].credentials[].path` is an absolute path to a regular file, outside
  `/etc/unbounded` and `/var/lib/unbounded`. Names are unique within a plugin.

The LocalDNS Corefile template variables become a documented contract:
`NodeListenerIP`, `ClusterListenerIP`, `NodeUpstreamIPs`,
`NodeUpstreamIPsJoined`, `ClusterDNSServiceIP` and `MetricsAddress`
(`pkg/agent/goalstates/localdns.go:101-108`). The default template uses all but
`NodeUpstreamIPs` (`pkg/agent/goalstates/assets/default-localdns.Corefile.tmpl:2-33`).
Flex's LocalDNS rendering depends on these names.

The extension template variables are a documented contract in the same way:
`NodeName`, `MachineName`, `APIServer`, `ClusterDNS` and
`KubeletKubeconfigPath` ([extensions, section 2](02-extensions.md#2-declarative-extensions)). New variables can be added; renaming
or removing one needs a new configuration `apiVersion`.

### Install-time settings and goal settings

The configuration has two kinds of settings:

| Kind | Fields | Where they come from | How they change |
|---|---|---|---|
| Install-time settings | `machine`, `nodeRuntime` (except `nodeRuntime.nspawnContainer.image`), `agentRuntime`, `credentials`, `control`, `plugins`, `hooks`, `extensions` | The configuration file only | Reset, then `start` with the new file. Running `start` with different install-time settings on an installed host fails the install check. |
| Goal settings (`config.GoalSettings`) | `cluster`, `kubelet`, `components`, `artifacts`, `nodeRuntime.nspawnContainer.image` | The configuration file at bootstrap, then the goal source | A goal with different goal settings; drift decides whether a repave is needed ([control and state, section 5](04-control-and-state.md#5-daemon-reconcile)) |

The install check compares the machine name and a fingerprint of the
install-time settings plus the three goal settings v1 fingerprints: the
Kubernetes version, the OCI image and the API server
(`cmd/agent/internal/cmd/bootstrap.go:65-78`). It then behaves as v1 does
(`cmd/agent/internal/installstate/store.go:216-224`):

- If the machine name or fingerprint differs from the install record's,
  `start` fails and asks for a reset.
- On a complete install, `start` verifies the node and the daemon, repairs
  the daemon if needed, and ignores any other differences in the file.
- On a resumed install, a node that has not started yet is provisioned from
  the file. A node that is already running is kept as it is. Bootstrap
  records the goal settings saved in its bundle manifest as applied, not the
  file's ([node runtime, section 2](03-node-runtime.md#2-node-spec-and-bundle)), so any other difference in the file, such
  as a changed label, is not applied. v1's e2e suite checks exactly this
  (`hack/agent/e2e-kind/e2e.py:3762-3803`).

A goal source returns goal settings only. A plugin's `goal.get` response that
contains any other field fails validation. In particular, a goal cannot change
the node runtime, the agent runtime, the plugins, the hooks or the extensions.

The distribution, the plugin executables and the extension definitions, is
the one part of the install-time settings with a second way to change: an
AgentUpgrade replaces it together with the agent
([extensions, section 2.1](02-extensions.md#21-distributions)). It cannot change which plugins and
extensions exist, or what they may do: plugin names, permissions,
credentials, environment, timeouts and hooks, and extensions' shared host
paths and cleanup declarations, stay as installed. The plugin `command[0]` and
`sha256` values and the extension definitions in the file are therefore used
only at first install. After it, the distribution stored beside the running
agent, or under an image agent runtime the one in its image, is what applies. The file's plugins and extensions are part of the
install check's fingerprint, so `start` with changed ones on an installed host
is refused like any other changed install-time setting, while `start` again
with the original file is not affected by later upgrades.

### Choosing the node runtime

A reinstall wipes the host, so the node runtime has to be stored in the
cluster, or a replaced host could come back with a different node runtime than
its node had.
Falling back to `nspawn-container` on a host built for `host` would run host
preparation that masks the host's kubelet and installs `systemd-container`
(`pkg/agent/phases/host/os.go:21-34`), which fails, or breaks the host, where
those packages are missing or cannot be installed.

The node runtime is therefore a field on the Machine:

- **`spec.agent.nodeRuntime`** takes `nspawn-container`, the default, or `host`,
  next to the existing `spec.agent.image` and `spec.agent.version`
  (`api/machina/v1alpha3/machine_types.go:558-583`). The CRD validates it.
- **Every provisioning path writes it into the agent configuration.**
  `provision.BuildAgentConfig` (`internal/provision/agent_config.go:141`)
  builds the configuration for machina's SSH provisioning
  (`cmd/machina/machina/controller/machine_controller.go:576`), HostReplace on
  Azure and OCI (`internal/machineops/bootstrap.go:122`), metalman's network
  installs (`internal/metalman/netboot/netboot.go:228`) and
  `kubectl unbounded machine manual-bootstrap`
  (`cmd/kubectl-unbounded/app/machine_manual_bootstrap.go:491`). It gains a
  `NodeRuntime` field, which converts to `nodeRuntime.type`
  ([v1 compatibility, section 5](08-v1-compatibility.md#5-converting-a-v1-configuration)).
- **Provisioning installs the build that can run it.** A `host` Machine gets
  the v2 build, because only v2 can run `host`. Until the first official v2
  release, an `nspawn-container` Machine gets the v1 build, and moves to v2 by
  being upgraded to the v2 build ([migrating from v1, section 4](10-migration-from-v1.md#4-migrating-early)).
  `manual-bootstrap` builds its Machine from flags
  (`machine_manual_bootstrap.go:474-486`), so it gains a `--node-runtime` flag.
- **A Machine the daemon creates records the node runtime.** When the daemon
  registers a Machine that does not exist yet, as v1 does for hosts installed
  outside machina (`cmd/agent/internal/daemon/daemon.go:307-350`), it sets
  `spec.agent.nodeRuntime` to the installed node runtime, so a later reinstall
  keeps it.
- **Old agent binaries are blocked.** v1 reads its configuration with plain
  `json.Unmarshal` (`cmd/agent/internal/cmd/config.go:94`), so a binary that
  does not know the field would ignore it and install `nspawn-container`.
  Provisioning refuses, with a clear reason, a `host` Machine whose
  `spec.agent.version` pins a release that has no v2 build, and the v1 build
  refuses a configuration that asks for `host`. A Machine can also name its
  agent by `spec.agent.url` or `spec.agent.baseURL`, which provisioning cannot
  check against a release (`api/machina/v1alpha3/machine_types.go:566-583`), so
  for a `host` Machine the provisioning script also runs the downloaded
  binary's `version` and stops, before `start`, unless it says `agent: v2`.
- **A change takes effect at the next reinstall.** The node runtime is an
  install-time setting, so editing the field never changes a running host.
  Until a HostReplace, or a reset followed by `start`, reinstalls the host,
  the Machine shows `ReinstallPending=True` with reason `NodeRuntimeChanged`, and
  `status.agent.nodeRuntime` shows the node runtime actually installed ([control and state, section 1](04-control-and-state.md#1-control-plane-integration)). Moving a
  fleet between node runtimes is: set the field, then replace each Machine.
- **Not on Azure Container Linux.** An ACL Machine stays `nspawn-container`.
  The `host` node runtime's capability probes fail on ACL, so a Machine set to
  `host` there fails bootstrap with `RuntimeUnsupported`
  ([node runtime, section 5.3](03-node-runtime.md#53-host-capabilities)).
- **The agent runtime follows the same rules.** `spec.agent.agentRuntime` is
  stored on the Machine, written into the configuration by every provisioning
  path as a new `AgentRuntime` field, recorded by a Machine the daemon
  registers, and applied at the next reinstall, with reason
  `AgentRuntimeChanged`. `manual-bootstrap` gains an `--agent-runtime` flag ([agent runtime, section 9](12-agent-runtime.md#9-choosing-the-agent-runtime)).
- **Upgrade order.** The CRD and every provisioning component (machina,
  machine-ops-controller, metalman and `kubectl unbounded`) must understand the
  node runtime and agent runtime fields before any Machine is set to the
  `host` node runtime or an image agent runtime. A component that does not
  would ignore them and install `nspawn-container` with the `host` agent
  runtime. The operator deploys these
  together, and the release notes state the order.
- **Whether the host can run them** is checked on the host by the node
  runtime's and the agent runtime's capability probes ([node runtime, section 5.3](03-node-runtime.md#53-host-capabilities),
  [agent runtime, section 10](12-agent-runtime.md#10-host-requirements)). A host that cannot fails bootstrap with
  `AgentBootstrapped=False` and reason `RuntimeUnsupported`, naming the probe
  that failed.

A configuration file written by hand, for a host without a Machine, sets
`nodeRuntime.type` and `agentRuntime.type` directly.
