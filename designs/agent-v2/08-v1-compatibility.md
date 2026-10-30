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

v2 should have the same outcomes as v1. Each difference below has a reason
that justifies it. Everything else matches v1.

| Situation | v1 outcome | v2 outcome | Why | Section |
|---|---|---|---|---|
| The node never registers | Nothing happens and nothing is reported | Nothing happens; status reports `NodeNotRegistered` after 10 minutes | Visibility only; the action is the same | [control and state, section 5](04-control-and-state.md#5-daemon-reconcile) |
| The daemon restarts after it saw the Node deleted, but before the repave finished | The repave is forgotten. The operator has to bring the Node back and delete it again. | The repave continues, because the approval was saved to disk when the deletion was seen | Self-healing: the daemon finishes what it was approved to do | [control and state, section 4](04-control-and-state.md#4-state) |
| The new node fails to start during a repave | v1 saves the new configuration before checking kubelet (`cmd/agent/internal/daemon/nodeoperator.go:279-282`). Repaving into `kube2`, it retries. Repaving into `kube1`, the retry finds `kube1` already configured, because v1 checks `kube1` first (`nodeoperator.go:77`). It reports success although kubelet never started, and leaves the old `kube2` on disk. | Retried until the new node is ready, and reported as `Failed` with reason `RepaveFailed` in the meantime | v1 can report a broken repave as successful | [lifecycles, section 2](05-lifecycles.md#2-repave) |
| The host loses power while a repave unpacks the image | The next attempt skips unpacking because the machine directory is not empty, and starts from a partial image (`pkg/agent/phases/rootfs/oci/task.go:61-64`) | Unpacks again unless a completion marker exists, as v1's bootstrap already does (`pkg/agent/phases/rootfs/provision.go:23-27`) | v1 can start a corrupt node | [node runtime, section 2](03-node-runtime.md#2-node-spec-and-bundle) |
| After a reset | Docker stays masked, swap stays off, and the sysctl file, nftables flush unit, apt drop-ins and masked host kubelet and containerd remain, although `reset --help` promises to restore the host (`cmd/agent/internal/cmd/reset.go:22-24`) | Every change the agent made is undone. Installed packages stay. | v1 does not do what it promises | [host changes, section 3](06-host-changes.md#3-host-preparation-and-its-reversal) |
| Network cleanup on repave and reset | Deletes fixed link names whatever their type. Cleans WireGuard links only from `wg51820` to `wg51899` (`pkg/agent/phases/reset/network.go:225-241`), though gateway ports have no upper limit (`internal/net/controller/gatewaypool_controller.go:411-415`). Leaves the `UNBOUNDED-*`, `KUBE-*` and `CNI-*` iptables chains after reset. | Removes all of these. A link with the right name but the wrong type is reported, not deleted. | v1 leaves state behind, and can delete a link that is not its own | [host changes, section 2](06-host-changes.md#2-host-network-reset) |
| A MachineOperation kind the agent does not know, such as a newer agent operation sent to an older agent | Ignored, so it stays pending forever (`pkg/agent/daemon/machineoperation.go:114-116`) | Failed with `UnsupportedOperation`, if the kind starts with `Node` or `Agent` | An operation that stays pending forever blocks automation | [control and state, section 6](04-control-and-state.md#6-machineoperation-handling) (open question 1) |
| Configuration through environment variables | Without a configuration file, v1 reads `KUBE_VERSION`, `BOOTSTRAP_TOKEN`, `NODE_LABELS` and others (`cmd/agent/internal/cmd/config.go:37-40,104-161`) | Configuration file only. v1 JSON files are still accepted. | One configuration source. `kubectl unbounded` always writes a file (`cmd/kubectl-unbounded/app/assets/node-bootstrap/script.sh:20-40`). | [CLI and configuration, section 2](01-cli-and-configuration.md#2-configuration) |
| A v2 host is upgraded to a binary without v2 | Not applicable | The candidate is rejected before the switch | Without the check, the switch happens, the daemon fails, and the recovery unit rolls back: the same end state, but slower and noisier | [lifecycles, section 4](05-lifecycles.md#4-agentupgrade) |

v2 matches v1 on purpose in these places, where a different outcome was
considered and rejected:

- Only a Node deletion the daemon sees as it happens starts a repave. A Node
  deleted while the daemon is not running is ignored, so stopping the daemon
  is a safe way to keep a node as it is ([control and state, section 5](04-control-and-state.md#5-daemon-reconcile)).
- A new goal version, and the Machine's `RepavePending` condition, are
  recorded only when the daemon handles a Node deletion ([control and state, section 1](04-control-and-state.md#1-control-plane-integration)).
- Host devices are found again at every node start ([node runtime, section 4](03-node-runtime.md#4-host-boot-and-boot-units)).
- The install check uses v1's fingerprint and v1's resume behavior ([CLI and configuration, section 2](01-cli-and-configuration.md#2-configuration)).
- A NodeReboot on `host` shows a new Node boot ID and a `Rebooted` event, as
  on `nspawn-container` ([node runtime, section 5.2](03-node-runtime.md#52-host)).

Moving from `pkg/agent` as a library to plugins, hooks and declarative
extensions is the purpose of v2 ([extensions](02-extensions.md)), not a change to how a node
behaves.

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
