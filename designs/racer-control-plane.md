# Racer control-plane server

## Scope and status

One Go binary uses controller-runtime directly, with three replicas and one leader.
Kubernetes is the authority for desired state. Rust nodes receive full publications,
compute placement/routes locally, and serve disposable encrypted cache pages.

This document describes intended behavior. Phase 1 bounded codecs, canonical
hashing, and shared Go/Rust contract vectors, Phase 2 pure membership/catalog
reconciliation, Phase 3 initialization/publication lifecycle, Phase 4 issuer
and shared-key rotation, Phase 5 token bootstrap/authenticated HTTPS serving,
and Phase 6 server-side managed workload reconciliation are implemented. Phase 7
server integration and bounded publication/reconciliation measurements are complete.
The initialize-only command and leader-scoped HTTPS service are operational;
deployment must provide serving TLS files, bootstrap server trust, and a compatible
dataplane image. The Rust control client, local identity logic, and transport
were implemented independently and are outside the scope of this server work.
Shared contract tests retain a test-only reference codec and also exercise the
existing Rust runtime codec against the server fixtures.
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

Serving 100,000 HTTPS long polls/full snapshots from one leader remains a target,
not a tested capacity claim. Phase 7 measured 100,000 publication waiters and
100,000-member reconciliation separately from live HTTPS/API authorization below.

## Implementation order

1. Implement bounded codecs/canonical hashing and cross-language contract vectors (complete).
2. Implement pure membership/catalog reconciliation, including cold-start rules (complete).
3. Implement explicit initialization, version CAS, immutable publication install,
   manager startup enqueue, and leadership cancellation (complete).
4. Implement issuer/shared-key Secret rotation, including failure recovery (complete).
5. Implement token bootstrap and mTLS serving with adversarial identity tests (complete).
6. Implement server-side managed workload reconciliation and deployment wiring (complete).
7. Exercise envtest integration, failover, rotation, and bounded fanout (server-only complete;
   measured limits and remaining deployment/client validation are listed below).

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
  `Server.Start` implements these gates in Phase 5. Controller
  reconciliation currently has no per-reconcile timeout; committed publications
  retain that leader-derived context. Do not introduce a short-lived reconcile
  timeout without separately supplying the full leadership context for serving.
- All three controllers use `initialEnqueue()` as a raw source, so startup runs
  even for empty lists. Sources/workers are leader-scoped; cache synchronization
  precedes worker execution. Pod `spec.nodeName` is indexed; predicates ignore
  readiness/unrelated inputs and map relevant events to one singleton key.
  Workload reconciliation is implemented in server-only Phase 6; keyring in Phase 4.

Targeted fake-client and race tests cover initialization crash ordering, ambiguous
responses, CAS conflicts, cancellation before writes/install, counter transitions,
immutable ownership, readiness, and 256 simultaneous poll wakeups. Phase 7 adds
real-apiserver immutability/election and 100,000 publication waiters; neither suite
establishes 100,000-node HTTPS capacity.

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
  enrollment receipt ledger. `AuthenticateCertificate` is implemented in Phase 5.
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
API-server/election verification is covered by Phase 7. Kubelet projection is a
deployment integration check and is not exercised by envtest.

## Phase 5 serving and authorization handoff

- `Bootstrap.Authenticate` sends the exact bearer token to TokenReview with the
  `racer-control` audience and requires that audience in the authenticated result.
  Only TokenReview's exact ServiceAccount username/UID and singleton bound Pod
  extras establish caller identity. APIReader checks the live Pod UID, assignment,
  nonterminal/nonterminating state, ServiceAccount UID, current configured
  DaemonSet controller owner name/UID, and live nonexcluded Node UID. Optional
  TokenReview Node extras must agree when present. Pod readiness/IP is not an
  authentication prerequisite. The authenticated token's JWT `exp` only bounds
  issuance authorization; raw JWT identities are never used. `Enroll` caps its
  context by that expiration and delegates Ed25519 CSR proof/signing to `Issue`.
- `AuthenticateCertificate` requires TLS-verified evidence and independently
  verifies the presented chain against fresh authoritative `TrustRoots` on every
  request. It checks current chain validity, Ed25519/digital-signature/client-auth
  usage, one exact cluster/Node URI, live Node UID/exclusion, and a live authorized
  Pod of the current managed DaemonSet and ServiceAccount. Node certificates bind
  Node UIDs rather than Pod UIDs, so replacement authorized Pods on the same Node
  can continue using a locally persisted valid identity. UID-only certificates
  use an informer UID index only to discover the Node name, then authoritatively
  GET that Node and recheck UID, exclusion, and deletion. Missing/ambiguous hints
  return retryable unavailable without falling back to a full live Node list.
  The namespace-scoped live Pod list is filtered by `spec.nodeName`.
