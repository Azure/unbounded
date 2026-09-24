# Racer authorization and representation contract

This is a direct format cutover. Deploy matching SDK, origin adapter, and
dataplane versions. No earlier descriptor, metadata, or slab format is accepted.

## Local SDK and origin HTTP

- Send at most one `Authorization` field. Its value is optional, opaque, and
  immutable for the request, including all metadata/page requests and retries.
  A present value must contain 1 through 65,536 ASCII bytes in `0x20..0x7e`,
  with no leading/trailing whitespace. There is no scheme parsing. The wire
  accepts either no space or one space after the colon. Duplicate fields,
  empty values, tabs, control bytes, and non-ASCII values are rejected with 400;
  oversized values are rejected with 431.
- All request bytes other than the Authorization field line have a separate
  8,192-byte budget. The field line has at most 17 framing bytes in addition
  to its value. There are at most 64 fields. Receive buffers start at 8 KiB,
  grow on demand, and cannot exceed 73,745 bytes. Responses remain bounded
  to 8 KiB of headers.
- Authorization travels in the normal HTTP header, never the target. Only
  explicitly supported metadata is forwarded; this is not a header proxy.
- Backend HEAD must supply Content-Length and a canonical strong ETag consisting
  of a quoted 64-character lowercase hexadecimal checksum. Backend range GET
  receives Authorization, Range, If-Match, and Accept-Encoding: identity.
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
- Cache hits are content-addressed and credential-independent. Racer is an
  authorization pass-through, not an authorization decision cache. Network
  flights are isolated by process-keyed BLAKE3 credential fingerprints and
  expected Content-Type. Slab
  keys, checksums, page placement, and routing remain credential-independent.

## Metadata record and slab

The metadata body is exactly **306 bytes**, with no version prefix:

| Offset | Length | Value |
| --- | --- | --- |
| 0 | 32 | Raw checksum |
| 32 | 8 | Object length, little-endian u64 |
| 40 | 8 | Expiry Unix seconds, little-endian u64; zero means request-scoped |
| 48 | 2 | Content-Type byte length, little-endian u16; zero means absent |
| 50 | 256 | Content-Type bytes followed by mandatory zero padding |

The slab magic is `RACERS06`, tree magic `RACERN06`, and leaf entries are
346 bytes (32-byte key, 8-byte kind, 306-byte record). Tree fanout is 11.
Payload extents remain independent 64 MiB pages. Use a fresh slab for this format.

## Peer HTTP descriptor and request binding

`X-Racer-Fault` is hexadecimal encoding of a descriptor bounded to 3,500 decoded
bytes. RF04 budget and RF06 cursor framing retain their existing layouts. The
inner descriptor is now **RF08**:

- Metadata: `RF08`, byte 0, then target bytes.
- Page: `RF08`, byte 1, LE u64 offset, LE u64 object length, 32 checksum bytes,
  the 258-byte Content-Type length/padding field above, then target bytes.
  The page target starts at offset 311.

An outer RF06 chain wraps the RF04 budget and binds the immutable namespace
and forwarding allowances: `RF06 | namespace:32 | hops:u8 | work:u8 |
candidate:LE-u32 | RF04...`. The inner RF06 cursor remains placement-specific.
Each new metadata or page resolution receives at most eight hops and 255 units
of work. Forwarding splits work between the child and local recovery; admission
retries do not spend it. Placement rebasing and transport retries never renew it.

Payload receive admission reserves slots by the resolution's initial local hop
allowance, independent of placement. A newly originated payload chain caps its
hops to `min(8, NUMA pool slots - 1)` before its first receive. A received chain
keeps its allowance unchanged, including after rebasing. Forwarding decrements
that allowance; local retries retain their original admission rank even after
spending hops. A peer-dependent receive whose rank cannot fit the local pool
fails with local-capacity Busy instead of clamping the received rank. Local-owner
fetches and completed cache hits need no forwarding reserve. Thus a four-slot
pool supports three-hop cold payload paths; extra rebases or recovery attempts
can exhaust that bounded chain. Metadata does not consume payload slots or use
this capacity cap. Mixed fleets and heterogeneous pool sizes may reject payload
chains originated with a larger allowance; this does not authorize origin
fallback or prove owner unavailability.

Distributed client target admission reserves 410 bytes for the largest page,
cursor, budget, and chain framing, allowing **3,090 target bytes**.

`X-Racer-Attempt` is 96 hexadecimal characters: a 32-byte BLAKE3 digest followed
by a random 16-byte nonce. Hash input is the concatenation of:

1. ASCII `racer/request-binding/v2` (no terminator).
2. LE u32 descriptor length, then the complete decoded descriptor, including
   its outer RF06 chain when present.
3. LE u32 Authorization value length, then its bytes (zero length if absent).

Ingress verifies the digest against the descriptor and normal Authorization
header after authenticating the TLS peer. Failure reports echo this attempt.

## RDMA control request

Application metadata is an **RF07** envelope inside encrypted TLS control,
never DMA-exposed memory:

`RF07 | descriptor_len:LE-u16 | auth_len:LE-u16 | descriptor | auth`

The descriptor includes the RF06 chain and RF04/RF06/RF08 framing. Zero auth length means absent.
The 4,096-byte control frame has a 112-byte transport header, leaving **3,984
bytes** for this envelope. RDMA is selected only when
`8 + descriptor_len + auth_len <= 3984`; otherwise HTTP is selected before
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
