# Racer control API (v1)

This is the contract for the Go controller and Rust dataplane. Types and service
interfaces define the Rust enrollment, TLS publication, and credential lifecycle.
The Rust dataplane implements these operations independently of the server work.
The Go controller implements bounded wire codecs, canonical content hashing,
token bootstrap, and HTTPS/mTLS serving.

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

Accepted member history is retained in a bounded, Node-UID-bound annotation after
publication. Controller recovery can retain admitted endpoints through Pod gaps;
Node deletion, UID replacement, and exclusion cannot inherit that identity.
Authenticated enrollment proposes `RACER_SHARES` (default four). The controller
records the proposal; an explicit Node shares annotation wins. Local configuration
never independently changes the topology used for placement.

## Three public HTTPS operations

| Operation | Authentication and result |
| --- | --- |
| `POST /v1/bootstrap` | Server-authenticated TLS plus bearer service-account token; returns 200 with the node certificate chain and resolved Node UID |
| `GET /v1/snapshot?after=<sequence>` | mTLS; returns 200 with the newest publication or negotiated delta, or 204 after a 30-second wait |
| `GET /v1/keyring?after=<generation>` | mTLS in steady state; server-authenticated TLS plus a live-authorized `racer-control` bearer token for bootstrap/recovery; returns the current bounded JSON bundle |

JSON uses snake_case fields, decimal strings for u64 counters, and padded standard
base64 for bytes. UUIDs use canonical lowercase hyphenated text. DER encodes CSRs
and certificates. Missing `after` requests the current snapshot immediately.
Only one poll per route is outstanding per node. Snapshot and keyring polls have
independent cursors, admission, and progress loops. Enrollment bodies, publications, and bundles
carry `schema_version: 1`. Reject unknown versions, duplicate JSON fields/identities,
invalid values, and unknown enum variants; ignore unknown object fields.
Limits: bootstrap request/response 64 KiB, shared keyring bundle 512 KiB, publication
64 MiB and 100,000 members. Enforce byte limits before allocating decoded state.

Codec details: required fields cannot be absent or null; `numa_node` is optional
but cannot be null. Empty collections encode as `[]`. Counters are positive,
canonical decimal strings (no sign, leading zero, fraction, or exponent). Numeric
fields are unsigned integer JSON tokens. Base64 must include canonical padding
and zero padding bits, without whitespace. UUID syntax is checked for every
identity; UUID version bits are not restricted. Reject invalid UTF-8, unpaired
UTF-16 escapes, and JSON nesting deeper than 64 containers, including in ignored
fields. Field names are case-sensitive, and escaped equivalents count as duplicate
fields. Errors from codecs contain only protocol codes, never supplied values.

