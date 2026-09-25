# Racer control-plane scaffold

## Scope and status

One Go binary uses controller-runtime directly, with three replicas and one leader.
Kubernetes is the authority for desired state. Rust nodes receive full publications,
compute placement/routes locally, and serve disposable encrypted cache pages.

This document describes intended behavior. Phase 1 bounded codecs, canonical
hashing, and shared Go/Rust contract vectors, Phase 2 pure membership/catalog
reconciliation, Phase 3 initialization/publication lifecycle, and Phase 4 issuer
and shared-key rotation are implemented. Token bootstrap, certificate request
authentication, TLS serving, and workload construction remain fail-closed stubs.
The initialize-only command is operational; normal invocation validates
recovery state but cannot yet start an operational HTTPS service.
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

### Initialization protocol (Phase 3)

`racer-controller initialize` is an initialize-only command; it never starts the
manager, issuer, workload, or listener. Normal invocation only recovers existing
counters. Both commands read `RACER_INSTALLATION_CONFIGMAP_NAME` (default
`racer-installation`) authoritatively, not from an informer or environment snapshot.
This ConfigMap is permanent deployment configuration, separate from runtime
`racer-config`. Its data contains `cluster`, `version_configmap`, and `state`.

The one-way transition is `fresh` (mutable) -> `consumed` (immutable). Only an
operator installing a genuinely new cluster may provision `fresh`. The deployment
template defaults to `consumed` and immutable; rendering a first installation
requires the explicit `InitializationState=fresh` setting and a new permanent UUID.
With `make racer-manifests`, use `RACER_INITIALIZATION_STATE=fresh` only for that
first render; the unset/default state is consumed. Apply the configuration/RBAC,
run the controller image once with argument `initialize` and the same configuration
environment/service account, then start the controller Deployment. Set the rendered
permanent configuration back to consumed after the successful command.
Never render/apply `fresh` for an existing UUID. After initialization, retain the
consumed immutable object in deployment configuration/backups; normal GitOps must
use `InitializationState=consumed`. Kubernetes immutability prevents an old fresh
manifest from rolling its data back. Do not delete/recreate this permanent object.

The command validates the UUID/name binding and absence of the version ConfigMap,
then resource-version-CAS updates the installation record to `consumed` AND
`immutable: true` in one request. Only the invocation that receives a successful
CAS response may make ONE create attempt for the initial version ConfigMap. It
contains counters 1/1 and the hashes of empty membership/catalog. The version
ConfigMap annotation `racer.unbounded-cloud.io/installation-uid` binds it to the
permanent installation object's UID. No serving or workload startup precedes this
ordering. A repeated/concurrent initialize command always rejects consumed state.

Crash/error recovery is deliberately conservative:

| Durable state | Recovery |
| --- | --- |
| Fresh marker, no counters | Explicit initialize may run; normal startup rejects |
| Consumed marker, missing counters | Fail closed; including a crash after marker CAS or an ambiguous create failure; never retry creation |
| Consumed marker, valid bound counters | Normal startup reconstructs and commits current inputs; initialize rejects |
| Missing/corrupt marker, wrong UID/cluster binding, or corrupt counters | Fail closed |

A successful create followed by process death is recovered through normal startup,
not another initialize. Cancellation is checked before each write; cancellation
after marker consumption can leave the deliberately unrecoverable gap. The marker
is never reset, even when it is known that no publication was served. Kubernetes
object deletion or restoration of stale whole-cluster backups is outside the
one-way guarantee; restoring either object independently is not counter recovery.

Explicit rebootstrap means stop the old installation, provision a NEW cluster UUID
and new permanent installation/version objects (preferably a new namespace), run
initialize once, and rebootstrap dataplane identities and disposable cache state.
Do not copy old issuer/keyring state into the new cluster or reuse the old UUID.
There is intentionally no force/reset/repair command. Loss of counters cannot be
distinguished from a previously published high watermark and never authorizes 1/1.

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

The pure `ReconcileMembers` helper returns candidate accepted history and diagnostics
without mutating inputs. The topology controller must install that history only
after committing the candidate publication. Annotation attributes are accepted as
one unit, independently of the endpoint: a malformed annotation preserves the
previous attributes while a valid new endpoint can still replace the old one, and
valid annotation changes can take effect during a Pod gap. Identical rail mappings
are deduplicated; conflicting mappings for the same rail ID reject the attributes.
Missing annotations reset to defaults. Missing managed DaemonSets are endpoint gaps.
Callers supply installation-namespace Pods and the current managed DaemonSet UID;
endpoint selection checks the `apps/v1` DaemonSet controller owner reference.

