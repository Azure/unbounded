# Repeatable AKS Flex cluster deployment

`hack/scripts/aks-flex-cluster.sh` is a self-contained Bash entrypoint for an
Azure evaluation cluster. It does not require the AKSFlexNode checkout, external
ARM/Bicep templates, or a local Go build. It downloads pinned public release
artifacts and installs the workstation plugin in its own state directory.

## Topology and prerequisites

The default topology is:

- One AKS-managed system node with `networkPlugin=none`.
- One logical ARM `FlexNodes` user pool named `flexpool`.
- Three standalone Ubuntu 24.04 x86_64 VMs, each with its own system-assigned
  managed identity, NIC, static Standard public IPv4 address, and 128 GiB disk.
- One VNet with separate `/24` system and Flex subnets.
- The Unbounded operator, networking controller/node agent, Machina, and the
  Flex site's managed kube-proxy. Optional components otherwise follow the
  selected operator release's defaults.
- Two Sites and private-L3 mesh peering. There are no public gateway pools or
  public WireGuard ports in this topology.

Three Flex VMs means **four Kubernetes nodes total**, including the system node.
The Flex pool itself does not provision Azure compute; the script creates the
VMs separately and registers them with the Flex agent.

Run the script from a Linux or macOS Bash workstation with:

- Azure CLI 2.90.0 or later, already authenticated with `az login`.
- `kubectl`, `jq`, Python 3, `curl`, `tar`, `openssl`, `base64`, and OpenSSH
  (`ssh` and `ssh-keygen`). Python uses only the standard library; no Python
  packages are needed. Both bootstrap transports still require an SSH key for
  VM access, so OpenSSH is required even with Run Command.
- Subscription access sufficient to register the two Flex preview features,
  refresh the ContainerService provider, create the infrastructure, read the
  AKS admin kubeconfig, assign the required scoped identity roles, and invoke
  Azure VM Run Command when using the default transport. The VM's Azure guest
  agent must be running; the selected Ubuntu marketplace image supplies it.
- A region/subscription where `AKSFlexNodePreview`, `PutMachinePreview`, and the
  built-in Azure Kubernetes Service Flex Node Agent Role are available.
- Outbound access to Azure, GitHub release assets, upstream bootstrap artifacts,
  and container registries. The smoke workload uses Docker Hub's BusyBox image.

The script installs `aks-preview` 22.0.0b8 if no compatible extension is present.
It stops if the Flex Node Agent Role is not published. It does not substitute a
Contributor or admin role for the VM identities.

The defaults pin Unbounded `v0.8.0` and AKS Flex Node `v0.2.0`, the paired baseline
in the Flex repository's operator guide. They are not aliases for `latest`.
The initial Kubernetes version defaults to the AKS regional default; pass
`--kubernetes-version X.Y.Z` to select a full supported patch version. The Flex
pool is created at the control plane's accepted version, and each host fetches
fresh pool-issued bootstrap settings.

## Usage

From the Unbounded repository root, inspect the offline plan:

```bash
bash hack/scripts/aks-flex-cluster.sh \
  --name my-flex-cluster \
  --location canadacentral \
  --subscription "<subscription-uuid>" \
  --plan
```

Create the whole deployment:

```bash
bash hack/scripts/aks-flex-cluster.sh \
  --name my-flex-cluster \
  --location canadacentral \
  --subscription "<subscription-uuid>" \
  --yes
```

Run **the same command again** to reconcile an existing deployment. To create a
separate cluster, use a different `--name`. Use `--state-dir` when maintaining
clusters with the same name in different subscriptions or resource groups.

The default resource group has the cluster's name and must be new or previously
created by this script with matching ownership/configuration tags. An unrelated
existing resource group is not adopted. `--resource-group` changes the name but
not this ownership requirement.

