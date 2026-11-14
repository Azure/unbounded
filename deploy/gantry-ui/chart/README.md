# Gantry dashboard chart

This standalone, on-demand chart installs one dashboard replica on a Linux system
node. It does not install Gantry, publish images, configure Azure identities, or
create image-pull Secrets. The release namespace contains its demo workloads and
must be dedicated to this demo: helper ownership labels and the `gantry-reclaim`
Job name are shared by all dashboards in a namespace. Use separate namespaces
and distinct nodes for separate installations.

## Full setup for an existing AKS Flex cluster

The repository's deployment script can configure the Azure prerequisites, build
and publish the dashboard, install this chart with image generation enabled, and
verify the complete generation/push/pull workflow:

```sh
bash hack/scripts/gantry-ui-deploy.sh \
  --cluster-state tmp/aks-flex/my-flex-cluster \
  --acr-name myregistry \
  --image ghcr.io/myorg/gantry-ui \
  --engine podman \
  --build-dns 1.1.1.1
```

Run from the repository root. The state directory must come from an existing
`aks-flex-cluster.sh` deployment made with a compatible custom `--plugin-binary`.
Retain its cached installer, checksum in the saved intent, and kubeconfig. The
script validates that the selected context matches that AKS resource. It targets
the `flexpool` workers and requires a Linux system node matching `--platform`
(default `linux/amd64`).

Requirements are Azure CLI authentication, GHCR push authentication through the
selected engine, kubectl, Helm (or the repository's `bin/helm`), git, jq, python3,
curl, and openssl. Azure permissions must allow AKS workload-identity setup,
managed identities and federation, ACR role assignments, image imports, and the
existing managed Flex ACR-authentication setup. `--build-dns` is optional and
affects only Podman build containers; use a DNS server permitted by your network.

The destinations and identities are intentionally separate:

- The dashboard/helper executable image is built from this checkout, published
  to GHCR, and installed by its registry digest. It must be publicly pullable.
  If the GHCR package is private, the script stops before Azure setup changes:
  make that package Public in GitHub and rerun. It never publishes generated
  images to GHCR or transfers your GHCR credentials into the cluster.
- The dashboard gets a dedicated read-only ACR workload identity. A separate
  builder identity gets ACR read/push permissions. Existing identities are
  reused only when their ownership tags and federated subjects match.
  Both legacy ACR RBAC and repository ABAC modes are supported.
- A BusyBox runtime base is selected for the requested Linux architecture and
  imported into `gantry-demo/runtime` in your ACR by its single-platform digest.
  The chart pins that digest; a multiarch index is not used as the runtime base.
- UI-generated images are pushed into your ACR's `gantry-demo/generated`
  repository. Managed ACR authentication applies to current and future Flex
  workers. Configuring it may restart Flex kubelets without draining nodes.

After installation, the script opens a loopback-only local tunnel, verifies the
dashboard's ACR catalog access, and asks the actual UI API to generate one 1 MB
payload layer. It checks the resulting ACR digest, builder Job completion, and
the generated DaemonSet's availability on every matching Flex worker. This
small test image and workload remain in ACR and the demo namespace for viewing.
Verification is for ordinary OCI images; it does not enable Gantry Artifact
Streaming on the cluster. Streaming workloads require compatible workers with
streaming already enabled; see the
[Artifact Streaming configuration](../../../docs/content/guides/gantry.md#4-optional-enable-acr-artifact-streaming).
Existing non-streaming Flex hosts require a separately planned NBD/nspawn
migration; dashboard setup does not prepare those hosts.

Full setup also enables the **Reclaim disk space** button and its dedicated
Kubernetes account/RBAC. No cleanup is performed during setup or verification.
Clicking the button requires typing `DELETE ALL DEMOS`; it removes this
dashboard's demo workloads and reclaims unused node images, preserving ACR
images and active or uncertain runtime leases. Use `--no-reclaimer` to leave
this privileged capability disabled. The manual chart defaults remain unchanged.

The script prints the dashboard's local URL and keeps the tunnel running until
Ctrl-C. This closes only the tunnel, not the installed dashboard. Use
`--no-port-forward` to close the verification tunnel and exit instead; the script
prints a command to reopen it. `--yes` skips the Azure-change confirmation.

Generated values and setup ownership are kept under
`tmp/gantry-ui/<cluster>/<namespace>/<release>/`. Reruns reuse matching Azure
identities and grants, publish a new dashboard image, upgrade the Helm release,
and verify a new small generation/pull workload. They do not delete existing
demos or reclaim disks. Concurrent runs for the same setup are rejected.

## Manual chart installation

Prerequisites:

- Gantry agents in the existing `gantryNamespace` (default `unbounded-system`).
- An existing public-cloud ACR containing the demo images and appropriate worker
  node pull access.
- Azure workload identity webhook and a dashboard managed identity with ACR
  catalog/read access. Create a federated credential for the release's generated
  service account; `helm template` or installation NOTES show its name/subject.
- A published digest-pinned dashboard image, public or accessible using existing
  Secrets. The chart never retrieves registry credentials.

Install with an actual image digest and dashboard identity:

```sh
helm upgrade --install dashboard deploy/gantry-ui/chart \
  --namespace gantry-demo --create-namespace \
  --set registry=example.azurecr.io \
  --set image.digest=sha256:<64-lowercase-hex-digits> \
  --set azure.clientID=<dashboard-managed-identity-client-id>
```

Use the port-forward command printed in NOTES to open `http://127.0.0.1:8088`.
The fixed loopback listener uses `hostNetwork` to reach agent Pod IPs.
No Service or Ingress is created. The default selector requires
`kubernetes.azure.com/mode=system` and Linux; configure `nodeSelector` to pin a
specific node if needed. Multiple dashboards on the same node conflict on port
8088. `Recreate` prevents the release's own rolling update from conflicting.

Both helpers are disabled by default:

- Enable image generation using `builder.enabled=true`,
  `builder.azure.clientID`, and `builder.base` set to a digest-pinned runtime
  image in `registry`. Configure the builder's generated service account as a
  separate federated subject with ACR push/pull access. `builder.repository`
  defaults to `gantry-demo/generated`; `builder.concurrency` defaults to 1.
- Set `reclaimer.enabled=true` only when privileged host disk reclamation is
  intended. This creates a separate reclaimer account with node-list access and
  demo namespace Job/Pod/DaemonSet permissions, including DaemonSet deletion.
  The dashboard can delete only the fixed `gantry-reclaim` Job for retries.

Helpers use exactly the dashboard image repository and digest.
`imagePullSecrets` accepts Kubernetes `{name: existing-secret}` references for
the dashboard. Independently set `helperImagePullSecret` to one existing Secret
name for helper Jobs and reclaimer node workers. These Secrets must exist in the
release namespace. Azure workload identity authorizes application ACR calls;
it does not authenticate kubelet image pulls.

The dashboard explicitly selects `WorkloadIdentityCredential` through
`AZURE_TOKEN_CREDENTIALS`, preventing host-network IMDS fallback. It runs as
UID/GID 65532 with a read-only filesystem, no capabilities, and loopback probes.

Render tests follow the repository's Go test pattern and need Helm on PATH:

```sh
PATH="$PWD/bin:$PATH" go test ./deploy/gantry-ui/chart/tests
```
