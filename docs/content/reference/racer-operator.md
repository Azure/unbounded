---
title: "Racer Operator Configuration"
weight: 7
description: "Control Racer controller management by unbounded-operator."
---

The operator installs the `clustercaches.racer.unbounded-cloud.io` CRD at startup.
When a `ClusterCache` exists, it sets up the Racer controller in the operator's
namespace, including controller RBAC, configuration, admission policies, serving
TLS, a Service, and a PodDisruptionBudget. No Site is required. The controller
image uses the operator's image registry and version.

This integration does not deploy or manage Racer dataplane workloads. It does
not adopt an existing standalone controller installation. Keep the installation
claim, marker, version state, and credentials intact; deleting them is not a
supported way to reset an installation.

## Enable or disable management

`ENABLE_RACER` defaults to `true`. Set it to `false` in
`unbounded-operator-config` and restart the operator to stop Racer management:

```sh
kubectl -n unbounded-system patch configmap unbounded-operator-config \
  --type merge -p '{"data":{"ENABLE_RACER":"false"}}'
kubectl -n unbounded-system rollout restart deployment/unbounded-operator
```

Use your operator namespace if it is not `unbounded-system`. Empty or invalid
boolean values prevent operator startup. Disabling management leaves existing
resources running and keeps the ClusterCache CRD installed. It does not uninstall
Racer or delete cached data.

Disabling management also stops serving TLS renewal. Existing certificates keep
their expiry dates: serving certificates last 14 days and the CA lasts 28 days
from issuance, not from when management is disabled. TLS connections fail once
the serving certificate expires. Re-enable management before expiry to avoid
this outage. If the CA expires, re-enabling management cannot renew it; restore
valid serving TLS state before reconciliation can continue.

For rendered manifests, use `--set EnableRacer=false` with the manifest renderer,
or `UNBOUNDED_OPERATOR_ENABLE_RACER=false` with `make unbounded-operator-manifests`.
The rendered configuration hash rolls the operator when this setting changes.

With management enabled, removing the last ClusterCache retains the installation
and maintains its established serving TLS, but stops workload repair. It does not
delete the controller or reset identity.

## Controller workload overrides

[Workload overrides](../workload-overrides/) support `component: racer` with
`kind: Deployment`. Omit `sites`: Racer is a cluster singleton. Overrides apply
only when the component plans the controller Deployment, not to its RBAC,
credentials, or external dataplane workloads.
