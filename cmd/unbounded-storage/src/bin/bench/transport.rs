// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! `bench storage transport` drives the production libfabric transport
//! end to end and reports page-fetch throughput and latency. It is the
//! transport-layer counterpart to the `block` benchmark: where `block`
//! exercises the io_uring + `StorageEngine` + `LocalStorage` stack, this
//! exercises the fabric RPC + RMA path that moves cached pages between
//! nodes.
//!
//! ## What it measures, and why it is honest
//!
//! Every component on the hot path is production code:
//!
//! - **RPC layer / server side.** The server runs the real
//!   [`PoolHandler`] behind [`Fabric::start_rpc_server`]. Each client
//!   request lands on a real fabric receive, is dispatched to the
//!   handler on a dedicated OS thread, and the resulting page is
//!   RMA-written straight into the client's registered memory by the
//!   real `fi_write` path. No request shortcut exists.
//! - **Pre-allocated buffer pool / client side.** The client drives the
//!   real [`Pool`]: every fetched page is allocated from the pool's
//!   pre-registered free list, filled by the transport, observed by the
//!   workload, and returned to the free list. In-flight coalescing,
//!   stream limits, and backpressure are all the production ones.
//! - **Transport.** The client uses the real [`FabricTransport`]; the
//!   server replies with real RMA writes into the client MR.
//! - **Thread pinning.** Client shards and fabric progress threads are
//!   placed via the crate's [`PinnedRuntime`] using CPUs drawn from
//!   `topology::CorePlan`, exactly like the daemon. When sysfs topology is
//!   unavailable (containers / dev hosts) it falls back to an unpinned
//!   `DefaultRuntime` with a warning.
//!
//! The single bench-specific component is [`MemBlockStore`]: an
//! in-memory `BlockStore` the server uses to "serve" every requested
//! stripe so the run measures pure transport, not disk. It fills each
//! served page with a deterministic byte derived from the stripe key so
//! `--verify` can prove bytes actually traversed the fabric before any
//! throughput number is trusted.
//!
//! ## Modes
//!
//! - `server` / `client` split the two roles across processes. They
//!   exchange libfabric addresses over a tiny TCP control socket, then
//!   run the server (page source) and client (page fetcher / measurer)
//!   code paths. Each process owns one fabric per device, matching the
//!   production daemon's per-HCA domain topology.
//!
//! ## Multi-HCA fan-out
//!
//! `--devices mlx5_0,mlx5_1,...` stands up one independent fabric
//! (domain + RDM endpoint) per listed device and drives them in
//! parallel: the server serves pages from every endpoint, and the client
//! runs one pinned read shard per endpoint, each with its own buffer
//! pool and transport. The server and client must pass the same device
//! list in the same order; endpoint `i` on each side is paired during
//! the control handshake, which exchanges one wire address per device.
//! This is how all of a host's HCAs are exercised at once: a libfabric
//! scalable endpoint cannot span physical HCAs, and the verbs;ofi_rxm
//! RDM provider does not support scalable endpoints anyway, so the
//! aggregate-bandwidth path is N domains fanned out, not one endpoint
//! with N contexts. `--device` (singular) remains the single-HCA form.
//!
//! For a single-host run, start a `server` and point a `client` at its
//! control address over loopback:
//!
//! ```text
//! bench storage transport server --provider tcp --control-addr 127.0.0.1:7777
//! bench storage transport client --provider tcp --server-control-addr 127.0.0.1:7777
//! ```
//!
//! There is intentionally no in-process mode that stands up both a
//! server and a client fabric in one address space: co-locating two
//! libfabric `tcp` domains in a single process faults inside the
//! provider's progress copy under concurrent in-flight RMA writes (a
//! provider-level limitation the production one-fabric-per-process
//! topology never hits). The split processes exercise the exact same
//! server / client code paths without that hazard.
//!
//! Any provider `unbounded-storage` supports works: `--provider tcp`
//! uses the libfabric `tcp` RDM provider; `--provider verbs` with an
//! RDMA `--device` (e.g. `mlx5_0`) uses the `verbs` provider.

use std::cell::{Cell, RefCell};
use std::collections::{BTreeMap, VecDeque};
use std::io::{BufRead, BufReader, Write};
use std::net::{TcpListener, TcpStream};
use std::process::ExitCode;
use std::rc::Rc;
use std::sync::Arc;
use std::sync::atomic::Ordering;
use std::sync::mpsc;
use std::time::{Duration, Instant};

use clap::{Args, Subcommand, ValueEnum};

use unbounded_storage::bufferpool::{
    BlockStore, BufferPool, Error as PoolError, NullBlockStore, PageRef, Pool, PoolConfig,
    StripeKey,
};
use unbounded_storage::fabric::{
    ConnectionSpec, Fabric, FabricTransport, PeerId, PoolHandler, Provider, RpcServerHandle,
    StaticPeer, apply_tcp_env_defaults, defaults_for,
};
use unbounded_storage::memory::{Backing, BackingKind, BackingRequest, HUGEPAGE_2MB, allocate};
use unbounded_storage::runtime::{
    DefaultRuntime, PinnedRuntime, ShardLoop, Threading, WorkerIdx, WorkerSpec,
    block_on_cooperative, set_preferred_node,
};
use unbounded_storage::storage::StripeReq;
use unbounded_storage::topology::{CorePlan, CorePlanConfig, Host};

use crate::{
    LATENCY_RING, SHUTDOWN, format_duration, human_bytes, install_signal_handler, make_key,
    percentile,
};

/// Page size moved per request. Matches the production cache page and
/// the `block` bench's user page so one request is one 2 MiB RMA write.
const PAGE_BYTES: usize = HUGEPAGE_2MB;

// Worker-slot assignments. Each fabric pins all of its progress threads
// to a single `WorkerIdx`'s CPU, and each read-issuing client shard pins
// to its own. To keep the bench honest these slots must be distinct CPUs
// so the pieces do not contend. With `N` fanned-out fabrics the server
// pins slots `0..N` (one per fabric's progress threads); the client pins
// slots `0..N` for the fabrics plus `N..2N` for the per-fabric read
// shards.

/// Peer id the client assigns to the server in each per-fabric
/// connection table. Every client fabric holds exactly one connection,
/// so the same id is reused across fabrics.
const SERVER_PEER_ID: PeerId = PeerId(2);

/// Peer id the server assigns to the client in each per-fabric
/// connection table.
const CLIENT_PEER_ID: PeerId = PeerId(1);

#[derive(Args)]
pub struct TransportArgs {
    #[command(subcommand)]
    mode: TransportMode,
}