- `Server.Start` waits for lifecycle readiness, loads deployment TLS files, binds
  the listener, and marks serving ready. It serves TLS 1.3 HTTP/1.1 only, requests
  and verifies optional client certificates using fresh roots per handshake, and
  disables session tickets. Bootstrap recovery omits an expired certificate.
  HTTPS server certificate files are loaded at startup; deployment certificate
  replacement requires a controller restart. Peer root rotation remains live.
- Only exact POST `/v1/bootstrap` and GET `/v1/snapshot` routes are admitted.
  Alternate methods/paths, encoded path aliases, unknown/duplicate/noncanonical
  query parameters, snapshot bodies, and bootstrap media/encoding mismatches fail
  with protocol errors. No ServeMux redirects or implicit HEAD endpoint exists.
  Errors contain only bounded wire codes; 429/503 include `Retry-After: 1`.
- HTTP admission holds one slot per Node and a global poll bound through response
  flush, in addition to `Publications.Wait`'s waiting admission. Handshakes' trust
  reads, request authorization and enrollment share bounded authentication slots;
  slow snapshot writes have separate bounded slots. Saturation rejects immediately.
  Long polls are capped by the earliest verified-chain expiration and reauthorize
  after waiting, before writing. Expiration can return a bounded 401 recovery error.
  Snapshot bytes use `WriteTo` with bounded scratch and request cancellation checks.
- `Limits.WriteTimeout` bounds authentication/API calls, bootstrap body reading and
  issuance, header/handshake reading, and response writes. Each snapshot write gets
  a fresh window capped by certificate expiration. HTTP parser header limits are
  supplemented by application header accounting. The standard library may reject
  malformed/oversized HTTP framing before routing. Leadership cancellation cancels
  requests, closes listeners and active/idle connections immediately, withdraws
  readiness, and bounds shutdown by `Limits.ShutdownTimeout`. Connection context
  tracks the raw transport so write deadlines/cancellation also close TCP directly;
  TLS close-notify cannot extend a blocked write past its admission deadline.

Phase 5 tests include real TLS enrollment/snapshots, strict errors/routes/bounds,
live UID/ownership/audience attacks, pooled trust retirement and expiry, disabled
resumption, expired-identity recovery, poll expiration/cancellation, write-completion
admission, API/body deadlines, listener startup/readiness and leadership shutdown.
Scaffold composition tests remain; Phase 6 owns server-side workload wiring and
Phase 7 verifies the real API-server/election and measures capacity constraints.
Projection requires a kubelet and remains deployment verification.

## Phase 6 server-only workload and deployment handoff

- `WorkloadReconciler` validates deployment configuration and creates the managed
  DaemonSet from the startup singleton enqueue, including when no objects exist.
  Normal `Run` validates workload configuration before constructing the manager;
  initialize-only operation still does not require a dataplane image or endpoint.
- Reconciliation uses authoritative reads and optimistic resource-version patches.
  Conflicts and create races requeue for a fresh read; canceled leadership stops
  writes. Foreign management labels or incompatible immutable selectors fail
  closed. Terminating workloads are allowed to finish deletion before recreation.
  The controller repairs the pod spec, including added privileges or volumes,
  while preserving DaemonSet metadata and pod-template rollout annotations.
- The managed service account has no API RBAC binding and automatic token mounting
  is disabled. A dedicated projected token has audience `racer-control`, one-hour
  requested lifetime, and mode 0400. The common Secret exposes only `bundle.json`
  at `/etc/racer/keyring`; the issuer Secret is never mounted. Deployment server
  trust is a distinct ConfigMap mounted at `/etc/racer/bootstrap/ca.crt`.
  Projection mounts use directories without subPath so kubelet rotation is visible.
- Node-local host directories are `/var/lib/racer/identity` (identity persistence),
  `/var/lib/racer/slabs` (disposable slabs), and `/run/racer` (both socket endpoint
  trees). They use `DirectoryOrCreate`; the pod runs as root with all capabilities
  dropped, privilege escalation disabled, and a read-only root filesystem.
  Private-key creation, file permissions, reload, and retirement are client responsibilities.
  Linux affinity excludes every Node carrying the exclusion label, regardless of
  its value. The pod uses the supplied image's default entrypoint; no unsupported
  `control` subcommand is injected. The generated environment uses the existing
  client's `RACER_CONTROL_ENDPOINT`, `RACER_PEER_LISTEN`, `RACER_TRUST_BUNDLE`,
  `RACER_SERVICE_ACCOUNT_TOKEN`, and `RACER_SECRET_DIRECTORY` settings, with
  explicit identity/slab directories. The server's own `RACER_CONTROL_URL` and
  `RACER_PEER_PORT` settings are translated when building the DaemonSet.