`BuildCatalog` returns a UID-sorted catalog or rejects the whole candidate. It
defaults socket mode to 0660, preserves explicit zero permissions, and delegates
canonical path validation to the wire package. Nodes and caches remain present
until absent from the input lists (or explicitly excluded for Nodes); only Pod
endpoint selection filters deletion timestamps. Complete publication byte bounds
and canonical hashes are checked by `wire.ContentHashes` before version assignment.

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
2. Implement pure membership/catalog reconciliation, including cold-start rules (complete).
3. Implement explicit initialization, version CAS, immutable publication install,
   manager startup enqueue, and leadership cancellation (complete).
4. Implement issuer/shared-key Secret rotation, including failure recovery (complete).
5. Implement token bootstrap and mTLS serving with adversarial identity tests.
6. Implement the managed workload and Rust identity/TLS/projection boundaries.
7. Exercise envtest integration, failover, rotation, and bounded fanout.

Each step replaces stubs with meaningful success/failure/edge tests. Keep scaffold
composition tests; do not add tests that merely enumerate every placeholder.

## Phase 3 handoff interfaces

- `Initialize(ctx, Config)` / `TopologyReconciler.InitializeVersion(ctx)` implement
  the one-shot command. `Config.InstallationConfigMapName` binds permanent
  configuration; `Run` validates it and counters before starting manager runnables.
  Defaults are loaded from `RACER_*` and `POD_NAMESPACE`. Later phases must add
  TLS/workload-specific validation when implementing those entry points.
- `Publications.Prepare(previous, resourceVersion, candidateHistory, catalog)`
  validates, hashes, assigns counters, and owns a private encoding.
  `TopologyReconciler.CommitVersion(ctx, prepared)` authoritatively rereads state
  and CAS-updates the version ConfigMap, even for unchanged content. Conflicts
  requeue the singleton with fresh inputs and unchanged accepted history.
  `Publications.Install(committed)` rejects zero/foreign/rolled-back/conflicting
  values and canceled contexts. Only then does topology replace accepted history.
  Missing/invalid durable state suspends readiness and wakes waiting polls; input
  validation failures retain the previous valid publication. No recovery Create.
- `CommittedPublication.Version()` returns a value; `Encoding()` returns an
  immutable shared string. `WriteTo(io.Writer)` checks leadership between bounded
  32 KiB writes. Phase 5 must impose request cancellation, certificate deadlines,
  concurrent write admission, and HTTP write deadlines around it and close active
  connections on leadership loss. Never convert the entire encoding to `[]byte`
  per response. Retained publication references are bounded by admitted handlers.
- `Publications.Wait(ctx, NodeIdentity, *wire.Sequence)` owns bounded waiting
  admission (global `Limits.MaxPolls`, one waiter per node). Nil cursor returns
  current immediately, lower cursor returns latest, future/zero cursor conflicts,
  equal waits at most 30 seconds. `(nil, nil)` is normal 204 timeout. Expiration,
  request cancellation, and committed leadership cancellation terminate waits.
  Phase 5 must keep HTTP admission through response completion so a node cannot
  overlap a waiting/writing response; `Wait` releases its slot when it returns.
- `Application.Lifecycle` is shared with Keyring and Server. It is a one-shot
  leader runnable and waits for manager cache sync. Phase 4 calls
  `SetIssuerReady(bool)` only for usable issuer/trust, resetting on failure.
  Phase 5 calls `Lifecycle.Wait(ctx)` before listener startup, then
  `SetServingReady(true)` when accepting authenticated connections, resetting on
  shutdown. `Server.Ready` delegates to all lifecycle gates plus publications.
  `Server.Start` remains fail-closed pending TLS implementation. Controller
  reconciliation currently has no per-reconcile timeout; committed publications
  retain that leader-derived context. Do not introduce a short-lived reconcile
  timeout without separately supplying the full leadership context for serving.
- All three controllers use `initialEnqueue()` as a raw source, so startup runs
  even for empty lists. Sources/workers are leader-scoped; cache synchronization
  precedes worker execution. Pod `spec.nodeName` is indexed; predicates ignore
  readiness/unrelated inputs and map relevant events to one singleton key.
  Workload reconciliation remains Phase 6 work; keyring is implemented in Phase 4.

Targeted fake-client and race tests cover initialization crash ordering, ambiguous
responses, CAS conflicts, cancellation before writes/install, counter transitions,
immutable ownership, readiness, and 256 simultaneous poll wakeups. This is not an
envtest election/real-apiserver immutability test or a 100,000-node capacity claim;
those integration/load checks remain Phase 7.