#[derive(Subcommand)]
enum TransportMode {
    /// Run only the server (page source). Prints a control address a
    /// `client` invocation connects to for libfabric address exchange.
    Server(ServerArgs),
    /// Run only the client (page fetcher / measurer) against a remote
    /// `server`.
    Client(ClientArgs),
}

/// Provider selector exposed on the CLI, mapped onto [`Provider`].
#[derive(Copy, Clone, Debug, ValueEnum)]
enum BenchProvider {
    /// libfabric `tcp` RDM provider (works everywhere, no RDMA NIC).
    Tcp,
    /// libfabric `verbs` provider (requires an RDMA-capable device).
    Verbs,
}

impl BenchProvider {
    fn to_fabric(self) -> Provider {
        match self {
            BenchProvider::Tcp => Provider::Tcp,
            BenchProvider::Verbs => Provider::Verbs,
        }
    }
}

/// Knobs shared by every mode.
#[derive(Args, Clone)]
struct CommonArgs {
    /// Transport provider to benchmark.
    #[arg(long = "provider", value_enum, default_value_t = BenchProvider::Tcp)]
    provider: BenchProvider,

    /// Fabric device / domain name. `lo` for tcp loopback; an RDMA
    /// device name (e.g. `mlx5_0`) for verbs. Ignored when `--devices`
    /// is set.
    #[arg(long = "device", default_value = "lo")]
    device: String,

    /// Comma-separated list of fabric devices to fan out across, one
    /// independent endpoint per device (e.g. `mlx5_0,mlx5_1,...,mlx5_7`).
    /// When set this overrides `--device` and drives every listed HCA in
    /// parallel. The server and client must list the same devices in the
    /// same order; endpoint `i` on each side is paired.
    #[arg(long = "devices")]
    devices: Option<String>,

    /// Per-device local bind address for the fabric data path, as a
    /// comma-separated list paired by position with the device list
    /// (`--device`/`--devices`). Each entry is `host` or `host:port`;
    /// an omitted port binds an ephemeral port (`:0`). Under FI_EP_MSG
    /// every endpoint is a connection-manager listener, so this is the
    /// address the peer dials back over the control-socket exchange.
    /// For verbs give each HCA's RoCE/IPoIB IP (e.g. `192.168.55.1`);
    /// for tcp loopback the default `127.0.0.1` keeps both fabrics on
    /// `lo`. Defaults to `127.0.0.1` for every device when unset.
    #[arg(long = "listen-addrs")]
    listen_addrs: Option<String>,

    /// `max_inflight` completion-registry slots on each fabric endpoint.
    #[arg(long = "inflight", default_value_t = 4096)]
    inflight: usize,

    /// Number of request receive buffers the RPC server keeps posted.
    /// Bounds server-side download concurrency.
    #[arg(long = "posted-recvs", default_value_t = 256)]
    posted_recvs: usize,

    /// Fabric progress threads per endpoint.
    #[arg(long = "progress-threads", default_value_t = 2)]
    progress_threads: u8,

    /// Number of QPs (libfabric endpoints) per logical peer connection.
    /// Each request round-robins across the QPs, spreading RMA writes
    /// over independent QPs to lift the single-QP throughput ceiling.
    /// `1` reproduces the original single-QP behavior.
    #[arg(long = "qps", default_value_t = 1)]
    qps: usize,

    /// Server-side write pipeline depth: how many `fi_writedata` page
    /// writes the responder keeps outstanding per request before
    /// blocking on the oldest completion. Deeper pipelines hide
    /// per-write completion latency on multi-page responses. `1`
    /// reproduces the original depth-1 (post-then-park) behavior.
    #[arg(long = "write-pipeline", default_value_t = 1)]
    write_pipeline: usize,

    /// Force an unpinned `DefaultRuntime` even when sysfs topology is
    /// available. Useful to isolate the effect of pinning.
    #[arg(long = "no-pin")]
    no_pin: bool,

    /// Back the registered RMA regions with 2 MiB hugepages that are
    /// hard-bound to each HCA's NUMA node (`mbind(MPOL_BIND |
    /// MPOL_MF_STRICT)`) instead of the default heap allocation. Strict
    /// binding guarantees every page stays resident on the NIC-local
    /// node, so a NIC never DMAs across sockets; the soft
    /// `MPOL_PREFERRED` heap path can silently spill under per-node
    /// memory pressure (two HCAs sharing one node). Requires reserved
    /// 2 MiB hugepages on the host (per node); allocation fails hard if
    /// the pool cannot satisfy the request locally.
    #[arg(long = "hugepages")]
    hugepages: bool,
}

impl CommonArgs {
    /// Expand the configured device(s) into an ordered list. `--devices`
    /// (comma-separated) takes precedence and enables multi-HCA fan-out;
    /// otherwise the single `--device` is used. Order is significant:
    /// endpoint `i` on the server is paired with endpoint `i` on the
    /// client.
    fn device_list(&self) -> Result<Vec<String>, String> {
        match &self.devices {
            Some(list) => {
                let devices: Vec<String> = list
                    .split(',')
                    .map(|s| s.trim().to_string())
                    .filter(|s| !s.is_empty())
                    .collect();
                if devices.is_empty() {
                    return Err("--devices must include at least one device".to_string());
                }
                Ok(devices)
            }
            None => Ok(vec![self.device.clone()]),
        }
    }

    /// Allocator for the registered RMA backings. `--hugepages` selects
    /// the strictly NUMA-bound 2 MiB hugepage path; otherwise the heap
    /// path (soft NUMA preference) is used.
    fn backing_kind(&self) -> BackingKind {
        if self.hugepages {
            BackingKind::Hugepage2Mb
        } else {
            BackingKind::Heap
        }
    }

    /// Expand `--listen-addrs` into one `host:port` bind string per
    /// device, paired by position with [`Self::device_list`]. Entries
    /// without an explicit port get `:0` (ephemeral). When unset, every
    /// device defaults to `127.0.0.1:0` so tcp loopback works out of the
    /// box; cross-host verbs runs must pass each HCA's RoCE IP.
    fn listen_addr_list(&self) -> Result<Vec<String>, String> {
        let devices = self.device_list()?;
        let raw: Vec<String> = match &self.listen_addrs {
            Some(list) => list
                .split(',')
                .map(|s| s.trim().to_string())
                .filter(|s| !s.is_empty())
                .collect(),
            None => {
                return Ok(devices.iter().map(|_| "127.0.0.1:0".to_string()).collect());
            }
        };
        if raw.len() != devices.len() {
            return Err(format!(
                "--listen-addrs has {} entr(ies) but {} device(s); they must \
                 pair one-to-one in the same order",
                raw.len(),
                devices.len()
            ));
        }
        Ok(raw.into_iter().map(normalize_bind_addr).collect())
    }
}

