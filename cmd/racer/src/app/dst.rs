//! Generated traffic through the assembled production graph on hostless descriptors.
//! The oracle owns immutable origin versions, never consults placement or cache data.

use crate::admission::ResourceClass;
use crate::app::*;
use crate::lifecycle::Service;
use crate::model::PAGE_BYTES;
use crate::model::*;
use racer_crypto::identity::PendingIdentity;
use racer_crypto::identity::SigningIdentity;
use racer_io::ring::Descriptor;
use racer_io::ring::SocketAddress;
use racer_io::sim::Fault;
use racer_io::sim::SimOs;
use racer_wire::control as wire;
use racer_wire::control::CacheDefinition;
use racer_wire::control::CacheId;
use racer_wire::control::MembershipVersion;
use sha2::Digest;
use sha2::Sha256;
use std::cell::RefCell;
use std::collections::BTreeMap;
use std::collections::VecDeque;
use std::num::NonZeroUsize;

/// Assembles one worker graph on `env` through `WorkerApplication::assemble`.
pub(crate) fn local_worker_with_fabric(
    env: &racer_io::Env,
    config: &Config,
    node: &Arc<NodeState>,
    id: u16,
    discovered_nics: Vec<racer_wire::control::RailMapping>,
) -> (WorkerApplication, WorkerRuntime, PageCryptoEngine) {
    let worker = WorkerId(id);
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        config.limits.clone(),
    )));
    let (io, engine) = crate::security::pair(worker, 0, config.limits.queue_entries);
    let runtime = WorkerRuntime {
        reactor: Rc::new(Reactor::new(admission.clone(), env)),
        admission,
        crypto: Rc::new(crate::security::CryptoClient::new(io)),
    };
    let local = WorkerRuntime {
        reactor: runtime.reactor.clone(),
        admission: runtime.admission.clone(),
        crypto: runtime.crypto.clone(),
    };
    let app =
        WorkerApplication::assemble(config, node.clone(), worker, local, discovered_nics).unwrap();
    (
        app,
        runtime,
        PageCryptoEngine::new(CryptoRuntime {
            port: engine,
            ctx: env.ctx.clone(),
        }),
    )
}

/// A one-member publication carrying `caches` at `sequence`.
pub(crate) fn publication(
    config: &Config,
    sequence: u64,
    caches: Vec<racer_wire::control::CacheDefinition>,
) -> wire::Publication {
    wire::Publication {
        pools: Vec::new(),
        schema_version: racer_wire::control::SCHEMA_VERSION,
        cluster: config.cluster.clone(),
        sequence: wire::PublicationSequence(sequence),
        // Pools cover the caches, so each catalog takes its own version.
        membership_version: racer_wire::control::MembershipVersion(sequence),
        members: vec![racer_wire::control::Member {
            node: config.node.clone(),
            shares: std::num::NonZeroU32::new(1).unwrap(),
            peer_endpoint: "127.0.0.1:7443".into(),
            rails: vec![],
            site: String::new(),
        }],
        caches,
        page_size: racer_wire::object::PageSize::DEFAULT,
    }
    .with_default_pool()
}

/// Base node configuration shared by the simulation and unit fixtures.
pub mod cluster {
    use crate::config::Config;
    use crate::config::Limits;
    use racer_wire::control::ClusterId;
    use racer_wire::control::NodeId;
    use std::num::NonZeroU32;
    use std::num::NonZeroUsize;
    use std::time::Duration;

    pub fn config(enable_rdma: bool) -> Config {
        let count = NonZeroUsize::new(16).unwrap();
        let bytes = NonZeroUsize::new(128 * 1024 * 1024).unwrap();
        Config {
            spill: Default::default(),
            prefetch: Default::default(),
            send_tag_pair: None,
            page_hedge: Default::default(),
            replica_selection: Default::default(),
            peer_admission: Default::default(),
            peer_receive: Default::default(),
            shares: NonZeroU32::new(4).unwrap(),
            disk_page_entries: NonZeroUsize::new(65536).unwrap(),
            admission_mode: crate::config::AdmissionMode::SecondSight,
            admission_history_bytes: NonZeroUsize::new(4 * 1024 * 1024).unwrap(),
            admission_period: Duration::from_secs(60),
            checkpoint_bytes: NonZeroUsize::new(64 * 1024 * 1024).unwrap(),
            device_directory: "/host/dev".into(),
            device_busy_wait: Duration::ZERO,
            cluster: ClusterId("00000000-0000-4000-8000-000000000001".into()),
            node: NodeId("00000000-0000-4000-8000-000000000002".into()),
            max_threads: 2,
            io_workers: None,
            crypto_helpers: None,
            allow_smt: false,
            opaque_relay: false,
            peer_tcp_nodelay: false,
            memory_pressure: true,
            pprof_enabled: false,
            enable_rdma,
            rdma_breaker: crate::rdma::breaker::BreakerConfig::default(),
            enable_gpu: crate::config::GpuMode::Disabled,
            gpu_staging_bytes: crate::config::DEFAULT_GPU_STAGING_BYTES,
            gpu_landing_bytes: crate::config::DEFAULT_GPU_LANDING_BYTES,
            gpu_pci_p2pdma: false,
            gpu_peer: true,
            control_endpoint: Some("https://control.invalid".into()),
            managed_control: None,
            enrollment_directory: None,
            peer_listen: "127.0.0.1:0".parse().unwrap(),
            diagnostics_listen: "127.0.0.1:0".parse().unwrap(),
            trust_bundle: "unused/ca".into(),
            service_account_token: "unused/token".into(),
            identity_directory: "unused/identity".into(),
            slab_directory: "unused/slabs".into(),
            slab_bytes: 1024 * 1024 * 1024,
            segment_bytes: 64 * 1024 * 1024,
            free_segment_reserve: 2,
            origin_connections_per_cache: NonZeroUsize::new(8).unwrap(),
            request_timeout: Duration::from_secs(30),
            peer_attempt_timeout: Duration::from_secs(30),
            reader_stall_timeout: Duration::from_secs(10),
            shutdown_timeout: Duration::from_secs(30),
            limits: Limits {
                plaintext_bytes: bytes,
                ciphertext_bytes: bytes,
                dirty_bytes: bytes,
                registered_bytes: bytes,
                request_context_bytes: bytes,
                flights: count,
                waiters_per_flight: count,
                queue_entries: count,
                connections_per_neighbor: count,
                client_connections: count,
                peer_connections: count,
                outbound_connections: count,
                pipes: count,
                range_window_pages: count,
                header_bytes: NonZeroUsize::new(16 * 1024).unwrap(),
                placement_cache_bytes: NonZeroUsize::new(16 * crate::topology::RANKING_BYTES)
                    .unwrap(),
                path_cache_bytes: NonZeroUsize::new(8 * 1024 * 1024).unwrap(),
                active_path_searches: NonZeroUsize::new(8).unwrap(),
                cached_paths: count,
                retained_snapshots: count,
                metadata_entries: count,
                relay_transfers: count,
            },
            scaled_limits: Default::default(),
            origin: Default::default(),
            authority: None,
        }
    }
}

/// Re-encodes a signed head so tests can mutate a private copy.
pub(crate) fn clone_head(
    head: &crate::peer::protocol::SignedHead,
) -> crate::peer::protocol::SignedHead {
    use crate::http::Codec;
    use crate::peer::protocol;
    use crate::peer::protocol::SignedHead;
    let codec = Codec::new(protocol::MAX_HEAD);
    let encoded = codec.encode_head(&head.head).unwrap();
    SignedHead {
        head: codec.decode_head(&encoded).unwrap().unwrap().0,
        signature: head.signature.clone(),
        chain: head.chain.clone(),
    }
}

/// Issues and accepts a node signing identity under the harness CA.
fn signing_identity(
    pending: PendingIdentity,
    ca: &rcgen::Certificate,
    ca_key: &rcgen::KeyPair,
    cluster: racer_wire::control::ClusterId,
    node: racer_wire::control::NodeId,
) -> Arc<SigningIdentity> {
    let (pending, chain) = racer_crypto::identity::test_util::issue_pending(
        pending,
        ca,
        ca_key,
        &cluster,
        &node,
        |_| {},
    );
    Arc::new(
        pending
            .accept(
                cluster,
                node,
                chain,
                &[ca.der().to_vec()],
                &racer_crypto::identity::WallClock::host(),
            )
            .unwrap(),
    )
}

/// Real wakers for every poll the harness makes. Wakes are recorded rather
/// than ignored, so [`Harness::settle`] can fail a run whose pending work has
/// no wake source left (no completion, no in-flight operation, no wake).
mod wake {
    use std::cell::RefCell;
    use std::sync::Arc;
    use std::sync::atomic::AtomicBool;
    use std::sync::atomic::Ordering;
    use std::task::Waker;

    #[derive(Default)]
    struct Flag(AtomicBool);

    impl std::task::Wake for Flag {
        fn wake(self: Arc<Self>) {
            self.0.store(true, Ordering::Release);
        }

        fn wake_by_ref(self: &Arc<Self>) {
            self.0.store(true, Ordering::Release);
        }
    }

    thread_local! {
        static FLAG: RefCell<Option<Arc<Flag>>> = const { RefCell::new(None) };
    }

    fn flag() -> Arc<Flag> {
        FLAG.with(|flag| {
            flag.borrow_mut()
                .get_or_insert_with(Default::default)
                .clone()
        })
    }

    /// The harness thread's recording waker.
    pub(super) fn waker() -> Waker {
        Waker::from(flag())
    }

    /// Whether any task was woken since the last call; clears the record.
    pub(super) fn take() -> bool {
        flag().0.swap(false, Ordering::AcqRel)
    }
}

/// Seeds of the default generated corpus; together they meet every coverage
/// obligation in `coverage_misses`.
const DEFAULT_SEEDS: [u64; 3] = [1, 7, 42];

/// Result of one app scenario run.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Run {
    /// Digest of the run's replay trace; equal inputs give equal digests.
    pub digest: [u8; 32],
}

