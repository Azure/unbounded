# Racer prefetch API

Status: approved. This document is the spec for the Rust server, Go SDK, and
Python SDK work. Where it disagrees with older prose, this document wins (see
[Superseded statements](#superseded-statements)).

## Summary

Racer's warming (HEAD `Cache-Control: max-age`, `Racer-Retain-Until`, hard
`HolderKind::Warm` holds, and the `warm` peer operation) is removed and replaced
by a JSON prefetch API on the existing per-cache client socket
(`/run/racer/<cache>/client/socket`). A client submits a ticket of object
ranges. The node schedules the missing pages to their owners, which fill them
through the normal flight table and persist them with normal eviction. No
pages are pinned. A ticket reports progress and per-ref outcomes. A rolling
window is the same ticket updated with higher sequence numbers.

Tickets are node-local and in memory. A restart loses them; the client
resubmits the same id and only missing pages are scheduled.

## Current code this replaces

- Client parser: only `HEAD` and `POST` under `/v1/objects/` are accepted
  (`cmd/racer/wire/src/object/client.rs:177-179`, `:338-356`); any other
  method is 405 with `Allow: HEAD, POST` (`client.rs:615-619`). A request body
  is rejected: `Content-Length` is accepted only as `0`
  (`client.rs:241-255`). Query strings are not parsed; the target is used
  verbatim as the key path. Error responses have an empty body
  (`client.rs:615-616`).
- HEAD `Cache-Control: max-age` is parsed into `ClientRequest::retain`
  (`client.rs:239`, `:358-362`, `:371`).
- Warm job state and config live in `cmd/racer/src/read/warm.rs`
  (`WarmConfig.retained_percent` at `warm.rs:29`), and the warm peer operation
  in `cmd/racer/src/peer/protocol.rs:275-283`, `:518-550`, `:632-663`, with
  responses `held`, `warmed`, `released` (`protocol.rs:744-749`).
- Warm hard holds: `HolderKind::Warm` (`cmd/racer/src/store/holds.rs:109-114`,
  `cmd/racer/src/store/catalog.rs:251-258`, `:349`, `:494`).
- Spill uses `HolderKind::Reader` holds (`cmd/racer/src/read/dispatch.rs:603`,
  `:643`, `:659`), and the hold capacity cap comes from
  `warm.config().retained_percent` (`dispatch.rs:426`), parsed from
  `RACER_RETAINED_SEGMENT_PERCENT` (`cmd/racer/src/config.rs:228`).
  Spill therefore still needs that cap; see [Removed](#removed).
- Go SDK `Warm` (`pkg/racersdk/client.go:233`); Python
  `stat(retain_seconds=...)` (`clients/python/racer/src/racer/client.py:371-378`).

## Wire: client socket

All prefetch requests use HTTP/1.1 on the client socket with `Host: racer`,
same as object reads. Paths:

| Method | Path | Purpose |
|--------|------|---------|
| `PUT` | `/v1/prefetches/{id}` | Create, or update (rolling window) |
| `GET` | `/v1/prefetches/{id}` | Status; query `wait`, `coverage`, `refs`, `cursor` |
| `DELETE` | `/v1/prefetches/{id}` | Cancel (release interest) |
| `GET` | `/v1/prefetches` | List this cache's tickets |

Other methods on these paths return 405 with `Allow: GET, PUT, DELETE` (item)
or `Allow: GET` (collection). The `/v1/objects/` rules are unchanged except the
removal of `Cache-Control: max-age` handling on HEAD (the header is ignored
like any other unknown header).

`{id}` matches `[A-Za-z0-9._-]{1,128}`, client-chosen, scoped to the cache the
socket belongs to. `.` and `..` alone are invalid (400).

### Parser extension

The `racer_wire::object` client parser gains a separate prefetch route:

- Dispatch on the path prefix before the `/v1/objects/` key parse. Split the
  target at the first `?` for prefetch paths only.
- `PUT` accepts a body with `Content-Length: 1..=RACER_PREFETCH_MAX_BODY_BYTES`
  and `Content-Type: application/json` (parameters ignored; missing is 400).
  `Transfer-Encoding`, `Expect`, and `Content-Encoding` stay rejected (400).
  A larger `Content-Length` is 413 without reading the body.
- `GET` and `DELETE` require no body (`Content-Length` absent or `0`).
- Query strings: `application/x-www-form-urlencoded` pairs, percent-decoding
  allowed. Unknown or duplicate parameters are 400.
- `Authorization` is accepted on `PUT` only, with the same opaque validation as
  reads (`client.rs:476`).

### PUT request body

```json
{
  "cache_uid": "5b0c7a5e-2f7d-4b8e-9d0e-6f6f2f3b1a10",
  "priority": "normal",
  "generation": 0,
  "sequence": 0,
  "ttl_seconds": 600,
  "max_pending_bytes": 0,
  "refs": [
    {"key": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
     "etag": "\"0x8DC1A2B3C4D5E6F\"", "offset": 0, "length": 1073741824,
     "metadata": "opaque"},
    {"key": "60303ae22b998861bce3b28f33eec1be758a213c86c93c076dbe9f558c11c752"}
  ]
}
```

| Field | Type | Default | Rules |
|-------|------|---------|-------|
| `cache_uid` | string | absent | If present, must equal the live ClusterCache UID, else 409 `cache_uid_mismatch` |
| `priority` | string | `"normal"` | `high`, `normal`, `low` |
| `generation` | u64 | `0` | `0..=2^53-1` |
| `sequence` | u64 | `0` | `0..=2^53-1` |
| `ttl_seconds` | u64 | `600` | `1..`; silently capped at `RACER_PREFETCH_MAX_TTL_SECS`; the applied value is in status |
| `max_pending_bytes` | u64 | `0` | `0` means `RACER_PREFETCH_DEFAULT_PENDING_BYTES`; capped at `RACER_PREFETCH_MAX_PENDING_BYTES` |
| `refs` | array | required | `1..=RACER_PREFETCH_MAX_REFS` entries (an empty array is 400) |
| `refs[].key` | string | required | 64 lowercase hex chars (same as `/v1/objects/{key}`) |
| `refs[].etag` | string | absent | Strong quoted ETag as in `If-Match`. Absent: resolved once at submit and recorded |
| `refs[].offset` | u64 | `0` | Must be `< object length` when length is known, else ref fails `invalid_range` |
| `refs[].length` | u64 | to end | `>= 1` when present; clamped to object end |
| `refs[].metadata` | string | absent | Passed to the origin like `Racer-Metadata`; same validation |

Unknown top-level or ref fields are 400 (strict parse). Duplicate refs are
allowed and deduplicated at page level. The `Authorization` header applies to
every ref, is kept in memory only, and is replaced by each accepted PUT (a PUT
without it clears it). This includes a PUT with the same `(generation,
sequence)` and body whose `Authorization` value differs: it is applied again
with the new credential instead of being answered as a replay. Credentials
belong to tickets, not to the units they share; see "Credentials" below.

Refs are normalized into per-version page sets (16 MiB pages) right after etag
resolution; the server does not retain the raw JSON. It keeps a SHA-256 of the
canonical request body plus the `Authorization` header presence bit (not its
value) as the payload hash.

### Idempotency and ordering

Let the ticket hold `(G, S)`.

| Incoming `(g, s)` | Result |
|-------------------|--------|
| No ticket | Create, 201 |
| `g == G, s == S`, same hash | 200, returns current status, renews TTL |
| `g == G, s == S`, different hash | 409 `payload_mismatch` |
| `g == G, s > S` | Replace desired set, 200 |
| `g == G, s < S` | 409 `stale_sequence` |
| `g > G` (any `s`) | Supersede, 200; the previous window is reported `superseded` in metrics only, the ticket keeps its id |
| `g < G` | 409 `stale_generation` |

A PUT on a terminal ticket (SUCCEEDED, PARTIAL, FAILED, CANCELLED) with a
higher `(g, s)` restarts it. With equal `(g, s)` and same hash it returns the
retained status (200). A one-shot batch is a single PUT with `(0, 0)`.

Replacing the desired set: pages no longer desired drop this ticket's
interest (queued units with zero interest are removed; in-flight fills are not
canceled). New pages are planned. Pages already admitted for this ticket stay
counted.

Every accepted PUT renews the TTL. On expiry the ticket goes CANCELLED with
reason `expired` and releases its interest. Nothing is evicted.

### Status codes

| Code | When |
|------|------|
| 201 | Created |
| 200 | Existing, updated, status, list, cancel |
| 400 | Invalid JSON, field, id, query, or headers (`invalid_request`) |
| 404 | Unknown id on GET or DELETE (`not_found`) |
| 405 | Method not allowed |
| 409 | `cache_uid_mismatch`, `payload_mismatch`, `stale_sequence`, `stale_generation`, `passthrough` |
| 413 | Body over `RACER_PREFETCH_MAX_BODY_BYTES` or refs over `RACER_PREFETCH_MAX_REFS` (`too_large`) |
| 429 | Node ticket limit or node pending-bytes limit (`ticket_limit`, `pending_limit`), with `Retry-After: <seconds>` (integer, 1 to 60) |
| 503 | Node draining or cache not ready (`unavailable`) |

A PUT to a passthrough cache is 409 `passthrough` (the old HEAD returned 200
without `Racer-Retain-Until`).

### Error body

Every non-2xx prefetch response has `Content-Type: application/json` and:

```json
{"code": "stale_sequence", "message": "sequence 3 is older than current 5"}
```

`code` is from the fixed set above; `message` is human text with no contract.
Object read errors keep their empty bodies.

### Status response

`GET /v1/prefetches/{id}` and successful PUTs return:

```json
{
  "id": "epoch-17",
  "cache_uid": "5b0c7a5e-2f7d-4b8e-9d0e-6f6f2f3b1a10",
  "state": "PARTIAL",
  "reason": null,
  "priority": "normal",
  "generation": 2,
  "sequence": 5,
  "settled": true,
  "ttl_seconds": 600,
  "expires_at_ms": 1793750400000,
  "created_at_ms": 1793749800000,
  "updated_at_ms": 1793749810000,
  "refs_total": 2,
  "refs_succeeded": 1,
  "refs_failed": 1,
  "refs_pending": 0,
  "bytes": {
    "logical_requested": 1073741824,
    "aligned_requested": 1090519040,
    "already_resident": 268435456,
    "queued": 0,
    "in_flight": 0,
    "joined": 16777216,
    "fetched_from_origin": 805306368,
    "admitted": 1090519040,
    "failed": 16777216
  },
  "coverage": {"resident_bytes": 1073741824, "observed_at_ms": 1793749811000},
  "refs": [
    {"index": 1, "key": "60303ae2...c752", "etag": "\"0x8DC...\"",
     "offset": 0, "length": 16777216, "outcome": "failed",
     "error": "version_changed"}
  ],
  "next_cursor": null
}
```

- `state`: `QUEUED` (accepted, nothing dispatched), `RUNNING`, `SUCCEEDED`
  (every ref admitted), `PARTIAL` (settled, some refs failed), `FAILED` (settled,
  every ref failed), `CANCELLED`.
- `reason`: `null` unless CANCELLED: `client`, `expired`, `superseded`
  (only used for a ticket replaced by a cache UID change), `cache_removed`,
  `passthrough`.
- `settled`: true when no unit for the current `(generation, sequence)` is
  queued or in flight. Terminal states are settled. A settled window reports
  SUCCEEDED, PARTIAL, or FAILED and returns to RUNNING on its next update.
- Success means every page of the ref was admitted (owner replied `admitted`
  or `resident`) when the ticket settled. It is not a residency guarantee;
  eviction can follow.
- `bytes` (all u64, 16 MiB aligned except `logical_requested`):
  `logical_requested` is the sum of ref lengths; `aligned_requested` is the
  sum of distinct pages; `already_resident` was resident at plan time;
  `queued`/`in_flight` are current; `joined` attached to an existing flight;
  `fetched_from_origin` started a new origin fill; `admitted` is resident or
  admitted; `failed` belongs to failed refs.
- `coverage` is present only with `?coverage=1`; otherwise omitted. It sends a
  `probe` to owners and reports resident bytes of the current desired set.
  Probe failures count as not resident. Probing gets at most 3/4 of the time
  left on the request, so a slow owner yields a lower bound, not a missing
  response.
- `refs` is present only with `?refs=failed` or `?refs=all`. Pages of
  `RACER_PREFETCH_REFS_PAGE` (256) entries; `next_cursor` is an opaque string
  or `null`; pass it back as `?cursor=`. A cursor from another
  `(generation, sequence)` is 400. `index` is the ref position in the PUT.
  `outcome`: `succeeded`, `failed`, `pending`. `error` (failed only):
  `version_changed`, `not_found`, `forbidden`, `origin_error`,
  `peer_unsupported`, `budget_timeout`, `invalid_range`. `etag` is the
  resolved ETag (`null` if resolution failed).

`?wait=<dur>`: Go-style duration (`500ms`, `30s`, `2m`), `0` to
`RACER_PREFETCH_MAX_WAIT_SECS`; larger is 400. The server returns as soon as
`settled` is true or the wait ends, whichever is first, then the status
(200 either way). Client disconnect ends the wait.

### List response

`GET /v1/prefetches` returns summaries without `refs` or `coverage`, sorted by
id, at most 1024 (the node ticket limit):

```json
{"prefetches": [
  {"id": "epoch-17", "state": "RUNNING", "reason": null, "priority": "normal",
   "generation": 2, "sequence": 5, "settled": false,
   "expires_at_ms": 1793750400000, "bytes": {"...": 0}}
]}
```

### DELETE

Returns 200 with the CANCELLED status (`reason: "client"`). Interest is
released; queued units with no remaining interest are dropped; in-flight fills
complete normally; nothing is evicted. A canceled ticket is retained like
other terminal tickets.

## Configuration

All `RACER_PREFETCH_*`; out-of-range values fail startup like other config.

| Variable | Default | Bounds | Meaning |
|----------|---------|--------|---------|
| `RACER_PREFETCH_MAX_BODY_BYTES` | `4194304` | 4096..=67108864 | PUT body cap |
| `RACER_PREFETCH_MAX_REFS` | `16384` | 1..=1048576 | Refs per PUT |
| `RACER_PREFETCH_MAX_TICKETS` | `1024` | 1..=65536 | Live plus retained tickets per node, all caches |
| `RACER_PREFETCH_MAX_TTL_SECS` | `3600` | 1..=86400 | TTL cap |
| `RACER_PREFETCH_TICKET_RETENTION_SECS` | `300` | 0..=86400 | Keep terminal batch tickets |
| `RACER_PREFETCH_MAX_WAIT_SECS` | `60` | 1..=600 | `?wait` cap |
| `RACER_PREFETCH_DEFAULT_PENDING_BYTES` | `1073741824` | 16 MiB..=MAX | Per-ticket queued+in-flight bound |
| `RACER_PREFETCH_MAX_PENDING_BYTES` | `17179869184` | 16 MiB..=2^50 | Per-ticket cap on `max_pending_bytes` |
| `RACER_PREFETCH_NODE_PENDING_BYTES` | `68719476736` | 16 MiB..=2^52 | Node-wide queued+in-flight; over it new PUTs are 429 |
| `RACER_PREFETCH_CONCURRENT_FILLS` | `16` | 1..=1024 | Owner-side concurrent prefetch fills |
| `RACER_PREFETCH_ORIGIN_BYTES_PER_SEC` | `0` | 0..=2^40 | Owner-side token bucket; 0 disables |
| `RACER_PREFETCH_MAX_ATTEMPTS` | `5` | 1..=32 | Per page before `budget_timeout`/mapped failure |

Retry backoff is fixed: 100 ms base, doubling, 10 s cap, full jitter, honoring
`retry_after` when larger. `RACER_PREFETCH_REFS_PAGE` is a constant (256), not
config.

## Server design

### Coordinator

A per-node `PrefetchCoordinator` replaces `WarmRegistry`. It owns:

- Ticket registry keyed by `(cache, id)`.
- Unit table keyed by `(ObjectVersion, page)` with an interest count and the
  set of interested tickets. Units are shared across tickets and caches never
  mix (version includes the cache).
- One priority queue per destination owner: strict priority `high` over
  `normal` over `low`; within a priority, round-robin across tickets; within a
  ticket, ref order. A ticket dispatches only while its queued-and-in-flight
  bytes are under its `max_pending_bytes`.
- Planning sends each page to its rank-0 owner using the same ranking reads
  use. With mesh v2 node pools (`designs/racer-mesh-v2.md`), owners are ranked
  only within the cache's pool. Pages the coordinator node owns are filled
  locally through the same owner path. A page with no owner is retried.
- Only pool members accept tickets for a cache. A node outside the pool answers
  `PUT` with `503 unavailable` and peer prefetch operations with
  `StaleMembership`.
- Results update every interested ticket.
- Retries for `Overloaded`, `Unavailable`, `Io`, and `deferred` with the
  backoff above, up to `RACER_PREFETCH_MAX_ATTEMPTS`; then the ref fails
  `budget_timeout` (for deferrals and overload) or `origin_error`.
  `VersionUnavailable` fails only that ref (`version_changed`), giving PARTIAL.
  `NotFound` maps to `not_found`; `OriginRejected`/`OriginForbidden` to
  `forbidden`, which fails only the tickets whose credential was refused (see
  "Credentials").

### Credentials

A unit is shared by every ticket that wants it, but a credential belongs to the
ticket that supplied it:

- A fill of a unit uses the credential of the highest-priority live ticket
  interested in it (ties broken by ticket key). A ticket without
  `Authorization` is filled without a credential; it is never filled using
  another ticket's token.
- A batch carries one credential: units whose chosen credential differs from
  the batch's first unit wait for a later batch.
- When the origin refuses a fill (`forbidden`), the refs of every ticket
  holding that credential fail `forbidden` for that unit, and the unit stays
  queued for the remaining tickets with the next distinct credential, without
  backoff. Each distinct credential is tried at most once per unit; when no
  other credential is left, the unit fails as before.
- A ticket that adds interest with a credential already refused for a tracked
  unit fails that unit at once. A PUT with a new credential joins it again.
- Membership change re-plans queued and in-flight-to-departed-owner units.
  Active windows (not settled or within TTL) re-queue admitted pages whose
  owner left the cluster or the cache's pool. A selector change publishes a new
  membership version, so it re-plans too.
- Passthrough switch cancels the cache's tickets (`passthrough`); ClusterCache
  removal or UID change, or this node leaving the cache's pool, cancels them
  (`cache_removed`).

ETag resolution at submit uses the normal metadata path with the ticket's
`Authorization`. A resolution failure fails the ref without failing the PUT.

### Owner peer operation

New operation, replacing `warm`:

Request headers (signed, same envelope as other operations):

| Header | Value |
|--------|-------|
| `racer-operation` | `prefetch` |
| `racer-cache`, `racer-key`, `racer-etag` | as `version_fields` (`protocol.rs:396-400`) |
| `racer-prefetch-action` | `fill` or `probe` |
| `racer-prefetch-pages` | padded standard base64 of concatenated u64 big-endian page numbers, ascending, unique, 1..=256 entries |
| `racer-object-length`, `racer-content-type` | admitted metadata, `fill` only; owner refuses a mismatching copy |
| `authorization` / origin credentials | `fill` only, as the warm fill carried them |
| `racer-priority` | `high`, `normal`, `low` (`fill` only) |

Response: `racer-status: prefetch` with header `racer-prefetch-results`,
padded base64 of one record per requested page, same order:

```
u8 kind   0=admitted 1=resident 2=deferred 3=failed
u32 value deferred: retry_after_ms; failed: code; else 0
```

Failed codes: 1 `version_changed`, 2 `not_found`, 3 `forbidden`,
4 `origin_error`, 5 `overloaded`, 6 `unavailable`. For `probe`, kind is `1` (resident) or
`3` with code `0` (not resident); `0` and `2` are not used. Whole-op
errors use the existing responses (`stale-membership`, `unavailable`,
`overloaded`).

Owner `fill` behavior per page: resident replies `resident`; else join or
start a flight in the shared `Flights` table with the new prefetch admission
class, persist normally (no hold), reply `admitted` after the page is written.
Over `RACER_PREFETCH_CONCURRENT_FILLS` or the byte token bucket, reply
`deferred` with a retry time instead of queueing. Passthrough caches reply
`failed` 6.

An old owner that cannot decode `prefetch` returns a decode error or unknown
response; the coordinator retries per the budget and then fails the ref
`peer_unsupported`. Peer wire `VERSION` does not change, as with warm.

### Demand interaction

- Remove the warmed-page wait and the selection/spill/progressive-deadline
  skips that reference warm jobs.
- Prefetch fills use a `Prefetch` admission class. A demand miss on a page with
  an in-flight prefetch flight joins it through the normal flight table and
  promotes it: the flight leaves the prefetch concurrency count and token
  bucket.
- Queued, undispatched prefetch units never delay demand reads.

## Removed

- HEAD `Cache-Control: max-age` parsing and `ClientRequest::retain`;
  `Racer-Retain-Until`.
- `WarmRegistry`, warm admission, leases, `HolderKind::Warm`, warm peer actions
  and responses (`held`, `warmed`, `released`), warm metrics
  (`racer_warm_*`).
- `RACER_WARM_MAX_TTL_SECS`, `RACER_WARM_FILL_TIMEOUT_SECS`,
  `RACER_WARM_MAX_JOBS`, `RACER_WARM_WINDOW_PAGES` from server config, the
  deploy template, and the operator. Drop them from the known-key list
  (`config.rs:785-789`) so the loader treats them like any other unknown key.
- Kept: `RACER_RETAINED_SEGMENT_PERCENT`. Spill holds use it via
  `dispatch.rs:426`; it moves from `WarmConfig` to the store/read config.
- Checkpoints: format unchanged. Warm hold records in an old checkpoint restore
  as reader holds and expire on their recorded time.
- Go SDK `Warm`, `WarmOptions`, `WarmResult`; Python `stat(retain_seconds=...)`.
- Tests that exercise only removed behavior are deleted (approved).

## SDKs

Go (`pkg/racersdk`):

```go
type PrefetchRef struct {
    Key      string // 64 hex
    ETag     string // optional
    Offset   int64
    Length   int64 // 0 = to end
    Metadata string
}
type PrefetchSpec struct {
    CacheUID        string
    Priority        string // "high" | "normal" | "low"; "" = normal
    Generation      uint64
    Sequence        uint64
    TTL             time.Duration // 0 = server default
    MaxPendingBytes int64
    Authorization   string
    Refs            []PrefetchRef
}
type StatusOptions struct {
    Wait     time.Duration
    Coverage bool
    Refs     string // "", "failed", "all"
    Cursor   string
}
func (c *Client) Prefetch(ctx context.Context, id string, spec PrefetchSpec) (PrefetchStatus, error)
func (c *Client) PrefetchStatus(ctx context.Context, id string, opts StatusOptions) (PrefetchStatus, error)
func (c *Client) CancelPrefetch(ctx context.Context, id string) (PrefetchStatus, error)
func (c *Client) ListPrefetches(ctx context.Context) ([]PrefetchStatus, error)
func SplitPrefetch(id string, spec PrefetchSpec, maxRefs, maxBodyBytes int) []PrefetchPart // part ids "<id>.<n>"
type PrefetchWindow struct{ /* id, generation, sequence */ }
func (c *Client) NewPrefetchWindow(id string, spec PrefetchSpec) *PrefetchWindow
func (w *PrefetchWindow) Update(ctx context.Context, refs []PrefetchRef) (PrefetchStatus, error) // sequence++
func (w *PrefetchWindow) Reset(ctx context.Context, refs []PrefetchRef) (PrefetchStatus, error)  // generation++, sequence=0
```

`PrefetchStatus` mirrors the JSON. Errors are a typed `*PrefetchError{Status
int, Code string, Message string, RetryAfter time.Duration}`. `fakeracer`
implements the ticket semantics in memory (idempotency table, states, wait,
paging; all refs succeed unless configured).

Python (`clients/python/racer`): `prefetch(id, spec)`,
`prefetch_status(id, wait=None, coverage=False, refs=None, cursor=None)`,
`cancel_prefetch(id)`, `list_prefetches()`, `PrefetchWindow`, `split_prefetch`,
`PrefetchError`, and the same fake-server semantics.

## Metrics

Labels are only `priority` and, where stated, `outcome`.

| Metric | Type | Labels |
|--------|------|--------|
| `racer_prefetch_requested_bytes_total` | counter | priority |
| `racer_prefetch_aligned_bytes_total` | counter | priority |
| `racer_prefetch_already_resident_bytes_total` | counter | priority |
| `racer_prefetch_queued_bytes` | gauge | priority |
| `racer_prefetch_fetched_bytes_total` | counter | priority |
| `racer_prefetch_joined_bytes_total` | counter | priority |
| `racer_prefetch_admitted_bytes_total` | counter | priority |
| `racer_prefetch_failed_bytes_total` | counter | priority |
| `racer_prefetch_demand_joins_total` | counter | priority |
| `racer_prefetch_demand_wait_seconds` | histogram | priority |
| `racer_prefetch_retries_total` | counter | priority |
| `racer_prefetch_deferrals_total` | counter | priority |
| `racer_prefetch_throttle_seconds_total` | counter | priority |
| `racer_prefetch_window_updates_total` | counter | priority |
| `racer_prefetch_window_dropped_bytes_total` | counter | priority |
| `racer_prefetch_tickets` | gauge | priority |
| `racer_prefetch_tickets_finished_total` | counter | priority, outcome (`succeeded`, `partial`, `failed`, `cancelled`) |

Deferred: used-before-evict vs evicted-unused. It needs a per-page
"prefetched, unread" bit in the page index plus a hook on first read and on
eviction. That is an index format question not settled here; implementation
agents must not add it in this change.

## Implementation notes

The Rust server follows this document. The client-socket wire contract
(paths, methods, JSON fields, status codes, error codes, headers) is as
specified above. Where the server had to choose, it did the following.

- Peer response outcome: the prefetch response uses the existing envelope
  field `racer-outcome: prefetch` (this document's `racer-status` names the
  same field). Requests carry `racer-mode: acquire`. The verifier accepts
  results only for a `prefetch` request, only one record per requested page,
  and only `resident` or `failed` code 0 for a probe (fill results never use
  code 0).
- `max_pending_bytes` bounds a ticket's dispatched (in-flight) bytes. An idle
  ticket may always dispatch one page, even under a bound smaller than a page.
  The node limit (`RACER_PREFETCH_NODE_PENDING_BYTES`) counts queued plus
  in-flight bytes and is checked on each PUT that creates or changes a ticket.
- Dispatch: one pump per node keeps up to 8 batches in flight. A batch is up
  to `RACER_PREFETCH_CONCURRENT_FILLS` (at most 256) pages of one version
  chosen by strict priority, round-robin across tickets, and ref order. Its
  pages are then grouped by rank-0 owner (the warm plan's ranking). There is
  no separate queue per owner.
- Ticket limit: before answering 429 `ticket_limit`, the oldest retained
  terminal ticket is dropped to make room.
- TTL expiry: an unsettled ticket becomes CANCELLED (`expired`). A settled
  ticket keeps its SUCCEEDED, PARTIAL, or FAILED state and releases its
  interest. Retention (`RACER_PREFETCH_TICKET_RETENTION_SECS`) counts from
  expiry or cancellation.
- A ref to an empty object with offset 0 and no length succeeds with no
  pages.
- Membership change re-plans queued units (owners are ranked at dispatch) and
  re-queues admitted pages of live tickets whose owner left. Units in flight
  to a departed owner are not canceled; their failure is retried and
  re-ranked.
- A unit that already failed for one ticket is reported failed to another
  ticket that adds interest while the unit is still tracked.
- Demand promotion is applied where a demand read starts on the owner
  (`Work::Fetch`, `Work::FetchSealed`, and peer page acquisition), before it
  joins the flight. `racer_prefetch_demand_wait_seconds` measures promotion
  to fill completion.
- Prefetch responses close the connection. The client socket accepts request
  bodies up to 64 MiB so that `RACER_PREFETCH_MAX_BODY_BYTES` alone decides
  413.
- Version resolution for one PUT is bounded at 60 s (16 concurrent lookups);
  one dispatch, including the owner's fills, at 120 s.
- The former `racer_warm_held_*` gauges, which report spill holds, are
  renamed `racer_held_*`.
- 405 responses carry the error code `method_not_allowed` (not in the
  status-code table above) with the `Allow` header.
- The payload hash covers the PUT body re-encoded as sent: whitespace and
  field order do not matter, but absent fields are omitted rather than
  defaulted, so an explicit default (`"ttl_seconds": 600`, `"generation": 0`)
  is a different payload from an omitted field, and two TTLs that cap to the
  same value differ. The SDKs always encode a spec the same way, so their
  retries replay. A replay renews the TTL only of a ticket that has not
  expired or been canceled.
- Both 429 codes send `Retry-After: 5`.
- The Go `fakeracer` and the Python fake server follow this contract,
  including the cursor format, but have no pages or owners: an unsettled
  ticket reports `RUNNING` (never `QUEUED`), and only the requested,
  admitted, fetched, in-flight, and failed byte counters are filled.
- Deferred: the owner does not check that it is the rank-0 owner of each
  requested page; a per-owner dispatch queue and eager re-plan of in-flight
  units are not implemented.

## Superseded statements

- `designs/racer-unordered-fetch.md`: section 1 ("Warm progress is a completed
  set"), the warm interaction rules in the direct-fill section ("a direct fill
  waits on the warm job", "While a warm job runs for the version, reads start
  no selections", `Work::Warm`/`Work::WarmLocal`), and "No spill while a warm
  job covers the version". Spill holds are unaffected.
- `api/racer/v1alpha1/clustercache_types.go:61`: "Warm and hold (pinning)
  requests are rejected" becomes "Prefetch requests are rejected with 409
  passthrough"; regenerate CRDs.
- `docs/content/reference/racer.md`: "Warm and hard retention" section,
  `RACER_WARM_*` rows, passthrough retention bullet (line 113-115), `racer_warm_*`
  metrics, and the Go SDK "Warm an exact version" section are replaced by
  prefetch documentation.

## Testing

- Parser: methods, ids, query params, body limits, content type, 405/413.
- Idempotency table rows above, TTL expiry, retention GC, 429 limits.
- Scheduler: dedup across tickets, priority and round-robin order, pending
  bound, window replace dropping interest without canceling flights,
  re-plan on membership change.
- Owner: resident, join, start, deferred under budget, probe bitmap,
  passthrough, old-peer `peer_unsupported`.
- Demand join promotes a prefetch flight; queued units do not delay demand.
- SDK: request encoding, error decoding, split helper, window helper, fakes.
