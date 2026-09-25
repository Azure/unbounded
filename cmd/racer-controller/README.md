# Racer controller scaffold

This is a compiling API scaffold, not an operational controller. `LoadConfig`,
reconciliation, persistence, issuance, TLS setup, codecs, and publication admission
return `ErrUnimplemented`. The executable exits unsuccessfully before Kubernetes
access. Constructors only wire dependencies; they open no files or listeners and
start no goroutines. Reserved HTTP handlers return 503, never fake authentication.

The [control API](../racer-dataplane/CONTROL_API.md) is shared with the Rust
dataplane. The [design](../../designs/racer-control-plane.md) describes intended
behavior and implementation order.

## Structure

- `api/racer/v1alpha1`: cluster-scoped ClusterCache resource and generated CRD.
- `internal/racer`: three ordinary controller-runtime reconcilers, manager,
  publication state, token bootstrap, certificate issuance, and mTLS server.
- `internal/racer/wire`: shared contract declarations and bounded codec boundaries.
- `deploy/racer`: controller/RBAC/config templates. Workload reconciliation owns
  the future dataplane DaemonSet.

There is no Kubernetes abstraction, custom queue/leader-election framework,
per-node Secret, enrollment ledger, or goal-state checkpoint store. Only the
version ConfigMap, shared keyring Secret, and controller issuer Secret are durable
controller state, alongside the normal leader Lease and managed workload.

## Checks

```sh
make racer-generate
make racer-controller-build
make racer-test
make racer-manifests RACER_CLUSTER_ID=<permanent-cluster-uuid>
```

Run `make fmt` before committing. Generated deepcopy/CRD files are regenerated,
never hand-edited. Behavioral and interoperability tests accompany implementation
of each boundary; scaffold tests verify actual composition and fail-closed entry.

Manifests declare the intended deployment but the scaffold cannot become ready.
Deployment must supply the namespace, `racer-controller-tls` serving Secret, and
`racer-bootstrap-trust` ConfigMap with `ca.crt`. The serving certificate must cover
the configured service DNS name. These public trust/server TLS inputs are separate
from the controller-managed node issuer. No sample private keys are shipped.
