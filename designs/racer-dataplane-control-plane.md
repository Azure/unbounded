# Racer control plane in the dataplane

## Scope

Run Racer with one `racer-dataplane` DaemonSet. Remove the Go controller binary,
image, Deployment, Service, and its operator bootstrap and PKI code. Keep
ClusterCache, the SDK, origins, sidecars, and the existing data path.

The operator installs static resources. It does not compute membership, create
encryption keys, issue certificates, rotate trust, or choose control nodes.

## Roles

Every pod runs the same Rust binary and is a goal-state client. Some pods claim
control seats using `racer-seat-0` through `racer-seat-15` Leases. A node holds at
most one seat. One seat also holds the `racer-primary` Lease.

The primary observes Nodes, managed dataplane Pods, ClusterCaches, and seats. It
computes membership and the catalog, reconciles keys and trust roots, and commits
state before publishing it. Other seats serve the same committed state. Clients
discover seats through the Kubernetes API, spread connections across seats, and
try another seat when a connection fails.

Seat election must work before enrollment or the first publication. Requiring
an already-enrolled node to win the first seat would deadlock startup. A holder
is not advertised as ready until it can serve authenticated, committed state.

## Durable state and fencing

The operator creates an empty Opaque Secret named `racer-state`. Its UID is the
Racer cluster ID. Runtime initialization atomically commits the initial state
and an initialization marker. An initialized Secret with missing or corrupt
data is not an empty installation.

The Secret holds publication counters and hashes, keyring generations and keys,
rotation schedules, retired trust roots, and the primary's fencing term. A full
publication can be much larger than a Kubernetes object, so it is not stored in
the Secret.

The primary's term binds the primary Lease UID, its transition number, and the
holder identity. Acquiring the Lease alone does not authorize publication: the
primary must first commit its term with a resourceVersion compare-and-swap.
Every subsequent commit checks that term. An old writer must stop on a lost
term, not reload and overwrite its successor's state. Transition numbers restart
when the Lease is recreated, so they are compared only within one Lease UID. A
term may take over state stamped under another UID only when its own UID is the
live Lease UID, read after the state; a writer whose Lease is gone is fenced.

Publication sequence, membership version, and keyring generation never decrease.
The same version never names different content. Only committed content is
served. If failover reconstructs different content, it must commit a new
version. Repeated confirmations of unchanged content do not create new
versions. Serving authority has a bounded lifetime and is withdrawn when lease
ownership or committed-state freshness cannot be confirmed.

Keep root retirement deadlines in durable state. Recovering them only from a
running peer would lose trust after all pods restart. Missing established state
requires recovery or an explicit new installation, not silent key regeneration.

## Discovery and identity

Clients authenticate the Kubernetes API with a projected API-audience token and
the API server CA. Seat Leases publish the endpoint and its serving certificate.
Clients pin that certificate, verify TLS, and only then send an enrollment
token. Lease changes invalidate pooled connections to the previous holder.

Enrollment uses a separate projected token with audience `racer-control`.
TokenReview is followed by live ServiceAccount, Pod, DaemonSet owner, and Node
binding checks. A node name supplied by a caller is not proof of identity.
The node's private key stays on the node.

Each seat has its own issuer key. The primary commits its public root to the
keyring before that seat issues certificates. Retired roots remain trusted for
the full lifetime of the leaves they could have issued. Keyring capacity and
encoded-byte limits apply during rotation as well as steady state.

## Membership

Use the current managed DaemonSet UID to identify its Pods. Pod selection needs
`spec.nodeName`, `status.podIP`, phase, and owner references; a metadata-only Pod
watch cannot supply these fields. Exclusion and Site labels come from Nodes.

Nodes publish shares and RDMA NICs in their own Pod's
`racer.unbounded-cloud.io/report` annotation. Administrator Node annotations take
precedence. Seats do not write enrollment or recovery annotations to Nodes.
Invalid input must not replace a valid admitted member with unvalidated data.

Cache identity remains the ClusterCache UID. The catalog retains the existing
socket paths, priorities, and key admission rules. Cache removal, capacity
pressure, and a primary change must not silently give an existing cache new keys.

## Runtime and protocol

The control service starts before the dataplane's enrollment loop and has an
explicit thread budget. It must not add an unbounded thread pool or block data
I/O on API requests. API operations, TLS handshakes, request bodies, active
connections, long polls, and shutdown all need bounds.

Keep the existing control wire schema and canonical hashes. Moving the
implementation to Rust does not require changing the data-path contract.
Protocol fixtures belong to the Rust control-wire crate rather than the deleted
Go controller. Standalone test endpoints may still exercise the client without
Kubernetes; managed deployments do not depend on a fixed controller URL or an
operator-generated trust bundle.

The three public routes remain `/v1/bootstrap`, `/v1/snapshot`, and
`/v1/keyring`. Preserve strict decoding, authentication, cursor checks, error
mapping, replay checks, and independent keyring delivery. A replica must never
serve an image solely because another replica sent it; it must match committed
state.

## Admission and permissions

Seats serve the object-sidecar webhook. A selectorless `racer-webhook` Service
and a primary-managed EndpointSlice route admission to ready seats. Add trust
before adding an endpoint; remove an endpoint before removing its trust.
The operator installs the webhook configuration but does not maintain its keys
or CA bundle.

The dataplane ServiceAccount receives the permissions needed to become a seat.
Kubernetes RBAC cannot make those permissions depend on Lease ownership. A seat
placement preference is therefore not a security boundary.

Restrict writes to the named Leases, state Secret, webhook configuration, and
EndpointSlice. Admission policies constrain Pod reporting to the caller's own
Pod and the one report annotation. Do not grant Node writes. Install and verify
write restrictions before granting the corresponding permissions.

RBAC resource names do not restrict an arbitrary label-selected list or watch.
Use named GETs or an exact name field selector where name-scoped access is
required; grant any remaining list/watch permission explicitly.

## Upgrade

This is an alpha cold-cache cutover, not mixed-controller operation. Stop the
old managed controller and withdraw its permissions before starting the new
authority. Remove only resources whose ownership and UID have been verified.
Do not adopt or delete unrelated resources with matching names.

Keep source objects available while caches refill. Do not erase arbitrary host
directories or raw-device guard areas. Old identity and checkpoint handling
must distinguish this explicit cutover from unexpected loss of established
state. Never reuse old counters or encryption keys under a newly created
cluster ID.

## Required validation

- Cold start with one node and with simultaneous contenders.
- Seat limits, unique node ownership, renewal, replacement, and graceful drain.
- Primary loss before and after a successful state commit; stale-writer fencing.
- Same-version hash conflicts, rollback, malformed state, and lost state.
- Trust-root and cache-key rotation through restart and full-fleet restart.
- Token, Pod UID, Node UID, DaemonSet UID, and certificate binding failures.
- API outage, watch expiration, credential rotation, and slow or oversized peers.
- TLS pin mismatch, failover, and invalidation of pooled connections.
- Admission behavior, restricted writes, and no unmanaged-resource adoption.
- Real object reads, Gantry integration, and a primary failure in a kind cluster.
