# Dataplane configuration

`Config::from_env()` parses base settings and calls `Config::validate()`.
Both are side-effect-free with respect to files, sockets, pools, and threads.
The executable uses `Config::from_env_with_fabric_ports()` to additionally load
trusted native associations, including a projected file when explicitly selected.
Malformed settings return `Error::InvalidConfiguration` without including values
in diagnostics. Optional settings default only when absent; empty is invalid.
Integers are unsigned decimal with no sign, whitespace, or unit suffix. Byte
quantities are bytes, and timeout quantities are integer milliseconds. Booleans
are exactly `true` or `false`. Known settings must contain valid Unicode and at
most 4096 bytes, with no control characters.

## Deployment and identity

| Environment variable | Default | Contract |
| --- | --- | --- |
| `RACER_CLUSTER_ID` | Required | Persistent non-nil, canonical lowercase hyphenated UUID |
| `RACER_CONTROL_ENDPOINT` | Required | HTTPS authority, optional port and single trailing slash |
| `RACER_MAX_THREADS` | `8` | Total userspace thread cap, 2 through 256; odd caps floor to complete pairs |
| `RACER_ENABLE_RDMA` | `false` | Optional RDMA; hardware/capability failure retains HTTP fallback |
| `RACER_PEER_LISTEN` | `0.0.0.0:7443` | Numeric local IP:port, IPv6 bracketed |
| `RACER_DIAGNOSTICS_LISTEN` | `127.0.0.1:9090` | Numeric local IP:port, loopback by default |
| `RACER_TRUST_BUNDLE` | `/etc/racer/trust/ca.crt` | Deployment bootstrap/server CA file |
| `RACER_SERVICE_ACCOUNT_TOKEN` | `/var/run/secrets/racer-control/token` | Projected token file with audience `racer-control` |
| `RACER_SECRET_DIRECTORY` | `/etc/racer/keys` | Projected common keyring directory containing `bundle.json` |
| `RACER_IDENTITY_DIRECTORY` | `/var/lib/racer/identity` | Node-private persistent signing identity directory |
| `RACER_SLAB_DIRECTORY` | `/var/lib/racer/slabs` | Persistent encrypted slab/checkpoint directory |

The control endpoint accepts ASCII DNS names, IPv4, or bracketed IPv6. It rejects
credentials, paths other than `/`, queries, fragments, percent escapes, whitespace,
invalid hosts/ports, and unspecified/multicast/broadcast IPs. There is no insecure
TLS option. Listener ports must be 1 through 65535; multicast, broadcast,
IPv4-mapped IPv6, and scoped IPv6 addresses are rejected. Equal listener ports
are rejected if addresses match or either address is unspecified, conservatively
covering dual-stack wildcard conflicts. A deployment exposing diagnostics outside
loopback must explicitly change its address. The controller-managed peer port
must match `RACER_PEER_LISTEN`; config does not publish membership endpoints.

All five paths must be absolute, non-root, canonical lexical paths: no empty,
`.` or `..` components, repeated/trailing slashes, controls, components exceeding
255 bytes, or paths exceeding 4095 bytes. None may equal, contain, or be contained
by another configured path. Shared parents are fine. Validation does not resolve
symlinks, inspect mounts, read trust/tokens, check permissions, or create paths.
Filesystem owners must enforce file type, ownership, permissions, and actual
separation at open time while supporting Kubernetes projection symlinks for
read-only trust/token/keyring inputs. Private identities must stay separate from
projected Secrets and slabs.

There is no Node identity override. `from_env` sets `config.node` to
`NodeId(UNRESOLVED_NODE_ID.into())`, where `UNRESOLVED_NODE_ID` is the empty string.
This is deliberately not a valid UID. `validate` accepts this pre-bootstrap state
or a syntactically valid non-nil UUID, but cannot authenticate an identity.
Only verified bootstrap or verified local identity recovery may replace the
sentinel. Do not replace it with the node name, a random UUID, or an environment UID.

`RACER_NODE_UID`, `RACER_NODE_ID`, `RACER_SHARES`, `RACER_RAILS`, and
`RACER_ALIGNED_RAILS` are rejected if present, even when empty. Shares, rails, and
alignment come from accepted controller membership. Other unrelated environment
variables are ignored. CPU/cpuset/quota discovery belongs to runtime
`AffinityPlan`; config neither reads CPU files nor overrides allowed CPUs.

## Trusted local native associations

| Environment variable | Default | Contract |
| --- | --- | --- |
| `RACER_FABRIC_PORTS` | Absent (no associations) | JSON array, at most 4096 bytes with no literal control characters |
| `RACER_FABRIC_PORTS_FILE` | Absent | Absolute path to a read-only operator-managed JSON file, at most 65536 bytes |

