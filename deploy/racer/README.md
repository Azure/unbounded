# Standalone Racer controller deployment

These templates deploy the Go Racer controller and its HTTPS Service. They do
not install a dataplane or SDK, or enable operator integration. Workload fixtures
under `testdata/` are for tests only and are not rendered for installation.

## Prerequisites

- Kubernetes 1.32+ with `ServiceAccountTokenPodNodeInfo` (stable and always
  enabled from 1.32). Bootstrap and keyring bearer authentication require
  `authentication.kubernetes.io/node-name` and `authentication.kubernetes.io/node-uid`
  in TokenReview results for Pod-bound ServiceAccount tokens. Requests without
  either extra are rejected. See Kubernetes
  [Pod-bound token metadata](https://kubernetes.io/docs/reference/access-authn-authz/service-accounts-admin/#additional-metadata-in-pod-bound-tokens).
- Support for `admissionregistration.k8s.io/v1` `ValidatingAdmissionPolicy` and
  its binding, and permission to install the policy, CRDs, and RBAC.
- An existing namespace, default `unbounded-system`, and a controller image
  available to its nodes. Use a fixed image tag or digest.
- A permanent cluster UUID. Use a new UUID for each new installation.
- A serving TLS Secret named `racer-controller-tls` in that namespace, supplied
  and maintained separately. It must contain:
  - `tls.crt`: the serving certificate chain, with a DNS SAN matching
    `racer-controller.<namespace>.svc`.
  - `tls.key`: the matching leaf private key.
  - `ca-bundle.crt`: PEM public CA certificates that validate the serving chain,
    including retained roots needed during rotation. A lone `ca.crt` key does
    not satisfy the template.

The Pod mounts only those three keys. The public bundle is mounted as
`/etc/racer/tls/ca.crt` for replica verification. Do not mount a CA private key or
private rotation state. Serving TLS is separate from the controller-owned
`racer-credentials` Secret; the controller does not provision serving TLS.

## Render and install

From the repository root, render a **new installation only**:

```sh
timeout --signal=TERM --kill-after=10s 300s make racer-manifests \
  RACER_NAMESPACE=unbounded-system \
  RACER_CLUSTER_ID='<new-permanent-uuid>' \
  RACER_INITIALIZATION_STATE=fresh \
  RACER_CONTROLLER_IMAGE='<registry>/racer-controller:<tag>'
```

Replace the placeholders first. Output is in `deploy/racer/rendered/`.
Rendering does not contact a cluster. Build and test commands are in the
[component README](../../cmd/racer-controller/README.md).

Review the output, then install in this order using your deployment tool:

1. Install `crd/` and wait for the CRDs to be established.
2. Install `create-restriction.yaml` and `node-restriction.yaml`, including both
   bindings, and verify that the policies are active with no type-check errors
   **before** applying RBAC or any controller workload. RBAC cannot restrict
   Secret and ConfigMap creation by name or limit Node patches to specific fields.
   These fail-closed deny policies supply those restrictions.
3. Ensure the serving TLS Secret exists. Apply `config.yaml`. Only for a new UUID
   with no existing version state, create `installation.yaml` once. Do not apply
   it over an existing marker.
4. Apply `rbac.yaml`, then `controller.yaml` (Deployment and Service) and
   `controller-pdb.yaml`.
5. Verify that the installation marker is consumed and immutable and that the
   replicas pass `/readyz`. The default Service uses HTTPS port 8443. Liveness
   and readiness use port 8081; metrics use port 8080.

Do not apply the entire rendered directory blindly. There is no initialization
Job or `initialize` command. Bootstrap trust and dataplane configuration must be
provisioned separately and are not included in the controller render.

## Keep permanent state

Fresh manifests use `initialization_protocol: staged-v1`. Each replica runs the
startup guard: it creates or validates a version candidate, then consumes and
freezes the marker with that candidate's Kubernetes UID in `version_uid`.
Serving still requires validated local state.

After initialization, remove `RACER_INITIALIZATION_STATE=fresh` from render inputs
and render again. Default rendering omits the marker entirely, including when
the state input is `consumed`. Preserve the actual immutable marker with **all**
its data and UID, including `initialization_protocol` and `version_uid`. Do not
reapply the fresh manifest or generate a replacement consumed marker. Exclude
the finalized marker from GitOps pruning and replacement.

Retain `racer-installation`, `racer-version`, and `racer-credentials`, including
their UID bindings, claims, counters, and private credential material. Protect
them from deletion, replacement, renaming, and deployment-tool pruning. Never
commit credentials to source control.

Missing, replaced, or corrupt committed state is not permission to regenerate
it. Do not reset a marker, add the staged protocol to an existing installation,
or recreate counters under the same UUID. Recovery requires consistent durable
state or an explicit new-cluster rebootstrap with a new UUID.

## Availability and network access

The Deployment has three replicas, zero unavailable during rolling updates, and
one surge. The PodDisruptionBudget requires two available replicas for voluntary
evictions. Preferred anti-affinity spreads replicas across hosts when possible
without blocking small clusters; it does not guarantee separate hosts or protect
against node failure. Check placement and capacity before maintenance.

Each controller runs as UID 65532 with RuntimeDefault seccomp, all capabilities
dropped, privilege escalation disabled, and a read-only root filesystem. Defaults
request 100m CPU and 128Mi memory and limit each replica to 2 CPUs and 1Gi memory.
Size these resources for the installation's topology and request load.

`RACER_HANDSHAKE_TIMEOUT` defaults to `5s`. The `HandshakeTimeout` template input
can override it with a positive duration, independently of request and long-poll
limits. Limit HTTPS ingress (TCP 8443) to control clients and controller replicas.
Replicas need direct Pod-to-Pod HTTPS as well as Service access. Allow kubelet
probes on TCP 8081, authorized monitoring on TCP 8080, DNS, and Kubernetes API
access. No universal NetworkPolicy is shipped; select rules for your CNI and
client locations. Host-network and external traffic may need host or upstream
firewall rules, not just Pod selectors.

## Atomic credentials and configuration

The controller owns one `racer-credentials` Secret containing private
`issuer.json`, public-root/cache-key `bundle.json`, and `rotation.json`. Only
the validated bundle is delivered through the control API; workloads never mount
this Secret. Rotation updates all three entries in one resource-version CAS.
Only an authoritative postwrite reread can install serving trust.

The staged protocol creates an installation-bound credentials candidate, then
commits its claim and UID together on the version ConfigMap. Uncommitted material
cannot serve. Missing or replaced committed credentials never authorize
regeneration. There is no migration reader for former split Secrets.

Secret read/update/patch permissions name only the credentials Secret. The Secret
informer uses the required name field selector. Secret creation is namespace-wide
in RBAC, so install the fail-closed admission policy before the RoleBinding.

The Node policy scopes updates to `racer-controller` in the configured namespace.
It permits adding, changing, or removing only these annotations:

- `racer.unbounded-cloud.io/enrolled-shares`
- `racer.unbounded-cloud.io/enrolled-rdma-nics`
- `racer.unbounded-cloud.io/last-admitted-member`

All other Node fields must stay unchanged, except API-server field ownership
bookkeeping (`metadata.managedFields`). Node creation and deletion are not granted
by RBAC. The policy does not restrict other accounts, including node agents.
Install its binding before the ClusterRoleBinding and retain both policies while
the controller has write access.

`InstallationConfigMapName`, `VersionConfigMapName`, and `CredentialsSecretName`
template inputs set runtime configuration, RBAC, and admission policy names.
Never rename durable state in an existing installation. `RACER_DAEMONSET_NAME`
selects the single external workload for live-UID discovery and authorization;
`RACER_DATAPLANE_SERVICE_ACCOUNT` selects its account, and `RACER_PEER_PORT` sets
the published endpoint port. These settings do not create workloads. Workload
image, host networking, bootstrap trust, and diagnostics settings are not
controller configuration.

Bundle generation is a publication version, not a rotation count. Catalog
admission, preparation, activation, and root retirement consume versions only
when state changes. Activation removes replaced symmetric keys immediately;
issuer roots overlap for their retention period. Existing admitted cache UIDs
take priority over new ones when the bounded catalog is full.
