//! Every environment variable Racer accepts. See [`super::spec`] for the engine.

use super::spec::Default::Unset;
use super::spec::Default::Value;
use super::spec::Group;
use super::spec::Kind;
use super::spec::Owner;
use super::spec::Scope;
use super::spec::TextKind;
use super::spec::Unit;
use super::spec::Var;

const MIB: u64 = 1024 * 1024;
/// Largest node-wide byte budget.
pub(super) const MAX_BYTES: u64 = 64 * 1024 * MIB;
/// Largest index-like entry count.
pub(super) const MAX_ENTRIES: u64 = 1_048_576;
/// A stored record: one page, its AEAD tag, and the largest record header.
pub(super) const RECORD_BYTES: u64 =
    crate::model::PAGE_BYTES + 16 + crate::store::MAX_HEADER_BYTES as u64;
pub(super) const DUPLICATE_BYTES: u64 = crate::read::candidates::DUPLICATE_BYTES as u64;
const GPU_UNIT: u64 = super::GPU_STAGING_UNIT;

const fn uint(min: u64, max: u64, unit: Unit) -> Kind {
    Kind::Unsigned {
        min,
        max,
        step: 1,
        unit,
    }
}

const COUNT: Unit = Unit::None;
const BYTES: Unit = Unit::Bytes;
const MS: Unit = Unit::Millis;
const SECS: Unit = Unit::Seconds;

const fn text(kind: TextKind) -> Kind {
    Kind::Text(kind)
}

const fn var(
    name: &'static str,
    group: Group,
    kind: Kind,
    default: super::spec::Default,
    doc: &'static str,
) -> Var {
    Var {
        name,
        kind,
        default,
        scope: Scope::Node,
        owner: Owner::Dataplane,
        group,
        doc,
    }
}

const fn per_worker(mut v: Var) -> Var {
    v.scope = Scope::PerWorker;
    v
}

const fn control(mut v: Var) -> Var {
    v.owner = Owner::Authority;
    v
}

/// A budget the node's I/O workers share, accepted as a node-wide total
/// (divided among workers) or as a per-worker amount (multiplied by the final
/// worker count first). The per-worker form carries the default; setting both
/// is an error.
macro_rules! shared_budget {
    ($node:literal, $worker:literal, $kind:expr, $default:literal, $doc:literal) => {
        [
            var(
                $node,
                Group::Limits,
                $kind,
                Unset(concat!("`", $worker, "` applies")),
                concat!($doc, " Node-wide total, divided among I/O workers."),
            ),
            per_worker(var(
                $worker,
                Group::Limits,
                $kind,
                Value($default),
                concat!($doc, " Multiplied by the final I/O worker count."),
            )),
        ]
    };
}

/// Shared budgets as (node-wide name, per-worker name) pairs, in [`LIMITS`] order.
pub(super) const SHARED_BUDGETS: [(&str, &str); 16] = [
    ("RACER_PLAINTEXT_BYTES", "RACER_PLAINTEXT_BYTES_PER_WORKER"),
    (
        "RACER_CIPHERTEXT_BYTES",
        "RACER_CIPHERTEXT_BYTES_PER_WORKER",
    ),
    ("RACER_DIRTY_BYTES", "RACER_DIRTY_BYTES_PER_WORKER"),
    (
        "RACER_REGISTERED_BYTES",
        "RACER_REGISTERED_BYTES_PER_WORKER",
    ),
    (
        "RACER_REQUEST_CONTEXT_BYTES",
        "RACER_REQUEST_CONTEXT_BYTES_PER_WORKER",
    ),
    ("RACER_FLIGHTS", "RACER_FLIGHTS_PER_WORKER"),
    ("RACER_QUEUE_ENTRIES", "RACER_QUEUE_ENTRIES_PER_WORKER"),
    (
        "RACER_CLIENT_CONNECTIONS",
        "RACER_CLIENT_CONNECTIONS_PER_WORKER",
    ),
    (
        "RACER_PEER_CONNECTIONS",
        "RACER_PEER_CONNECTIONS_PER_WORKER",
    ),
    (
        "RACER_OUTBOUND_CONNECTIONS",
        "RACER_OUTBOUND_CONNECTIONS_PER_WORKER",
    ),
    ("RACER_PIPES", "RACER_PIPES_PER_WORKER"),
    (
        "RACER_PLACEMENT_CACHE_BYTES",
        "RACER_PLACEMENT_CACHE_BYTES_PER_WORKER",
    ),
    ("RACER_CACHED_PATHS", "RACER_CACHED_PATHS_PER_WORKER"),
    (
        "RACER_PATH_CACHE_BYTES",
        "RACER_PATH_CACHE_BYTES_PER_WORKER",
    ),
    (
        "RACER_METADATA_ENTRIES",
        "RACER_METADATA_ENTRIES_PER_WORKER",
    ),
    ("RACER_RELAY_TRANSFERS", "RACER_RELAY_TRANSFERS_PER_WORKER"),
];