The two sources are mutually exclusive, even if either value is empty. An empty
environment value or file is invalid; `[]` explicitly selects no associations.
The file path follows the lexical path rules above and must not contain or live
inside the identity or slab directories. It must resolve to a readable regular
file. Kubernetes projection symlinks are supported; one opened descriptor is read
once before application startup, with a hard byte cap even if the file grows.
JSON whitespace/newlines are allowed in files. Changes require process restart.
Mount the file read-only, readable by the runtime UID, with write access limited
to trusted deployment administrators. It is local physical configuration, not a
controller publication or a source of rail/alignment authority.

Example inline value (replace every association with verified local values):

```sh
export RACER_FABRIC_PORTS='[{"fabric":"fabric-a","device":"mlx5_0","port":1,"gid":"fe800000000000000000000000001234"}]'
```

Each entry accepts exactly `fabric`, `device`, `port`, and optional `gid`:

- `fabric`: nonempty opaque UTF-8 label, at most 4096 bytes, no control characters
  or leading/trailing ASCII spaces. Compared exactly to authenticated membership;
  labels are never inferred from NIC names, GIDs, or enumeration order.
- `device`: 1-63 ASCII bytes; first character alphanumeric, remaining characters
  alphanumeric, `_`, `.`, or `-`; `..` is forbidden. This is the native device name.
- `port`: JSON integer from 1 through 255.
- `gid`: optional (omitted or `null` means no GID pin), exactly 32 lowercase hex
  digits in network byte order; zero and multicast GIDs are invalid. A configured
  GID pins matching against discovery; it does not select a GID table index. The
  current adapter discovers index zero. Discovery must still be unambiguous when
  the pin is omitted.

At most 64 entries are accepted. Duplicate fabric labels and repeated physical
`(device, port)` pairs are rejected even when their GIDs differ. Unknown or
duplicate JSON fields, invalid numeric types, and malformed/trailing JSON fail
startup with `InvalidConfiguration`, including when RDMA is disabled. File read
errors also fail startup rather than silently discarding an explicit mapping.

Main forwards associations through `Application::with_fabric_ports`. Activation
still requires the Rust `rdma` feature, `RACER_ENABLE_RDMA=true`, native resources
and quota, and authenticated local rail/alignment membership. Native matching
requires exactly one association and discovered port for each published fabric,
including GID and published NUMA constraints. Extra unpublished associations do
not create rails. Missing or incompatible hardware/publication retains HTTP
fallback. `Config::from_env()` alone does not load these separate associations;
embedders must explicitly supply them to the builder.

## Storage and timeouts

| Environment variable | Default | Validation |
| --- | --- | --- |
| `RACER_SLAB_BYTES` | `1073741824` (1 GiB) | Positive, at most `i64::MAX`, exact multiple of segment size |
| `RACER_SEGMENT_BYTES` | `67108864` (64 MiB) | Fits a 16 MiB page plus 16-byte AEAD tag and `store::format::MAX_HEADER_BYTES` |
| `RACER_FREE_SEGMENT_RESERVE` | `2` | Positive and strictly smaller than slab segment count |
| `RACER_REQUEST_TIMEOUT_MS` | `30000` | 1 through 86400000 |
| `RACER_READER_STALL_TIMEOUT_MS` | `10000` | 1 through request timeout |
| `RACER_SHUTDOWN_TIMEOUT_MS` | `30000` | 1 through 3600000 |

`RACER_REQUEST_TIMEOUT_MS` also bounds incoming peer HTTP headers. Each exchange
starts one fixed header budget, capped by the listener deadline and sharing its
cancellation, including idle time on accepted and reused keepalive connections.
Partial header bytes do not extend it. Expiry closes the connection; admission
charges remain held until outstanding I/O is fenced. Once the head is received,
authenticated peer dispatch and response transfer use the existing signed request
deadline capped by the listener deadline, not the header cap. Challenge and
handshake responses continue under the listener scope. This is a header-only
ingress limit; it does not renew or otherwise change the fixed read/range request
budget.

A slab has at most 1048576 segments. Geometry arithmetic and conversions are
checked before resource creation. The envelope bound is currently 16384 bytes,
so the minimum unpadded segment size is 16793616 bytes. Filesystem-specific direct
I/O address/offset/length alignment and padded-record fit must additionally pass
`store::slab` validation at open. Config does not guess that alignment.

## Node resource limits

All limits are strictly positive. Aggregate limits are node-wide ceilings, not
promised concurrency or allocations made by configuration. Runtime partitions
aggregate dimensions after affinity discovery; per-operation caps such as window,
headers, waiters per flight, connections per neighbor, and retained snapshots stay
unchanged. Replay has one shared node-wide table. MiB means 1048576 bytes.