DER codecs check CSR/certificate syntax and reject trailing DER. Proof of
possession, certificate trust, expiry, key algorithm, and caller authorization
remain the enrollment/TLS layer's responsibility. Rails have unique IDs per node
and nonempty fabric names without NUL, CR, or LF. Endpoint ports are nonzero and
IP zone identifiers are rejected. Cache names are lowercase DNS subdomains with
labels of at most 63 bytes; each complete socket path is at most 107 bytes (plus
the Linux pathname socket's NUL terminator).

Failures have `{ "code": "..." }`: 400 `invalid_request`, 401 `unauthenticated`,
403 `forbidden`, 409 `conflict` (including a zero cursor), 413 `too_large`,
426 `unsupported_version`, 429 `overloaded`, or 503 `unavailable`. Back off on
transport errors/429/503 with jitter, exponentially from 1 to 30 seconds; honor
`Retry-After`. A cursor ahead of the contacted replica returns 503 `unavailable`;
retry without rolling back accepted or pending state or forcing reenrollment.
Certificate renewal remains independently lifetime-driven. Other errors require corrected
input/credentials. Normal 204 polls may immediately repeat. Keep tokens and key
material out of diagnostics.

## Publication lifecycle

A publication contains cluster ID, schema version, sequence, membership version,
members, and cache definitions (UID, resource name, client/origin socket paths).
Socket paths must be `/run/racer/<cache name>/client/socket` and
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
blobs or checkpoint chunks are persisted in that ConfigMap. Accepted member
history is stored separately on each Node. Only the leader reconciles topology and
rotates keys, but every synchronized replica serves public requests. Followers
obtain full images through authenticated internal replication, recompute canonical
hashes, and confirm exact counters/hashes against authoritative durable state
before installing. A hash alone never authorizes reconstructing or serving bytes.
Replica serving is process-scoped, not tied to the publisher's election context.
Election loss still stops the old leader process and its connections; other
synchronized replicas continue subject to local freshness and trust gates.

Background observations independently refresh validated credentials and confirm
the installed image. The default `RACER_SNAPSHOT_MAX_AGE=30s` bounds serving since
the last authoritative confirmation. Matching unchanged durable state can renew
freshness without a new publication; a newer durable record or replication 204
does not renew an older image. Transient API/replication failures retain accepted
state only within that bound. Observed invalidity, deletion, rollback, or conflict
fails closed immediately; read outages cannot restore withdrawn trust. Stale
replicas withdraw readiness and public requests return unavailable.

Each process retains an authoritative version high-water mark independently of
installed image bytes, including before the first image and across suspension.
Rollback or conflicting counters/hashes cannot be hidden by a lagging image.
An admitted snapshot response pins its freshness deadline; later confirmations
do not extend that response. Superseding its image or suspending authority revokes
the response, even if authority recovers before its next write. Blocked writes and
flushes are bounded by that pinned deadline, certificate/token expiry, and the write
timeout. Poll and write admission remain held through explicit response flush.
Public snapshot and bootstrap responses also pin a separate trust freshness
deadline and revocable trust generation. Trust invalidation cancels active writes
even if publications remain fresh or trust immediately recovers. Normal validated
rotation permits already-admitted responses to finish within their original
deadlines; new requests authenticate against current trust.

Controllers also expose `GET /internal/v1/snapshot` on the same TLS listener before
public readiness, avoiding replication startup deadlock. This is not a dataplane
operation. Only the leader answers, after TokenReview of a Pod-bound token with
audience `racer-controller-replication` and live controller Pod/ServiceAccount UID
checks. A dataplane token or Node certificate alone does not authorize this route.
Followers discover the leader directly from the unexpired Lease's Pod name/UID,
verify the live Pod, and dial its IP using deployment CA trust and the controller
Service DNS name for TLS verification. Internal responses are full snapshots,
not negotiated deltas.

Clients may send `X-Racer-Delta-Base` with their canonical publication hash. The
controller shares one bounded predecessor delta across clients when it is smaller
than the full snapshot. Delta v1 binds cluster, base sequence/hash, target
sequence/hash, member upserts/removals, and cache definitions. A missing base,
restart, follower response, or skipped generation falls back to a full snapshot.
Receivers validate the reconstructed canonical hash before installation. The 4 MiB delta cap does not
replace the full publication cap. TLS connections are reused within an unchanged
trust/identity epoch. Authenticated connections have a per-dial jittered maximum
reuse age of four to five minutes, allowing later requests to reach other ready
replicas. Crossing that age does not interrupt an active response; it prevents
subsequent reuse. Idle timeout, credential expiry, and trust/identity changes can
retire a connection sooner. Credential renewal and keyring delivery remain independent of
pending publication installation.

Canonical content uses compact UTF-8 JSON, with no whitespace or trailing newline.
The publication-content object's field order is `schema_version`, `cluster`,
`members`, `caches`; the membership-content object omits `caches`. Neither object
contains counters. Members sort by Node UID, caches by cache UID, and rails by
numeric rail ID. Member field order is `node`, `shares`, `peer_endpoint`, `rails`,
`alignment_enabled`; rail field order is `rail`, `fabric`, optional `numa_node`;
cache field order is `id`, `name`, `client_socket`, `origin_socket`.
JSON strings use short escapes for backspace, tab, newline, form feed, and carriage
return; remaining control characters use lowercase `\u00xx`. Quote and backslash
are escaped, as are U+2028/U+2029; other Unicode and `<`, `>`, `&` remain literal.
Hashes are lowercase hexadecimal SHA-256 of those bytes. All member fields affect
both hashes; cache-only changes affect only publication content. Input order and
counter changes affect neither hash. Hashing accepts zero candidate counters so
the controller can compare content before assigning committed versions.

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
TokenReview locally on the receiving ready replica and checks the live bound Pod
UID, its authorized service account and managed workload, and its assigned Node.
It resolves the Node UID authoritatively;
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
token. Bootstrap and renewal are not proxied to the leader; authoritative signing
state reads remain required, so cached snapshot continuity does not enable offline
enrollment. Old verification material serves already-admitted traffic.

The HTTPS listener verifies client certificates when supplied; the snapshot route
requires an exact same-cluster Node identity and a currently valid chain under
controller-installed validated trust. Both TLS handshakes and snapshot requests
use local trust with zero Kubernetes calls. Recheck the chain, identity, and
validity before and after every poll, including pooled connections, and bound
polls and writes by certificate expiration. Kubernetes Node/Pod/DaemonSet/ServiceAccount
authorization is required at enrollment, renewal, and bearer-authenticated keyring
bootstrap/recovery. Steady-state keyring mTLS uses the same local authorization
as snapshot polling. Deletion, recreation,
exclusion, and membership removal do not revoke an issued certificate: membership
is routing, not authorization. Any authenticated same-cluster node may connect.
Controller reconciliation installs trust updates and withdraws trust after observed
invalid or deleted durable authority. An API read outage may retain accepted local
state only within the replica freshness bound, but cannot restore previously
withdrawn trust.
Bootstrap can connect without a client certificate, including recovery from an
expired identity. There are no control HTTP signatures or challenge endpoint.
TLS authenticates responses. Peer HTTP uses separate certificate-authenticated v2
connection sessions with signed, strictly ordered immediate-hop heads. It adds no
payload TLS encryption; see `designs/racer-peer-security.md`.

## Shared keyring over control HTTPS

The controller retains one durable common Secret containing `bundle.json`; it is
not mounted into dataplane Pods. `GET /v1/keyring` returns that existing JSON wire
bundle, bounded to 512 KiB, never issuer private material or rotation metadata.
Without `after`, return the current bundle immediately with 200. An optional
`after` is a positive canonical decimal generation (no zero, sign, whitespace,
leading zero, or duplicate/unknown query parameters). A lower generation returns
the newest bundle with 200; equal waits up to 30 seconds and returns 204 if unchanged;
a future generation returns 409. This is a full replacement, not a delta.

Before enrollment and during expired-identity recovery, fetch using the projected
`racer-control` bearer token over server-authenticated HTTPS without a client
certificate. TokenReview and live bound Pod, ServiceAccount, DaemonSet, and Node
checks authorize access; a raw JWT claim is not authority. Install validated peer
trust from the fetched bundle before accepting a newly issued identity. Once a
valid identity is active, use mTLS for keyring polling. Keep topology polling,
keyring polling, and renewal independent so an unchanged topology cannot block
rotation. Retry delivery failures with bounded jittered backoff while retaining
the last accepted bundle; never fall back to a keyring file or log response bodies.

The bundle contains schema/cluster
identity, increasing generation, peer trust roots, and
cache-scoped keys (`prepared`, `active`, `retiring`). Keys carry IDs, purposes
(`page`, `origin_credentials`), and 32-byte material. Exactly one active key per
cache/purpose is allowed. Node certificates and private keys are never in this
bundle; local signing identity rotates independently. Prepared keys can
decrypt received ciphertext but cannot encrypt new fills. Peer trust updates do
not replace deployment bootstrap trust. Accept one complete validated response;
malformed updates retain the last valid bundle. Reject generation rollback or
conflicting replay; equal generation with identical content is idempotent.

Bundle JSON fields are `schema_version`, `cluster`, `generation`,
`peer_trust_roots`, `cache_keys`. Each key object contains `cache`, `id` (16 bytes),
`purpose`, `state`, and `material` (32 bytes). Key identity is scoped by cache and
purpose; duplicate identities and duplicate trust-root DER are rejected. Each
represented cache/purpose must have exactly one active key, alongside any prepared
or retiring keys. An empty cache-key list is valid. Trust roots and bootstrap
certificate chains must be nonempty. Bundle and chain array ordering is preserved;
publication sorting does not apply to them. The Go bundle codec is the only JSON
material encoder; ordinary key formatting and JSON never expose material.

Shared executable contract vectors live in `internal/racer/wire/testdata/` and
are consumed by both Go and Rust tests. Bundle keys there are synthetic all-zero
or repeated-byte test values, and the certificate/CSR contain only public data.

Stage replacements before activation, targeting daily rotation without waiting
for acknowledgments. Retiring or missing keys stop new leases. Accepted operations
retain zeroizing secret leases until completion, independently of ciphertext I/O
and native DMA resource fences. Historical keyless ciphertext/checkpoints may remain.
On restart discard unavailable cache UIDs and keys before installation, including
standalone metadata when its UID or active page key is unavailable. Lagging nodes
fail operations
requiring missing keys; uninterrupted interoperability during rotation is not
guaranteed. Snapshot and bundle generations advance independently; admit cache
operations only when both configuration and required credentials are usable.
