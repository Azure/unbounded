# Racer origin data and representation contract

Racer 1.0 is a clean break. Deploy matching control-plane, SDK, origin adapter,
and dataplane versions with fresh CA state and slabs. There is no migration or
mixed-version support. Historical pre-release formats numbered 1 are not the
1.0 contract.

## Version identifiers

Control subscriptions, enrollment, proof, and replica proof use `/v1/config`,
`/v1/enroll`, `/v1/proof`, and `/v1/replica-proof`. The protobuf package remains
`racer.control.v1` and the subscription profile is 1. Every volume references
an explicit member catalog, including volumes with no remote members. A supplied
topology must explicitly select routing algorithm 1 for physical product routing.
Only the 71-byte RR01 product cursor is accepted.

RDMA negotiation and offers use version 1, with `RCR1` transport frames. CA
state, trust bundles, identity claims, generation records, and revision checkpoint
formats are version 1. Cache namespaces use `racer-volume-v1`.

The Kubernetes API remains `racer.unbounded-cloud.io/v1alpha1`. Revision,
generation, epoch, and storage-policy counters retain their operational meanings.

## Local SDK and origin HTTP

- Send at most one `Racer-Origin-Data` field. Its value is optional, opaque, and
  immutable for the request, including all metadata/page requests and retries.
  It is canonical padded standard base64 of at most 65,536 arbitrary bytes.
  Empty means absent. There is no application schema parsing. The wire accepts
  either no space or one space after the colon. Duplicate fields and malformed
  or noncanonical base64 are rejected with 400; encoded values exceeding
  87,384 bytes are rejected with 431. Decoded overflow is rejected with 400.
- All request bytes other than the origin data field line have a separate
  8,192-byte budget. The field line has at most 21 framing bytes in addition
  to its value. There are at most 64 fields. Receive buffers start at 8 KiB,
  grow on demand, and cannot exceed 95,597 bytes. Responses remain bounded
  to 8 KiB of headers.
- Origin data travels in the dedicated HTTP header, never the target. Only
  explicitly supported metadata is forwarded; this is not a header proxy.
- Backend HEAD must supply Content-Length and a canonical strong ETag consisting
  of a quoted 64-character lowercase hexadecimal checksum. Backend range GET
  receives Racer-Origin-Data, Range, If-Match, and Accept-Encoding: identity.
  HEAD and GET Content-Type must agree, including absence.
- Optional `Content-Type` is a singleton with at most 256 value bytes. Optional
  `WWW-Authenticate` and `Retry-After` are singletons with at most 1,024 and 128
  value bytes respectively. These response values use HTTP field-value bytes
  (including obs-text), reject controls other than HTAB, and reject empty
  present values. HTTP boundary whitespace is trimmed. No value is truncated.
- Backend 401/403 remain 401/403 through peers and to the SDK. The challenge and
  Retry-After are preserved. Error bodies are discarded by closing the upstream
  connection, including chunked or close-delimited auth error responses. Auth
  outcomes never trigger owner reselection or unavailable-peer classification.
  A failure before response headers is emitted with zero body; a later streaming
  failure closes the stream because HTTP headers have already been sent.
- Cache hits are content-addressed and origin-data-independent. Origin data is
  input to origin fetches, not per-read authorization. Tenant or representation
  identity belongs in the namespace/target. Network flights are isolated by
  process-keyed BLAKE3 origin data fingerprints and
  expected Content-Type. Slab
  keys, checksums, page placement, and routing remain origin-data-independent.
  Origin data may contain secrets; never log, persist, or echo it in diagnostics.

## Metadata record and slab

The metadata body is exactly **306 bytes**, with no version prefix:

| Offset | Length | Value |
| --- | --- | --- |
| 0 | 32 | Raw checksum |
| 32 | 8 | Object length, little-endian u64 |
| 40 | 8 | Expiry Unix seconds, little-endian u64; zero means request-scoped |
| 48 | 2 | Content-Type byte length, little-endian u16; zero means absent |
| 50 | 256 | Content-Type bytes followed by mandatory zero padding |

The slab magic is `RACERS01`, tree magic `RACERN01`, and leaf entries are
346 bytes (32-byte key, 8-byte kind, 306-byte record). Tree fanout is 11.
Payload extents are independent 64 MiB pages. Bitmap and layout markers are
`RACERB01` and `RACERL01`. The managed path is `/cache/cache-v1.slab`.
Use a fresh slab for this format.

## Peer HTTP descriptor and request binding

`X-Racer-Fault` is hexadecimal encoding of a descriptor bounded to 3,500 decoded
bytes. Distinct four-byte tags identify version 1 of each layer: `RB01` budget,
`RR01` product cursor (algorithm 1), and `RD01` descriptor:

- Metadata: `RD01`, byte 0, then target bytes.
- Page: `RD01`, byte 1, LE u64 offset, LE u64 object length, 32 checksum bytes,
  the 258-byte Content-Type length/padding field above, then target bytes.
  The page target starts at offset 311.

An outer RC01 chain wraps the RB01 budget and binds the immutable namespace
and forwarding allowances: `RC01 | namespace:32 | hops:u8 | work:u8 |
candidate:LE-u32 | RB01...`. The inner cursor is placement-specific. Algorithm 1
uses physical member indexes for candidate attribution, with distinct ranked
physical candidates compiled per placement slot.
Each new metadata or page resolution receives at most eight hops and 255 units
of work. Forwarding splits work between the child and local recovery; admission
retries do not spend it. Transport retries never renew it.

