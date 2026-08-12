# RACER

- (R) RDMA
- (A) Allocator
- (C) (for highly) Concurrent
- (E) Elastic
- (R) Registers

RACER is a peer-to-peer distributed block device riding on top of NVMe-oF (Non-Volatile Memory Express Over Fabrics).
It allocates and replicates 4KB and 4MB pages striped across large (100k+ node) clusters.

## Design Goals

- Hot pages automatically __fan out__ to additional nodes without explicit coordination.
- Clusters are __self healing__. Losing a subset of nodes will cause pages to proactively replicate to other nodes, to avoid the tail latency of cold starts.
- Pages have moderate __durability__. Injesting a value blocks until at least 2 nodes are ready to serve its page(s).
- RACER block devices have __special semantics__ that can be used to implement application-specific caching logic (not just key/value stores).
- All fabric IO uses normal __NVMe-oF__ frames. The overlayed metadata scheme is designed in such a way that allows for simple DPU/FPGA hardware offload in the future.
- Non-conflict IO operations are guaranteed to be executed in a __single round trip__ to all nodes owning the page.

## Service Architecture

- 4MB pages are zero copy, 4KB pages use a single copy + CRC32C
- IO uses one `io_uring` worker thread pinned to each physical CPU core, with minimal cross-core coordination
- Verified using model checkers and DSTs

## Cluster Architecture

- __Universes__: a shared LBA space, and the security boundary around it
- __Zones__: groups of ~1000 homogeneous nodes within a single universe
- __Groups__: consensus groups of 3 nodes within the same zone

### Universes

A universe is one flat, sparse address space measured in 4KB blocks, spanning every node that participates in it. It is also the unit of partitioning: the control plane publishes one NVMe-oF namespace per universe and attaches it only to that universe's members, so a node that was never given the namespace cannot address the universe at all. Nothing on the wire names a universe - the namespace a frame arrives on is the universe - which is what makes the boundary a transport property rather than a check the data plane could get wrong.

Each universe carries its own topology: its own catalog of consensus groups, its own zones and entry nodes, its own peers and its own epoch. A node may belong to several universes at once and shares nothing between them but its store.

### Zones

Nodes within a particular zone __always__ share a direct connection, typically using RDMA.
Across zones within a universe, there is no guarantee of direct connectivity - clients may need to jump through an additional neighbor.
These additional hops are actually important: they fan out read capacity for hot pages, since intermediate nodes can cache the values that they proxy.

Nodes within a zone are __homogeneous__. Every node belongs to the same number of groups, so every node stores the same share of the zone. A node in several universes stores the sum of its shares.

### Groups

Operations __never__ target single nodes.
Filling a page always blocks until it is ready to be served by at least 2 nodes in the group.
This helps handle cases where hot pages are evicted before a spike in demand, and hedges against node failures / scaling events.

## API

An __extent__ is a range of a universe's address space, placed there by the control plane. It carries its own page kind, its home zone, the zone it is migrating to, and its own tombstone epoch. Extents are the unit of placement, sealing, migration and accounting.

A local block __device__ is an ordered list of whole extents, concatenated. Nothing binds an extent to one device: two hosts may map the same extents in different orders and combinations, and the address a page has does not change when they do.

Each extent has a specific type:

- __LWW__ (last write wins): writes will never conflict, application is either a single process or has its own lock mechanism
- __OCC__ (optimistic concurrency control): RACER tracks the revision of a page when it is read. Writes cause a conflict error if another consumer has modified the same page since the previous read.
- __Immutable__: write once, free once. Useful for implementing [CORFU](https://www.usenix.org/system/files/conference/nsdi12/nsdi12-final30.pdf). Sparse allocated, supports the full block device address space.

Only immutable extents support wide (e.g. 4MB) pages. Others are strictly 4KB. A device may not mix the two page sizes.

## Control Plane

A separate sidecar control plane component populates the protobuf config file, gets feedback by scraping dataplane metrics, and manages nvmet NVMe-oF devices.
RACER itself doesn't know anything about Kubernetes, NVMe-oF, etc.
