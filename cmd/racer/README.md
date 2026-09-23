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

- __Sites__: map to Unbounded sites e.g. routing boundaries
- __Zones__: groups of ~1000 nodes within a single site
- __Groups__: consensus groups of 3 nodes within the same zone

### Sites

Sites communicate through ordinary nodes that hold a link into another site, similar to `unbounded-net`. Which nodes those are is the control plane's choice; the data plane has no gateway role.

### Zones

Nodes within a particular zone __always__ share a direct connection, typically using RDMA.
Across zones, there is no guarantee of direct connectivity - clients may need to jump through an additional neighbor.
These additional hops are actually important: they fan out read capacity for hot pages, since intermediate nodes can cache the values that they proxy.

### Groups

Operations __never__ target single nodes.
Filling a page always blocks until it is ready to be served by at least 2 nodes in the group.
This helps handle cases where hot pages are evicted before a spike in demand, and hedges against node failures / scaling events.

## API

RACER volumes are composed of one or more extents, each having a specific type:

- __LWW__ (last write wins): writes will never conflict, application is either a single process or has its own lock mechanism
- __OCC__ (optimistic concurrency control): RACER tracks the revision of a page when it is read. Writes cause a conflict error if another consumer has modified the same page since the previous read.
- __Immutable__: write once, free once. Useful for implementing [CORFU](https://www.usenix.org/system/files/conference/nsdi12/nsdi12-final30.pdf). Sparse allocated, supports the full block device address space.

Only immutable extents support wide (e.g. 4MB) pages. Others are strictly 4KB.

## Control Plane

A separate sidecar control plane component populates the protobuf config file, gets feedback by scraping dataplane metrics, and manages nvmet NVMe-oF devices.
RACER itself doesn't know anything about Kubernetes, NVMe-oF, etc.
