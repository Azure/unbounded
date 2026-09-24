---
title: "Racer"
weight: 4
description: "A high-performance distributed blob cache for data-intensive workloads."
---

{{< callout type="note" >}}
Racer is a new component, and this documentation is preliminary. More guides
and reference documentation are coming later.
{{< /callout >}}

Racer is a distributed blob cache that keeps frequently accessed data close to
your workloads. It sits between applications and underlying storage, reducing
repeated data transfers and helping workloads spend less time waiting for data.

## How It Works

Racer uses storage across participating nodes as a shared cache. Requests reuse
cached data locally or from peers, fetching missing data from the underlying
source as needed. The same cache layer can serve different kinds of blobs,
including datasets, model weights, and application artifacts.

Objects are split into **64 MiB pages**. Metadata has its own hashed owner,
and each page is independently hashed across the cluster using its object
identity, version, length, and aligned offset. Different pages can therefore
use different nodes, although hash collisions can place them on the same node.
Each page starts its own bounded fallback chain when an owner is unavailable.
HTTP and RDMA use the same placement rules.

Each Site universe has 262,144 placement slots, shared by its caches. Stateless
highest-random-weight (HRW) hashing chooses owners using the universe and Node
UID-derived identity. An unchanged membership set produces the same ownership
after a control-plane restart or Pod replacement. Balance is statistical;
adjacent slots can have the same owner. Fallback attempts advance through distinct
ranked physical peers, so each candidate attempt selects a different owner.

Cache capacity is rounded up to a 64 MiB boundary, with a minimum of 512 MiB
per storage shard. The managed agent automatically sizes workers and page buffers
within host CPU, memory, and locked-memory budgets. Its scheduling requests are
100m CPU and 512 MiB memory, with no CPU or memory limits.

When all eligible page buffers are in use, requests wait for a buffer release
until their caller or selected-peer deadline expires. Releases wake eligible
waiters directly, with reserved capacity for downstream peer requests. Buffer
pressure does not allocate extra page buffers or consume a timer-based retry
budget.

The control plane publishes committed desired configurations. Each cache agent
applies updates independently and can skip intermediate revisions. A slow or
disconnected agent does not hold up publication to other agents. Agents keep
their last working configuration if local preparation of an update fails.

Control subscriptions use mutually authenticated TLS at `/v1/config`. An agent
reports the configuration it has applied separately from the update it has
received. Cache capacity policies converge independently of topology; certificate
issuance and CA rotation have their own security checks.

The control plane rebuilds desired state from live Kubernetes objects. Its
durable runtime state is one revision checkpoint ConfigMap, one public trust
ConfigMap, one CA Secret containing at most two roots, and one leader Lease.
Existing control-plane Pods carry bounded current CSR/certificate response
annotations. There is no persistent participant list or topology history.
Last-good control-plane intent is process-local; agents retain compatible
persisted cache storage across restart.

## Built for Performance

Racer is designed for high throughput, low latency, and efficient CPU use:

- **RDMA support** accelerates transfers between cache nodes on compatible
  networks, reducing CPU overhead when moving large amounts of data.
- **Fast local storage** serves cached data close to workloads and provides
  cache capacity beyond memory alone.
- **Distributed reuse** lets nodes share cached data, reducing repeated reads
  from the underlying storage as more workloads request the same content.

Racer also supports standard TCP networking, so RDMA hardware is optional.

## When to Use Racer

Use Racer when many workers read the same blobs, repeated downloads limit
throughput, or shared storage becomes a bottleneck. It is a good fit for
data-intensive workloads such as machine learning and analytics.

## Cluster-wide installation

With the Unbounded operator installed, any live (nonterminating) `P2PCache`
installs both `Deployment/racer-controlplane` and the ownerless
`DaemonSet/racer-dataplane`. This applies even with zero Sites or a cache selector
that currently matches none. Sites determine cache universes and participation;
they do not enable or disable Racer installation.

Create a cache for your application, for example:

```yaml
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: P2PCache
metadata:
  name: application-cache
spec:
  siteSelector: {}
  cacheGeneration: 1
  maxCandidateAttempts: 3
```