Payload receive admission reserves slots by the resolution's initial local hop
allowance, independent of placement. A newly originated payload chain caps its
hops to `min(8, NUMA pool slots - 1)` before its first receive. A received chain
keeps its allowance unchanged. Forwarding decrements
that allowance; local retries retain their original admission rank even after
spending hops. A peer-dependent receive whose rank cannot fit the local pool
fails with local-capacity Busy instead of clamping the received rank. Local-owner
fetches and completed cache hits need no forwarding reserve. Thus a four-slot
pool supports three-hop cold payload paths; recovery attempts
can exhaust that bounded chain. Metadata does not consume payload slots or use
this capacity cap. Mixed fleets and heterogeneous pool sizes may reject payload
chains originated with a larger allowance; this does not authorize origin
fallback or prove owner unavailability.

Distributed client target admission reserves 436 bytes for the largest page,
cursor, budget, and chain framing, allowing **3,064 target bytes**.

RR01 has a fixed 71-byte body: topology identity (32 bytes), source member,
placement owner slot, candidate attempt (three LE u32 values), position (u8),
path length (u8), five physical member indexes (LE u32, unused entries all ones),
failed member (LE u32, all ones when absent), and repair position (u8).
Paths include endpoints and contain at most four successful physical edges.
Each receiver reconstructs the deterministic healthy path or its single local
repair and validates the entire path, selected candidate, and local position.
Product requests with a different topology identity fail closed; they do not
rebase and restart the four-edge allowance. Historical 45-byte RR01 bodies,
RR02 descriptors, algorithm 2, and slot-graph snapshots are rejected. Every
volume requires explicit product topology, including K1 singleton volumes.

Healthy product paths advance a distance-two coordinate first at every step.
All members in adjacent role bundles are physical neighbors. Same-role endpoints
use two edges through the lowest adjacent role. An initiated qualifying failure
of the immediate intermediate HTTP peer permits one same-candidate repair:
replace it with a surviving bundle twin or route around its role. The successful
prefix is retained and prefix plus repair remains at most four edges. RDMA first
recovers over same-peer HTTP. Repair never renews candidate deadlines, chain work,
hop allowances, or receive ranks. Destination failure retains origin-only
candidate advancement. Product flights are request-local to avoid repair cycles.
Each forwarding hop reserves 500 ms once in the child's wire budget; the local
exchange retains its existing candidate deadline. The owner separately reserves
500 ms for its backend response. Thus a 30-second request with eight candidates
starts with a 3.6875-second candidate window, leaving 1.1875 seconds before
transport elapsed time after four hop reserves and the owner's backend reserve.
Same-hop retries reuse the existing cap and spend existing chain authority.
The absolute consumer deadline is retained separately from private candidate
caps. An initiated final-peer HTTP timeout at a private cap is service-failure
evidence and permits origin-only advancement while caller time remains. HTTP
metadata, payload, and RDMA-to-HTTP recovery use the same distinction; actual
caller expiry never authorizes owner advancement or intermediate repair. RDMA's
speculative grant/read timeout still recovers over HTTP rather than proving an
owner failure. Local repair retains the current candidate cap, including expiry.
Repair evidence is fenced by the HTTP breaker permit generation: stale success
cannot erase newer failure evidence, and stale failure cannot undo recovery.
Variable-membership admission uses the shared product membership predicate:
K1 permits only one member, K2 permits only two members, and bundled roles
require base degree at least two. This rejects a singleton K2 bridge between
two members of the other role before serving requests. The exact-10,000
HS50 x Abas200 singleton assignment has neither this bundle degeneracy nor a
degree expansion: each physical node has 23 peers.

`X-Racer-Attempt` is 96 hexadecimal characters: a 32-byte BLAKE3 digest followed
by a random 16-byte nonce. Hash input is the concatenation of:

1. ASCII `racer/request-binding/origin-data/v1` (no terminator).
2. LE u32 descriptor length, then the complete decoded descriptor, including
   its outer RC01 chain when present.
3. LE u32 decoded origin data length, then its raw bytes (zero length if absent).

Ingress verifies the digest against the descriptor and decoded Racer-Origin-Data
header after authenticating the TLS peer. Failure reports echo this attempt.

## RDMA control request

Application metadata is an **RO01** envelope inside encrypted TLS control,
never DMA-exposed memory:

`RO01 | descriptor_len:LE-u16 | data_len:LE-u16 | descriptor | raw_origin_data`

The descriptor includes the RC01 chain and RB01/RR01/RD01 framing. Zero data length means absent.
The 4,096-byte control frame has a 112-byte transport header, leaving **3,984
bytes** for this envelope. RDMA is selected only when
`8 + descriptor_len + data_len <= 3984`; otherwise HTTP is selected before
creating an RDMA attempt or acquiring its breaker. This is intentional transport
selection, not a failed RDMA attempt. Metadata fetches use HTTP.

## Typed peer failure

The failure record is exactly **1,217 bytes**. Its first 61 bytes retain the
identity/candidate/reason/evidence layout. Reason values 11 and 12 mean
Unauthorized and Forbidden. Reason 13 means MetadataChanged (HTTP 412): reject
only the expected checksum/length/Content-Type identity, excluding expiry.
Reason 10 remains a checksum precondition failure. A metadata mismatch must not
reject another Content-Type with the same checksum or remove its cached HEAD.
Append at offset 61 a 1,026-byte challenge field
(LE u16 length plus 1,024 zero-padded bytes), then at offset 1,087 a 130-byte
Retry-After field (LE u16 length plus 128 zero-padded bytes).

HTTP hex-encodes the record in `X-Racer-Failure`, echoes `X-Racer-Attempt`,
and emits matching WWW-Authenticate/Retry-After headers with Content-Length: 0.
RDMA negative responses carry the existing 32-byte descriptor hash followed by
the failure record. All lengths and padding are validated. No credential is
included in the failure record.
