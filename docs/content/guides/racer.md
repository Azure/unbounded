---
title: "Operate the Racer Cache"
weight: 9
description: "Enable the Site-scoped Racer HTTP cache, configure volume Services, and diagnose runtime prerequisites."
---

Racer caches HTTP objects across nodes in a Site. A shared control plane publishes
topology updates over mutual TLS (mTLS), and a per-Site DaemonSet serves cached
objects and fetches misses from an origin Service. Applications can use the Go
client and origin helpers in `github.com/Azure/unbounded/pkg/racer`.

## Enable Racer for a Site

Racer is opt-in. Enable it on an existing Site:

```bash
kubectl patch site.unbounded-cloud.io edge-a --type=merge \
  -p '{"spec":{"components":{"racer":{"enabled":true}}}}'
```

The operator deploys `racer-controlplane` and the `racer-edge-a` DaemonSet in its
own namespace, normally `unbounded-system`. Dataplane Pods are named
`racer-edge-a-<random-suffix>`. It selects version-matched
`ghcr.io/azure/racer-controlplane` and `ghcr.io/azure/racer-dataplane` images.

All Linux nodes belonging to that Site are eligible by default. Membership comes
from the canonical `unbounded-cloud.io/site` Node label. The deprecated
`net.unbounded-cloud.io/site` label is used only when the canonical label is
absent; an empty canonical label does not fall back. A Node's old Racer universe
annotation is not a membership authority.

Exclude a node with the exact label value `true`:

```bash
kubectl label node NODE racer.unbounded-cloud.io/exclude=true --overwrite
# Re-enroll it:
kubectl label node NODE racer.unbounded-cloud.io/exclude-
```

Eligibility does not establish runtime readiness. Scheduling resources, taints,
kernel support, and storage requirements still apply.

An enabled Site can be healthy with no volume Services. The controller selects
Running, DaemonSet-controlled Pods using the `racer-dataplane` service account in
its state namespace and the Site's dataplane/universe labels, then authorizes
idle readiness through the normal mTLS activation protocol.
This applies both before the first volume and after deleting the last volume,
including Pod replacement during upgrades. Adding a volume requires its listener
to activate before the Pod becomes Ready. Excluded, moved, unavailable, and
historical Node identities receive removal snapshots without idle readiness.

## Prepare the nodes

The managed `http-small-v1` profile uses one shard, one I/O worker, one compute
worker, eight buffers per NUMA node, a 10 GiB slab, and requests/limits of three
CPUs and 2 GiB memory. Before enabling it, provide:

- Linux with cgroup v2 and the io_uring operations required by Racer.
- Sufficient allowed physical cores for worker placement and NUMA memory binding.
  A CPU quota of three CPUs alone does not prove that placement is possible.
- Enough CPU and memory capacity for the managed requests and limits, accounting
  for ancestor cgroup limits.
- An ext4 filesystem with 4 KiB base pages at the cache location. The managed
  host path is `/var/lib/racer`, mounted as `/cache`; creating a hostPath directory
  does not provision or format a filesystem.
- Free space for the unallocated portion of the slab and working hole-punch
  support on that filesystem. Allow additional headroom for other disk usage.
- An inherited soft locked-memory allowance of at least 256 MiB.

The main container uses `Unconfined` seccomp and `SYS_RESOURCE` to set its
locked-memory limit and then starts the daemon. Bootstrap uses
`RuntimeDefault` seccomp, runs as a non-root user, and drops capabilities.
The daemon initializes worker placement, storage, buffer pools, and io_uring
during startup; setup failures stop startup.

Existing slabs must match their configured size and layout. Preserve existing
data when changing placement or storage settings. Racer does not automatically
resize, migrate, or reformat an incompatible slab.

## Configure a volume Service

**Create the volume Service in the operator namespace**, alongside the Racer
dataplane Pods. Kubernetes Service selectors cannot select Pods in another
namespace. The origin Service may be in a different namespace.

For a Site named `edge-a`, the mapped universe is `edge-a`. Both the volume's
annotation and selector must explicitly specify that universe. There is no
implicit `default` universe. Site names that are not valid Kubernetes label
values map to `site_` followed by the lowercase unpadded base32 SHA-256 of the
Site name. Use the operator-created dataplane Pod's universe label when in doubt.