- Existing manifests provide three controller replicas, leader-readiness Service
  routing, controller RBAC, and the unprivileged dataplane ServiceAccount. Templates
  accept `ServingTLSSecret`, `BootstrapTrustConfigMap`, and `ControlURL` overrides.
  Supply the serving TLS Secret externally. Supply the trust ConfigMap externally,
  or render with `BootstrapCA` containing the public PEM CA bundle. An omitted
  `BootstrapCA` emits no ConfigMap, preserving externally managed trust. It must
  verify the controller Service hostname and must not be copied from rotating peer
  roots. For example, the generic renderer accepts `--set BootstrapCA="$(cat ca.crt)"`
  with `--templates-dir deploy/racer --output-dir deploy/racer/rendered` and the
  same namespace, cluster UUID, and image settings used by `make racer-manifests`.
  ConfigMap environment changes and serving certificate replacement require a
  controller rollout; bootstrap trust remains a live projected directory.

Phase 6 tests cover workload creation from startup enqueue, drift repair/no-op,
ownership rejection, conflict/create-race recovery, cancellation after reads,
credential/storage/affinity contracts, and rendered deployment/RBAC consistency.
These are fake-client and render tests, not a claim of a working Rust client or
end-to-end dataplane deployment. Phase 7 now verifies real API-server
defaulting/admission and election. Kubelet projected-token rotation and node
filesystem integration remain deployment checks; the client runtime is out of scope.

## Phase 7 server verification and measurements

### Reproducible checks

Use Go 1.26.6 and repository-local envtest assets. `make racer-server-test` runs
server, wire, and manifest contract suites under the race detector plus lint.
`make racer-envtest KUBEBUILDER_ASSETS=<absolute-repository-path>` explicitly runs
the real control plane and fails if assets cannot execute. `make racer-scale`
sets `RACER_SCALE=1`, `GOMAXPROCS=8`, and executes the scale suite without race
instrumentation. Both targets place runtime temporary files inside this worktree.
`cmd/racer-controller/README.md` documents asset setup and operational initialization.

`internal/racer/integration_test.go` exercises Kubernetes 1.37.0 etcd/apiserver with
the generated ClusterCache CRD. Assertions cover:

- Concurrent initializers contend on real marker resourceVersion CAS: exactly one
  wins. The server rejects rollback of consumed immutable data and removal of
  immutability. Ambiguous counter Create outcomes recover only when the bound
  counters actually exist. A concurrent metadata write after the authoritative
  version read causes a real API-server 409; no install token escapes. Cancellation
  between commit and install rejects the publication.
- Real Secret writes interrupted after private staging, after common activation,
  and before private pruning recover in fresh applications. Pending issuer material,
  bundle generation, deadlines, cache-key overlap, and trust-before-private pruning
  survive these boundaries. The broader before/after-write matrix remains in the
  existing unit/race tests.
- Two real managers use production options and all three reconcilers. Only election
  durations and bound addresses are shortened/isolated for the test; release-on-cancel
  remains false. Readiness probes distinguish leader/follower. Empty input startup
  creates the DaemonSet, real admission defaults do not cause a write loop, and a
  watched privilege mutation is repaired.
- Real Pod-bound TokenRequest credentials pass real TokenReview over HTTPS;
  wrong-audience tokens fail. The returned identity uses the API-assigned Node UID.
  The managed Pod becomes published through the informer, and mTLS snapshot serving
  uses that certificate. A pooled connection observes live Node exclusion.
- A transport fault rejects only the leader's Lease renewal writes. The real elector
  loses leadership, manager exits, readiness withdraws, an authenticated pending poll
  terminates without snapshot data, and the old TCP listener closes. The follower
  acquires the expired Lease and serves the same counters with the existing client
  certificate. Normal manager cancellation also withdraws readiness and publication
  authority. The measured failover below uses test durations 4s/2s/500ms, not the
  production manager defaults.

The whole server call path was reviewed: both HTTP routes reach implemented code.
Unused `Server.Poll`/`ParseCursor` scaffold methods and their `Pending` error helpers
were removed. Existing composition tests/functions and committed wire vectors are
retained. No client runtime was added. Production manager options are shared with
integration tests; no new module dependencies or storage protocol changes were needed.

### Publication and reconciliation scale

