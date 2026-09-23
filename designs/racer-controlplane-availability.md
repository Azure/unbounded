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

Before narrowing an existing legacy Service, the operator probes each managed
replica's live `/readyz?verbose` contract. Only the old `leader-tls` check permits
an operator-installed routing hint. Both old leaders and old standbys receive
the hint: their old readiness contract still selects only the elected leader,
including a leadership change during migration. Generic readiness and image tags
are never leadership evidence. Unknown or unreachable processes block migration
without changing the old Service. Pod patches are optimistic and are dependencies
of the Service apply; failures retain the old selector and block the Deployment
update. New warm replicas remain unready until the Service has leader-only
selection, and clear inherited hints before startup. Leader replacement can still
have an election and endpoint-propagation gap.

Shutdown removes a hint only when the routing boot annotation belongs to the
exiting process. Each predecessor-sweep patch uses a Pod snapshot read before a
durable fence check. A new leader changes all candidate resource versions before
publishing, so an in-flight predecessor sweep conflicts instead of removing the
new route. There are no retries that reuse old leadership with freshly read Pods.

Tests exercise readiness, certificate expiry, missing proof and failed production
listeners, request gating on followers and cancellation, stale hint cleanup,
delayed predecessor CAS rejection, placement/PDB resources and RBAC. A Deployment
budget model covers both one-ready legacy and two-ready warm replicas, with healthy
and failed successors. This is not a live Kubernetes Deployment-controller test;
full scheduling, eviction and endpoint propagation need cluster-level validation.
