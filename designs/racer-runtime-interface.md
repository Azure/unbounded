# Rust runtime wiring contract

The Rust runtime exports `kubernetes`, `subscription`, and `status` from the
library. Main/service own TLS and leadership acquisition. The runtime owns the
Kubernetes watches, topology/storage persistence, and `/v4/config` Router.

## Interface

```rust
use racer_controlplane::kubernetes::{Runtime, RuntimeOptions, SecurityContext};
// Called synchronously on every request and immediately before publication.
// None on election loss, cancellation, or incomplete security initialization.
// state is an immutable, indexed, current CaState snapshot refreshed by service
// after enrollment/issuance/retirement. Never perform a full CA read per request.
let security: Arc<dyn Fn() -> Option<SecurityContext> + Send + Sync> = ...;
// SecurityContext { fence: String, state: Arc<security::CaState> }
let runtime = Runtime::new(RuntimeOptions::new(namespace), security);
let router = runtime.router(); // merge into service Router; no TLS listener here
// Every accepted TLS request must contain Extension<security::VerifiedPeer>.
// spawn runtime.clone().run(client, shutdown_token)
```

`runtime.selection(universe_hex, node_hex)` returns a selected process binding
(`Selection { node_name, pod_namespace, pod_name, pod_uid, universe }`) or None.
`runtime.ready()` is false until complete watch initialization and authoritative
restart load/fence acquisition. Selection is indexed, with no request-time Node
list. Enrollment may use selection but must still perform its own authoritative
Pod/Node/workload authorization. `/v4/config` calls `VerifiedPeer::node_pod` and
`CaState::verify_member` using the process boot header on each request, including
after long-poll wakeup. The returned durable member must not be tombstoned.

The runtime is reusable across leadership changes. Security getter fence changes
force an authoritative reload and pointer-fence claim before publishing. Set the
getter to None immediately on election loss. Store writes use captured pointer
resourceVersions, never freshly fetched versions on a stale write retry.

The handler implements DesiredState, never ControlCommand. It accepts identity
from the trusted certificate URI and the boot header, not query identity claims.
Storage feedback is bound to the exact offered Pod/boot/identity/version and
operational freshness is 75 seconds. It does not constitute a fresh TLS proof.

Status/API error retries and work queues are runtime concerns. No remote phases,
rollout ledger, idle-subscriber persistence, or per-request all-node scans exist.

## Implemented persistence and checks

`RecordStore` uses `racer-v4-topology-<universe hex>` and
`racer-v4-storage-<node hex>` ConfigMap pointers, labeled
`racer.unbounded-cloud.io/rust-state=pointer`. Content lives in immutable
`racer-v4-chunk-<sha256>` ConfigMaps with binaryData `content`. The security
bootstrap artifact check must recognize these prefixes/labels so losing the CA
Secret cannot regenerate an authority over existing topology.

Before each pointer write, RecordStore invokes
`security::kubernetes::KubernetesCaStore::check_fence()` after capturing the target
resourceVersion. It uses the existing `racer-controlplane` Lease and `racer-ca`
Secret names. The synchronous SecurityContext getter also gates completion and
every held subscription. Runtime reload claims all pointer fences before serving.

The runtime's concrete signature is `async fn run(self, Client,
CancellationToken) -> anyhow::Result<()>`. Runtime is Clone; `router()` returns
`axum::Router`. Public `subscriptions.cache_usage()` and `waiter_count()` expose
bounded-memory/receiver metrics. The default hold is 28 seconds.

Verification uses real kube HTTP requests against a resourceVersion-enforcing
test API, plus real rustls HTTP cancellation, restart, uncertain pointer commit,
storage-only wakeup and 10,000 simultaneous held handler requests. RecordStore
also runs against a real kube-apiserver/etcd using KUBEBUILDER_ASSETS. The separate
production-binary harness owns full service/cluster integration.

The default distinct-client TLS scenario covers 24 nodes in three universes with
a 12-second deadline and 20-second watchdog. The full 10,000-client TLS stress
scenario is ignored by default. Historical loopback completion measurements do
not verify 10,000-node first-byte tails; that measurement was canceled. See
`designs/racer-go-retirement-audit.md` for exact results and boundaries.

## Chunk collection and bounded work

RecordStore serializes authoritative reads, staging and collection with a
resourceVersion-CAS `racer-v4-store-gate` ConfigMap. A new leadership fence may
replace a crashed predecessor's gate. Staging reserves the target pointer before
writing chunks: `proposed` references never authorize serving. Collection first
CAS-clears abandoned proposals while holding the gate, fencing delayed pointer
completion. Committed references remain protected. Reused immutable chunks get
a metadata RV update; collection deletes with UID/RV preconditions, so a paused
old collector cannot delete a newly reused chunk. A local guard registers each
acquisition before sending I/O and retains the operation, fence, and pre-write
resourceVersion across cancellation. Release retries authoritative read/CAS for
up to 30 seconds while holding the local lock; failed cleanup remains pending for
the next call on the same RecordStore or its clones. Cleanup only clears its own
operation and fence. It also invalidates a still-pending acquisition by touching
its pre-write resourceVersion, or creating an empty gate to fence a delayed first
create. A successor's changed resourceVersion ends cleanup without unlocking it.
Losing pending local state (a process crash, runtime shutdown, or dropping all
store clones after failed cleanup) requires takeover. No wall-clock grace period
establishes safety or permits stealing a live same-fence operation.

Collection runs every five minutes without recompiling universes. Lists use
64-object pages and chunk lists request metadata only. The reference set has a
one-million-entry ceiling; exceeding it aborts before deletion. Failed scans or
conditional deletes are retried from authoritative state. Empty reservation
pointers are retained as CAS targets, not served as committed generations.

Snapshot construction uses sparse edge indexes at 10,000-member geometry and a
dense fallback for high local ownership. Four builders and 16,384 admitted
requests bound concurrency; queued builders retain no topology Arc. Payload
accounting includes conservative allocation overhead. Digest metadata is capped
at 100,000 recipients and invalidated on publication. No CA-state copies or
Node-list scans occur per request. Storage I/O remains a bounded 16-operation
queue; the store gate intentionally serializes durable mutations and GC.