/// Normalize a bind entry into `host:port`, defaulting an omitted port
/// to `0` (ephemeral). IPv4 / hostname only: a value already containing
/// a `:` is taken as `host:port` verbatim.
fn normalize_bind_addr(addr: String) -> String {
    if addr.contains(':') {
        addr
    } else {
        format!("{addr}:0")
    }
}

/// Knobs that only make sense where this process drives client reads.
#[derive(Args, Clone)]
struct ClientWorkloadArgs {
    /// Concurrent client read tasks issuing page fetches.
    #[arg(short = 'w', long = "workers", default_value_t = 32)]
    workers: usize,

    /// Duration of the timed phase, in seconds.
    #[arg(short = 't', long = "duration", default_value_t = 10)]
    duration: u64,

    /// Optional cap on total page fetches.
    #[arg(long = "ops")]
    ops: Option<u64>,

    /// PRNG-ish seed mixed into the per-op stripe keys.
    #[arg(long = "seed", default_value_t = 0xBE_DE_CA_FE_u64)]
    seed: u64,

    /// Client buffer-pool size, in 2 MiB pages. Must comfortably exceed
    /// `--workers` so every worker always has a free page.
    #[arg(long = "client-pages", default_value_t = 256)]
    client_pages: usize,

    /// Run a small closed-loop correctness check instead of the timed
    /// phase: fetch a handful of keys and assert each returned page
    /// carries the deterministic byte the server stamped for its key.
    #[arg(long = "verify")]
    verify: bool,
}

/// Knobs that only make sense where this process serves pages.
#[derive(Args, Clone)]
struct ServerWorkloadArgs {
    /// Server scratch-pool size, in 2 MiB pages. Bounds how many pages
    /// the server can have in flight; size it at or above
    /// `--posted-recvs`.
    #[arg(long = "server-pages", default_value_t = 256)]
    server_pages: usize,
}

#[derive(Args)]
struct ServerArgs {
    #[command(flatten)]
    common: CommonArgs,
    #[command(flatten)]
    server: ServerWorkloadArgs,

    /// TCP control address to listen on for client address exchange.
    #[arg(long = "control-addr", default_value = "0.0.0.0:7777")]
    control_addr: String,
}

#[derive(Args)]
struct ClientArgs {
    #[command(flatten)]
    common: CommonArgs,
    #[command(flatten)]
    client: ClientWorkloadArgs,

    /// TCP control address of the `server` process to connect to.
    #[arg(long = "server-control-addr", default_value = "127.0.0.1:7777")]
    server_control_addr: String,
}

pub fn run(args: TransportArgs) -> ExitCode {
    // Apply the same libfabric tcp provider env defaults the daemon uses
    // (zero-copy send size and the saved-message cap that otherwise wedges
    // the fabric after ~64 sequential requests). Must run before any
    // fabric/provider init, while the process is still single-threaded.
    apply_tcp_env_defaults();
    install_signal_handler();
    let result = match args.mode {
        TransportMode::Server(a) => run_server(a),
        TransportMode::Client(a) => run_client(a),
    };
    match result {
        Ok(()) => ExitCode::SUCCESS,
        Err(e) => {
            eprintln!("bench storage transport: {e}");
            ExitCode::FAILURE
        }
    }
}

// ---------------------------------------------------------------------
// In-memory server BlockStore (the one bench-specific component).
// ---------------------------------------------------------------------

/// A `BlockStore` that treats every stripe as resident and fills the
/// requested scratch page with a byte derived from the stripe key.
///
/// `base` is the address of the server scratch `Backing` whose pages the
/// `PoolHandler` reserves and then RMA-writes to the client. Filling
/// `base + page_idx * page_size` is therefore filling the exact bytes
/// the server is about to send, so `--verify` reading `key.0[0]` back on
/// the client proves the page traversed the fabric.
struct MemBlockStore {
    base: usize,
    page_size: usize,
}

// SAFETY: `base` points at a `Backing` whose pinning invariant outlives
// the store (the scratch backing is owned by the `PoolHandler` for the
// life of the server). Writes are bounded to a single page selected by
// the handler, which never hands out overlapping pages concurrently.
unsafe impl Send for MemBlockStore {}
unsafe impl Sync for MemBlockStore {}

impl BlockStore for MemBlockStore {
    fn register_pages(&self, _backing: &Backing) -> Result<(), PoolError> {
        Ok(())
    }

    async fn read_page(
        &self,
        key: StripeKey,
        _stripe_off: u64,
        dst: PageRef,
    ) -> Result<bool, PoolError> {
        let fill = fill_byte(&key);
        // SAFETY: `dst` is a page the handler reserved from the scratch
        // backing at `self.base`; the write stays within that page.
        unsafe {
            let p = (self.base + dst.page_idx as usize * self.page_size) as *mut u8;
            std::ptr::write_bytes(p, fill, self.page_size);
        }
        Ok(true)
    }

    async fn write_page(
        &self,
        _key: StripeKey,
        _stripe_off: u64,
        _page: PageRef,
    ) -> Result<(), PoolError> {
        Ok(())
    }
}

/// Deterministic fill byte for a stripe key. The first key byte is
/// enough for `--verify` to detect a page that never arrived (it would
/// read back as zero from the freshly allocated client pool page).
fn fill_byte(key: &StripeKey) -> u8 {
    key.0[0].wrapping_add(0x11)
}

// ---------------------------------------------------------------------
// Runtime / thread pinning.
// ---------------------------------------------------------------------