This example assumes an existing origin Service `datasets/model-origin` whose
TCP Service port is named `http`:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: model-cache
  namespace: unbounded-system
  annotations:
    racer.unbounded-cloud.io/universe: edge-a
    racer.unbounded-cloud.io/origin-service: model-origin
    racer.unbounded-cloud.io/origin-namespace: datasets
    racer.unbounded-cloud.io/origin-port: http
spec:
  selector:
    racer.unbounded-cloud.io/dataplane: "true"
    racer.unbounded-cloud.io/universe: edge-a
  ports:
    - name: http
      port: 80
      protocol: TCP
```

The volume needs exactly one TCP port and must be non-headless. Do not set
`publishNotReadyAddresses`. The origin must be a separate, live, non-headless
ClusterIP Service, and its ClusterIP families must cover participating Pods.
`origin-port` selects a Service port, not a Pod targetPort. Origins must provide
the Racer representation contract, including strong checksum ETags; use the SDK
origin helpers rather than assuming any HTTP server meets that contract.

The controller allocates a listener port, patches `targetPort`, and sets
`internalTrafficPolicy: Local`. For NodePort and LoadBalancer Services it also
sets `externalTrafficPolicy: Local`. Clients need a ready local dataplane endpoint
on their node. Applications in other namespaces can address
`model-cache.unbounded-system.svc`.

Volume identity is `namespace/name`. Increment
`racer.unbounded-cloud.io/cache-generation` when replacing the dataset behind
that identity. Listener ports and slot counts are immutable after allocation;
deleted volume identities retain port reservations. Management port 9090 and
peer mTLS port 9443 are reserved. The `status` annotation describes publication,
while Pod readiness describes dataplane activation.

## Identity, enrollment, and ports

Managed subscriptions use TLS 1.3 and client certificates at
`GET /v3/<universe-id>/<node-id>` on port 8443. This is a breaking cutover requiring
matching controller and dataplane versions. Detached command/peer signatures and
shared peer-key bundles are no longer used.

Each dataplane generates its own private key and enrolls through
`POST /v3/enroll` on server-authenticated HTTPS port 8444. The request carries a
projected Pod-bound bearer token for audience `racer-control`, `X-Racer-Boot`
(a 64-hex process nonce), and JSON fields `csr` (PEM), `pod_namespace`, and
`pod_name`. The response contains `certificate` (PEM leaf and issuing root),
`generation`, and `issuer` (SHA-256 of the issuing root DER). Kubernetes ownership
and membership checks determine identity; CSR-supplied identities are not trusted.
The token is used for enrollment and renewal, not subscription heartbeats.

Node certificates identify
`spiffe://racer/universe/<universe-id>/node/<node-id>/pod/<pod-uid>`.
Node identity incorporates the Kubernetes Node UID; a replacement Pod keeps the
node ID but gets its own credentials. Recreating a Node changes its ID; Site
reassignment changes its universe. Peer requests check the certificate identity
against the selected peer and Pod in the addressed routing generation.

| TCP port | Purpose |
| --- | --- |
| 8443 | Leader mTLS subscriptions, exposed by the controller Service |
| 8444 | Leader HTTPS enrollment, exposed by the controller Service |
| 8446 | Leader `POST /v3/proof`, exposed by the controller Service |
| 8445 | `GET /v3/replica-proof` on each controller Pod, probed directly by the leader over HTTPS |
| 8081 | Controller HTTP `/healthz` and leader `/readyz` probes |
| 9443 | Dedicated dataplane peer mTLS listener |
| 9090 | Dataplane HTTP management, including `/status` and `/metrics` |

Volume ingress and origin fetches remain ordinary HTTP. Peer mTLS does not add
client authentication or encryption to those application endpoints. Optional
RDMA uses authenticated session negotiation; its payload is not TLS-encrypted.

## CA rotation and hot reload

The controller retains CA private state in the `racer-ca` Secret and publishes
public roots in the `racer-trust` ConfigMap's `bundle.json`. Participant records
live in immutable ConfigMap shards referenced by the CA state. Preserve the
complete state namespace across restarts and upgrades. Missing private state
alongside existing trust or topology fails closed rather than creating a new CA.
Disabling the last Site retains the shared control plane, CA state, and host cache.

Managed Pods mount `/var/run/racer-trust` as a directory without `subPath`.
Trust and leaf certificates hot-reload without restarting dataplanes. Every worker
must install a context before it is acknowledged. Invalid or rolled-back bundles
retain the last valid context and report an error; certificates still expire.
The credential loop reloads independently of topology polling and renews leaves
under the active issuer.

