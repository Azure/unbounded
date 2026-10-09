# Racer controller manifests

The operator installs the Racer controller when a `ClusterCache` exists. These
manifests define the controller resources, not the external dataplane workloads.

## Retention after the last cache is deleted

Deleting the last `ClusterCache` does not uninstall Racer. The operator
intentionally retains the healthy controller Deployment, PodDisruptionBudget,
and RBAC resources, along with the installation's durable state.

While no caches exist, the operator continues maintaining established serving TLS
and its published trust. It does not repair or recreate the controller workload.
Workload repair resumes when a `ClusterCache` exists again. Missing or damaged
durable state still requires recovery; deleting caches does not reset it.

Retention does not bypass admission safety checks. If a required admission guard
is missing or has drifted, the operator can revoke the controller's RoleBinding
and ClusterRoleBinding even while no caches exist.
