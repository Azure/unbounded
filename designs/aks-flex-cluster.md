# Repeatable AKS Flex cluster deployment

`hack/scripts/aks-flex-cluster.sh` is a self-contained Bash entrypoint for an
Azure evaluation cluster. It does not require the AKSFlexNode checkout, external
ARM/Bicep templates, or a local Go build. It downloads pinned public release
artifacts and installs the workstation plugin in its own state directory.

## Topology and prerequisites

The default topology is:

- One AKS-managed system node with `networkPlugin=none`.
- One logical ARM `FlexNodes` user pool named `flexpool`.
- One Uniform VMSS with three Ubuntu 24.04 x86_64 instances initially, a shared
  system-assigned worker identity, and one private NIC and 128 GiB disk per instance.
  `singlePlacementGroup=false` and no overprovisioning permit up to 1,000 instances.
- One Standard worker load balancer and one static public IPv4 address, shared
  for outbound access and inbound NAT V2 SSH mappings on ports 50000-50999.
  Worker instances have no individual public IPs.
- One VNet with a `/24` system subnet and a `/21` Flex subnet. The Flex Pod pool
  defaults to `/14`, accommodating 1,024 of the networking controller's default
  `/24` per-node blocks (`internal/net/controller/site_controller.go:710-720`).
- The Unbounded operator, Gantry, networking controller/node agent, Machina, and
  the Flex site's managed kube-proxy. Gantry starts without ACR configuration;
  other component settings follow the selected operator release's defaults.
- Two Sites and private-L3 mesh peering. There are no public gateway pools or
  public WireGuard ports in this topology.

Three Flex instances means **four Kubernetes nodes total**, including the system node.
The Flex pool itself does not provision Azure compute; the script creates the
VMSS separately. Every new instance derives its unique node name and private
address from IMDS and registers through the Flex agent automatically.

The shared worker identity is separate from AKS's control-plane/system-pool
identities and the optional shared ACR pull identity. The one public IP described
here is the worker IP, not a promise about AKS-managed system-pool resources.

Run the script from a Linux or macOS Bash workstation with:

- Azure CLI 2.90.0 or later, already authenticated with `az login`.
- `kubectl`, `jq`, Python 3, `curl`, `tar`, `openssl`, `base64`, and OpenSSH
  (`ssh` and `ssh-keygen`). Python uses only the standard library; no Python
  packages are needed. OpenSSH supplies the worker access key, not bootstrap transport.
- Subscription access sufficient to register the two Flex preview features,
  refresh the ContainerService provider, create the infrastructure, read the
  AKS admin kubeconfig, and assign the required scoped identity roles.
- A region/subscription where `AKSFlexNodePreview`, `PutMachinePreview`, and the
  built-in Azure Kubernetes Service Flex Node Agent Role are available.
- Outbound access to Azure, GitHub release assets, upstream bootstrap artifacts,
  and container registries. The smoke workload uses Docker Hub's BusyBox image.

The script installs `aks-preview` 22.0.0b8 if no compatible extension is present.
It stops if the Flex Node Agent Role is not published. It does not substitute a
Contributor or admin role for the shared worker identity.

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

VMSS custom data installs an enabled `aks-flex-vmss-bootstrap.service`. It starts
after cloud-final and network-online, waits for shared identity authorization,
and invokes the pinned Flex bootstrapper with per-instance registration settings.
The cloud-init launcher schedules the service without blocking, avoiding a
dependency deadlock. Transient failures restart the service; foreign or partial
host installations return exit 78 and require explicit repair. Cloud-init exit
2 permits recoverable warnings; other failures do not proceed to host installation.
Neither SSH nor per-instance Run Command is used for onboarding.

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
`--flex-count` accepts 1-1,000 instances for a new deployment; it does not scale an
existing deployment. `--vm-size` and `--system-vm-size` select separate sizes,
but Flex VMs must support the script's x86_64 image and agent archive. The
`--timeout` value is used for individual polling/rollout and host-bootstrap
waits, not an overall deadline for every Azure CLI operation.
The initial host-bootstrap timeout is saved with the VMSS intent. Changing
`--timeout` on a rerun changes local polling/rollout waits, not installed custom
data or future instances' bootstrap timeout.

