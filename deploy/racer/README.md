# Standalone Racer controller deployment

These templates deploy the Go Racer controller and its HTTPS Service. They do
not install a dataplane or SDK, or enable operator integration. The retained
workload builder and dataplane configuration templates support separate work;
the controller does not apply dataplane workloads.

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
2. Install `create-restriction.yaml`, including its binding, and verify that the
   policy is active with no type-check errors **before** applying RBAC or any
   controller workload. RBAC cannot restrict Secret and ConfigMap creation by
   name; this deny policy supplies that restriction.
3. Ensure the serving TLS Secret exists. Apply `config.yaml` and, only for the
   new UUID with no existing version state, `installation.yaml`.
4. Apply `rbac.yaml`, then `controller.yaml` (Deployment and Service).
5. Verify that the installation marker is consumed and immutable and that the
   replicas pass `/readyz`. The default Service uses HTTPS port 8443. Liveness
   and readiness use port 8081; metrics use port 8080.

Do not apply the entire rendered directory blindly. The bootstrap-trust and
dataplane-config files are not needed for this controller-only deployment.
There is no initialization Job or `initialize` command.

## Keep permanent state

Fresh manifests use `initialization_protocol: staged-v1`. Each replica runs the
startup guard: it creates or validates a version candidate, then consumes and
freezes the marker with that candidate's Kubernetes UID in `version_uid`.
Serving still requires validated local state.

After initialization, retain `state: consumed` in declarative configuration and
preserve **all** marker data, including `initialization_protocol` and
`version_uid`. Do not reapply the fresh manifest or replace the marker with a
newly rendered consumed template that omits those fields.

Retain `racer-installation`, `racer-version`, and `racer-credentials`, including
their UID bindings, claims, counters, and private credential material. Protect
them from deletion, replacement, renaming, and deployment-tool pruning. Never
commit credentials to source control.

Missing, replaced, or corrupt committed state is not permission to regenerate
it. Do not reset a marker, add the staged protocol to an existing installation,
or recreate counters under the same UUID. Recovery requires consistent durable
state or an explicit new-cluster rebootstrap with a new UUID.
