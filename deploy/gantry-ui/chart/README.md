# Gantry dashboard chart

This standalone, on-demand chart installs one dashboard replica on a Linux system
node. It does not install Gantry, publish images, configure Azure identities, or
create image-pull Secrets. The release namespace contains its demo workloads and
must be dedicated to this demo: helper ownership labels and the `gantry-reclaim`
Job name are shared by all dashboards in a namespace. Use separate namespaces
and distinct nodes for separate installations.

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
