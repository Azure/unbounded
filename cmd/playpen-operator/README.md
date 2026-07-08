# playpen-operator

`playpen-operator` serves the aggregated Kubernetes API for short-lived
KubeVirt-backed playpen VMs. Test runners call only the aggregated API through
the Kubernetes API server; the operator owns all KubeVirt, Multus, endpoint pod,
Secret, ConfigMap, and cleanup work.

Each allocation creates a powered-off KubeVirt `VirtualMachine`, an L2 Multus
bridge for PXE traffic, and a privileged endpoint pod that terminates the
cluster-side WireGuard and VXLAN interfaces. The client configures the local
WireGuard/VXLAN peer and can run local PXE services against the returned VM MAC
and lease metadata. Redfish is served by the endpoint pod over the WireGuard
address and controls the VM through KubeVirt subresources.

The data path is intentionally small: one hostPort-backed WireGuard UDP socket
reaches the endpoint pod, and a point-to-point VXLAN device over that WireGuard
link carries only PXE/guest traffic. The client creates a private network
namespace with the matching WireGuard/VXLAN devices and assigns the returned
guest gateway address to the VXLAN device before running PXE helpers there.

## API

The operator is registered as `v1alpha1.playpen.unbounded-cloud.io` and is meant
to be reached through the Kubernetes API server, not by calling the operator
Service directly. It trusts only Kubernetes front-proxy client certificates and
authorizes requests with `SubjectAccessReview`.

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/apis/playpen.unbounded-cloud.io` | `GET` | API group discovery |
| `/apis/playpen.unbounded-cloud.io/v1alpha1` | `GET` | API version discovery |
| `/apis/playpen.unbounded-cloud.io/v1alpha1/allocations` | `POST` | Allocate a powered-off VM |
| `/apis/playpen.unbounded-cloud.io/v1alpha1/deallocations` | `POST` | Delete an allocation |
| `/healthz` | `GET` | Liveness probe |
| `/readyz` | `GET` | Readiness probe |

Alloc requests require an idempotency key and a valid WireGuard public key. The
idempotency key can be passed in the `Idempotency-Key` header or, for `kubectl`
clients without a header flag, as `idempotencyKey` in the JSON body:

```bash
kubectl create --raw /apis/playpen.unbounded-cloud.io/v1alpha1/allocations \
	-f - <<'EOF'
{"idempotencyKey":"smoke-test-1","wireGuardPublicKey":"<client-wireguard-public-key>","architecture":"amd64"}
EOF
```

The same idempotency key can be retried with the same request body. Reusing the
key with a different request returns `409 Conflict`.

Deallocation is idempotent:

```bash
kubectl create --raw /apis/playpen.unbounded-cloud.io/v1alpha1/deallocations \
	-f - <<'EOF'
{"allocationID":"<allocation-id>"}
EOF
```

## Kubernetes Behavior

- New allocations default to `amd64`, `2Gi` memory, `2` CPUs, and a `40Gi`
  `emptyDisk` root disk.
- VMs use `spec.runStrategy: Manual` and are returned powered off.
- Allocations expire after `--allocation-ttl`, defaulting to `30m`.
- The endpoint pod uses a UDP hostPort selected from
  `--wireguard-host-port-start` through `--wireguard-host-port-end`.
- The alloc response uses the endpoint pod node's `ExternalIP`, falling back to
  `InternalIP`, plus the selected hostPort as the client WireGuard endpoint.
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
