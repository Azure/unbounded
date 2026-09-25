# Racer control API (v1)

This is the contract for the Go controller and Rust dataplane. Types and service
interfaces define the Rust enrollment, TLS publication, and credential lifecycle.
The Rust dataplane implements these operations. The Go controller remains a
scaffold; deployment requires a compatible operational control service.

The [client and origin API](CLIENT_ORIGIN_API.md) defines the separate HTTP/1.1
Unix-socket read contract. The [Go SDK design](../../designs/racer-sdk.md) specifies
its streaming client, origin callback, resource defaults, and implementation plan.

## Authority and membership

The controller watches Kubernetes and publishes identical topology inputs to every
node. Node IDs and cache IDs are Kubernetes Node and ClusterCache UIDs, respectively;
recreation creates a new identity. Deployment supplies a persistent cluster UUID
and bootstrap CA bundle. Certificates and publications are scoped to that cluster.

Membership inputs come entirely from Kubernetes:

| Input | Source |
| --- | --- |
| Shares | Node annotation `racer.unbounded-cloud.io/shares`: decimal positive u32, default 4 |
| Peer endpoint | Non-terminating Racer Pod IP and controller-managed DaemonSet peer port |
| Rails | Node annotation `racer.unbounded-cloud.io/rails`: JSON array described below; default empty |
| Alignment | Node annotation `racer.unbounded-cloud.io/aligned-rails`: `true` (default) or `false` |
| Exclusion | Presence of Node label `racer.unbounded-cloud.io/exclude`; also excludes DaemonSet scheduling |

Rails use `[{"rail":0,"fabric":"fabric-a","numa_node":0}]`: unique u16 rail
IDs, shared fabric names, and optional node-local NUMA IDs. Dataplanes validate
mappings against local hardware; absent/incompatible mappings use HTTP. Discovery
never changes published topology. Endpoints are IP:port strings (IPv6 bracketed).
During Pod overlap, select the newest by creation time, then UID. Retain the last
endpoint during Pod gaps; a new node waits for its first endpoint. Pod readiness
and disconnection do not change ownership. Invalid annotations reject the proposed
update with a controller diagnostic, preserving accepted state.

Accepted member history is memory-only. After controller recovery, a node without
an eligible endpoint or with invalid annotations and no accepted values is omitted
until its inputs are usable. Missing annotations still use their defaults. Recovery
can therefore change membership; no accepted-member checkpoints are persisted.

This intentionally replaces `tmp/design.md`'s shares environment variable. There
is no dataplane shares/alignment override, node-report API, or status-write API.

## Two HTTPS operations

| Operation | Authentication and result |
| --- | --- |
| `POST /v1/bootstrap` | Server-authenticated TLS plus bearer service-account token; returns 200 with the node certificate chain and resolved Node UID |
| `GET /v1/snapshot?after=<sequence>` | mTLS; returns 200 and the newest full publication, or 204 after a 30-second wait |

JSON uses snake_case fields, decimal strings for u64 counters, and padded standard
base64 for bytes. UUIDs use canonical lowercase hyphenated text. DER encodes CSRs
and certificates. Missing `after` requests the current snapshot immediately.
Only one poll is outstanding per node. Enrollment bodies, publications, and bundles
carry `schema_version: 1`. Reject unknown versions, duplicate JSON fields/identities,
invalid values, and unknown enum variants; ignore unknown object fields.
Limits: bootstrap request/response 64 KiB, shared keyring bundle 512 KiB, publication
64 MiB and 100,000 members. Enforce byte limits before allocating decoded state.

Failures have `{ "code": "..." }`: 400 `invalid_request`, 401 `unauthenticated`,
403 `forbidden`, 409 `conflict` (including a future cursor), 413 `too_large`,
426 `unsupported_version`, 429 `overloaded`, or 503 `unavailable`. Back off on
transport errors/429/503 with jitter, exponentially from 1 to 30 seconds; honor
`Retry-After`. Other errors require corrected input/credentials. Normal 204 polls
may immediately repeat. Keep tokens and key material out of diagnostics.

## Publication lifecycle