/// App scenario names accepted by [`run`]: `generated` and
/// `generated-native` replay one seed through generated traffic, churn and
/// faults; `corpus` and `corpus-native` run the default seed set with its
/// coverage obligations; the rest are targeted regressions on fixed seeds.
pub const SCENARIOS: &[&str] = &[
    "generated",
    "generated-native",
    "corpus",
    "corpus-native",
    "default-grace-staggered-checkpoint",
    "origin-recovery-before-healthy-reads",
    "native-faults-warmed-peers",
    "refused-peer-connect-fallback",
    "shutdown-checkpoint-async-io",
    "worker-subscriptions-contend",
    "completed-peer-dispatches-cancellation",
    "healthy-relayed-page-reads",
    "healthy-relayed-page-reads-opaque",
    "listeners-survive-queue-pressure",
    "concurrent-relayed-layers",
    "concurrent-relayed-layers-opaque",
];

/// Runs app scenario `name` (see [`SCENARIOS`]) with `seed` and up to
/// `steps` generated actions (generated scenarios only; 0 means one full
/// action cycle). Every safety invariant (oracle bytes, 412/502 contract,
/// resource bounds, release at quiescence, stranded work) is a panic, which
/// becomes `Err` carrying the panic message.
pub fn run(name: &str, seed: u64, steps: usize) -> std::result::Result<Run, String> {
    let steps = if steps == 0 {
        WEIGHTED_ACTIONS.len()
    } else {
        steps.min(512)
    };
    let outcome = std::panic::catch_unwind(|| {
        let fixed = |f: fn()| {
            f();
            Sha256::digest(name.as_bytes()).into()
        };
        match name {
            "generated" => replay(seed, steps, false).trace.digest(),
            "generated-native" => replay(seed, steps, true).trace.digest(),
            "corpus" => run_corpus(false, &DEFAULT_SEEDS, WEIGHTED_ACTIONS.len(), true),
            "corpus-native" => run_corpus(true, &DEFAULT_SEEDS, WEIGHTED_ACTIONS.len(), true),
            "default-grace-staggered-checkpoint" => {
                fixed(default_grace_staggered_nodes_and_periodic_checkpoint_traffic)
            }
            "origin-recovery-before-healthy-reads" => {
                fixed(dst_origin_recovery_completes_before_concurrent_healthy_reads)
            }
            "native-faults-warmed-peers" => {
                fixed(dst_native_faults_reach_warmed_peers_in_small_topologies)
            }
            "refused-peer-connect-fallback" => {
                fixed(dst_refused_peer_connect_preserves_candidate_fallback)
            }
            "shutdown-checkpoint-async-io" => {
                fixed(dst_shutdown_checkpoint_uses_async_io_and_releases_all_shards)
            }
            "worker-subscriptions-contend" => fixed(
                scenarios::worker_subscriptions_contend_across_servers_and_recover_after_release,
            ),
            "completed-peer-dispatches-cancellation" => {
                fixed(scenarios::completed_peer_dispatches_do_not_exhaust_worker_cancellation)
            }
            "healthy-relayed-page-reads" => fixed(scenarios::healthy_relayed_page_reads),
            "healthy-relayed-page-reads-opaque" => {
                fixed(scenarios::healthy_relayed_page_reads_opaque_opt_in)
            }
            "listeners-survive-queue-pressure" => {
                fixed(scenarios::listeners_survive_repeated_reactor_queue_pressure)
            }
            "concurrent-relayed-layers" => {
                fixed(scenarios::concurrent_relayed_layers_diagnose_receive_pressure_and_recover)
            }
            "concurrent-relayed-layers-opaque" => {
                fixed(scenarios::concurrent_relayed_layers_opaque_opt_in_pressure_and_recovery)
            }
            other => panic!("unknown app scenario {other}"),
        }
    });
    outcome.map(|digest| Run { digest }).map_err(|panic| {
        panic
            .downcast_ref::<String>()
            .cloned()
            .or_else(|| panic.downcast_ref::<&str>().map(|s| (*s).to_string()))
            .unwrap_or_else(|| "non-string panic".into())
    })
}

const MAX_NODES: usize = 32;
const MAX_TURNS: usize = 100_000;
/// Consecutive settle turns with unfinished work but no completion, no
/// in-flight operation and no wake before the run fails as stranded.
const STRANDED_TURNS: usize = 2_000;
/// Subscription fields every simulated reader sends with its POST.
// Keeps the removed Racer-Ordered header as a compatibility check: the
// dataplane ignores it.
const SUBSCRIPTION_FIELDS: &str = "Content-Length: 0\r\nRacer-Page-Credits: 64\r\nRacer-Byte-Credits: 1073741824\r\nRacer-Ordered: 1\r\n";

/// Seeded role entropy replays for the same reads and stays separate between
/// role streams.
#[test]
fn racer_entropy_replay_bytes_are_stable() {
    let clock = racer_io::SimClock::new(7);
    let role = clock.ctx(11);
    let mut bytes = [0; 32];
    for chunk in bytes.chunks_mut(3) {
        role.rng.fill(chunk).unwrap();
    }
    let replay_role = clock.ctx(11);
    let mut replay = [0; 32];
    for chunk in replay.chunks_mut(3) {
        replay_role.rng.fill(chunk).unwrap();
    }
    assert_eq!(bytes, replay);
    let mut neighbor = [0; 32];
    clock.ctx(12).rng.fill(&mut neighbor).unwrap();
    assert_ne!(bytes, neighbor);
}

mod faults;
mod scenarios;
mod traffic;

#[derive(Clone, Copy, Debug)]
enum Action {
    AddNode,
    RemoveNode,
    Update,
    Evict,
    Restart,
    ShortIo,
    ConnectFailure,
    DelayedWrite,
    ClientCancel,
    PeerOutage,
    InflightCrash,
    OldPin,
    FailedDirtyWrite,
    InflightMembership,
    Traffic,
    Partition,
    WallJump,
    OriginFault,
    MalformedClient,
    KeyRetirement,
    CacheRecreate,
    NativeFault,
    PeerSecurity,
    DiskCorruption,
    PendingWriteCrash,
}

// Repeated entries are weights. Keep the original bag order so swap_remove and
// seeded selection preserve the action schedule and random draws.
const WEIGHTED_ACTIONS: &[Action] = &[
    Action::AddNode,
    Action::RemoveNode,
    Action::Update,
    Action::Evict,
    Action::Restart,
    Action::ShortIo,
    Action::ConnectFailure,
    Action::DelayedWrite,
    Action::ClientCancel,
    Action::PeerOutage,
    Action::InflightCrash,
    Action::OldPin,
    Action::FailedDirtyWrite,
    Action::InflightMembership,
    // Former IDs 14-17 all selected ordinary traffic.
    Action::Traffic,
    Action::Traffic,
    Action::Traffic,
    Action::Traffic,
    Action::Partition,
    Action::WallJump,
    Action::OriginFault,
    Action::MalformedClient,
    Action::KeyRetirement,
    Action::CacheRecreate,
    Action::NativeFault,
    Action::PeerSecurity,
    Action::DiskCorruption,
    Action::PendingWriteCrash,
    Action::OriginFault,
    Action::OriginFault,
    Action::OriginFault,
    Action::OriginFault,
    Action::PeerSecurity,
    Action::NativeFault,
    Action::NativeFault,
    Action::NativeFault,
    Action::NativeFault,
    Action::NativeFault,
    Action::NativeFault,
    Action::NativeFault,
    Action::NativeFault,
];

const CLASSES: [ResourceClass; 11] = [
    ResourceClass::Plaintext,
    ResourceClass::Ciphertext,
    ResourceClass::DirtyCiphertext,
    ResourceClass::Registered,
    ResourceClass::RequestContext,
    ResourceClass::Flight,
    ResourceClass::Waiter,
    ResourceClass::Connection,
    ResourceClass::Pipe,
    ResourceClass::ControlProgress,
    ResourceClass::Relay,
];

/// SplitMix64 is local to action generation; cryptographic entropy is a separate stream.
struct Random(u64);
impl Random {
    fn next(&mut self) -> u64 {
        self.0 = self.0.wrapping_add(0x9e3779b97f4a7c15);
        let mut z = self.0;
        z = (z ^ (z >> 30)).wrapping_mul(0xbf58476d1ce4e5b9);
        z = (z ^ (z >> 27)).wrapping_mul(0x94d049bb133111eb);
        z ^ (z >> 31)
    }
    fn pick(&mut self, n: usize) -> usize {
        (self.next() % n as u64) as usize
    }
}

#[derive(Clone)]
struct Version {
    tag: String,
    bytes: Vec<u8>,
}
#[derive(Default)]
struct Catalog {
    current: BTreeMap<usize, Version>,
    calls: usize,
    gets: usize,
    fault: Option<OriginFault>,
    faults: BTreeMap<&'static str, usize>,
}
#[derive(Clone, Copy, Debug)]
enum OriginFault {
    Reject,
    Forbidden,
    DuplicateLength,
    Truncate,
    WrongEtag,
}
#[derive(Default, Debug, PartialEq, Eq)]
struct Coverage {
    actions: BTreeMap<&'static str, usize>,
    operations: BTreeMap<String, usize>,
    injected: BTreeMap<String, usize>,
    observed: BTreeMap<String, usize>,
    success: usize,
    failures: usize,
    bytes: usize,
    persisted: usize,
    relay_turns: usize,
    secondary_worker_turns: usize,
    native_faults: BTreeMap<String, usize>,
    native_writes: usize,
    origin_gets: usize,
    origin_faults: BTreeMap<&'static str, usize>,
    trace: ReplayTrace,
}

/// Ordered streaming digest plus bounded diagnostic history. No host addresses,
/// debug Instants, or allocator identities enter this normalized trace. Resource
/// IDs are the simulators' world-local IDs and are intentionally NOT reordered.
struct ReplayTrace {
    hash: Sha256,
    events: u64,
    recent: VecDeque<String>,
    checkpoints: Vec<(u64, [u8; 32])>,
}
impl std::fmt::Debug for ReplayTrace {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ReplayTrace")
            .field("events", &self.events)
            .field(
                "digest",
                &format_args!("{:x}", self.hash.clone().finalize()),
            )
            .field("recent", &self.recent)
            .finish()
    }
}
impl Default for ReplayTrace {
    fn default() -> Self {
        Self {
            hash: Sha256::new(),
            events: 0,
            recent: VecDeque::new(),
            checkpoints: Vec::new(),
        }
    }
}
impl PartialEq for ReplayTrace {
    fn eq(&self, other: &Self) -> bool {
        self.events == other.events
            && self.digest() == other.digest()
            && self.checkpoints == other.checkpoints
    }
}
impl Eq for ReplayTrace {}

