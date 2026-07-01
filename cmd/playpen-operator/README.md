# playpen-operator

`playpen-operator` is the HTTPS allocator for playpen runner pods. It runs in
Kubernetes next to a pool of `playpen-runner` pods and hands one idle runner to a
client that presents a WireGuard public key.

On claim, the operator patches the selected pod with the client key, creates a
per-runner UDP NodePort Service for WireGuard, and returns the endpoint, network,
VXLAN, and Redfish details needed to use the playpen VM.

## API

The server listens on `--listen-addr` (default `:8443`). It serves TLS using the
Secret named by `--tls-secret-name`; if the Secret does not exist, the operator
creates a self-signed certificate.

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/playpen/v1/claims` | `POST` | Allocate an idle runner pod |
| `/playpen/v1/releases` | `POST` | Release an allocation and delete its runner pod |
| `/healthz` | `GET` | Liveness probe |
| `/readyz` | `GET` | Readiness probe |

Claim requests require an `Idempotency-Key` header and a valid WireGuard public
key:

```bash
curl -k -X POST https://playpen-operator/playpen/v1/claims \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: smoke-test-1' \
  -d '{"wireGuardPublicKey":"<client-wireguard-public-key>"}'
```

The same idempotency key can be retried with the same request body. Reusing the
key with a different WireGuard public key returns `409 Conflict`.

Release uses the same idempotency key and is idempotent:

```bash
curl -k -X POST https://playpen-operator/playpen/v1/releases \
  -H 'Idempotency-Key: smoke-test-1'
```

## Kubernetes Behavior

- Runner pods are selected from `--runner-namespace` using
  `--runner-label-selector`.
- A claim writes pod annotations for the client key, request hash, idempotency
  key hash, and claim time, then labels the pod with an allocation ID.
- The operator creates a `NodePort` Service named `playpen-runner-<hash>` for
  the runner's WireGuard UDP port.
- The claim response uses the runner node's `ExternalIP` as the gateway. If that
  node has no `ExternalIP`, any node `ExternalIP` may be used. If no node has an
  `ExternalIP`, claims fail with `503 Service Unavailable`.
- `--playpen-ttl` is the only playpen pod TTL enforcement point. It deletes
  expired claimed pods. Releases also delete the claimed pod and its NodePort
  Service. The runner Deployment is expected to replace the deleted pod with a
  fresh idle one.
- On startup, the operator ensures the operator TLS Secret and the shared runner
  WireGuard private-key Secret exist.

## Run

For local development against the current Kubernetes context:

```bash
go run ./cmd/playpen-operator --listen-addr=:8443
```

For the in-cluster deployment, build the shared playpen image with
`make image-playpen-local`, render manifests with `make playpen-manifests`, and
apply the files under `deploy/playpen/rendered`.
