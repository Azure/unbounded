# Racer control-plane scaffold

## Scope and status

One Go binary uses controller-runtime directly, with three replicas and one leader.
Kubernetes is the authority for desired state. Rust nodes receive full publications,
compute placement/routes locally, and serve disposable encrypted cache pages.

This document describes intended behavior. Phase 1 bounded codecs, canonical
hashing, and shared Go/Rust contract vectors are implemented. Other operational
methods remain fail-closed stubs; composition, controller registration, API
declarations, and reserved 503 routes are also implemented. The executable cannot
start an operational service.
The normative wire contract is `cmd/racer-dataplane/CONTROL_API.md`.

## Controllers and lifecycle

`manager.go` constructs a standard manager with cache, client, APIReader, Lease
leader election, metrics, and probes. `Assemble` is side-effect-free. There is no
generic repository or Kubernetes adapter. Namespaced watches are restricted to the
installation namespace; Nodes and ClusterCaches are cluster-scoped.

- `TopologyReconciler` watches Nodes, managed Pods, and ClusterCaches. Relevant
  events map to a singleton key, with one reconcile at a time. Implementation must
  filter unrelated Pods, index by assigned Node, and enqueue initial reconciliation
  even when all input lists are empty. Build canonical content from synchronized
  inputs, compare version hashes, commit the small ConfigMap, then install.
- `KeyringReconciler` watches ClusterCaches and credential Secrets. It manages
  node issuer/trust and shared cache key transitions, persisting timestamps beside
  bundle.json. RequeueAfter schedules rotation; no extra rotation goroutine or
  persistent jobs. Stage roots/keys before activation, retain retiring material,
  and enforce the 512 KiB bundle bound including overlap. No acknowledgments.
- `WorkloadReconciler` manages the DaemonSet. Its desired spec includes a projected
  racer-control token, common keyring and bootstrap trust, node-private identity
  storage, slabs, client/origin socket mounts, and exclusion-label affinity.
  Missing DaemonSets must be reconciled on startup, not only on watch events.

The server is a leader-election runnable. Readiness requires synchronized inputs,
usable issuer/trust, and an installed publication. Followers are live but unready.
On leadership loss the manager/process stops, closes connections, and cancels all
polls; release-on-cancel remains disabled. Kubernetes updates use resource-version
preconditions and conflict retries. Controller-runtime election is not a general
storage fencing service: no claim of strict fencing under arbitrary process pauses
is made. The implementation must honor leader cancellation before writes/serving
and never refresh/retry an old leadership operation after cancellation.

## Minimal durable state

Persist a permanent cluster UUID through deployment configuration. Persist one
version ConfigMap with cluster ID, sequence, membership version, and two canonical
content hashes. Hashes exclude counters. No member checkpoints, publication blobs,
or history. Candidate bytes become visible only after a successful ConfigMap CAS.
Unchanged content reuses counters; cache-only changes preserve membership version.

Initial creation must be an explicit initialize-only operation. Recovery never
recreates missing established counters. Before implementing startup, define the
initialization marker/command and its crash ordering so a missing ConfigMap cannot
silently reset an existing cluster. Loss/corruption requires explicit new-cluster
rebootstrap. This scaffold does not pretend that initialization is implemented.

The common keyring Secret retains keys, generation, and transition times. The
controller-only issuer Secret retains signing material. Kubernetes stores the
managed workload and leader Lease as usual. Do not log Secret data or token bodies.

## Membership recovery tradeoff

Shares default to four; rails default empty; alignment defaults true. Validate
annotations, deduplicate rails, choose non-terminating managed Pod endpoints by
creation time then UID, and exclude Nodes carrying the exclusion label. Readiness
never changes ownership. UIDs, not names, identify Nodes and caches.

Accepted values and last endpoints are process-local. During normal operation,
invalid updates preserve accepted values and Pod gaps preserve endpoints. On
recovery, omit nodes with unavailable endpoints or malformed annotations until
inputs become usable; absent annotations still get defaults. This intentionally
relaxes restart continuity in exchange for eliminating derived-state checkpoints.
Failover may change placement and cause cold fills. Never import an untrusted
dataplane's remembered membership as controller authority.

## Authentication

Two HTTPS endpoints only:

1. POST /v1/bootstrap: projected bearer token, audience racer-control, CSR proof
   of possession, TokenReview plus live bound Pod/ServiceAccount/workload/Node
   checks through APIReader. Resolve the Node UID; never honor requested CSR SANs.
   Return a public leaf-first certificate chain directly. Retry correlation IDs
   do not create persistent enrollment records.
2. GET /v1/snapshot: mTLS with node identity in its URI SAN. Verify chain, cluster,
   usage, validity, and authorization on every request. Limit each node to one poll.

The listener verifies client certificates when provided; the snapshot handler
requires them. Renewal at 16 hours and recovery both reuse bootstrap with a fresh
token and local Ed25519 key. Certificates last 24 hours. A recovery client omits its
expired certificate. TLS responses replace the proposed control HTTP signatures
and challenges. Peer traffic still uses HTTP signatures and AEAD page protection.

Node keys/certificates remain local, independent of shared bundle generations.
HTTPS server trust is deployment-provided and distinct from rotating peer roots.
Server TLS resumption/pooled connection lifetimes cannot bypass expiry or trust
rotation. Long-poll contexts are bounded by certificate expiration.

## Publications and bounds

`Publications` owns one immutable current encoding and one broadcast notification.
Waiters hold references, not copied 64 MiB buffers. Bound poll admission, concurrent
writes, headers, bootstrap concurrency, and write/shutdown deadlines. Coalesce
input bursts. Publication full-replacement semantics allow reconnects to skip
intermediate states. Rust retains bounded old membership leases for in-flight work.

Wire counters are decimal strings; bytes use padded standard base64; CSRs/certs
use DER. Reject duplicate fields/identities, invalid values, unknown versions and
enums before acceptance. Unknown object fields are ignored. Do not substitute the
default Go JSON decoder for these validation requirements. The bundle codec alone
handles secret key material. Socket paths are derived and length-checked.

Serving 100,000 long polls/full snapshots from one leader is a target, not a tested
capacity claim. Validate fanout and reconciliation cost during implementation.

## Implementation order

1. Implement bounded codecs/canonical hashing and cross-language contract vectors (complete).
2. Implement pure membership/catalog reconciliation, including cold-start rules.
3. Implement explicit initialization, version CAS, immutable publication install,
   manager startup enqueue, and leadership cancellation.
4. Implement issuer/shared-key Secret rotation, including failure recovery.
5. Implement token bootstrap and mTLS serving with adversarial identity tests.
6. Implement the managed workload and Rust identity/TLS/projection boundaries.
7. Exercise envtest integration, failover, rotation, and bounded fanout.

Each step replaces stubs with meaningful success/failure/edge tests. Keep scaffold
composition tests; do not add tests that merely enumerate every placeholder.