fn handle(fd: &Descriptor) -> racer_io::sim::Handle {
    fd.as_sim().expect("DST escaped into host I/O")
}
fn would_block(error: &std::io::Error) -> bool {
    error.kind() == std::io::ErrorKind::WouldBlock
}
fn key(object: usize) -> String {
    format!("{object:064x}")
}
fn node_id(id: usize) -> NodeId {
    NodeId(format!("{id:08x}-1111-4111-8111-111111111111"))
}
fn cache(id: usize) -> CacheDefinition {
    let name = format!("dst-{id}");
    let (client_socket, origin_socket) =
        racer_wire::control::canonical_socket_paths(&name).unwrap();
    CacheDefinition {
        name,
        id: CacheId("33333333-3333-4333-8333-333333333333".into()),
        client_socket,
        origin_socket,
        priority: racer_wire::control::DEFAULT_PRIORITY,
        mode: racer_wire::control::CacheMode::Cached,
        origin: None,
    }
}

/// Bounded raw HTTP fixture. It deliberately does not use Racer's HTTP codec,
/// range normalization, metadata structs, read service, or crypto implementation.
struct OriginConnection {
    fd: Descriptor,
    input: Vec<u8>,
    output: Vec<u8>,
    sent: usize,
}
struct Adapter {
    listener: Descriptor,
    connections: Vec<OriginConnection>,
}
impl Adapter {
    fn new(sim: &SimOs, id: usize) -> Self {
        let path = cache(id).origin_socket;
        sim.create_dir_all(path.parent().unwrap()).unwrap();
        let _ = sim.unlink(&path);
        Self {
            listener: sim.listen(SocketAddress::Unix(path)).unwrap(),
            connections: vec![],
        }
    }
    fn poll(&mut self, catalog: &Rc<RefCell<Catalog>>) {
        match handle(&self.listener).accept() {
            Ok(fd) => self.connections.push(OriginConnection {
                fd,
                input: vec![],
                output: vec![],
                sent: 0,
            }),
            Err(e) if would_block(&e) => (),
            Err(e) => panic!("origin accept: {e}"),
        }
        self.connections.retain_mut(|c| {
            if c.output.is_empty() {
                let mut bytes = [0; 8192];
                match handle(&c.fd).recv(&mut bytes) {
                    Ok(0) => return false,
                    Ok(n) => c.input.extend_from_slice(&bytes[..n]),
                    Err(e) if would_block(&e) => return true,
                    Err(_) => return false,
                }
                assert!(c.input.len() <= 32768, "unbounded origin head");
                if !c.input.ends_with(b"\r\n\r\n") {
                    return true;
                }
                c.output = origin_response(&c.input, &mut catalog.borrow_mut());
            }
            match handle(&c.fd).send(&c.output[c.sent..]) {
                Ok(n) => c.sent += n,
                Err(e) if would_block(&e) => (),
                Err(_) => return false,
            }
            c.sent != c.output.len()
        });
    }
}

fn headers(bytes: &[u8]) -> (String, BTreeMap<String, String>, usize) {
    let end = bytes
        .windows(4)
        .position(|w| w == b"\r\n\r\n")
        .expect("complete HTTP head")
        + 4;
    let text = std::str::from_utf8(&bytes[..end]).unwrap();
    let mut lines = text.split("\r\n");
    let start = lines.next().unwrap().to_owned();
    let mut fields = BTreeMap::new();
    for line in lines.filter(|l| !l.is_empty()) {
        let (name, value) = line.split_once(": ").expect("canonical HTTP field");
        assert!(
            fields
                .insert(name.to_ascii_lowercase(), value.into())
                .is_none(),
            "duplicate field"
        );
    }
    (start, fields, end)
}
fn origin_response(bytes: &[u8], catalog: &mut Catalog) -> Vec<u8> {
    let (start, h, _) = headers(bytes);
    let mut parts = start.split_whitespace();
    let method = parts.next().unwrap();
    let object = usize::from_str_radix(
        parts.next().unwrap().strip_prefix("/v1/objects/").unwrap(),
        16,
    )
    .unwrap();
    assert_eq!(h.get("host").map(String::as_str), Some("racer"));
    assert_eq!(
        h.get("racer-metadata").map(String::as_str),
        Some("dst opaque metadata")
    );
    assert_eq!(
        h.get("authorization").map(String::as_str),
        Some("Bearer dst-fixture")
    );
    catalog.calls += 1;
    if let Some(fault) = catalog.fault.take() {
        let (name, status) = match fault {
            OriginFault::Reject => ("credential-reject", 401),
            OriginFault::Forbidden => ("credential-forbidden", 403),
            OriginFault::DuplicateLength => ("origin-duplicate-length", 0),
            OriginFault::Truncate => ("origin-truncated-body", 0),
            OriginFault::WrongEtag => ("origin-malformed-etag", 0),
        };
        *catalog.faults.entry(name).or_default() += 1;
        if status != 0 {
            return format!(
                "HTTP/1.1 {status} Rejected\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
            )
            .into_bytes();
        }
        let mut response = origin_response(bytes, catalog);
        match fault {
            OriginFault::DuplicateLength => (),
            OriginFault::Truncate => {
                response.pop();
            }
            OriginFault::WrongEtag => {
                if let Some(at) = response.windows(6).position(|w| w == b"ETag: ") {
                    response[at + 6] = b'W';
                }
            }
            _ => unreachable!(),
        }
        if matches!(fault, OriginFault::DuplicateLength) {
            let at = response.windows(4).position(|w| w == b"\r\n\r\n").unwrap();
            response.splice(at..at, b"\r\nContent-Length: 999".iter().copied());
        }
        return response;
    }
    let Some(version) = catalog.current.get(&object) else {
        return b"HTTP/1.1 404 Missing\r\nContent-Length: 0\r\nConnection: close\r\n\r\n".to_vec();
    };
    if h.get("if-match").is_some_and(|tag| tag != &version.tag) {
        return b"HTTP/1.1 412 Gone\r\nContent-Length: 0\r\nConnection: close\r\n\r\n".to_vec();
    }
    let size = version.bytes.len();
    let (status, first, end) = if method == "HEAD" || size == 0 {
        (200, 0, size)
    } else {
        let (first, last) = h["range"]
            .strip_prefix("bytes=")
            .unwrap()
            .split_once('-')
            .unwrap();
        let first: usize = first.parse().unwrap();
        let last: usize = last.parse().unwrap();
        assert_eq!(first % PAGE_BYTES as usize, 0);
        assert!(last == first + PAGE_BYTES as usize - 1 || last + 1 == size);
        if first >= size {
            return format!("HTTP/1.1 416 Range\r\nContent-Length: 0\r\nContent-Range: bytes */{size}\r\nConnection: close\r\n\r\n").into_bytes();
        }
        (206, first, (last + 1).min(size))
    };
    let range = if status == 206 {
        format!("Content-Range: bytes {first}-{}/{size}\r\n", end - 1)
    } else {
        String::new()
    };
    let mut response = format!("HTTP/1.1 {status} Result\r\nContent-Length: {}\r\nContent-Type: application/octet-stream\r\nETag: {}\r\nRacer-Expires-At: 0\r\n{range}Connection: close\r\n\r\n", end - first, version.tag).into_bytes();
    if method == "GET" {
        catalog.gets += 1;
        response.extend_from_slice(&version.bytes[first..end]);
    }
    response
}

struct Node {
    id: usize,
    config: Config,
    workers: Vec<LocalWorker>,
    adapter: Adapter,
    control: Option<Rc<Session>>,
}
/// Page crypto plus, for the first worker built, the node's RDMA owner, both
/// stepped on the harness thread. Production runs the owner on its own
/// thread; this harness is single-threaded and deterministic.
struct SteppedCrypto {
    engine: crate::security::PageCryptoEngine,
    owner: Option<rdma_verbs::Owner>,
}

impl crate::lifecycle::Service<RequestScope> for SteppedCrypto {
    fn start<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        self.engine.start(scope)
    }

    fn poll_budgeted(&mut self, cx: &mut Context<'_>, budget: usize) -> Result<()> {
        self.engine.poll_budgeted(cx, budget)?;
        if let Some(owner) = &mut self.owner {
            owner.poll_budgeted(budget)?;
        }
        Ok(())
    }

    fn drain<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        self.engine.drain(scope)
    }

    fn shutdown<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        self.engine.shutdown(scope)
    }
}

fn stepped_crypto(
    node: &NodeState,
    engine: crate::security::PageCryptoEngine,
) -> Box<dyn crate::lifecycle::Service<RequestScope>> {
    let owner = node.native.take_owner(engine.ctx().clone());
    Box::new(SteppedCrypto { engine, owner })
}

struct LocalWorker {
    app: WorkerApplication,
    runtime: WorkerRuntime,
    crypto: Box<dyn crate::lifecycle::Service<RequestScope>>,
}
impl Node {
    /// Steps every worker once; returns the completions reaped.
    fn poll(&mut self, budget: usize) -> usize {
        let mut completions = 0;
        for worker in &mut self.workers {
            if worker.app.control.is_some() {
                // This harness supplies accepted publications and keys directly.
                // Keep listener-owner semantics without starting either live feed.
                worker.app.control_task = Some(Box::pin(std::future::pending()));
                worker.app.keyring_task = Some(Box::pin(std::future::pending()));
            }
            let _local = worker
                .app
                .directory
                .simulation_scope(Some((worker.app.worker, worker.app.coordinator.clone())));
            completions += worker.runtime.reactor.poll_budgeted(budget).unwrap();
            worker
                .crypto
                .poll_budgeted(&mut Context::from_waker(&wake::waker()), budget)
                .unwrap();
            worker.runtime.crypto.poll_budgeted(budget).unwrap();
            worker
                .app
                .poll_budgeted(&mut Context::from_waker(&wake::waker()), budget)
                .unwrap();
            for class in CLASSES {
                assert!(
                    worker.runtime.admission.used(class) <= worker.runtime.admission.limit(class),
                    "node {} resource bound",
                    self.id
                );
            }
            assert!(
                worker.app.store.writer.pending_count() <= self.config.limits.queue_entries.get()
            );
            assert!(
                worker.app.clients.active_connections()
                    <= self.config.limits.client_connections.get()
            );
        }
        completions
    }
}

