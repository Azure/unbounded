# Racer control API (v1)

This is the contract for the Go controller and Rust dataplane. Types and service
interfaces are scaffolded; network, filesystem, and cryptographic operations still
return `Unimplemented`.

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

This intentionally replaces `tmp/design.md`'s shares environment variable. There
is no dataplane shares/alignment override, node-report API, or status-write API.

## Two HTTPS operations

| Operation | Authentication and result |
| --- | --- |
| `POST /v1/enroll` | Server-authenticated TLS plus bearer service-account token; returns 202 and an enrollment receipt |
| `GET /v1/snapshot?after=<sequence>` | mTLS; returns 200 and the newest full publication, or 204 after a 30-second wait |

JSON uses snake_case fields, decimal strings for u64 counters, and padded standard
base64 for bytes. UUIDs use canonical lowercase hyphenated text. DER encodes CSRs
and certificates. Missing `after` requests the current snapshot immediately.
Only one poll is outstanding per node. Enrollment bodies, publications, and bundles
carry `schema_version: 1`. Reject unknown versions, duplicate JSON fields/identities,
invalid values, and unknown enum variants; ignore unknown object fields.
Limits: enrollment request/receipt 64 KiB, credential bundle 512 KiB, publication
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

Snapshots are complete replacements; reconnects may skip intermediate updates.
Validate bounds, identities, versions, paths, and resource availability before
atomically accepting. Equal sequence is an idempotent replay only of identical
state; lower sequence or conflicting state is rejected. Retain the last accepted
snapshot on failure. Cache removal drains its listeners and outstanding work.
New work uses current membership; pinned work may retain bounded older versions.
Unknown/evicted memberships fail explicitly. Disconnection preserves accepted
state; expired credentials prohibit new authenticated work. Cluster mismatch
requires explicit rebootstrap. Acceptance/reload failures are local diagnostics.

## Enrollment and projected credentials

Generate Ed25519 private keys locally. Persist them in a node-private directory
outside projected Secrets. Submit cluster ID, Node UID, a UUID enrollment ID, and
CSR with a projected token whose audience is `racer-control`. The controller checks
the live bound Pod, its authorized service account, and its Node UID; it never
trusts the requested node identity alone. Reusing an enrollment ID with identical
input returns the same receipt; different input conflicts.

The receipt echoes identities and enrollment ID, not certificate bytes. The
controller delivers the certificate in that node's Secret as `bundle.json`.
Certificates bind identity using URI SAN
`spiffe://<cluster-uuid>/node/<node-uid>` and permit control client authentication
and peer signatures. They last 24 hours; begin renewal with a fresh key at 16 hours.
Validate chain, identity, validity, and local key pairing before activation. Initial
bootstrap and expired-certificate recovery use enrollment; other operations require
a valid certificate. Old verification material serves already-admitted traffic.

Each bundle carries schema/cluster/Node identity, increasing generation, enrollment-
tagged certificates (`pending`, `active`, `retiring`), peer trust roots, and
cache-scoped keys (`prepared`, `active`, `retiring`). Keys carry IDs, purposes
(`page`, `origin_credentials`), and 32-byte material. Exactly one active key per
cache/purpose and one active node certificate are allowed. Prepared keys can
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
