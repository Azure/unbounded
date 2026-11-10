# Racer Dataplane

Racer's node-local read-through cache dataplane and its supporting Rust libraries.

## Configuration

Every environment variable the dataplane accepts is one entry in
`src/config/table.rs`. That table drives parsing, defaults, per-variable bounds,
and the generated reference in `docs/content/reference/racer.md`, and it is
exported to `internal/operator/components/racer/dataplane-settings.json` for the
operator's checks. After changing the table, regenerate both with
`BLESS=1 cargo test -p racer config::tests::generated_reference_is_current`;
the same test fails when they are stale.

- Only `main` reads the process environment (`Config::from_env`). Tests and
  simulations build a `Config` from an explicit map (`Config::from_map`).
- Any `RACER_*` variable that the table does not list fails startup. Test
  harness knobs therefore use other prefixes (for example `DST_SEEDS`).
- Errors name the variable, its value, and the rule it broke, for example
  `RACER_RANGE_WINDOW_PAGES="65": must be from 1 to 64`.
- Budgets that I/O workers share have a `*_PER_WORKER` form (the default,
  multiplied by the final worker count) and a plain node-wide form (divided
  among workers). Setting both is an error.

Renamed variables. The dataplane has no compatibility aliases, but the
operator rewrites the renamed keys in `racer-settings` to their replacements
with the same value (the old budget keys were already per-worker amounts). It
removes the `RACER_WARM_*` keys, which never had an effect, and reports
`RACER_PEER_PORT` for a manual change:

| Old | New |
| --- | --- |
| `RACER_SLAB_BYTES` | `RACER_SLAB_BYTES_PER_WORKER` |
| `RACER_FREE_SEGMENT_RESERVE` | `RACER_FREE_SEGMENT_RESERVE_PER_WORKER` |
| `RACER_ACTIVE_PATH_SEARCHES` | `RACER_ACTIVE_PATH_SEARCHES_PER_WORKER` |
| `RACER_CONNECTIONS_PER_NEIGHBOR` | `RACER_NEIGHBOR_CONNECTIONS_PER_WORKER` |
| `RACER_ORIGIN_CONNECTIONS_PER_CACHE` | `RACER_CACHE_ORIGIN_CONNECTIONS_PER_WORKER` |
| `RACER_TRUST_BUNDLE` | `RACER_CONTROL_TRUST_BUNDLE` |
| `RACER_SERVICE_ACCOUNT_TOKEN` | `RACER_CONTROL_TOKEN` (also cluster control's token) |
| `RACER_API_CA` | `RACER_KUBE_API_CA` |
| `RACER_API_TOKEN` | `RACER_KUBE_API_TOKEN` |
| `RACER_SEND_CRC_PAIR` | `RACER_SEND_TAG_PAIR` |
| `RACER_PEER_PORT` | Removed; the port of `RACER_PEER_LISTEN` is published |
| `RACER_DST_SEEDS`, `RACER_DST_STEPS`, `RACER_DST_FILTER` | `DST_SEEDS`, `DST_STEPS`, `DST_FILTER` |

Renamed and removed Prometheus metrics (no compatibility aliases). Update
dashboards and alerts that use the old names:

| Old | New |
| --- | --- |
| `racer_recycler_bytes` | `racer_buffer_pool_bytes` |
| `racer_recycler_entries` | Removed; see `racer_buffer_pool_misses_total` and `racer_buffer_pool_rejects_total` |
| `racer_worker_recycler_bytes` | `racer_worker_buffer_pool_bytes` |
| `racer_fill_checksum_nanoseconds_{sum,count}` | `racer_fill_tag_check_nanoseconds_{sum,count}` |
| `racer_crypto_decrypt_crc_rejected_total` | Removed; use `racer_crypto_decrypt_aead_rejected_total` |
| `racer_crypto_decrypt_failure_corrupt_crc_total` | Removed; use `racer_crypto_decrypt_failure_corrupt_aead_total` |
| `racer_disk_class_index_evicted_pages_total` | Removed |
| `racer_disk_class_index_evicted_payload_bytes_total` | Removed |
| `racer_retention_heat_entries` | Removed |
| `racer_peer_local_pressure_total` | Removed; `racer_peer_admission_limit` is now a fixed cap |


## Adapter persistence

A ClusterCache with `spec.persistence: Adapter` publishes `CacheMode::Adapter`
(wire spelling `adapter`). `CacheMode::disk()` is false for it, as for
passthrough, so the store refuses its writes (`StoreWriter::allowed`), fills
never persist it, reads skip Racer's disk and never spill, and every snapshot
purges its disk records. Unlike passthrough, its pages and metadata stay in
memory.

On a miss, `Fill::acquire_once` asks the sources that
`racer_core::read::local_sources` lists after memory and pending copies, and
before the ranked owner walk and the remote origin. For an adapter cache that
is `LocalSource::AdapterReplica`: `Origin::local_page_reserved` sends the
node's own origin socket a pinned page `GET` with `Racer-Source: local`. A
`404` or `412` is a local miss. A miss or any failure other than cancellation
falls through without origin failure notes or link-health accounting.

Next is `LocalSource::Holders`. With no disk copy, once an owner evicts an
adapter page the durable copies live in the adapters of the nodes that read
it. An owner that serves an adapter page to a peer `Acquire` records the
signed requester (`Coordinator::note_holder`) in its worker's holder table
(`racer_core::read::holders::Holders`, on `CandidatePolicy::holders`). Keys
are page identities, whose version carries the etag, so a new version never
routes to holders of an old one. Each page keeps at most `HOLDERS_PER_PAGE`
(4) nodes, newest first and deduplicated, each for `HOLDER_TTL` (24h); the
table keeps at most `HOLDER_CAPACITY` (65536) pages and evicts the oldest
inserted page first. Time is injected, so the table is deterministic.

`Fill::acquire_from_holders` asks up to `HOLDER_ATTEMPTS` (2) unexpired
holders, other than this node, with a holder probe (`FetchMode::Holder`, wire
mode `holder`). Like an offer, a probe is a credential-free page request
with no remote attempts, sent on its own `HOLDER_TIMEOUT` (2s) deadline and a
fresh one-exchange grant, so a dead holder neither stalls the fill nor spends
the reader's budget. The holder (`Coordinator::serve_holder`) answers from
memory or its own adapter (`LocalSource::AdapterReplica`), never acquires,
forwards, or contacts the remote origin, and answers `Miss` for any other
cache mode. A returned copy is decrypted, which authenticates it, and
published to memory. A miss, a failure, or an unauthenticated copy forgets
that holder and asks the next; after the last the fill continues with the
ranked walk. Receivers that predate the mode reject it, which counts as a
failure. `racer_holder_records_total`, `racer_holder_probes_served_total`,
`racer_holder_hits_total`, `racer_holder_misses_total`, and
`racer_holder_failures_total` count these steps.

## Invalidation

`DELETE /v1/objects/<key>` with a strong `If-Match` ETag on the client socket
drops that version (`src/client/invalidate.rs`, `src/read/invalidate.rs`). The
node drops its metadata and pages, then sends a `prefetch` peer operation with
action `invalidate` to each page's ranked owners. Receivers drop local state
only and never forward it. See "Invalidation" in
`docs/content/reference/racer.md` for limits.

## Origin HTTPS

Origins reached over HTTPS (for example Azure Blob Storage) use rustls for the
handshake and then move the record layer into the kernel (kernel TLS, the `tls`
module) so response bodies are decrypted directly into page buffers. Kernel TLS is
optional: if `setsockopt(TCP_ULP, "tls")` fails, for example because the module is
not loaded and cannot be autoloaded from the container, Racer logs
`racer: kernel TLS unavailable (...); using userspace TLS` once and encrypts and
decrypts with rustls on the worker's own reactor instead, which costs more CPU per
byte but no extra threads. A missing module is remembered per origin endpoint; an
`EPERM` from a security policy is retried on each new connection. To get kernel
TLS, load the module on the node (`modprobe tls`) or list it in
`/etc/modules-load.d`.

- `RACER_ORIGIN_TRUST_BUNDLE`: PEM trust roots for origin and token endpoint
  HTTPS. Defaults to `/etc/ssl/certs/ca-certificates.crt`, which the Racer image
  installs. Loaded once at startup and never reloaded. If it cannot be loaded,
  startup is unaffected but every HTTPS origin request fails as unavailable
  until the process restarts with a usable bundle.
- `RACER_AZURE_FEDERATED_TOKEN_FILE`: projected service account token for Azure
  workload identity; reread on every token exchange.
- `RACER_AZURE_AUTHORITY_HOST`: Entra authority for workload identity. Defaults
  to `https://login.microsoftonline.com/`.
