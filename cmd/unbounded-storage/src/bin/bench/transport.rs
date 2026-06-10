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
//!   `topology::Plan`, exactly like the daemon. When sysfs topology is
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
//!   code paths. Each process owns exactly one fabric, matching the
//!   production daemon's topology.
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
    block_on_cooperative,
};
use unbounded_storage::storage::StripeReq;
use unbounded_storage::topology::{Host, Plan, PlanConfig};

use crate::{
    LATENCY_RING, SHUTDOWN, format_duration, human_bytes, install_signal_handler, make_key,
    percentile,
};

/// Page size moved per request. Matches the production cache page and
/// the `block` bench's user page so one request is one 2 MiB RMA write.
const PAGE_BYTES: usize = HUGEPAGE_2MB;

// Worker-slot assignments. A fabric pins all of its progress threads to
// a single `WorkerIdx`'s CPU, and the read-issuing client shard pins to
// its own. To keep the bench honest these slots must be distinct CPUs so
// the pieces do not contend. Each mode runs a single fabric per process:
// the `server` role has only the fabric; the `client` role has the
// fabric plus its read-issuing shard.

/// `WorkerIdx` for the fabric's progress threads (the lone fabric in
/// both `server` and `client` mode occupies slot 0).
const FABRIC_WORKER_IDX: WorkerIdx = WorkerIdx(0);

/// `WorkerIdx` the client shard runs on in `client` mode, kept off the
/// fabric's CPU (slot 0).
const CLIENT_SHARD_WORKER_IDX: WorkerIdx = WorkerIdx(1);

/// Peer id the client assigns to the server in its connection table.
const SERVER_PEER_ID: PeerId = PeerId(2);

/// Peer id the server assigns to the client in its connection table.
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
    /// device name (e.g. `mlx5_0`) for verbs.
    #[arg(long = "device", default_value = "lo")]
    device: String,

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

    /// Force an unpinned `DefaultRuntime` even when sysfs topology is
    /// available. Useful to isolate the effect of pinning.
    #[arg(long = "no-pin")]
    no_pin: bool,
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
/// come from `topology::Plan`, falling back to an unpinned
/// `DefaultRuntime` with a warning when topology cannot supply `needed`
/// distinct CPUs or `--no-pin` is set.
fn build_runtime(no_pin: bool, needed: usize) -> (Arc<dyn Threading>, String) {
    if no_pin {
        return (
            DefaultRuntime::new(needed),
            "unpinned (DefaultRuntime, --no-pin)".to_string(),
        );
    }
    let host = Host::discover();
    if host.cpus.is_empty() {
        eprintln!("bench: sysfs topology empty (no CPUs visible); running unpinned");
        return (
            DefaultRuntime::new(needed),
            "unpinned (no sysfs topology)".to_string(),
        );
    }
    // Ask the planner for RDMA progress + a couple of handler CPUs and a
    // TCP fallback so we get disjoint, NUMA-local cores on both RDMA and
    // non-RDMA hosts.
    let cfg = PlanConfig {
        rdma_progress_per_hca: 1,
        rdma_handlers_per_hca: 2,
        nvme_threads_per_drive: 0,
        network_shards_per_nic: 0,
        tcp_fallback_threads: 2,
        ..Default::default()
    };
    let plan = Plan::for_host(&host, &cfg);
    // Distinct CPUs in plan order; the planner already keeps them
    // disjoint and cpu0-excluded.
    let mut specs: Vec<WorkerSpec> = Vec::new();
    let mut seen: Vec<u32> = Vec::new();
    for w in &plan.workers {
        if seen.contains(&w.cpu) {
            continue;
        }
        seen.push(w.cpu);
        specs.push(WorkerSpec::new(w.cpu, w.numa));
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
        );
    }
    let label = format!("pinned (PinnedRuntime, cpus={seen:?})");
    (PinnedRuntime::new(specs), label)
}

// ---------------------------------------------------------------------
// Fabric / address helpers.
// ---------------------------------------------------------------------