/// Build the threading runtime used by the fabric endpoints and the
/// client shard. `needed` is the number of distinct worker slots the
/// calling mode pins (see the `*_WORKER_IDX` constants): one per fabric
/// plus one for the client shard. Prefers a `PinnedRuntime` whose CPUs
/// come from `topology::CorePlan`, falling back to an unpinned
/// `DefaultRuntime` with a warning when topology cannot supply `needed`
/// distinct CPUs or `--no-pin` is set.
fn build_runtime(
    host: &Host,
    no_pin: bool,
    needed: usize,
) -> (Arc<dyn Threading>, String, Vec<WorkerSpec>) {
    if no_pin {
        return (
            DefaultRuntime::new(needed),
            "unpinned (DefaultRuntime, --no-pin)".to_string(),
            Vec::new(),
        );
    }
    if host.cpus.is_empty() {
        eprintln!("bench: sysfs topology empty (no CPUs visible); running unpinned");
        return (
            DefaultRuntime::new(needed),
            "unpinned (no sysfs topology)".to_string(),
            Vec::new(),
        );
    }
    // Ask the core planner for the host's disjoint, NUMA-local,
    // cpu0-excluded cores. We pin one distinct CPU per worker slot,
    // drawing from all three classes so both RDMA and non-RDMA hosts
    // yield enough cores.
    let plan = CorePlan::for_host(host, &CorePlanConfig::default());
    // Distinct CPUs in plan order across the three classes; the planner
    // already keeps them disjoint and cpu0-excluded, so the dedup is a
    // safety net that also preserves ordering.
    let mut specs: Vec<WorkerSpec> = Vec::new();
    let mut seen: Vec<u32> = Vec::new();
    for sc in &plan.storage_cores {
        if !seen.contains(&sc.cpu) {
            seen.push(sc.cpu);
            specs.push(WorkerSpec::new(sc.cpu, sc.numa));
        }
    }
    for group in &plan.nic_workers {
        for w in &group.workers {
            if !seen.contains(&w.cpu) {
                seen.push(w.cpu);
                specs.push(WorkerSpec::new(w.cpu, w.numa));
            }
        }
    }
    for s in &plan.serving_shards {
        if !seen.contains(&s.cpu) {
            seen.push(s.cpu);
            specs.push(WorkerSpec::new(s.cpu, s.numa));
        }
    }
    if specs.len() < needed {
        eprintln!(
            "bench: topology produced {} usable CPU(s) (<{needed} needed for this mode); \
             running unpinned",
            specs.len()
        );
        return (
            DefaultRuntime::new(needed),
            "unpinned (insufficient topology CPUs)".to_string(),
            Vec::new(),
        );
    }
    let label = format!("pinned (PinnedRuntime, cpus={seen:?})");
    (PinnedRuntime::new(specs.clone()), label, specs)
}

/// Assign each device `slots_per_device` worker slots that are NUMA-local
/// to that device's HCA. `specs` is the runtime's ordered worker list
/// (`WorkerIdx(i)` pins to `specs[i].cpu`); when it is empty (an unpinned
/// runtime) the slots are handed out sequentially because pinning is then
/// a no-op. The HCA's NUMA node comes from the discovered host topology.
///
/// Drawing each fabric's progress thread (and, on the client, its read
/// shard) from a CPU on the HCA's own NUMA node avoids the cross-socket
/// CQ polling and bounce-buffer traffic that otherwise collapses an
/// 8-HCA fan-out on this 4-node host. When a node's local slots are
/// exhausted, the next free slot from any node is used so we never run
/// short, losing only locality for the overflow.
fn plan_numa_slots(
    host: &Host,
    specs: &[WorkerSpec],
    devices: &[String],
    slots_per_device: usize,
) -> Vec<Vec<WorkerIdx>> {
    if specs.is_empty() {
        // Unpinned: slot identity does not affect placement, so hand
        // out distinct sequential indices.
        let mut next = 0u16;
        return devices
            .iter()
            .map(|_| {
                (0..slots_per_device)
                    .map(|_| {
                        let w = WorkerIdx(next);
                        next = next.wrapping_add(1);
                        w
                    })
                    .collect()
            })
            .collect();
    }
    // Slot indices grouped by their CPU's NUMA node, in spec order.
    let mut by_numa: BTreeMap<Option<u16>, VecDeque<usize>> = BTreeMap::new();
    for (idx, spec) in specs.iter().enumerate() {
        by_numa.entry(spec.numa).or_default().push_back(idx);
    }
    let mut pop = |want: Option<u16>| -> Option<usize> {
        if let Some(n) = want {
            if let Some(q) = by_numa.get_mut(&Some(n)) {
                if let Some(i) = q.pop_front() {
                    return Some(i);
                }
            }
        }
        // Local node exhausted (or unknown): take the first free slot
        // from any node.
        for q in by_numa.values_mut() {
            if let Some(i) = q.pop_front() {
                return Some(i);
            }
        }
        None
    };
    let mut out = Vec::with_capacity(devices.len());
    let mut overflow = 0u16;
    for dev in devices {
        let want = host
            .hcas
            .iter()
            .find(|h| h.dev_name == *dev)
            .and_then(|h| h.numa);
        let mut slots = Vec::with_capacity(slots_per_device);
        for _ in 0..slots_per_device {
            let idx = pop(want).unwrap_or_else(|| {
                // All slots consumed (more requested than specs);
                // reuse from the front. Should not happen because
                // `build_runtime` guarantees specs.len() >= needed.
                let o = overflow;
                overflow = overflow.wrapping_add(1);
                o as usize
            });
            slots.push(WorkerIdx(idx as u16));
        }
        out.push(slots);
    }
    out
}

/// The NUMA node each device's HCA is attached to, in `devices` order.
/// `None` when the device is not found or the host does not expose a node
/// for it. Used to first-touch each fabric's RMA backing on the HCA's
/// local node so the NIC DMAs into near memory instead of bouncing across
/// sockets.
fn hca_numa_list(host: &Host, devices: &[String]) -> Vec<Option<u16>> {
    devices
        .iter()
        .map(|d| {
            host.hcas
                .iter()
                .find(|h| h.dev_name == *d)
                .and_then(|h| h.numa)
        })
        .collect()
}

// ---------------------------------------------------------------------
// Fabric / address helpers.
// ---------------------------------------------------------------------

/// Build and start a fabric endpoint for `device`/`provider` on
/// `runtime`, returning the wrapped `Fabric`.
fn new_fabric(
    common: &CommonArgs,
    device: &str,
    runtime: Arc<dyn Threading>,
    worker_idx: WorkerIdx,
    self_peer: PeerId,
    listen_addr: String,
) -> Result<Arc<Fabric>, String> {
    let mut cfg = defaults_for(device.to_string(), runtime, worker_idx);
    cfg.provider = common.provider.to_fabric();
    cfg.max_inflight = common.inflight;
    cfg.rpc_posted_recvs = common.posted_recvs;
    cfg.progress_threads = common.progress_threads;
    cfg.qps_per_connection = common.qps;
    cfg.write_pipeline_depth = common.write_pipeline;
    // `defaults_for` leaves `self_peer` at PeerId(0). It is the cm
    // CONNREQ private data and the single-dialer input (the lower peer
    // id dials, the higher accepts), so it must be this side's real peer
    // id or the pair fails to converge to one endpoint.
    cfg.self_peer = self_peer;
    // Under FI_EP_MSG every endpoint is a connection-manager listener.
    // Each side advertises its own `self_address` over the control
    // socket; the single-dialer rule then has exactly one side dial and
    // the other accept, so both fabrics must still listen on a
    // peer-dialable address.
    cfg.listen = true;
    cfg.listen_addr = Some(listen_addr);
    cfg.validate()
        .map_err(|e| format!("fabric config invalid: {e}"))?;
    let fabric = Fabric::new(cfg).map_err(|e| format!("Fabric::new failed: {e}"))?;
    Ok(Arc::new(fabric))
}

