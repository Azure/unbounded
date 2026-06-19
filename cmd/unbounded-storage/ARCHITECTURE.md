# unbounded-storage Architecture

`unbounded-storage` is the per-host storage daemon for Project Unbounded. It is
the only Rust crate in the repository (Rust edition 2024) and has its own
conventions for layout, build, and testing. Agents working under
`cmd/unbounded-storage/` must read `cmd/unbounded-storage/AGENTS.md` first; the
repository-wide Go conventions do not apply here.

## 1. Purpose and Place in the System

Project Unbounded serves immutable, content-addressed data to workloads through
a tiered cache that sits in front of a slow origin. The design (see
`designs/storage-high-level.md`) has three tiers:

1. **P2P cache** - runs on every node. Content-addressed by checksum, strictly
   immutable, and shares hot data between peers over RDMA. **This daemon is a
   P2P cache node.**
2. **Regional cache** - a pull-through tier with bounded mutability (etag
   validated) that absorbs misses from the P2P tier.
3. **Origin** - the authoritative store (S3, Azure Blob, or POSIX).

The read path is:

```
workload -> local client -> P2P cache (local NVMe / peer pull / regional pull-through)
                                 -> regional cache -> origin
```

A single daemon instance owns one host's NVMe drives and RDMA HCAs, exposes an
HTTP frontend to local clients, fetches misses from an origin over HTTP, and
serves/relays stripes to and from peer nodes over a libfabric RDMA fabric.

## 2. Core Design Principles

- **Runtime-agnostic, no async runtime.** The crate uses `async fn` extensively
  but ships **no** tokio/async-std. Futures are driven by hand-rolled
  noop-waker executors: a cooperative tick loop on shard threads
  (`ShardLoop`), and dedicated per-request OS threads inside the fabric RPC
  server. This keeps the daemon in full control of CPU pinning and polling.
- **Thread-per-core with hard pinning.** Work is partitioned into shards, each
  pinned to a dedicated CPU with NUMA-local memory. Most hot-path types are
  `!Send`/`!Sync` (`Rc`, `RefCell`, `Cell`, raw pointers) and never cross a
  thread boundary. Cross-core communication is explicit and channel-based.
- **Topology-driven.** At startup the daemon discovers host hardware (CPUs,
  NUMA nodes, HCAs, NVMe drives) from sysfs and computes a `Plan` that assigns
  disjoint CPUs to roles. Everything downstream is sized from that plan.
- **NUMA locality everywhere.** Memory backings, fabric memory regions, and disk
  engines are all allocated on, and pinned to, the NUMA node of the CPU that
  uses them.
- **Hot-reloadable configuration.** Peers, disks, backends, and frontends
  reconcile into the running process when `config.toml` changes; only the
  backing memory, fabric, and topology plan are fixed at startup.
- **Content addressing.** Stripes are keyed by a 32-byte `StripeKey` derived
  from a checksum, so data is immutable and freely cacheable/relayable.

## 3. Build and Toolchain

Build only through the top-level `Makefile`, never raw `cargo`:

- `make unbounded-storage` - fmt + lint + test + release build; copies the
  binary to `bin/unbounded-storage`.
- `make unbounded-storage-build` - release build only (used in Containerfiles).
- `make unbounded-storage-test` - `cargo test --locked --all-targets`.

All cargo invocations use `--locked`.

The crate produces one binary:

- `unbounded-storage` (`src/main.rs`) - the daemon.

Synthetic benchmark traffic is generated in-process by configuring a
`loadgen` frontend. Results are exposed through the daemon's normal
Prometheus metrics, alongside the storage, backend, bufferpool, and fabric
metrics from the service path being exercised.

### libfabric

The fabric layer links libfabric and uses connection-managed MSG endpoints
for both `verbs` and `tcp` providers. The pinned libfabric build provides the
native `tcp` provider (requires libfabric 2.0+; the experimental `net`
provider has been merged into `tcp`).
`make libfabric` installs the pinned `LIBFABRIC_VERSION` under
`tmp/libfabric/<version>/`, and the Makefile exports
`LIBFABRIC_PKG_CONFIG_PATH` and `LD_LIBRARY_PATH`. The build compiles a small C
shim (`shim.c`) against the libfabric headers via the `cc`/`pkg-config` build
dependencies.

## 4. Process Lifecycle (`src/main.rs`)

The daemon's `main` wires every subsystem together. The default config path is
`/etc/unbounded-storage/config.toml`.

### CLI (clap derive)

`--config <PATH>` is the only command-line option. A missing **default**
path is non-fatal (falls back to a built-in `Config::default()`); a
missing **explicit** path is fatal. Everything else - both the
dynamically reloadable cluster state and the startup-fixed settings - is
read from the config file.