### Scale out without repeating onboarding

After the initial three-instance deployment, scale the existing worker set:

```sh
az vmss scale \
  --resource-group my-flex-cluster \
  --name my-flex-cluster-workers \
  --new-capacity 1000 \
  --subscription "<subscription-uuid>"
```

Every new instance receives the existing VMSS custom data and shared identity.
No per-instance identity, role assignment, SSH bootstrap, or ACR attachment is
required. A provisioning rerun with the same **initial** arguments retains the
VMSS's current capacity. Scaling requires adequate regional compute quotas and
Flex preview support; do not provision 1,000 instances just to test the script.
Scale-in, draining, stale Kubernetes Node cleanup, upgrades, and reimaging are
not managed by this tool.

One IP imposes a real egress limit: the outbound rule allocates **32 SNAT ports
per backend**, preserving a port budget for 1,000 workers and SSH mappings.
Concurrent connections to a given destination can exhaust that allocation.
Workload-specific capacity testing is required; this is not a guarantee of
1,000-worker image-pull throughput or general-purpose egress capacity.

## Attach ACR separately

Cluster deployment is independent of registry authentication. A fresh cluster
installs Gantry without ACR configuration. The script does not read or write
registry policy, enable OIDC for registry access, configure federation, or grant
ACR roles. The removed `--acr`, `--acr-pull-identity`, and `--acr-probe-image`
options are rejected, including with `--plan`. Old registry state files are
ignored rather than restored or deleted on a provisioning rerun.

