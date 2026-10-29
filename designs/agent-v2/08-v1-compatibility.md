# Unbounded Agent v2: v1 Compatibility

Part of the [Unbounded Agent v2 design](README.md). The README has the status,
repository baseline, citation conventions and terms.

## 1. Parity with v1

| v1 capability | v1 location | v2 location |
|---|---|---|
| Install check, locks and fingerprint | `cmd/agent/internal/installstate`, `cmd/agent/internal/bootstrap` | `state`, `bootstrap` |
| Preflight, including resume awareness | `cmd/agent/internal/cmd/preflight.go`, `pkg/agent/phases/*/preflight*` | `preflight`, plus runtime and plugin checks |
| Configuration file | `cmd/agent/internal/cmd/config.go` | `config`, v1alpha1 plus v1 JSON conversion |
| Host preparation | `pkg/agent/phases/host` | `hostprep`, now with an undo log |
| Artifacts, offline sources, overrides, image archives | `pkg/agent/phases/rootfs/{kube,cri,cni,container_images}.go`, `pkg/agent/artifactsource`, `pkg/agent/bootstrapartifacts` | `artifacts` |
| containerd, kubelet and LocalDNS configuration | `pkg/agent/phases/nodestart` | `bundle` |
| Kubelet authentication modes | `pkg/agent/phases/nodestart/kubelet.go:282-379` | `credentials` and `bundle` |
| TPM attestation | `cmd/agent/internal/attest` | `credentials`, the `tpm` provider |
| Machine registration and status | `cmd/agent/internal/daemon/daemon.go:303-406`, `bootstrap_status.go` | `control`, the `machina` providers |
| NVIDIA, AMD and host devices | `pkg/agent/goalstates`, `pkg/agent/phases/nodestart/nvidia.go` | Discovery in `nodespec`; each runtime makes the devices available to the node |
| nspawn lifecycle hooks | `pkg/agent/nspawnlifecycle` | `runtime.BootHook` and `v2 boot-hook` |
| Daemon CSR credentials | `pkg/agent/daemoncred` | `credentials`, the `csr` provider |
| Repave on Node delete with drift detection | `cmd/agent/internal/daemon/controller_node.go`, `nodeoperator.go` | `daemon`, `nodemanager` |
| NodeReboot, AgentUpgrade, AgentReset | `cmd/agent/internal/daemon/controller_machineoperation.go` | `daemon`, `agentupgrade`, `nodemanager` |
| Daemon recovery unit | `cmd/agent/internal/daemon/assets` | `daemon` |
| First-boot unit removal on reset | `cmd/agent/internal/daemon/reset.go:191` | Reset |
| Host-root move from `/usr/local` | `internal/hostroot` | Not carried over: v2 only installs fresh |
| Configuration from legacy environment variables | `cmd/agent/internal/cmd/config.go` | Dropped |

## 2. Behavior changes from v1

Where v2 intentionally behaves differently from v1. Everything not listed
here should match v1.