| Environment variable | Default | Maximum |
| --- | --- | --- |
| `RACER_PLAINTEXT_BYTES` | `268435456` (256 MiB) | 64 GiB |
| `RACER_CIPHERTEXT_BYTES` | `268435456` (256 MiB) | 64 GiB |
| `RACER_DIRTY_BYTES` | `134217728` (128 MiB) | 64 GiB |
| `RACER_REGISTERED_BYTES` | `134217728` (128 MiB) | 64 GiB |
| `RACER_REQUEST_CONTEXT_BYTES` | `16777216` (16 MiB) | 64 GiB |
| `RACER_FLIGHTS` | `64` | 65536 |
| `RACER_WAITERS_PER_FLIGHT` | `64` | 4096 |
| `RACER_QUEUE_ENTRIES` | `256` | 65536 |
| `RACER_CONNECTIONS_PER_NEIGHBOR` | `2` | 1024 |
| `RACER_CLIENT_CONNECTIONS` | `128` | 65536 |
| `RACER_PIPES` | `16` | 65536 |
| `RACER_RANGE_WINDOW_PAGES` | `2` | 64 |
| `RACER_REPLAY_ENTRIES` | `262144` | 1048576 |
| `RACER_HEADER_BYTES` | `32768` | 32768 |
| `RACER_ROUTE_SEARCH_WORK` | `150000` | 1048576 |
| `RACER_CACHED_RANKINGS` | `128` | 1048576 |
| `RACER_CACHED_PATHS` | `128` | 1048576 |
| `RACER_RETAINED_SNAPSHOTS` | `2` | 64 |
| `RACER_METADATA_ENTRIES` | `4096` | 1048576 |
| `RACER_RELAY_TRANSFERS` | `16` | 65536 |

Replay capacity budgets the admitted envelope rate over the entire freshness
window, not just concurrent requests. Entries can remain live for 65 seconds
(60-second age plus 5-second future skew); requests, responses, handshakes, and
relay envelopes all consume the same node-wide budget. The default supports about
4032 admissions/second over that interval. The former 4096 default supported only
about 63/second and saturated under 64-client/node load even with cached handshakes.
Size explicit overrides for the node's aggregate peer rate, including bursts.
The table allocates as it grows, never evicts live entries, and still rejects new
nonces at the configured ceiling. This limit is not multiplied by worker count.

Route-search work counts edge examinations plus meeting-node comparisons, not
just members. The default covers healthy four-link searches at 100,000 members
and eight-link searches at 1,500 members. The former 4096 default could reject
healthy 1,500-member routes with `Overloaded` before any peer request, leaving
Gantry pulls failing with HTTP 503 even while local candidate reads succeeded.
Explicit lower limits remain hard bounds; larger failure searches may need a
higher setting. Searches still yield cooperatively and honor their deadlines.

Additional progress requirements (checked here for one worker; integration must
recheck them after partitioning and reduce the pair count if needed):

- Plaintext fits `(range_window_pages + 1) * PAGE_BYTES`.
- Ciphertext fits `(range_window_pages + 1) * (PAGE_BYTES + 16)` plus the maximum
  record header. This reserves an acquisition/record margin beyond a full window;
  direct-I/O padding is additionally checked against actual alignment at startup.
- Dirty bytes fit at least one full encrypted page. Registered bytes do likewise
  when RDMA is enabled; otherwise the registered limit remains positive but unused.
- Headers are at least 1024 bytes. Request context bytes are at least 128 KiB for
  an admitted head, bounded opaque fields, and credential sealing output/scratch.
- At least two queue entries and two retained snapshots allow data/control and
  current/replacement progress. Runtime must actually preserve progress reserves.
- Connections per neighbor cannot exceed client connections (the admission
  connection ceiling). Flights times waiters per flight cannot exceed 1048576.
- Each byte dimension fits `isize::MAX`; their sum fits `isize::MAX` and is at
  most 256 GiB, including optional RDMA bytes.
  Counted metadata, TLS, control publications, and kernel allocations are separate;
  this is not an RSS prediction.

## Integration order

1. Call `Config::from_env` (or `validate` for programmatically constructed config)
   before creating resources. Preserve all existing `Config` fields.
   The executable instead calls `from_env_with_fabric_ports`, then forwards the
   returned associations to `Application::with_fabric_ports` before `run`.
2. Discover and validate `AffinityPlan`, including actual CPU pairs, quotas, and
   thread accounting. Eight is a total thread default, not eight worker pairs.
3. On the sole control owner, validate trust and recover or bootstrap the signing
   identity. Replace `config.node` with the verified local identity's Node UID
   before constructing Keyring, candidate policy, or other identity-bound workers.
   An empty sentinel must never be sent as identity or used for admission/readiness.
4. Partition node-wide resource limits, recheck all progress floors per worker,
   and reduce pair count if necessary. Defaults fund four pairs. Wire partitioned
   `Limits` into `WorkerFactory::limits` and each worker admission/queue/table
   owner, rather than retaining factory fallback defaults. Apply configured
   request, reader-stall, and shutdown deadlines to the
   corresponding lifecycle operations. Config only validates these settings.
5. Enforce actual filesystem ownership/alignment and resource availability, recover
   storage, accept compatible membership and keys, and then enable listeners and
   readiness. Apply per-cache UDS paths from accepted publications; there is no
   configurable client/origin socket override.

The server and client were implemented independently. This
document describes configuration only; it does not claim the application lifecycle
or every consumer is fully integrated.