Measurements on Linux amd64, Go 1.26.6, `GOMAXPROCS=8`, using
`TestServerScale`: one managed Pod per Node, two rails per Node, and 16 caches.
Input list/watch responses are synthetic; the controller-runtime informer cache,
Pod field index, safe deep copies, full `TopologyReconciler.Reconcile`, canonical
hashing, and encoding are real. Version persistence uses the fake client, so these
times exclude apiserver latency, watch ingestion/startup, and durable storage cost.
They include 100,000 indexed Pod queries at the largest size.

| Members | Cold reconcile | Unchanged reconcile | Allocated bytes (cold) | Publication bytes |
| ---: | ---: | ---: | ---: | ---: |
| 1,000 | 24.140268 ms | 24.287835 ms | 21,322,144 | 216,638 |
| 10,000 | 280.400074 ms | 239.813666 ms | 230,600,040 | 2,146,202 |
| 100,000 | 2.823713048 s | 2.936335556 s | 2,356,837,080 | 21,503,748 |

Allocated bytes are total allocation traffic, not peak/live heap. Even unchanged
reconciliation rebuilds and hashes the full candidate. Singleton coalescing prevents
one full rebuild for every input event, but this remains substantial CPU/GC work.
This fixture demonstrates approximately linear scaling, not a bound for arbitrary
annotations, catalog sizes, or event rates.

The waiter test prepares a second full-size publication, admits 100,000 concurrent
goroutines through the production `Publications.Wait`, rejects the next identity
and a duplicate identity, installs once, and verifies every result is the exact
same committed pointer and admission returns to zero. Both old/new encodings remain
live. It measures registration, broadcast/delivery, heap, and goroutine stacks,
not TLS connections, authorization, response writes, or network transfer:

- Admission: 351.471643 ms; `Install` broadcast: 126.662294 ms.
- All 100,000 waiters delivered: 279.626321 ms from installation start.
- Parked heap delta: 161,741,360 bytes; stack delta: 409,534,464 bytes.
- Next encoding: 21,500,803 bytes (changed shares, empty catalog).
- Total scale test: 10.470 s. Two garbage collections clear temporary encoding
  pools before the waiter baseline; memory deltas are not process RSS or peak usage.

The same complete scale suite also passed under `-race` in 70.453 s. At 100,000
members, race-instrumented cold/unchanged reconciliation took 17.151336473 s /
16.843947661 s; all 100,000 waiters received the shared pointer in 1.219179561 s.
Race timings are correctness instrumentation results, not production performance.

### Live HTTPS/API authorization cost

The original Phase 7 envtest measurement used the actual HTTPS handler with a real
certificate and an instrumented authoritative client; the connection was warmed first. Ten sequential
snapshot requests each perform two authorization passes. Each pass reads installation,
version, issuer, and common Secret state, lists Nodes and assigned Pods, and gets
the DaemonSet and ServiceAccount. This is **16 API requests and two full Node lists
per response**, plus four trust reads on a new TLS handshake. Source:
`internal/racer/server.go:374` and `:65`, `internal/racer/certificates.go:116`
and `:259`, and `internal/racer/authorization.go:64`.

A race-instrumented run with envtest QPS=1000/burst=2000 measured:

| Live Nodes | Snapshots | Elapsed | API requests | Node lists | API response bytes |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 10 | 246.773746 ms | 160 | 20 | 273,340 |
| 1,001 | 10 | 966.947811 ms | 160 | 20 | 7,058,860 |

That standalone race envtest run passed in 15.532 s (14.45 s inside the test),
including a 4.647877135 s forced-Lease-loss-to-authenticated-recovery interval with
unchanged sequence/membership 2/2. The final full relevant race suite, including
the added real workload-drift and normal-cancellation assertions, passed:
`internal/racer` 27.873 s, `internal/racer/wire` 17.342 s, and `deploy/racer`
1.093 s; API and controller command packages compiled with no test files. Targeted
`make fmt` and lint reported zero issues. Manifest rendering and controller build
also passed. These elapsed values are observations from this host, not guarantees.

The added Nodes are excluded from membership but still appear in authoritative
Node lists. Response-byte totals count API bodies, not TLS framing. This is a small
sequential cost measurement, not a throughput or concurrent HTTPS capacity result.
At 100,000 clients polling every 30 seconds, the original algorithm would require
about 53,333 API requests/s and 666.7 million Node entries/s when the cluster also
has 100,000 Nodes, before reconnects and changes. Those are arithmetic extrapolations,
not measured achieved rates. Production capacity at that size is unverified and
the full-list authorization path was a known scaling limitation. The separately
approved startup-overload fix below replaces that lookup while retaining live
authorization freshness.

### Reviewed startup-overload lookup fix

