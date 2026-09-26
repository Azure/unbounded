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
use std::{cell::RefCell, collections::BTreeMap};

const MAX_NODES: usize = 32;
const MAX_TURNS: usize = 100_000;
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
}
#[derive(Default, Debug)]
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
}
impl Coverage {
    fn action(&mut self, name: &'static str) {
        *self.actions.entry(name).or_default() += 1;
    }
    fn collect(&mut self, sim: &Simulation) {
        for event in sim.take_trace() {
            if let Some(op) = event.operation.strip_prefix("fault:") {
                *self.observed.entry(op.into()).or_default() += 1;
            }
            if event.operation.starts_with("complete:") && event.result >= 0 {
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
        socket_mode: 0o600,
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
    app: WorkerApplication,
    runtime: WorkerRuntime,
    crypto: Box<dyn CryptoService>,
    adapter: Adapter,
}
impl Node {
    fn poll(&mut self, budget: usize) {
        self.runtime.reactor.poll_budgeted(budget).unwrap();
        self.crypto.poll_budgeted(budget).unwrap();
        self.runtime.crypto.poll_budgeted(budget).unwrap();
        self.app
            .poll_budgeted(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                budget,
            )
            .unwrap();
        for class in CLASSES {
            assert!(
                self.runtime.admission.used(class) <= self.runtime.admission.limit(class),
                "node {} resource bound",
                self.id
            );
        }
        assert!(self.app.store.writer.pending_count() <= self.config.limits.queue_entries.get());
        assert!(
            self.app.clients.active_connections() <= self.config.limits.client_connections.get()
        );
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
        config.slab_directory = format!("/dst/node-{id}/slabs").into();
        config.slab_bytes = 256 * 1024 * 1024;
        config.free_segment_reserve = 1;
        config.limits.queue_entries = NonZeroUsize::new(64).unwrap();
        config.limits.client_connections = NonZeroUsize::new(64).unwrap();
        config.limits.connections_per_neighbor = NonZeroUsize::new(2).unwrap();
        config.limits.range_window_pages = NonZeroUsize::new(2).unwrap();
        config.limits.replay_entries = NonZeroUsize::new(8192).unwrap();
        config.limits.route_search_work = NonZeroUsize::new(4096).unwrap();
        config.limits.metadata_entries = NonZeroUsize::new(128).unwrap();
        config.limits.retained_snapshots = NonZeroUsize::new(64).unwrap();
        config.limits.request_context_bytes = NonZeroUsize::new(4 * 1024 * 1024).unwrap();
        let node = Arc::new(NodeState::new(vec![WorkerId(0)], 64).unwrap());
        if self.native {
            node.native
                .prepare([WorkerId(0)].into_iter(), &config.limits)
                .unwrap();
        }
        let (mut app, runtime, engine) = integration_tests::local_worker(&config, &node, 0);
        let mut crypto = node.native.crypto(WorkerId(0), engine).unwrap();
        if self.native {
            app.fabric_ports = vec![crate::rdma::device::FabricPort {
                fabric: "dst-fabric".into(),
                device,
                port: 1,
                gid: Some(gid),
            }];
        }
        app.keys
            .install(wire::KeyringBundle {
                schema_version: 1,
                cluster: config.cluster.clone(),
                generation: wire::BundleGeneration(1),
                peer_trust_roots: vec![self.ca.der().to_vec()],
                cache_keys: [
                    wire::CacheKeyPurpose::Page,
                    wire::CacheKeyPurpose::OriginCredentials,
                ]
                .into_iter()
                .enumerate()
                .map(|(i, purpose)| wire::CacheEncryptionKey {
                    key: wire::CacheKeyRef {
                        cache: cache(id).id,
                        id: KeyId([7 + i as u8; 16]),
                        purpose,
                    },
                    state: wire::CacheKeyState::Active,
                    material: [19 + i as u8; 32],
                })
                .collect(),
            })
            .unwrap();
        app.keys
            .install_signing_identity(self.identity(&config, id))
            .unwrap();
        // The harness supplies accepted control inputs; startup/recovery and every
        // datapath dependency remain the production application implementation.
        app.control = None;
        self.generation += 1;
        let mut publication =
            integration_tests::publication(&config, self.generation, vec![cache(id)]);
        publication.membership_version = MembershipVersion(self.generation);
        publication.members = self.members();
        publication.members.push(member(&config));
        app.snapshots.publish(publication).unwrap();
        let startup = scope(Duration::from_secs(30)).unwrap();
        drive_local(&runtime, &mut *crypto, app.start(&startup)).unwrap();
        if self.native {
            assert!(
                !app.actual_rails.is_empty(),
                "native activation must not silently fall back"
            );
        }
        drive_local(
            &runtime,
            &mut *crypto,
            app.clients.reconcile(&[cache(id)], &startup),
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
            app,
            runtime,
            crypto,
            adapter,
        });
        self.publish();
        self.tick();
        self.coverage
            .action(if restart.is_some() { "restart" } else { "add" });
    }
    fn members(&self) -> Vec<crate::topology::membership::Member> {
        self.nodes.iter().map(|n| member(&n.config)).collect()
    }
    fn publish(&mut self) {
        self.generation += 1;
        let members = self.members();
        for node in &mut self.nodes {
            let mut p =
                integration_tests::publication(&node.config, self.generation, vec![cache(node.id)]);
            p.membership_version = MembershipVersion(self.generation);
            p.members = members.clone();
            node.app.snapshots.publish(p).unwrap();
        }
    }
    fn tick(&mut self) {
        self.clock.advance(Duration::from_micros(10));
        let start = self.rng.pick(self.nodes.len());
        let budget = 1 + self.rng.pick(64);
        for offset in 0..self.nodes.len() {
            let index = (start + offset) % self.nodes.len();
            self.nodes[index].adapter.poll(&self.catalog);
            self.nodes[index].poll(budget);
            self.coverage.relay_turns += usize::from(
                self.nodes[index]
                    .runtime
                    .admission
                    .used(ResourceClass::Relay)
                    > 0,
            );
        }
        self.coverage.collect(&self.sim);
        assert!(
            self.sim.live_handles() <= self.nodes.len() * 256 + 32,
            "descriptor bound"
        );
    }
    fn remove(&mut self, index: usize) -> usize {
        self.retire(index, false)
    }
    fn retire(&mut self, index: usize, crash: bool) -> usize {
        let mut node = self.nodes.remove(index);
        let shutdown = scope(Duration::from_secs(30)).unwrap();
        if crash {
            // Process loss discards memory and omits the graceful checkpoint cut.
            // The OS completion fence still runs before Rust backing is destroyed.
            assert_eq!(node.app.drivers.pending(), 0);
            node.app.stop_admission().unwrap();
            node.app.peer_task.take();
        } else {
            drive_local(&node.runtime, &mut *node.crypto, node.app.drain(&shutdown)).unwrap();
        }
        drive_local(
            &node.runtime,
            &mut *node.crypto,
            node.app.shutdown(&shutdown),
        )
        .unwrap();
        let id = node.id;
        let Node {
            app,
            runtime,
            mut crypto,
            adapter,
            ..
        } = node;
        drop((app, adapter));
        drive_local(&runtime, &mut *crypto, runtime.reactor.drain()).unwrap();
        assert_eq!(runtime.crypto.outstanding(), 0);
        let admission = runtime.admission.clone();
        drop((runtime, crypto));
        for class in CLASSES {
            assert_eq!(
                admission.used(class),
                0,
                "retired node {id}, class {class:?}"
            );
        }
        self.publish();
        self.coverage.action(if crash { "crash" } else { "remove" });
        id
    }
    fn settle(&mut self) {
        for _ in 0..MAX_TURNS {
            self.tick();
            if self.nodes.iter().all(|n| {
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
        // Every run proves a completed disk hit, then a memory hit. The origin
        // version is unavailable, so refetching cannot disguise a cache failure.
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
        for node in &self.nodes {
            node.app.memory.evict_idle(usize::MAX).unwrap();
        }
        // Construct requests from independent version facts even with origin offline.
        self.catalog.borrow_mut().current.insert(2, version.clone());
        let client = self.request_on(2, true, false, node);
        self.catalog.borrow_mut().current.remove(&2);
        self.exchange(client, false);
        assert!(
            self.coverage
                .operations
                .get("complete:read")
                .copied()
                .unwrap_or(0)
                > reads,
            "cache-only request did not read encrypted storage"
        );
        assert_eq!(
            self.catalog.borrow().calls,
            calls,
            "disk hit reached origin"
        );
        self.coverage.action("verified-disk-hit");
        let reads = self.coverage.operations["complete:read"];
        self.catalog.borrow_mut().current.insert(2, version.clone());
        let client = self.request_on(2, true, false, node);
        self.catalog.borrow_mut().current.remove(&2);
        self.exchange(client, false);
        assert_eq!(
            self.coverage.operations["complete:read"], reads,
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

    fn check(&mut self, client: &Client, faulted: bool) {
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
                    faulted && matches!(status, 502 | 503),
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
        for step in 0..steps {
            let action = self.rng.pick(18);
            eprintln!(
                "dst seed={} step={step} action={action} nodes={}",
                self.seed,
                self.nodes.len()
            );
            match action {
                0 if self.nodes.len() < MAX_NODES => self.add(None),
                1 if self.nodes.len() > 1 => {
                    let index = self.rng.pick(self.nodes.len());
                    self.remove(index);
                }
                2 => {
                    let object = self.rng.pick(8);
                    self.update(object);
                }
                3 => {
                    self.settle();
                    for node in &self.nodes {
                        node.app.memory.evict_idle(usize::MAX).unwrap();
                    }
                    self.coverage.action("evict");
                    self.traffic(1, false);
                }
                4 => {
                    let index = self.rng.pick(self.nodes.len());
                    let id = self.remove(index);
                    self.add(Some(id));
                }
                5 => {
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
                6 => {
                    self.sim.inject("connect", Fault::Errno(libc::ECONNREFUSED));
                    *self.coverage.injected.entry("connect".into()).or_default() += 1;
                    // A fresh unpinned object requires an origin connection even
                    // when every peer already has a cached copy of the old version.
                    self.update(2);
                    let client = self.request(2, false, false);
                    self.exchange(client, true);
                    self.coverage.action("connect-failure");
                }
                7 => {
                    self.sim.inject("write", Fault::Delay(3 + self.rng.pick(8)));
                    *self.coverage.injected.entry("write".into()).or_default() += 1;
                    self.update(3);
                    let client = self.request(3, false, false);
                    self.exchange(client, false);
                    self.coverage.action("delayed-write");
                }
                8 => {
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
                9 => self.peer_outage(),
                10 => {
                    self.settle();
                    let index = self.rng.pick(self.nodes.len());
                    let id = self.retire(index, true);
                    self.add(Some(id));
                }
                11 => {
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
                12 => {
                    self.sim.inject("write", Fault::Errno(libc::EIO));
                    *self.coverage.injected.entry("write".into()).or_default() += 1;
                    self.update(4);
                    let client = self.request(4, false, false);
                    self.exchange(client, false);
                    self.coverage.action("failed-dirty-write");
                }
                13 if self.nodes.len() < MAX_NODES => {
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
                _ => {
                    let count = 1 + self.rng.pick(4);
                    self.coverage.action("traffic");
                    self.traffic(count, false);
                }
            }
        }
        // Recovery liveness is a mandatory oracle obligation, independent of the
        // generator's action mix: every current object must still be readable.
        for object in 0..8 {
            let client = self.request(object, false, false);
            self.exchange(client, false);
        }
        for node in &self.nodes {
            let snapshot = node.app.store.writer.index().snapshot().unwrap();
            assert!(snapshot.entries.len() <= node.config.limits.metadata_entries.get());
            self.coverage.persisted += snapshot.entries.len();
        }
        assert!(self.coverage.success >= 8 && self.coverage.bytes > PAGE_BYTES as usize);
        assert!(self.catalog.borrow().gets > 0 && self.coverage.persisted > 0);
        assert!(
            self.coverage
                .operations
                .get("complete:write")
                .copied()
                .unwrap_or(0)
                > 0
        );
        assert!(
            self.coverage
                .operations
                .get("complete:accept")
                .copied()
                .unwrap_or(0)
                > 0,
            "no actual peer exchange"
        );
        self.cache_obligations();
        for (operation, injected) in &self.coverage.injected {
            assert_eq!(
                self.coverage.observed.get(operation),
                Some(injected),
                "unobserved fault rule"
            );
        }
        while !self.nodes.is_empty() {
            self.remove(0);
        }
        self.coverage.collect(&self.sim);
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
        if self.native {
            let trace = self.fabric.trace();
            let writes = trace
                .iter()
                .filter(|e| {
                    e.operation == crate::rdma::lifecycle::simulation::Operation::Write
                        && e.completion
                        && e.result == 0
                })
                .count();
            assert!(writes > 0, "native graph never completed a DMA write");
            eprintln!("dst native completed writes={writes}");
        }
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
    let seeds: Vec<u64> = std::env::var("RACER_DST_SEEDS")
        .unwrap_or_else(|_| "1,7,42".into())
        .split(',')
        .map(|s| s.trim().parse().expect("comma-separated u64 DST seeds"))
        .collect();
    assert!(!seeds.is_empty() && seeds.len() <= 1024);
    let steps = setting("RACER_DST_STEPS", 32, 4096);
    for seed in seeds {
        let sim = Simulation::new();
        let _os = sim.enter();
        let clock = SimulationClock::new(seed);
        let environment = clock.environment(0);
        let _time = environment.enter();
        let mut harness = Harness::new(seed, sim, clock, native);
        harness.generated(steps);
    }
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