The startup-fixed settings (collected into `StartupSettings`: the fabric
endpoint and thread pools, per-shard memory sizing, and CPU-topology
selection) live in the config's `[startup]` section. They are read once
at process start and cannot change without a restart, so they are
excluded from the live-reload diff.

- `[startup.memory]` - `no_hugepages` (allocate shard backings from
  the heap instead of 2 MiB hugepages) and `memory_total_bytes` (u64
  bytes, no suffix; unset/null defaults to 128 MiB) - the total backing
  pool, split evenly across the serving shards so the host footprint stays
  fixed regardless of the auto-scaled shard count.
- `[startup.fabric]` - one `binds` table (`tcp`, `rdma`, or `auto_rdma`),
  `progress_threads` (2), `progress_poll_us` (10), `rpc_worker_threads` (4),
  `max_inflight` (1024) - the fabric endpoint, thread pools, and in-flight
  cap. `auto_rdma.hcas_per_numa_node` caps automatically selected HCAs per
  NUMA node and defaults to 1 when `auto_rdma` is configured.
- `[startup.topology]` - `serving_cores` (unset/null = auto-fill every usable
  CPU), `nic_workers` (fabric CPUs per active HCA, unset/null defaults to 4),
  and the toggles `use_smt_siblings`, `ignore_isolated`, `include_node_cpu0`,
  `allow_inactive_port`, `disable_rdma` - feed
  `startup_to_core_plan_config`, which builds the `topology::CorePlanConfig`
  consumed by `CorePlan::for_host`. The toggles default off; the three
  "negative" plan fields (`respect_isolated`, `exclude_node_cpu0`,
  `require_active_port`) are inverted from the corresponding config field
  so the historical "on" behavior is the default.

### Startup sequence

1. Load and validate config; build `StartupSettings` from the config's
   `[startup]` section.
2. `Host::discover()` reads hardware from sysfs.
3. `CorePlan::for_host(&host, &settings.core_plan_config)` partitions the
   host's usable CPUs into three disjoint, NUMA-local classes: one
   `StorageCore` per NVMe drive, `nic_workers` `NicWorker`s per active HCA,
   and a `ServingShard` on every remaining CPU (optionally capped by
   `serving_cores`).
4. **One shard thread is spawned per `ServingShard`.** If the host yields no
   serving shards, the daemon exits with failure. With no usable HCA the
   NIC-worker class is simply empty and the shards serve over the
   loopback/TCP path.
5. A `PinnedRuntime` is built over the planned CPUs so `WorkerIdx(i)` pins the
   i-th worker thread to its assigned core and NUMA node.
6. A `DiskChannelDirectory` (Arc) is created before shards as the hot-swap
   publication surface for disk channels.
7. Read-only shared state (`Arc<Vec<FrontendSpec>>`, `Arc<Vec<BackendSpec>>`;
   startup-fixed fabric settings come from the config `[startup]` section via
   `StartupSettings`) and routing (`build_routing` -> `Arc<FingerTable>` plus
   `Arc<HashMap<NodeId, PeerId>>`) are constructed once and shared across
   shards.
8. Each shard is spawned with `rt.spawn_pinned(widx, name, Box<FnOnce>)`. The
   `!Send` shard objects are constructed **inside** `run_shard`, after pinning.
9. After every shard reports `Up`, peers are reconciled per shard, the disk
   supervisor opens disks and publishes channels, and the config watcher takes
   over for the lifetime of the process.

### Shard readiness and panic safety

Each shard reports exactly one `ShardReady` message: `Up { descriptor, fabric }`
(after which it parks while holding its `Sender`), or `Failed(String)`.
`report_on_panic` wraps shard bring-up in `catch_unwind`/`AssertUnwindSafe` so a
panicking shard still emits one `Failed` via a dedicated panic channel;
otherwise main's bounded receive would hang. Main performs a bounded `recv()`
exactly `joins.len()` times (it does **not** drain to disconnect, because `Up`
shards never drop their sender).

### Shutdown

A process-wide `static SHUTDOWN: AtomicBool` is set by an async-signal-safe
SIGINT/SIGTERM handler (installed via `libc::sigaction`, relaxed atomic store).
Every thread polls this flag.

Teardown order is deliberate: join shard threads in reverse (releasing
`Arc<engine>` references) **first**, then drop the disk channel directory, then
`disk_registry.drain()`.

## 5. Shard Bring-up (`run_shard`)

`run_shard` runs on an already-pinned thread and constructs the entire
per-shard `!Send` object graph:

1. **Fabric.** Pick a provider (`Provider::from_device_name` for the HCA, or
   `Provider::Tcp` fallback on `lo`); build a `FabricConfig` via
   `fabric::defaults_for(device_name, runtime, widx)`; `Fabric::new` and resolve
   `self_address()`.
2. **Memory.** Allocate a NUMA-local `Backing` via `memory::allocate` **on the
   pinned thread** (mempolicy keeps pages local; the hugepage variant `mbind`s).
   Register it with the fabric as a memory region (MR), and register it with the
   socket ring as a fixed buffer for zero-copy send/recv.
