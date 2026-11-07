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

Renamed variables (no compatibility aliases):

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
| `RACER_PEER_PORT` | Removed; the port of `RACER_PEER_LISTEN` is published |
| `RACER_DST_SEEDS`, `RACER_DST_STEPS`, `RACER_DST_FILTER` | `DST_SEEDS`, `DST_STEPS`, `DST_FILTER` |


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
  installs. Loaded on first HTTPS use, so nodes whose origins use only
  plain HTTP never read it. If it cannot be loaded, HTTPS origin requests fail as
  unavailable (retried after 30 seconds) and startup is unaffected.
- `RACER_AZURE_FEDERATED_TOKEN_FILE`: projected service account token for Azure
  workload identity; reread on every token exchange.
- `RACER_AZURE_AUTHORITY_HOST`: Entra authority for workload identity. Defaults
  to `https://login.microsoftonline.com/`.