struct Harness {
    /// Shared publication retention selected before constructing any worker graph.
    retained_snapshots: usize,

    seed: u64,
    rng: Random,
    sim: SimOs,
    nodes: Vec<Node>,
    next_node: usize,
    generation: u64,
    catalog: Rc<RefCell<Catalog>>,
    oracle: BTreeMap<(usize, String), Vec<u8>>,
    revisions: BTreeMap<usize, u64>,
    coverage: Coverage,
    ca: rcgen::Certificate,
    ca_key: rcgen::KeyPair,
    clock: racer_io::SimClock,
    fabric: rdma_verbs::simulation::Simulation,
    native: bool,
    key_epoch: u8,
    cache_epoch: u64,
    origin_faults: Vec<OriginFault>,
    security_faults: Vec<bool>,
    native_rules: Vec<(
        rdma_verbs::simulation::Operation,
        rdma_verbs::simulation::Fault,
    )>,
    payload_regression: bool,
    opaque_relay: bool,
    concurrent_layers: bool,
}
impl Harness {
    fn new(seed: u64, sim: SimOs, clock: racer_io::SimClock, native: bool) -> Self {
        let key = racer_crypto::SigningKey::from_seed(&[91; 32])
            .to_pkcs8_der()
            .unwrap();
        let ca_key = rcgen::KeyPair::from_pkcs8_der_and_sign_algo(
            &rustls::pki_types::PrivatePkcs8KeyDer::from(key.as_slice()),
            &rcgen::PKCS_ED25519,
        )
        .unwrap();
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
        params.key_usages = vec![
            rcgen::KeyUsagePurpose::KeyCertSign,
            rcgen::KeyUsagePurpose::CrlSign,
        ];
        let ca = params.self_signed(&ca_key).unwrap();
        Self {
            retained_snapshots: 64,
            seed,
            rng: Random(seed),
            sim,
            nodes: vec![],
            next_node: 0,
            generation: 0,
            catalog: Rc::new(RefCell::new(Catalog::default())),
            oracle: BTreeMap::new(),
            revisions: BTreeMap::new(),
            coverage: Coverage::default(),
            ca,
            ca_key,
            clock,
            fabric: rdma_verbs::simulation::Simulation::new(),
            native,
            key_epoch: 0,
            cache_epoch: 0,
            origin_faults: vec![],
            security_faults: vec![],
            native_rules: vec![],
            payload_regression: false,
            opaque_relay: false,
            concurrent_layers: false,
        }
    }
    fn update(&mut self, object: usize) {
        let revision = self.revisions.entry(object).or_default();
        *revision += 1;
        let length = match object {
            1..=4 if self.concurrent_layers => 4 * PAGE_BYTES as usize + 257,
            0 => 0,
            1 if self.payload_regression => 4 * PAGE_BYTES as usize + 257,
            1 => PAGE_BYTES as usize + 257,
            _ => 1 + self.rng.pick(16384),
        };
        let mut random =
            Random(self.seed ^ (object as u64).rotate_left(17) ^ revision.rotate_left(37));
        let bytes: Vec<u8> = (0..length).map(|_| random.next() as u8).collect();
        let tag = format!("\"object-{object}-v{revision}\"");
        self.coverage.trace.record(format!(
            "update:{object}:{revision}:{length}:{:x}",
            Sha256::digest(&bytes)
        ));
        self.oracle.insert((object, tag.clone()), bytes.clone());
        self.catalog
            .borrow_mut()
            .current
            .insert(object, Version { tag, bytes });
        self.coverage.action("update");
    }
    fn identity(&self, config: &Config, id: usize) -> Arc<SigningIdentity> {
        let mut random = Random(self.seed ^ id as u64);
        let seed = std::array::from_fn(|_| random.next() as u8);
        let key = racer_crypto::SigningKey::from_seed(&seed)
            .to_pkcs8_der()
            .unwrap();
        let pending = PendingIdentity::recover(&key).unwrap();
        signing_identity(
            pending,
            &self.ca,
            &self.ca_key,
            config.cluster.clone(),
            config.node.clone(),
        )
    }
    fn add(&mut self, env: &racer_io::Env, restart: Option<usize>) {
        let id = restart.unwrap_or_else(|| {
            let id = self.next_node;
            self.next_node += 1;
            id
        });
        // Restart is a new process incarnation, not a rewind of nonce entropy.
        let role = self.clock.ctx(1 + id as u64 + (self.generation << 32));
        let env = racer_io::Env::new(env.driver.clone(), role.clone());
        let device = format!("dst-rnic-{id}");
        let gid = [1 + (id % 254) as u8; 16];
        let fabric = self
            .fabric
            .with_devices(vec![rdma_verbs::simulation::Device::new(
                device.clone(),
                gid,
            )])
            .unwrap();
        let _fabric = fabric.enter();
        let mut config = cluster::config(self.native);
        config.opaque_relay = self.opaque_relay;
        // Memory pressure reads the real host cgroup and PSI files, which the
        // simulation does not control. Turn it off so replays stay deterministic.
        config.memory_pressure = false;
        config.node = node_id(id);
        config.peer_listen = format!("127.0.0.1:{}", 20000 + id).parse().unwrap();
        config.diagnostics_listen = format!("127.0.0.1:{}", 30000 + id).parse().unwrap();
        config.slab_directory = format!("/dst/node-{id}/slabs").into();
        config.slab_bytes = 256 * 1024 * 1024;
        config.free_segment_reserve = 1;
        config.limits.queue_entries = NonZeroUsize::new(64).unwrap();
        config.limits.client_connections = NonZeroUsize::new(64).unwrap();
        config.limits.connections_per_neighbor = NonZeroUsize::new(2).unwrap();
        config.limits.range_window_pages = NonZeroUsize::new(2).unwrap();
        config.limits.metadata_entries = NonZeroUsize::new(128).unwrap();
        config.limits.retained_snapshots = NonZeroUsize::new(self.retained_snapshots).unwrap();
        // Match one quarter of the production node budget, including peer envelope
        // staging. This fixture assembles workers directly, bypassing size_workers.
        config.limits.request_context_bytes = NonZeroUsize::new(16 * 1024 * 1024).unwrap();
        if self.payload_regression {
            config.limits.plaintext_bytes = NonZeroUsize::new(64 * 1024 * 1024).unwrap();
            config.limits.ciphertext_bytes = NonZeroUsize::new(64 * 1024 * 1024).unwrap();
        }
        if self.concurrent_layers {
            config.limits.plaintext_bytes = NonZeroUsize::new(256 * 1024 * 1024).unwrap();
            config.limits.ciphertext_bytes = NonZeroUsize::new(256 * 1024 * 1024).unwrap();
            config.limits.client_connections = NonZeroUsize::new(512).unwrap();
            config.limits.connections_per_neighbor = NonZeroUsize::new(16).unwrap();
        }
        let worker_count = 1 + self.rng.pick(2);
        self.coverage.trace.record(format!(
            "node:{id}:{restart:?}:{worker_count}:{}",
            self.generation
        ));
        let worker_ids: Vec<_> = (0..worker_count).map(|i| WorkerId(i as u16)).collect();
        let mut state = NodeState::new(worker_ids.clone(), 64, env.ctx.clone()).unwrap();
        // Retention belongs to the shared publication authority, not worker handles.
        state.publications = Arc::new(crate::control::cell::PublicationCell::new(
            crate::control::Snapshot::retention(config.limits.retained_snapshots.get()),
        ));
        let node = Arc::new(state);
        if self.native {
            node.native
                .prepare(worker_ids.into_iter(), &config.limits)
                .unwrap();
        }
        let discovered_nics = if self.native {
            vec![racer_wire::control::RailMapping {
                rail: racer_wire::control::RailId(0),
                numa_node: None,
                device,
                port: 1,
                gid: Some(gid),
            }]
        } else {
            Vec::new()
        };
        let (mut app, runtime, engine) =
            local_worker_with_fabric(&env, &config, &node, 0, discovered_nics);
        let crypto = stepped_crypto(&node, engine);
        app.keys
            .install(self.bundle(&config, u64::from(self.key_epoch) + 1))
            .unwrap();
        app.keys
            .install_signing_identity(self.identity(&config, id))
            .unwrap();
        app.telemetry
            .attach_io(runtime.reactor.clone(), runtime.admission.clone())
            .unwrap();
        // The harness supplies accepted control inputs; startup/recovery and every
        // datapath dependency remain the production application implementation.
        let control = app.control.take();
        self.generation += 1;
        let mut publication = publication(&config, self.generation, vec![self.definition(id)]);
        publication.membership_version = MembershipVersion(self.generation);
        publication.members = self.members().into_iter().map(Into::into).collect();
        publication.members.push(member(&config).into());
        app.snapshots.apply(publication).unwrap();
        let startup = scope(&env.ctx.clock, Duration::from_secs(30)).unwrap();
        let mut workers = vec![LocalWorker {
            app,
            runtime,
            crypto,
        }];
        for worker in 1..worker_count {
            let role = self
                .clock
                .ctx(1 + id as u64 + (self.generation << 32) + ((worker as u64) << 48));
            let env = racer_io::Env::new(env.driver.clone(), role.clone());
            let (app, runtime, engine) = local_worker_with_fabric(
                &env,
                &config,
                &node,
                worker as u16,
                workers[0].app.discovered_nics.clone(),
            );
            let crypto = stepped_crypto(&node, engine);
            workers.push(LocalWorker {
                app,
                runtime,
                crypto,
            });
        }
        lifecycle(&mut workers, &startup, Lifecycle::Start);
        self.persist_provisioning(id, &config, &workers);
        if self.native {
            // Rails are node-wide: the owner's worker activates them for all.
            assert!(
                !workers[0].app.actual_rails.is_empty()
                    && workers
                        .iter()
                        .all(|w| !w.app.links.as_ref().unwrap().active().is_empty()),
                "native activation must not silently fall back"
            );
        }
        if worker_count > 1 {
            self.coverage.action("multi-worker");
        }
        self.start_node_listeners(&env, id, &config, &mut workers[0], &startup);
        let adapter = Adapter::new(&self.sim, id);
        self.nodes.push(Node {
            id,
            config,
            workers,
            adapter,
            control,
        });
        self.publish();
        self.tick();
        self.coverage
            .action(if restart.is_some() { "restart" } else { "add" });
    }