3. **Origin backend.** Resolve the origin URL and build an `HttpBackend`
   over the shard socket, carving origin-fetch pages from the backing.
4. **Transport + blockstore + pool.** Build a `RoutedTransport` (Chord routing +
   fabric transport + origin backend), a `LiveShardLocalStore` over the disk
   channel directory as the `BlockStore`, then `Pool::new`.
5. **RPC server.** Allocate a **separate** scratch backing
   (`RPC_SCRATCH_PAGES = 8`, one scratch page per in-flight serve/forward),
   register it as its own fabric MR and its own `LiveShardLocalStore` (a
   `PageRef` resolves through exactly one backing's geometry, so scratch needs a
   distinct store). Build a `RecursiveHandler` and start the fabric RPC server.
6. **Frontends.** A shard hosts a `FrontendRegistry` of any number of
   frontends keyed by component name. Each spec binds its listener with `SO_REUSEPORT` and
   builds an `HttpDriver`/`S3Driver`; the registry can add and remove frontends
   in place on a live config apply (a removed driver's `Drop` closes its
   listener fd).
7. **Tick loop.** Register tick hooks (socket-ring `progress()`, the
   control-drain hook that reconciles backends/frontends/routing from applied
   configs, and the frontend registry's `progress()`), report `Up`, and run
   `shard_loop.run_until_with(|| SHUTDOWN.load(Acquire) || layer_stop, 100us)` -
   busy-poll when active, sleep 100us when idle. The fabric and pool self-drive
   on their own threads.

Drop order at shard exit is critical: `shard_loop -> pool -> socket ->
rpc_server -> fabric` (fabric last; it joins progress threads, closes the
scratch MR, and tears libfabric down).

## 6. Module Map (`src/lib.rs`)

All subsystems are `pub mod`: `backend`, `bufferpool`, `config`, `fabric`,
`fanout`, `frontend`, `http`, `io`, `memory`, `metrics`, `obs`, `p2p`,
`ring`, `runtime`, `storage`, and `topology`. The `profiling` module is
exported when the `profiling` feature is enabled.

Each subsystem follows the same layout convention: `src/<area>/mod.rs` declares
private submodules and re-exports a curated public surface via `pub use`. Files
are kept under ~1500 lines and split on concept boundaries.

```
                         +------------------+
   local client  ---->   |  frontend (HTTP) |
                         +---------+--------+
                                   |
                                   v
                         +------------------+        +-----------------+
                         |    bufferpool    |  <---->|     backend     |---> origin (HTTP)
                         |    (Pool/shard)  |        |    (HttpBackend)|
                         +----+--------+----+        +-----------------+
                              |        |
              transport (peer)|        | blockstore (local)
                              v        v
                    +-----------+   +---------------------+
                    |    p2p    |   |       storage       |
                    | (Chord +  |   | (engine/btree/disk) |
                    | Routed/   |   +----------+----------+
                    | Recursive)|              |
                    +-----+-----+              | page_channel (cross-core)
                          |                    v
                          v             +-------------+
                    +-----------+       |    ring     |
                    |  fabric   |       | (io_uring)  |
                    | (libfabric|       +-------------+
                    |   RDMA)   |
                    +-----------+
```

Foundational layers shared by everyone: `memory` (NUMA backings), `ring`
(io_uring), `runtime` (pinning + tick loop), `topology` (planning), `config`,
and `http` (wire codec).

## 7. Subsystems

### 7.1 `runtime/` - pinning and the cooperative loop

This layer owns two things: putting each shard thread on a fixed CPU, and giving
it a simple loop to drive work without an async runtime.

- `WorkerIdx(u16)` is the universal worker-slot id; the pool, fabric, and
  io_uring layers all pin against the same index.
- `trait Threading: Send + Sync + 'static` exposes `worker_count`,
  `numa_of(WorkerIdx)`, and `spawn_pinned(idx, name, Box<FnOnce + Send>)`.
  `spawn_pinned` pins CPU affinity and NUMA mempolicy **first**, then runs the
  closure. `DefaultRuntime` is the non-pinning fallback (tests/non-Linux);
  `PinnedRuntime` is the Linux implementation.
- `ShardLoop` is the cooperative driver: `add_tick_hook(FnMut() -> bool)` and
  `run_until_with(stop, idle_sleep)`. Tick hooks return `true` to stay hot.

### 7.2 `topology/` - hardware discovery and planning

- `Host::discover()` reads CPUs, NUMA nodes, HCAs (`Hca`), NICs (`Nic`), and
  NVMe drives (`Nvme`) from sysfs.
- `CorePlan::for_host(&host, &CorePlanConfig)` partitions the host's usable
  CPUs into three disjoint, NUMA-local classes, scheduled most-constrained
  first: a `StorageCore` per NVMe drive, then `nic_workers` `NicWorker`s per
  active HCA (grouped into a `NicWorkerGroup` per HCA), then a `ServingShard`
  on every remaining CPU. Each CPU is handed out at most once; an exhausted
  pool oversubscribes rather than panicking. The shared CPU/HCA filtering
  engine (SMT collapse, isolcpus, cpu0 exclusion, active-port gating) lives in
  `topology/filters.rs`.
- `CorePlanConfig` knobs (defaults): `nic_workers` (4 per active HCA),
  `serving_cores` (`None` = claim every remaining CPU), `use_smt_siblings`
  (false), `respect_isolated` (true), `exclude_node_cpu0` (true),
  `require_node_type_ca` (true, currently a no-op), `require_active_port`
  (true), and `disable_rdma` (drop every HCA so no NIC-worker group is placed
  and serving shards serve over loopback/TCP - the escape hatch for unusable
  verbs).
- Key fields consumed by main: `plan.serving_shards` (one shard thread each),
  `plan.nic_workers` (the fabric worker groups), and `plan.storage_cores`,
  which main maps to one `DiskCpuSlot` per NVMe drive.
### 7.3 `memory/` - NUMA-local backings

- `Backing` is a pinned, NUMA-local memory region carved into fixed-size pages
  (`base`, `page_size`, `page_count`).
- `memory::allocate(BackingRequest { kind, bytes, numa })` must run on the
  pinned thread so mempolicy applies; the hugepage variant (`HUGEPAGE_2MB`)
  `mbind`s to the NUMA node.
- This single backing is shared by reference across the bufferpool (data pages),
  the fabric MR, and the socket-ring fixed buffer.

### 7.4 `ring/` - the io_uring engine

This is the crate's only path to the kernel for disk and socket I/O. One engine
per thread submits and reaps io_uring operations; everything above it talks to
that engine through two thin facades.

- `RingCore` is the single low-level engine: one io_uring ring, a slot map keyed
  by monotonic `user_data`, a sparse registered-buffer table, a registered file
  table, and a FIFO back-pressure queue bounded by `queue_depth`. It is
  `!Send + !Sync` (pinned to its building thread).
- `progress()` pushes queued SQEs, drains CQEs, fills slots, and wakes futures
  plus one parked back-pressure waiter. It implements `SEND_ZC` two-CQE
  semantics (the `F_MORE` byte-count completion, then the notification that
  releases the source buffer).
- Two facades sit on top: `StorageRing`/`StorageRingConfig` for block read/write
  (IOPOLL-capable), and `NetHandle`/`NetworkRing`/`SockAddr` for sockets (no
  IOPOLL). A thread-local registry exposes the current `StorageRing`
  (`set`/`clear`/`current`/`with_current_storage_ring`), which is how
  `CoreLocalDevice` finds its ring.

### 7.5 `bufferpool/` - the per-shard page pool

The buffer pool is the heart of the read path. It hands out fixed-size pages
from the shard's backing memory, coalesces concurrent readers of the same
stripe, and knows how to fill a miss from either a peer or the local disk.

- One `Pool` per shard; single-threaded and `!Send`. `Pool<T: Transport<R>,
  S: BlockStore, R: Req>` is an `Rc<PoolInner>`.
- `Pool::new` carves the backing into pages, calls `blockstore.register_pages`
  once, and validates invariants (page size power-of-two; non-zero page count
  `<= u32::MAX`; non-null base). It performs no async I/O - RDMA `ibv_reg_mr` is
  done out-of-band by the embedder before `Pool::new`, and the embedder
  constructs the pool off the pinned thread.
- `PoolInner` tracks a `FreeList`, an `inflight` map
  (`StripeKey -> Rc<RefCell<StripeFetch>>`) so concurrent readers of the same
  stripe coalesce, and `inflight_prefetch_pages`, a global speculative-prefetch
  budget capped by `cfg.max_inflight_pages` (the head page never counts against
  it).
- Core traits (`traits.rs`): `Req { key() -> StripeKey }`; `PageStream` (a
  local, futures-core-free `Stream` of `Result<PageRef, Error>`); and
  `Transport<R> { bulk_get(&req, src: BulkRef, dsts: &[PageRef]) -> Stream }`,
  which fetches from a **peer**. Blanket impls cover `Arc<T>`.
- Public surface also includes `PoolGroup`/`ShardDescriptor`/`ShardRouter`
  (sharding helpers), `PageGuard`/`ReadStream`/`WindowedRead` (read API), and
  `NullBlockStore`.

A shard's data path therefore has two miss sources: the `Transport` (peer pull
over fabric) and the `BlockStore` (local NVMe). The `RoutedTransport` decides
between them, and the origin `Backend` is the final fallback.

