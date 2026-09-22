---
title: "Operate the Racer Cache"
weight: 9
description: "Enable the Site-scoped Racer HTTP cache, configure volume Services, and diagnose runtime prerequisites."
---

Racer caches HTTP objects across nodes in a Site. A shared control plane publishes
signed topology updates, and a per-Site DaemonSet serves cached objects and
fetches misses from an origin Service. Applications can use the Go client and
origin helpers in `github.com/Azure/unbounded/pkg/racer`.

## Enable Racer for a Site

Racer is opt-in. Enable it on an existing Site:

```bash
kubectl patch site.unbounded-cloud.io edge-a --type=merge \
  -p '{"spec":{"components":{"racer":{"enabled":true}}}}'
```

The operator deploys `racer-controlplane` and `racer-dataplane-edge-a` in its own
namespace, normally `unbounded-system`. It selects version-matched
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
idle readiness through the normal authenticated, signed activation protocol.
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
deleted volume identities retain port reservations. Management port 9090 is
reserved. The `status` annotation describes publication, while Pod readiness
describes dataplane activation.

## Control protocol and keys

Managed subscriptions use only `/v2/<universe-id>/<node-id>` with signed protobuf
commands and a projected Pod-bound token for audience `racer-control`. Node
identity incorporates the Kubernetes Node UID; recreating a Node changes its
identity. Site reassignment changes its universe.

The controller manages `racer-config-signing` and `racer-peer-signing` Secrets.
Dataplanes receive only `bundle.json`, mounted as directories so key rotation can
reload them. Configuration signing seeds and controller-private `ring.json`
must remain with the controller. Signed commands authenticate configuration;
they do not encrypt bearer tokens, so use a trusted cluster network or an
authenticated encrypted transport proxy.

Preserve signing Secrets and durable control-plane ConfigMaps across restarts
and upgrades. Disabling the last Site removes its dataplane workload but retains
the installed shared control plane, signing state, and host cache files.

## Diagnose startup and traffic

```bash
kubectl get site.unbounded-cloud.io edge-a -o yaml
kubectl get nodes -L unbounded-cloud.io/site,racer.unbounded-cloud.io/exclude
kubectl -n unbounded-system get pods -l racer.unbounded-cloud.io/universe=edge-a -o wide
kubectl -n unbounded-system logs daemonset/racer-dataplane-edge-a -c bootstrap
kubectl -n unbounded-system logs daemonset/racer-dataplane-edge-a -c dataplane
kubectl -n unbounded-system get service model-cache -o yaml
kubectl -n unbounded-system get endpointslice -l kubernetes.io/service-name=model-cache
```

Check bootstrap for Site/universe mismatches and dataplane logs for startup
failures. Management port 9090 serves `/startupz`, `/readyz`, `/livez`, and
`/metrics`. A reachable metrics endpoint alone does not establish worker health.
Only the elected controller leader serves subscription readiness; a standby
replica being unready on that port is expected.

Nightly and release-upgrade gates discover Racer only when a Site explicitly
enables it or the shared controller installation is retained. They verify the
controller and each enabled Site's DaemonSet image transition, including the
bootstrap init image. The controller gate requires replacement of all replicas,
healthy processes, and a serving leader rather than all replicas being Ready.
Core namespace smoke accepts an unready standby only after verifying Deployment
ownership, container state, `/healthz` on port 8081, and `/readyz` through the
controller Service on port 8080. These checks use the Kubernetes API's Pod and
Service proxies; the deploy/smoke credentials need access to those subresources.

For local development, `make racer-build` produces binaries under `bin/`.
`make racer-test` runs the Go and Rust suites, including separate Rust doctests;
`make racer-crosslang-test` enables the real Go/Rust interoperability harnesses.
Use the crate's `TESTING.md` for kernel prerequisites and explicitly opt-in tests.