/// Stringify a fabric's own address into a peer-usable `wire_addr`.
/// Under FI_EP_MSG `self_address` already returns the listener's
/// "ip:port" dial string for both providers.
fn wire_addr_of(fabric: &Fabric, _provider: Provider) -> Result<String, String> {
    let addr = fabric
        .self_address()
        .map_err(|e| format!("self_address failed: {e}"))?;
    if addr.is_empty() {
        return Err("self-address is empty".to_string());
    }
    Ok(addr)
}

// ---------------------------------------------------------------------
// Server bring-up.
// ---------------------------------------------------------------------

/// A running server endpoint plus the resources that must outlive it.
struct ServerEndpoint {
    fabric: Arc<Fabric>,
    device: String,
    wire_addr: String,
    // Held to keep the server running; dropping stops it.
    _rpc: RpcServerHandle,
}

/// Bring up one server fabric on `device`, register its scratch pool,
/// install the production `PoolHandler` over an all-resident
/// `MemBlockStore`, and start the real RPC server. The returned
/// `wire_addr` is the address a client adds as a connection.
fn start_server(
    common: &CommonArgs,
    server: &ServerWorkloadArgs,
    device: &str,
    runtime: Arc<dyn Threading>,
    worker_idx: WorkerIdx,
    hca_numa: Option<u16>,
    listen_addr: String,
) -> Result<ServerEndpoint, String> {
    let fabric = new_fabric(
        common,
        device,
        runtime,
        worker_idx,
        SERVER_PEER_ID,
        listen_addr,
    )?;
    let page_size = PAGE_BYTES;
    // Place this endpoint's page source on the HCA's NUMA node so the
    // NIC DMAs the served pages out of near memory. With `--hugepages`
    // the Hugepage2Mb path hard-binds the region (MPOL_BIND |
    // MPOL_MF_STRICT) so it can never spill cross-node; the Heap path
    // zero-fills (first-touches) on this setup thread, so set the soft
    // mempolicy before `allocate` for it to take effect.
    if let Some(node) = hca_numa {
        let _ = set_preferred_node(node);
    }
    let scratch = allocate(BackingRequest {
        kind: common.backing_kind(),
        bytes: page_size * server.server_pages,
        numa: hca_numa,
    })
    .map_err(|e| format!("server scratch allocation failed: {e}"))?;
    let scratch_pages = scratch.page_count as u32;
    let base = scratch.base as usize;

    let mr = fabric
        .register_backing(&scratch, None)
        .map_err(|e| format!("server register_backing failed: {e}"))?;

    let store = Arc::new(MemBlockStore { base, page_size });
    let handler = Arc::new(PoolHandler::new(store, scratch, scratch_pages));
    let rpc = fabric
        .start_rpc_server::<StripeReq, _>(handler, Some(mr), page_size)
        .map_err(|e| format!("start_rpc_server failed: {e}"))?;

    let wire_addr = wire_addr_of(&fabric, common.provider.to_fabric())?;
    Ok(ServerEndpoint {
        fabric,
        device: device.to_string(),
        wire_addr,
        _rpc: rpc,
    })
}

// ---------------------------------------------------------------------
// Client workload.
// ---------------------------------------------------------------------

/// Aggregated result of one client run.
struct ClientReport {
    elapsed: Duration,
    ops: u64,
    bytes: u64,
    errors: u64,
    latencies: Vec<Duration>,
}

/// Per-worker accumulator. A bounded latency ring keeps memory flat over
/// arbitrarily long runs while still yielding a useful distribution.
struct WorkerStat {
    ops: u64,
    bytes: u64,
    errors: u64,
    lat: Vec<Duration>,
    lat_pos: usize,
}

impl WorkerStat {
    fn new() -> Self {
        Self {
            ops: 0,
            bytes: 0,
            errors: 0,
            lat: Vec::with_capacity(LATENCY_RING),
            lat_pos: 0,
        }
    }

    fn record(&mut self, bytes: u64, latency: Duration) {
        self.ops += 1;
        self.bytes += bytes;
        if self.lat.len() < LATENCY_RING {
            self.lat.push(latency);
        } else {
            self.lat[self.lat_pos] = latency;
            self.lat_pos = (self.lat_pos + 1) % LATENCY_RING;
        }
    }
}

/// The concrete client pool type: production `Pool` over the production
/// `FabricTransport`, with a `NullBlockStore` so every read misses
/// locally and is satisfied by the peer over the fabric.
type ClientPool = Pool<FabricTransport<StripeReq, StaticPeer>, NullBlockStore, StripeReq>;

