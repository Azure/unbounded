# Racer control-plane availability

The managed two-replica Deployment distinguishes a warm replica from the active
Service backend. `/readyz` requires production listeners, the replica-proof
listener, and installed, unexpired production and proof certificates. Standbys
run these transports before election, but subscription/enrollment handlers and
the trust-proof accept loop reject traffic until leadership starts. Existing TLS,
Pod authentication, and durable PKI fencing remain in force.

The Service additionally selects `racer.unbounded-cloud.io/serving-leader=true`.
A process removes its old routing hint before starting its transports. The elected
leader clears predecessor hints and publishes its own after certificate and
listener initialization. Routing writes use resource-version CAS; the predecessor
sweep also changes a boot annotation on Pods without a hint, preventing an
in-flight predecessor publication from restoring a stale route. The final Pod
read precedes the durable fence check. Cancellation rejects requests even while
endpoint updates propagate; shutdown also attempts to remove the local hint.

Rolling updates use one surge and one unavailable replica, with ten seconds of
continuous readiness before availability credit. Healthy standbys let updates
finish; failed successors cannot remove the last available old replica. One
unavailable also permits migration from the previous leader-only readiness, whose
two-replica ReplicaSet reports only one available Pod. A PDB with `minAvailable: 1`
counts both leaders and warm standbys. It protects voluntary eviction, not
Deployment deletions or involuntary failure. It may allow leader eviction, followed
by election of the retained standby; this is recoverable availability, not
uninterrupted serving.

Soft hostname and zone spread constraints include all revisions. They prefer
failure-domain separation while allowing a surge on two nodes or scheduling during
degraded operation. They cannot guarantee separation when capacity is constrained.

## Limitations and validation

The first upgrade from an older binary has a routing compatibility gap: the
operator installs the new Service selector before an old leader can publish the
new hint. Requests resume after a new replica becomes leader. Avoid claiming a
zero-downtime upgrade across this boundary. Subsequent upgrades can also have an
election and endpoint-propagation gap when the leader is replaced.

Tests exercise readiness, certificate expiry, missing proof and failed production
listeners, request gating on followers and cancellation, stale hint cleanup,
delayed predecessor CAS rejection, placement/PDB resources and RBAC. A Deployment
budget model covers both one-ready legacy and two-ready warm replicas, with healthy
and failed successors. This is not a live Kubernetes Deployment-controller test;
full scheduling, eviction and endpoint propagation need cluster-level validation.