const LIMITS: [[Var; 2]; 16] = [
    shared_budget!(
        "RACER_PLAINTEXT_BYTES",
        "RACER_PLAINTEXT_BYTES_PER_WORKER",
        uint(1, MAX_BYTES, BYTES),
        "134217728",
        "Plaintext admission budget for in-flight decrypts and pages held by range reads. Each worker's share must fit one page more than `RACER_RANGE_WINDOW_PAGES`."
    ),
    shared_budget!(
        "RACER_CIPHERTEXT_BYTES",
        "RACER_CIPHERTEXT_BYTES_PER_WORKER",
        uint(1, MAX_BYTES, BYTES),
        "134217728",
        "Ciphertext admission budget, including the memory cache. Each worker's share must fit the range window plus one stored record."
    ),
    shared_budget!(
        "RACER_DIRTY_BYTES",
        "RACER_DIRTY_BYTES_PER_WORKER",
        uint(crate::model::PAGE_BYTES + 16, MAX_BYTES, BYTES),
        "167772160",
        "Budget for pages waiting to be written to disk."
    ),
    shared_budget!(
        "RACER_REGISTERED_BYTES",
        "RACER_REGISTERED_BYTES_PER_WORKER",
        uint(1, MAX_BYTES, BYTES),
        "402653184",
        "Native RDMA page-buffer pool, used only when RDMA is enabled; then at least one page plus 16 bytes. One native thread owns the whole pool, so it is never divided. When neither variable is set, the scaled default is capped at 8 GiB node-wide. The pool is split evenly over RDMA rails, and a rail without a whole buffer is not used. Each grant holds one 18 MiB buffer until its page is consumed; `racer_rdma_free_buffers` shows what is left."
    ),
    shared_budget!(
        "RACER_REQUEST_CONTEXT_BYTES",
        "RACER_REQUEST_CONTEXT_BYTES_PER_WORKER",
        uint(128 * 1024, MAX_BYTES, BYTES),
        "33554432",
        "Request-context budget."
    ),
    shared_budget!(
        "RACER_FLIGHTS",
        "RACER_FLIGHTS_PER_WORKER",
        uint(1, 65_536, COUNT),
        "64",
        "Concurrent single-flight page fetches."
    ),
    shared_budget!(
        "RACER_QUEUE_ENTRIES",
        "RACER_QUEUE_ENTRIES_PER_WORKER",
        uint(2, 65_536, COUNT),
        "256",
        "Queued I/O operations."
    ),
    shared_budget!(
        "RACER_CLIENT_CONNECTIONS",
        "RACER_CLIENT_CONNECTIONS_PER_WORKER",
        uint(1, 65_536, COUNT),
        "64",
        "Client and control connections. A quarter of each worker's share, at most three, is kept for control connections; the rest accept client connections. Peer and outbound connections have their own limits."
    ),
    shared_budget!(
        "RACER_PEER_CONNECTIONS",
        "RACER_PEER_CONNECTIONS_PER_WORKER",
        uint(1, 65_536, COUNT),
        "2048",
        "Inbound peer connections. A peer connection runs one exchange at a time and stays open between them; when every worker's share is in use the listener answers new connections with the unsigned overload head and closes them, so the sender asks elsewhere. Each connection is a file descriptor."
    ),
    shared_budget!(
        "RACER_OUTBOUND_CONNECTIONS",
        "RACER_OUTBOUND_CONNECTIONS_PER_WORKER",
        uint(1, 65_536, COUNT),
        "64",
        "Outbound peer and origin connections, busy or idle. One slot per worker is kept for origin metadata when the share is at least two. Raise it when many concurrent page reads to different peers are refused as `Overloaded`."
    ),
    shared_budget!(
        "RACER_PIPES",
        "RACER_PIPES_PER_WORKER",
        uint(1, 65_536, COUNT),
        "16",
        "Splice pipes."
    ),
    shared_budget!(
        "RACER_PLACEMENT_CACHE_BYTES",
        "RACER_PLACEMENT_CACHE_BYTES_PER_WORKER",
        uint(crate::topology::RANKING_BYTES as u64, 512 * MIB, BYTES),
        "16777216",
        "Cached page placement rankings."
    ),
    shared_budget!(
        "RACER_CACHED_PATHS",
        "RACER_CACHED_PATHS_PER_WORKER",
        uint(1, MAX_ENTRIES, COUNT),
        "128",
        "Cached topology paths."
    ),
    shared_budget!(
        "RACER_PATH_CACHE_BYTES",
        "RACER_PATH_CACHE_BYTES_PER_WORKER",
        uint(1, 512 * MIB, BYTES),
        "8388608",
        "Memory for cached topology paths."
    ),
    shared_budget!(
        "RACER_METADATA_ENTRIES",
        "RACER_METADATA_ENTRIES_PER_WORKER",
        uint(1, MAX_ENTRIES, COUNT),
        "4096",
        "Object metadata catalog entries."
    ),
    shared_budget!(
        "RACER_RELAY_TRANSFERS",
        "RACER_RELAY_TRANSFERS_PER_WORKER",
        uint(1, 65_536, COUNT),
        "16",
        "Concurrent relayed transfers."
    ),
];