/// Build and start a fabric endpoint for `device`/`provider` on
/// `runtime`, returning the wrapped `Fabric`.
fn new_fabric(
    common: &CommonArgs,
    runtime: Arc<dyn Threading>,
    worker_idx: WorkerIdx,
) -> Result<Arc<Fabric>, String> {
    let mut cfg = defaults_for(common.device.clone(), runtime, worker_idx);
    cfg.provider = common.provider.to_fabric();
    cfg.max_inflight = common.inflight;
    cfg.rpc_posted_recvs = common.posted_recvs;
    cfg.progress_threads = common.progress_threads;
    cfg.validate()
        .map_err(|e| format!("fabric config invalid: {e}"))?;
    let fabric = Fabric::new(cfg).map_err(|e| format!("Fabric::new failed: {e}"))?;
    Ok(Arc::new(fabric))
}

/// Stringify a fabric's own address into a peer-usable `wire_addr`. For
/// tcp the self-address is a sockaddr blob rendered as `ip:port`; for
/// verbs it is the raw libfabric address rendered as lowercase hex.
fn wire_addr_of(fabric: &Fabric, provider: Provider) -> Result<String, String> {
    let raw = fabric
        .self_address()
        .map_err(|e| format!("self_address failed: {e}"))?;
    let addr = match provider {
        Provider::Tcp => decode_sockaddr_to_string(&raw),
        Provider::Verbs => hex_encode(&raw),
    };
    if addr.is_empty() {
        return Err(format!(
            "could not stringify self-address ({} bytes)",
            raw.len()
        ));
    }
    Ok(addr)
}

fn hex_encode(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut s = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        s.push(HEX[(b >> 4) as usize] as char);
        s.push(HEX[(b & 0x0f) as usize] as char);
    }
    s
}

/// Render a Linux sockaddr blob as `ip:port`, matching the shim's
/// `getaddrinfo` parser. Mirrors the helper in `fabric/tests.rs`.
fn decode_sockaddr_to_string(raw: &[u8]) -> String {
    if raw.len() < 2 {
        return String::new();
    }
    let family = u16::from_ne_bytes([raw[0], raw[1]]);
    const AF_INET: u16 = libc::AF_INET as u16;
    const AF_INET6: u16 = libc::AF_INET6 as u16;
    if family == AF_INET && raw.len() >= 8 {
        let port = u16::from_be_bytes([raw[2], raw[3]]);
        let ip = format!("{}.{}.{}.{}", raw[4], raw[5], raw[6], raw[7]);
        format!("{ip}:{port}")
    } else if family == AF_INET6 && raw.len() >= 28 {
        let port = u16::from_be_bytes([raw[2], raw[3]]);
        let mut groups = [0u16; 8];
        for (i, g) in groups.iter_mut().enumerate() {
            *g = u16::from_be_bytes([raw[8 + 2 * i], raw[8 + 2 * i + 1]]);
        }
        let ip = format!(
            "{:x}:{:x}:{:x}:{:x}:{:x}:{:x}:{:x}:{:x}",
            groups[0], groups[1], groups[2], groups[3], groups[4], groups[5], groups[6], groups[7],
        );
        format!("[{ip}]:{port}")
    } else {
        String::new()
    }
}

// ---------------------------------------------------------------------
// Server bring-up.
// ---------------------------------------------------------------------

/// A running server endpoint plus the resources that must outlive it.
struct ServerEndpoint {
    fabric: Arc<Fabric>,
    wire_addr: String,
    // Held to keep the server running; dropping stops it.
    _rpc: RpcServerHandle,
}