Save this as `application-cache.yaml` and run `kubectl apply -f application-cache.yaml`.
An empty `siteSelector` selects all live Sites, each with an independent cache
universe. Applications serve their node-local origin at
`/run/racer/application-cache/origin/socket` and access the client socket at
`/run/racer/application-cache/client/socket`. Paths use the ClusterCache's
metadata.name and are published as `status.originSocket` and `status.clientSocket`.
For container images, use the dedicated
[Gantry backing cache](../../guides/gantry/#operator-managed-enablement) instead.

After the last live P2PCache is removed, each existing Racer workload is retained
and updated independently, including image upgrades. A missing or deleting
workload is not recreated just because its sibling still exists. Shared support
resources are maintained while either workload survives, but ServiceAccounts,
RBAC, Services, ConfigMaps, Secrets, and Leases are not reinstall markers.

Agents run on eligible Linux nodes with nonempty Site membership, honoring the
`racer.unbounded-cloud.io/exclude: "true"` label and taint restrictions. Membership
requires the canonical `unbounded-cloud.io/site` Node label naming a live Site;
the deprecated `net.unbounded-cloud.io/site` label does not establish membership.

Bootstrap derives each process's identity from its Node UID and Site. Enrollment
verifies the live Pod-to-singleton-DaemonSet ownership chain. When Node identity
or Site membership changes, the old process is deconfigured and its Pod is
gracefully replaced; it cannot join the new universe using its old identity.
Both Racer workload override components are cluster-wide and reject `sites`.

### Cache capacity and upgrade

Cache capacity comes only from the Node annotation
`racer.unbounded-cloud.io/cache-size`, or **10Gi** when that annotation is absent.
There is no Site-level capacity setting. For example:

```bash
kubectl annotate node worker-1 racer.unbounded-cloud.io/cache-size=100Gi --overwrite
```

Use a Kubernetes quantity representing whole bytes, at least `512Mi`; capacity
is rounded up to a `64Mi` boundary. An empty or invalid annotation reports an
error rather than falling back to the default. Removing the annotation restores
the `10Gi` desired capacity.

Before upgrading from Site-based configuration, copy each desired
`spec.components.racer.cacheSize` value to the Nodes that previously inherited
it, preserving any intentional per-Node overrides. Otherwise those Nodes use
`10Gi` after upgrade. Remove the obsolete `spec.components.racer` block from your
Site manifests: both `enabled` and `cacheSize` have been removed. Creating a Site
alone no longer installs Racer; create the P2PCaches your applications need.

### Manual uninstall

Remove all P2PCaches first, then delete both workloads in either order:

```bash
kubectl delete p2pcaches.racer.unbounded-cloud.io --all
kubectl -n unbounded-system delete deployment/racer-controlplane
kubectl -n unbounded-system delete daemonset/racer-dataplane
```

Use your installation namespace if different. A remaining live P2PCache causes
the operator to recreate either deleted workload. With no caches, deleting one
workload leaves the other update-only until you delete it too. Leftover support
resources do not reinstall either workload. This procedure removes the running
workloads; it does not erase node-local slabs or retained support resources.

### Certificates and rotation

Certificates contain signed Pod, Node, universe, boot, and role claims. Enrollment
and renewal both check live workload ownership and current selection; a process
that is no longer eligible cannot renew its old identity. The CA records the
maximum expiry of every issued root's leaves before returning a certificate.

Rotation first publishes both roots, then waits the configured overlap delay
plus clock skew before switching the issuer. The old root is retired only after
its maximum issued leaf expiry plus skew. Offline participants do not block
rotation. Fresh TLS proof endpoints are diagnostic, not a fleet acknowledgment
barrier.

`--ca-overlap-delay` defaults to `5m` and accepts durations from `1s` to `24h`.
Use a cluster-wide [workload override]({{< relref "reference/workload-overrides" >}})
to append it to the `controller` container, for example as an entry in the
override document:

```yaml
- component: racer-controlplane
  kind: Deployment
  extraArgs:
    controller: ["--ca-overlap-delay=10m"]
```

Successor leaders retain or increase persisted safety margins; reducing the
flag does not shorten an existing domain's recorded overlap delay. The default
leaf lifetime is 24 hours and clock skew is five minutes.

### Historical stateless-version cutover

The pre-1.0 stateless version required a coordinated fresh dev/test cutover with
matching control-plane and dataplane images. The repository's
`designs/racer-stateless-cutover.md` records that historical cleanup/reset workflow.
This is not the Racer 1.0 upgrade procedure; see the fresh-state requirements
below. Changing installation or Gantry backend selection does not itself migrate
cached data or reset CA state.

## Build from Source

The control plane and dataplane are independent Rust crates built with Rust
1.96.0. Each uses its own `Cargo.lock`; the test-only load generator uses the
repository's Go module.

On Ubuntu, install the native build dependencies:

```bash
sudo apt-get install build-essential perl libibverbs-dev libssl-dev pkg-config
make racer-build
```

The binaries are written to `bin/`. The control plane uses system OpenSSL for
certificate cryptography and rustls for TLS transport. The dataplane uses
vendored OpenSSL for its kTLS-capable transport.

Run `make racer-fmt-check racer-test` to check formatting and run the Go and
Rust suites, including Rust doctests. To check only the Rust control plane,
use `make racer-controlplane-fmt-check racer-controlplane-test`.

Build the managed images from the repository root:

```bash
make image-racer-controlplane-local image-racer-dataplane-local
```

Use matching control-plane and dataplane versions. The operator's cache agents
subscribe to `/v1/config` and enroll at `/v1/enroll`.

Racer 1.0 requires fresh CA state and cache storage. It does not migrate
pre-release state or support mixed-version deployments. The managed slab path
is `/cache/cache-v1.slab`. Racer consumers require the canonical
`unbounded-cloud.io/site` Node label; the deprecated `net.unbounded-cloud.io/site`
label does not establish Racer membership. The Kubernetes API remains
`racer.unbounded-cloud.io/v1alpha1`.
