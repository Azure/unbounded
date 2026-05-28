# Daemon Controller Authn/z

The host-side `unbounded-agent` daemon controller needs a Kubernetes client
credential for reconciling local Unbounded machine state. The credential must
authenticate to kube-apiserver, keep the same baseline `Node` permissions as a
kubelet, and carry an additional Unbounded group for Machina CR access.

Today `cmd/agent/internal/daemon` builds that client from the applied agent
config, which contains the API server URL, the cluster CA bundle, and the kubelet
TLS bootstrap token. The daemon controller uses that bootstrap-token client to
ensure the local `Machine` CR exists, publish `AgentUpgrade` startup status, run
the shared daemon controller, watch local `Node` deletion events, and update
local `Machine` status after repave-related reconciliation. This works today
because the bootstrap token can authenticate with an extra authorization group
that RBAC uses for Machina CR access. That extra group is part of the bootstrap
token's authenticated user info, not a durable daemon identity.

This is useful during early bootstrap, but it is not acceptable for a long-running
daemon controller. The TLS bootstrap token is short-lived and is intended for
joining, not ongoing reconciliation. The extra bootstrap-token group also will
not be propagated into the built-in kubelet client certificate issued by Kubernetes;
the built-in kubelet client signer accepts only the kubelet-shaped `system:nodes`
organization. The daemon controller therefore needs a rotated client certificate
whose authorization shape matches its two roles: kubelet-like own-node access and
Unbounded CR access.

## Goals

- Give the daemon controller a client certificate for kube-apiserver
  authentication and authorization.
- Restrict daemon-controller `Node` permissions to the same baseline used by the
  kubelet: own-Node read, own-Node main-resource create/update/patch subject to
  NodeRestriction, and own-Node status update/patch.
- Include an extra group in the certificate so RBAC can grant Machina CR access
  to the daemon controller.
- Keep lifecycle operations that require privileged `Node` mutation outside the
  daemon controller, including updating node labels or taints and deleting the
  `Node` object to trigger repave.
- Keep daemon-controller behavior limited to local reconciliation and status
  reporting.
- Ensure the daemon implementation does not rely on own-Node main-resource
  updates, even though the credential has kubelet-style own-Node update/patch
  capability subject to NodeRestriction.

## Non-goals

- Define the separate lifecycle controller or operator workflow that performs
  cordon, drain, privileged node changes, or Node deletion.
- Change the kubelet client certificate signer or kubelet TLS bootstrap behavior.
- Define a general-purpose node identity model for components other than the
  `unbounded-agent` daemon controller.
- Define revocation or CA rotation for daemon-controller certificates.

## Proposed Certificate Identity

A normal kubelet client certificate authenticates as a Kubernetes node identity:

```text
CN = system:node:<nodeName>
O  = system:nodes
```