### 7.6 `p2p/` - the Chord stripe DHT

- Routing topology only. Each node deterministically computes a k-arc finger
  table from the sorted node list, and routing is recursive Chord
  closest-preceding-finger. Caching, admission, and eviction belong to the
  storage layer, not here.
- `FingerTable` is fully deterministic in `(local, peers, k)`, so there is **no
  stabilization phase**. `FingerTableConfig { k }` defaults to 100 (targeting
  100-150 fingers against a ~200 RDMA QP budget per node). It holds a local
  `PeerEntry`, `fingers: Vec<PeerEntry>` of length `k` (arc `i` is the half-open
  arc at `local.ring + i*arc_span`; an empty arc stores a clone of `local` and
  `next_hop` filters it out), plus successor/predecessor.
- `RoutedTransport<R, B: Backend<Req = R>>` (the client side) makes the
  first-hop decision via a single Chord `next_hop(stripe_to_ring(key))`:
  - `None` -> this node owns the stripe; serve from the local origin `Backend`.
  - `Some(peer)` -> hand off to a wrapped `FabricTransport<R, FingerRouter>`
    with a `MAX_HOPS` TTL; recursion happens server-side.
- `RecursiveHandler` (the server side) **resolves** every request (in contrast
  to `fabric::PoolHandler`, which only serves locally resident pages). It
  computes `stripe_to_ring(key)` and consults `next_hop`:
  - `None` -> own stripe: read the local `BlockStore`, and on a miss fetch from
    the origin `Backend`.
  - `Some` with hops remaining -> forward to the next hop with a decremented
    TTL; the downstream node RDMA-writes the page into **this** node's scratch,
    the handler yields it, and the RPC server relays it upstream.
  - `Some` with no hops remaining -> `HopLimitExceeded`.
  It owns a dedicated scratch `Backing` (its own fabric MR and an extra
  `BlockStore` buffer); the shared scratch allocator hands out one zeroed
  scratch page per in-flight request, reclaimed when the response stream drops.