    fn persist_provisioning(&self, id: usize, config: &Config, workers: &[LocalWorker]) {
        // Persist provisioning, not cached page writes. Every ancestor binding is
        // explicit so a power loss cannot erase the fixture's whole node directory.
        for path in [
            std::path::PathBuf::from("/"),
            "/dst".into(),
            format!("/dst/node-{id}").into(),
            config.slab_directory.clone(),
        ] {
            self.sim.disk().sync(&path).unwrap();
        }
        for worker in workers {
            self.sim
                .disk()
                .sync(
                    &config
                        .slab_directory
                        .join(format!("worker-{}-slab-0.dat", worker.app.worker.0)),
                )
                .unwrap();
        }
    }

    fn start_node_listeners(
        &self,
        env: &racer_io::Env,
        id: usize,
        config: &Config,
        worker: &mut LocalWorker,
        startup: &RequestScope,
    ) {
        let LocalWorker {
            app,
            runtime,
            crypto,
        } = worker;
        drive_local(
            runtime,
            &mut **crypto,
            app.clients.reconcile(&[self.definition(id)], startup),
        )
        .unwrap();
        let peers = app.peers.clone();
        let address = config.peer_listen;
        let listener_scope = scope(&env.ctx.clock, Duration::from_secs(365 * 24 * 3600)).unwrap();
        let peer_scope = listener_scope.clone();
        app.listener_scope = Some(listener_scope);
        app.peer_task = Some(Box::pin(
            async move { peers.listen(address, &peer_scope).await },
        ));
    }
    fn members(&self) -> Vec<crate::topology::Member> {
        self.nodes.iter().map(|n| member(&n.config)).collect()
    }
    fn definition(&self, id: usize) -> CacheDefinition {
        let mut definition = cache(id);
        definition.id = CacheId(format!("33333333-3333-4333-8333-{:012x}", self.cache_epoch));
        definition
    }
    fn bundle(&self, config: &Config, _generation: u64) -> wire::KeyringBundle {
        wire::KeyringBundle {
            schema_version: racer_wire::control::KEYRING_SCHEMA_VERSION,
            cluster: config.cluster.clone(),
            generation: wire::BundleGeneration(u64::from(self.key_epoch) + 1),
            peer_trust_roots: vec![self.ca.der().to_vec()],
            cache_keys: [
                wire::CacheKeyPurpose::Page,
                wire::CacheKeyPurpose::OriginCredentials,
            ]
            .into_iter()
            .enumerate()
            .map(|(i, purpose)| {
                wire::CacheEncryptionKey::new(
                    wire::CacheKeyRef {
                        cache: self.definition(0).id,
                        id: crate::model::key_id_from_generation(
                            u64::from(self.key_epoch) + 1,
                            7 + i as u32 + u32::from(self.key_epoch) * 2,
                        )
                        .unwrap(),
                        purpose,
                    },
                    wire::CacheKeyState::Active,
                    zeroize::Zeroizing::new([19 + i as u8 + self.key_epoch * 2; 32]),
                )
            })
            .collect(),
        }
    }
    fn publish(&mut self) {
        self.generation += 1;
        let members = self.members();
        let definitions: Vec<_> = self.nodes.iter().map(|n| self.definition(n.id)).collect();
        for (node, definition) in self.nodes.iter_mut().zip(definitions) {
            let mut p = publication(&node.config, self.generation, vec![definition]);
            p.membership_version = MembershipVersion(self.generation);
            p.members = members.iter().cloned().map(Into::into).collect();
            node.workers[0].app.snapshots.apply(p).unwrap();
        }
    }
    /// Advances 1 ms and steps every node once; returns completions reaped.
    fn tick(&mut self) -> usize {
        let mut completions = 0;
        self.clock.advance(Duration::from_millis(1));
        let start = self.rng.pick(self.nodes.len());
        let budget = 1 + self.rng.pick(64);
        self.coverage.trace.record(format!(
            "tick:{}:{start}:{budget}",
            self.clock.elapsed().as_nanos()
        ));
        for offset in 0..self.nodes.len() {
            let index = (start + offset) % self.nodes.len();
            let _endpoint = self
                .sim
                .enter_endpoint(SocketAddress::Inet(self.nodes[index].config.peer_listen));
            self.nodes[index].adapter.poll(&self.catalog);
            completions += self.nodes[index].poll(budget);
            for worker in &self.nodes[index].workers {
                let used: Vec<_> = CLASSES
                    .iter()
                    .map(|class| worker.runtime.admission.used(*class))
                    .collect();
                self.coverage.trace.record(format!(
                    "invariant:{}:{}:{used:?}:{}:{}:{}:{}",
                    self.nodes[index].id,
                    worker.app.worker.0,
                    worker.app.drivers.pending(),
                    worker.runtime.crypto.outstanding(),
                    worker.runtime.reactor.in_flight(),
                    worker.app.store.writer.pending_count()
                ));
            }
            self.coverage.secondary_worker_turns += self.nodes[index]
                .workers
                .iter()
                .skip(1)
                .filter(|w| {
                    w.runtime.crypto.outstanding() != 0
                        || w.app.drivers.pending() != 0
                        || w.app.store.writer.pending_count() != 0
                })
                .count();
            self.coverage.relay_turns += usize::from(
                self.nodes[index].workers[0]
                    .runtime
                    .admission
                    .used(ResourceClass::Relay)
                    > 0,
            );
        }
        self.coverage.collect(&self.sim);
        self.coverage.collect_native(&self.fabric);
        assert!(
            self.sim.live_handles()
                <= self.nodes.iter().map(|n| n.workers.len()).sum::<usize>() * 256 + 32,
            "descriptor bound"
        );
        completions
    }
    fn remove(&mut self, env: &racer_io::Env, index: usize) -> usize {
        self.retire(env, index, false)
    }
    fn retire(&mut self, env: &racer_io::Env, index: usize, crash: bool) -> usize {
        let mut node = self.nodes.remove(index);
        self.coverage
            .trace
            .record(format!("retire:{}:{crash}", node.id));
        let shutdown = scope(&env.ctx.clock, Duration::from_secs(30)).unwrap();
        if crash {
            self.sim
                .disk()
                .crash_under(std::path::Path::new(&format!("/dst/node-{}", node.id)))
                .unwrap();
            node.workers[0].app.directory.simulation_crash();
            for worker in &mut node.workers {
                if let Some(endpoint) = &mut worker.app.endpoint {
                    endpoint.simulation_crash();
                }
                worker.app.drivers.simulation_crash();
            }
            node.workers[0].app.directory.simulation_crash();
        } else {
            lifecycle(&mut node.workers, &shutdown, Lifecycle::Drain);
            // Production checkpoint publication promises an atomic logical cut,
            // not fsync durability (store/checkpoint.rs). Power-loss actions may
            // therefore lose it; never assert durability merely from shutdown.
            lifecycle(&mut node.workers, &shutdown, Lifecycle::Shutdown);
        }
        let id = node.id;
        let Node {
            workers,
            adapter,
            control,
            ..
        } = node;
        drop((adapter, control));
        let mut admissions = Vec::new();
        for LocalWorker {
            app,
            runtime,
            mut crypto,
        } in workers
        {
            let _local = app
                .directory
                .simulation_scope(Some((app.worker, app.coordinator.clone())));
            let drivers = app.drivers.clone();
            let directory = app.directory.clone();
            // The simulated process is gone even though this oracle retains its
            // admission counters. Prevent final payload owners from recycling
            // into a dead process's idle pool while completion fences run.
            runtime.admission.stop();
            drop(app);
            if crash {
                drivers.simulation_crash();
                directory.simulation_crash();
                // This is only the OS cancellation fence after graph destruction,
                // not WorkerApplication::drain: no acquisition, persistence, or
                // checkpoint producer is polled after the process-loss cut.
                let mut fence = runtime.reactor.drain();
                let mut done = false;
                for _ in 0..MAX_TURNS {
                    runtime.reactor.poll_budgeted(64).unwrap();
                    if let Poll::Ready(result) = fence
                        .as_mut()
                        .poll(&mut Context::from_waker(&wake::waker()))
                    {
                        result.unwrap();
                        done = true;
                        break;
                    }
                }
                assert!(done, "crash kernel fence stalled");
            } else {
                drive_local(&runtime, &mut *crypto, runtime.reactor.drain()).unwrap();
                assert_eq!(runtime.crypto.outstanding(), 0);
            }
            let admission = runtime.admission.clone();
            drop((drivers, directory));
            drop((runtime, crypto));
            admissions.push(admission);
        }
        for admission in admissions {
            let used: Vec<_> = CLASSES.iter().map(|class| admission.used(*class)).collect();
            self.coverage
                .trace
                .record(format!("retired-invariants:{id}:{used:?}"));
            for class in CLASSES {
                assert_eq!(
                    admission.used(class),
                    0,
                    "retired node {id}, class {class:?}"
                );
            }
        }
        self.publish();
        self.coverage.action(if crash { "crash" } else { "remove" });
        id
    }
    fn settle(&mut self) {
        let mut idle = 0;
        for _ in 0..MAX_TURNS {
            let completions = self.tick();
            let woken = wake::take();
            let in_flight: usize = self
                .nodes
                .iter()
                .flat_map(|n| &n.workers)
                .map(|w| w.runtime.reactor.in_flight() + w.runtime.crypto.outstanding())
                .sum();
            idle = if completions == 0 && !woken && in_flight == 0 {
                idle + 1
            } else {
                0
            };
            assert!(
                idle < STRANDED_TURNS,
                "seed={} pending work without a wake source for {idle} turns: {:?}",
                self.seed,
                self.coverage
            );
            if self.nodes.iter().flat_map(|n| &n.workers).all(|n| {
                n.app.store.writer.is_idle()
                    && n.app.writer_task.is_none()
                    && n.app.clients.active_connections() == 0
                    && n.runtime.crypto.outstanding() == 0
                    && n.app.drivers.pending() == 0
                    && n.app.links.as_ref().is_none_or(|links| links.setups() == 0)
            }) {
                return;
            }
        }
        panic!("seed={} failed to settle: {:?}", self.seed, self.coverage);
    }