/// Bring up the server fabric, register its scratch pool, install the
/// production `PoolHandler` over an all-resident `MemBlockStore`, and
/// start the real RPC server. The returned `wire_addr` is the address a
/// client adds as a connection.
fn start_server(
    common: &CommonArgs,
    server: &ServerWorkloadArgs,
    runtime: Arc<dyn Threading>,
) -> Result<ServerEndpoint, String> {
    let fabric = new_fabric(common, runtime, FABRIC_WORKER_IDX)?;
    let page_size = PAGE_BYTES;
    let scratch = allocate(BackingRequest {
        kind: BackingKind::Heap,
        bytes: page_size * server.server_pages,
        numa: None,
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

/// Run the client workload against `peer`. Allocates and registers the
/// client buffer pool, builds the production transport, then drives
/// `workers` concurrent read tasks on a (possibly pinned) shard thread
/// for `duration` / `ops`. Returns the aggregated report.
fn run_client_workload(
    fabric: Arc<Fabric>,
    peer: PeerId,
    runtime: Arc<dyn Threading>,
    cw: &ClientWorkloadArgs,
    shard_idx: WorkerIdx,
) -> Result<ClientReport, String> {
    let page_size = PAGE_BYTES;
    let backing = allocate(BackingRequest {
        kind: BackingKind::Heap,
        bytes: page_size * cw.client_pages,
        numa: None,
    })
    .map_err(|e| format!("client pool allocation failed: {e}"))?;
    let mr = fabric
        .register_backing(&backing, None)
        .map_err(|e| format!("client register_backing failed: {e}"))?;
    let transport =
        FabricTransport::<StripeReq, StaticPeer>::new(fabric, mr, StaticPeer { peer }, page_size)
            .map_err(|e| format!("FabricTransport::new failed: {e}"))?;

    let workers = cw.workers.max(1);
    let deadline = Instant::now() + Duration::from_secs(cw.duration);
    let max_ops = cw.ops;
    let seed = cw.seed;
    let verify = cw.verify;

    let (tx, rx) = mpsc::channel::<Result<(Duration, Vec<WorkerStat>), String>>();
    let handle = runtime.spawn_pinned(
        shard_idx,
        "bench-transport-client",
        Box::new(move || {
            let res = client_shard(backing, transport, workers, deadline, max_ops, seed, verify);
            let _ = tx.send(res);
        }),
    );
    let shard_result = rx
        .recv()
        .map_err(|_| "client shard thread died".to_string())?;
    let _ = handle.join();
    let (elapsed, stats) = shard_result?;

    let mut report = ClientReport {
        elapsed,
        ops: 0,
        bytes: 0,
        errors: 0,
        latencies: Vec::new(),
    };
    for s in stats {
        report.ops += s.ops;
        report.bytes += s.bytes;
        report.errors += s.errors;
        report.latencies.extend(s.lat);
    }
    Ok(report)
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
    let per_worker_ops = max_ops.map(|n| n.div_ceil(workers as u64).max(1));

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
        shard.spawn(async move {
            client_worker(pool, stat, w as u64, deadline, per_worker_ops, seed).await;
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
    max_ops: Option<u64>,
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
        if let Some(cap) = max_ops {
            if stat.borrow().ops >= cap {
                break;
            }
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
    // server pins a single CPU: the lone fabric's progress threads (slot
    // 0). No client shard runs in this process.
    let (runtime, pin_label) = build_runtime(args.common.no_pin, 1);
    let provider = args.common.provider.to_fabric();
    let server = start_server(&args.common, &args.server, runtime)?;

    let listener = TcpListener::bind(&args.control_addr)
        .map_err(|e| format!("control listen on {} failed: {e}", args.control_addr))?;
    print_header("server", &args.common, &pin_label, &server.wire_addr);
    println!("server: control socket listening on {}", args.control_addr);
    println!("server: fabric wire address = {}", server.wire_addr);

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
        match handshake_server(stream, &server.wire_addr) {
            Ok(client_addr) => {
                server
                    .fabric
                    .add_connection(ConnectionSpec {
                        peer: CLIENT_PEER_ID,
                        wire_addr: client_addr.clone(),
                        hca_numa: None,
                        labels: Vec::new(),
                    })
                    .map_err(|e| format!("server.add_connection failed: {e}"))?;
                println!("server: added client connection {client_addr}; serving (Ctrl-C to stop)");
            }
            Err(e) => eprintln!("server: handshake with {peer} failed: {e}"),
        }
        // Keep the (single) provider plumbed; the verbs/tcp providers
        // accept multiple clients sequentially. Loop back to accept.
        let _ = provider;
    }
    Ok(())
}

fn run_client(args: ClientArgs) -> Result<(), String> {
    // client pins two CPUs: the lone fabric's progress threads (slot 0)
    // and the read-issuing shard (slot 1).
    let (runtime, pin_label) = build_runtime(args.common.no_pin, 2);
    let provider = args.common.provider.to_fabric();
    let client = new_fabric(&args.common, Arc::clone(&runtime), FABRIC_WORKER_IDX)?;
    let client_addr = wire_addr_of(&client, provider)?;

    let stream = TcpStream::connect(&args.server_control_addr).map_err(|e| {
        format!(
            "connect to server control {} failed: {e}",
            args.server_control_addr
        )
    })?;
    let server_addr = handshake_client(stream, &client_addr)?;
    client
        .add_connection(ConnectionSpec {
            peer: SERVER_PEER_ID,
            wire_addr: server_addr.clone(),
            hca_numa: None,
            labels: Vec::new(),
        })
        .map_err(|e| format!("client.add_connection failed: {e}"))?;

    print_header("client", &args.common, &pin_label, &server_addr);
    if args.client.verify {
        run_client_workload(
            client,
            SERVER_PEER_ID,
            runtime,
            &args.client,
            CLIENT_SHARD_WORKER_IDX,
        )?;
        println!("verify: OK - 8 keys fetched and byte-verified over the transport");
        return Ok(());
    }
    let report = run_client_workload(
        client,
        SERVER_PEER_ID,
        runtime,
        &args.client,
        CLIENT_SHARD_WORKER_IDX,
    )?;
    print_report(&args.client, &report);
    Ok(())
}

/// Server side of the control handshake: send our wire address, read the
/// client's. Both peers write before reading; the payloads are tiny and
/// fit the socket buffer so this cannot deadlock.
fn handshake_server(stream: TcpStream, my_addr: &str) -> Result<String, String> {
    let mut writer = stream
        .try_clone()
        .map_err(|e| format!("clone control stream: {e}"))?;
    writeln!(writer, "{my_addr}").map_err(|e| format!("write self addr: {e}"))?;
    writer.flush().ok();
    let mut reader = BufReader::new(stream);
    let mut line = String::new();
    reader
        .read_line(&mut line)
        .map_err(|e| format!("read peer addr: {e}"))?;
    let addr = line.trim().to_string();
    if addr.is_empty() {
        return Err("peer sent empty address".to_string());
    }
    Ok(addr)
}

/// Client side of the control handshake: symmetric to the server.
fn handshake_client(stream: TcpStream, my_addr: &str) -> Result<String, String> {
    let mut writer = stream
        .try_clone()
        .map_err(|e| format!("clone control stream: {e}"))?;
    writeln!(writer, "{my_addr}").map_err(|e| format!("write self addr: {e}"))?;
    writer.flush().ok();
    let mut reader = BufReader::new(stream);
    let mut line = String::new();
    reader
        .read_line(&mut line)
        .map_err(|e| format!("read peer addr: {e}"))?;
    let addr = line.trim().to_string();
    if addr.is_empty() {
        return Err("server sent empty address".to_string());
    }
    Ok(addr)
}

// ---------------------------------------------------------------------
// Reporting.
// ---------------------------------------------------------------------

fn print_header(mode: &str, common: &CommonArgs, pin_label: &str, server_addr: &str) {
    let provider = match common.provider {
        BenchProvider::Tcp => "tcp",
        BenchProvider::Verbs => "verbs",
    };
    println!("== bench storage transport ({mode}) ==");
    println!("  provider        : {provider}");
    println!("  device          : {}", common.device);
    println!("  threading       : {pin_label}");
    println!("  max_inflight    : {}", common.inflight);
    println!("  posted_recvs    : {}", common.posted_recvs);
    println!("  progress_threads: {}", common.progress_threads);
    println!("  page_size       : {}", human_bytes(PAGE_BYTES as u64));
    println!("  server addr     : {server_addr}");
}

fn print_report(cw: &ClientWorkloadArgs, report: &ClientReport) {
    let secs = report.elapsed.as_secs_f64().max(f64::MIN_POSITIVE);
    let ops_per_s = report.ops as f64 / secs;
    let bytes_per_s = report.bytes as f64 / secs;

    let mut lat = report.latencies.clone();
    lat.sort_unstable();

    println!("-- results --");
    println!("  workers         : {}", cw.workers);
    println!("  wall time       : {}", format_duration(report.elapsed));
    println!("  page fetches    : {}", report.ops);
    println!("  errors          : {}", report.errors);
    println!("  bytes moved     : {}", human_bytes(report.bytes));
    println!("  throughput      : {}/s", human_bytes(bytes_per_s as u64));
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