A publication contains cluster ID, schema version, sequence, membership version,
members, and cache definitions (UID, resource name, client/origin socket paths,
socket mode). Socket paths must be `/run/racer/<cache name>/client/socket` and
`/run/racer/<cache name>/origin/socket`; validate the name as a single safe path
component and enforce the platform UDS path-length limit. Separate endpoint
directories support independently authorized pod mounts.
Both counters start at 1 and persist across leader changes/restarts. Every update
advances sequence; member-input changes also advance membership version. Cache-only
updates do not. Endpoint/rail updates cannot move ownership: placement uses only
node IDs and shares. Controller instances serialize durable updates through the
leader. Loss of counter state requires a new cluster identity and explicit rebootstrap.

Persist only a small version ConfigMap: cluster ID, both counters, and canonical
publication-content/membership-content hashes (excluding counters). Reconstruct
goal state from synchronized Kubernetes inputs. Compare hashes, commit changed
counters with resource-version preconditions, then expose the publication. Never
serve uncommitted counters. Initial creation is explicit cluster initialization;
missing established state must not silently recreate counters. No publication
blobs, member history, or checkpoint chunks are persisted. Only the initialized
leader serves; leadership loss closes connections and cancels long polls.

Snapshots are complete replacements; reconnects may skip intermediate updates.
Validate bounds, identities, versions, paths, and resource availability before
atomically accepting. Equal sequence is an idempotent replay only of identical
state; lower sequence or conflicting state is rejected. Retain the last accepted
snapshot on failure. Cache removal drains its listeners and outstanding work.
New work uses current membership; pinned work may retain bounded older versions.
Unknown/evicted memberships fail explicitly. Disconnection preserves accepted
state; expired credentials prohibit new authenticated work. Cluster mismatch
requires explicit rebootstrap. Acceptance/reload failures are local diagnostics.

## Bootstrap and local signing identity

Generate Ed25519 private keys locally. Persist them in a node-private directory
outside projected Secrets. Submit cluster ID, a UUID enrollment ID, and DER CSR
with a projected token whose audience is `racer-control`. The controller performs
TokenReview and checks the live bound Pod UID, its authorized service account and
managed workload, and its assigned Node. It resolves the Node UID authoritatively;
CSR SANs and caller-supplied identities are never authority. The request contains
no Node UID, allowing first startup with only a projected token and public trust.

The response contains schema_version, cluster, node, enrollment, and
certificate_chain (a leaf-first array of base64 DER certificates). Enrollment IDs
correlate replies with locally persisted private keys. Retries may issue equivalent
certificates; there is no durable receipt ledger or global enrollment-ID conflict
check. Certificates bind identity using URI SAN
`spiffe://<cluster-uuid>/node/<node-uid>` and permit control client authentication
and peer signatures. They last 24 hours; begin renewal with a fresh key at 16 hours.
Validate response correlation, chain, identity, validity, and local key pairing
before persisting/activating. Initial bootstrap, renewal, and expired-certificate
recovery all use the same token-authenticated endpoint with a fresh projected
token. Old verification material serves already-admitted traffic.

The HTTPS listener verifies client certificates when supplied; the snapshot route
requires a verified node identity. Recheck validity/authorization on every request,
including pooled connections, and bound long polls by certificate expiration.
Bootstrap can connect without a client certificate, including recovery from an
expired identity. There are no control HTTP signatures or challenge endpoint.
TLS authenticates responses. Peer HTTP retains its signature/replay machinery.

## Shared projected keyring

One common Secret carries bundle.json for all dataplanes. It contains schema/cluster
identity, increasing generation, peer trust roots, and
cache-scoped keys (`prepared`, `active`, `retiring`). Keys carry IDs, purposes
(`page`, `origin_credentials`), and 32-byte material. Exactly one active key per
cache/purpose is allowed. Node certificates and private keys are never in this
bundle; local signing identity rotates independently. Prepared keys can
decrypt received ciphertext but cannot encrypt new fills. Peer trust updates do
not replace deployment bootstrap trust. Read one coherent projected generation;
malformed updates retain the last valid bundle. Reject generation rollback or
conflicting replay; equal generation with identical content is idempotent.

Stage replacements before activation, targeting daily rotation without waiting
for acknowledgments. Missing previously installed keys initiate local retirement:
stop new use, evict dependent ciphertext/checkpoint references, drain leases, fence
late writes, then release material. Projection removal cannot bypass that ordering.
On restart discard records whose keys are unavailable. Lagging nodes fail operations
requiring missing keys; uninterrupted interoperability during rotation is not
guaranteed. Snapshot and bundle generations advance independently; admit cache
operations only when both configuration and required credentials are usable.
