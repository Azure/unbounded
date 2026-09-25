# Racer controller implementation status

Phases 1-4 implement configuration, bounded codecs, membership/catalog calculation,
one-shot initialization, durable publication CAS, issuer/shared-key rotation, and
certificate issuance. Token authentication, TLS serving, and managed workload
construction remain fail-closed, so this is not yet an operational HTTPS service.
The `initialize` command performs Kubernetes writes; normal startup validates
existing durable state. Constructors only wire dependencies; they open no files
or listeners and start no goroutines. Reserved HTTP handlers return 503.

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
version ConfigMap (including its one-way credential initialization claim), shared
keyring Secret, and controller issuer Secret are durable controller state, alongside
the permanent installation ConfigMap, normal leader Lease, and managed workload.
See the design's Phase 3 initialization protocol and Phase 4 recovery/handoff
sections before provisioning an installation. Lost established state never
authorizes automatic reinitialization.

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