The serving controller defaults to a 30-day CA rotation interval
(`-ca-rotation-interval=720h`), 24-hour leaves, and a five-minute retirement
clock-skew allowance. Rotation progresses through:

1. **Stable:** one root issues production leaves.
2. **Overlap:** both roots are published, with the old issuer still active.
   Every retained dataplane and controller process must install the exact bundle
   and prove trust in the next root through a fresh TLS handshake.
3. **Switched:** the new issuer is active and leaves renew. Both roots remain
   until fresh proofs, old-connection draining, and the old issuer's latest leaf
   expiry plus clock skew permit retirement.
4. **Stable again:** the old root is removed. Each transition advances the public
   trust generation.

Dataplane proofs use an empty-body `POST /v3/proof` over a new mTLS connection,
with `X-Racer-Boot`, `X-Racer-Trust-Generation`, `X-Racer-Trust-Digest`,
`X-Racer-Certificate-Issuer`, and `X-Racer-Old-Connections` headers. Success is
204. During overlap the proof server presents a next-root leaf, while the client
may still use an old-root leaf. Headers alone cannot satisfy this barrier.
Controller replicas obtain certificates through Pod-owned ConfigMaps and expose
their installed state at `/v3/replica-proof`; the leader verifies a fresh TLS
handshake pinned to each replica's key and boot.

To request rotation, annotate the public ConfigMap with a unique nonempty nonce:

```bash
kubectl -n unbounded-system annotate configmap racer-trust \
  racer.unbounded-cloud.io/rotate-ca="$(date -u +%Y%m%dT%H%M%S%N)" --overwrite
kubectl -n unbounded-system get configmap racer-trust -o jsonpath='{.data.bundle\.json}'
```

Use the actual operator namespace. Repeating the current request nonce is
idempotent; let a rotation finish before requesting another. Check the bundle generation,
active root, and root count, and compare dataplane `/status` TLS generation,
trust digest, issuer, installed-worker counts, and error. Two roots after issuer
switch are expected while old leaves remain valid. Do not edit private CA state
or delete participants to force retirement. Readiness loss or a network timeout
does not prove process termination; retained unavailable processes can block a
rotation barrier. Leader failover requires fresh proof evidence.

### OpenSSL and kTLS

Native dataplane builds need OpenSSL 3 development headers and `pkg-config` in
addition to the C toolchain and libibverbs (Ubuntu:
`build-essential libibverbs-dev libssl-dev pkg-config`). The runtime kTLS gate
requires **OpenSSL >= 3.5 and Linux >= 6.14** for TLS 1.3 rekeying. OpenSSL 3.0
uses encrypted software TLS for the whole connection. Older kernels also use
software TLS; there is no plaintext fallback. Version eligibility alone does
not guarantee offload. Check TX and RX independently using
`racer_dataplane_tls_ktls_tx_connections_total` and
`racer_dataplane_tls_ktls_rx_connections_total`.

## Diagnose startup and traffic

```bash
kubectl get site.unbounded-cloud.io edge-a -o yaml
kubectl get nodes -L unbounded-cloud.io/site,racer.unbounded-cloud.io/exclude
kubectl -n unbounded-system get pods -l racer.unbounded-cloud.io/universe=edge-a -o wide
kubectl -n unbounded-system logs daemonset/racer-edge-a -c bootstrap
kubectl -n unbounded-system logs daemonset/racer-edge-a -c dataplane
kubectl -n unbounded-system get service model-cache -o yaml
kubectl -n unbounded-system get endpointslice -l kubernetes.io/service-name=model-cache
```

Check bootstrap for Site/universe mismatches and dataplane logs for startup
failures. Management port 9090 serves `/startupz`, `/readyz`, `/livez`, and
`/metrics`. A reachable metrics endpoint alone does not establish worker health.
Only the elected controller leader passes `/readyz` on port 8081; an unready
standby with healthy `/healthz` is expected.

For direct diagnosis, probe controller Pods on port 8081. The controller Service
does not expose an HTTP readiness endpoint.

For local development, `make racer-build` produces binaries under `bin/`.
`make racer-test` runs the Go and Rust suites, including separate Rust doctests;
`make racer-crosslang-test` enables the real Go/Rust interoperability harnesses.
Use the crate's `TESTING.md` for kernel prerequisites and explicitly opt-in tests.