During the 1,500-node rollout on September 26, 2026, full Node lists in both
snapshot authorization passes competed for the same 32 slots as TLS trust reads.
One sampled worker run completed initial identity bootstrap, started its worker
threads, then repeatedly had TLS admission rejected while waiting for its first
snapshot and exited at its unchanged 30-second worker startup deadline.

The reviewed change uses the synchronized Node informer's `racer.nodeUID` index
only as a name hint. Each authorization pass still GETs the live Node and checks
the certificate UID, exclusion, and deletion before checking live Pods, DaemonSet,
and ServiceAccount. Trust-root reads and post-poll reauthorization are unchanged.
A stale hint cannot authorize a recreated, excluded, or deleted Node. Cache lag,
lookup errors, and ambiguous hints fail closed with retryable unavailable; there
is no full-list fallback. No wire, credential, admission-limit, or timeout changes
are required.

`TestNodeAuthorizationUsesOnlyLiveStateAfterIndexedHint` covers stale hints,
revocation, lookup failures, and absence of live Node lists. The pooled TLS tests
freeze the hint to exercise revocation before informer updates. The real HTTPS
envtest measurement now checks 1 and 1,501 live Nodes: ten snapshots still perform
160 authoritative API requests, but exactly 20 Node GETs and zero Node lists, with
identical Node response-byte totals at both sizes. The historical full-list table
above is retained as the baseline, not a measurement of the new lookup.

The race-instrumented regression run measured 316.4 ms at 1 Node and 253.6 ms at
1,501 Nodes for ten snapshots. Node GET response bodies totaled 3,740 bytes in
both cases. This verifies removal of cluster-size-dependent Node transfer, not
1,500 concurrent startup capacity; live rollout validation remains separate.

Full snapshot distribution also sends one copy over the network per recipient:
the measured 21.5 MB publication would require about 2.15 TB per 100,000-recipient
update. Shared in-process bytes do not remove that bandwidth cost. Authentication
and response-write concurrency are bounded at 32 and 128 by default; saturation
returns 429 rather than creating unbounded work.

### Scope boundary

Envtest has no scheduler, DaemonSet controller, kubelet, or Service routing. Pods
are created/assigned explicitly in integration tests. The envtest clients use test
admin credentials; shipped RBAC has manifest contract coverage, not a restricted
ServiceAccount deployment test. Actual kubelet token renewal,
Secret projection/reload, host filesystem permissions, and end-to-end dataplane
operation remain unverified. Rust control-client runtime, local identity management,
and transport are explicitly out of scope. This server-only Phase 7 does not claim
those components work or that one leader sustains 100,000 authenticated HTTPS polls.

## Integration onto racer-v2

The seven server phases were cherry-picked onto the independently completed client
branch at `093c6f63`. The resulting commits are `7dab33a4`, `15328a54`, `91ca1f85`,
`e4b5c0eb`, `c7f3c2c0`, `b6e5b84c`, and `12f6d76a`. Integration preserved the
production Rust codec and exports, Cargo dependency versions (including
`x509-parser` 0.16 with verification), and native/release/image Makefile targets.
The server reference codec is namespaced under `cfg(test)`; its original tests
remain present. An additional test checks the production codec against the shared
server fixtures and canonical bytes. The DaemonSet now emits the existing client's
accepted configuration names and matching projection/storage paths.

Combined-branch verification on September 25, 2026:

- `GOTOOLCHAIN=go1.26.6 make fmt`: passed, including repository-wide auto-fix lint.
  The host default Go 1.27.1 caused the installed Go 1.26-built linter to panic;
  selecting the repository's declared Go toolchain resolved this.
- `make racer-test` with Go 1.26.6: server/deployment lint and race tests,
  Rust formatting, and locked all-target/all-feature checks passed. Rust library,
  executable, and enabled conformance tests passed. The shell timeout interrupted
  the final production suite; rerunning that suite with a larger timeout passed
  all six tests. All 31 doctests also passed. In total, 557 Rust tests passed and
  seven existing opt-in tests were ignored (six native RDMA and one SDK bridge).
- `make racer-envtest` with repository-local Kubernetes 1.37.0 assets: all three
  subtests passed under the race detector, including real manager election,
  authenticated HTTPS recovery, initialization/CAS, and rotation crash recovery.
- `make racer-scale`: passed through 100,000 members and 100,000 waiters. This
  remains a reconciliation/publication test, not an HTTPS capacity result.
- `make racer-controller-build`: passed.

This integration does not establish kubelet projection, scheduling, or a full
deployed Go-controller/Rust-dataplane end-to-end result. Those deployment checks
remain as described above.