## Phase 4 durable protocol and handoff

- `KeyringReconciler` reads the installation/version objects, ClusterCaches, and
  both credential Secrets through `APIReader`. It uses resource-version CAS and
  singleton `RequeueAfter` deadlines, with no rotation goroutine or acknowledgments.
  Conflicts restart from authoritative inputs. Cancellation is terminal and checked
  before every write; failures reset `Lifecycle.SetIssuerReady(false)`.
- Credential initialization CAS-adds the one-way
  `racer.unbounded-cloud.io/credentials` annotation to the existing version
  ConfigMap, binding the two Secret names and initial root fingerprint. Topology
  preserves it. Only that successful invocation may create the issuer and common
  Secrets, in that order. The same claim is attached to both Secrets. An ambiguous
  claim/Create response is recovered only if both valid Secrets exist. A consumed
  claim with either Secret missing fails closed, including incomplete first-time
  initialization; it requires explicit new-cluster rebootstrap. Missing/corrupt
  version or installation objects never authorize credential creation. Renaming
  configured Secrets does not authorize a new credential lineage. As with Phase 3,
  privileged deletion of the claim or stale backup restoration is not recovery.
- The controller-only issuer Secret stores `issuer.json`: root DER and PKCS#8
  Ed25519 private material indexed by SHA-256 root fingerprint, plus a pending
  root reference. The common Secret stores bounded `bundle.json` and controller
  `rotation.json` metadata. No node identity/private key or deployment HTTPS trust
  is included. Only `bundle.json` is a dataplane wire contract.
- Rotation metadata persists the next rotation, preparation deadline, selected
  active/prepared issuer fingerprints, retirement deadlines for roots and scoped
  cache-key IDs, and earliest next transition. Both cache-key purposes rotate.
  Initial cache keys are active immediately; replacements are prepared before
  activation. Caches added during an existing preparation retain their initial
  active keys until the next cycle. Removed cache UIDs lose their key scopes;
  recreation receives unrelated keys.
- Before staging, encode the complete overlapping candidate under the 512 KiB
  limit, including its incremented generation. Then persist new private issuer
  material first and publish its root and prepared cache keys in one common-Secret
  CAS. An interrupted staging write reuses the pending private issuer. Activation
  changes cache states and the selected signing issuer in the common Secret alone,
  so that Secret is the signing authority. Retiring material remains for the full
  configured overlap after actual activation, at least the 24-hour leaf lifetime.
  Pruning removes common trust first, private material second. Failures between
  these writes leave recoverable extra private material, not dangling trust.
- Deadlines are not reset on replay/restart and missed rotations are not replayed
  in a loop. Long downtime that exhausts a prepared root cancels that unused
  preparation and stages fresh material with a full preparation delay. Expired
  active issuers remain unready until activation recovers usable signing state.
  Generation exhaustion, malformed state, missing material, and size overflow
  fail closed without resetting generation or overwriting corrupt state.
- `Issuer.Issue(ctx, NodeIdentity, wire.BootstrapRequest)` verifies Ed25519 CSR
  proof of possession and response bounds, discards requested names/extensions,
  and signs a 24-hour client-auth/digital-signature leaf with the resolved cluster
  and Node URI. It returns a leaf-first public chain and enrollment correlation.
  Phase 5 must first obtain `NodeIdentity` from live token authorization, including
  its authorization expiration, and gate issuance on leadership. There is no
  enrollment receipt ledger. `AuthenticateCertificate` remains Phase 5 work.
- `Issuer.TrustRoots(ctx)` returns a new owned pool from authoritative committed
  credentials, distinct from deployment HTTPS server trust. Phase 5 must use fresh
  trust for TLS admission and reverify chain, usage, identity, validity, and live
  authorization on every snapshot request; pooled TLS `VerifiedChains` alone
  cannot authorize retired roots. Phase 5 uses the existing `Lifecycle.Wait(ctx)`
  and `SetServingReady` hooks, and must cancel serving on leadership loss.

Phase 4 fake-client/race tests cover initialization and rotation write boundaries,
ambiguous responses, restart deadlines, multiple retiring generations, private
material cleanup, expired preparation, conflicts/cancellation, authoritative reads,
lost/corrupt durable state, catalog churn, overlap overflow, certificate identity,
proof-of-possession rejection, trust retirement, and concurrent issuance. Real
API-server/election and projection integration remain Phase 7 verification.