| Area | v1 | v2 | Section |
|---|---|---|---|
| Noticing a deleted Node | Reacts only to delete events, so it misses one deleted while the daemon is down | Compares the observed Node on every reconcile, and records its UID | [control and state, section 5](04-control-and-state.md#5-daemon-reconcile) (open: [overview, section 3](README.md#3-open-questions), question 6) |
| A Node deleted and recreated | Not handled | Treated as a deletion; repaves if there is drift | [control and state, section 5](04-control-and-state.md#5-daemon-reconcile) |
| A node that never registers | Stays as it is, with no report | Stays as it is, and reports `NodeNotRegistered` after 10 minutes | [control and state, section 5](04-control-and-state.md#5-daemon-reconcile) |
| Repave commit | Saves the new configuration before kubelet is confirmed running (`cmd/agent/internal/daemon/nodeoperator.go:275-285`) | Commits only once the new node is ready | [lifecycles, section 2](05-lifecycles.md#2-repave) |
| A crash during a repave unpack | Skips unpacking when the machine directory is not empty, so later attempts reuse a partial rootfs (`pkg/agent/phases/rootfs/oci/task.go:61-64`) | A slot is provisioned again unless its completion marker is present | [node runtime, section 2](03-node-runtime.md#2-node-spec-and-bundle) |
| Active slot after a crash | `kube1` wins if both slots have a configuration file (`cmd/agent/internal/daemon/nodeoperator.go:77`) | The runtime reports the enabled slot; `Recover` reconciles by bundle digest | [control and state, section 4](04-control-and-state.md#4-state) |
| Boot hooks | Regenerate configuration from the applied config | Read the bundle manifest saved at provision time | [node runtime, section 2](03-node-runtime.md#2-node-spec-and-bundle) |
| Host network reset | Fixed names and a fixed WireGuard range | Names plus link kinds, any WireGuard port, and owned iptables chains removed on reset | [host changes, section 2](06-host-changes.md#2-host-network-reset) |
| Reset | Leaves host preparation in place | Reverses it from the undo log | [host changes, section 3](06-host-changes.md#3-host-preparation-and-its-reversal) |
| MachineOperations without a handler | Ignored | Failed, if the kind belongs to the agent | [control and state, section 6](04-control-and-state.md#6-machineoperation-handling) |
| Configuration | v1 JSON or environment variables | Versioned v2 file, or v1 JSON converted; environment variables dropped | [CLI and configuration, section 2](01-cli-and-configuration.md#2-configuration), section 5 |
| Install check | Fingerprints the Kubernetes version, OCI image and API server, and fails `start` on any change | Fingerprints install-time settings only; goal-setting changes are handled as drift | [CLI and configuration, section 2](01-cli-and-configuration.md#2-configuration) |
| Agent upgrade | Checks the candidate answers `version` | Also checks it answers `v2 version` | [lifecycles, section 4](05-lifecycles.md#4-agentupgrade) |
| Extension points | `pkg/agent` as a library | Plugins, hooks and declarative extensions | [extensions](02-extensions.md) |

## 3. Code copied from v1

Go's `internal` rule prevents `internal/agentv2` from importing
`cmd/agent/internal`. v2 copies what it needs rather than moving shared code,
so v1 stays untouched and the copies can diverge.

| Source | Destination | Expected divergence |
|---|---|---|
| `cmd/agent/internal/installstate` | `internal/agentv2/state` | The record gains the runtime kind |
| `cmd/agent/internal/bootstrap` | `internal/agentv2/bootstrap` | Stages renamed for runtime-neutral work |
| `cmd/agent/internal/attest` | `internal/agentv2/credentials` (the `tpm` provider) | Wrapped as a bootstrap credential provider |
| `cmd/agent/internal/daemon` unit and recovery assets, enable, stop and repair logic | `internal/agentv2/daemon` | Unit runs `<current> v2 daemon` |
| `cmd/agent/internal/daemon/bootstrap_status.go` and Machine registration (`daemon.go:303-406`) | `internal/agentv2/control` (the `machina` providers) | Behind the goal and status interfaces |
| `cmd/agent/internal/daemon/agentupgrade.go`, `hostupgrade.go` | `internal/agentv2/agentupgrade` | Verifies `v2` support in candidates |
| The MachineOperation reconciler in `pkg/agent/daemon/machineoperation.go` | `internal/agentv2/control` | Fails unsupported operations that belong to the agent ([control and state, section 6](04-control-and-state.md#6-machineoperation-handling)) |
| Rendering templates from `pkg/agent/phases/nodestart/assets` and `pkg/agent/phases/rootfs/assets` | `internal/agentv2/bundle` and `runtime/nspawn` | None at first; parity is enforced by golden tests ([implementation, section 3](09-implementation.md#3-testing-and-validation)) |

Each copied file starts with a comment naming its source path and the commit it
was copied from. This list is kept current so v1 bug fixes can be checked
against the copies.

## 4. Existing packages used as libraries

v2 imports these unchanged: `pkg/agent/phases`, `pkg/agent/preflight`,
`pkg/agent/agentbinary`, `pkg/agent/daemoncred`, `pkg/agent/artifactsource`,
`pkg/agent/bootstrapartifacts`, the resolvers in `pkg/agent/goalstates`, the
OCI unpack in `pkg/agent/phases/rootfs/oci`, `pkg/agent/daemon` (controller
shell only), `internal/executil`, `internal/fsutil` and `internal/hostroot`.

Changes outside `internal/agentv2`:

| Change | Why | Effect on v1 and its consumers |
|---|---|---|
| Register the `v2` command group in `cmd/agent/internal/cmd/cmd.go:30-39` | Delivery: the `v2` command group ([CLI and configuration, section 1](01-cli-and-configuration.md#1-cli)) | None; v1 commands are unchanged |
| In `pkg/agent/phases/host/preflight_existing_deployment.go:130-139`, also detect v2's install record and daemon unit | Without it, v1 `start` on a v2 `host` runtime would mask v2's `kubelet` and `containerd` units | Additive; it only fires on hosts with v2 installed. AKS Flex Node calls `host.Preflight`, so it sees the check too. |
| Add the NVIDIA Container Toolkit artifact to `hack/cmd/agent-artifacts-builder` | The `host` runtime ships the toolkit as an artifact ([node runtime, section 5.2](03-node-runtime.md#52-host)) | Offline artifact bundles gain an artifact that v1 ignores |

In the other direction, v2's preflight refuses a host where v1 is installed. It
looks for v1's install record (`/var/lib/unbounded/agent/install-state.json`,
`cmd/agent/internal/installstate/store.go:21,85`), v1's daemon unit
(`unbounded-agent-daemon.service`, `pkg/agent/goalstates/constants.go:24`) and
v1's applied-configuration files (`/etc/unbounded/agent/kube{1,2}-applied-config.json`,
`pkg/agent/goalstates/constants.go:97-101`). On `nspawn-container`, a v1
machine and a v2 machine look the same, so these v1-only files are what tell
them apart.

## 5. Converting a v1 configuration

A configuration file without `apiVersion` is read as v1 `UnboundedAgentConfig`
JSON (`internal/provision/agent_config.go:34-81`, `pkg/agent/config/config.go`)
and converted field by field:

| v1 field | v2 field |
|---|---|
| `MachineName`, `NodeName` | `machine.name`, `machine.nodeName` |
| `ProvisioningFormat` | `machine.provisioningFormat` |
| `Cluster.Version`, `Cluster.CaCertBase64`, `Cluster.ClusterDNS` | `cluster.kubernetesVersion`, `cluster.caCertBase64`, `cluster.clusterDNS` |
| `Kubelet.ApiServer` | `cluster.apiServer` |
| `Kubelet.Auth.BootstrapToken` | `credentials.bootstrap` with type `static`; `credentials.kubelet` with type `bootstrap`; `credentials.daemon` with type `csr` |
| `Kubelet.Auth.ExecCredential` | `credentials.kubelet` with type `exec` |
| `Kubelet.KubeconfigData` | `credentials.kubelet` with type `kubeconfig` |
| `Kubelet.NodeIP`, `Labels`, `RegisterWithTaints`, `Configuration`, `ImageCredentialProvider` | The matching `kubelet.*` fields |
| `CRI.Containerd.Version`, `CRI.Containerd.SandboxImage`, `CRI.Runc.Version` | `components.containerd.version`, `components.containerd.sandboxImage`, `components.runc.version` |
| `CNI.PluginVersion` | `components.cni.version` |
| `Gantry`, `LocalDNS` | `components.gantry`, `components.localDNS` |
| `OCIImage`, `AdditionalHostDevices`, `AdditionalHostMounts` | `runtime.nspawnContainer.image`, `.additionalHostDevices`, `.additionalHostMounts` |
| `OfflineArtifacts` | `artifacts.offline` |
| `Attest.URL` | `credentials.bootstrap` with type `tpm` and that URL |
| `Downloads.<artifact>.BaseURL`, `.URL` | `artifacts.downloads.<artifact>.baseURL`, `.url` |
| `Downloads.<artifact>.Version` | The matching `components` version. For `Kubernetes`, it must equal `Cluster.Version`; a configuration where they differ is rejected. |
| `NSpawnLifecycle` | Not converted. v2 always runs its own boot hooks, so a v1 configuration that sets it is rejected with an explanation. |

A converted configuration gets the v1 defaults for everything v1 did not
configure: `runtime.type` is `nspawn-container`, the goal source, status reporter
and MachineOperations are `machina`, and there are no plugins, hooks or
extensions.