Follow the [ACR attachment guide](../docs/content/guides/gantry.md#acr-attachment-and-explicit-authentication-on-flex-nodes)
using this deployment's kubeconfig at `tmp/aks-flex/<name>/kubeconfig`.
Authentication is unchanged by default; managed authentication requires explicit
opt-in. Attachment needs a compatible plugin/operator release, not the pinned
`v0.8.0` baseline. Upgrade separately if necessary; provisioning reruns preserve
an already-upgraded operator.

With a compatible operator and explicit `aks-identity-binding` attachment, the
existing authentication policy configures new Linux Flex nodes automatically.
The operator also reconciles the configured token audience grant for their
individual kubelet identities. Attaching the registry again after scaling is
not necessary.

The shared VMSS identity and Flex Node Agent Role are only for node bootstrap,
not ACR pulls. Retiring a cluster and its registry access remains separate
from this provisioning-only script; do not remove a shared customer identity
or grant as part of one cluster's cleanup.

## Outputs and security

By default, private local state lives under `tmp/aks-flex/<name>/`:

- `intent.json`: immutable provisioning arguments and SSH public key.
- `id_ed25519`: generated private key, if applicable.
- `kubeconfig`: separate admin kubeconfig for this cluster, mode `0600`.
- `known_hosts`: recorded when you first execute a generated SSH command using
  `accept-new`; subsequent connections verify the saved key. Bootstrap does not
  establish SSH trust.
- `ssh-access.txt`: copyable SSH commands using the shared IP and queried NAT V2
  mappings, sampling up to 20 instances at large capacities.
- `cache/`: pinned release archives, checksums, and bootstrap script.
- Generated Site, pool, VMSS, custom-data, and smoke payloads.
- VMSS inventory and worker readiness snapshots.

For additional SSH mappings, find an instance's private IP with `az vmss nic
list`, then POST `{"ipAddress":"<private-ip>"}` with `az rest` to the
`queryInboundNatRulePortMapping?api-version=2023-06-01` URL in `ssh-access.txt`.
Use the returned `frontendPort`, not a port inferred from the instance ID.

The state directory has mode `0700`. Do not commit, publish, or share it. On
the workstation, the script does not overwrite the user's default kubeconfig,
install binaries under `/usr/local`, or switch the active Azure subscription.
Every resource command specifies the chosen subscription explicitly. On each
Flex VM, bootstrap runs as root and the selected agent release controls its
host installation paths; `v0.2.0` installs under `/usr/local`.

The shared VMSS identity receives one Flex Node Agent Role assignment only at
the logical Flex pool scope, covering current and future instances.
The control-plane identity gets Network Contributor only at this deployment's
VNet scope. Bootstrap credentials are retrieved on the VM using IMDS, not
embedded into the local script or distributed to other hosts. On the VM the
Flex bootstrapper stores its config as `0600 root:root`.

## Idempotence and recovery boundaries

The script reconciles Azure state rather than trusting a local "completed"
marker. It creates only missing, deterministically named resources. Matching
existing VMSS, identities, pool, and role assignments are reused. A local lock
prevents concurrent runs using the same state directory.

It checks configuration/ownership before reusing resources. It refuses changed
initial counts, topology, SSH keys, initial version selections, sizes, and release pins
rather than treating a provisioning rerun as a scaling, upgrade, resize, or
reimage operation. It also rejects incompatible live network/VM/AKS settings
and unexpected custom Flex NSG rules. Saved standalone-VM intent is not migrated;
use a new deployment rather than modifying an existing standalone cluster.
Failed Azure list requests are fatal;
they are never interpreted as proof that a resource does not exist.

The per-instance bootstrap service checks the local host on startup. If the
agent unit and matching cluster, pool, node name, and node IP config exist,
bootstrap is skipped. A stopped matching daemon can be started without
reinstalling its binary or changing its config. The script does not downgrade
an agent or operator that was upgraded
outside this provisioning flow.

A run interrupted before bootstrap can resume. After VMSS creation or role
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
DaemonSet and Service. It samples up to three worker Pods for cross-node HTTP
and Service/DNS tests, avoiding quadratic API calls at large capacities.
`kubectl exec` and `logs` also exercise the
control-plane-to-kubelet callback path. These workloads are reconciled on reruns;
they are not created under random names or removed by this script.

### Evaluated deployment

The deployment and a subsequent reuse run passed in `canadacentral` with
Kubernetes `1.36.5`, Unbounded `v0.8.0`, AKS Flex Node `v0.2.0`, one system node,
and three standalone Flex VMs. All four nodes were Ready; the reuse run reported
all three Flex hosts as already joined. Pod HTTP, Service/DNS, exec, and logs
checks passed. This records one evaluated combination, not a guarantee that
every version, VM size, region, or subscription supports the preview. That live
evaluation predates the VMSS refactor and does not validate VMSS provisioning
or automatic scale-out. Subsequent isolated branch-image testing verified private
ACR pulls on two of those standalone Flex nodes and audience authorization,
as detailed in the [Gantry guide](../docs/content/guides/gantry.md).

Direct SSH from that workstation failed before authentication. Run Command
bootstrap and the cluster smoke checks succeeded without broadening the `/32`
SSH rule. Successful Run Command bootstrap does not verify workstation-to-VM
SSH connectivity.

**The VMSS topology has offline validation only.** Neither a live VMSS deployment
nor a 1,000-worker deployment, SSH connection, or throughput benchmark has been
run. Existing evaluation resources were not migrated or changed for this refactor.

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

Coverage includes shared VMSS resources and identity, automatic custom-data
bootstrap, per-instance IMDS settings, 1,000-worker configuration, external
scale-out, retained capacity on reruns, NAT V2 mappings, resource and host reuse,
registration retries and pagination, scoped roles and ownership,
public-IP changes, corrupt archives, configuration validation, asynchronous
operator workload creation, the canonical Flex pool label, and smoke-test
failure propagation. Assertions ensure deployment and reruns do no ACR wiring,
ignore obsolete registry intent, preserve customer-owned authentication, and
reject the removed ACR flags before cloud calls or state writes. Generated
custom-data launchers and bootstrap registration arguments are exercised in
sandboxed fixtures, with IMDS replaced by local JSON files. Documentation
examples are exercised with `--plan` to
prevent accidental cloud calls.

The tests use offline Azure/Kubernetes/systemd models and keep all files under the
repository's `tmp/`. Only generated unit-test payloads execute locally, with
their file writes redirected to the fixture directory. They are not a
substitute for a live Azure deployment or a full host-bootstrap test.