- Ring math lives in `ring.rs`: `node_to_ring`, `stripe_to_ring`, `splitmix64`.

### 7.7 `fabric/` - the libfabric RDMA transport

This is how nodes move pages between each other. It binds directly to libfabric
and exposes a small RPC server: a peer asks for a stripe, and this node
RDMA-writes the requested pages straight into the asker's memory.

A thin, direct binding over libfabric (no high-level wrapper crate). It is built
in phases: types/error/FFI, class lifecycle with one pinned progress thread per
completion queue, connection CRUD + MR registration + tagged ping/pong, and the
streaming RPC server plus client `Transport`.

- `Fabric` owns the libfabric objects and progress threads. `MrHandle` is a
  `Copy` value handle; the underlying `fid_mr` is owned by `Fabric`.
- `Provider` selects the libfabric provider (`from_device_name` or `Tcp`);
  `defaults_for` builds a `FabricConfig`.
- The completion machinery (`CompletionFuture`, `CompletionInfo`,
  `CompletionRegistry`, `CompletionSlot`) bridges libfabric CQ entries to
  futures.
- **RPC server model** (`rpc.rs`): `start_rpc_server` spawns a fixed pool of
  `rpc_worker_threads` long-lived OS threads pinned to the shard's worker slot.
  The connection receive path decodes framed requests and enqueues jobs onto a
  bounded `JobQueue` capped by `max_inflight`; excess requests receive an
  overload `ERROR_ACK`. A worker pulls a job, drives the `Handler` stream,
  submits libfabric writes/sends, and parks on completion futures with a real
  thread waker. Wire framing uses an 8-byte `MsgHeader` prefix with a message
  kind and request id. The client sends a bincode `RequestHeader` plus request
  body; the server `fi_write`s each page into the client's destination MR and
  sends one `PageAck` per page. A short success sends `RESPONSE_END`; any error
  sends `ERROR_ACK`. `RpcServerHandle::drop` uninstalls the request sink, closes
  the queue, signals shutdown, and joins the workers.
- `Handler`/`HandlerStream` is the server-side resolution trait;
  `PoolHandler`/`PoolHandlerStream` serve locally-resident pages.
- The client side (`transport`) provides `FabricTransport`, the `PeerRouter`
  trait, `StaticPeer`, and capacity helpers
  (`ensure_launch_fits_registry`, `required_completion_slots`).

### 7.8 `backend/` and `frontend/` - the HTTP edges

These are symmetric twins around the buffer pool.

- `Backend` (origin side): `bulk_get(&req, src: BulkRef, dsts: &[PageRef]) ->
  Stream` resolves a `BulkRef` from the authoritative **origin** into
  destination pages, one page at a time (contrast `Transport`, which pulls from
  a peer). `HttpBackend` (Linux) memcpys origin bytes into pages carved from the
  backing and holds an `Rc<socket>`. `NullBackend` is the no-op.
