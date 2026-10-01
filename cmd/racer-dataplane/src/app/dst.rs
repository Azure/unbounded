//! Generated traffic through the assembled production graph on hostless descriptors.
//! The oracle owns immutable origin versions, never consults placement or cache data.
use super::*;
use crate::{
    control::{state::CacheDefinition, wire},
    model::{KeyId, PAGE_BYTES, ResourceClass, *},
    runtime::{
        environment::SimulationClock,
        reactor::{
            Descriptor, SocketAddress,
            simulation::{Fault, Simulation},
        },
    },
    security::identity::{PendingIdentity, SigningIdentity},
};
use ed25519_dalek::pkcs8::EncodePrivateKey;
use sha2::{Digest, Sha256};
use std::{cell::RefCell, collections::BTreeMap};

const MAX_NODES: usize = 32;
const MAX_TURNS: usize = 100_000;

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

fn handle(fd: &Descriptor) -> &crate::runtime::reactor::simulation::Handle {
    match fd {
        Descriptor::Sim(h) => h,
        _ => panic!("DST escaped into host I/O"),
    }
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
        crate::control::state::canonical_socket_paths(&name).unwrap();
    CacheDefinition {
        name,
        id: CacheId("33333333-3333-4333-8333-333333333333".into()),
        client_socket,
        origin_socket,
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
    fn new(sim: &Simulation, id: usize) -> Self {
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
    control: Option<Rc<ControlClient>>,
}
struct LocalWorker {
    app: WorkerApplication,
    runtime: WorkerRuntime,
    crypto: Box<dyn CryptoService>,
}
impl Node {
    fn poll(&mut self, budget: usize) {
        for worker in &mut self.workers {
            if worker.app.control.is_some() {
                worker.app.control_task = Some(Box::pin(std::future::pending()));
            }
            let _local = worker
                .app
                .directory
                .simulation_scope(Some((worker.app.worker, worker.app.coordinator.clone())));
            worker.runtime.reactor.poll_budgeted(budget).unwrap();
            worker.crypto.poll_budgeted(budget).unwrap();
            worker.runtime.crypto.poll_budgeted(budget).unwrap();
            worker
                .app
                .poll_budgeted(
                    &mut Context::from_waker(futures::task::noop_waker_ref()),
                    budget,
                )
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
    }
}

struct Harness {
    seed: u64,
    rng: Random,
    sim: Simulation,
    nodes: Vec<Node>,
    next_node: usize,
    generation: u64,
    catalog: Rc<RefCell<Catalog>>,
    oracle: BTreeMap<(usize, String), Vec<u8>>,
    revisions: BTreeMap<usize, u64>,
    coverage: Coverage,
    ca: rcgen::Certificate,
    ca_key: rcgen::KeyPair,
    clock: SimulationClock,
    fabric: crate::rdma::lifecycle::simulation::Simulation,
    native: bool,
    key_epoch: u8,
    cache_epoch: u64,
    origin_faults: Vec<OriginFault>,
    security_faults: Vec<bool>,
    native_rules: Vec<(
        crate::rdma::lifecycle::simulation::Operation,
        crate::rdma::lifecycle::simulation::Fault,
    )>,
    payload_regression: bool,
    opaque_relay: bool,
    concurrent_layers: bool,
}
impl Harness {
    fn new(seed: u64, sim: Simulation, clock: SimulationClock, native: bool) -> Self {
        let key = ed25519_dalek::SigningKey::from_bytes(&[91; 32])
            .to_pkcs8_der()
            .unwrap();
        let ca_key = rcgen::KeyPair::from_pkcs8_der_and_sign_algo(
            &rustls::pki_types::PrivatePkcs8KeyDer::from(key.as_bytes()),
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
            fabric: crate::rdma::lifecycle::simulation::Simulation::new(),
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
        let key = ed25519_dalek::SigningKey::from_bytes(&seed)
            .to_pkcs8_der()
            .unwrap();
        let pending = PendingIdentity::recover(key.as_bytes()).unwrap();
        let key = rcgen::KeyPair::from_pkcs8_der_and_sign_algo(
            &rustls::pki_types::PrivatePkcs8KeyDer::from(key.as_bytes()),
            &rcgen::PKCS_ED25519,
        )
        .unwrap();
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        params.subject_alt_names = vec![rcgen::SanType::URI(
            format!("spiffe://{}/node/{}", config.cluster.0, config.node.0)
                .try_into()
                .unwrap(),
        )];
        params.key_usages = vec![rcgen::KeyUsagePurpose::DigitalSignature];
        params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ClientAuth];
        let cert = params.signed_by(&key, &self.ca, &self.ca_key).unwrap();
        Arc::new(
            pending
                .accept(
                    config.cluster.clone(),
                    config.node.clone(),
                    vec![cert.der().to_vec()],
                    &[self.ca.der().to_vec()],
                )
                .unwrap(),
        )
    }
    fn add(&mut self, restart: Option<usize>) {
        let id = restart.unwrap_or_else(|| {
            let id = self.next_node;
            self.next_node += 1;
            id
        });
        // Restart is a new process incarnation, not a rewind of nonce entropy.
        let role = self
            .clock
            .environment(1 + id as u64 + (self.generation << 32));
        let _role = role.enter();
        let device = format!("dst-rnic-{id}");
        let gid = [1 + (id % 254) as u8; 16];
        let fabric = self
            .fabric
            .with_devices(vec![crate::rdma::lifecycle::simulation::Device::new(
                device.clone(),
                gid,
            )])
            .unwrap();
        let _fabric = fabric.enter();
        let mut config = crate::test_support::cluster::config(self.native);
        config.opaque_relay = self.opaque_relay;
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
        config.limits.retained_snapshots = NonZeroUsize::new(64).unwrap();
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
        let node = Arc::new(NodeState::new(worker_ids.clone(), 64).unwrap());
        if self.native {
            node.native
                .prepare(worker_ids.into_iter(), &config.limits)
                .unwrap();
        }
        let fabric_ports = if self.native {
            vec![crate::rdma::FabricPort {
                fabric: "dst-fabric".into(),
                device,
                port: 1,
                gid: Some(gid),
            }]
        } else {
            Vec::new()
        };
        let (mut app, runtime, engine) =
            test_support::local_worker_with_fabric(&config, &node, 0, fabric_ports);
        let crypto = node.native.crypto(WorkerId(0), engine).unwrap();
        app.keys.install(self.bundle(&config, 1)).unwrap();
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
        let mut publication =
            test_support::publication(&config, self.generation, vec![self.definition(id)]);
        publication.membership_version = MembershipVersion(self.generation);
        publication.members = self.members();
        publication.members.push(member(&config));
        app.snapshots.publish(publication).unwrap();
        let startup = scope(Duration::from_secs(30)).unwrap();
        let mut workers = vec![LocalWorker {
            app,
            runtime,
            crypto,
        }];
        for worker in 1..worker_count {
            let role = self
                .clock
                .environment(1 + id as u64 + (self.generation << 32) + ((worker as u64) << 48));
            let _role = role.enter();
            let (app, runtime, engine) = test_support::local_worker_with_fabric(
                &config,
                &node,
                worker as u16,
                workers[0].app.fabric_ports.clone(),
            );
            let crypto = node.native.crypto(WorkerId(worker as u16), engine).unwrap();
            workers.push(LocalWorker {
                app,
                runtime,
                crypto,
            });
        }
        lifecycle(&mut workers, &startup, Lifecycle::Start);
        self.persist_provisioning(id, &config, &workers);
        if self.native {
            assert!(
                workers.iter().all(|w| !w.app.actual_rails.is_empty()),
                "native activation must not silently fall back"
            );
        }
        if worker_count > 1 {
            self.coverage.action("multi-worker");
        }
        self.start_node_listeners(id, &config, &mut workers[0], &startup);
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
        let listener_scope = scope(Duration::from_secs(365 * 24 * 3600)).unwrap();
        let peer_scope = listener_scope.clone();
        app.listener_scope = Some(listener_scope);
        app.peer_task = Some(Box::pin(
            async move { peers.listen(address, &peer_scope).await },
        ));
    }
    fn members(&self) -> Vec<crate::topology::membership::Member> {
        self.nodes.iter().map(|n| member(&n.config)).collect()
    }
    fn definition(&self, id: usize) -> CacheDefinition {
        let mut definition = cache(id);
        definition.id = CacheId(format!("33333333-3333-4333-8333-{:012x}", self.cache_epoch));
        definition
    }
    fn bundle(&self, config: &Config, generation: u64) -> wire::KeyringBundle {
        wire::KeyringBundle {
            schema_version: 1,
            cluster: config.cluster.clone(),
            generation: wire::BundleGeneration(generation),
            peer_trust_roots: vec![self.ca.der().to_vec()],
            cache_keys: [
                wire::CacheKeyPurpose::Page,
                wire::CacheKeyPurpose::OriginCredentials,
            ]
            .into_iter()
            .enumerate()
            .map(|(i, purpose)| wire::CacheEncryptionKey {
                key: wire::CacheKeyRef {
                    cache: self.definition(0).id,
                    id: KeyId([7 + i as u8 + self.key_epoch * 2; 16]),
                    purpose,
                },
                state: wire::CacheKeyState::Active,
                material: [19 + i as u8 + self.key_epoch * 2; 32],
            })
            .collect(),
        }
    }
    fn publish(&mut self) {
        self.generation += 1;
        let members = self.members();
        let definitions: Vec<_> = self.nodes.iter().map(|n| self.definition(n.id)).collect();
        for (node, definition) in self.nodes.iter_mut().zip(definitions) {
            let mut p = test_support::publication(&node.config, self.generation, vec![definition]);
            p.membership_version = MembershipVersion(self.generation);
            p.members = members.clone();
            node.workers[0].app.snapshots.publish(p).unwrap();
        }
    }
    fn tick(&mut self) {
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
            self.nodes[index].poll(budget);
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
    }
    fn remove(&mut self, index: usize) -> usize {
        self.retire(index, false)
    }
    fn retire(&mut self, index: usize, crash: bool) -> usize {
        let mut node = self.nodes.remove(index);
        self.coverage
            .trace
            .record(format!("retire:{}:{crash}", node.id));
        let shutdown = scope(Duration::from_secs(30)).unwrap();
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
                        .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
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
        for _ in 0..MAX_TURNS {
            self.tick();
            if self.nodes.iter().flat_map(|n| &n.workers).all(|n| {
                n.app.store.writer.is_idle()
                    && n.app.writer_task.is_none()
                    && n.app.clients.active_connections() == 0
                    && n.runtime.crypto.outstanding() == 0
                    && n.app.drivers.pending() == 0
            }) {
                return;
            }
        }
        panic!("seed={} failed to settle: {:?}", self.seed, self.coverage);
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
        let endpoint = if head { "v2" } else { "v1" };
        let request = format!("{} /{endpoint}/objects/{} HTTP/1.1\r\nHost: racer\r\n{pin}{range}Racer-Metadata: dst opaque metadata\r\nAuthorization: Bearer dst-fixture\r\nConnection: close\r\n\r\n", if head { "HEAD" } else { "GET" }, key(object)).into_bytes();
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
            .unwrap()
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

fn setting(name: &str, default: usize, max: usize) -> usize {
    let value = std::env::var(name).map_or(default, |v| {
        v.parse().expect("positive integer DST setting")
    });
    assert!((1..=max).contains(&value), "{name} must be in 1..={max}");
    value
}

#[test]
fn dst_generated_traffic_churn_oracle() {
    run_corpus(false);
}

#[test]
fn phase5_default_grace_staggered_nodes_and_periodic_checkpoint_traffic() {
    let sim = Simulation::new();
    let _os = sim.enter();
    let clock = SimulationClock::new(505);
    let environment = clock.environment(0);
    let _time = environment.enter();
    let mut harness = Harness::new(505, sim, clock, false);
    for object in 0..8 {
        harness.update(object);
    }
    for _ in 0..4 {
        harness.add(None);
    }
    // Replace only the store's admission policy, retaining each real node's
    // registry and all production peer/read dependencies.
    for node in &mut harness.nodes {
        let published = node.workers[0].app.node.publications.clone();
        node.workers[0].app.snapshots = Rc::new(SnapshotStore::new(
            node.config.cluster.clone(),
            published,
            2,
        ));
    }
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
        let mut p = test_support::publication(
            &node.config,
            harness.generation,
            vec![harness.definition(node.id)],
        );
        p.membership_version = MembershipVersion(harness.generation);
        p.members = members.clone();
        node.workers[0].app.snapshots.publish(p).unwrap();
        assert!(
            node.workers[0]
                .app
                .node
                .publications
                .membership(old)
                .is_ok()
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

#[test]
fn dst_generated_native_traffic_churn_oracle() {
    run_corpus(true);
}

#[test]
fn dst_origin_recovery_completes_before_concurrent_healthy_reads() {
    for native in [false, true] {
        let sim = Simulation::new();
        let _os = sim.enter();
        let clock = SimulationClock::new(106);
        let environment = clock.environment(0);
        let _time = environment.enter();
        let _strict = crate::runtime::environment::require_simulated();
        let mut harness = Harness::new(106, sim, clock, native);
        for object in 0..8 {
            harness.update(object);
        }
        for _ in 0..3 {
            harness.add(None);
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
            harness.remove(0);
        }
        assert_eq!(harness.sim.live_handles(), 0);
        assert_eq!(harness.fabric.live_resources(), 0);
    }
}

#[test]
fn dst_native_faults_reach_warmed_peers_in_small_topologies() {
    for nodes in [1, 2, 3] {
        let sim = Simulation::new();
        let _os = sim.enter();
        let clock = SimulationClock::new(118);
        let environment = clock.environment(0);
        let _time = environment.enter();
        let _strict = crate::runtime::environment::require_simulated();
        let mut harness = Harness::new(118, sim, clock, true);
        for _ in 0..nodes {
            harness.add(None);
        }
        for _ in 0..9 {
            harness.native_fault();
            assert_eq!(harness.fabric.pending_faults(), 0);
        }
        // One complete rule bag covers Bind/Write/Invalidate x all three faults.
        assert!(harness.native_rules.is_empty());
        assert_eq!(harness.coverage.native_faults.len(), 9);
        assert!(harness.coverage.native_faults.values().all(|&n| n == 1));
        assert!(harness.coverage.native_writes > 0);
        while !harness.nodes.is_empty() {
            harness.remove(0);
        }
        assert_eq!(harness.sim.live_handles(), 0);
        assert_eq!(harness.fabric.live_resources(), 0);
    }
}

fn run_corpus(native: bool) {
    for (name, _) in std::env::vars().filter(|(name, _)| name.starts_with("RACER_DST_")) {
        assert!(
            matches!(
                name.as_str(),
                "RACER_DST_SEEDS" | "RACER_DST_STEPS" | "RACER_DST_FILTER"
            ),
            "unsupported DST parameter {name}"
        );
    }
    let custom_seeds = std::env::var("RACER_DST_SEEDS").ok();
    let seeds: Vec<u64> = custom_seeds
        .as_deref()
        .unwrap_or("1,7,42")
        .split(',')
        .map(|s| s.trim().parse().expect("comma-separated u64 DST seeds"))
        .collect();
    assert!(!seeds.is_empty() && seeds.len() <= 1024);
    let steps = setting("RACER_DST_STEPS", WEIGHTED_ACTIONS.len(), 512);
    let required = requires_corpus_coverage(custom_seeds.as_deref(), steps);
    let mut counts = BTreeMap::new();
    eprintln!(
        "dst corpus native={native} seeds={seeds:?} steps={steps} coverage-required={required}"
    );
    for seed in seeds {
        let first = replay(seed, steps, native);
        let second = replay(seed, steps, native);
        let mismatch = first
            .trace
            .checkpoints
            .iter()
            .zip(&second.trace.checkpoints)
            .position(|(a, b)| a != b);
        assert_eq!(
            first, second,
            "DST CORRECTNESS FAILURE: whole-app replay diverged seed={seed} steps={steps} native={native} first checkpoint={mismatch:?}"
        );
        eprintln!(
            "dst replay verified seed={seed} native={native} events={} digest={:x}",
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
}

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
        native_faults: BTreeMap::from([("Write:delay".into(), 1)]),
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
        "native:Write:delay",
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
        "native:Write:delay",
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
    let sim = Simulation::new();
    let _os = sim.enter();
    let clock = SimulationClock::new(seed);
    let environment = clock.environment(0);
    let _time = environment.enter();
    let _strict = crate::runtime::environment::require_simulated();
    let mut harness = Harness::new(seed, sim, clock, native);
    harness
        .coverage
        .trace
        .record(format!("run:{seed}:{steps}:{native}"));
    let result =
        std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| harness.generated(steps)));
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

fn member(config: &Config) -> crate::topology::membership::Member {
    crate::topology::membership::Member {
        node: config.node.clone(),
        shares: std::num::NonZeroU32::new(1).unwrap(),
        peer_endpoint: config.peer_listen.to_string(),
        rails: if config.enable_rdma {
            vec![crate::topology::rails::RailMapping {
                rail: crate::topology::rails::RailId(0),
                fabric: "dst-fabric".into(),
                numa_node: None,
            }]
        } else {
            vec![]
        },
        alignment_enabled: config.enable_rdma,
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
            crypto.poll_budgeted(64).unwrap();
            runtime.crypto.poll_budgeted(64).unwrap();
            if let Some(op) = operation {
                if let Poll::Ready(result) = op
                    .as_mut()
                    .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
                {
                    result.unwrap();
                    *operation = None;
                }
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
    crypto: &mut dyn CryptoService,
    mut operation: Operation<'_, T>,
) -> Result<T> {
    for _ in 0..MAX_TURNS {
        runtime.reactor.poll_budgeted(64)?;
        crypto.poll_budgeted(64)?;
        runtime.crypto.poll_budgeted(64)?;
        if let Poll::Ready(result) = operation
            .as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
        {
            return result;
        }
    }
    panic!("local application lifecycle stalled")
}
