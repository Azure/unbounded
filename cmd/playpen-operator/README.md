# playpen-operator

`playpen-operator` serves the aggregated Kubernetes API for playpen runner pods.
It runs next to a pool of `playpen-runner` pods and allocates one idle runner to
a client that presents a WireGuard public key.

On alloc, the operator patches the selected pod with the client key, creates a
per-runner UDP NodePort Service for WireGuard, and returns the endpoint, network,
VXLAN, and Redfish details needed to use the playpen VM.

The returned guest network metadata is intended for a VM whose default gateway is
configured on the client side of the tunnel. The runner pod exposes only the VM's
L2 path into VXLAN; VM egress is routed and NATed by the client tunnel namespace.

The operator does not proxy Redfish or console traffic. Clients reach Redfish and
the serial console stream on the runner through the WireGuard tunnel. The alloc
response includes the runner Redfish URL, the system URL, and the OEM serial
console stream URI for convenience.

## API

The operator is registered as `v1alpha1.playpen.unbounded-cloud.io` and is meant
to be reached through the Kubernetes API server, not by calling the operator
Service directly. It trusts only Kubernetes front-proxy client certificates and
authorizes requests with `SubjectAccessReview`.

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/apis/playpen.unbounded-cloud.io` | `GET` | API group discovery |
| `/apis/playpen.unbounded-cloud.io/v1alpha1` | `GET` | API version discovery |
| `/apis/playpen.unbounded-cloud.io/v1alpha1/allocs` | `POST` | Allocate an idle runner pod |
| `/apis/playpen.unbounded-cloud.io/v1alpha1/deallocs` | `POST` | Deallocate by idempotency key and delete the runner pod |
| `/healthz` | `GET` | Liveness probe |
| `/readyz` | `GET` | Readiness probe |

Alloc and dealloc share one RBAC action. Both handlers authorize `create` on
`allocs.playpen.unbounded-cloud.io`, so granting that action grants both API
operations together.

Alloc requests require an `Idempotency-Key` header and a valid WireGuard public
key:

```bash
kubectl create --raw /apis/playpen.unbounded-cloud.io/v1alpha1/allocs \
  -H 'Idempotency-Key: smoke-test-1' \
  -f - <<'EOF'
{"wireGuardPublicKey":"<client-wireguard-public-key>"}
EOF
```

The same idempotency key can be retried with the same request body. Reusing the
key with a different WireGuard public key returns `409 Conflict`.

Dealloc uses the same idempotency key and is idempotent:

```bash
kubectl create --raw /apis/playpen.unbounded-cloud.io/v1alpha1/deallocs \
  -H 'Idempotency-Key: smoke-test-1'
```

## Kubernetes Behavior

- Runner pods are selected from `--runner-namespace` using
  `--runner-label-selector`.
- An alloc writes pod annotations for the client key, request hash, idempotency
  key hash, and allocation time, then labels the pod with an allocation ID.
- The operator creates a `NodePort` Service named `playpen-runner-<hash>` for
  the runner's WireGuard UDP port. It does not create a NodePort for Redfish or
  serial console traffic.
- The alloc response uses the runner node's `ExternalIP` as the gateway. If that
  node has no `ExternalIP`, any node `ExternalIP` may be used. If no node has an
  `ExternalIP`, allocs fail with `503 Service Unavailable`.
- `--playpen-ttl` is the only playpen pod TTL enforcement point. It deletes
  expired allocated pods. Deallocs also delete the allocated pod and its NodePort
  Service. The runner Deployment is expected to replace the deleted pod with a
  fresh idle one.
- On startup, the operator ensures the operator TLS Secret exists, then injects
  the serving certificate into the APIService `caBundle`.

## Run

For local development against the current Kubernetes context:

```bash
go run ./cmd/playpen-operator --listen-addr=:8443
```

For the in-cluster deployment, build the shared playpen image with
`make image-playpen-local`, render manifests with `make playpen-manifests`, and
apply the files under `deploy/playpen/rendered`.
