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
per storage shard. The managed agent uses eight page buffers (512 MiB total)
on one NUMA node and requests 4 GiB of memory.

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

With the Unbounded operator installed, Racer defaults on when a Site exists.
The operator creates one `Deployment/racer-controlplane` and one ownerless
`DaemonSet/racer-dataplane`. To explicitly vote for installation on an existing
Site, replace `my-site` with your Site's name:

```bash
kubectl patch sites.unbounded-cloud.io my-site --type=merge \
  -p '{"spec":{"components":{"racer":{"enabled":true}}}}'
```

An omitted Racer block or `enabled` field means true. Explicit false opts a Site
out of initial installation voting. If every Site opts out, a fresh installation
is absent; once installed, both components are retained and repaired even after
all Sites opt out or disappear. With no Sites and no existing installation,
Racer is not installed.

Agents run on eligible Linux nodes with nonempty Site membership, honoring the
existing Racer exclusion label and taint restrictions. Canonical
`unbounded-cloud.io/site` membership takes precedence over the deprecated label,
including when its value is empty. Every existing, nonterminating Site remains
an independent cache universe regardless of its installation vote. P2PCache
Site selectors and cache capacity inheritance (Node annotation, Site setting,
then 10 GiB) continue to apply.

Bootstrap derives each process's identity from its Node UID and Site. Enrollment
verifies the live Pod-to-singleton-DaemonSet ownership chain. When Node identity
or Site membership changes, the old process is deconfigured and its Pod is
gracefully replaced; it cannot join the new universe using its old identity.
Both Racer workload override components are cluster-wide and reject `sites`.

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

### Stateless-version cutover

This version requires a coordinated fresh dev/test cutover with matching control
plane and dataplane images. Earlier CA formats and certificates are not migrated
automatically. Stop all old processes before restarting in a new trust domain;
removing obsolete runtime ConfigMaps does not reset or migrate the CA. CA reset
must be a separate explicit decision. Preserve compatible cache slabs and the
P2PCache identities that isolate their contents. The repository's
`designs/racer-stateless-cutover.md` contains the scoped cleanup/reset workflow;
`hack/scripts/racer-obsolete-runtime.sh` only inventories obsolete objects.

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