    /// Set up the persistent link from node `a` to node `b` and wait until
    /// both ends are ready, waiting out any backoff. If `a` is ready but `b`
    /// is not (`b` restarted under `a`'s link), `b` initiates instead.
    fn connect_links(&mut self, env: &racer_io::Env, a: usize, b: usize) {
        let rail = racer_wire::control::RailId(0);
        for _ in 0..64 {
            let (local, peer) = (
                self.nodes[a].config.node.clone(),
                self.nodes[b].config.node.clone(),
            );
            let links = self.nodes[a].workers[0].app.links.clone().unwrap();
            let remote = self.nodes[b].workers[0].app.links.clone().unwrap();
            if links.ready(&peer, rail).is_some() && remote.ready(&local, rail).is_some() {
                self.coverage.action("native-link");
                return;
            }
            for state in [links.state(&peer, rail), remote.state(&local, rail)] {
                if let rdma_verbs::LinkState::Backoff(until) = state {
                    self.clock.advance(
                        until.saturating_duration_since(env.ctx.now()) + Duration::from_millis(1),
                    );
                }
            }
            // A crash-restart of `b` leaves `a` holding a half-open `Ready`
            // link that `a` only discovers on use, so `a` never wants a setup.
            // In production the restarted peer sets up the
            // link when it needs one and `a` replaces the stale link; do that.
            let (from, to, target) = if links.ready(&peer, rail).is_some() {
                (b, a, local)
            } else {
                (a, b, peer)
            };
            let endpoint =
                crate::http::Endpoint::Peer(self.nodes[to].config.peer_listen.to_string());
            self.nodes[from].workers[0]
                .app
                .peers
                .transfers()
                .ensure_link(&endpoint, &target, rail);
            self.settle();
        }
        panic!("seed={} link setup stalled", self.seed);
    }

    fn request(&mut self, object: usize, pinned: bool, head: bool) -> Client {
        let node = self.rng.pick(self.nodes.len());
        self.request_on(object, pinned, head, node)
    }
    fn request_on(&mut self, object: usize, pinned: bool, head: bool, node: usize) -> Client {
        let (tag, size) = {
            let catalog = self.catalog.borrow();
            let version = &catalog.current[&object];
            (version.tag.clone(), version.bytes.len())
        };
        let (first, end) = if !pinned {
            (0, size.min(PAGE_BYTES as usize))
        } else if size == 0 {
            (0, 0)
        } else {
            let first = self.rng.pick(size);
            (first, first + 1 + self.rng.pick(size - first))
        };
        let range = if head {
            String::new()
        } else if !pinned {
            format!("Range: bytes=0-{}\r\n", PAGE_BYTES - 1)
        } else if size == 0 {
            "Range: bytes=0-0\r\n".into()
        } else {
            format!("Range: bytes={first}-{}\r\n", end - 1)
        };
        let pin = if pinned {
            format!("If-Match: {tag}\r\n")
        } else {
            String::new()
        };
        let request = format!("{} /v1/objects/{} HTTP/1.1\r\nHost: racer\r\n{}{pin}{range}Racer-Metadata: dst opaque metadata\r\nAuthorization: Bearer dst-fixture\r\nConnection: close\r\n\r\n", if head { "HEAD" } else { "POST" }, key(object), if head { "" } else { SUBSCRIPTION_FIELDS }).into_bytes();
        let fd = self
            .sim
            .connect(SocketAddress::Unix(
                cache(self.nodes[node].id).client_socket,
            ))
            .unwrap();
        self.coverage.trace.record(format!(
            "request:{}:{object}:{first}:{end}:{head}:{pinned}:{:x}",
            self.nodes[node].id,
            Sha256::digest(&request)
        ));
        Client {
            frame_cursor: None,
            releases: Vec::new(),
            released: 0,
            fd: Some(fd),
            request,
            sent: 0,
            response: vec![],
            object,
            tag,
            first,
            end,
            size,
            head,
            pinned,
            done: false,
            disconnected: false,
            expected_status: None,
        }
    }

    fn traffic(&mut self, count: usize, faulted: bool) {
        let mut clients = Vec::new();
        for _ in 0..count {
            let object = self.rng.pick(8);
            let pinned = self.rng.pick(2) == 0;
            let head = self.rng.pick(5) == 0;
            clients.push(self.request(object, pinned, head));
        }
        for _ in 0..MAX_TURNS {
            self.tick();
            let start = self.rng.pick(clients.len());
            for offset in 0..clients.len() {
                let index = (start + offset) % clients.len();
                if clients[index].done {
                    continue;
                }
                if clients[index].poll() {
                    self.check(&clients[index], faulted);
                    clients[index].fd.take();
                    clients[index].done = true;
                }
            }
            if clients.iter().all(|c| c.done) {
                self.settle();
                return;
            }
        }
        panic!(
            "seed={} traffic stalled; coverage={:?}",
            self.seed, self.coverage
        );
    }

    fn exchange(&mut self, mut client: Client, faulted: bool) {
        for _ in 0..MAX_TURNS {
            self.tick();
            if client.poll() {
                self.check(&client, faulted);
                client.fd.take();
                self.settle();
                return;
            }
        }
        panic!("seed={} exchange stalled", self.seed);
    }

    fn object_id(&self, object: usize) -> ObjectId {
        ObjectId {
            cache: self.definition(self.nodes[0].id).id,
            key: CacheKey::parse_hex(key(object).as_bytes()).unwrap(),
        }
    }

    fn ranked_nodes(&self, object: &ObjectId) -> Vec<usize> {
        // Use a separate placement cache for fixture setup, not the byte oracle.
        Placement::new(1)
            .rank(
                self.nodes[0].workers[0]
                    .app
                    .snapshots
                    .current()
                    .unwrap()
                    .membership
                    .clone(),
                object,
                PageNumber(0),
            )
            .ordered
            .iter()
            .map(|id| {
                self.nodes
                    .iter()
                    .position(|n| &n.config.node == id)
                    .unwrap()
            })
            .collect()
    }

    fn check(&mut self, client: &Client, faulted: bool) {
        self.coverage.trace.record(format!(
            "client:{}:{}:{}:{}:{}:{}:{}:{faulted}:{}:{:x}",
            client.object,
            client.tag,
            client.first,
            client.end,
            client.head,
            client.pinned,
            client.disconnected,
            client.response.len(),
            Sha256::digest(&client.response)
        ));
        if !client.response.windows(4).any(|w| w == b"\r\n\r\n") {
            assert!(faulted, "healthy client disconnected before headers");
            self.coverage.failures += 1;
            return;
        }
        let (start, headers, end) = headers(&client.response);
        let status: u16 = start.split_whitespace().nth(1).unwrap().parse().unwrap();
        let length: usize = headers["content-length"].parse().unwrap();
        if status >= 400 {
            assert_eq!(length, 0);
            assert_eq!(client.response.len(), end);
            assert!(!headers.contains_key("etag") && !headers.contains_key("racer-expires-at"));
            if status == 416 {
                assert!(client.size == 0 && !client.head);
                assert_eq!(headers["content-range"], "bytes */0");
                self.coverage.action("empty-range");
            } else {
                assert!(
                    (faulted && matches!(status, 401 | 403 | 502 | 503))
                        || client.expected_status == Some(status),
                    "unexpected status {status}: {start}"
                );
                self.coverage.failures += 1;
            }
            return;
        }
        assert_eq!(
            headers["etag"], client.tag,
            "fresh read or pin switched versions"
        );
        assert!(
            client.expected_status.is_none(),
            "malformed client request was accepted"
        );
        let expected = &self.oracle[&(client.object, headers["etag"].clone())];
        assert_eq!(expected.len(), client.size);
        let body = &client.response[end..];
        if client.head {
            assert_eq!(status, 200);
            assert_eq!(length, client.size);
            assert!(body.is_empty());
            assert!(!headers.contains_key("content-range"));
        } else {
            assert_eq!(status, 200);
            assert_eq!(headers["content-type"], "application/octet-stream");
            assert_eq!(headers["racer-range-start"], client.first.to_string());
            assert!(body.len() <= length);
            let mut cursor = 0;
            let mut offset = client.first;
            while cursor + 21 <= body.len() {
                let kind = body[cursor];
                let start =
                    u64::from_be_bytes(body[cursor + 9..cursor + 17].try_into().unwrap()) as usize;
                let count =
                    u32::from_be_bytes(body[cursor + 17..cursor + 21].try_into().unwrap()) as usize;
                cursor += 21;
                if kind == 2 {
                    assert_eq!(offset, client.end);
                    assert_eq!(count, 0);
                    break;
                }
                assert_eq!(kind, 1);
                assert_eq!(start, offset);
                let available = count.min(body.len() - cursor);
                assert_eq!(
                    &body[cursor..cursor + available],
                    &expected[offset..offset + available],
                    "oracle byte mismatch"
                );
                cursor += available;
                offset += available;
                if available != count {
                    break;
                }
            }
            if body.len() != length {
                if !faulted || !client.disconnected {
                    let mut diagnostics = String::new();
                    for node in &self.nodes {
                        for worker in &node.workers {
                            worker
                                .app
                                .telemetry
                                .failures
                                .write(&mut diagnostics)
                                .unwrap();
                        }
                    }
                    panic!(
                        "healthy response truncated: body={} expected={length} offset={offset}: {diagnostics}",
                        body.len()
                    );
                }
                self.coverage.failures += 1;
                return;
            }
        }
        self.coverage.success += 1;
        self.coverage.bytes += if client.head {
            0
        } else {
            client.end - client.first
        };
    }
}

struct Client {
    frame_cursor: Option<usize>,
    releases: Vec<u8>,
    released: usize,
    fd: Option<Descriptor>,
    request: Vec<u8>,
    sent: usize,
    response: Vec<u8>,
    object: usize,
    tag: String,
    first: usize,
    end: usize,
    size: usize,
    head: bool,
    pinned: bool,
    done: bool,
    disconnected: bool,
    expected_status: Option<u16>,
}

