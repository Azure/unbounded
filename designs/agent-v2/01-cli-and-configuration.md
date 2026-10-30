# Unbounded Agent v2: CLI and Configuration

Part of the [Unbounded Agent v2 design](README.md). The README has the status,
repository baseline, citation conventions and terms.

## 1. CLI

| Command | Purpose |
|---|---|
| `v2 start` | Bootstrap: install check, host preparation, provision, start the node, install the daemon |
| `v2 preflight [--output json] [--ignore-errors]` | Read-only checks: built-in, runtime, extension and plugin checks |
| `v2 daemon` | Long-running reconciler, run by `unbounded-agent-v2-daemon.service` |
| `v2 reset [--force-standard-paths]` | Remove the node, everything the runtime owns, extensions, host preparation and agent state |
| `v2 status [--output json]` | New. Runtime, slots, applied goal version, node readiness and any cleanup leftovers |
| `v2 agent-upgrade [--preflight]` | Activate a staged agent binary without a MachineOperation, for host delivery automation ([lifecycles, section 4](05-lifecycles.md#4-agentupgrade)) |
| `v2 boot-hook <pre-start\|post-start> <slot>` (hidden) | Called by systemd around the node boot unit; runs the runtime's boot hook |
| `v2 record-agent-upgrade-failure-signal` (hidden) | Used by the daemon recovery script |
| `v2 version` | Prints the version; also used to verify an upgrade candidate supports v2 |

The `v2` group is listed in `unbounded-agent --help` as "Experimental v2 agent
(unstable)". Every `v2` command logs this warning as its first line of output:

> unbounded-agent v2 is experimental: configuration, plugin protocol and
> on-host layout may change without notice; not supported for production use.

Configuration is read from `--config` or `UNBOUNDED_AGENT_CONFIG_FILE`, the
variable cloud-init and Ignition already set
(`cmd/kubectl-unbounded/app/ignition.go:187`). v2 does not read configuration
from other environment variables.

`--force-standard-paths` only has an effect when the host runtime's ownership
record is missing ([host runtime cleanup, section 9](07-host-runtime-cleanup.md#9-teardown)). MachineOperations and goal-driven resets
never set it.

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
  "runtime": {
    "type": "nspawn-container",
    "nspawnContainer": { "image": "", "additionalHostDevices": [], "additionalHostMounts": [] }
  },
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
      "timeouts": { "goal.get": "60s" }
    }
  ],
  "hooks": [
    { "event": "node.start.after", "plugin": "aks", "timeout": "2m", "failurePolicy": "Fail" },
    { "event": "agent.upgrade.*", "plugin": "aks", "timeout": "30s" }
  ],
  "extensions": []
}
```

For the `host` runtime, the `runtime` block looks like this instead:

```json
"runtime": {
  "type": "host",
  "host": {
    "stopGracePeriod": "30s",
    "killTimeout": "15s",
    "repave": { "retainContainerImages": false },
    "adoptExistingPaths": [ { "path": "/opt/cni/bin", "policy": "Discard" } ],
    "adoptPreserveMaxBytes": 1073741824
  }
}
```

Enumerated fields:

| Field | Values |
|---|---|
| `runtime.type` | `host`, `nspawn-container`. Defaults to `nspawn-container` *(open: [overview, section 3](README.md#3-open-questions), question 3)*. |
| `control.goal.type` | `machina`, `static` (the goal settings in the configuration file are the goal), `plugin` |
| `control.goal.pollInterval` | A duration. Default `10m`. Only for the `plugin` source ([control and state, section 1](04-control-and-state.md#1-control-plane-integration)). |
| `control.status.type` | `machina`, `none`, `plugin` |
| `control.operations.enabled` | `true` (default): handle MachineOperations through machina. `false`: ignore them. |
| `hooks[].failurePolicy` | `Fail`, `Ignore`. If omitted, each event's default ([extensions, section 1](02-extensions.md#1-extension-mechanisms)). |
| `runtime.host.adoptExistingPaths[].policy` | `Preserve`, `Discard` ([host runtime cleanup, section 3](07-host-runtime-cleanup.md#3-ownership-and-adoption)). Required. |
| `runtime.host.adoptPreserveMaxBytes` | The largest `Preserve` backup allowed for any one path. Default 1 GiB. |

Credentials:

| Setting | Serves | Values |
|---|---|---|
| `credentials.bootstrap` | The token, API server and CA used for kubelet TLS bootstrapping and the daemon's first certificate request | `static` (from this file), `tpm` (metalman attestation), `plugin` (the `credentials.bootstrap` plugin method) |
| `credentials.kubelet` | How kubelet authenticates | `bootstrap` (TLS bootstrapping with the bootstrap token), `kubeconfig` (a supplied kubeconfig), `exec` (an exec credential plugin that kubelet runs itself) |
| `credentials.daemon` | How the daemon authenticates | `csr` (a CSR-issued client certificate, requested once with the bootstrap token), `kubeconfig`, `plugin` (the `credentials.cluster` plugin method) |

Kubelet's `exec` cannot use the plugin protocol: kubelet runs that executable
itself, through client-go's exec credential protocol. Its command must be an
absolute host path, and the runtime makes it visible inside the node at the
same path ([node runtime, section 5](03-node-runtime.md#5-runtimes)).

When the bootstrap credentials come from `tpm` or `plugin`, the non-empty
values they return override `cluster.apiServer` and `cluster.caCertBase64` for
that provision. They are never written back to the configuration or the
applied state. AKS Flex Node handles refreshed bootstrap data the same way.

`components.*` set component versions and settings. `artifacts.downloads` only
says where to download each artifact from, as a URL or base URL; it never sets
a version. A v1 download override's `Version` converts to the matching
`components` version ([v1 compatibility, section 5](08-v1-compatibility.md#5-converting-a-v1-configuration)).

Validation rules:

- `runtime.nspawnContainer` settings are rejected when `runtime.type` is
  `host`, and `runtime.host` settings are rejected for `nspawn-container`.
- `runtime.host.adoptExistingPaths[].path` must be a built-in owned path
  ([host runtime cleanup, section 4](07-host-runtime-cleanup.md#4-owned-resources)) or an extension-declared state path.
- For the `host` runtime, `cgroupRoot` and `cgroupDriver` are reserved and
  rejected in `kubelet.configuration`, in addition to the fields v1 already
  reserves, including `authorization.mode`
  (`pkg/agent/config/config.go:604-623`).
- Extensions follow the rules in [extensions, section 2](02-extensions.md#2-declarative-extensions).

The LocalDNS Corefile template variables become a documented contract:
`NodeListenerIP`, `ClusterListenerIP`, `NodeUpstreamIPsJoined`,
`ClusterDNSServiceIP` and `MetricsAddress`
(`pkg/agent/goalstates/assets/default-localdns.Corefile.tmpl:2-33`). Flex's
LocalDNS rendering depends on these names.

### Install-time settings and goal settings

The configuration has two kinds of settings:

| Kind | Fields | Where they come from | How they change |
|---|---|---|---|
| Install-time settings | `machine`, `runtime` (except `runtime.nspawnContainer.image`), `credentials`, `control`, `plugins`, `hooks`, `extensions` | The configuration file only | Reset, then `v2 start` with the new file. Running `v2 start` with different install-time settings on an installed host fails the install check. |
| Goal settings (`config.GoalSettings`) | `cluster`, `kubelet`, `components`, `artifacts`, `runtime.nspawnContainer.image` | The configuration file at bootstrap, then the goal source | A goal with different goal settings; drift decides whether a repave is needed ([control and state, section 5](04-control-and-state.md#5-daemon-reconcile)) |

The install check compares the machine name and a fingerprint of the
install-time settings plus the three goal settings v1 fingerprints: the
Kubernetes version, the OCI image and the API server
(`cmd/agent/internal/cmd/bootstrap.go:65-78`). It then behaves as v1 does
(`cmd/agent/internal/installstate/store.go:216-224`):

- If the machine name or fingerprint differs from the install record's,
  `v2 start` fails and asks for a reset.
- On a complete install, `v2 start` verifies the node and the daemon, repairs
  the daemon if needed, and ignores any other differences in the file.
- On a resumed install, a node that has not started yet is provisioned from
  the file. A node that is already running is kept as it is. Bootstrap
  records the goal settings saved in its bundle manifest as applied, not the
  file's ([node runtime, section 2](03-node-runtime.md#2-node-spec-and-bundle)), so any other difference in the file, such
  as a changed label, is not applied. v1's e2e suite checks exactly this
  (`hack/agent/e2e-kind/e2e.py:3762-3803`).

A goal source returns goal settings only. A plugin's `goal.get` response that
contains any other field fails validation. In particular, a goal cannot change
the runtime, the plugins, the hooks or the extensions. Removing or changing an
extension needs a reset and a new install.

A v1 configuration converts to the `nspawn-container` runtime. Machina
generates v1 configurations and machina API changes are a non-goal, so a
machina-provisioned host always gets `nspawn-container`. Using the `host`
runtime needs a configuration file written by hand, for example through
`kubectl unbounded` manual bootstrap or a custom first-boot configuration,
until machina adds a way to choose the runtime.
