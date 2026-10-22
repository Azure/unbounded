# Private AzureVM Karpenter POC assets

Start with the [short POC explanation](../../designs/provider-independent-autoscaling/poc.md)
for the architecture and result. This runbook contains the environment-specific
deployment, testing, evidence, and cleanup details.

These assets target the dedicated `unbounded-karp-poc-1021-c25d` resource group
in `eastus2` and private registry `ubkarppoc1021c25d.azurecr.io`. They do not
provision the base infrastructure or upload artifacts during packaging/rendering.
The explicitly invoked acceptance test does drive real worker provisioning and
release through the installed controllers. No helper changes the current
Kubernetes context. Run them from this worktree, not another checkout.

The bounded Azure/AWS extension passed live acceptance in the same retained
cluster at **2026-10-22T14:41:02.519516Z**. The
[AWS extension runbook](#aws-extension-with-the-frozen-karpenter-plugin) below
and [accepted dual-cloud evidence](#accepted-dual-cloud-run) are separate from
the historical Azure procedure and evidence. This is a bounded two-provider
proof, not full provider or scale conformance. Keep the accepted implementation
and harness frozen; do not rebuild the Karpenter plugin or accepted agent.

**Historical Azure verified outcome:** the finite Karpenter/AzureVM POC passed on
**2026-10-22T02:47:30.330246Z**, including scale from zero, actual worker readiness,
provisioning-time controller restart recovery, PDB-blocked termination, and
confirmed Azure cleanup. See [the evidence record](#accepted-live-run) and
[scope limits](#qualification-limits). This is not full provider-independent
capacity conformance.

Keep kubeconfig, SSH keys, Docker configuration, generated bootstrap, and
new POC credentials under the ignored `tmp/karpenter-poc/`.
Pass the local kubeconfig explicitly to every Kubernetes command. Never
publish the modified binaries or bootstrap configuration.

Existing signed-in Azure CLI authentication may be reused with
`AZURE_CONFIG_DIR` unset. Every Azure operation in the acceptance helper passes
the dedicated subscription explicitly; the helper never runs `az account set`
or changes the Kubernetes context. Do not copy existing CLI credentials into
the worktree or require another login. If supplying `AZURE_CONFIG_DIR`, it must
identify an existing directory under `tmp/karpenter-poc/`.

## Retained environment

The environment remains available for repetition, not retired. Current
coordinator-reported state after recovery and dual-cloud cleanup on 2026-10-22:

- The original deallocated `system` pool could not restart because Azure
  returned `SkuNotAvailable`. The coordinator created `systemv6` with
  `Standard_D4s_v6`, retaining 4 vCPU, 16 GiB, x64, Generation 2, Azure Linux,
  zone `1`, and Kubernetes 1.35.8 in the same AKS cluster.
- The replacement node `aks-systemv6-26337157-vmss000000` is Ready at
  `10.240.0.5`, with node pod CIDR `100.124.1.0/24`. The current `gw-main`
  public endpoint is `74.249.57.110`, observed WireGuard UDP port `51822`.
  The NSG port range remains `51820-51899` with the existing NAT-IP allowlist.
- Before deleting the old pool, the coordinator verified replacement readiness,
  controller availability, system-side DNS, exec/logs, and no remaining PVCs or
  application pods on the old pool. All controllers are now available.
- An Azure Workload Identity webhook outage had blocked the Azure controller's
  failed ReplicaSet. After recovery, its rollout was restarted successfully.
  Both provider controllers now use the same image below, retaining explicit
  `AzureVM` / `AWSEC2` provider selection and their separate identities:

  ```text
  ubkarppoc1021c25d.azurecr.io/machine-ops-controller@sha256:0465176e5fc327590d9585be938ef27f292dca5123f6ade5287555d51fd66d16
  ```

- The Karpenter source/image and accepted agent/rootfs remain unchanged.
  The accepted dual-cloud run used `Standard_D4s_v6` / `eastus2-1` and
  `m6i.xlarge` / `us-east-1a` concurrently; recheck current offering eligibility
  before repetition.
- After normal worker release, the coordinator independently verified only the
  healthy `systemv6` Node remains and all controller Deployments are
  Ready. Allocations, MachineOperations, Machines, NodeClaims, worker Nodes, and
  worker child resources are zero; both dual-probe Deployments have replicas
  zero. The Azure worker resource group is empty. AWS has no active worker
  subnet instances; the captured EC2 instance is terminated and its ENI/EBS
  are absent. Terminated EC2 metadata can remain visible normally.

### Accepted Azure baseline (historical)

The accepted Azure run and the coordinator's independent final check left:

- The AKS system Node Ready, all six controller Deployments Ready, and the
  Karpenter Deployment at 2/2 Ready replicas.
- MachineClass `azure-poc` and NodePool `azure-poc` Ready, independently checked
  after acceptance succeeded.
- No worker Nodes, Machines, MachineOperations, NodeClaims, or allocations.
- The worker resource group empty, including VMs, NICs, and managed disks.
- `karpenter-poc/worker-demo` at replicas zero, its `minAvailable: 1` PDB
  restored, and the private net-node pull Secret still attached.
- Optional Gantry absent after explicit uninstall and operator reconciliation.

### Current retained identifiers

| Resource | Identifier |
| --- | --- |
| Tenant | `70a036f6-8e4d-4615-bad6-149c02e7720d` |
| Subscription | `44654aed-2753-4b88-9142-af7132933b6b` |
| Region | `eastus2` |
| Main resource group | `unbounded-karp-poc-1021-c25d` |
| AKS cluster | `unbounded-karp-poc` |
| System pool / SKU | `systemv6` / `Standard_D4s_v6` |
| System pool OS / zone | Azure Linux / `1` |
| Current system node | `aks-systemv6-26337157-vmss000000` |
| System node internal IP | `10.240.0.5` |
| System node pod CIDR | `100.124.1.0/24` |
| Current `gw-main` public endpoint | `74.249.57.110`, WireGuard UDP `51822` |
| AKS managed-node resource group | `unbounded-karp-poc-1021-c25d-nodes` |
| Azure worker resource group | `unbounded-karp-poc-1021-c25d-workers` |
| Virtual network | `unbounded-karp-poc-vnet` |
| Private ACR | `ubkarppoc1021c25d.azurecr.io` |
| Private Blob storage | Account `ubkarppoc1021c25d`, container `artifacts` |
| Capacity Workload Identity client ID | `0873ab46-c634-4899-a81e-ba88dd079f36` |
| Worker managed identity client ID | `357ec185-ed12-43ca-9a1e-d359fb969894` |
| Site / MachineClass / NodePool | `azure-poc` |
| Accepted Azure provider ConfigMap (historical) | `unbounded-system/azure-poc-provider-9c98269c680a4241` |
| Current Azure v6 provider ConfigMap | `unbounded-system/azure-poc-provider-f3c102acc16b5507` |
| Current Azure MachineClass generation / revision | `3` / `sha256:b8b1dc3aeb7d2e609fd202443ad02805a58c404db7ca031d506fcaf9fa6f4132` |
| Immutable bootstrap ConfigMap | `unbounded-system/azure-poc-bootstrap-59af8ed63bcc8993` |

The full retained AKS, worker subnet, and worker identity ARM IDs are:

```text
/subscriptions/44654aed-2753-4b88-9142-af7132933b6b/resourceGroups/unbounded-karp-poc-1021-c25d/providers/Microsoft.ContainerService/managedClusters/unbounded-karp-poc
/subscriptions/44654aed-2753-4b88-9142-af7132933b6b/resourceGroups/unbounded-karp-poc-1021-c25d/providers/Microsoft.Network/virtualNetworks/unbounded-karp-poc-vnet/subnets/workers
/subscriptions/44654aed-2753-4b88-9142-af7132933b6b/resourceGroups/unbounded-karp-poc-1021-c25d/providers/Microsoft.ManagedIdentity/userAssignedIdentities/unbounded-karp-poc-worker
```

The public AKS API endpoint is:

```text
https://unbounded--unbounded-karp-p-44654a-d30gqh9z.hcp.eastus2.azmk8s.io:443
```

The retained cluster uses Kubernetes 1.35.8, `network-plugin=none`, Unbounded
CNI, OIDC, and Workload Identity. System/worker node CIDRs are
`10.240.0.0/24` / `10.240.1.0/24`; their pod CIDRs are
`100.124.0.0/16` / `100.125.0.0/16`. The replacement system node's individual pod
CIDR is `100.124.1.0/24`; the cluster Site's pod supernet did not change.
Private agent delivery still requires the
worker identity's container-scoped Blob Reader grant and the capacity
identity's separately scoped provisioning permissions.

## Private agent delivery

Use a private Azure Blob container and a dedicated worker user-assigned
managed identity. This avoids SAS URLs in custom resources, process arguments,
and the existing installer's download logs.

The infrastructure coordinator must prepare:

- A storage account in the POC resource group with anonymous Blob access and
  shared-key authentication disabled, HTTPS required, and TLS 1.2 or newer.
  Its authenticated HTTPS Blob endpoint must be reachable from the worker
  subnet. A private container does not require a private endpoint.
- A private container, such as `artifacts`.
- A dedicated worker identity with `Storage Blob Data Reader` scoped to that
  container. Attach this identity to each standalone worker VM.
- An upload identity authorized with `Storage Blob Data Contributor` on that
  container. Upload with Entra authentication, not account keys.

The capacity-controller identity is separate: it provisions/deletes resources
and may require permission to assign the worker identity. The Karpenter plugin
has no Azure identity and never reads Blob credentials.

### Build and package

After the agent lifecycle guards are implemented, run:

```bash
bash hack/karpenter-poc/prepare-artifacts.sh
```

This builds the current worktree's `linux/amd64` agent and writes:

```text
tmp/karpenter-poc/artifacts/
  manifest.json
  SHA256SUMS
  unbounded-agent-linux-amd64.tar.gz
  worker-artifacts.py
```

The manifest identifies the tarball by SHA-256 and its immutable Blob name.
It also records the source commit and whether the worktree was dirty. Rebuild
after agent changes; a source commit alone does not identify uncommitted code.
No cloud operation runs during preparation.

The default rootfs is the existing public Ubuntu 24.04 image, resolved from
`ghcr.io/azure/agent-ubuntu2404:v20260619-1`:

```text
ghcr.io/azure/agent-ubuntu2404@sha256:d72dd1b1d98d6b74250467fe28d3e31989a9f413b0355abedcbd1072e3653863
```

The index includes a Linux amd64 image. The accepted live run verified bootstrap
with the pinned host image and Kubernetes version below. That does not qualify
other rootfs, host-image, architecture, or Kubernetes combinations.
An alternate rootfs must also be digest-pinned:

```bash
bash hack/karpenter-poc/prepare-artifacts.sh \
  registry.example/rootfs@sha256:YOUR_VERIFIED_DIGEST
```

If an agent was already built in this worktree, package it without rebuilding:

```bash
python3 hack/karpenter-poc/package-artifacts.py \
  --agent-binary tmp/karpenter-poc/build/unbounded-agent \
  --rootfs-image ghcr.io/azure/agent-ubuntu2404@sha256:d72dd1b1d98d6b74250467fe28d3e31989a9f413b0355abedcbd1072e3653863
```

### Upload and integrate

The coordinator uploads the tarball to the private container using exactly
`manifest.json`'s `agent.blobName`. Do not overwrite that content-addressed
name with different bytes. The bootstrap helper itself is embedded in
private VM cloud-init; it does not need a public download URL.

The AzureVM bootstrap implementation must perform these steps in order:

1. Write `worker-artifacts.py` into `/var/lib/unbounded/bootstrap/`.
2. Run it with the storage account, container, manifest Blob name, expected
   SHA-256, and worker identity client ID.
3. Stop bootstrap on failure. The helper uses IMDS, sends its token only as an
   HTTPS Authorization header to Azure Blob, rejects redirects, verifies the
   downloaded bytes, and atomically installs a mode-0600 local tarball.
4. Run the existing Unbounded installer with
   `AGENT_URL=file:///var/lib/unbounded/poc/unbounded-agent-linux-amd64.tar.gz`
   and `AGENT_SHA256` set to the manifest digest. The installer verifies the
   tarball before extraction; no signed URL is logged.
5. Supply the pinned rootfs through agent configuration's `OCIImage`.

For a complete local cloud-init example, place a valid, credential-bearing
Unbounded agent JSON configuration in the ignored state directory, then run:

```bash
python3 hack/karpenter-poc/render-bootstrap.py \
  --agent-config tmp/karpenter-poc/agent-config.json \
  --account YOUR_STORAGE_ACCOUNT \
  --container artifacts \
  --client-id YOUR_WORKER_IDENTITY_CLIENT_ID
```

The mode-0600 output is `tmp/karpenter-poc/worker-cloud-init.yaml`. It includes
bootstrap credentials from the input, so do not print, commit, or put it into
a public custom resource. The renderer enforces Azure's decoded customData
size limit. This reference renderer does not replace the capacity controller's
per-allocation bootstrap/token lifecycle.

## Local validation

```bash
python3 -m unittest discover -s hack/karpenter-poc -p 'test_*.py'
shellcheck hack/karpenter-poc/prepare-artifacts.sh
```

These tests verify transport and packaging invariants. They do not constitute
live autoscaling acceptance.

## Pinned controller integration

The isolated `plugins/karpenter` module pins upstream Karpenter `v1.14.2`.
It does not upgrade the root module's Kubernetes dependencies or import cloud
adapters. Its NodeClass is the existing `MachineClass`, not a second CRD.
The generated shared capacity schemas are checked by the plugin tests.

The coordinator deployed the corrected `karpenter-poc-v3` plugin at:

```text
ubkarppoc1021c25d.azurecr.io/unbounded-karpenter@sha256:ab49c4f98fd9c2f6e1d9b6711dc63f33614f7770f0f00219cc3e64904fe6ce1b
```

The accepted Azure run used the matching `karpenter-poc-v3` machine-ops controller:

```text
ubkarppoc1021c25d.azurecr.io/machine-ops-controller@sha256:9967f86fe989f90a7b85f95e51984cd554a6e35a31cdfd41a9826fb00a46bb3b
```

That machine-ops digest is historical. The current Azure and AWS controller
deployments both use digest `0465176e5fc327590d9585be938ef27f292dca5123f6ade5287555d51fd66d16`
as recorded in [the retained environment](#retained-environment); do not roll
back to the historical image when repeating acceptance.

Keep the controller image pins separate. Plugin v3 includes the scheduling and
resource-conversion corrections described below; do not roll back to v2.
Scheduling now creates a NodeClaim, MachineAllocation, and
HostAllocate operation. The historical attempts below explain the corrections
that preceded successful acceptance; they are not removed by the final result.
Root controller images and the private agent artifact were unchanged by the
initial plugin scheduling correction.

The next live attempt reached a real Azure VM with its allocation-derived ARM
name and native VM ID; both controllers restarted with all pods replaced and
Ready. HostAllocate tracking then stalled because the Azure adapter assumed a
NIC name instead of tracking the NIC identity actually attached to the VM.
The original NodeClaim retired before registration, and the coordinator stopped
the run and requested safe cleanup. The provider NIC/disk tracking correction is
included in the deployed machine-ops v3 image. This attempt did not prove
Ready-worker recovery or safe termination.

The first VM also independently failed rootfs preparation: ORAS copied the
pinned registry digest without a local ref-name annotation, and umoci could not
resolve that digest as a tag. The agent now gives the verified local copy a
fixed alias while preserving the original remote digest and content checks.
Local registry regressions cover tags, pinned single/multiarch OCI images,
Docker manifest lists, missing digests, corrupted content, and unsupported
platforms. Full layer extraction also passes in an offline test container.
These checks are not live worker-bootstrap acceptance.

The rebuilt agent tarball SHA-256 is
`32f4485b9706dafc46f089c7800470f0f31f30dab77898fb46447cc8cdd5a0c0`.
Its private package and manifest are under
`tmp/karpenter-poc/artifacts/`; the coordinator uploaded the new content-addressed
private blob and applied the updated immutable class inputs at generation 2.
Use that manifest when reproducing artifact delivery, not an older package.
The previous `ef1413529dc0e6c74b5d1d828701da9c90a4db62121b2766369e9b92a5c4a007`
artifact is superseded. The pinned rootfs digest has not changed.

The earlier allocation renderer had a separate final cleanup command that could
mask an installer failure. Allocation bootstrap now uses one shell command with
an EXIT cleanup trap: staging failure blocks installation, staging or installer
failure remains nonzero, and temporary agent configuration and installer files
are removed on success and failure. ReplaceCloudInit output is unchanged.
Rendered-command execution regressions verify these cases without invoking the
real installer or printing user data. The deployed machine-ops v3 image includes
this correction; the cleanup-image v2 snapshot predates it. Cloud-init status 0
still does not prove a registered or Ready worker.

The preceding finite acceptance run reached a real Ready worker with the exact
provider ID, running network components, approved/issued kubelet and daemon
certificates, and completed HostAllocate child tracking. Both controllers
restarted with all replacement controller pods Ready. Karpenter hydration then
rejected the Node's zero-valued hugepages resources because advertised capacity
omits zero entries. Plugin v3 corrects that validation without changing the
observed resource maps or accepting positive over-capacity values.

The coordinator stopped that run and confirmed normal cleanup at
2026-10-22T02:35:05Z: zero worker VMs, NICs, disks, NodeClaims, and allocations,
with the demo scaled to zero and no finalizer bypass. This is not completed
acceptance for that attempt: the workload-running and PDB-blocked drain
scenarios were proven by the subsequent corrected-plugin run below.

The final plugin v3 run started at 2026-10-22T02:42:33Z and succeeded at
02:47:30Z. Its complete result is recorded under [Accepted live run](#accepted-live-run).

```bash
make -C plugins/karpenter lint test build manifests \
  VERSION=karpenter-poc-v3 \
  IMAGE=ubkarppoc1021c25d.azurecr.io/unbounded-karpenter@sha256:ab49c4f98fd9c2f6e1d9b6711dc63f33614f7770f0f00219cc3e64904fe6ce1b
```

The ignored `tmp/karpenter-poc/karpenter.yaml` now uses that exact deployed v3
digest. After any future image push, regenerate it with the returned immutable
digest before rollout rather than reapplying a superseded deployment.

The binary and rendered deployment stay under `tmp/karpenter-poc`. The
deployment contains two leader-elected replicas on AKS system nodes, no Azure
identity, and the upstream CRDs embedded by the pinned module. The capacity
controller must already publish current-generation Ready classes, fresh
availability observations, and sourced USD hourly prices. Missing price
observations fail closed; stale or unknown availability is never launchable.
Only `CloudProvider.Delete`, after Karpenter drain authorization, requests
allocation release. Infrastructure absence is reported only after confirmation.

The module cache contains its own ignored `go.mod` boundary, so root `go
build ./...` does not traverse downloaded dependency source.

### Provisioner compatibility regression

The first live scheduling attempt exposed an upstream offering-compatibility
failure before any NodeClaim or Azure VM was created. The generic capacity
label `unbounded-cloud.io/capacity-type` was returned as an offering constraint
in addition to Karpenter's normalized capacity-type label. Karpenter rejects
undeclared custom offering keys even when the instance type otherwise matches.

Discovery now retains all provider labels in instance-type requirements, while
offering constraints include only upstream-known or NodePool-declared keys.
Caller requirements, custom template labels, correlated offerings, resource
overhead, and workload NodePool isolation are preserved.

`TestPinnedSchedulerScalesCapturedWorkerPodFromZero` replays the captured
MachineClass status and NodePool spec through the actual pinned upstream
`NewScheduler` prefilter and `Solve`, using the checked-in workload and a clock
inside the observation's validity interval. Its checked-in fixtures contain
only the relevant nonsecret fields from that first attempt. To replay the
original complete nonsecret captures instead, use the normal worktree-local
Go environment and set:

```bash
export UNBOUNDED_KARPENTER_REPLAY_DIRECTORY="$PWD/tmp/karpenter-poc"
```

The directory must contain `live-machineclass.json` and `live-nodepool.json`.
No live API calls occur during these scheduler tests. Validate and build the
current corrected plugin using a distinct third image tag:

```bash
make -C plugins/karpenter lint test build \
  VERSION=karpenter-poc-v3

make -C plugins/karpenter image \
  CONTAINER_ENGINE=docker VERSION=karpenter-poc-v3 \
  IMAGE=ubkarppoc1021c25d.azurecr.io/unbounded-karpenter:karpenter-poc-v3
```

The coordinator pushes the image, rerenders with the pushed digest, and reruns
the original finite acceptance. Passing this offline scheduler regression does
not establish live worker bootstrap or safe termination.

### Observed-resource conversion regression

Shared allocation capacity omits zero resources, while registered Node
allocatable is copied verbatim, including zero-valued hugepages. An omitted
ResourceList key means zero. The plugin's common hydration validator now uses
that numeric value rather than requiring every allocatable key to be explicitly
present in capacity. Negative allocatable, unadvertised positive resources,
positive over-capacity values, and invalid required CPU/memory/pods still fail.

`TestObservedZeroResourcesThroughCreateGetAndList` uses the exact captured
capacity/allocatable maps through all three public provider methods and their
real unstructured allocation converter. The matching negative regressions
exercise those same consumers. To replay the original nonsecret capture
instead of the checked-in minimal fixture, use the worktree-local Go environment:

```bash
export UNBOUNDED_LIVE_ALLOCATION_RESOURCES="$PWD/tmp/karpenter-poc/live-allocation-resources.json"
make -C plugins/karpenter test
```

The original replay failed before the fix with the live hugepages error and
passes afterward. Shared API/controller/CRD behavior is unchanged.

## Base network installation

The coordinator has installed baseline images tagged `karpenter-poc-base`.
Do not confuse those images with final lifecycle-capable controller or agent
artifacts. The live Site names are `cluster` for the AKS system subnet and
`azure-poc` for the standalone worker subnet. `base-sites.yaml` records their
CIDRs and explicitly disables optional Gantry and token refresher.

Disabling Gantry does not uninstall an existing operator-managed installation.
The operator retains it if either its ConfigMap or DaemonSet remains
(`internal/operator/components/gantry/gantry.go:146`,
`internal/operator/components/gantry/gantry.go:290`). Deleting only the DaemonSet
is therefore insufficient. For an explicit opt-out in this dedicated POC:

1. Ensure every POC Site has `spec.components.gantry.enabled: false`.
2. Record the POC operator's replica count and temporarily scale only that
   operator to zero. Do not pause a shared operator.
3. Delete exactly `unbounded-system/daemonset/gantry` and
   `unbounded-system/configmap/gantry-config`.
4. Restore the operator's recorded replica count, wait for readiness, and verify
   both Gantry resources remain absent.

Use the explicit worktree-local kubeconfig for every operation. Preserve the
private net-node pull Secret and its ServiceAccount configuration. The
coordinator completed these steps with the POC operator restored to one Ready
replica, and both Gantry resources remain absent after reconciliation. No source
changes or affinity workarounds were needed.

The external `gw-main` gateway advertises the system node's public endpoint
to the unpeered worker Site. For the accepted Azure-only run, UDP ingress
`51820-51899` on the system node's NSG was restricted to the workers' NAT
address `20.81.185.46/32`; the AWS extension adds only its NAT
`98.82.143.0/32`, not internet-wide access. AKS private
ACR attachment does not grant standalone workers registry access. Configure
stable private net-node image-pull authentication separately before launching
workers; never place registry credentials in a capacity custom resource.

The existing public digest-pinned rootfs needs no pull credential. Modified
agent and controller artifacts remain private.

### Standalone-worker private image authentication

The coordinator prepares a private registry Docker configuration with
repository-read-only credentials for `unbounded-net-node`, stored mode 0600
at `tmp/karpenter-poc/net-node-dockerconfig.json`. An ACR repository-scoped
token is suitable; do not enable the registry admin account, publish the
image, or place token contents in these manifests. Giving the worker identity
`AcrPull` alone does not install a kubelet credential provider.

The coordinator has configured scope map `poc-net-node-read` with only
`unbounded-net-node` `content/read` and `metadata/read`, token `poc-net-node`,
Secret `unbounded-system/poc-net-node-pull`, and the ServiceAccount attachment
below. The issued `password1` expires approximately **2026-11-21**. Refresh it
before expiry and before repeating the POC after a long pause. Inspect only
expiration metadata, never credential values:

```bash
az acr token show \
  --subscription 44654aed-2753-4b88-9142-af7132933b6b \
  --registry ubkarppoc1021c25d \
  --name poc-net-node \
  --query 'credentials.passwords[].{name:name,expiry:expiry}' \
  --output table
```

For rotation, the coordinator generates a replacement token password with an
overlap window, updates the mode-0600 local Docker configuration without
printing credentials or enabling shell tracing, and reapplies the Secret using
the pipeline below. Verify a fresh private image pull before retiring the old
password. Cached images on existing nodes do not prove the replacement works.

Create the Kubernetes Secret without printing its contents, then attach it
to the net-node ServiceAccount using a separate field manager:

```bash
tmp/karpenter-poc/bin/kubectl --kubeconfig tmp/karpenter-poc/kubeconfig \
  create secret generic poc-net-node-pull \
  --namespace unbounded-system \
  --type kubernetes.io/dockerconfigjson \
  --from-file=.dockerconfigjson=tmp/karpenter-poc/net-node-dockerconfig.json \
  --dry-run=client --output=json |
tmp/karpenter-poc/bin/kubectl --kubeconfig tmp/karpenter-poc/kubeconfig \
  apply --server-side --field-manager=poc-registry-access -f -

tmp/karpenter-poc/bin/kubectl --kubeconfig tmp/karpenter-poc/kubeconfig \
  apply --server-side --field-manager=poc-registry-access \
  -f hack/karpenter-poc/net-node-pull-secret.yaml
```

The operator uses server-side apply with its own field manager
(`internal/operator/component/env.go:367`); its net-node ServiceAccount template
does not declare `imagePullSecrets`
(`deploy/net/node/01-serviceaccount.yaml.tmpl:6`). Verify the attachment remains
after the final operator rollout. Existing managed-node pods can continue using AKS
ACR attachment; newly created worker pods must receive this Secret reference.
Never print the Docker configuration, Secret, or bootstrap token during proof.

### Render the pinned capacity inputs

For the current recovery, reuse the accepted private agent/rootfs and existing
content-addressed Blob; changing the worker SKU does not require rebuilding or
uploading the agent. Render
nonsecret provider/bootstrap ConfigMaps, MachineConfiguration, the
WorkloadIdentity credential declaration, and MachineClass:

```bash
python3 hack/karpenter-poc/render-capacity.py \
  --price-observed-at 2026-10-22T14:05:30Z \
  --output tmp/karpenter-poc/aws/capacity-azure-v6.yaml
```

The accepted Azure run used the real Microsoft retail API observation at
`2026-10-22T01:03:58Z` for Linux `Standard_D4s_v3` in `eastus2`, USD 0.192 per
instance hour. Those SKU/price identifiers remain historical evidence, not
current recovery inputs. At `2026-10-22T14:04Z`, the coordinator observed that
offering unavailable despite a Ready class.

The current `Standard_D4s_v6` / `eastus2-1` target retains the 4-vCPU/16-GiB
bounds, x64/Generation 2 host compatibility, 64-GiB Premium_LRS root disk,
agent/rootfs/bootstrap, and conservative reserves. Its real retail observation
at `2026-10-22T14:05:30Z` is **USD 0.202 per instance hour**, Linux Consumption,
primary-region `Virtual Machines Dsv6 Series` / `D4s v6`, `1 Hour`. The raw
response is `tmp/karpenter-poc/aws/azure-v6-retail-prices.json`; Windows, Spot,
and Low Priority entries are excluded. Do not reuse the v3 price or invent a
timestamp. These prices exclude disks, discounts, and other charges.

The renderer requires the packaged helper's checksum, a digest-pinned rootfs,
and the local OpenSSH public key. Referenced ConfigMap names are content
addressed and immutable. No credential is embedded in any custom resource.
The current provider ConfigMap is `azure-poc-provider-f3c102acc16b5507`;
bootstrap remains `azure-poc-bootstrap-59af8ed63bcc8993`. The local render-input
record is `tmp/karpenter-poc/aws/azure-v6-render-inputs.json`. The applied manifest
is `tmp/karpenter-poc/aws/capacity-azure-v6.yaml`; the accepted dual-cloud run
used MachineClass generation `3`, revision
`sha256:b8b1dc3aeb7d2e609fd202443ad02805a58c404db7ca031d506fcaf9fa6f4132`.

The coordinator applied these immutable provider inputs and the existing
MachineClass and NodePool before the successful dual-cloud run. The explicit
reapply and current-eligibility checks remain:

```bash
tmp/karpenter-poc/bin/kubectl --kubeconfig tmp/karpenter-poc/kubeconfig \
  apply -f tmp/karpenter-poc/aws/capacity-azure-v6.yaml
tmp/karpenter-poc/bin/kubectl --kubeconfig tmp/karpenter-poc/kubeconfig \
  apply -f hack/karpenter-poc/nodepool.yaml
tmp/karpenter-poc/bin/kubectl --kubeconfig tmp/karpenter-poc/kubeconfig \
  wait --for=condition=OfferingsCurrent machineclass/azure-poc --timeout=180s
tmp/karpenter-poc/bin/kubectl --kubeconfig tmp/karpenter-poc/kubeconfig \
  wait --for=condition=Ready nodepool/azure-poc --timeout=180s
```

Do not edit or overwrite the old immutable ConfigMap, delete/recreate the Site
or MachineClass, or remove prior revisions needed by frozen allocations.
Applying the same resource names preserves their UIDs and configuration
contract. Actual offering availability/resources remain provider-discovered;
the same CPU/memory bounds do not prove current launch eligibility.

The host image is pinned to
`Canonical:ubuntu-24_04-lts:server:24.04.202610160`. The worker Site, class,
configuration, operation credential, and NodePool are named `azure-poc`.
Initial configuration version is explicitly pinned to `1`; when intentionally
changing the configuration template, inspect its generated version and pass
`--configuration-version` rather than assuming an old version contains new
content. Hard eviction is left at kubelet defaults; the capacity adapter must
advertise the corresponding overhead.

### Final shared controller prerequisites

The retained environment already has the accepted base components and current
shared provider-controller image installed, as recorded above. Do not rebuild
or reinstall them merely to repeat acceptance or change the Azure worker SKU.
The following build notes describe preparing a fresh installation only.

Rebuild the current worktree's `machine-ops-controller`, `machina`, and
`unbounded-operator` images with a separate final tag. The operator embeds the
new capacity CRDs; the baseline image does not. Keep the existing network
images and private net-node pull Secret in place. Do not rerun repository-wide
formatting to prepare these builds.

The operator stamps component image versions. The coordinator may need to alias
unchanged private network/token-refresher images from `karpenter-poc-base` to
the selected final tag, or supply matching explicit image references. Such
aliases do not mean those components gained lifecycle changes. Verify their
resolved digests and the private pull Secret after rollout.

Render the capacity controller with its explicit feature flags and separate
Workload Identity. Run this using the worktree-local Go toolchain/cache
environment already used for root builds:

```bash
go run ./hack/cmd/render-manifests \
  --templates-dir deploy/machine-ops \
  --output-dir tmp/karpenter-poc/machine-ops \
  --set Namespace=unbounded-system \
  --set ControllerImage=ubkarppoc1021c25d.azurecr.io/machine-ops-controller@sha256:0465176e5fc327590d9585be938ef27f292dca5123f6ade5287555d51fd66d16 \
  --set Provider=AzureVM \
  --set Site=azure-poc \
  --set EnableCapacity=true \
  --set CapacityConfigNamespace=unbounded-system \
  --set AllocationConsumer=karpenter.sh/v1/NodeClaim \
  --set WorkloadIdentityClientID=0873ab46-c634-4899-a81e-ba88dd079f36 \
  --set APIServerEndpoint=unbounded--unbounded-karp-p-44654a-d30gqh9z.hcp.eastus2.azmk8s.io:443
```

The federation subject is
`system:serviceaccount:unbounded-system:machine-ops-controller`. The controller
needs the machina component's `07-bootstrapper-rbac.yaml` bindings for bootstrap
group `system:bootstrappers:unbounded-agent-daemons`; verify they remain
installed during the final rollout.

The capacity-enabled render also retains the narrow
`kube-public/machine-ops-controller-capacity-cluster-metadata` Role and matching
RoleBinding. They grant this ServiceAccount only `get` on the named ConfigMap
`aks-cluster-metadata`; do not replace that permission with namespace-wide
ConfigMap access.

The Azure adapter lists Resource SKUs at subscription scope
(`internal/machineops/providers/azurevm/capacity_client.go:248`). Its bounded
SKU discovery needs `Microsoft.Compute/skus/read` at the subscription scope,
in addition to existing worker-resource-group, subnet, and worker-identity
permissions. There is no usage/quota API call in this discovery path. Use a
narrow read-only role instead of subscription Contributor; the coordinator
owns creating or assigning roles.

The coordinator granted only `Microsoft.Compute/skus/read` through dedicated
subscription role definition `9f7c7538-7903-441c-bf6f-4ceccf3801c2` and assignment
`e5e7b3e8-2ace-477b-8342-4b4b3628f00d`. These are outside the POC resource groups
and require the explicit final cleanup below.

After the final controller images and shared CRDs are deployed, the coordinator
applies the plugin CRDs/deployment and the rendered capacity inputs:

```bash
tmp/karpenter-poc/bin/kubectl --kubeconfig tmp/karpenter-poc/kubeconfig \
  apply --server-side -f tmp/karpenter-poc/karpenter.yaml

tmp/karpenter-poc/bin/kubectl --kubeconfig tmp/karpenter-poc/kubeconfig \
  apply --server-side -f tmp/karpenter-poc/capacity.yaml

tmp/karpenter-poc/bin/kubectl --kubeconfig tmp/karpenter-poc/kubeconfig \
  wait --for=condition=Ready machineclass/azure-poc --timeout=300s

tmp/karpenter-poc/bin/kubectl --kubeconfig tmp/karpenter-poc/kubeconfig \
  apply --server-side -f hack/karpenter-poc/nodepool.yaml
```

Server-side apply avoids storing large upstream CRDs in last-applied
annotations. The capacity controller deployment must use the external AKS API
endpoint and `--allocation-consumer=karpenter.sh/v1/NodeClaim`, with the
separate Workload Identity configured by its owner. It may need narrowly scoped
subscription-level permission for additional observations if the adapter changes;
verify its actual read actions rather than granting broad subscription Contributor.

Build the private plugin image without touching baseline images:

```bash
make -C plugins/karpenter image \
  CONTAINER_ENGINE=docker VERSION=karpenter-poc-v3 \
  IMAGE=ubkarppoc1021c25d.azurecr.io/unbounded-karpenter:karpenter-poc-v3
```

The coordinator pushes it using the local Docker authentication directory and
should rerender deployment with its returned image digest before acceptance.
Image pushes and cluster apply commands are coordinator operations, not side
effects of rendering, testing, or agent packaging.

## Finite live acceptance

Only the infrastructure coordinator runs this test. It changes resources in
the dedicated POC cluster. First deploy the final modified capacity controller,
Machina, agent artifacts, and Karpenter image. Configure a Ready MachineClass
and dynamic NodePool named `azure-poc`; use CPU limit `8` with the pinned
four-vCPU VM size to bound the POC to two workers. NodePool limits are
eventually consistent, not an Azure-enforced quota.

Start with zero POC VMs, NICs, disks, NodeClaims, and allocations. Use the
existing signed-in Azure CLI with `AZURE_CONFIG_DIR` unset and the local matching
kubectl. An optional custom config directory must already exist under the ignored
state directory; do not export an unauthenticated directory merely to run the
test. The demo Deployment starts at zero; the script scales it to one:

This is the exact successful shell 767 invocation from this worktree root.
`AZURE_CONFIG_DIR` was unset, with no extra flags or environment overrides:

```bash
python3 hack/karpenter-poc/acceptance.py \
  --kubeconfig tmp/karpenter-poc/kubeconfig \
  --kubectl tmp/karpenter-poc/bin/kubectl
```

The test has a bounded deadline and writes mode-0600 JSONL evidence under
`tmp/karpenter-poc`. It records only identifiers and safety observations, never
bootstrap tokens, Secrets, VM user data, or full configuration snapshots.
The default main-run deadline is 2400 seconds, polling interval 3 seconds, and
blocked-drain observation 30 seconds. Cleanup has its own 600-second deadline.

### Accepted live run

The successful evidence file is:

```text
tmp/karpenter-poc/acceptance-20261022T024231097511Z.jsonl
```

The coordinator's shell 767 exited 0. The log's terminal event is
`acceptance-succeeded` at `2026-10-22T02:47:30.330246+00:00`, after
`cleanup-confirmed`, not merely after a pod became Ready.

| Observation | UTC time on 2026-10-22 |
| --- | --- |
| Zero-worker precondition and unschedulable workload proven | 02:42:33 / 02:42:35 |
| Actual Azure VM present while allocation still provisioning | 02:42:52 |
| Every capacity/Karpenter controller pod replaced and Ready | 02:43:20 |
| Workload running on a Ready worker after restart | 02:45:31 |
| Restart identity recovery recorded | 02:45:33 |
| Thirty-second PDB-blocked drain proven | 02:46:07 |
| Original Azure VM absent | 02:46:36 |
| Original NIC and managed OS disk absent | 02:46:38 |
| Original NodeClaim and Node finalized | 02:46:54 |
| All workers, NICs, disks, claims, and allocations absent | 02:47:29 |
| Cleanup confirmed; acceptance succeeded | 02:47:30 |

The following identities remained unchanged across both controller restarts:

- Allocation UID: `50a4687a-6d59-4b4f-983c-13b7c353621d`.
- HostAllocate operation UID: `4d25f944-ae66-4de0-b831-bf1b9f9ffea7`.
- Azure native VM ID: `1e0b5976-5064-4db1-9146-1f96a6aa03e6`.
- ARM VM name: `ub-50a4687a6d594b4f983c13b7c353621d`, in the worker resource
  group above.
- Provider ID:
  `unbounded:///machineallocations/50a4687a-6d59-4b4f-983c-13b7c353621d`.

The VM's actual attached resources were
`ub-50a4687a6d594b4f983c13b7c353621d-nic-d9517bb7` and
`ub-50a4687a6d594b4f983c13b7c353621d-osdisk`. During the budget block the
allocation was Fenced but not Released, the workload was not deleting, and the
same native VM remained present. Removing only the PDB allowed Karpenter to
drain and authorize release. Final cleanup also safely released the proactive
replacement. No VM was manually deleted and no finalizer was bypassed.

The coordinator independently checked the retained zero-worker/controller-ready
state described at the start of this runbook. The JSONL is the primary local
acceptance evidence; the independent final check was reported by the coordinator,
not synthesized by these helpers.

Preserve the earlier `acceptance-*.jsonl` files and the matching red/green test
logs. They record the scheduling, attempt-loss, rootfs, and resource-conversion
failures that led to this successful run.

The required live observations are:

1. A worker-targeted pod is Pending from zero capacity.
2. Restart both capacity and Karpenter controllers while the first ARM VM
   exists but its allocation is still provisioning. Recover the same
   allocation UID, operation UID, ARM resource ID, and Azure VM ID.
3. Observe the pod Running and Ready on a Ready standalone worker with the
   exact allocation provider ID.
4. Delete its NodeClaim while a `minAvailable: 1` PodDisruptionBudget protects
   the sole workload pod. For at least 30 seconds, observe the Node terminating,
   allocation Fenced, lifecycle not Released, pod not deleting, and the same
   Azure VM still present.
5. Remove the budget blocker. Let Karpenter evict/drain and then authorize
   capacity release. Confirm the original VM, its NICs and managed disks,
   Node, and NodeClaim disappear.
   A replacement worker may temporarily use the second allowed slot.
6. Scale the workload to zero, safely delete remaining POC NodeClaims, and
   confirm zero Azure VMs, NICs, disks, allocations, and NodeClaims.

The script attempts the same safe zero-workload cleanup on failure. It never
deletes a VM directly, removes finalizers, or writes allocation lifecycle
itself. A missed provisioning restart window fails acceptance rather than
claiming recovery from a post-launch restart. Cleanup failures remain errors
and require investigation, not forced finalizer removal.

While waiting for the original worker to become Ready, each poll verifies the
captured allocation and NodeClaim UIDs and their active lifecycle. Confirmed
disappearance, replacement, termination, fencing, or release fails immediately
with `original attempt lost:` and enters the same safe cleanup path. The test
does not adopt a replacement launch or wait for an assumed fixed launch timeout.
A valid Pending attempt remains retryable; API errors retain their original
error instead of being interpreted as missing resources.

After success it leaves `workload.yaml` applied at replicas zero, with its PDB
restored, so the same test can be repeated. Do not delete the POC resource groups
until all allocations confirm release and the coordinator has reviewed cleanup.

### Manual reproduction

Prefer the finite helper above when collecting acceptance evidence. For a
manual run, work only in this dedicated cluster and define a shell-local
wrapper that always supplies the ignored kubeconfig:

```bash
k() {
  tmp/karpenter-poc/bin/kubectl \
    --kubeconfig tmp/karpenter-poc/kubeconfig "$@"
}

k apply -f hack/karpenter-poc/workload.yaml
k --namespace karpenter-poc scale deployment/worker-demo --replicas=1
k --namespace karpenter-poc get pods \
  --selector app.kubernetes.io/name=worker-demo --output=wide
```

1. Start at the same zero-worker precondition. Observe the demo pod Pending
   before a worker exists. Record the exact NodeClaim/allocation names and UIDs,
   allocate operation UID, provider ID, ARM VM ID, and native `vmId`.
2. While that VM exists but its allocation is not yet Allocated, restart both
   controllers and wait for rollout:

   ```bash
   k --namespace unbounded-system rollout restart \
     deployment/machine-ops-controller deployment/unbounded-karpenter
   k --namespace unbounded-system rollout status \
     deployment/machine-ops-controller --timeout=180s
   k --namespace unbounded-system rollout status \
     deployment/unbounded-karpenter --timeout=180s
   k --namespace karpenter-poc rollout status \
     deployment/worker-demo --timeout=900s
   ```

   Verify the original identities, Ready Node, workload readiness, and absence
   of duplicate allocations. Inspect only selected IDs/status, never VM
   customData, bootstrap tokens, Secrets, or full agent configuration.
3. Set `CLAIM`, `ALLOCATION`, `NODE`, and `VM_ID` to those captured identities.
   Capture actual NIC/disk IDs, not guessed names:

   ```bash
   az vm show \
     --subscription 44654aed-2753-4b88-9142-af7132933b6b \
     --ids "$VM_ID" \
     --query '{vmId:vmId,nics:networkProfile.networkInterfaces[].id,osDisk:storageProfile.osDisk.managedDisk.id}'

   k delete nodeclaim "$CLAIM" --wait=false
   ```

   Leave the demo PDB installed for at least 30 seconds. Repeatedly verify the
   original Node is terminating, allocation Fenced but not Released, original
   workload pod not deleting, and the same native VM present. Fencing alone
   must not release the VM. Stop and investigate any violated invariant.
4. Remove only the demo PDB, not the workload or a finalizer:

   ```bash
   k --namespace karpenter-poc delete poddisruptionbudget/worker-demo
   k wait --for=delete "nodeclaim/$CLAIM" --timeout=600s
   k wait --for=delete "node/$NODE" --timeout=600s
   ```

   Confirm the original VM and every captured NIC/disk are absent. A replacement
   can use the second worker slot; do not confuse its IDs with the retired VM.
5. Use cleanup-only below to scale the demo to zero, safely release remaining
   workers, and restore `workload.yaml` and its PDB.

To retry safe cleanup after a failed run without claiming acceptance:

```bash
python3 hack/karpenter-poc/acceptance.py \
  --kubeconfig tmp/karpenter-poc/kubeconfig \
  --kubectl tmp/karpenter-poc/bin/kubectl \
  --cleanup-only
```

This mode still checks the dedicated API endpoint and uses Karpenter drain
and release. It never bypasses workload budgets other than removing the
demo's own PDB. Investigate any remaining allocation that requires release
authorization rather than deleting its finalizer or Azure resources by hand.

### Qualification limits

The historical Azure-only run proves the bounded Karpenter/AzureVM POC, not the
full design's conformance profile. Its accepted configuration is Linux/amd64,
the pinned Ubuntu host and rootfs, AKS 1.35.8, Karpenter 1.14.2, one
`Standard_D4s_v3` on-demand offering
in `eastus2` zone 1, a single demo workload, and a NodePool CPU limit of 8.
The second worker slot permits a replacement; NodePool limits are eventually
consistent rather than an Azure-enforced VM quota.

CAS, existing-inventory reservation, and the count-managed `MachinePool` controller
remain deferred. That historical run is not managed AKS NAP, a multi-provider
proof, a scale/load test, or qualification of Spot, other zones/images/
architectures, all storage detach scenarios, repairs, drift policies, or every
orphan/late-join/outage fault.
The observed budget block and restart window are explicit live evidence, not a
claim that all conformance cases have been exercised.
The separately accepted [dual-cloud run](#accepted-dual-cloud-run) adds the
Azure v6/AWS pair, not full provider-independent capacity conformance.

## Renewal and local handoff preservation

Before repeating after a pause:

1. Check the private net-node token's expiry and rotate before approximately
   2026-11-21 using the procedure above. Keep replacement values in mode-0600
   files, reapply with the dedicated field manager, and prove a fresh worker
   image pull. Preserve the Secret attachment across operator reconciliation.
2. Refresh genuine price observations and rerender immutable inputs when needed;
   never replace an old observation timestamp with the current time without a
   new source observation. Preserve configuration-version/revision pinning.
3. Ensure normal Azure CLI authentication is still valid without `az account set`.
   The worker's Blob identity and controller's scoped identity grants must
   remain available. Managed Identity delivery needs no renewable SAS URL.
4. If the local admin kubeconfig must be refreshed, write only the ignored file:

   ```bash
   az aks get-credentials \
     --subscription 44654aed-2753-4b88-9142-af7132933b6b \
     --resource-group unbounded-karp-poc-1021-c25d \
     --name unbounded-karp-poc \
     --admin \
     --file "$PWD/tmp/karpenter-poc/kubeconfig" \
     --overwrite-existing
   chmod 600 tmp/karpenter-poc/kubeconfig
   ```

   Do not use the default kubeconfig path or switch the global context.

The accepted images were built from uncommitted work on
`ismille/provider-independent-autoscaling`, based on
`8728a2e4099cbf6a1f8f4b898c324ed343073c76`. The manifest's dirty-worktree flag
matters: that base commit alone does not contain the accepted implementation.
Preserve the worktree and immutable image/artifact digests; no commit or push
was made for this POC.

Keep all of the following private local handoff material:

- `kubeconfig`, SSH key pairs, `net-node-dockerconfig.json`, and any `docker/` or
  custom Azure authentication directory.
- `bin/`, `build/`, pinned `tools/`, and `base-src/`, including the baseline CLI
  binary used for installation.
- `artifacts/` with manifest, checksums, tarball, and packaged helper; both
  controller renders; `capacity.yaml`; and the source manifests.
- Every `acceptance-*.jsonl`, all validation/build logs including `logs/`, and
  the three nonsecret `live-*.json` regression captures.
- The private ACR images and content-addressed Blob until environment retirement.

Only recreatable caches are optional cleanup candidates:
`tmp/karpenter-poc/go-cache/`, `lint-cache/`, and completed contents of `go-tmp/`,
after confirming no build/test process is using them. Keep `go-mod/`, including
its boundary `go.mod`, for offline rebuilds. Do not delete the whole ignored
state directory or treat failed-attempt evidence as disposable. No files are
removed by these documentation updates.

## Final environment retirement

Safe worker cleanup is distinct from deleting the environment. Keep credentials
and permissions available until all allocations confirm release and the
coordinator verifies zero worker VMs, NICs, and disks. Preserve the JSONL
acceptance evidence without publishing Secrets or bootstrap data.

Deleting POC resource groups does **not** remove the subscription-scoped SKU
role assignment or custom role definition. After worker release is confirmed
and the POC will not be resumed, the coordinator removes these exact dedicated
resources:

```bash
az role assignment delete \
  --subscription 44654aed-2753-4b88-9142-af7132933b6b \
  --ids /subscriptions/44654aed-2753-4b88-9142-af7132933b6b/providers/Microsoft.Authorization/roleAssignments/e5e7b3e8-2ace-477b-8342-4b4b3628f00d

az role assignment list \
  --subscription 44654aed-2753-4b88-9142-af7132933b6b \
  --role 9f7c7538-7903-441c-bf6f-4ceccf3801c2 \
  --all \
  --query '[].id' \
  --output tsv
```

Only if the assignment list is empty, delete the dedicated role definition:

```bash
az role definition delete \
  --subscription 44654aed-2753-4b88-9142-af7132933b6b \
  --name 9f7c7538-7903-441c-bf6f-4ceccf3801c2
```

After reviewing and preserving evidence, the coordinator can retire the
dedicated environment. Delete the AKS resource first so Azure performs its
managed-node cleanup; do not manually delete its node resource group while AKS
still exists:

```bash
az aks delete \
  --subscription 44654aed-2753-4b88-9142-af7132933b6b \
  --resource-group unbounded-karp-poc-1021-c25d \
  --name unbounded-karp-poc \
  --yes

az group delete \
  --subscription 44654aed-2753-4b88-9142-af7132933b6b \
  --name unbounded-karp-poc-1021-c25d-workers \
  --yes

az group delete \
  --subscription 44654aed-2753-4b88-9142-af7132933b6b \
  --name unbounded-karp-poc-1021-c25d \
  --yes
```

These commands destroy the retained AKS, network, identities, registry, and
storage; they are optional retirement, not the repeatable zero-worker cleanup.
If the dedicated `unbounded-karp-poc-1021-c25d-nodes` resource group remains after
AKS deletion finishes, investigate the managed cleanup before explicitly
retiring that orphaned group. If retaining the registry instead, revoke the POC
token and remove its dedicated scope map when no longer used.
Remove local credential files only after cleanup and evidence review are
complete. No test or render helper performs these retirement operations.

## AWS extension with the frozen Karpenter plugin

**Status: bounded Azure/AWS live acceptance passed at
2026-10-22T14:41:02.519516Z.** The coordinator ran the finite proof after AKS
system-pool recovery and Azure v6 offering validation. The accepted implementation
and harness are frozen; no further code or image change is needed for this
handoff. This is not full provider, outage, or scale conformance.
The earlier Azure procedures and evidence remain the historical baseline.
Do not rerun their historical plugin/agent rebuild steps for this extension.

The same deployed Karpenter image remains:

```text
ubkarppoc1021c25d.azurecr.io/unbounded-karpenter@sha256:ab49c4f98fd9c2f6e1d9b6711dc63f33614f7770f0f00219cc3e64904fe6ce1b
```

`plugins/karpenter` is frozen. Its unchanged 27-file pre-extension hash inventory
is retained in
`tmp/karpenter-poc/aws/plugin-source-before.sha256`. Adding `AWSEC2` uses the
existing generic `MachineClass` and `MachineAllocation` contract; it does not
introduce an AWS NodeClass, provider-specific plugin fields, or a new provider
enum schema. The operator and frozen Karpenter remain unchanged. The coordinator
installed the same AWS-capable controller digest
`0465176e5fc327590d9585be938ef27f292dca5123f6ade5287555d51fd66d16` on both the
Azure and AWS deployments so both receive the approved shared operation fence.
Azure retains explicit `--provider=AzureVM` and its existing Workload Identity;
AWS retains its separate `--provider=AWSEC2` deployment and web identity.

### Dedicated infrastructure and immutable artifacts

All runtime inputs, policies, rendered manifests, and new evidence remain under
`tmp/karpenter-poc/aws`; use `TMPDIR="$PWD/tmp"` and explicit kubectl/kubeconfig
paths throughout. No helper changes the global Kubernetes context.

| Input | Approved value |
|---|---|
| Account / region / AZ | `891516228446` / `us-east-1` / `us-east-1a` (`use1-az2`) |
| VPC | `vpc-00ecf903dd4fc76d6`, `10.241.0.0/16` |
| Private worker subnet | `subnet-0d0ebf59b8827359d`, `10.241.1.0/24` |
| Worker security group | `sg-0d4949c3225b327b2`, self-SG ingress only |
| Private egress | NAT `nat-0021af5994ca16fdd`, EIP `98.82.143.0` |
| Site pods | `100.126.0.0/16` |
| Host / type | Canonical Ubuntu 24.04 `ami-02a9cdd4e98bc3c78`, `m6i.xlarge` |
| Capacity role | `unbounded-karp-poc-c25d-capacity` |
| Capacity inline policy | `unbounded-ec2-capacity` |
| Worker role and instance profile | `unbounded-karp-poc-c25d-worker` |
| Worker inline policy | `unbounded-private-agent` |
| Private bucket | `unbounded-karp-poc-891516228446-c25d` |
| Object version | `mwyEJNLmYQm_VAmMARzoyP9s.BR4Yxln` |

The object key is
`agent/32f4485b9706dafc46f089c7800470f0f31f30dab77898fb46447cc8cdd5a0c0/unbounded-agent-linux-amd64.tar.gz`.
It is the accepted 32,958,565-byte tarball with that SHA-256, not a rebuilt agent.
The rootfs remains
`ghcr.io/azure/agent-ubuntu2404@sha256:d72dd1b1d98d6b74250467fe28d3e31989a9f413b0355abedcbd1072e3653863`.
The bucket is versioned, BucketOwnerEnforced, AES256 encrypted, public-access
blocked, and TLS-only. Preserve infrastructure tag
`unbounded-poc-session=c25d8ea4-fa38-44fe-902c-f695dc631ea6`.

### IAM and private bootstrap

`render-aws-iam.py` produces policies without calling AWS. Supply the exact
OIDC provider ARN and `Url` returned by AWS, including the observed trailing
slash. The coordinator has verified that URL as:

```text
eastus2.oic.prod-aks.azure.com/70a036f6-8e4d-4615-bad6-149c02e7720d/f2a70f1e-c6b9-4521-be19-ca06823e1855/
```

For example, using those already verified values:

```bash
export TMPDIR="$PWD/tmp"
OIDC_URL=eastus2.oic.prod-aks.azure.com/70a036f6-8e4d-4615-bad6-149c02e7720d/f2a70f1e-c6b9-4521-be19-ca06823e1855/
python3 hack/karpenter-poc/render-aws-iam.py \
  --oidc-provider-arn "arn:aws:iam::891516228446:oidc-provider/$OIDC_URL" \
  --oidc-provider-url "$OIDC_URL" \
  --subnet-id subnet-0d0ebf59b8827359d \
  --security-group-ids sg-0d4949c3225b327b2 \
  --version-id mwyEJNLmYQm_VAmMARzoyP9s.BR4Yxln
```

The coordinator reviews and applies `capacity-trust.json`,
`capacity-policy.json`, `worker-trust.json`, and `worker-policy.json` from
`tmp/karpenter-poc/aws/iam`, creates the worker instance profile, and attaches
the worker role to it. `bucket-tls-policy.json` records the TLS-only bucket
policy already installed. Confirm the actual role/profile ARNs before applying
capacity configuration.

For this retained environment, the coordinator can execute the prepared
mode-0700 `tmp/karpenter-poc/aws/create-iam.sh` from the worktree root. It verifies
the approved account, creates/tag-scopes the worker role/profile and capacity
role, applies these reviewed policies, and retains JSON responses locally.
It deliberately fails on an existing role/profile name instead of replacing
trust. Inspect a partial failure rather than blindly rerunning or deleting IAM
resources. The integration coding agent only prepared and syntax-checked this
script; cloud creation remains the coordinator's action.

An offline complete-payload measurement is retained in
`tmp/karpenter-poc/aws/userdata-budget.json`: the actual bootstrap files,
shared installer, accepted rootfs, cluster public CA, and representative agent
configuration with a synthetic token rendered to **19,619 raw bytes / 7,391
gzip bytes**. This is not a real launch or a substitute for checking the
adapter's exact final payload. The raw result exceeds 16 KiB, so compression
and the adapter's final compressed-size guard are required.

Controller trust permits only audience `sts.amazonaws.com` and subject
`system:serviceaccount:unbounded-system:machine-ops-controller-aws`.
The projected AWS token is separate from the normal Kubernetes API token.
The adapter uses explicit web-identity credentials and validates the caller
account; static credentials, shared profiles, or controller IMDS are not
fallbacks. Never put an AWS role ARN in `WorkloadIdentityClientID`, which remains
Azure-specific.

The coordinator has created both roles and the worker instance profile and
verified STS `AssumeRoleWithWebIdentity` using a real AKS-issued token for the
actual AWS ServiceAccount and audience. This validates the exact trailing-slash
issuer/trust subject/audience combination, not EC2 provisioning or worker
acceptance. The temporary token was removed and no returned AWS credentials
were saved. Do not rerun the create-only IAM script now that those names exist.

The capacity policy permits the required EC2 describe calls, launch against only
the pinned image/subnet/SG, launch-time tagging, and tagged resource release.
`iam:PassRole` is limited to the worker role and EC2 service. Instance, ENI, and
EBS launch requests carry `unbounded-managed-by=unbounded-machine-ops` and an
allocation UID; no worker receives controller permissions.

The worker policy grants only `s3:GetObjectVersion` on the exact object and
version, not bucket listing, current-object reads, writes, or compute access.
Bootstrap installs Ubuntu-signed `python3-boto3` and `ca-certificates` with
bounded apt timeouts, then runs `aws-worker-artifacts.py` under a 300-second
timeout. It requires instance-profile credentials, IMDSv2 on the host, signed
regional HTTPS S3 requests, the exact returned version and expected bucket owner,
bounded content length, and the accepted SHA-256 before an atomic mode-0600
install. It logs SDK versions and sanitized outcomes, not SDK exceptions,
credentials, or artifact contents.

The adapter compresses the complete generic allocation cloud-config and rejects
compressed user data exceeding EC2's 16-KiB limit. Bootstrap script size alone
is not that limit. No presigned URL, public modified binary, static access key,
or bootstrap token appears in the provider ConfigMap or custom resources.

### Approved recovery boundary

The parent-approved protocol uses the same allocation-derived EC2 client token,
same subnet/AZ, and identical frozen launch parameters on an unrecorded Begin
replay. An exact client-token lookup adopts the owned instance when visible.
An already recorded instance is resumed, never replaced if missing or terminal.
No client-token retention duration is assumed.

After EC2 accepts an instance ID, allocation polling may wait up to 10 minutes
for that exact ID to become visible. Expiry fails the allocation while retaining
the instance identity for release observation; it is not permission to relaunch,
and is not an EC2 idempotency TTL. Unknown ownership or unrecorded live resources
fail closed. Final controller validation also covers fencing an allocation whose
previously recorded HostAllocate operation disappears, rather than recreating
that operation. These are required safety boundaries, not claims of complete
orphan/outage conformance.

Never delete a MachineOperation mid-flight: a missing/replaced recorded
HostAllocate operation now fences the attempt instead of authorizing
recreation. Do not upgrade the controller image during an ambiguous Begin
replay either; its embedded installer is part of user data, so changing the
image can change replay parameters and fail closed with
`IdempotentParameterMismatch` / `ProviderConflict`. The acceptance helper only
restarts existing Deployments without changing their images and never deletes
MachineOperations.

### Render and deploy only the AWS slice

`tmp/karpenter-poc/aws/capacity-inputs.json` has exactly two sections:

```json
{
  "provider": {
    "accountID": "891516228446",
    "region": "us-east-1",
    "availabilityZone": "us-east-1a",
    "instanceType": "m6i.xlarge",
    "subnetID": "subnet-0d0ebf59b8827359d",
    "securityGroupIDs": "sg-0d4949c3225b327b2",
    "imageID": "ami-02a9cdd4e98bc3c78",
    "instanceProfileARN": "arn:aws:iam::891516228446:instance-profile/unbounded-karp-poc-c25d-worker"
  },
  "artifact": {
    "bucket": "unbounded-karp-poc-891516228446-c25d",
    "key": "agent/32f4485b9706dafc46f089c7800470f0f31f30dab77898fb46447cc8cdd5a0c0/unbounded-agent-linux-amd64.tar.gz",
    "versionID": "mwyEJNLmYQm_VAmMARzoyP9s.BR4Yxln",
    "bucketOwner": "891516228446",
    "region": "us-east-1"
  }
}
```

All provider values are strings. Optional provider keys are `rootVolumeSizeGiB`,
`rootVolumeType`, `maxPods`, `memoryOverheadPercent`, `kubeReserved`, and
`systemReserved`; unknown keys fail. Defaults are a 64-GiB gp3 root, 110 pods,
kube-reserved `cpu=100m,memory=256Mi`, and system-reserved
`cpu=250m,memory=512Mi`. The renderer adds the real AWS Price List observation
USD 0.192/hour at `2026-10-22T05:41:57Z`; this is compute-only pricing, not EBS,
NAT, transfer, discounts, or other costs. It never invents freshness.

```bash
python3 hack/karpenter-poc/render-aws-capacity.py \
  --inputs tmp/karpenter-poc/aws/capacity-inputs.json

go run ./hack/cmd/render-manifests \
  --templates-dir deploy/machine-ops \
  --output-dir tmp/karpenter-poc/aws/machine-ops \
  --set Namespace=unbounded-system \
  --set ControllerName=machine-ops-controller-aws \
  --set ControllerImage="$AWS_MACHINE_OPS_IMAGE" \
  --set Provider=AWSEC2 \
  --set Site=aws-poc \
  --set EnableCapacity=true \
  --set CapacityConfigNamespace=unbounded-system \
  --set AllocationConsumer=karpenter.sh/v1/NodeClaim \
  --set AWSRoleARN=arn:aws:iam::891516228446:role/unbounded-karp-poc-c25d-capacity \
  --set AWSRegion=us-east-1 \
  --set APIServerEndpoint=unbounded--unbounded-karp-p-44654a-d30gqh9z.hcp.eastus2.azmk8s.io:443
```

Set `AWS_MACHINE_OPS_IMAGE` to the current shared provider-controller image
`ubkarppoc1021c25d.azurecr.io/machine-ops-controller@sha256:0465176e5fc327590d9585be938ef27f292dca5123f6ade5287555d51fd66d16`,
not the historical Azure-only digest or an unpinned tag. The renderer rejects
mixed Azure/AWS
identity and requires explicit AWS provider, region, Site, and a controller
name distinct from the default. Existing default/Azure selectors and identity
behavior are unchanged. The distinct name scopes Deployment, ServiceAccount,
RBAC names/subjects, and pod selectors; provider/Site scope the leader lease.
Retain the narrow named `aks-cluster-metadata` GET permission for AWS bootstrap
on AKS too.

After review, the coordinator applies only this new slice:

```bash
tmp/karpenter-poc/bin/kubectl --kubeconfig tmp/karpenter-poc/kubeconfig \
  apply -f hack/karpenter-poc/aws-site.yaml
tmp/karpenter-poc/bin/kubectl --kubeconfig tmp/karpenter-poc/kubeconfig \
  apply -f tmp/karpenter-poc/aws/machine-ops
tmp/karpenter-poc/bin/kubectl --kubeconfig tmp/karpenter-poc/kubeconfig \
  apply -f tmp/karpenter-poc/aws/capacity.yaml
tmp/karpenter-poc/bin/kubectl --kubeconfig tmp/karpenter-poc/kubeconfig \
  apply -f hack/karpenter-poc/aws-nodepool.yaml
```

The AWS Site explicitly disables Gantry from creation. Preserve
`poc-net-node-pull` and the existing `unbounded-net-node` ServiceAccount
attachment. The AWS NodePool's 8-CPU/32-GiB limit permits approximately two
`m6i.xlarge` workers for replacement headroom; Karpenter limits are eventually
consistent, not a hard cloud quota. Steady proof uses one Azure and one AWS
worker. No additional public ingress is required: AWS private hosts use NAT;
Azure WireGuard UDP `51820-51899` permits only Azure NAT `20.81.185.46/32`
and AWS NAT `98.82.143.0/32`.

### Accepted dual-cloud run

Attempt 2 exited **0**, with final event `dual-cloud-acceptance-succeeded` at
**2026-10-22T14:41:02.519516Z**. Durable private evidence:

- JSONL: `tmp/karpenter-poc/aws/dual-acceptance-20261022T143255723461Z.jsonl`.
- Console: `tmp/karpenter-poc/aws/dual-acceptance-run2.log`.
- Frozen plugin source inventory:
  `tmp/karpenter-poc/aws/plugin-source-before.sha256` (all 27 files unchanged).

The same retained AKS cluster simultaneously hosted one Azure
`Standard_D4s_v6` worker in `eastus2-1` and one AWS `m6i.xlarge` worker in
`us-east-1a`, with both original workloads Ready. Both machine-ops Deployments
and all Karpenter replicas were replaced from each allocation's still-pending
restart window, without replacing its consumer, allocation, HostAllocate
operation, or native instance. The JSONL retains the before/after controller
Pod UIDs, full ARM IDs/ARNs, actual child identities, and operation references.

| Captured identity | Azure | AWS |
| --- | --- | --- |
| MachineAllocation UID | `2aaf0cec-e431-42d6-858c-83930921c653` | `af73a689-18c1-4720-b3a1-2a111a12d560` |
| HostAllocate operation UID | `49d1a456-ebba-43ba-9c2a-6b129e827974` | `daec0875-e907-49c1-9266-b49022408f30` |
| Native VM identity | `vmId: 9fe6eb30-45ab-43b1-8153-3529e410a225` | `i-076ac397fb6bfe18b` |
| Actual NIC / ENI | `ub-2aaf0cece43142d6858c83930921c653-nic-89bfcc7b` | `eni-08f01cd289fe6b2b2` |
| Actual managed OS disk / EBS | `ub-2aaf0cece43142d6858c83930921c653-osdisk` | `vol-08753ad785994daea` |

All milestone times below are UTC on `2026-10-22`:

| Evidence milestone | Azure | AWS |
| --- | --- | --- |
| Pending allocation and actual children captured before restarts | `14:33:15.103674` | `14:35:07.506001` |
| Original worker/workload and restart recovery proven | `14:34:54.921853` | `14:36:18.845600` |
| PDB blocked fenced drain for at least 30 seconds | `14:37:13.060445` | `14:38:39.636643` |
| Normal release of native instance, actual children, Node, and claim proven | `14:38:07.052422` | `14:39:47.044047` |

Bidirectional Pod-IP HTTP and Service-DNS HTTP, plus remote `exec` and `logs`
on each worker, passed before `concurrent-dual-ready-proven` at
`14:36:40.707198Z`. Cleanup confirmed zero active scoped instances, actual children,
NodeClaims, allocations, worker Nodes, and workload replicas at
`14:41:02.519456Z`. The coordinator's additional independent final checks
confirmed zero Machines/MachineOperations, an empty Azure worker resource
group, no active AWS subnet instances, the captured EC2 instance terminated,
and its ENI/EBS absent. Terminated EC2 metadata is not a live worker.

This is a bounded same-cluster Karpenter proof using the exact two offerings,
frozen plugin image/source, shared controller image, and accepted agent/rootfs
recorded above. It is **not** full provider/scale conformance, exhaustive
ambiguous-replay/orphan/outage coverage, Spot or other zone/image/architecture
qualification, every storage/detach/repair/drift scenario, existing-inventory
reservation, or CAS/`MachinePool` integration.

#### Retention, billing, and retirement

Keep the failed first attempt, historical Azure evidence, applied immutable
manifests, IAM inputs, and both run logs under the existing private handoff
directory. Before repetition, recheck Azure/AWS CLI authentication, the scoped
IAM/OIDC roles and instance profile, and the pinned private S3 object version.
The existing private net-node pull-token renewal requirement applies to both
providers.

Worker cleanup did not retire the environment. Retained AKS/system
capacity, ACR/Blob, AWS NAT/EIP, storage, and transfer can continue to incur
charges. The observed Azure USD 0.202/hour and AWS USD 0.192/hour prices are
compute-only observations, not total retained-environment costs.
Keep the [renewal and preservation instructions](#renewal-and-local-handoff-preservation)
and [retirement safeguards](#final-environment-retirement). The Azure retirement
commands do not remove AWS resources; any AWS IAM/OIDC, versioned S3, NAT/EIP,
or VPC retirement requires a separate coordinator decision after verified
worker release. No retirement is authorized or performed by this handoff.

### Finite dual-cloud acceptance and safe cleanup

Run only after final provider/controller validation and both controllers/classes/pools are
Ready. Keep the original `acceptance.py` unchanged for Azure-only repetition.
The finite helper requires AWS CLI in the approved account and the same
existing signed-in Azure CLI as the original helper (`AZURE_CONFIG_DIR` may
remain unset):

```bash
python3 hack/karpenter-poc/dual-cloud-acceptance.py \
  --kubeconfig tmp/karpenter-poc/kubeconfig \
  --kubectl tmp/karpenter-poc/bin/kubectl \
  --vpc-id vpc-00ecf903dd4fc76d6 \
  --subnet-id subnet-0d0ebf59b8827359d
```

It discovers the running private net-node digest for its nonprivileged Python
HTTP probes; `--probe-image` can explicitly supply that same repository digest.
Probe pods use only generic NodePool selectors, a dedicated no-token
ServiceAccount, and the existing private pull Secret.

The finite sequence requires:

1. Zero scoped workers, actual children, Nodes, claims, allocations, and probe
   pods; unchanged plugin hash inventory and running image digest.
2. Each pending workload causes a real native VM while its allocation remains
   pending. A bounded metadata wait captures the actual attached NIC/ENI and
   disk/EBS identities before any controller restart. Incomplete creating
   metadata may be retried, but invalid ready topology, a missed pending
   allocation window, or changed ownership fails. Both machine-ops controllers
   and the same Karpenter controller are restarted while that allocation is
   still pending, preserving the original consumer/allocation/operation/native
   VM. Original-attempt loss or premature fencing fails immediately during
   waits.
3. Both original workers and probe workloads remain concurrently Ready.
   Actual `kubectl exec` and `logs` must work on each, including AWS through
   the existing gateway-host Konnectivity path. Bidirectional Pod-IP HTTP and
   Service-DNS HTTP must return the other pod's exact per-run identity.
4. Each provider's captured claim is deleted while its PDB blocks drain for at
   least 30 seconds. The captured allocation must be fenced without lifecycle
   release or HostRelease, while the original VM and protected pod remain.
5. Removing that PDB permits normal release. The actual native VM, every
   observed attached ENI/NIC and EBS volume/disk, Node, and claim must disappear.
6. Safe cleanup scales only owned probes to zero, removes their PDBs, requests
   normal claim deletion, and confirms zero scoped cloud resources, Nodes,
   claims, and allocations. It restores reusable zero-replica probe manifests
   and verifies the frozen plugin again.

Evidence is mode-0600 JSONL at
`tmp/karpenter-poc/aws/dual-acceptance-<UTC>.jsonl`. The final
`dual-cloud-acceptance-succeeded` event is written only if proof and cleanup
both pass. Failure and cleanup failure both remain nonzero exits. No AWS/Azure
resource deletion, finalizer bypass, or allocation lifecycle patch is performed
by acceptance. AWS reads use exact native IDs or dedicated VPC/subnet/class/
ownership filters, not whole-account inventory scans in polling loops.

The first dual-cloud attempt on `2026-10-22` failed at `14:17:40Z` when the
harness applied its ready-only child-metadata assertion immediately after
observing a native Azure VM with a still-pending allocation. Its original ARM
provisioning state was not recorded, so creating-state behavior is reproduced
by an offline real-call-chain regression, not claimed as captured live state.
Normal cleanup confirmed zero scoped workers and children at `14:18:46Z`.
Evidence is `tmp/karpenter-poc/aws/dual-acceptance-20261022T141723870190Z.jsonl`.
Its console log is `tmp/karpenter-poc/aws/dual-acceptance-run1.log`.
The bounded metadata wait was a harness-only correction. Attempt 2 subsequently
passed the bounded proof recorded above; keep the first failed attempt as
diagnostic evidence, not successful acceptance evidence.

After interruption, the coordinator can explicitly request the same normal
Kubernetes cleanup, without rerunning proof:

```bash
python3 hack/karpenter-poc/dual-cloud-acceptance.py \
  --kubeconfig tmp/karpenter-poc/kubeconfig \
  --kubectl tmp/karpenter-poc/bin/kubectl \
  --vpc-id vpc-00ecf903dd4fc76d6 \
  --subnet-id subnet-0d0ebf59b8827359d \
  --cleanup-only
```

An incomplete cleanup remains an error; retain permissions, resources, and
evidence for diagnosis instead of directly deleting cloud children or stripping
finalizers. Worker cleanup retains AKS, both provider deployments, IAM/OIDC,
S3 artifact version, NAT/EIP/VPC, and the private registry. Environment
retirement remains a separate coordinator decision.
