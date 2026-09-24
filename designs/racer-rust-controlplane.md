# Rust control plane implementation

> Version note: Racer 1.0 uses `/v1/config` and CA state format 1, requires
> canonical Site labels, and removes pre-release migration paths. See
> `cmd/racer-dataplane/CONTRACT.md` for the current version inventory.

## Accepted scope

The control plane is a standalone async Rust crate using Tokio, Axum/Hyper,
kube-rs, rustls, and prost. The capacity target is 10,000 nodes. The stateless
cutover is a coordinated fresh dev/test deployment with matching CP/DP binaries,
not a rolling compatibility upgrade. See [the cutover workflow](racer-stateless-cutover.md).

This contract supersedes the earlier durable topology/chunk store, placement
history, participant shards, replica ConfigMaps, and fleet-proof rotation
barriers. Historical Go-retirement results do not validate this replacement.

## Stateless placement and publication

Each universe has **262,144 slots**. Highest-random-weight (HRW) rendezvous
placement chooses the highest score for each slot from eligible Node UID-derived
identities. All caches in a universe share this owner table. Cache UID and
generation still isolate data, but are not placement inputs. Pod replacement,
IP changes, watch order, leadership fences, revisions, and prior ownership do
not alter placement for an unchanged universe/member set.

Scores use `XXH64(SHA256(domain || universe_id || node_id) || LE32(slot), 0)`,
where domain is `racer/placement/hrw/v1\0` and IDs are the existing binary
SHA-256 identities. Ties select the lexically smallest Node identity. Balance is
statistical, not an exact quota. Adjacent slots may repeat an owner and a member
may own zero slots. Retry candidates remain slots; repeated owners are allowed,
and candidate attempts are not reinterpreted as distinct physical peers.

The runtime rebuilds topology and storage intent from complete live Kubernetes
inventory on leadership acquisition. Payloads, placement acceleration, received
cursors, and operational observations are process-local. There is no durable
topology, storage payload, participant census, boot history, or rollout ledger.
One fixed-schema `ConfigMap/racer-runtime-revisions` reserves monotonically
increasing revision ranges under a leadership fence. Missing, corrupt, or
exhausted checkpoint state and observed replacement/regression fail closed
rather than authorizing a reset.
Only fresh trust-domain bootstrap initializes it.

Nodes independently apply `/v4/config` desired state and can skip revisions.
Received cursors are separate from applied revision/digest. Unchanged requests
hold for 27-30 seconds; success reconnects immediately, and local state or
credential changes interrupt polls. Operational freshness is 75 seconds.
Invalid intent preserves a process's last-good state, not a durable copy across
CP restart. After restart, invalid storage intent produces no valid new offer;
the dataplane retains compatible persisted slab geometry.

HTTP and RDMA route using local placement during mixed *revision* convergence.
A shared hop budget is decremented on every forward and is never reset by retry
or transport fallback. Candidate attempts and total execution are bounded
separately. Preserve universe, cache UID/generation, integrity, and authenticated
membership. These guarantees do not imply mixed-binary cutover compatibility.

## Bounded security state

The runtime owns a constant set of durable objects in its state namespace:

| Object | Contents |
| --- | --- |
| `ConfigMap/racer-runtime-revisions` | Format, revision high-water mark, fence |
| `ConfigMap/racer-trust` | Current public trust bundle |
| `Secret/racer-ca` | Version-5 bounded CA state, at most two roots and expiry watermarks |
| `Lease/racer-controlplane` | Current leadership |

This constant object count excludes workload objects and status on existing
Nodes/P2PCaches. Existing CP Pods hold only the current
`racer.unbounded-cloud.io/pki-request` and `pki-response` annotations, each
bounded to 16 KiB. Responses bind Pod UID, boot, and CSR digest; Pod UID and
resourceVersion CAS fence writes. Private process keys remain process-local.

Signed certificate claims carry namespace, Pod name/UID, Node identity,
universe, boot, and role. TLS authentication no longer needs a participant
lookup. Subscription authorization also checks current live selection.
Enrollment **and renewal** perform live Pod/DaemonSet/Node/Site authorization
and require current selection. Exact old-identity renewal after eligibility or
selection loss is intentionally not preserved. CP issuance checks the live
Pod/ReplicaSet/Deployment ownership chain.

Before any certificate is returned, its issuer's maximum issued expiry is
durably committed and read back. Rotation publishes the two-root bundle and
records confirmed publication time, then waits `--ca-overlap-delay` **plus clock
skew** before switching issuer. The default overlap delay is five minutes.
Retirement waits until the old root's maximum issued expiry plus skew. Successor
leaders may increase persisted safety margins, never shorten them. Offline
participants do not block rotation. Diagnostic fresh TLS proofs do not grant
rotation credit or create a durable fleet acknowledgment ledger.

The version-5 CA parser rejects older state. No CA migration is implemented;
resetting an old dev/test trust domain is a separate, explicit destructive
decision. Runtime cleanup must never delete CA material or cache slabs implicitly.

## Implementation anchors and validation

- Placement: `cmd/racer-controlplane/src/model.rs:12` and
  `cmd/racer-controlplane/src/topology.rs:34`.
- Revision reservation: `cmd/racer-controlplane/src/revision.rs:18`;
  live rebuild: `cmd/racer-controlplane/src/kubernetes.rs:196`.
- CA bounds/format: `cmd/racer-controlplane/src/security/state.rs:25`;
  expiry-before-return and rotation: the same file at `:448` and `:546`.
- Pod exchange: `cmd/racer-controlplane/src/service.rs:1032`;
  live renewal selection: the same file at `:780`.

Validate history-independent placement, membership changes, restart/fencing,
uncertain checkpoint/CA commits, independent convergence, long-poll
cancellation, live authorization, actual signed-claim TLS, expiry-based
rotation, bounded object size/count, HTTP/RDMA retry budgets, and compatible
slab restart. Inspect assertions before citing test coverage. Report missing
runtime prerequisites separately from passing tests; structural scale tests do
not establish hardware throughput or fleet first-byte latency.