/// Run the client workload fanned out across `fabrics`. For each fabric
/// it allocates and registers an independent client buffer pool, builds
/// the production transport, and drives `cw.workers` concurrent read
/// tasks on a dedicated (possibly pinned) shard thread for
/// `cw.duration` / `cw.ops`. All shards share one wall-clock deadline so
/// the HCAs run concurrently; their per-worker stats are aggregated into
/// a single report. Effective client concurrency is
/// `cw.workers * fabrics.len()`.
///
/// `peers[i]` is the connection-table peer id the server fabric `i` was
/// added under, `shard_slots[i]` is the `WorkerIdx` shard `i` pins to,
/// and `hca_numa[i]` is the NUMA node fabric `i`'s HCA is attached to (so
/// its RMA backing first-touches near the NIC). All slices must be the
/// same length as `fabrics`.
fn run_client_workload(
    fabrics: &[Arc<Fabric>],
    peers: &[PeerId],
    runtime: Arc<dyn Threading>,
    cw: &ClientWorkloadArgs,
    shard_slots: &[WorkerIdx],
    hca_numa: &[Option<u16>],
    backing_kind: BackingKind,
) -> Result<ClientReport, String> {
    let n = fabrics.len();
    let page_size = PAGE_BYTES;
    let workers = cw.workers.max(1);
    let max_ops = cw.ops;
    let verify = cw.verify;
    // One shared deadline so every HCA's shard times out together.
    let deadline = Instant::now() + Duration::from_secs(cw.duration);

    let mut receivers = Vec::with_capacity(n);
    let mut handles = Vec::with_capacity(n);
    for i in 0..n {
        // Place this fabric's RMA target on its HCA's NUMA node. With
        // `--hugepages` the Hugepage2Mb path hard-binds the region so it
        // cannot spill cross-node; the Heap path zero-fills on this
        // (setup) thread, so the soft mempolicy must be set before
        // `allocate`, not in the shard.
        if let Some(node) = hca_numa[i] {
            let _ = set_preferred_node(node);
        }
        let backing = allocate(BackingRequest {
            kind: backing_kind,
            bytes: page_size * cw.client_pages,
            numa: hca_numa[i],
        })
        .map_err(|e| format!("client pool allocation failed (fabric {i}): {e}"))?;
        let mr = fabrics[i]
            .register_backing(&backing, None)
            .map_err(|e| format!("client register_backing failed (fabric {i}): {e}"))?;
        let transport = FabricTransport::<StripeReq, StaticPeer>::new(
            Arc::clone(&fabrics[i]),
            mr,
            StaticPeer { peer: peers[i] },
            page_size,
        )
        .map_err(|e| format!("FabricTransport::new failed (fabric {i}): {e}"))?;

        // Offset each fabric's key stream so the HCAs fetch distinct
        // pages rather than racing on an identical key sequence.
        let seed = cw
            .seed
            .wrapping_add((i as u64).wrapping_mul(0x9E37_79B9_7F4A_7C15));
        let shard_max_ops = shard_op_budget(max_ops, i, n);
        let (tx, rx) = mpsc::channel::<Result<(Duration, Vec<WorkerStat>), String>>();
        let handle = runtime.spawn_pinned(
            shard_slots[i],
            "bench-transport-client",
            Box::new(move || {
                let res = client_shard(
                    backing,
                    transport,
                    workers,
                    deadline,
                    shard_max_ops,
                    seed,
                    verify,
                );
                let _ = tx.send(res);
            }),
        );
        receivers.push(rx);
        handles.push(handle);
    }

    let mut report = ClientReport {
        elapsed: Duration::ZERO,
        ops: 0,
        bytes: 0,
        errors: 0,
        latencies: Vec::new(),
    };
    let mut first_err: Option<String> = None;
    for (i, rx) in receivers.into_iter().enumerate() {
        match rx.recv() {
            Ok(Ok((elapsed, stats))) => {
                report.elapsed = report.elapsed.max(elapsed);
                for s in stats {
                    report.ops += s.ops;
                    report.bytes += s.bytes;
                    report.errors += s.errors;
                    report.latencies.extend(s.lat);
                }
            }
            Ok(Err(e)) if first_err.is_none() => first_err = Some(format!("fabric {i}: {e}")),
            Ok(Err(_)) => {}
            Err(_) if first_err.is_none() => {
                first_err = Some(format!("fabric {i}: client shard thread died"))
            }
            Err(_) => {}
        }
    }
    for h in handles {
        let _ = h.join();
    }
    if let Some(e) = first_err {
        return Err(e);
    }
    Ok(report)
}

fn shard_op_budget(total: Option<u64>, shard: usize, shards: usize) -> Option<u64> {
    total.map(|ops| {
        let base = ops / shards as u64;
        let extra = u64::from((shard as u64) < (ops % shards as u64));
        base + extra
    })
}

/// The client shard body: runs on the pinned worker thread, owns the
/// `!Send` `Pool`, and drives the worker futures to completion.
fn client_shard(
    backing: Backing,
    transport: FabricTransport<StripeReq, StaticPeer>,
    workers: usize,
    deadline: Instant,
    max_ops: Option<u64>,
    seed: u64,
    verify: bool,
) -> Result<(Duration, Vec<WorkerStat>), String> {
    let pool: ClientPool = Pool::new(PoolConfig::default(), backing, transport, NullBlockStore)
        .map_err(|e| format!("Pool::new failed: {e}"))?;
    let pool = Rc::new(pool);

    if verify {
        verify_client(&pool)?;
        // Verify mode reports a single synthetic stat so the caller's
        // aggregation stays uniform; throughput numbers are meaningless
        // here and the caller prints a verify-specific line.
        return Ok((Duration::ZERO, Vec::new()));
    }

    let stats: Vec<Rc<RefCell<WorkerStat>>> = (0..workers)
        .map(|_| Rc::new(RefCell::new(WorkerStat::new())))
        .collect();
    let remaining_ops = max_ops.map(|n| Rc::new(Cell::new(n)));

    // Drive every worker on the shard's cooperative future-set loop, the
    // same discipline the production shards use (`runtime::ShardLoop`).
    // The Fabric and Pool self-drive their own progress on dedicated
    // threads, so the loop only has to poll the registered worker futures
    // and never registers a tick hook. A `Cell` counter, decremented as
    // each worker returns, gives the stop condition without borrowing the
    // loop from inside its own `should_stop` closure.
    let remaining = Rc::new(Cell::new(workers));
    let mut shard = ShardLoop::new();
    for (w, stat) in stats.iter().enumerate() {
        let pool = Rc::clone(&pool);
        let stat = Rc::clone(stat);
        let remaining = Rc::clone(&remaining);
        let remaining_ops = remaining_ops.as_ref().map(Rc::clone);
        shard.spawn(async move {
            client_worker(pool, stat, w as u64, deadline, remaining_ops, seed).await;
            remaining.set(remaining.get() - 1);
        });
    }

    let start = Instant::now();
    // Throughput path: a zero idle interval keeps the loop spinning so
    // measured latency and bandwidth reflect the transport, not loop
    // parking.
    shard.run_until_with(|| remaining.get() == 0, Duration::ZERO);
    let elapsed = start.elapsed();

    let out: Vec<WorkerStat> = stats
        .into_iter()
        .map(|s| {
            Rc::try_unwrap(s)
                .ok()
                .expect("worker stat still shared")
                .into_inner()
        })
        .collect();
    Ok((elapsed, out))
}

/// One client read task: in a loop, fetch a distinct page over the
/// fabric and record its latency, until the deadline, op cap, or a
/// shutdown signal. Distinct keys per op defeat the pool's in-flight
/// coalescing so every iteration is a real transport fetch.
async fn client_worker(
    pool: Rc<ClientPool>,
    stat: Rc<RefCell<WorkerStat>>,
    worker_id: u64,
    deadline: Instant,
    remaining_ops: Option<Rc<Cell<u64>>>,
    seed: u64,
) {
    let page_len = PAGE_BYTES as u64;
    let mut seq: u64 = 0;
    loop {
        if SHUTDOWN.load(Ordering::Relaxed) {
            break;
        }
        if Instant::now() >= deadline {
            break;
        }
        if let Some(ops) = &remaining_ops {
            let remaining = ops.get();
            if remaining == 0 {
                break;
            }
            ops.set(remaining - 1);
        }
        let key = make_key(seed, worker_id, seq);
        seq = seq.wrapping_add(1);
        let req = StripeReq::new(key);

        let start = Instant::now();
        let mut bytes: u64 = 0;
        let mut failed = false;
        match pool.read(&req, 0, page_len).await {
            Ok(mut stream) => loop {
                match stream.next_page().await {
                    Some(Ok(guard)) => {
                        bytes += guard.len() as u64;
                        drop(guard);
                    }
                    Some(Err(_)) => {
                        failed = true;
                        break;
                    }
                    None => break,
                }
            },
            Err(_) => failed = true,
        }
        let elapsed = start.elapsed();
        let mut s = stat.borrow_mut();
        if failed {
            s.errors += 1;
            if let Some(ops) = &remaining_ops {
                ops.set(ops.get() + 1);
            }
        } else {
            s.record(bytes, elapsed);
        }
    }
}

