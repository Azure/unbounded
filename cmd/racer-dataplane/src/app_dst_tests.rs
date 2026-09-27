//! Generated traffic through the assembled production graph on hostless descriptors.
//! The oracle owns immutable origin versions, never consults placement or cache data.
use super::*;
use crate::{
    control::{caches::CacheDefinition, wire},
    model::{envelope::KeyId, identity::*, limits::ResourceClass, range::PAGE_BYTES},
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
impl ReplayTrace {
    fn record(&mut self, event: String) {
        self.hash.update((event.len() as u64).to_le_bytes());
        self.hash.update(event.as_bytes());
        self.events += 1;
        if self.recent.len() == 32 {
            self.recent.pop_front();
        }
        self.recent.push_back(event);
    }
    fn digest(&self) -> [u8; 32] {
        self.hash.clone().finalize().into()
    }
    fn checkpoint(&mut self) {
        assert!(self.checkpoints.len() < 514, "bounded replay checkpoints");
        self.checkpoints.push((self.events, self.digest()));
    }
}
impl Coverage {
    fn add_to_corpus(&self, counts: &mut BTreeMap<String, usize>, native: bool) {
        for (name, count) in self.corpus_counts(native) {
            *counts.entry(name).or_default() += count;
        }
    }

    // Only path/action evidence is aggregated. The oracle, bounds, fault
    // consumption, and replay assertions are checked before a run contributes.
    fn corpus_counts(&self, native: bool) -> BTreeMap<String, usize> {
        let mut counts = BTreeMap::new();
        for (name, count) in [
            ("successful-responses", self.success),
            ("response-bytes", self.bytes),
            ("origin-gets", self.origin_gets),
            ("persisted-records", self.persisted),
            ("secondary-worker-data-turns", self.secondary_worker_turns),
            ("relay-turns", self.relay_turns),
        ] {
            counts.insert(name.into(), count);
        }
        for action in [
            "multi-worker",
            "key-retirement",
            "cache-retire-recreate",
            "inflight-crash",
            "pending-write-crash",
            "partition-heal",
            "wall-jump",
            "malformed-client",
            "peer-replay",
            "peer-signature-corruption",
            "disk-corruption",
            "verified-disk-hit",
            "verified-memory-hit",
        ] {
            counts.insert(
                format!("action:{action}"),
                self.actions.get(action).copied().unwrap_or(0),
            );
        }
        for operation in ["complete:write", "complete:accept", "blocked:send"] {
            counts.insert(
                format!("os:{operation}"),
                self.operations.get(operation).copied().unwrap_or(0),
            );
        }
        for fault in [
            "credential-reject",
            "credential-forbidden",
            "origin-duplicate-length",
            "origin-truncated-body",
            "origin-malformed-etag",
        ] {
            counts.insert(
                format!("origin:{fault}"),
                self.origin_faults.get(fault).copied().unwrap_or(0),
            );
        }
        if native {
            counts.insert("native-writes".into(), self.native_writes);
            for operation in ["Bind", "Write", "Invalidate"] {
                for fault in ["reject", "delay", "completion"] {
                    let rule = format!("{operation}:{fault}");
                    counts.insert(
                        format!("native:{rule}"),
                        self.native_faults.get(&rule).copied().unwrap_or(0),
                    );
                }
            }
        }
        counts
    }

    fn collect_native(&mut self, fabric: &crate::rdma::lifecycle::simulation::Simulation) {
        for event in fabric.take_trace() {
            self.native_writes += usize::from(
                event.operation == crate::rdma::lifecycle::simulation::Operation::Write
                    && event.completion
                    && event.result == 0,
            );
            self.trace.record(format!(
                "native:{:?}:{}:{:?}:{}:{}",
                event.operation, event.resource, event.work_id, event.result, event.completion
            ));
        }
    }
    fn action(&mut self, name: &'static str) {
        self.trace.record(format!("action:{name}"));
        *self.actions.entry(name).or_default() += 1;
    }
    fn collect(&mut self, sim: &Simulation) {
        for event in sim.take_trace() {
            self.trace.record(format!(
                "os:{}:{}:{}",
                event.operation, event.resource, event.result
            ));
            if let Some(op) = event.operation.strip_prefix("fault:") {
                *self.observed.entry(op.into()).or_default() += 1;
            }
            if (event.operation.starts_with("complete:") && event.result >= 0)
                || event.operation.starts_with("blocked:")
                || event.operation == "disk:crash"
            {
                *self.operations.entry(event.operation).or_default() += 1;
            }
        }
    }
}

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
        crate::control::caches::canonical_socket_paths(&name).unwrap();
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
impl std::ops::Deref for Node {
    type Target = LocalWorker;
    fn deref(&self) -> &LocalWorker {
        &self.workers[0]
    }
}
impl std::ops::DerefMut for Node {
    fn deref_mut(&mut self) -> &mut LocalWorker {
        &mut self.workers[0]
    }
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
        }
    }
    fn update(&mut self, object: usize) {
        let revision = self.revisions.entry(object).or_default();
        *revision += 1;
        let length = match object {
            0 => 0,
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
        config.limits.request_context_bytes = NonZeroUsize::new(4 * 1024 * 1024).unwrap();
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
        let (mut app, runtime, engine) = integration_tests::local_worker(&config, &node, 0);
        let crypto = node.native.crypto(WorkerId(0), engine).unwrap();
        if self.native {
            app.fabric_ports = vec![crate::rdma::device::FabricPort {
                fabric: "dst-fabric".into(),
                device,
                port: 1,
                gid: Some(gid),
            }];
        }
        app.keys.install(self.bundle(&config, 1)).unwrap();
        app.keys
            .install_signing_identity(self.identity(&config, id))
            .unwrap();
        app.telemetry
            .attach_io(runtime.reactor.clone(), runtime.admission.clone())
            .unwrap();
        app.keys
            .register_retirement_barriers(node.retirement.clone())
            .unwrap();
        // The harness supplies accepted control inputs; startup/recovery and every
        // datapath dependency remain the production application implementation.
        let control = app.control.take();
        self.generation += 1;
        let mut publication =
            integration_tests::publication(&config, self.generation, vec![self.definition(id)]);
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
            let (mut app, runtime, engine) =
                integration_tests::local_worker(&config, &node, worker as u16);
            app.fabric_ports = workers[0].app.fabric_ports.clone();
            let crypto = node.native.crypto(WorkerId(worker as u16), engine).unwrap();
            workers.push(LocalWorker {
                app,
                runtime,
                crypto,
            });
        }
        lifecycle(&mut workers, &startup, Lifecycle::Start);
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
        for worker in &workers {
            self.sim
                .disk()
                .sync(
                    &config
                        .slab_directory
                        .join(format!("worker-{}-slab-0.dat", worker.app.worker.0)),
                )
                .unwrap();
        }
        if self.native {
            assert!(
                workers.iter().all(|w| !w.app.actual_rails.is_empty()),
                "native activation must not silently fall back"
            );
        }
        if worker_count > 1 {
            self.coverage.action("multi-worker");
        }
        let LocalWorker {
            app,
            runtime,
            crypto,
        } = &mut workers[0];
        drive_local(
            runtime,
            &mut **crypto,
            app.clients.reconcile(&[self.definition(id)], &startup),
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
            let mut p =
                integration_tests::publication(&node.config, self.generation, vec![definition]);
            p.membership_version = MembershipVersion(self.generation);
            p.members = members.clone();
            node.app.snapshots.publish(p).unwrap();
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
                self.nodes[index]
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
            node.app.directory.simulation_crash();
            for worker in &mut node.workers {
                if let Some(endpoint) = &mut worker.app.endpoint {
                    endpoint.simulation_crash();
                }
                worker.app.drivers.simulation_crash();
            }
            node.app.directory.simulation_crash();
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
        let version = self.catalog.borrow().current[&object].clone();
        let size = version.bytes.len();
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
            format!("If-Match: {}\r\n", version.tag)
        } else {
            String::new()
        };
        let request = format!("{} /v1/objects/{} HTTP/1.1\r\nHost: racer\r\n{pin}{range}Racer-Metadata: dst opaque metadata\r\nAuthorization: Bearer dst-fixture\r\nConnection: close\r\n\r\n", if head { "HEAD" } else { "GET" }, key(object)).into_bytes();
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
            fd: Some(fd),
            request,
            sent: 0,
            response: vec![],
            object,
            tag: version.tag,
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

    fn cache_obligations(&mut self) {
        // Probe disk and memory hits on every run. The origin version is
        // unavailable, so refetching cannot disguise a cache failure. Actual
        // disk-path evidence contributes to default corpus coverage.
        let node = self.rng.pick(self.nodes.len());
        let client = self.request_on(2, false, false, node);
        self.exchange(client, false);
        let version = self.catalog.borrow_mut().current.remove(&2).unwrap();
        let calls = self.catalog.borrow().calls;
        let reads = self
            .coverage
            .operations
            .get("complete:read")
            .copied()
            .unwrap_or(0);
        for node in self.nodes.iter().flat_map(|n| &n.workers) {
            node.app.memory.evict_idle(usize::MAX).unwrap();
        }
        // Construct requests from independent version facts even with origin offline.
        self.catalog.borrow_mut().current.insert(2, version.clone());
        let client = self.request_on(2, true, false, node);
        self.catalog.borrow_mut().current.remove(&2);
        self.exchange(client, false);
        let disk_reads = self
            .coverage
            .operations
            .get("complete:read")
            .copied()
            .unwrap_or(0);
        assert_eq!(
            self.catalog.borrow().calls,
            calls,
            "disk hit reached origin"
        );
        if disk_reads > reads {
            self.coverage.action("verified-disk-hit");
        }
        self.catalog.borrow_mut().current.insert(2, version.clone());
        let client = self.request_on(2, true, false, node);
        self.catalog.borrow_mut().current.remove(&2);
        self.exchange(client, false);
        assert_eq!(
            self.coverage
                .operations
                .get("complete:read")
                .copied()
                .unwrap_or(0),
            disk_reads,
            "memory hit read disk"
        );
        assert_eq!(
            self.catalog.borrow().calls,
            calls,
            "memory hit reached origin"
        );
        self.coverage.action("verified-memory-hit");
        self.catalog.borrow_mut().current.insert(2, version);
    }

    fn peer_outage(&mut self) {
        let index = self.rng.pick(self.nodes.len());
        let address = self.nodes[index].config.peer_listen;
        let listener_scope = self.nodes[index].app.listener_scope.take().unwrap();
        listener_scope.cancel().unwrap();
        self.nodes[index].app.peer_task.take();
        let endpoint = crate::http::pool::Endpoint::Peer(address.to_string());
        for node in &self.nodes {
            node.app.http.invalidate(&endpoint);
        }
        // Fence the listener-owned accept and receive operations before probing.
        for _ in 0..8 {
            self.tick();
        }
        self.coverage.action("peer-outage");
        self.traffic(2, true);
        let node = &mut self.nodes[index];
        let peers = node.app.peers.clone();
        let listener_scope = scope(Duration::from_secs(365 * 24 * 3600)).unwrap();
        let peer_scope = listener_scope.clone();
        node.app.listener_scope = Some(listener_scope);
        node.app.peer_task = Some(Box::pin(
            async move { peers.listen(address, &peer_scope).await },
        ));
        self.tick();
        self.coverage.action("peer-heal");
        self.traffic(1, false);
    }

    fn partition_traffic(&mut self) {
        // Partition a generated cut after normal traffic has established pooled
        // streams. No endpoint invalidation or membership rewrite accompanies it.
        self.traffic(2, false);
        let mut clients: Vec<_> = (0..4)
            .map(|_| {
                let object = self.rng.pick(8);
                self.request(object, false, false)
            })
            .collect();
        for _ in 0..16 {
            self.tick();
            for client in &mut clients {
                if !client.done && client.poll() {
                    self.check(client, false);
                    client.done = true;
                    client.fd.take();
                }
            }
        }
        let cut = 1 + self.rng.pick(self.nodes.len() - 1);
        let mut links = Vec::new();
        for a in &self.nodes[..cut] {
            for b in &self.nodes[cut..] {
                let pair = (
                    SocketAddress::Inet(a.config.peer_listen),
                    SocketAddress::Inet(b.config.peer_listen),
                );
                self.sim.partition(pair.0.clone(), pair.1.clone());
                links.push(pair);
            }
        }
        for _ in 0..100 + self.rng.pick(100) {
            self.tick();
            for client in &mut clients {
                if !client.done && client.poll() {
                    self.check(client, true);
                    client.done = true;
                    client.fd.take();
                }
            }
        }
        for (a, b) in links {
            self.sim.heal(a, b);
        }
        for client in clients {
            if !client.done {
                self.exchange(client, true);
            }
        }
        self.settle();
        self.coverage.action("partition-heal");
    }

    fn crash_inflight(&mut self) {
        let node = self.rng.pick(self.nodes.len());
        self.update(1);
        let mut client = self.request_on(1, false, false, node);
        let mut admitted = false;
        for _ in 0..MAX_TURNS {
            self.tick();
            if client.poll() {
                break;
            }
            if self.nodes[node].workers.iter().any(|w| {
                w.app.drivers.pending() != 0
                    || w.runtime.crypto.outstanding() != 0
                    || w.app.store.writer.slabs().writes_in_flight() != 0
            }) {
                admitted = true;
                break;
            }
        }
        assert!(admitted, "crash must interrupt accepted work");
        let id = self.retire(node, true);
        self.exchange(client, true);
        self.add(Some(id));
        self.coverage.action("inflight-crash");
    }

    fn crash_pending_write(&mut self) {
        self.update(7);
        self.sim.inject("write", Fault::Delay(64));
        *self.coverage.injected.entry("write".into()).or_default() += 1;
        let mut client = self.request(7, false, false);
        let mut completed = false;
        let mut victim = None;
        for _ in 0..MAX_TURNS {
            self.tick();
            if !completed && client.poll() {
                self.check(&client, false);
                completed = true;
                client.fd.take();
            }
            victim = self.nodes.iter().position(|n| {
                n.workers
                    .iter()
                    .any(|w| w.app.store.writer.slabs().writes_in_flight() != 0)
            });
            if victim.is_some() {
                break;
            }
        }
        let victim = victim.expect("generated write never reached OS submission");
        let id = self.retire(victim, true);
        if !completed {
            self.exchange(client, true);
        }
        self.add(Some(id));
        self.coverage.action("pending-write-crash");
    }

    fn key_retirement(&mut self) {
        self.key_epoch = self.key_epoch.checked_add(1).expect("key epoch exhausted");
        assert!(self.key_epoch < 100);
        self.generation += 1;
        let bundles: Vec<_> = self
            .nodes
            .iter()
            .map(|n| self.bundle(&n.config, self.generation + 1000))
            .collect();
        for (node, bundle) in self.nodes.iter_mut().zip(bundles) {
            node.app.keys.install(bundle).unwrap();
            node.app.control = node.control.clone();
        }
        let mut finished = false;
        for _ in 0..MAX_TURNS {
            self.tick();
            if self.nodes.iter().all(|n| {
                n.app.keys.pending_retirements().unwrap().is_empty()
                    && n.workers.iter().all(|w| !w.app.retiring)
            }) {
                finished = true;
                break;
            }
        }
        assert!(finished, "key retirement stalled");
        for node in &mut self.nodes {
            node.app.control = None;
            node.app.control_task.take();
        }
        self.coverage.action("key-retirement");
    }

    fn cache_recreate(&mut self) {
        use crate::control::caches::CacheLifecycle;
        self.generation += 1;
        let members = self.members();
        let mut staged = Vec::new();
        for node in &mut self.nodes {
            node.app.control = node.control.clone();
            staged.push(caches::Adapter {
                node: node.app.node.as_ref().unwrap().clone(),
                listeners: node.app.prepared_listeners.clone(),
                capacity: node.config.limits.metadata_entries.get(),
            });
        }
        let mut committed = vec![false; self.nodes.len()];
        for _ in 0..MAX_TURNS {
            for (index, adapter) in staged.iter().enumerate() {
                if committed[index] {
                    continue;
                }
                match adapter.stage(&[]) {
                    Ok(transition) => {
                        let node = &self.nodes[index];
                        let mut publication =
                            integration_tests::publication(&node.config, self.generation, vec![]);
                        publication.membership_version = MembershipVersion(self.generation);
                        publication.members = members.clone();
                        node.app
                            .snapshots
                            .publish_staged(publication, Some(transition))
                            .unwrap();
                        committed[index] = true;
                    }
                    Err(Error::Unavailable) => (),
                    Err(error) => panic!("cache stage: {error:?}"),
                }
            }
            self.tick();
            if committed.iter().all(|v| *v)
                && self
                    .nodes
                    .iter()
                    .all(|n| n.workers.iter().all(|w| !w.app.retiring))
            {
                break;
            }
        }
        assert!(committed.iter().all(|v| *v));
        self.cache_epoch += 1;
        self.key_retirement();
        self.publish();
        for node in &mut self.nodes {
            let definitions = node.app.snapshots.current().unwrap().caches.clone();
            let LocalWorker {
                app,
                runtime,
                crypto,
            } = &mut node.workers[0];
            drive_local(
                runtime,
                &mut **crypto,
                app.clients
                    .reconcile(&definitions, &scope(Duration::from_secs(30)).unwrap()),
            )
            .unwrap();
        }
        self.coverage.action("cache-retire-recreate");
    }

    fn native_fault(&mut self) {
        use crate::rdma::lifecycle::simulation::{
            Fault as NativeFault, Operation as NativeOperation,
        };
        if self.native_rules.is_empty() {
            for op in [
                NativeOperation::Bind,
                NativeOperation::Write,
                NativeOperation::Invalidate,
            ] {
                for fault in [
                    NativeFault::Reject,
                    NativeFault::Delay(3 + self.rng.pick(8)),
                    NativeFault::Completion(1),
                ] {
                    self.native_rules.push((op, fault));
                }
            }
        }
        let index = self.rng.pick(self.native_rules.len());
        let (operation, fault) = self.native_rules.swap_remove(index);
        self.fabric.fault(operation, fault);
        self.coverage.action("native-fault");
        for _ in 0..32 {
            self.update(5);
            let client = self.request(5, false, false);
            self.exchange(client, true);
            if self.fabric.pending_faults() == 0 {
                *self
                    .coverage
                    .native_faults
                    .entry(format!(
                        "{operation:?}:{}",
                        match fault {
                            NativeFault::Reject => "reject",
                            NativeFault::Delay(_) => "delay",
                            NativeFault::Completion(_) => "completion",
                        }
                    ))
                    .or_default() += 1;
                self.coverage.action("native-fault-observed");
                return;
            }
        }
        panic!("native fault not consumed");
    }

    fn origin_fault(&mut self) {
        if self.origin_faults.is_empty() {
            self.origin_faults.extend([
                OriginFault::Reject,
                OriginFault::Forbidden,
                OriginFault::DuplicateLength,
                OriginFault::Truncate,
                OriginFault::WrongEtag,
            ]);
        }
        let index = self.rng.pick(self.origin_faults.len());
        let fault = self.origin_faults.swap_remove(index);
        self.catalog.borrow_mut().fault = Some(fault);
        self.update(6);
        let client = self.request(6, false, false);
        self.exchange(client, true);
        assert!(
            self.catalog.borrow().fault.is_none(),
            "origin fault not consumed"
        );
        self.coverage.action("origin-fault");
    }

    fn malformed_client(&mut self) {
        let mut client = self.request(2, false, false);
        let (method, fields, status) = match self.rng.pick(3) {
            0 => ("GET", "Host: racer\r\nHost: racer\r\n", 400),
            1 => ("POST", "Host: racer\r\n", 405),
            _ => ("GET", "Host: racer\r\nContent-Length: 1\r\n", 400),
        };
        client.request =
            format!("{method} /v1/objects/{} HTTP/1.1\r\n{fields}\r\n", key(2)).into_bytes();
        client.expected_status = Some(status);
        self.exchange(client, false);
        self.coverage.action("malformed-client");
    }

    fn raw_peer(&mut self, node: usize, request: Vec<u8>) -> Vec<u8> {
        self.coverage.trace.record(format!(
            "peer-request:{}:{:x}",
            self.nodes[node].id,
            Sha256::digest(&request)
        ));
        let fd = self
            .sim
            .connect(SocketAddress::Inet(self.nodes[node].config.peer_listen))
            .unwrap();
        let mut sent = 0;
        let mut response = Vec::new();
        let mut finished = false;
        for _ in 0..MAX_TURNS {
            self.tick();
            if sent < request.len() {
                match handle(&fd).send(&request[sent..]) {
                    Ok(n) => sent += n,
                    Err(e) if would_block(&e) => (),
                    Err(_) => {
                        finished = true;
                        break;
                    }
                }
            }
            let mut bytes = [0; 32768];
            match handle(&fd).recv(&mut bytes) {
                Ok(0) => {
                    finished = true;
                    break;
                }
                Ok(n) => response.extend_from_slice(&bytes[..n]),
                Err(e) if would_block(&e) => (),
                Err(_) => {
                    finished = true;
                    break;
                }
            }
            if response.windows(4).any(|w| w == b"\r\n\r\n") {
                let (_, h, end) = headers(&response);
                if response.len() >= end + h["content-length"].parse::<usize>().unwrap() {
                    finished = true;
                    break;
                }
            }
        }
        drop(fd);
        assert!(
            finished,
            "peer probe timed out without an observed rejection or response"
        );
        self.coverage.trace.record(format!(
            "peer-response:{}:{:x}",
            self.nodes[node].id,
            Sha256::digest(&response)
        ));
        response
    }

    fn peer_security(&mut self) {
        use crate::{
            http::codec::{Codec, MessageHead, StartLine},
            peer::wire::WireCodec,
            security::protocol as p,
        };
        let receiver = self.rng.pick(self.nodes.len());
        let sender = (receiver + 1) % self.nodes.len();
        let keys = self.nodes[sender].app.keys.clone();
        let certificates = Rc::new(Certificates::new(
            self.nodes[sender].config.cluster.clone(),
            keys.clone(),
        ));
        let signatures = Signatures::new(keys, certificates.clone());
        let peer = self.nodes[receiver].config.node.clone();
        let mut head = MessageHead {
            start: StartLine::Request {
                method: "POST".into(),
                target: "/racer/peer/v1/handshake".into(),
            },
            headers: vec![],
        };
        p::push(&mut head, "content-length", 0);
        p::push(&mut head, "racer-kind", "handshake");
        p::push(&mut head, "racer-wire-version", crate::peer::wire::VERSION);
        p::push(&mut head, "racer-membership", self.generation);
        p::push(&mut head, "racer-receiver", &peer.0);
        let signed = signatures.sign(head).unwrap();
        let envelope = crate::security::forwarding::ForwardedHead {
            original: Arc::new(signed),
            hops: vec![],
        };
        let wire = WireCodec::encode(&envelope, false, 0).unwrap();
        let bytes = Codec::new(32768, u64::MAX).encode_head(&wire).unwrap();
        if self.security_faults.is_empty() {
            self.security_faults.extend([true, false]);
        }
        let selected = self.rng.pick(self.security_faults.len());
        if self.security_faults.swap_remove(selected) {
            // A retained signed proof on a new socket has no live session. Both
            // attempts must fail, including after receiver restart.
            let valid = self.raw_peer(receiver, bytes.clone());
            assert!(
                !valid.starts_with(b"HTTP/1.1 200"),
                "sessionless signed proof accepted"
            );
            let replay = self.raw_peer(receiver, bytes);
            assert!(
                !replay.starts_with(b"HTTP/1.1 200"),
                "replayed signed request accepted"
            );
            self.coverage.action("peer-replay");
        } else {
            // WireCodec wraps the signed original in a base64 field. Mutate the
            // signed signature before encoding so the outer HTTP remains legal.
            let original = &envelope.original;
            let mut signed = crate::security::signing::tests::clone_head(original);
            let signature = signed
                .head
                .headers
                .iter_mut()
                .find(|h| h.name.eq_ignore_ascii_case("signature"))
                .unwrap();
            let at = signature.value.iter().position(|b| *b == b':').unwrap() + 1;
            signature.value[at] = if signature.value[at] == b'A' {
                b'B'
            } else {
                b'A'
            };
            signed.signature[0] ^= 1;
            let wire = WireCodec::encode(
                &crate::security::forwarding::ForwardedHead {
                    original: Arc::new(signed),
                    hops: vec![],
                },
                false,
                0,
            )
            .unwrap();
            let bytes = Codec::new(32768, u64::MAX).encode_head(&wire).unwrap();
            let response = self.raw_peer(receiver, bytes);
            assert!(
                !response.starts_with(b"HTTP/1.1 200"),
                "corrupted signature accepted"
            );
            self.coverage.action("peer-signature-corruption");
        }
    }

    fn disk_corruption(&mut self) {
        use crate::runtime::reactor::simulation::DiskState;
        self.settle();
        let entries: Vec<_> = self
            .nodes
            .iter()
            .enumerate()
            .flat_map(|(n, node)| {
                node.workers
                    .iter()
                    .enumerate()
                    .flat_map(move |(w, worker)| {
                        worker
                            .app
                            .store
                            .writer
                            .index()
                            .snapshot()
                            .unwrap()
                            .entries
                            .into_iter()
                            .map(move |entry| (n, w, entry))
                    })
            })
            .collect();
        if entries.is_empty() {
            self.traffic(1, false);
            return;
        }
        let (node, worker, (page, entry)) = &entries[self.rng.pick(entries.len())];
        let object = usize::from_str_radix(&page.version.object.key.to_hex(), 16).unwrap();
        if object == 0
            || object == 1
            || self
                .catalog
                .borrow()
                .current
                .get(&object)
                .is_none_or(|v| v.tag.as_bytes() != page.version.etag.as_bytes())
        {
            self.traffic(1, false);
            return;
        }
        let client = self.request_on(object, true, false, *node);
        let path = self.nodes[*node]
            .config
            .slab_directory
            .join(format!("worker-{worker}-slab-0.dat"));
        self.sim.disk().sync_all().unwrap();
        let offset = entry.location.location.extent.offset();
        let original = self
            .sim
            .disk()
            .read(&path, offset, 1, DiskState::Durable)
            .unwrap();
        self.sim
            .disk()
            .corrupt(&path, offset, &[original[0] ^ 0x80], DiskState::Both)
            .unwrap();
        for node in &self.nodes {
            for worker in &node.workers {
                worker.app.memory.evict_idle(usize::MAX).unwrap();
            }
        }
        let reads = self
            .coverage
            .operations
            .get("complete:read")
            .copied()
            .unwrap_or(0);
        self.exchange(client, true);
        let read = self
            .coverage
            .operations
            .get("complete:read")
            .copied()
            .unwrap_or(0)
            > reads;
        self.sim
            .disk()
            .corrupt(&path, offset, &original, DiskState::Both)
            .unwrap();
        if read {
            self.coverage.action("disk-corruption");
        }
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
                assert!(client.pinned && client.size == 0 && !client.head);
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
        if client.head || client.size == 0 {
            assert_eq!(status, 200);
            assert_eq!(length, client.size);
            assert!(body.is_empty());
            assert!(!headers.contains_key("content-range"));
        } else {
            assert_eq!(status, 206);
            assert_eq!(headers["content-type"], "application/octet-stream");
            assert_eq!(
                headers["content-range"],
                format!("bytes {}-{}/{}", client.first, client.end - 1, client.size)
            );
            assert_eq!(length, client.end - client.first);
            assert!(body.len() <= length);
            assert_eq!(
                body,
                &expected[client.first..client.first + body.len()],
                "oracle byte mismatch"
            );
            if body.len() != length {
                assert!(faulted && client.disconnected, "healthy response truncated");
                self.coverage.failures += 1;
                return;
            }
        }
        self.coverage.success += 1;
        self.coverage.bytes += body.len();
    }

    fn generated(&mut self, steps: usize) {
        for object in 0..8 {
            self.update(object);
        }
        let initial = 2 + self.rng.pick(MAX_NODES - 1);
        for _ in 0..initial {
            self.add(None);
        }
        let mut actions = Vec::new();
        for step in 0..steps {
            if actions.is_empty() {
                actions.extend_from_slice(WEIGHTED_ACTIONS);
            }
            let selected = self.rng.pick(actions.len());
            let action = actions.swap_remove(selected);
            self.coverage.trace.record(format!(
                "step:{step}:{action:?}:{}:{}",
                self.nodes.len(),
                self.rng.0
            ));
            eprintln!(
                "dst seed={} step={step} action={action:?} nodes={}",
                self.seed,
                self.nodes.len()
            );
            match action {
                Action::AddNode if self.nodes.len() < MAX_NODES => self.add(None),
                Action::RemoveNode if self.nodes.len() > 1 => {
                    let index = self.rng.pick(self.nodes.len());
                    self.remove(index);
                }
                Action::Update => {
                    let object = self.rng.pick(8);
                    self.update(object);
                }
                Action::Evict => {
                    self.settle();
                    for worker in self.nodes.iter().flat_map(|n| &n.workers) {
                        worker.app.memory.evict_idle(usize::MAX).unwrap();
                    }
                    self.coverage.action("evict");
                    self.traffic(1, false);
                }
                Action::Restart => {
                    let index = self.rng.pick(self.nodes.len());
                    let id = self.remove(index);
                    self.add(Some(id));
                }
                Action::ShortIo => {
                    let operation = if self.rng.pick(2) == 0 {
                        "send"
                    } else {
                        "recv"
                    };
                    self.sim
                        .inject(operation, Fault::Short(1 + self.rng.pick(128)));
                    *self.coverage.injected.entry(operation.into()).or_default() += 1;
                    self.coverage.action("short-io");
                    self.traffic(1, false);
                }
                Action::ConnectFailure => {
                    self.sim.inject("connect", Fault::Errno(libc::ECONNREFUSED));
                    *self.coverage.injected.entry("connect".into()).or_default() += 1;
                    // A fresh unpinned object requires an origin connection even
                    // when every peer already has a cached copy of the old version.
                    self.update(2);
                    let client = self.request(2, false, false);
                    self.exchange(client, true);
                    self.coverage.action("connect-failure");
                }
                Action::DelayedWrite => {
                    self.sim.inject("write", Fault::Delay(3 + self.rng.pick(8)));
                    *self.coverage.injected.entry("write".into()).or_default() += 1;
                    self.update(3);
                    let client = self.request(3, false, false);
                    self.exchange(client, false);
                    self.coverage.action("delayed-write");
                }
                Action::ClientCancel => {
                    let mut client = self.request(1, false, false);
                    for _ in 0..1 + self.rng.pick(32) {
                        self.tick();
                        if client.poll() {
                            break;
                        }
                    }
                    self.sim.disconnect(client.fd.as_ref().unwrap()).unwrap();
                    client.fd.take();
                    self.settle();
                    self.coverage.action("client-cancel");
                }
                Action::PeerOutage => self.peer_outage(),
                Action::InflightCrash => {
                    self.crash_inflight();
                }
                Action::OldPin => {
                    // Warm the ingress before mutation. This old pin is provably
                    // retained, so unavailability cannot excuse a failed read.
                    let object = 2 + self.rng.pick(6);
                    let node = self.rng.pick(self.nodes.len());
                    let warm = self.request_on(object, false, false, node);
                    self.exchange(warm, false);
                    let client = self.request_on(object, true, false, node);
                    self.update(object);
                    self.exchange(client, false);
                    self.coverage.action("old-pin");
                }
                Action::FailedDirtyWrite => {
                    self.sim.inject("write", Fault::Errno(libc::EIO));
                    *self.coverage.injected.entry("write".into()).or_default() += 1;
                    self.update(4);
                    let client = self.request(4, false, false);
                    self.exchange(client, false);
                    self.coverage.action("failed-dirty-write");
                }
                Action::InflightMembership if self.nodes.len() < MAX_NODES => {
                    let mut client = self.request(1, false, false);
                    self.tick();
                    let complete = client.poll();
                    self.add(None);
                    if complete {
                        self.check(&client, false);
                        client.fd.take();
                        self.settle();
                    } else {
                        self.exchange(client, false);
                    }
                    self.coverage.action("inflight-membership");
                }
                Action::Partition if self.nodes.len() > 1 => self.partition_traffic(),
                Action::WallJump => {
                    let wall = crate::runtime::environment::wall_now();
                    let amount = Duration::from_secs(1 + self.rng.pick(120) as u64);
                    self.clock.set_wall_time(if self.rng.pick(2) == 0 {
                        wall + amount
                    } else {
                        wall - amount
                    });
                    self.traffic(1, true);
                    // Replay admission retains a wall high-water mark. Recovery
                    // advances past it; rolling back again is not a healthy clock.
                    self.clock.advance(Duration::from_secs(121));
                    let (anchor, wall_anchor) = crate::runtime::environment::clock_anchor();
                    self.clock.set_wall_time(
                        wall_anchor + crate::runtime::environment::now().duration_since(anchor),
                    );
                    self.coverage.action("wall-jump");
                }
                Action::OriginFault => self.origin_fault(),
                Action::MalformedClient => self.malformed_client(),
                Action::KeyRetirement => self.key_retirement(),
                Action::CacheRecreate => self.cache_recreate(),
                Action::NativeFault if self.native => self.native_fault(),
                Action::PeerSecurity if self.nodes.len() > 1 => self.peer_security(),
                Action::DiskCorruption => self.disk_corruption(),
                Action::PendingWriteCrash => self.crash_pending_write(),
                // Gated actions keep their slot and use ordinary traffic instead
                // of resampling, preserving both weights and random draws.
                Action::Traffic
                | Action::AddNode
                | Action::RemoveNode
                | Action::InflightMembership
                | Action::Partition
                | Action::NativeFault
                | Action::PeerSecurity => {
                    let count = 1 + self.rng.pick(4);
                    self.coverage.action("traffic");
                    self.traffic(count, false);
                }
            }
            self.coverage.trace.checkpoint();
        }
        // Recovery liveness is a mandatory oracle obligation, independent of the
        // generator's action mix: every current object must still be readable.
        for object in 0..8 {
            let client = self.request(object, false, false);
            self.exchange(client, false);
        }
        for node in &self.nodes {
            for worker in &node.workers {
                let snapshot = worker.app.store.writer.index().snapshot().unwrap();
                assert!(snapshot.entries.len() <= node.config.limits.metadata_entries.get());
                self.coverage.persisted += snapshot.entries.len();
            }
        }
        self.cache_obligations();
        self.coverage.origin_gets = self.catalog.borrow().gets;
        self.coverage.origin_faults = self.catalog.borrow().faults.clone();
        for (operation, injected) in &self.coverage.injected {
            assert_eq!(
                self.coverage.observed.get(operation),
                Some(injected),
                "injected OS fault was not consumed"
            );
        }
        while !self.nodes.is_empty() {
            self.remove(0);
        }
        self.coverage.collect(&self.sim);
        self.coverage.collect_native(&self.fabric);
        assert_eq!(
            self.sim.live_handles(),
            0,
            "all descriptors must be fenced and released"
        );
        assert_eq!(
            self.fabric.live_resources(),
            0,
            "native resources must be fenced and released"
        );
        self.coverage.trace.record(format!(
            "final-invariants:{}:{}:{}:{}:{}:{}",
            self.sim.live_handles(),
            self.fabric.live_resources(),
            self.coverage.success,
            self.coverage.failures,
            self.coverage.bytes,
            self.coverage.persisted
        ));
        self.coverage.trace.checkpoint();
        eprintln!("dst seed={} coverage={:?}", self.seed, self.coverage);
    }
}

struct Client {
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
impl Client {
    fn poll(&mut self) -> bool {
        let fd = handle(self.fd.as_ref().unwrap());
        if self.sent < self.request.len() {
            match fd.send(&self.request[self.sent..]) {
                Ok(n) => self.sent += n,
                Err(e) if would_block(&e) => return false,
                Err(_) => {
                    self.disconnected = true;
                    return true;
                }
            }
        }
        let mut bytes = [0; 65536];
        match fd.recv(&mut bytes) {
            Ok(0) => {
                self.disconnected = true;
                return true;
            }
            Ok(n) => self.response.extend_from_slice(&bytes[..n]),
            Err(e) if would_block(&e) => return false,
            Err(_) => {
                self.disconnected = true;
                return true;
            }
        }
        assert!(
            self.response.len() <= 2 * PAGE_BYTES as usize + 32768,
            "unbounded client response"
        );
        if self.response.windows(4).any(|w| w == b"\r\n\r\n") {
            let (_, h, end) = headers(&self.response);
            let length: usize = h["content-length"].parse().unwrap();
            self.response.len() >= end + if self.head { 0 } else { length }
        } else {
            false
        }
    }
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
fn dst_generated_native_traffic_churn_oracle() {
    run_corpus(true);
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