pub(super) fn default_grace_staggered_nodes_and_periodic_checkpoint_traffic() {
    let sim = SimOs::new();
    let clock = racer_io::SimClock::new(505);
    let env = racer_io::Env::sim(&sim, clock.ctx(0));
    let mut harness = Harness::new(505, sim, clock, false);
    harness.retained_snapshots = 2;
    for object in 0..8 {
        harness.update(object);
    }
    for _ in 0..4 {
        harness.add(&env, None);
    }
    // Retention is fixed when the node-wide authority is constructed. The shared
    // policy already retains the delayed topology; never replace a worker's view.
    let old = harness.nodes[0].workers[0]
        .app
        .snapshots
        .current()
        .unwrap()
        .membership
        .version;
    let members = harness.members();
    harness.generation += 1;
    for i in 1..4 {
        let node = &harness.nodes[i];
        let mut p = publication(
            &node.config,
            harness.generation,
            vec![harness.definition(node.id)],
        );
        p.membership_version = MembershipVersion(harness.generation);
        p.members = members.iter().cloned().map(Into::into).collect();
        node.workers[0].app.snapshots.apply(p).unwrap();
        assert!(
            node.workers[0]
                .app
                .node
                .publications
                .resolve(old.0, env.ctx.now())
                .unwrap()
                .is_some()
        );
    }
    let client = harness.request_on(2, false, false, 0);
    harness.exchange(client, false);
    harness.clock.advance(Duration::from_secs(6));
    for _ in 0..500 {
        harness.tick();
    }
    for node in &harness.nodes {
        assert!(
            node.workers[0]
                .app
                .telemetry
                .metrics
                .gauge(Gauge::CheckpointSequence)
                > 0
        );
    }
    // A node cannot resolve a future membership it has not received. Complete
    // propagation before requiring arbitrary new-to-old requests to succeed.
    harness.publish();
    harness.traffic(4, false);
}

pub(super) fn dst_origin_recovery_completes_before_concurrent_healthy_reads() {
    for native in [false, true] {
        let sim = SimOs::new();
        let clock = racer_io::SimClock::new(106);
        let env = racer_io::Env::sim(&sim, clock.ctx(0));
        let mut harness = Harness::new(106, sim, clock, native);
        for object in 0..8 {
            harness.update(object);
        }
        for _ in 0..3 {
            harness.add(&env, None);
        }
        for fault in [
            OriginFault::DuplicateLength,
            OriginFault::Truncate,
            OriginFault::WrongEtag,
            OriginFault::Reject,
            OriginFault::Forbidden,
        ] {
            harness.origin_faults = vec![fault];
            let failures = harness.coverage.failures;
            harness.origin_fault();
            assert!(
                harness.coverage.failures > failures,
                "fault did not fail a read"
            );
            let failures = harness.coverage.failures;
            harness.traffic(8, false);
            assert_eq!(harness.coverage.failures, failures);
        }
        assert_eq!(harness.coverage.actions["origin-recovered"], 5);
        while !harness.nodes.is_empty() {
            harness.remove(&env, 0);
        }
        assert_eq!(harness.sim.live_handles(), 0);
        assert_eq!(harness.fabric.live_resources(), 0);
    }
}

pub(super) fn dst_native_faults_reach_warmed_peers_in_small_topologies() {
    for nodes in [1, 2, 3] {
        let sim = SimOs::new();
        let clock = racer_io::SimClock::new(118);
        let env = racer_io::Env::sim(&sim, clock.ctx(0));
        let mut harness = Harness::new(118, sim, clock, true);
        for _ in 0..nodes {
            harness.add(&env, None);
        }
        for _ in 0..9 {
            harness.native_fault(&env);
            assert_eq!(harness.fabric.pending_faults(), 0);
        }
        // One complete rule bag covers Bind/WriteImm/Invalidate x all three faults.
        assert!(harness.native_rules.is_empty());
        assert_eq!(harness.coverage.native_faults.len(), 9);
        assert!(harness.coverage.native_faults.values().all(|&n| n == 1));
        assert!(harness.coverage.native_writes > 0);
        while !harness.nodes.is_empty() {
            harness.remove(&env, 0);
        }
        assert_eq!(harness.sim.live_handles(), 0);
        assert_eq!(harness.fabric.live_resources(), 0);
    }
}

pub(super) fn dst_refused_peer_connect_preserves_candidate_fallback() {
    // A refused connect is the peer's failure; EADDRNOTAVAIL (this node out of ephemeral ports)
    // is local load and must retry rather than fail the read. Both reads succeed.
    for (native, errno) in [
        (false, libc::ECONNREFUSED),
        (true, libc::ECONNREFUSED),
        (false, libc::EADDRNOTAVAIL),
        (true, libc::EADDRNOTAVAIL),
    ] {
        let sim = SimOs::new();
        let clock = racer_io::SimClock::new(119);
        let env = racer_io::Env::sim(&sim, clock.ctx(0));
        let mut harness = Harness::new(119, sim, clock, native);
        for _ in 0..4 {
            harness.add(&env, None);
        }
        harness.update(2);
        let ranked = harness.ranked_nodes(&harness.object_id(2));
        let requester = (0..4).find(|node| !ranked.contains(node)).unwrap();
        // The first cold request from a noncandidate connects to a peer before
        // origin. Raw ECONNREFUSED must retain the same fallback as synthetic Io.
        harness.sim.inject("connect", Fault::Errno(errno)).unwrap();
        let client = harness.request_on(2, false, false, requester);
        harness.exchange(client, false);
        assert_eq!(harness.coverage.observed.get("connect"), Some(&1));
        assert_eq!(harness.coverage.success, 1);
        assert_eq!(harness.coverage.failures, 0);
        // A refused owner is routed around through another candidate; an exhausted local port
        // range retries the same owner later.
        if errno == libc::ECONNREFUSED {
            assert!(harness.coverage.relay_turns > 0);
        }
        while !harness.nodes.is_empty() {
            harness.remove(&env, 0);
        }
        assert_eq!(harness.sim.live_handles(), 0);
        assert_eq!(harness.fabric.live_resources(), 0);
    }
}

pub(super) fn dst_shutdown_checkpoint_uses_async_io_and_releases_all_shards() {
    use crate::store::checkpoint::read_candidates;
    for case in [
        "empty",
        "periodic",
        "write-error",
        "rename-error",
        "budget",
        "populated-budget",
        "overflow",
        "cancel",
        "cancel-write",
    ] {
        let sim = SimOs::new();
        let clock = racer_io::SimClock::new(120);
        let env = racer_io::Env::sim(&sim, clock.ctx(0));
        let mut harness = Harness::new(120, sim, clock, false);
        harness.rng = Random((0..100).find(|seed| Random(*seed).pick(2) == 1).unwrap());
        harness.add(&env, None);
        assert_eq!(harness.nodes[0].workers.len(), 2);
        harness.settle();
        if case == "periodic" {
            harness.clock.advance(Duration::from_secs(6));
            for _ in 0..128 {
                harness.tick();
            }
        }
        let node = &mut harness.nodes[0];
        let state = node.workers[0].app.node.clone();
        let before = read_candidates(&env, &node.config.slab_directory).unwrap();
        let sequence = before.first().map_or(0, |(_, image)| image.sequence);
        assert_eq!(sequence > 0, case == "periodic");
        if case == "overflow" {
            state.periodic_checkpoint.lock().unwrap().last_sequence = u64::MAX;
        }
        if case == "budget" {
            node.workers[0].app.checkpoint_budget = 1;
        }
        if case == "populated-budget" {
            for worker in &mut node.workers {
                worker.app.checkpoint_budget = 32 * 1024;
                let index = worker.app.store.writer.index();
                index.set_page_capacity(1_000_000).unwrap();
                for number in 0u64..128 {
                    let mut key = [0; 32];
                    key[..8].copy_from_slice(&number.to_le_bytes());
                    let version = crate::model::ObjectVersion {
                        object: crate::model::ObjectId {
                            cache: worker.app.caches[0].id.clone(),
                            key: crate::model::CacheKey(key),
                        },
                        etag: crate::model::StrongEtag::parse(b"\"budget\"").unwrap(),
                    };
                    index
                        .publish_version(crate::model::VersionMetadata {
                            version,
                            length: 0,
                            content_type: None,
                        })
                        .unwrap();
                }
                assert!(!index.snapshot_metadata().is_empty());
            }
        }
        let deadline = scope(&env.ctx.clock, Duration::from_secs(30)).unwrap();
        if case == "cancel" {
            deadline.cancel().unwrap();
        }
        match case {
            "write-error" => harness
                .sim
                .inject("write", Fault::Errno(libc::ENOSPC))
                .unwrap(),
            "rename-error" => harness
                .sim
                .inject("rename", Fault::Errno(libc::EIO))
                .unwrap(),
            "empty" | "periodic" | "cancel-write" => {
                harness.sim.inject("write", Fault::Delay(32)).unwrap()
            }
            _ => (),
        }
        harness.sim.take_trace();
        let mut operations: Vec<_> = node
            .workers
            .iter()
            .map(|worker| Some(Box::pin(worker.app.checkpoint(&deadline))))
            .collect();
        let mut results = Vec::new();
        let mut delayed = false;
        let waker = wake::waker();
        let mut cx = Context::from_waker(&waker);
        for turn in 0..256 {
            for worker in &node.workers {
                worker.runtime.reactor.poll_budgeted(64).unwrap();
            }
            for operation in &mut operations {
                if let Some(task) = operation
                    && let Poll::Ready(result) = task.as_mut().poll(&mut cx)
                {
                    results.push(result);
                    *operation = None;
                }
            }
            if turn == 16 && matches!(case, "empty" | "periodic" | "cancel-write") {
                assert!(
                    results.is_empty(),
                    "publication did not yield for delayed I/O"
                );
                for worker in &node.workers {
                    assert!(matches!(
                        futures::executor::block_on(worker.app.store.checkpoint.snapshot_shard()),
                        Err(Error::Overloaded)
                    ));
                }
                delayed = true;
                if case == "cancel-write" {
                    deadline.cancel().unwrap();
                }
            }
            if operations.iter().all(Option::is_none) {
                break;
            }
        }
        assert_eq!(results.len(), node.workers.len(), "{case}");
        let expected = match case {
            "write-error" => Err(Error::Os(libc::ENOSPC)),
            "rename-error" => Err(Error::RenameUncertain(crate::error::PublicationCause::Os(
                libc::EIO,
            ))),
            "budget" | "populated-budget" => Err(Error::Overloaded),
            "overflow" => Err(Error::Unavailable),
            "cancel" | "cancel-write" => Err(Error::Canceled),
            _ => Ok(()),
        };
        assert!(
            results.iter().all(|result| *result == expected),
            "{case}: {results:?}"
        );
        let after = read_candidates(&env, &node.config.slab_directory).unwrap();
        if expected.is_ok() {
            assert!(delayed);
            assert_eq!(after[0].1.sequence, sequence + 1);
            assert_eq!(after[0].1.shards.len(), node.workers.len());
            assert_eq!(after[0].0, before.first().map_or(0, |(slot, _)| slot ^ 1));
            assert!(
                harness
                    .sim
                    .take_trace()
                    .iter()
                    .any(|event| event.operation == "complete:rename")
            );
        } else {
            assert_eq!(after.len(), before.len(), "{case}");
            if case == "populated-budget" {
                assert!(
                    !harness
                        .sim
                        .take_trace()
                        .iter()
                        .any(|event| event.operation == "submit:write"
                            || event.operation == "submit:rename")
                );
            }
        }
        drop(operations);
        for worker in &node.workers {
            futures::executor::block_on(worker.app.store.checkpoint.snapshot_shard()).unwrap();
            worker.app.store.checkpoint.finish_snapshot();
        }
        // The test directly exercised the final cut. Process-loss cleanup avoids
        // asking a failed shutdown cut to run again, while fencing all I/O owners.
        harness.retire(&env, 0, true);
        assert_eq!(harness.sim.live_handles(), 0);
    }
}