const fn flatten_limits() -> [Var; 32] {
    let mut out = [LIMITS[0][0]; 32];
    let mut i = 0;
    while i < 16 {
        out[2 * i] = LIMITS[i][0];
        out[2 * i + 1] = LIMITS[i][1];
        i += 1;
    }
    out
}

const SHARED: [Var; 32] = flatten_limits();

const BASE: &[Var] = &[
    // Control and identity.
    var(
        "RACER_CLUSTER_ID",
        Group::Control,
        text(TextKind::Uuid),
        Unset("managed mode adopts the elected cluster"),
        "Standalone cluster UUID. Set together with `RACER_CONTROL_ENDPOINT`; managed deployments set neither.",
    ),
    var(
        "RACER_CONTROL_ENDPOINT",
        Group::Control,
        text(TextKind::Url),
        Unset("managed mode"),
        "Standalone controller URL. Set together with `RACER_CLUSTER_ID`.",
    ),
    var(
        "RACER_CONTROL_TRUST_BUNDLE",
        Group::Control,
        text(TextKind::Path),
        Value("/etc/racer/trust/ca.crt"),
        "CA bundle that verifies the standalone controller.",
    ),
    var(
        "RACER_CONTROL_TOKEN",
        Group::Control,
        text(TextKind::Path),
        Unset(
            "`/var/run/racer-token/token` in managed mode, `/var/run/secrets/racer-control/token` in standalone mode",
        ),
        "Projected token with the `racer-control` audience, presented to the controller and to control seats.",
    ),
    var(
        "RACER_IDENTITY_DIRECTORY",
        Group::Control,
        text(TextKind::Path),
        Value("/var/lib/racer/identity"),
        "Node-private persistent keys. Must not overlap any other configured path.",
    ),
    var(
        "RACER_SHARES",
        Group::Control,
        uint(1, u32::MAX as u64, COUNT),
        Value("4"),
        "Reported shares; an explicit Node `shares` value wins.",
    ),
    // Threads.
    var(
        "RACER_MAX_THREADS",
        Group::Threads,
        uint(2, u64::MAX, COUNT),
        Unset("no cap"),
        "Total I/O, crypto, RDMA owner (when RDMA is enabled), and managed control threads. Managed deployments need at least 4, or 3 when `RACER_ENABLE_RDMA` is `false`. Kernel io_uring workers are not counted.",
    ),
    var(
        "RACER_IO_WORKERS",
        Group::Threads,
        uint(1, 65_536, COUNT),
        Unset("automatic"),
        "Exact I/O worker count. Requires `RACER_CRYPTO_HELPERS`; their sum, plus one RDMA owner thread when RDMA is enabled, must fit `RACER_MAX_THREADS`. Workers and helpers are spread over the NUMA nodes of the RDMA NICs in proportion to their usable CPUs, with each helper on the node of the workers it serves.",
    ),
    var(
        "RACER_CRYPTO_HELPERS",
        Group::Threads,
        uint(1, 65_536, COUNT),
        Unset("automatic"),
        "Exact crypto helper count, at most `RACER_IO_WORKERS`. Requires `RACER_IO_WORKERS`.",
    ),
    var(
        "RACER_ALLOW_SMT",
        Group::Threads,
        Kind::Bool,
        Value("false"),
        "`true` sizes from logical CPUs instead of one eligible CPU per physical core.",
    ),
    // RDMA and CPU.
    var(
        "RACER_ENABLE_RDMA",
        Group::Hardware,
        Kind::Enum(&["auto", "true", "false"]),
        Value("auto"),
        "`auto` uses native RDMA when built in and ports exist at startup; `true` reserves RDMA resources even without ports, for hotplug; `false` turns it off.",
    ),
    var(
        "RACER_RDMA_BREAKER",
        Group::Hardware,
        Kind::Bool,
        Value("false"),
        "Per peer and rail circuit breaker, off by default: after a hard RDMA failure with a peer (link break, setup failure, grant or write error), or 3 page timeouts in a row with no RDMA success between them, pages to and from it use HTTP at once until a cooldown passes and one probe page succeeds over RDMA. Load (no grant slot or flight room) never trips it.",
    ),
    var(
        "RACER_RDMA_BREAKER_COOLDOWN_MS",
        Group::Hardware,
        uint(1, 3_600_000, MS),
        Value("2000"),
        "First breaker cooldown; it doubles for each consecutive trip, up to `RACER_RDMA_BREAKER_MAX_COOLDOWN_MS`, less up to a fifth as jitter.",
    ),
    var(
        "RACER_RDMA_BREAKER_MAX_COOLDOWN_MS",
        Group::Hardware,
        uint(1, 3_600_000, MS),
        Value("30000"),
        "Longest breaker cooldown; at least `RACER_RDMA_BREAKER_COOLDOWN_MS`.",
    ),
    var(
        "RACER_RDMA_PAGE_TIMEOUT_MS",
        Group::Hardware,
        uint(1, 60_000, MS),
        Value("5000"),
        "Longest one page's RDMA grant or write may take before the page goes over HTTP in the same exchange; counts a soft breaker failure.",
    ),
    var(
        "RACER_RDMA_CM_PORT",
        Group::Hardware,
        uint(1, 65_535, COUNT),
        Value("18515"),
        "TCP port each dataplane's RDMA connection manager listens on; must match on every node.",
    ),
    var(
        "RACER_MEMORY_PRESSURE",
        Group::Hardware,
        Kind::Bool,
        Value("true"),
        "cgroup v2 memory-pressure monitoring and reclaim.",
    ),
    // GPU reads.
    var(
        "RACER_ENABLE_GPU",
        Group::Gpu,
        Kind::Enum(&["auto", "true", "false"]),
        Value("auto"),
        "`auto` turns GPU reads on at the first `gpu-read` if a usable GPU is found; `true` sets GPUs up at startup and fails without one; `false` turns them off.",
    ),
    var(
        "RACER_GPU_STAGING_BYTES",
        Group::Gpu,
        Kind::Unsigned {
            min: GPU_UNIT,
            max: u64::MAX,
            step: GPU_UNIT,
            unit: BYTES,
        },
        Value("268435456"),
        "Ciphertext staging memory per GPU. Below about 34 MiB the GPU does not decrypt and the CPU does.",
    ),
    var(
        "RACER_GPU_LANDING_BYTES",
        Group::Gpu,
        Kind::Unsigned {
            min: 0,
            max: u64::MAX,
            step: GPU_UNIT,
            unit: BYTES,
        },
        Value("268435456"),
        "GPU memory per GPU that peers' NICs write GPU-read pages into over RDMA; `0` turns landing off.",
    ),
    var(
        "RACER_GPU_PCI_P2PDMA",
        Group::Gpu,
        Kind::Bool,
        Value("false"),
        "`true` asks cuFile to read local NVMe into GPU memory over PCI P2PDMA instead of `nvidia-fs`. Needs cuFile 1.13 (CUDA 12.8) or later.",
    ),
    var(
        "RACER_GPU_PEER",
        Group::Gpu,
        Kind::Enum(&["auto", "off"]),
        Value("auto"),
        "`auto` lets one GPU copy another GPU's encrypted page over NVLink or PCIe peer-to-peer where the driver allows it; `off` moves such pages through host memory.",
    ),
    // Storage.
    var(
        "RACER_SLAB_DIRECTORY",
        Group::Storage,
        text(TextKind::Path),
        Value("/var/lib/racer/slabs"),
        "Slab files and checkpoints; still required and writable in device mode.",
    ),
    var(
        "RACER_DEVICE_DIRECTORY",
        Group::Storage,
        text(TextKind::Path),
        Value("/host/dev"),
        "Device root whose `disk/by-id` directory is scanned for raw devices.",
    ),
    var(
        "RACER_DEVICE_BUSY_WAIT_SECS",
        Group::Storage,
        uint(0, 3600, SECS),
        Value("120"),
        "Startup bound for retrying selected devices whose exclusive open is busy (`EBUSY`); `0` skips busy devices immediately.",
    ),
    per_worker(var(
        "RACER_SLAB_BYTES_PER_WORKER",
        Group::Storage,
        uint(1, i64::MAX as u64, BYTES),
        Value("17179869184"),
        "File-backed slab size of each I/O worker; a multiple of `RACER_SEGMENT_BYTES`. Does not limit device-mode capacity.",
    )),
    var(
        "RACER_SEGMENT_BYTES",
        Group::Storage,
        uint(RECORD_BYTES, i64::MAX as u64, BYTES),
        Value("1073741824"),
        "Segment size; must fit one page plus storage overhead.",
    ),
    per_worker(var(
        "RACER_FREE_SEGMENT_RESERVE_PER_WORKER",
        Group::Storage,
        uint(1, MAX_ENTRIES - 1, COUNT),
        Value("2"),
        "Reserved free segments in each I/O worker, fewer than that worker's segments.",
    )),
    var(
        "RACER_DISK_PAGE_ENTRIES",
        Group::Storage,
        uint(1, MAX_ENTRIES, COUNT),
        Value("65536"),
        "Disk page-index budget in file mode; device mode can raise it.",
    ),
    var(
        "RACER_CHECKPOINT_BYTES",
        Group::Storage,
        uint(1, 512 * MIB, BYTES),
        Value("67108864"),
        "Checkpoint working-set budget; device mode can raise it.",
    ),
    per_worker(var(
        "RACER_RETAINED_SEGMENT_PERCENT",
        Group::Storage,
        uint(1, 100, Unit::Percent),
        Value("50"),
        "Spill-hold cap of each I/O worker, as a percentage of its segments and page-index capacity.",
    )),
    var(
        "RACER_RANGE_SPILL_PAGES",
        Group::Storage,
        uint(0, crate::read::spill::MAX_RANGE_SPILL_PAGES as u64, COUNT),
        Value("0"),
        "Pages past the read window that a read may fetch to local disk under a short hold; `0` disables spill.",
    ),
    // Disk retention.
    var(
        "RACER_ADMISSION_MODE",
        Group::Admission,
        Kind::Enum(&["disabled", "second-sight"]),
        Value("second-sight"),
        "Disk retention policy. Does not change request admission.",
    ),
    var(
        "RACER_ADMISSION_HISTORY_BYTES",
        Group::Admission,
        uint(32, 512 * MIB, BYTES),
        Value("4194304"),
        "Bloom history, divided among I/O workers; each needs at least 32 bytes.",
    ),
    var(
        "RACER_ADMISSION_PERIOD_SECS",
        Group::Admission,
        uint(1, 86_400, SECS),
        Value("60"),
        "History rotation period.",
    ),
    // Admission budgets that are not shared among workers.
    var(
        "RACER_WAITERS_PER_FLIGHT",
        Group::Limits,
        uint(1, 4096, COUNT),
        Value("64"),
        "Readers that may wait on one single-flight fetch. Flights times waiters is at most 1,048,576.",
    ),
    per_worker(var(
        "RACER_NEIGHBOR_CONNECTIONS_PER_WORKER",
        Group::Limits,
        uint(1, 1024, COUNT),
        Value("8"),
        "Busy connections each I/O worker keeps to one neighbor; at most the worker's outbound connections.",
    )),
    var(
        "RACER_RANGE_WINDOW_PAGES",
        Group::Limits,
        uint(1, 64, COUNT),
        Value("4"),
        "Pages each read fetches ahead of delivery, in any order.",
    ),
    var(
        "RACER_HEADER_BYTES",
        Group::Limits,
        uint(1024, 32 * 1024, BYTES),
        Value("32768"),
        "Largest accepted HTTP header block.",
    ),
    per_worker(var(
        "RACER_ACTIVE_PATH_SEARCHES_PER_WORKER",
        Group::Limits,
        uint(1, 64, COUNT),
        Value("8"),
        "Distinct unfinished path searches in each I/O worker.",
    )),
    var(
        "RACER_RETAINED_SNAPSHOTS",
        Group::Limits,
        uint(2, 64, COUNT),
        Value("2"),
        "Retained topology snapshots.",
    ),
    // Listeners and peers.
    var(
        "RACER_PEER_LISTEN",
        Group::Network,
        text(TextKind::Address),
        Value("0.0.0.0:7443"),
        "Peer listener. A bracketed IPv4 address, as Kubernetes expands `[$(POD_IP)]`, is accepted. Its port is the peer port published to members.",
    ),
    var(
        "RACER_DIAGNOSTICS_LISTEN",
        Group::Network,
        text(TextKind::Address),
        Value("127.0.0.1:9090"),
        "Diagnostics listener; must differ from the peer listener.",
    ),
    var(
        "RACER_PEER_TCP_NODELAY",
        Group::Network,
        Kind::Bool,
        Value("true"),
        "TCP_NODELAY on peer sockets. `false` only reproduces the old socket behavior.",
    ),
    var(
        "RACER_OPAQUE_RELAY",
        Group::Network,
        Kind::Bool,
        Value("false"),
        "Experimental opaque HTTP transit instead of materialized relay.",
    ),
    var(
        "RACER_PEER_INFLIGHT_MAX",
        Group::Network,
        uint(1, 65_536, COUNT),
        Value("4096"),
        "Peer exchanges in flight.",
    ),
    var(
        "RACER_PEER_PER_NEIGHBOR_MAX",
        Group::Network,
        uint(1, 65_536, COUNT),
        Value("512"),
        "Peer exchanges in flight to one neighbor; at most `RACER_PEER_INFLIGHT_MAX`.",
    ),
    var(
        "RACER_PEER_ADMISSION_RECOVERY_MS",
        Group::Network,
        uint(1, 60_000, MS),
        Value("1000"),
        "Longest time between one-slot recoveries of a reduced peer-exchange limit; it also recovers one slot per `limit` verified completions.",
    ),
    var(
        "RACER_PEER_RECEIVE_MAX",
        Group::Network,
        uint(0, 65_536, COUNT),
        Value("0"),
        "Concurrently served peer requests; `0` turns the receive gate off.",
    ),
    var(
        "RACER_PEER_RECEIVE_QUEUE_MAX",
        Group::Network,
        uint(1, 65_536, COUNT),
        Value("256"),
        "Peer requests queued at the receive gate.",
    ),
    var(
        "RACER_PEER_RECEIVE_WAIT_MS",
        Group::Network,
        uint(1, 30_000, MS),
        Value("1000"),
        "Longest wait at the receive gate.",
    ),
    var(
        "RACER_PAGE_HEDGE_DELAY_MS",
        Group::Network,
        uint(1, 30_000, MS),
        Value("100"),
        "Delay before a hedged second page request.",
    ),
    var(
        "RACER_PAGE_HEDGE_SLOTS",
        Group::Network,
        uint(0, 32, COUNT),
        Value("0"),
        "Concurrent hedged page requests; `0` turns hedging off.",
    ),
    var(
        "RACER_PAGE_HEDGE_BYTES",
        Group::Network,
        uint(0, 32 * DUPLICATE_BYTES, BYTES),
        Value("33554448"),
        "Memory for hedged duplicates; with hedging on, at least one duplicate (two pages plus 16 bytes).",
    ),
    var(
        "RACER_REPLICA_SELECTION",
        Group::Network,
        Kind::Enum(&["primary", "spread"]),
        Value("spread"),
        "Which owner a non-owner asks first. `spread`: direct neighbors first, then a per-node hash spreads pages across owners. `primary`: the first-ranked owner.",
    ),
    // Origins.
    per_worker(var(
        "RACER_CACHE_ORIGIN_CONNECTIONS_PER_WORKER",
        Group::Origin,
        uint(1, 1024, COUNT),
        Value("32"),
        "Origin connections per cache in each I/O worker, independent of peer connections; at most the client connections. When unset it is lowered to fit them. Also caps connections to each Azure Blob endpoint, which caches in one storage account share.",
    )),
    var(
        "RACER_ORIGIN_TRUST_BUNDLE",
        Group::Origin,
        text(TextKind::Path),
        Value(http1::tls::DEFAULT_TRUST_BUNDLE),
        "CA bundle for HTTPS origins and the Entra authority, loaded once at startup; if it cannot be loaded, HTTPS origins fail until restart. Empty selects the default.",
    ),
    var(
        "RACER_AZURE_AUTHORITY_HOST",
        Group::Origin,
        text(TextKind::Url),
        Value(crate::origin::azure_auth::DEFAULT_AUTHORITY_HOST),
        "Microsoft Entra authority, ending in `/`. Empty selects the default.",
    ),
    var(
        "RACER_AZURE_FEDERATED_TOKEN_FILE",
        Group::Origin,
        text(TextKind::Path),
        Unset("workload identity is unavailable"),
        "Projected token exchanged for Entra tokens when an origin uses `workloadIdentity`.",
    ),
    // Prefetch.
    var(
        "RACER_PREFETCH_MAX_BODY_BYTES",
        Group::Prefetch,
        uint(4096, 64 * MIB, BYTES),
        Value("4194304"),
        "`PUT` body limit.",
    ),
    var(
        "RACER_PREFETCH_MAX_REFS",
        Group::Prefetch,
        uint(1, MAX_ENTRIES, COUNT),
        Value("16384"),
        "Refs per `PUT`.",
    ),
    var(
        "RACER_PREFETCH_MAX_TICKETS",
        Group::Prefetch,
        uint(1, 65_536, COUNT),
        Value("1024"),
        "Live and finished tickets, all caches.",
    ),
    var(
        "RACER_PREFETCH_MAX_TTL_SECS",
        Group::Prefetch,
        uint(1, 86_400, SECS),
        Value("3600"),
        "TTL limit.",
    ),
    var(
        "RACER_PREFETCH_TICKET_RETENTION_SECS",
        Group::Prefetch,
        uint(0, 86_400, SECS),
        Value("300"),
        "How long finished tickets stay visible.",
    ),
    var(
        "RACER_PREFETCH_MAX_WAIT_SECS",
        Group::Prefetch,
        uint(1, 600, SECS),
        Value("60"),
        "`wait` limit.",
    ),
    var(
        "RACER_PREFETCH_DEFAULT_PENDING_BYTES",
        Group::Prefetch,
        uint(16 * MIB, 1 << 50, BYTES),
        Value("1073741824"),
        "Per-ticket in-flight bound when `max_pending_bytes` is 0; at most `RACER_PREFETCH_MAX_PENDING_BYTES`.",
    ),
    var(
        "RACER_PREFETCH_MAX_PENDING_BYTES",
        Group::Prefetch,
        uint(16 * MIB, 1 << 50, BYTES),
        Value("17179869184"),
        "Limit on `max_pending_bytes`.",
    ),
    var(
        "RACER_PREFETCH_NODE_PENDING_BYTES",
        Group::Prefetch,
        uint(16 * MIB, 1 << 52, BYTES),
        Value("68719476736"),
        "Queued and in-flight prefetch bytes; checked on each `PUT` that creates or changes a ticket; over it is `429`.",
    ),
    var(
        "RACER_PREFETCH_CONCURRENT_FILLS",
        Group::Prefetch,
        uint(1, 1024, COUNT),
        Value("16"),
        "Concurrent prefetch fills per owner.",
    ),
    var(
        "RACER_PREFETCH_ORIGIN_BYTES_PER_SEC",
        Group::Prefetch,
        uint(0, 1 << 40, Unit::BytesPerSecond),
        Value("0"),
        "Prefetch origin read rate per owner; `0` turns the limit off.",
    ),
    var(
        "RACER_PREFETCH_MAX_ATTEMPTS",
        Group::Prefetch,
        uint(1, 32, COUNT),
        Value("5"),
        "Tries per page.",
    ),
    // Timeouts.
    var(
        "RACER_REQUEST_TIMEOUT_MS",
        Group::Timeouts,
        uint(1, 86_400_000, MS),
        Value("30000"),
        "Data request deadline.",
    ),
    var(
        "RACER_PEER_ATTEMPT_TIMEOUT_MS",
        Group::Timeouts,
        uint(1, 86_400_000, MS),
        Value("30000"),
        "Local cap on one peer exchange; never sent to peers.",
    ),
    var(
        "RACER_READER_STALL_TIMEOUT_MS",
        Group::Timeouts,
        uint(1, 86_400_000, MS),
        Value("10000"),
        "Longest a reader may stall; at most `RACER_REQUEST_TIMEOUT_MS`.",
    ),
    var(
        "RACER_SHUTDOWN_TIMEOUT_MS",
        Group::Timeouts,
        uint(1, 3_600_000, MS),
        Value("30000"),
        "Drain deadline at shutdown.",
    ),
    // Diagnostics.
    var(
        "RACER_PPROF_ENABLED",
        Group::Diagnostics,
        Kind::Bool,
        Value("false"),
        "Serve CPU profiles on the diagnostics listener.",
    ),
    var(
        "RACER_SEND_TAG_PAIR",
        Group::Diagnostics,
        text(TextKind::Other),
        Unset("off"),
        "`sender,receiver` node UUIDs whose peer sends get a sampled AEAD tag check for diagnosis.",
    ),
    // Cluster control. Managed deployments set these from the Pod spec.
    control(var(
        "RACER_NAMESPACE",
        Group::Kubernetes,
        text(TextKind::Name),
        Unset("standalone mode"),
        "Pod namespace. Setting it selects managed mode and starts embedded cluster control.",
    )),
    control(var(
        "RACER_POD_NAME",
        Group::Kubernetes,
        text(TextKind::Name),
        Unset("required in managed mode"),
        "Pod name.",
    )),
    control(var(
        "RACER_POD_UID",
        Group::Kubernetes,
        text(TextKind::Uuid),
        Unset("required in managed mode"),
        "Pod UID.",
    )),
    control(var(
        "RACER_NODE_NAME",
        Group::Kubernetes,
        text(TextKind::Other),
        Unset("required in managed mode"),
        "Kubernetes Node name.",
    )),
    control(var(
        "RACER_POD_IP",
        Group::Kubernetes,
        text(TextKind::Ip),
        Unset("required in managed mode"),
        "Primary Pod IP (`status.podIP`).",
    )),
    control(var(
        "RACER_POD_IPS",
        Group::Kubernetes,
        text(TextKind::IpList),
        Unset("single stack"),
        "Every Pod IP (`status.podIPs`), led by `RACER_POD_IP`, at most one per family. The webhook also serves the other family.",
    )),
    control(var(
        "RACER_CONTROL_SEATS",
        Group::Kubernetes,
        text(TextKind::Other),
        Value("auto"),
        "`auto` picks 1 to 16 seats from the cluster size; a decimal from 1 to 16 fixes the count.",
    )),
    control(var(
        "RACER_CONTROL_LISTEN",
        Group::Kubernetes,
        text(TextKind::Address),
        Value(":8443"),
        "Control seat listener. `:port` binds every IPv4 address.",
    )),
    control(var(
        "RACER_WEBHOOK_LISTEN",
        Group::Kubernetes,
        text(TextKind::Address),
        Value(":9443"),
        "Object sidecar admission webhook listener. `:port` binds every IPv4 address.",
    )),
    control(var(
        "RACER_OBJECT_IMAGE",
        Group::Kubernetes,
        text(TextKind::Image),
        Unset("webhook serving off"),
        "Image injected as the object sidecar.",
    )),
    control(var(
        "RACER_RETAINED_PAGE_KEYS",
        Group::Kubernetes,
        uint(1, racer_wire::control::MAX_RETAINED_PAGE_KEYS as u64, COUNT),
        Value("7"),
        "Readable page keys per cache: the active key plus older decrypt keys. Read by the authority. Lowering it never drops keys at once: extra decrypt keys are dropped at the next key rotation.",
    )),
    control(var(
        "RACER_PAGE_BYTES",
        Group::Kubernetes,
        uint(16 * 1024 * 1024, 16 * 1024 * 1024, BYTES),
        Value("16777216"),
        "Cluster-wide logical page size. Only 16777216 (16 MiB) is accepted: the client SDKs and the disk index do not yet support other sizes. Fixed at cluster creation: the authority publishes the value of the primary that made the first publication, records it in the `racer-state` Secret, and ignores later changes with a warning in the primary's log. Running nodes reject a publication that changes it.",
    )),
    control(var(
        "RACER_KUBE_API_CA",
        Group::Kubernetes,
        text(TextKind::Path),
        Value("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"),
        "Kubernetes API CA bundle.",
    )),
    control(var(
        "RACER_KUBE_API_TOKEN",
        Group::Kubernetes,
        text(TextKind::Path),
        Value("/var/run/secrets/kubernetes.io/serviceaccount/token"),
        "Default-audience token, used only for the Kubernetes API.",
    )),
    control(var(
        "RACER_DAEMONSET",
        Group::Kubernetes,
        text(TextKind::Name),
        Value("racer"),
        "Dataplane DaemonSet name.",
    )),
    control(var(
        "RACER_SERVICE_ACCOUNT",
        Group::Kubernetes,
        text(TextKind::Name),
        Value("racer"),
        "Dataplane ServiceAccount name.",
    )),
    control(var(
        "KUBERNETES_SERVICE_HOST",
        Group::Kubernetes,
        text(TextKind::Other),
        Unset("required in managed mode"),
        "Kubernetes API host, set by the kubelet.",
    )),
    control(var(
        "KUBERNETES_SERVICE_PORT",
        Group::Kubernetes,
        uint(1, 65_535, COUNT),
        Value("443"),
        "Kubernetes API port, set by the kubelet.",
    )),
    // Operator.
    Var {
        name: "RACER_NVIDIA",
        kind: Kind::Enum(&["off", "env", "cdi"]),
        default: Unset("`env`"),
        scope: Scope::Node,
        owner: Owner::Operator,
        group: Group::Operator,
        doc: "How the operator asks the container runtime for NVIDIA driver libraries. Read by the operator from `racer-settings`; the dataplane ignores it.",
    },
];

const fn concat_table() -> [Var; BASE.len() + SHARED.len()] {
    let mut out = [BASE[0]; BASE.len() + SHARED.len()];
    let mut i = 0;
    while i < BASE.len() {
        out[i] = BASE[i];
        i += 1;
    }
    let mut j = 0;
    while j < SHARED.len() {
        out[BASE.len() + j] = SHARED[j];
        j += 1;
    }
    out
}

const ALL: [Var; BASE.len() + SHARED.len()] = concat_table();

/// Every accepted variable.
pub static TABLE: &[Var] = &ALL;
