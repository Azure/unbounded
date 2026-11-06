# Racer Dataplane

Racer's node-local read-through cache dataplane and its supporting Rust libraries.


## Origin HTTPS

Origins reached over HTTPS (for example Azure Blob Storage) use rustls for the
handshake and then move the record layer into the kernel (kernel TLS, the `tls`
module) so response bodies are decrypted directly into page buffers. Kernel TLS is
optional: if `setsockopt(TCP_ULP, "tls")` fails, for example because the module is
not loaded and cannot be autoloaded from the container, Racer logs
`racer: kernel TLS unavailable (...); using userspace TLS` once and uses a
userspace rustls relay instead, which costs more CPU per byte. To get kernel TLS,
load the module on the node (`modprobe tls`) or list it in `/etc/modules-load.d`.

- `RACER_ORIGIN_TRUST_BUNDLE`: PEM trust roots for origin and token endpoint
  HTTPS. Defaults to `/etc/ssl/certs/ca-certificates.crt`, which the Racer image
  installs. Loaded on first HTTPS use, so nodes whose origins use only
  plain HTTP never read it. If it cannot be loaded, HTTPS origin requests fail as
  unavailable (retried after 30 seconds) and startup is unaffected.
- `RACER_AZURE_FEDERATED_TOKEN_FILE`: projected service account token for Azure
  workload identity; reread on every token exchange.
- `RACER_AZURE_AUTHORITY_HOST`: Entra authority for workload identity. Defaults
  to `https://login.microsoftonline.com/`.