Host bootstrap uses Azure VM Run Command by default. This works even when the
workstation's network blocks outbound SSH, and does not require opening another
inbound port. The script validates a unique remote shell-completion marker;
VM extension success alone does not count as successful bootstrap. Pass
`--bootstrap-transport ssh` to bootstrap over SSH instead. The transport can be
changed on a rerun without changing the infrastructure's saved intent. There is
no automatic transport fallback: an SSH-mode run that cannot connect fails
without resetting the host. Rerun with `--bootstrap-transport run-command` to
use the VM agent instead.

The script detects the workstation's public IPv4 with `https://api.ipify.org`.
The only custom inbound Flex NSG rule opens TCP 22 to that single `/32`. Azure's
default VNet-internal rules remain in place for node/mesh connectivity. If the
workstation IP changes, rerun the same command; the SSH rule is updated and the
other Azure infrastructure is reused. If
automatic detection is unsuitable, supply `--ssh-source-ip <public-ipv4>`.
CIDRs and private addresses are not accepted for that option.

An Ed25519 key is generated under the state directory unless `--ssh-key` names
an existing private key. Keep the generated key; losing it is not repaired by
silently rotating VM access. The script runs `ssh-keygen -y` to derive a supplied
key's public key; an encrypted private key may require an interactive passphrase
prompt. `--yes` skips only the deployment confirmation. For unattended runs,
use the generated, permission-restricted key or an appropriately protected
noninteractive key file.

Run `bash hack/scripts/aks-flex-cluster.sh --help` for the complete option list.
`--flex-count` accepts 1-20 VMs for a new deployment; it does not scale an
existing deployment. `--vm-size` and `--system-vm-size` select separate sizes,
but Flex VMs must support the script's x86_64 image and agent archive. The
`--timeout` value is used for individual polling/rollout and host-bootstrap
waits, not an overall deadline for every Azure CLI operation.

## Shared ACR onboarding without application changes

A shared registry does not require sharing VM identities across clusters. With
`--acr NAME`, this script creates one dedicated `NAME-acr-pull` user-assigned
identity per test cluster, grants pull access on the existing ACR, and federates
the cluster's infrastructure ServiceAccount. The VM system-assigned identities
remain for ARM node bootstrap; no VM receives an ACR role from this integration.

```bash
bash hack/scripts/aks-flex-cluster.sh \
  --name my-flex-cluster \
  --location canadacentral \
  --subscription "<subscription-uuid>" \
  --acr sharedtestacr \
  --yes
```

This path requires an operator release containing the federated registry broker.
The network-only `v0.8.0` baseline does not provide it. Select a compatible
published version with `--unbounded-version`, or preinstall a compatible operator
before onboarding an existing cluster. The script retains upgraded operators
and checks broker capability before registry identity or role changes. It never
pretends an old release supports the feature.

The shared ACR must already exist in the selected subscription. It is not
adopted, retagged, made public, or deleted. RBAC registries receive an `AcrPull`
grant; ABAC-enabled registries use the built-in repository-reader role instead.
These are read grants at the selected registry, not push or subscription-wide
permissions. Existing conditional assignments are not replaced with broader
ones. Permission failures stop the run; there is no administrator-role fallback.

The script enables the AKS OIDC issuer if necessary. Trust is exact:

- Issuer: the cluster's returned OIDC issuer URL.
- Subject: `system:serviceaccount:unbounded-system:registry-pull`.
- Audience: `api://AzureADTokenExchange`.

It writes the registry login server, pull identity client ID, and tenant ID into
`gantry-config`'s `node-auth.yaml` key. The operator adds the selected HTTPS
upstream while preserving unrelated runtime settings and registries. A JSON
patch with a resource-version test protects the script's policy handoff from
concurrent changes. An existing foreign node-auth policy is not overwritten.

The operator projects the infrastructure account's token into its node-local
brokers and configures the kubelet exec adapter. Applications keep their normal
ServiceAccounts and Pod specifications. The operator needs Kubernetes access,
not Azure IAM-administrator permissions. This works independently of the Flex
node's physical location and does not use IMDS for registry authentication.