/// Closed-loop correctness check: fetch a few keys and assert each
/// returned page is exactly the deterministic content the server stamps
/// for that key. Proves bytes traversed the transport.
fn verify_client(pool: &Rc<ClientPool>) -> Result<(), String> {
    const N: u64 = 8;
    let page_len = PAGE_BYTES as u64;
    for i in 0..N {
        let key = make_key(VERIFY_SEED, 0, i);
        let expect = fill_byte(&key);
        let req = StripeReq::new(key);
        let observed = block_on_cooperative(
            async {
                let mut stream = pool.read(&req, 0, page_len).await?;
                let mut first: Option<u8> = None;
                let mut uniform = true;
                let mut total: u64 = 0;
                while let Some(res) = stream.next_page().await {
                    let guard = res?;
                    let s = guard.as_slice();
                    if first.is_none() && !s.is_empty() {
                        first = Some(s[0]);
                    }
                    if let Some(f) = first {
                        if s.iter().any(|&b| b != f) {
                            uniform = false;
                        }
                    }
                    total += s.len() as u64;
                    drop(guard);
                }
                Ok::<_, PoolError>((first, uniform, total))
            },
            || {},
        )
        .map_err(|e| format!("verify read failed for key {i}: {e}"))?;

        let (first, uniform, total) = observed;
        if total != page_len {
            return Err(format!(
                "verify key {i}: fetched {total} bytes, expected {page_len}"
            ));
        }
        match first {
            Some(b) if b == expect && uniform => {}
            Some(b) => {
                return Err(format!(
                    "verify key {i}: page byte = {b:#04x} (uniform={uniform}), expected \
                     {expect:#04x}; bytes did not traverse the transport intact"
                ));
            }
            None => return Err(format!("verify key {i}: empty page")),
        }
    }
    Ok(())
}

/// Seed used by `--verify` so its keys are stable and independent of the
/// throughput workload seed.
const VERIFY_SEED: u64 = 0x5E_71_F0_00_u64;

// ---------------------------------------------------------------------
// Modes.
// ---------------------------------------------------------------------

fn run_server(args: ServerArgs) -> Result<(), String> {
    let devices = args.common.device_list()?;
    let n = devices.len();
    let host = Host::discover();
    // Server pins one CPU per fabric for that fabric's progress threads,
    // chosen NUMA-local to the HCA so CQ polling stays on-socket. No
    // client shard runs in this process.
    let (runtime, pin_label, specs) = build_runtime(&host, args.common.no_pin, n);
    let slots = plan_numa_slots(&host, &specs, &devices, 1);
    let hca_numa = hca_numa_list(&host, &devices);
    let listen_addrs = args.common.listen_addr_list()?;

    let mut endpoints = Vec::with_capacity(n);
    for (i, dev) in devices.iter().enumerate() {
        let ep = start_server(
            &args.common,
            &args.server,
            dev,
            Arc::clone(&runtime),
            slots[i][0],
            hca_numa[i],
            listen_addrs[i].clone(),
        )?;
        endpoints.push(ep);
    }
    let wire_addrs: Vec<String> = endpoints.iter().map(|e| e.wire_addr.clone()).collect();

    let listener = TcpListener::bind(&args.control_addr)
        .map_err(|e| format!("control listen on {} failed: {e}", args.control_addr))?;
    print_header("server", &args.common, &devices, &pin_label, &wire_addrs);
    println!("server: control socket listening on {}", args.control_addr);
    for ep in &endpoints {
        println!(
            "server: fabric device={} wire address = {}",
            ep.device, ep.wire_addr
        );
    }

    loop {
        if SHUTDOWN.load(Ordering::Relaxed) {
            println!("server: shutdown signalled");
            break;
        }
        let (stream, peer) = match listener.accept() {
            Ok(v) => v,
            Err(e) => return Err(format!("control accept failed: {e}")),
        };
        println!("server: client connected from {peer}");
        match exchange_addrs(stream, &wire_addrs) {
            Ok(client_addrs) => {
                if client_addrs.len() != n {
                    eprintln!(
                        "server: client sent {} address(es), expected {n}; ignoring",
                        client_addrs.len()
                    );
                    continue;
                }
                let mut ok = true;
                for (ep, client_addr) in endpoints.iter().zip(&client_addrs) {
                    if let Err(e) = ep.fabric.add_connection(ConnectionSpec {
                        peer: CLIENT_PEER_ID,
                        wire_addr: client_addr.clone(),
                        hca_numa: None,
                        labels: Vec::new(),
                    }) {
                        eprintln!("server: add_connection on {} failed: {e}", ep.device);
                        ok = false;
                        break;
                    }
                }
                if ok {
                    println!("server: added {n} client connection(s); serving (Ctrl-C to stop)");
                }
            }
            Err(e) => eprintln!("server: handshake with {peer} failed: {e}"),
        }
    }
    Ok(())
}

