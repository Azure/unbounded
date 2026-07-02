# playpen-operator

`playpen-operator` serves the aggregated Kubernetes API for playpen runner pods.
It runs next to a pool of `playpen-runner` pods and allocates one idle runner to
each client request.

Playpen assumes the cluster is already running unbounded-net as the CNI. Runner
pods use ordinary pod networking only. On allocation, the operator creates a
temporary synthetic Kubernetes `Node` for the external client, labels it with the
configured external unbounded-net site, and waits for unbounded-net to assign a
PodCIDR. The runner pod IP becomes the server-side VXLAN endpoint and the
client-side VXLAN endpoint is derived from the synthetic Node PodCIDR.

The same runner image is also used for pooled k3s control-plane pods. Those pods
run k3s plus `playpen-runner control-plane`, which publishes the kubeconfig and
guest API server metadata consumed by control-plane allocations.

The operator does not proxy Redfish or console traffic. Clients reach Redfish and
the serial console stream through the Playpen VXLAN path. The alloc response
includes the runner Redfish URL, the system URL, and the OEM serial console
stream URI for convenience.

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

Runner alloc requests require an idempotency key and the external client's real
unbounded-net underlay InternalIP. The idempotency key can be passed in the
`Idempotency-Key` header or, for `kubectl` clients without a header flag, as
`idempotencyKey` in the JSON body:

```bash
kubectl create --raw /apis/playpen.unbounded-cloud.io/v1alpha1/allocs \
	-f - <<'EOF'
{"idempotencyKey":"smoke-test-1","externalClientInternalIP":"10.0.0.25"}
EOF
```

The same idempotency key can be retried with the same request body. Reusing the
key with a different request returns `409 Conflict`.

Dealloc uses the same idempotency key and is idempotent:

```bash
kubectl create --raw /apis/playpen.unbounded-cloud.io/v1alpha1/deallocs \
	-f - <<'EOF'
{"idempotencyKey":"smoke-test-1"}
EOF
```

## Kubernetes Behavior

- Runner pods are created in `--runner-namespace` from `--runner-image`.
- `--runner-amd64-count` and `--runner-arm64-count` set the desired idle pool
  size for each architecture.
- Runner pods always use normal pod networking, never Kubernetes `hostNetwork`.
- Runner pods are selected from `--runner-namespace` using
  `--runner-label-selector` for allocation and reconciliation.
- An alloc creates or updates the synthetic external-client Node, waits for its
  PodCIDR, then writes pod annotations for the request hash, idempotency key
  hash, client InternalIP, client VXLAN address, and allocation time.
- The alloc response uses the selected runner pod IP as the server VXLAN address
  and Redfish host. Real Kubernetes Node `ExternalIP` addresses are not used for
  runner allocations.
- `--playpen-ttl` is the only playpen pod TTL enforcement point. It deletes
  expired allocated pods. Deallocs also delete the allocated pod and synthetic
  client Node. The operator replaces deleted allocated pods during runner pool
  reconciliation.
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