Use `--acr-pull-identity RESOURCE_ID` to deliberately reuse an existing
user-assigned identity in the selected subscription. The script resolves its
client/tenant/principal IDs without adopting its tags or lifecycle. Federation
binding names include a hash of the cluster identity and issuer, so different
clusters do not overwrite each other's trust. Many test clusters should normally
use the default per-cluster pull identity to keep access and retirement isolated.

An optional `--acr-probe-image sharedtestacr.azurecr.io/private/application:v1`
asks every broker to verify that existing image with an authenticated manifest
HEAD. This checks authorization without running the application or downloading
layers. Without it, repository permission is explicitly unverified; broker
rollout is not proof of end-to-end Gantry/P2P distribution. The ordinary network
smoke workload remains unchanged.

ACR onboarding can be added on a rerun of an existing deployment. It has a
separate saved intent and does not alter infrastructure ownership hashes or
rebootstrap joined VMs. Later ordinary reruns restore the saved registry
configuration. Changing its registry or identity target is rejected instead of
silently broadening access; an owned probe-image setting can be updated.

Retiring a cluster requires separate registry-role cleanup using the saved
scope/principal/role metadata. The shared ACR is outside the cluster resource
lifecycle. Do not remove an externally provided shared identity or a shared grant
as part of one cluster's retirement. This script remains provisioning-only and
does not delete these Azure resources automatically.

## Outputs and security

By default, private local state lives under `tmp/aks-flex/<name>/`:

- `intent.json`: immutable provisioning arguments and SSH public key.
- `id_ed25519`: generated private key, if applicable.
- `kubeconfig`: separate admin kubeconfig for this cluster, mode `0600`.
- `known_hosts`: recorded host keys when the SSH transport is used; later SSH
  connections verify them. Run Command bootstrap does not establish SSH trust.
- `ssh-access.txt`: one copyable SSH command for each Flex VM.
- `cache/`: pinned release archives, checksums, and bootstrap script.
- Generated Site, pool, smoke, and remote-bootstrap payloads.
- `registry-intent.json` and `registry-access.json` when ACR onboarding is enabled:
  the saved target, resolved identity identifiers, federation binding, scoped
  role metadata, and ownership information. These contain no registry tokens.
- `registry-config-patch.json` and diagnostic logs for the scoped policy handoff.
- `run-command-<vm>.sh` and `run-command-<vm>.json`: generated launchers and
  returned VM-agent output when using Run Command.

The state directory has mode `0700`. Do not commit, publish, or share it. On
the workstation, the script does not overwrite the user's default kubeconfig,
install binaries under `/usr/local`, or switch the active Azure subscription.
Every resource command specifies the chosen subscription explicitly. On each
Flex VM, bootstrap runs as root and the selected agent release controls its
host installation paths; `v0.2.0` installs under `/usr/local`.

Each VM receives the Flex Node Agent Role only at the logical Flex pool scope.
The control-plane identity gets Network Contributor only at this deployment's
VNet scope. Bootstrap credentials are retrieved on the VM using IMDS, not
embedded into the local script or distributed to other hosts. On the VM the
Flex bootstrapper stores its config as `0600 root:root`.

## Idempotence and recovery boundaries

The script reconciles Azure state rather than trusting a local "completed"
marker. It creates only missing, deterministically named resources. Matching
existing VMs, identities, pool, and role assignments are reused. A local lock
prevents concurrent runs using the same state directory.

It checks configuration/ownership before reusing resources. It refuses changed
counts, topology, SSH keys, initial version selections, sizes, and release pins
rather than treating a provisioning rerun as a scaling, upgrade, resize, or
reimage operation. It also rejects incompatible live network/VM/AKS settings
and unexpected custom Flex NSG rules. Failed Azure list requests are fatal;
they are never interpreted as proof that a resource does not exist.

Existing hosts are checked through the selected bootstrap transport. If the
agent unit and matching cluster, pool, node name, and node IP config exist,
bootstrap is skipped. A stopped matching daemon can be started without
reinstalling its binary or changing its config. The script does not downgrade
an agent or operator that was upgraded
outside this provisioning flow.