- Frontend (client side): concrete `HttpFrontend`/`S3Frontend` (Linux), built
  from a `FrontendSpec` via `from_spec`, that bind a listener once per shard with
  `SO_REUSEPORT` (`bind_listener`) and produce a per-shard `HttpDriver`/`S3Driver`.
  A driver's inherent `progress()` is a tick hook (returning `true` to stay hot);
  the socket ring's `progress()` is a **separate** tick hook. The drivers
  multiplex many connection futures. `range.rs` handles HTTP byte ranges and maps
  them to stripe sets (`ByteRange`, `ResolvedRange`, `StripeSlice`, `full_object`,
  `stripe_set`). The concrete frontend/driver types are generic over the
  bufferpool, which is only nameable in the binary, so they expose plain inherent
  methods rather than a trait.
- The live set of per-shard frontend drivers is held by the binary's
  `FrontendRegistry` (in `main.rs`), keyed by id; it implements
  `config::reconcile::FrontendReconcileTarget` so frontends are added/removed in
  place on a config apply. This mirrors the `DiskRegistry`/`BackendRegistry`
  string-keyed reconcile pattern.

### 7.9 `http/` - the wire codec

A transport-agnostic, opinion-free HTTP/1.1 parser/serializer shared by both
edges. Parsing uses `httparse` with zero-copy borrowed header views; typed
`Method`/`StatusCode` come from the external `http` crate (referred to as
`::http` to disambiguate from `crate::http`). It carries no storage policy
(ranges, stripes, status codes live in `frontend`/`backend`), so it can back
both the current plaintext pair and a future S3-compatible pair. Public surface:
`Header`/`ParseError`, `HttpRequest`/`serialize_request`,
`ResponseHead`/`serialize_response_head`, and re-exported `Method`/`StatusCode`.

### 7.10 `storage/` - the per-disk engine

The local NVMe cache, composed bottom-up: `blockdev` (the only layer that
touches the kernel device) -> `alloc` + `refcount` (pure LBA tables) -> `btree`
(a copy-on-write B+tree over a `BlockDevice`) -> `singleflight`/`lru`/
`admission` (concurrency and policy) -> `engine` (wires the whole thing into
`bufferpool::BlockStore`).

**`StorageEngine`** composes:

- a `BlockDevice` for NVMe I/O,
- an `Allocator` over LBA space (meta slots reserved at open),
- a `RefcountTable` for pin tracking and the SIEVE referenced bit,
- a `BTreeIndex` mapping `(value_hash, page_index) -> (lba, checksum,
  byte_len)`,
- a `SieveLru` for eviction,
- an `AdmissionFilter` (one-hit-wonder rejection), and
- a `Singleflight` for per-key write dedup.

`EngineConfig` defaults: `page_size_bytes` 2 MiB (a multiple of
`btree_page_bytes`), `btree_page_bytes` 4096 (the device atomic write unit),
`commit_batch_max` 1024, `commit_batch_deadline_us` 200,
`eviction_watermark` 0.9, `probationary_fraction` 0.1,
`admission_sketch_multiplier` 2, `singleflight_shards` 64,
`restart_scan_queue_depth` 256, `bypass_admission` false (bench/tooling only),
and `skip_recovery_scan_if_no_meta` false (set from the public
`skip_recovery_scan` config flag; production keeps it false so partial recovery
runs).

**CoW B+tree** (`btree/`): not `Sync`. Commits flow through a single mutator
task holding only a `RefCell` borrow on `MutatorState`; lookups are wait-free
through `arc_swap::ArcSwap<RootSnapshot>`. Each `apply_batch` coalesces sorted
`(key, op)` pairs and updates a mirror, rewrites only the spine pages on touched
paths (sharing untouched subtrees) reporting allocated and retired pages, writes
the new meta page into the **inactive** slot (a dual meta-page commit that
survives torn writes), records retired pages and the alive txn, then
`ArcSwap::store`s the new snapshot. When the previous snapshot's last `Guard`
drops it recomputes the minimum-alive txn and frees retired bundles. A failure
mid-commit unwinds the new pages and leaves the old snapshot intact.

**`BlockDevice` trait** (`blockdev/`): deliberately narrow (read page, write
page, geometry); higher layers own checksums, retries, and eviction. It is
generic over the runtime and not boxed (no `dyn BlockDevice`), so the engine is
generic over the device. Implementations: `CoreLocalDevice` (resolves its
`StorageRing` from the thread-local registry and returns `ENXIO` if driven off
its storage core), `MockDevice` (with `MockFaultMode` for fault injection),
`ScratchPool`/`ScratchPage`, and the Linux `UringDevice` open path
(`OpenDisk`, `provision_file`).

