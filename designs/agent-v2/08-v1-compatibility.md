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
| Machine registration and status | `cmd/agent/internal/daemon/daemon.go:307-410`, `bootstrap_status.go` | `control`, the `machina` providers |
| NVIDIA, AMD and host devices | `pkg/agent/goalstates`, `pkg/agent/phases/nodestart/nvidia.go` | Discovery in `nodespec`; each runtime makes the devices available to the node |
| nspawn boot hooks (v1's `nspawn-lifecycle` command) | `pkg/agent/nspawnlifecycle` | `runtime.BootHook` and `boot-hook` |
| Daemon CSR credentials | `pkg/agent/daemoncred` | `credentials`, the `csr` provider |
| Repave on Node delete with drift detection | `cmd/agent/internal/daemon/controller_node.go`, `nodeoperator.go` | `daemon`, `nodemanager` |
| NodeReboot, AgentUpgrade, AgentReset | `cmd/agent/internal/daemon/controller_machineoperation.go` | `daemon`, `agentupgrade`, `nodemanager` |
| HostOSReboot | `cmd/agent/internal/daemon/hostosreboot.go` | `daemon` ([lifecycles, section 4](05-lifecycles.md#4-hostosreboot)) |
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
| The new node fails to start during a repave | v1 saves the new configuration before checking kubelet (`cmd/agent/internal/daemon/nodeoperator.go:293-296`). Repaving into `kube2`, it retries. Repaving into `kube1`, the retry finds `kube1` already configured, because v1 checks `kube1` first (`nodeoperator.go:83`). It reports success although kubelet never started, and leaves the old `kube2` on disk. | Retried until the new node is ready, and reported as `Failed` with reason `RepaveFailed` in the meantime | v1 can report a broken repave as successful | [lifecycles, section 2](05-lifecycles.md#2-repave) |
| The host loses power while a repave unpacks the image | The next attempt skips unpacking because the machine directory is not empty, and starts from a partial image (`pkg/agent/phases/rootfs/oci/task.go:61-64`) | Unpacks again unless a completion marker exists, as v1's bootstrap already does (`pkg/agent/phases/rootfs/provision.go:23-27`) | v1 can start a corrupt node | [node runtime, section 2](03-node-runtime.md#2-node-spec-and-bundle) |
| A node image named by digest, `repository@sha256:...` | Unpacking fails with `tag "sha256:..." not found in OCI layout`. The registry copy is stored under the digest itself (`pkg/agent/artifactsource/ocilayout/acquire.go:114`), oras records no `ref.name` in `index.json` for a digest (oras-go v2.7.0 `content/oci/oci.go:437-453`), and umoci finds an image only by that name (`pkg/agent/phases/rootfs/oci/unpack.go:36-42`) | Unpacked. v2's copy of the unpack stores a pull by digest under a fixed local tag (section 3) | The image agent runtimes pull every image by digest ([agent runtime, section 4](12-agent-runtime.md#4-the-image-agent-runtimes)), and a digest is the safe way to name a node image too. Images named by tag unpack as before. | [node runtime, section 5.1](03-node-runtime.md#51-nspawn-container) |
| Owners of the files in a node's root file system | Every file is owned by root. v1 unpacks in umoci's rootless mode (`pkg/agent/phases/rootfs/oci/unpack.go:79`), which keeps an image's other owners only in a `user.rootlesscontainers` xattr, and by umoci's code also turns device nodes into empty files (umoci v0.6.0 `oci/layer/tar_extract.go:840-858`) | As in the image: v2 unpacks as root, without rootless mode | A service in the node image that runs as another user gets the files the image gives it. A migrated node keeps v1's unpack until its first repave ([migrating from v1, section 3](10-migration-from-v1.md#3-what-changes-on-the-host)), and validation task V15 compares both unpacks of every node image the repository builds ([testing, section 7](11-testing.md#7-validation-tasks)). | [node runtime, section 5.1](03-node-runtime.md#51-nspawn-container) |
| After a reset | Docker stays masked, swap stays off, and the sysctl file, nftables flush unit, apt drop-ins and masked host kubelet and containerd remain, although `reset --help` promises to restore the host (`cmd/agent/internal/cmd/reset.go:22-24`) | Every change the agent made is undone. Installed packages stay. | v1 does not do what it promises | [host changes, section 3](06-host-changes.md#3-host-preparation-and-its-reversal) |
| Network cleanup on repave and reset | Deletes fixed link names whatever their type. Cleans WireGuard links only from `wg51820` to `wg51899` (`pkg/agent/phases/reset/network.go:225-241`), though gateway ports have no upper limit (`internal/net/controller/gatewaypool_controller.go:411-415`). Leaves the `UNBOUNDED-*`, `KUBE-*` and `CNI-*` iptables chains after reset. | Removes all of these. A link with the right name but the wrong type is reported, not deleted. | v1 leaves state behind, and can delete a link that is not its own | [host changes, section 2](06-host-changes.md#2-host-network-reset) |
| An agent operation aimed at this Machine has `ttlSecondsAfterFinished` | Kept forever unless machine-ops-controller or metalman runs in the cluster; the agent has no deletion code | Deleted by the agent that many seconds after it completes or fails | Many clusters run neither controller, and fleet migrations create one operation per Machine (issue #1043) | [control and state, section 6](04-control-and-state.md#6-machineoperation-handling) |
| What the agent reports on the Machine | `AgentBootstrapped`, `RepavePending`, `status.configuration` and `status.observedProvisioningFormat` | The same, plus the `AgentHealthy` and `ReinstallPending` conditions and `status.agent.nodeRuntime` and `status.agent.agentRuntime` | v1 reports nothing on the Machine when a repave keeps failing or a node never registers, and the node runtime and agent runtime are new | [control and state, section 1](04-control-and-state.md#1-control-plane-integration) |
| The host's own containerd on an `nspawn-container` host | Stopped and masked (`cmd/agent/internal/cmd/bootstrap.go:109-111`) | Left as it is; the host's kubelet is still masked | The port checks v1 added with the masking catch the real conflict, and on Azure Container Linux the host's containerd ships enabled and holds ACL's own images, and under the `containerd` agent runtime it runs the agent itself | [host changes, section 3](06-host-changes.md#3-host-preparation-and-its-reversal) |
| Configuration through environment variables | Without a configuration file, v1 reads `KUBE_VERSION`, `BOOTSTRAP_TOKEN`, `NODE_LABELS` and others (`cmd/agent/internal/cmd/config.go:37-40,104-161`) | Configuration file only. v1 JSON files are still accepted. | One configuration source. `kubectl unbounded` always writes a file (`cmd/kubectl-unbounded/app/assets/node-bootstrap/script.sh:20-40`), and a migrated host's configuration comes from v1's applied config ([migrating from v1, section 7](10-migration-from-v1.md#7-converting-v1s-state)). | [CLI and configuration, section 2](01-cli-and-configuration.md#2-configuration) |
| A v2 host is upgraded to a v1 binary: a v1 build, or an older v1 release | Not applicable | The candidate is rejected before the switch. A migrated host goes back to v1 only through `migrate --rollback` on the host. | Without the check, the switch happens, the daemon fails, and the recovery unit rolls back: the same end state, but slower and noisier. A v1 binary must never run on a host v2 has changed. | [lifecycles, section 5](05-lifecycles.md#5-agentupgrade), [migrating from v1, section 8](10-migration-from-v1.md#8-rollback) |
| A Node is deleted while the daemon is not running, for example while it restarts for an AgentUpgrade | Ignored: v1 only reacts to deletion events it receives (`cmd/agent/internal/daemon/controller.go:147-156`). The node keeps running with no Node object. | Acted on when the daemon starts again. The daemon saves the UID of the Node it has seen, and a missing Node, or one with a new UID, counts as deleted. | A repave request must not be lost because it arrived during a restart. AKS Flex Node already behaves this way (`pkg/daemon/repave_reconciler.go:266-271` at Flex `2c149e991`). | [control and state, section 5](04-control-and-state.md#5-daemon-reconcile) |
| When the goal version is recorded, and the Machine's `status.configuration` and `RepavePending` written | Only when the daemon handles a Node deletion (`cmd/agent/internal/daemon/controller_node.go:54-68`) | Also on any check whose goal settings match the applied ones, usually right after the daemon starts | A goal source should learn which version a node runs without waiting for its first repave. Only the timing changes, and only when nothing differs. | [control and state, sections 1 and 5](04-control-and-state.md#5-daemon-reconcile) |
| An AgentUpgrade's new daemon starts but never becomes ready | Reported `Complete` as soon as the daemon registers the Machine (`cmd/agent/internal/daemon/daemon.go:166-172`) | Reported `Complete` only after the daemon reports ready to systemd; a daemon that never does is rolled back like one that crashes | An upgrade that leaves a stuck daemon is not a success | [lifecycles, section 5](05-lifecycles.md#5-agentupgrade) |
| The API server cannot be reached when the daemon starts | The daemon exits (`cmd/agent/internal/daemon/daemon.go:166-168`). During a long outage its start limit trips and the recovery unit swaps it for `last-good` (`unbounded-agent-daemon-recovery.sh:11,26`). | The daemon reports ready after its local checks and keeps retrying in the background. Only right after an AgentUpgrade does readiness need one API call. | A control-plane outage should not roll back binaries across the fleet | [control and state, section 5](04-control-and-state.md#5-daemon-reconcile) |
| The daemon finds its state inconsistent or corrupt | The daemon exits when it cannot make sense of the host's machines (`cmd/agent/internal/daemon/daemon.go:137-140`) | Limited mode: reported on the Machine, no change to the node, only AgentReset and AgentUpgrade are handled. Right after an AgentUpgrade, the new binary is rolled back instead. | A host nobody can log into can still be reset or fixed from the cluster | [control and state, section 5](04-control-and-state.md#5-daemon-reconcile) |

v2 matches v1 on purpose in these places, where a different outcome was
considered and rejected:

- Host devices are found again at every node start ([node runtime, section 4](03-node-runtime.md#4-host-boot-and-boot-units)).
- The install check uses v1's fingerprint and v1's resume behavior ([CLI and configuration, section 2](01-cli-and-configuration.md#2-configuration)).
- A NodeReboot on `host` shows a new Node boot ID and a `Rebooted` event, as
  on `nspawn-container` ([node runtime, section 5.2](03-node-runtime.md#52-host)).
- A MachineOperation kind the agent has no handler for is ignored
  ([control and state, section 6](04-control-and-state.md#6-machineoperation-handling)).
- A repave that keeps failing is retried with controller-runtime's default
  backoff and no automatic fallback, and a node that never registers is
  recovered by hand. v2 only adds the reporting ([lifecycles, section 2](05-lifecycles.md#2-repave)).
- `HostOSReboot` does not wait for out-of-band host operations on the same
  Machine (issue #1044; [lifecycles, section 4](05-lifecycles.md#4-hostosreboot)).

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
| `cmd/agent/internal/daemon` unit and recovery assets, enable, stop and repair logic | `internal/agentv2/daemon` | The unit waits for the readiness file and only starts a binary whose daemon is v2's |
| `cmd/agent/internal/daemon/bootstrap_status.go` and Machine registration (`daemon.go:307-410`) | `internal/agentv2/control` (the `machina` providers) | Behind the goal and status interfaces |
| `cmd/agent/internal/daemon/agentupgrade.go`, `hostupgrade.go` | `internal/agentv2/agentupgrade` | Rejects candidates whose daemon is not v2's |
| `cmd/agent/internal/daemon/hostosreboot.go` | `internal/agentv2/daemon` | None at first. It keeps using `RebootHost` and `SetMachineOperationResult` from `pkg/agent/daemon` (`reboot.go:22`, `machineoperation.go:232`). |
| The MachineOperation reconciler in `pkg/agent/daemon/machineoperation.go` | `internal/agentv2/control` | Deletes expired operations the agent ran for this Machine ([control and state, section 6](04-control-and-state.md#6-machineoperation-handling)). Like v1, it caches every MachineOperation in the cluster until issue #1058 is fixed, and takes that fix. |
| Rendering templates from `pkg/agent/phases/nodestart/assets` and `pkg/agent/phases/rootfs/assets` | `internal/agentv2/bundle` and `runtime/nspawn` | None at first; parity is enforced by golden tests ([testing, section 1](11-testing.md#1-unit-conformance-and-golden-tests)) |
| `pkg/agent/phases/rootfs/oci`, and the registry pull in `pkg/agent/artifactsource/ocilayout` (`acquire.go:97-129`, `registry.go`) | `internal/agentv2/ociimage` | Two fixes (section 2): a pull by digest is stored under a fixed local tag, so umoci can find it, and it unpacks as root without umoci's rootless mode, so files keep the image's owners. Used for node root file systems ([node runtime, section 5.1](03-node-runtime.md#51-nspawn-container)) and the `systemd-nspawn` agent runtime's images ([agent runtime, section 4](12-agent-runtime.md#4-the-image-agent-runtimes)). |
| AKS Flex Node's container install, Azure/AKSFlexNode#370: the image build, the units that run the image, `host-exec`, AgentUpgrade by image and image cleanup (`Dockerfile.aks-flex-node-agent`, `scripts/agent-image/`, `pkg/daemon/agent_upgrade_container.go`, `pkg/daemon/assets/*-container*.service`) | `internal/agentv2/agentruntime/image` and `agentruntime/containerd`, `images/agent-v2/` | Host files are reached only through `hostfs` under `/host`, not by mounting host directories at their own paths; readiness through the readiness file; no entrypoint script, because nothing has to be mounted at its own path ([agent runtime](12-agent-runtime.md)) |

Each copied file starts with a comment naming its source path and the commit it
was copied from, and code from AKS Flex Node also names that repository. This
list is kept current so v1 and Flex fixes can be checked against the copies.

## 4. Existing packages used as libraries

v2 imports these unchanged: `pkg/agent/phases`, `pkg/agent/preflight`,
`pkg/agent/agentbinary`, `pkg/agent/daemoncred`, `pkg/agent/artifactsource`,
`pkg/agent/bootstrapartifacts`, the resolvers in `pkg/agent/goalstates`,
`pkg/agent/daemon` (the controller shell, `RebootHost` and
`SetMachineOperationResult`), `internal/executil`, `internal/fsutil` and
`internal/hostroot`. v2 copies the OCI unpack instead of importing it
(section 3), because two of its behaviors must change (section 2) and
`pkg/agent` is frozen for v1.

Changes outside `internal/agentv2`:

| Change | Why | Effect on v1 and its consumers |
|---|---|---|
| Choose the command set at build time. The v1 build registers v1's commands as today (`cmd/agent/internal/cmd/cmd.go:30-39`); the v2 build, built with the `agentv2` tag, registers v2's instead. The release tooling builds and publishes both, the v2 build as `unbounded-agent-v2-linux-<arch>` with the binary still named `unbounded-agent` inside its archive (`.goreleaser.yml:31-45,90-108`). | Shipping v2: one agent per build ([migrating from v1, section 2](10-migration-from-v1.md#2-release-plan-and-cutover)) | None for the v1 build's commands. v2's packages are linked only into the v2 build. |
| In `pkg/agent/phases/host/preflight_existing_deployment.go:130-139`, also detect v2's install record | Without it, v1 `start` on a v2 `host` runtime would mask v2's `kubelet` and `containerd` units | Additive; it only fires on hosts with v2 installed. AKS Flex Node calls `host.Preflight`, so it sees the check too. |
| v1's `daemon` and `reset` refuse on a host with a v2 install record | v1 and v2 share the daemon unit names, so v1's `reset` would remove v2's daemon unit ([migrating from v1, section 11](10-migration-from-v1.md#11-changes-outside-internalagentv2)) | Additive; only fires on hosts with v2 installed |
| Add the AgentUpgrade parameters `distributionManifestURL` and `distributionManifestSHA256` to the `MachineOperationSpec.parameters` API comment, and so the generated CRD description, and to the MachineOperation reference page (`docs/content/reference/machina-crd.md`), with the `oci://` form of `downloadURL` for agents delivered by image | Updating the distribution with the binary ([lifecycles, section 5](05-lifecycles.md#5-agentupgrade)), and AgentUpgrade by image ([agent runtime, section 7](12-agent-runtime.md#7-agentupgrade-by-image)) | Documentation only. v1 daemons read only `downloadURL` and `sha256` (`cmd/agent/internal/daemon/agentupgrade.go:51-61`) and ignore the others. Every v2 release reads them. |
| Add `spec.agent.nodeRuntime`, `spec.agent.agentRuntime`, `status.agent.nodeRuntime` and `status.agent.agentRuntime` to the Machine API, the `AgentHealthy` and `ReinstallPending` conditions, and the `AgentBootstrapped` reasons `NodeStartFailed` and `RuntimeUnsupported`, with their API comments and the regenerated CRD | A reinstall must keep the Machine's node runtime and agent runtime ([CLI and configuration, section 2](01-cli-and-configuration.md#choosing-the-node-runtime)), and v2 reports what v1 does not ([control and state, section 1](04-control-and-state.md#1-control-plane-integration)) | Additive. The fields default to `nspawn-container` and `host`, and v1 neither reads nor writes the new fields. |
| `provision.BuildAgentConfig` writes the Machine's node runtime and agent runtime into the agent configuration, as new `NodeRuntime` and `AgentRuntime` fields, and every provisioning path installs the v2 build for a `host` Machine. Provisioning refuses a `host` Machine whose `spec.agent.version` has no v2 build, and the provisioning script checks the downloaded binary's `version`. A Machine with an image agent runtime gets the Ignition units that run the image ([agent runtime, section 8](12-agent-runtime.md#8-bootstrap-and-reset)). `kubectl unbounded machine manual-bootstrap` gains `--node-runtime` and `--agent-runtime` flags. | Every reinstall honors the node runtime and agent runtime ([CLI and configuration, section 2](01-cli-and-configuration.md#choosing-the-node-runtime)) | None for `nspawn-container` Machines with the `host` agent runtime |
| The release pipeline publishes the agent image, `ghcr.io/azure/unbounded-agent`, built reproducibly, with build provenance, for both architectures, and the root file system archive of each, made with `podman export`, with its SHA-256 ([agent runtime, section 2](12-agent-runtime.md#2-images)) | The image agent runtimes | None |
| v1's `start` refuses a configuration whose `NodeRuntime` is `host`, instead of ignoring the field | An old configuration reader would otherwise install `nspawn-container` on a host built for `host` | Only fires for configurations that ask for `host` |
| Add the NVIDIA Container Toolkit artifact to `hack/cmd/agent-artifacts-builder` | The `host` runtime ships the toolkit as an artifact ([node runtime, section 5.2](03-node-runtime.md#52-host)) | Offline artifact bundles gain an artifact that v1 ignores |

In the other direction, v2's preflight refuses a host where v1 is installed. It
looks for v1's install record (`/var/lib/unbounded/agent/install-state.json`,
`cmd/agent/internal/installstate/store.go:21,85`) and v1's applied-configuration
files (`/etc/unbounded/agent/kube{1,2}-applied-config.json`,
`pkg/agent/goalstates/constants.go:97-101`). The daemon unit cannot tell them
apart, because v1 and v2 use the same unit names, and on `nspawn-container` a
v1 machine and a v2 machine look the same. These v1-only files are what tell
them apart. The supported way to move such a host to v2 is to upgrade it to the
v2 build ([migrating from v1](10-migration-from-v1.md)).

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
| `OCIImage`, `AdditionalHostDevices`, `AdditionalHostMounts` | `nodeRuntime.nspawnContainer.image`, `.additionalHostDevices`, `.additionalHostMounts` |
| `OfflineArtifacts` | `artifacts.offline` |
| `Attest.URL` | `credentials.bootstrap` with type `tpm` and that URL |
| `Downloads.<artifact>.BaseURL`, `.URL` | `artifacts.downloads.<artifact>.baseURL`, `.url` |
| `Downloads.<artifact>.Version` | The matching `components` version. For `Kubernetes`, it must equal `Cluster.Version`; a configuration where they differ is rejected. |
| `NodeRuntime` | `nodeRuntime.type`. A file without it converts to `nspawn-container`. |
| `AgentRuntime` | `agentRuntime`. A file without it converts to the `host` agent runtime. |
| `NSpawnLifecycle` | Not converted. v2 always runs its own boot hooks, so a v1 configuration that sets it is rejected with an explanation. |

A converted configuration gets the v1 defaults for everything v1 did not
configure: `nodeRuntime.type` is `nspawn-container`, `agentRuntime.type` is `host`, the
goal source and status reporter are `machina`, MachineOperations are enabled,
and there are no plugins, hooks or extensions.