fn run_client(args: ClientArgs) -> Result<(), String> {
    let devices = args.common.device_list()?;
    let n = devices.len();
    let host = Host::discover();
    // Client pins 2 CPUs per fabric: one for the fabric's progress
    // threads and one for that fabric's read-issuing shard. Both are
    // drawn NUMA-local to the HCA so neither the CQ polling nor the
    // shard's submission path crosses sockets.
    let (runtime, pin_label, specs) = build_runtime(&host, args.common.no_pin, 2 * n);
    let slots = plan_numa_slots(&host, &specs, &devices, 2);
    let listen_addrs = args.common.listen_addr_list()?;
    let provider = args.common.provider.to_fabric();

    let mut fabrics = Vec::with_capacity(n);
    let mut self_addrs = Vec::with_capacity(n);
    for (i, dev) in devices.iter().enumerate() {
        let f = new_fabric(
            &args.common,
            dev,
            Arc::clone(&runtime),
            slots[i][0],
            CLIENT_PEER_ID,
            listen_addrs[i].clone(),
        )?;
        let addr = wire_addr_of(&f, provider)?;
        self_addrs.push(addr);
        fabrics.push(f);
    }

    let stream = TcpStream::connect(&args.server_control_addr).map_err(|e| {
        format!(
            "connect to server control {} failed: {e}",
            args.server_control_addr
        )
    })?;
    let server_addrs = exchange_addrs(stream, &self_addrs)?;
    if server_addrs.len() != n {
        return Err(format!(
            "server sent {} address(es), expected {n} (device lists must match)",
            server_addrs.len()
        ));
    }
    for (f, server_addr) in fabrics.iter().zip(&server_addrs) {
        f.add_connection(ConnectionSpec {
            peer: SERVER_PEER_ID,
            wire_addr: server_addr.clone(),
            hca_numa: None,
            labels: Vec::new(),
        })
        .map_err(|e| format!("client.add_connection failed: {e}"))?;
    }

    print_header("client", &args.common, &devices, &pin_label, &server_addrs);

    let peers = vec![SERVER_PEER_ID; n];
    let shard_slots: Vec<WorkerIdx> = (0..n).map(|i| slots[i][1]).collect();
    let hca_numa = hca_numa_list(&host, &devices);

    if args.client.verify {
        run_client_workload(
            &fabrics,
            &peers,
            runtime,
            &args.client,
            &shard_slots,
            &hca_numa,
            args.common.backing_kind(),
        )?;
        println!(
            "verify: OK - {} key(s) fetched and byte-verified over the transport",
            8 * n
        );
        return Ok(());
    }
    let report = run_client_workload(
        &fabrics,
        &peers,
        runtime,
        &args.client,
        &shard_slots,
        &hca_numa,
        args.common.backing_kind(),
    )?;
    print_report(&args.client, &devices, &report);
    Ok(())
}

/// Exchange wire-address lists over the TCP control socket. Writes our
/// address list (a decimal count line followed by one address per line),
/// then reads the peer's in the same format. Both peers write before
/// reading; the payloads are tiny and fit the socket buffer so this
/// cannot deadlock. Used symmetrically by server and client; endpoint
/// `i` on each side is paired by position.
fn exchange_addrs(stream: TcpStream, my_addrs: &[String]) -> Result<Vec<String>, String> {
    let mut writer = stream
        .try_clone()
        .map_err(|e| format!("clone control stream: {e}"))?;
    writeln!(writer, "{}", my_addrs.len()).map_err(|e| format!("write addr count: {e}"))?;
    for addr in my_addrs {
        writeln!(writer, "{addr}").map_err(|e| format!("write self addr: {e}"))?;
    }
    writer.flush().ok();

    let mut reader = BufReader::new(stream);
    let mut count_line = String::new();
    reader
        .read_line(&mut count_line)
        .map_err(|e| format!("read addr count: {e}"))?;
    let count: usize = count_line
        .trim()
        .parse()
        .map_err(|e| format!("parse addr count '{}': {e}", count_line.trim()))?;
    let mut addrs = Vec::with_capacity(count);
    for _ in 0..count {
        let mut line = String::new();
        reader
            .read_line(&mut line)
            .map_err(|e| format!("read peer addr: {e}"))?;
        let addr = line.trim().to_string();
        if addr.is_empty() {
            return Err("peer sent empty address".to_string());
        }
        addrs.push(addr);
    }
    Ok(addrs)
}

// ---------------------------------------------------------------------
// Reporting.
// ---------------------------------------------------------------------

fn print_header(
    mode: &str,
    common: &CommonArgs,
    devices: &[String],
    pin_label: &str,
    server_addrs: &[String],
) {
    let provider = match common.provider {
        BenchProvider::Tcp => "tcp",
        BenchProvider::Verbs => "verbs",
    };
    println!("== bench storage transport ({mode}) ==");
    println!("  provider        : {provider}");
    println!(
        "  devices         : {} ({} HCA endpoint(s))",
        devices.join(","),
        devices.len()
    );
    println!("  threading       : {pin_label}");
    let backing = match common.backing_kind() {
        BackingKind::Hugepage2Mb => "2MiB hugepages (mbind MPOL_BIND|STRICT, NUMA-local)",
        BackingKind::Heap => "heap (MPOL_PREFERRED, NUMA-local first-touch)",
    };
    println!("  rma backing     : {backing}");
    println!("  max_inflight    : {}", common.inflight);
    println!("  posted_recvs    : {}", common.posted_recvs);
    println!("  progress_threads: {}", common.progress_threads);
    println!("  qps/connection  : {}", common.qps);
    println!("  write_pipeline  : {}", common.write_pipeline);
    println!("  page_size       : {}", human_bytes(PAGE_BYTES as u64));
    for (i, addr) in server_addrs.iter().enumerate() {
        println!("  server addr[{i}]  : {addr}");
    }
}

fn print_report(cw: &ClientWorkloadArgs, devices: &[String], report: &ClientReport) {
    let secs = report.elapsed.as_secs_f64().max(f64::MIN_POSITIVE);
    let ops_per_s = report.ops as f64 / secs;
    let bytes_per_s = report.bytes as f64 / secs;
    let hcas = devices.len();

    let mut lat = report.latencies.clone();
    lat.sort_unstable();

    println!("-- results --");
    println!("  hca endpoints   : {hcas}");
    println!("  workers/hca     : {}", cw.workers);
    println!("  total workers   : {}", cw.workers * hcas);
    println!("  wall time       : {}", format_duration(report.elapsed));
    println!("  page fetches    : {}", report.ops);
    println!("  errors          : {}", report.errors);
    println!("  bytes moved     : {}", human_bytes(report.bytes));
    println!("  throughput      : {}/s", human_bytes(bytes_per_s as u64));
    let gbit = bytes_per_s * 8.0 / 1e9;
    println!("  throughput      : {gbit:.1} Gb/s");
    println!("  fetch rate      : {ops_per_s:.0} pages/s");
    if lat.is_empty() {
        println!("  latency         : (no samples)");
    } else {
        println!(
            "  latency p50/p90 : {} / {}",
            format_duration(percentile(&lat, 0.50)),
            format_duration(percentile(&lat, 0.90)),
        );
        println!(
            "  latency p99/p999: {} / {}",
            format_duration(percentile(&lat, 0.99)),
            format_duration(percentile(&lat, 0.999)),
        );
        println!(
            "  latency max     : {} (from {} samples)",
            format_duration(*lat.last().unwrap()),
            lat.len(),
        );
    }
}