A run interrupted before bootstrap can resume. After VM creation or role
assignment, the next run reuses those resources. A later host failure does not
cause already-joined earlier hosts to be bootstrapped again.

Recovery is deliberately non-destructive:

- If a bootstrap left a worker without an agent service, the script stops on
  that host. Inspect its logs and explicitly repair/reset it before rerunning.
- If an Azure resource has a terminal failed provisioning state, inspect and
  resolve its Azure operation before retrying.
- If a VM was reimaged/replaced, a changed SSH host key is not automatically
  trusted. Verify the new identity before updating the saved known-host entry.
- A stale local `.lock` directory must be removed manually after confirming no
  deployment process is still using it.

This script never drains/deletes Kubernetes Nodes, deletes Azure resources,
resets a host, or repairs a partial bootstrap by destroying it. It is an
idempotent provisioning tool, not a day-2 lifecycle or teardown tool.

## Validation

Success requires every requested Flex Node to be Ready, in the expected pool
and Site, with a successfully provisioned ARM Machine. Pool selectors use the
Flex agent's `kubernetes.azure.com/agentpool` label, not the short `agentpool`
label that AKS also applies to its native nodes. The CNI and managed kube-proxy
DaemonSets must be rolled out. The script waits for asynchronous operator
workload creation before starting each component's rollout check.

The script retains a small `aks-flex-smoke` namespace containing a BusyBox HTTP
DaemonSet and Service. From every Flex worker's pod it tests HTTP to every peer
pod and to the Service's DNS name. `kubectl exec` and `logs` also exercise the
control-plane-to-kubelet callback path. These workloads are reconciled on reruns;
they are not created under random names or removed by this script.

### Evaluated deployment

The deployment and a subsequent reuse run passed in `canadacentral` with
Kubernetes `1.36.5`, Unbounded `v0.8.0`, AKS Flex Node `v0.2.0`, one system node,
and three standalone Flex VMs. All four nodes were Ready; the reuse run reported
all three Flex hosts as already joined. Pod HTTP, Service/DNS, exec, and logs
checks passed. This records one evaluated combination, not a guarantee that
every version, VM size, region, or subscription supports the preview. That live
evaluation predates federated ACR onboarding and does not validate the new broker
path; it requires separate live validation with a compatible operator.

Direct SSH from that workstation failed before authentication. Run Command
bootstrap and the cluster smoke checks succeeded without broadening the `/32`
SSH rule. Successful Run Command bootstrap does not verify workstation-to-VM
SSH connectivity.

### Offline regression tests

Run the deployment checks without Azure credentials or a Go build:

```bash
make aks-flex-test
shellcheck hack/scripts/aks-flex-cluster.sh
```

The Make target runs Bash syntax validation and the standard-library Python
suite. The direct suite entrypoint remains
`python3 hack/scripts/aks-flex-cluster_test.py`. The `AKS Flex Deployment Tests`
CI job runs the same target and ShellCheck independently of the Go/frontend
jobs.

Coverage includes the default Run Command transport, explicit SSH transport,
resource and host reuse, partial-run recovery, scoped roles and ownership,
public-IP changes, corrupt archives, configuration validation, asynchronous
operator workload creation, the canonical Flex pool label, and smoke-test
failure propagation. ACR tests cover late onboarding, owned and supplied pull
identities, shared-registry preservation, exact federation, role scopes, ABAC
reader selection, policy ownership, reruns, and authorization failures without
admin fallback or application changes. Run Command tests check actual payload success/failure
through a sandboxed `/bin/sh` handler, plus missing, duplicate, stale, and CRLF
completion markers. Documentation examples are exercised with `--plan` to
prevent accidental cloud calls.

The tests use offline Azure/Kubernetes/SSH models and keep all files under the
repository's `tmp/`. Only generated unit-test payloads execute locally, with
their file writes redirected to the fixture directory. They are not a
substitute for a live Azure deployment or a full host-bootstrap test.