/// Replays every seed once (racer-dst double-runs the whole corpus) and, when `required`,
/// asserts the corpus coverage obligations; returns a digest over all seeds.
fn run_corpus(native: bool, seeds: &[u64], steps: usize, required: bool) -> [u8; 32] {
    let mut digest = Sha256::new();
    let mut counts = BTreeMap::new();
    eprintln!(
        "dst corpus native={native} seeds={seeds:?} steps={steps} coverage-required={required}"
    );
    for &seed in seeds {
        let first = replay(seed, steps, native);
        digest.update(first.trace.digest());
        eprintln!(
            "dst replay seed={seed} native={native} events={} digest={:x}",
            first.trace.events,
            first.trace.hash.clone().finalize()
        );
        first.add_to_corpus(&mut counts, native);
    }
    let missing = coverage_misses(&counts);
    eprintln!(
        "dst corpus coverage native={native} required={required} counts={counts:?} missing={missing:?}"
    );
    assert!(
        !required || missing.is_empty(),
        "DST COVERAGE MISS: default corpus native={native} steps={steps} missing={missing:?}; per-run correctness and exact replay passed"
    );
    digest.finalize().into()
}

#[cfg(test)]
fn requires_corpus_coverage(custom_seeds: Option<&str>, steps: usize) -> bool {
    custom_seeds.is_none() && steps >= WEIGHTED_ACTIONS.len()
}

fn coverage_misses(counts: &BTreeMap<String, usize>) -> Vec<&str> {
    counts
        .iter()
        .filter(|(name, count)| {
            let minimum = match name.as_str() {
                "successful-responses" => 8,
                "response-bytes" => PAGE_BYTES as usize + 1,
                _ => 1,
            };
            **count < minimum
        })
        .map(|(name, _)| name.as_str())
        .collect()
}

#[test]
fn dst_coverage_policy_and_aggregation() {
    let cycle = WEIGHTED_ACTIONS.len();
    assert!(requires_corpus_coverage(None, cycle));
    assert!(requires_corpus_coverage(None, cycle + 1));
    assert!(!requires_corpus_coverage(None, cycle - 1));
    for seeds in ["42", "1,7,42"] {
        for steps in [1, cycle, 512] {
            assert!(!requires_corpus_coverage(Some(seeds), steps));
        }
    }

    // Complementary runs satisfy obligations together, without counting the
    // replay twice. Missing evidence remains visible even in report-only mode.
    let first = Coverage {
        success: 4,
        bytes: PAGE_BYTES as usize,
        actions: BTreeMap::from([("multi-worker", 1)]),
        origin_faults: BTreeMap::from([("credential-reject", 1)]),
        native_faults: BTreeMap::from([("Bind:reject".into(), 1)]),
        ..Coverage::default()
    };
    let second = Coverage {
        success: 4,
        bytes: 1,
        secondary_worker_turns: 1,
        origin_faults: BTreeMap::from([("credential-forbidden", 1)]),
        native_faults: BTreeMap::from([("WriteImm:delay".into(), 1)]),
        ..Coverage::default()
    };
    let mut counts = BTreeMap::new();
    first.add_to_corpus(&mut counts, true);
    let missing = coverage_misses(&counts);
    for name in [
        "successful-responses",
        "response-bytes",
        "secondary-worker-data-turns",
        "origin:credential-forbidden",
        "native:WriteImm:delay",
    ] {
        assert!(missing.contains(&name), "{name}");
    }
    second.add_to_corpus(&mut counts, true);
    let missing = coverage_misses(&counts);
    for name in [
        "successful-responses",
        "response-bytes",
        "action:multi-worker",
        "secondary-worker-data-turns",
        "origin:credential-reject",
        "origin:credential-forbidden",
        "native:Bind:reject",
        "native:WriteImm:delay",
    ] {
        assert!(!missing.contains(&name), "{name}");
    }
    assert!(missing.contains(&"native:Invalidate:completion"));
    assert!(missing.contains(&"action:disk-corruption"));
    assert_eq!(counts["action:multi-worker"], 1);
    // Supplying evidence for every remaining obligation clears the diagnostic.
    for count in counts.values_mut() {
        *count = (*count).max(1);
    }
    assert!(coverage_misses(&counts).is_empty());
    assert!(
        first
            .corpus_counts(false)
            .keys()
            .all(|name| !name.starts_with("native"))
    );
}

fn replay(seed: u64, steps: usize, native: bool) -> Coverage {
    let sim = SimOs::new();
    let clock = racer_io::SimClock::new(seed);
    let env = racer_io::Env::sim(&sim, clock.ctx(0));
    let mut harness = Harness::new(seed, sim, clock, native);
    harness
        .coverage
        .trace
        .record(format!("run:{seed}:{steps}:{native}"));
    let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        harness.generated(&env, steps)
    }));
    if let Err(error) = result {
        harness.coverage.collect(&harness.sim);
        harness.coverage.collect_native(&harness.fabric);
        eprintln!(
            "DST CORRECTNESS FAILURE seed={seed} steps={steps} native={native} elapsed={:?} trace={:?}",
            harness.clock.elapsed(),
            harness.coverage.trace
        );
        std::panic::resume_unwind(error);
    }
    harness.coverage
}

fn member(config: &Config) -> crate::topology::Member {
    crate::topology::Member {
        node: config.node.clone(),
        shares: std::num::NonZeroU32::new(1).unwrap(),
        peer_endpoint: config.peer_listen.to_string(),
        rails: if config.enable_rdma {
            vec![racer_wire::control::RailMapping {
                rail: racer_wire::control::RailId(0),
                device: format!(
                    "dst-rnic-{}",
                    usize::from_str_radix(&config.node.0[..8], 16).unwrap()
                ),
                port: 1,
                gid: None,
                numa_node: None,
            }]
        } else {
            vec![]
        },
        site: "site1".into(),
    }
}
#[derive(Clone, Copy)]
enum Lifecycle {
    Start,
    Drain,
    Shutdown,
}
fn lifecycle(workers: &mut [LocalWorker], scope: &RequestScope, phase: Lifecycle) {
    let mut pending: Vec<_> = workers
        .iter_mut()
        .map(|worker| {
            let directory = worker.app.directory.clone();
            let coordinator = worker.app.coordinator.clone();
            let id = worker.app.worker;
            let installed = std::cell::Cell::new(!matches!(phase, Lifecycle::Start));
            let operation = match phase {
                Lifecycle::Start => worker.app.start(scope),
                Lifecycle::Drain => worker.app.drain(scope),
                Lifecycle::Shutdown => worker.app.shutdown(scope),
            };
            let mut operation = operation;
            let operation: Operation<'_, ()> = Box::pin(std::future::poll_fn(move |cx| {
                let _local =
                    directory.simulation_scope(installed.get().then(|| (id, coordinator.clone())));
                let result = operation.as_mut().poll(cx);
                // Installation can happen during a Pending startup poll. The next
                // poll selects this worker without attempting another installation.
                if matches!(result, Poll::Ready(_)) {
                    installed.set(true);
                }
                result
            }));
            (&worker.runtime, &mut worker.crypto, Some(operation))
        })
        .collect();
    for _ in 0..MAX_TURNS {
        for (runtime, crypto, operation) in &mut pending {
            runtime.reactor.poll_budgeted(64).unwrap();
            crypto
                .poll_budgeted(&mut Context::from_waker(&wake::waker()), 64)
                .unwrap();
            runtime.crypto.poll_budgeted(64).unwrap();
            if let Some(op) = operation
                && let Poll::Ready(result) =
                    op.as_mut().poll(&mut Context::from_waker(&wake::waker()))
            {
                result.unwrap();
                *operation = None;
            }
        }
        if pending.iter().all(|(_, _, op)| op.is_none()) {
            return;
        }
    }
    panic!("multi-worker lifecycle stalled");
}
fn drive_local<T>(
    runtime: &WorkerRuntime,
    crypto: &mut dyn crate::lifecycle::Service<RequestScope>,
    mut operation: Operation<'_, T>,
) -> Result<T> {
    for _ in 0..MAX_TURNS {
        runtime.reactor.poll_budgeted(64)?;
        crypto.poll_budgeted(&mut Context::from_waker(&wake::waker()), 64)?;
        runtime.crypto.poll_budgeted(64)?;
        if let Poll::Ready(result) = operation
            .as_mut()
            .poll(&mut Context::from_waker(&wake::waker()))
        {
            return result;
        }
    }
    panic!("local application lifecycle stalled")
}
