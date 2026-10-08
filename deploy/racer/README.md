# Racer controller operator manifests

These embedded manifests are operator inputs, not a standalone installation.
The operator supplies the controller image, namespace, permanent cluster UUID,
serving TLS, and public bootstrap trust. The ClusterCache CRD is installed by
operator bootstrap from the main API package.

Any ClusterCache activates installation, without requiring a Site. The operator
stages a non-startable marker, commits its UID in an immutable claim, and promotes
that marker with a resource-version check. Controller startup owns version and
credential initialization. Missing or replaced committed state fails closed;
restore consistent state instead of resetting claims or counters.

Removing all caches retains controller resources and durable state. Only existing
serving TLS and its public trust are maintained while inactive. Workloads and
missing credentials are not recreated. Serving CA rotation retains public roots
and cross-signed chains for overlap; private keys from old CAs are not retained.

The Deployment has three replicas, zero unavailable during rollout, and a PDB
requiring two available replicas. Each replica gets both its normal Kubernetes
API token and a separate bound token for controller replication. Only the leaf
key, serving chain, and public CA bundle are mounted from the serving Secret.

Admission policies must be installed before controller RBAC bindings and startup.
They restrict Node changes to three Racer annotations and restrict runtime
ConfigMap/Secret writes by name and type. The operator orders these dependencies.

No dataplane image, workload, service account, migration, or NetworkPolicy is
managed here. Read-only DaemonSet, Pod, and service account discovery and the
dataplane identity settings remain because the controller authenticates external
workloads. Provision those workloads separately.