Kubernetes recognizes this shape as a node identity. The built-in
[Node authorizer](https://github.com/kubernetes/kubernetes/blob/master/plugin/pkg/auth/authorizer/node/node_authorizer.go)
then limits node requests to kubelet-style node-scoped behavior. For `Node`
objects, that includes own-Node reads, own-Node main-resource create/update/patch,
and own-Node status update/patch. The
[NodeRestriction admission plugin](https://github.com/kubernetes/kubernetes/blob/master/plugin/pkg/admission/noderestriction/admission.go)
adds object-level restrictions for node requests, such as preventing a node from
modifying another node or changing protected parts of its own `Node` object. This
identity cannot delete Nodes through the Node authorizer path.

The daemon controller should get the same baseline `Node` behavior, so its custom
client certificate is intentionally kubelet-shaped and adds one configurable
Unbounded group:

```text
CN = system:node:<nodeName>
O  = system:nodes
O  = <daemonGroup>
```

`<daemonGroup>` is the extra group used by RBAC for Machina CR access. The
default value is `unbounded-agent-daemons`, but deployments may choose a different
non-privileged group name. The daemon, CSR approver, signer configuration when a
custom signer is used, and RBAC bindings must all use the same value.

Including `system:nodes` causes the built-in Node authorizer and NodeRestriction
admission plugin to apply the same own-node restrictions used for kubelets. This
is how the daemon controller satisfies the baseline `Node` requirement. This is
kubelet-style Node access, not a custom status-only permission model.

Including `<daemonGroup>` allows RBAC to grant additional Unbounded CR
permissions without granting those permissions to all kubelets.

This certificate cannot use the built-in kubelet client signer. The built-in
Kubernetes kubelet client signer validates kubelet client CSRs as exactly
`O=system:nodes` in the
[kubelet client CSR validation helper](https://github.com/kubernetes/kubernetes/blob/master/pkg/apis/certificates/helpers.go)
and does not preserve bootstrap-token extra groups into the issued kubelet client
certificate. By default, this design uses a strict custom CSR approver with the
built-in `kubernetes.io/kube-apiserver-client` signer. The generic API client
signer can issue kube-apiserver client certificates using the cluster's existing
client certificate signing configuration, so using it avoids duplicating signing
logic when the cluster supports that signer.

## Authorization Model

Authorization is split by resource family:

- Built-in Node authorizer and NodeRestriction admission handle `Node` access for
  the `system:node:<nodeName>` / `system:nodes` part of the identity.
- Kubernetes RBAC grants Machina CR access through the configured daemon group.

The Unbounded group must not grant additional `Node` mutation permissions. Node
labels, taints, privileged annotations, and Node deletion remain outside the
daemon controller.

## Machina RBAC

RBAC grants Machina-related permissions to the extra group, not directly to
`system:nodes`. This example uses the default daemon group:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: unbounded-agent-daemon-controller
rules:
- apiGroups: ["unbounded-cloud.io"]
  resources: ["machines"]
  verbs: ["get", "create", "update", "patch"]
- apiGroups: ["unbounded-cloud.io"]
  resources: ["machines/status"]
  verbs: ["get", "update", "patch"]
- apiGroups: ["unbounded-cloud.io"]
  resources: ["machineoperations"]
  verbs: ["get", "list", "watch"]
- apiGroups: ["unbounded-cloud.io"]
  resources: ["machineoperations/status"]
  verbs: ["get", "update", "patch"]
- apiGroups: ["unbounded-cloud.io"]
  resources: ["machineconfigurationversions"]
  verbs: ["get", "list", "watch"]
```

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: unbounded-agent-daemon-controller
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: unbounded-agent-daemon-controller
subjects:
- kind: Group
  apiGroup: rbac.authorization.k8s.io
  name: unbounded-agent-daemons
```

If a deployment configures a different daemon group, replace
`unbounded-agent-daemons` in the `ClusterRoleBinding` subject with that value.

## CSR Approver And Signer Setup

The default setup uses a custom approver and the built-in
`kubernetes.io/kube-apiserver-client` signer.

The approver is the security boundary. It is responsible for ensuring only the
local daemon controller can receive the kubelet-shaped certificate with the extra
group. It must be stricter than the signer: the signer proves the certificate is a
valid client certificate, while the approver proves this requester may receive
this specific daemon-controller identity.

Using `kubernetes.io/kube-apiserver-client` means the signer enforces only generic
API-client certificate constraints. All daemon-specific constraints are enforced
by the custom approver before the CSR is approved.

The approver validates:

- `spec.signerName` is `kubernetes.io/kube-apiserver-client` by default, or the
  configured daemon-controller signer for deployments that use a custom signer.
- The requested common name is `system:node:<expected-node-name>`.
- The requested organizations are exactly `system:nodes` and
  the configured daemon group, for example `unbounded-agent-daemons` by default.
- The requested usages are limited to client authentication.
- The request has no subject alternative names unless a future design explicitly
  allows them.
- The PKCS#10 CSR in `spec.request` does not request CA privileges, unexpected
  subject fields, unsupported key usages, or unexpected extensions.
- For initial issuance, the bootstrap requester is allowed to claim the requested
  node name.
- Renewal requests cannot change the node name or group set.
- A normal kubelet certificate that has `system:nodes` but does not have the
  configured Unbounded daemon group cannot renew into a daemon-controller
  certificate.

For production use, the approver must validate the corresponding node and
resource binding. In particular, initial issuance must prove that the bootstrap
token submitting the CSR is allowed to claim the requested Node and the expected
Machine CR. Renewal must prove that the existing certificate is already the same
daemon-controller identity and still maps to the same node/resource binding.

When the kube-controller-manager CSR signing controller is configured for
`kubernetes.io/kube-apiserver-client`, approved CSRs are signed using the
cluster's client certificate signing configuration, and the resulting
certificates are intended to be honored as client certificates by kube-apiserver.
This avoids implementing and operating a separate signer for the default
deployment.

The approver needs RBAC permission to approve CSRs for
`kubernetes.io/kube-apiserver-client`. That signer can issue general API client
certificates, not only daemon-controller certificates. For that reason, the
approval permission should be granted only to the daemon-controller CSR approver
service account. Other users or controllers should not receive this approval
permission unless they have their own independent policy and validation path.

Concretely, the approver service account needs permission to update CSR approval
subresources and to approve only the signer it handles:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: daemon-controller-csr-approver
rules:
- apiGroups: ["certificates.k8s.io"]
  resources: ["certificatesigningrequests"]
  verbs: ["get", "list", "watch"]
- apiGroups: ["certificates.k8s.io"]
  resources: ["certificatesigningrequests/approval"]
  verbs: ["update"]
- apiGroups: ["certificates.k8s.io"]
  resources: ["signers"]
  resourceNames: ["kubernetes.io/kube-apiserver-client"]
  verbs: ["approve"]
```

Bind this role only to the daemon-controller CSR approver service account. Do not
bind it to broad groups such as `system:authenticated`, `system:masters`, or the
daemon group itself.

Deployments may provide their own approver and signer instead. A custom approver
can enforce stricter node/resource validation, and a custom signer can use a
different CA or signer name if the cluster policy requires it. In that case, the
daemon, approver, signer, and RBAC configuration must agree on the signer name and
daemon group. See [Appendix: Integration Examples](#appendix-integration-examples)
for example wiring in Machina/Unbounded and AKS Flex Node environments.

Security-sensitive or multi-tenant deployments should prefer a custom signer name
so approval and signing RBAC can be scoped to the daemon-controller certificate
contract instead of the generic API client signer.

### Built-in Signer Requirements

The default model requires kube-controller-manager CSR signing to be enabled for
`kubernetes.io/kube-apiserver-client` and configured with a client signing CA/key
whose issued certificates are trusted by kube-apiserver client certificate
authentication.

If the cluster disables kube-controller-manager CSR signing, does not configure
the client signing key, or does not trust the resulting CA for API client
authentication, this default signer model cannot be used.

## Certificate Issuing Process

The daemon-controller certificate can be issued in two cases: initial issuance
from a bootstrap token and renewal from an existing daemon-controller
certificate. Both paths produce the same certificate shape:

```text
CN = system:node:<nodeName>
O  = system:nodes
O  = <daemonGroup>
```

### Initial Issuance From Bootstrap Token

Initial issuance starts before the daemon has a durable client certificate. The
daemon submits a CSR using the kubelet TLS bootstrap token from the applied agent
config.

The approval rule is: the bootstrap token submitting the CSR must be allowed to
claim the requested node name. The CSR subject is not trusted by itself. Without
this check, any valid bootstrap token could request a daemon-controller
certificate for another node.

The initial issuance flow is:

1. The daemon builds a bootstrap REST client from the API server URL, cluster CA,
   and kubelet TLS bootstrap token in the applied agent config.
2. The daemon creates a private key and CSR for
   `kubernetes.io/kube-apiserver-client` with `CN=system:node:<nodeName>`,
   `O=system:nodes`, and the configured Unbounded daemon group.
3. The custom approver validates the CSR shape, signer, usages, and requested
   expiration.
4. The custom approver resolves the bootstrap token identity and verifies that the
   token is authorized to claim the requested Machine and Node name.
5. The built-in API client signer signs the approved CSR with a CA trusted by the
   kube-apiserver client certificate authenticator.
6. The daemon stores the issued certificate and private key on the host with
   permissions limited to the daemon user.
7. The daemon uses the issued certificate for daemon-controller API requests.

The token-to-node check should use controller-owned state, such as Machine data,
provisioning state, or bootstrap-token metadata. The exact source can vary by
provisioning path, but the security property is fixed: the approver must prove
that this bootstrap token is allowed to request this node name.

### Renewal From Existing Certificate

Renewal starts after the daemon already has a valid daemon-controller
certificate. Renewal does not use the bootstrap token unless the existing
certificate has expired and the daemon must fall back to the initial issuance
path.

The renewal flow is:

1. The daemon periodically checks the stored certificate lifetime.
2. When the certificate enters its renewal window, the daemon creates a new
   private key and CSR for the same signer, common name, organizations, and
   usages.
3. The daemon submits the CSR using the current daemon-controller certificate.
4. The custom approver validates that the authenticated requester is the same
   `system:node:<nodeName>` identity and still carries the configured Unbounded
   daemon group.
5. The custom approver rejects renewal if the requested node name, group set,
   signer, or usages changed.
6. The signer issues a replacement certificate.
7. The daemon atomically replaces the stored certificate and uses it for new API
   connections.

If renewal fails while the current certificate remains valid, the daemon should
retry with backoff and expose metrics. If the certificate expires and no valid
bootstrap path exists, the daemon must fail closed and stop reconciling API-backed
operations until a valid credential is available.

## Request Flow

Certificate issuance:

```mermaid
flowchart TD
    BootstrapToken["Bootstrap token"]
    InitialCSR["Initial CSR for system:node:<nodeName> plus daemon group"]
    ExistingCert["Existing daemon-controller certificate"]
    RenewalCSR["Renewal CSR for same identity and groups"]
    Approver["Custom approver validates CSR shape and requester"]
    ClaimCheck["Initial issuance: verify bootstrap token may claim node"]
    RenewalCheck["Renewal: verify requester matches requested identity"]
    Signer["Built-in API client signer issues daemon-controller certificate"]
    StoredCert["Daemon stores cert and key on host"]

    BootstrapToken --> InitialCSR
    ExistingCert --> RenewalCSR
    InitialCSR --> Approver
    RenewalCSR --> Approver
    Approver --> ClaimCheck
    Approver --> RenewalCheck
    ClaimCheck --> Signer
    RenewalCheck --> Signer
    Signer --> StoredCert
```

Authentication and authorization:

```mermaid
flowchart TD
    Daemon["Daemon controller uses daemon-controller certificate"]
    APIServer["kube-apiserver authenticates client certificate"]
    Identity["username: system:node:<nodeName><br/>groups: system:nodes, <daemonGroup>"]
    NodeRequest["Kubelet-style own-Node request"]
    MachinaRequest["Machina CR request"]
    NodeAuthz["Node authorizer and NodeRestriction enforce own-node access"]
    RBAC["RBAC grants Machina CR access through daemon group"]

    Daemon --> APIServer
    APIServer --> Identity
    Identity --> NodeRequest
    Identity --> MachinaRequest
    NodeRequest --> NodeAuthz
    MachinaRequest --> RBAC
```

## Machina CR Scoping

RBAC bound to the configured daemon group is group-wide. The built-in Node
authorizer does not scope Unbounded CRDs to the local node or Machine. This
design assumes daemon controllers are trusted to reconcile only their local
`Machine` and `MachineOperation` objects by implementation logic and
controller-owned conventions.

If hard per-node or per-Machine enforcement is required later, it must be added
separately. Options include deterministic `resourceNames`-based RBAC, admission
checks for create/update/patch, or a custom authorizer.

## Node Permissions

The daemon controller receives kubelet-like baseline `Node` permissions from the
`system:node:<nodeName>` and `system:nodes` parts of its certificate. It may rely
on the built-in Kubernetes node path for own-Node reads and own-Node status
updates. The same credential can also attempt kubelet-style own-Node
main-resource create/update/patch, subject to NodeRestriction. The daemon
implementation will not rely on main-resource `Node` updates.

The configured daemon group must not grant any additional `Node`
permissions.

The daemon controller implementation must not perform lifecycle `Node` mutations
such as:

- Update `Node` labels.
- Update `Node` taints.
- Update privileged `Node` annotations.
- Delete `Node` objects.

Those operations must be performed by a separate controller, operator workflow,
or future lifecycle identity that is explicitly authorized for that purpose.

Today the daemon controller only watches the local `Node` for deletion and reacts
by reconciling repave. It does not issue the `Node` deletion itself.

The daemon should not assume that a normal shared informer list/watch over Nodes
will work with this identity. The Node authorizer does not allow a node identity
to read all Nodes. The implementation should use a single-object watch if the
client path can express one, or fall back to periodic `GET` of its own `Node`.

## Alternatives Considered

One alternative is a completely separate daemon-controller identity, for example:

```text
CN = unbounded-agent:<nodeName>
O  = unbounded-agent-daemons
```

The group shown here is only an example. If a deployment uses a different daemon
group, the same concern applies to that configured group.

That has cleaner audit separation from kubelet identities, but it no longer uses
the built-in Node authorizer and NodeRestriction admission behavior. To preserve
the same own-node safety guarantees, Unbounded would need to replicate that logic
with a custom CSR approver, ValidatingAdmissionPolicy rules, and an authorization
webhook for dynamic own-node checks. The authorization webhook is required to
avoid creating one RBAC binding per node identity. That is more complicated than
using a kubelet-shaped identity and letting Kubernetes enforce the baseline Node
permissions.

Another alternative is granting Machina CR permissions to `system:nodes`. That is
not acceptable because it grants those permissions to all kubelets, not only
Unbounded daemon controllers.

## Implementation Requirements And Limitations

The custom certificate includes `system:nodes`, so requests appear as
`system:node:<nodeName>` in server-side audit logs. The extra group records that
the request also belongs to the configured daemon group, but the username is still
a node-shaped identity.

The default signer is `kubernetes.io/kube-apiserver-client`, so this model depends
on the cluster's kube-controller-manager CSR signing configuration. Deployments
that choose a custom signer must ensure the custom signer issues certificates
from a CA trusted by the kube-apiserver client certificate authenticator.

The bootstrap-token-to-node claim check is the critical approval decision. The
approver must have controller-owned state that maps the bootstrap token to the
expected Machine and Node name. Until that mapping is available and reliable, the
approver cannot safely issue daemon-controller certificates.

The daemon needs a concrete storage path for the custom certificate and key. The
path should be on the host, not inside the nspawn machine, because the host-side
daemon is the certificate consumer.

Renewal timing, retry backoff, and metrics names still need implementation-level
definition. The daemon must expose enough state to detect renewal failures before
the current certificate expires.

There is no online revocation mechanism for these daemon-controller certificates
in this design. If a certificate should stop working before expiry, enforcement
depends on authorization or admission denying the identity, or on client CA
rotation.

Accepted risk: the daemon-controller certificate includes `system:nodes`, so it
receives the same baseline Node authorizer behavior as a kubelet. This includes
own-Node main-resource update/patch permissions subject to NodeRestriction, not
only Node/status updates. The design accepts this because the daemon is node-local
and the desired behavior is to inherit Kubernetes' existing kubelet node-scoping
model rather than replicate it with a custom authorizer or ValidatingAdmissionPolicy.
Compromise of this certificate should be treated similarly to compromise of a
kubelet client credential for that node, plus the additional Machina CR
permissions granted to the configured daemon group.

## Appendix: Integration Examples

### Machina / Unbounded

In the default Machina/Unbounded deployment, the daemon group is
`unbounded-agent-daemons` and the signer is
`kubernetes.io/kube-apiserver-client`.

The CSR approver is included as part of the Machina controller deployment. This
keeps approval decisions close to the controller-owned Machine, bootstrap token,
and site state used to validate daemon-controller certificate requests.

The bootstrap token should authenticate with a bootstrap-scoped group that allows
the approver to identify requests for daemon-controller certificates, for example:

```text
system:bootstrappers:unbounded-agent-daemons
```

Unbounded should issue bootstrap token Secrets with controller-owned binding data,
including the site name the token is allowed to join. The token Secret can carry
that binding through labels or annotations owned by Unbounded, for example the
site label used by the bootstrap flow.

The approver validates the bootstrap requester, the CSR shape, and the requested
node identity. For initial issuance, the approver resolves the bootstrap token
Secret from the authenticated `system:bootstrap:<tokenID>` requester and verifies
that the requested node belongs to a valid Unbounded site for that token. If the
token is not bound to a valid site, or the requested node does not match the
token's site binding, the approver rejects the CSR.

When a `Machine` exists, the approver can also verify that the token is bound to
the expected `Machine` and `Node` through controller-owned state, such as the
Machine's bootstrap token reference and node reference.

Machina RBAC grants CR access to the configured daemon group. The group binding
must include the same group that appears in the issued certificate:

```yaml
subjects:
- kind: Group
  apiGroup: rbac.authorization.k8s.io
  name: unbounded-agent-daemons
```

This gives daemon controllers access to Machina resources without granting those
permissions to all kubelets in `system:nodes`.

### AKS Flex Node

In an AKS Flex Node style environment, the same certificate shape can be used, but
the deployment may choose a different daemon group and a different approver or
signer implementation.

The AKS resource provider should run a managed CSR approver for this flow. That
approver validates that the bootstrap token and requested daemon-controller
certificate come from an expected Flex agent node ARM resource before approving
the CSR. In other words, the AKS RP owns the platform-specific ARM
resource-to-node binding check instead of relying on Machina site or Machine
state.

The integration contract is:

- The daemon requests `CN=system:node:<nodeName>`.
- The certificate includes `O=system:nodes` so Kubernetes applies Node authorizer
  and NodeRestriction behavior.
- The certificate includes the configured daemon group so RBAC can grant the
  platform-specific CR permissions required by the daemon controller.
- The managed AKS RP approver verifies the platform's node/resource binding before
  approving the CSR.

The daemon group value should be treated as deployment configuration. For example,
AKS Flex Node may use a platform-owned group name instead of
`unbounded-agent-daemons`, as long as the daemon, approver, signer configuration,
and RBAC bindings use the same value.