**Cross-core bridge** (`page_channel.rs`): a `StorageEngine<CoreLocalDevice>` is
pinned to one storage core and can only be driven from that core. Shard cores
ship each page op (read/write/buffer-registration) over an mpsc `PageChannel` to
a `PageService` running on the storage core, which executes it against the
engine and completes the caller's `ReplySlot`. The handle is `Send + Sync` and
clonable; when the last handle drops, the service drains and exits. **Buffer
lifetime contract:** dropping a read/write future does **not** cancel the
in-flight op (the kernel may still touch the buffer), so the caller must keep the
buffer alive until the `ReplySlot` is set - the same lifetime rule as the
bufferpool `Backing`.

**Disk lifecycle** (`disks/`): `trait DiskTarget { open(spec, pin) ->
(Handle, PageChannel) }`, with `UringDiskTarget` in production (runs the disk on
a pinned storage core) and a mock for tests. `DiskRegistrySet<T>` is seeded with
the plan's disjoint NVMe `DiskCpuSlot`s, keeps those slots globally disjoint
across per-cache registries, and `reconcile(desired)` closes missing paths, opens
new ones, and treats any spec drift (kind/numa/size/queue_depth/page_size/
skip_recovery_scan) as a remove + add.
`assign_disk_cpus` keeps survivors on their physical pin (preserving the
disjoint-CPU invariant and idempotence under churn) and assigns new disks
NUMA-local-first, then any free slot, then unpinned. `channels_snapshot()` is
path-sorted for stable hashing; `drain()` clears channels before handles.
`DiskChannelDirectory` is the Arc'd hot-swap publication surface, and
`LiveShardLocalStore` is a per-shard view over the live directory that
re-registers buffers when it observes a swap (`current_or_replay`).

**Stripe keys**: `stripe_key` derives the 32-byte content-addressed key;
`METADATA_STRIPE_IDX` and `OriginRef`/`StripeReq` describe the request shape.
The metadata entry (sentinel `METADATA_STRIPE_IDX`) carries an
`ObjectMetadata` blob (object length plus a small pass-through KV set)
encoded into its page, rather than object data.

### 7.11 `config/` - typed, validated, hot-reloadable schema

The schema is defined by `api/unbounded-storage/config.proto` (prost-generated, with
serde `Deserialize` derived on each message so a TOML file still loads)
and is proto3-native: byte sizes are plain integer byte counts, and scalar
fields with documented defaults use proto3 `optional` presence. `Config::apply_defaults`
promotes absent/null values to documented defaults while preserving an
explicit numeric zero. The TOML loader is strict (`deny_unknown_fields`), so a
typo'd key fails loudly at parse time.
Protobuf is used here purely as the schema IDL (the prost-generated
structs replace hand-written ones); the on-disk config format and the only
load path are TOML, and the config file is the sole configuration interface
for the foreseeable future. The top-level `version` field is an opaque,
operator-assigned config version: the controller seeds it at startup and
tracks the latest-known, latest-applied, and startup config versions
(`ConfigVersionStatus`), plumbed through ready to be published as gauge
metrics (not yet exposed). The startup config version is pinned to the
config realized at process start and never advances; it lags the applied
version whenever a later config changes a `[startup]`-only knob that a
restart has not yet picked up. On a successful reparse the dynamically
reloadable sections are
reconciled in place on the live shard layer **without a restart**: the
peer/disk/routing surfaces and each shard's backend and frontend
registries are all updated without tearing the shard layer down. Backing
memory, the topology plan, and the fabric max in-flight knob are fixed at
startup (sourced from the config `[startup]` section, see the CLI
section), not reloadable config fields.

Sections (all optional, each falling back to defaults):

- `version` - top-level opaque `u64` config version (0 = unversioned).
- `[[backends]]` - `name` and one `config` table: `http`, `s3`, or `azure`
  with a required `url`, or `fake` for synthetic objects. Backend stripe size
  must be a power of two.
- `[[neighborhoods]]` - `name`, `source` (a backend component name),
  `local_node_id`, `local_tags`, `fingers_per_node` (100), and an optional
  `[neighborhoods.routing_plan]` (`fingers`, `successor`, `predecessor`). When
  `routing_plan` is present the node skips the global finger-table build and
  uses exactly the listed neighbor ids (each must be a
  `[[neighborhoods.peers]]` id, none may be `local_node_id`). This is "disjoint
  discovery": a node is told only its direct routing neighbors rather than the
  full cluster, yet routes identically to the global build (see
  `designs/storage-disjoint-routing-parity.md`).
- `[startup]` - startup-fixed knobs, read once at process start and
  excluded from the live-reload diff: `[startup.memory]`
  (`no_hugepages`, `memory_total_bytes`), `[startup.fabric]` (`binds`,
  `progress_threads`, `progress_poll_us`, `rpc_worker_threads`,
  `max_inflight`), and `[startup.topology]` (`serving_cores`, `nic_workers`,
  `use_smt_siblings`, `ignore_isolated`, `include_node_cpu0`,
  `allow_inactive_port`, `disable_rdma`).
  `startup_to_core_plan_config` inverts the negative plan fields so the
  historical defaults hold. See the CLI section for the per-field
  defaults.
- `[[neighborhoods.peers]]` - `id` (unique `u64` within the neighborhood;
  fabric peer ids are scoped by neighborhood), `tags` for placement-aware
  routing, and one transport table (`tcp` with a `SocketAddr`, or `rdma` with a
  provider-native address encoded as `hex:<fi_getname-bytes>`).
- `[[caches]]` - `name`, `source` (a backend or neighborhood component name),
  and `[[caches.disks]]`. Each disk has one `config` table (`block` with
  `path` and optional `numa`, or `file` with `path` and required `size`),
  `queue_depth` (optional), `page_size_bytes`, and `skip_recovery_scan` (fields
  that disk reconcile treats as drift, see 7.10). Disk paths must be unique
  across all caches.
- `[[frontends]]` - `name`, `source` (a backend, cache, or neighborhood
  component name), and one `config` table (`http`, `s3`, or `loadgen`).

The watcher (`notify`-based) emits `ConfigUpdate`s; main's
`wait_for_shutdown_with_updates` reconciles peers (remove + add on address/numa
drift, via a `last_applied` cache), disks, and - by broadcasting the applied
config to every shard - each shard's backend and frontend registries plus the
routing snapshot. It republishes the channel snapshot each update, logs
`config gen=N ...`, and sets `SHUTDOWN` if the watcher disconnects.

## 8. Concurrency Model Summary

| Layer | Threading | Notes |
|-------|-----------|-------|
| Shard | One pinned OS thread per `ServingShard` | Owns `!Send` pool, transport, frontend, RPC handler |
| Shard loop | Cooperative tick hooks, noop waker | Busy-poll active, sleep 100us idle |
| Fabric progress | One pinned thread per CQ | Self-driving |
| Fabric RPC serve | Fixed worker pool per shard | Bounded job queue, real-waker completion waits |
| Storage engine | One pinned storage core per disk | Reached only via `PageChannel` mpsc |
| Config watcher | `notify` thread + main loop | Reconciles peers/disks live |

Cross-thread hand-offs are explicit: shard-to-disk via `PageChannel`, peer pulls
via fabric RPC, and shard readiness/shutdown via channels and the `SHUTDOWN`
atomic.

## 9. Conventions

- Every `.rs` file (source and test) begins with exactly:
  ```rust
  // Copyright (c) Microsoft Corporation.
  // Licensed under the MIT License.
  ```
- No em-dashes anywhere (use an ASCII hyphen or rephrase).
- Default `rustfmt`/`clippy` (no `clippy.toml`/`rustfmt.toml`).
- Subsystem layout: private submodules behind a `mod.rs` that re-exports a
  curated public surface.

## 10. Testing

Four complementary layers:

1. **Inline unit tests** - `#[cfg(test)] mod tests {}` at the bottom of the file
   that defines the construct.
2. **Module integration tests** - `src/<area>/tests.rs`, gated behind
   `#[cfg(test)] mod tests;`, driven by a hand-rolled noop-waker `block_on`
   (spin bound `< 1_000_000`; no async runtime, because the crate is
   runtime-agnostic).
3. **Deterministic Simulation Testing (DST)** - under `tests/`, driven by
   `proptest`. The framework (`tests/framework/executor.rs`) uses a seeded
   `ChaCha8Rng`, a thread-local `SimState`, a random ready-queue pick per step,
   a step budget for liveness, and a `Deadlock` error. Public surface:
   `Executor`, `SimState`, `RunError`, `with_sim`, `yield_once`, `yield_n`. Each
   area gets a plugin directory `tests/<area>/` with `mod.rs`, `mocks.rs` (all
   randomness via `with_sim`, I/O latency via `yield_n`, a per-area `*SimCfg`
   behind an `Rc`), `workload.rs` (`Workload`, `workload_strategy()`,
   `run_workload`), an optional `oracle.rs`, and `tests.rs` (a single
   `proptest!` with `ProptestConfig { cases: 256 }` and many small
   `assert_<invariant>` functions). `tests.proptest-regressions` files are
   committed. `tests/dst.rs` declares the area modules. Current DST areas:
   `bufferpool`, `fabric`, `p2p`, `page_channel`, and `storage` (the last with
   `oracle.rs` and `recovery.rs`).
4. **End-to-end smoke test** - `hack/smoke-storage.py` runs two real binaries on
   loopback over real libfabric tcp, with file-backed disks, HTTP frontends, and
   a stub origin, exercising a cross-node fabric RPC fetch. It needs `sudo` (to
   pin io_uring buffers and raise `RLIMIT_MEMLOCK`) and runs in CI
   (`.github/workflows/smoke-storage.yaml`). Run it after any change to the
   fabric layer, `shim.c`, the FFI, the `main.rs` wiring, or the libfabric
   version.
