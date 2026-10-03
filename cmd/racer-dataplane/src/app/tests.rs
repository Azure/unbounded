//! Application lifecycle tests and shared assembled-worker fixtures.
use super::*;
use crate::control::state;
use racer_control_wire as wire;
use std::{
    collections::VecDeque,
    io::{Read, Write},
    num::NonZeroUsize,
    path::PathBuf,
    thread,
    time::Instant,
};

pub(super) struct ControlFixture {
    pub bundle: Arc<Mutex<wire::KeyringBundle>>,
    pub keyring_override: Arc<Mutex<Option<(usize, Vec<u8>)>>>,
    pub keyring_tokens: Arc<Mutex<Vec<String>>>,
    pub reject_keyring_mtls: Arc<AtomicBool>,
    pub handshake_alerts: Arc<Mutex<VecDeque<u8>>>,
    pub directory: PathBuf,
    pub stop: Arc<AtomicBool>,
    pub server: Option<thread::JoinHandle<()>>,
    pub config: Option<Config>,
    pub enrollments: Arc<AtomicUsize>,
    pub polls: Arc<AtomicUsize>,
    pub binding: Arc<Mutex<NodeId>>,
    pub bootstrap_status: Arc<AtomicUsize>,
    pub poll_status: Arc<AtomicUsize>,
    pub certificate_age: Arc<AtomicUsize>,
    pub publication: Arc<Mutex<Option<state::Publication>>>,
    pub bootstrap_requests: Arc<Mutex<Vec<state::EnrollmentRequest>>>,
    pub poll_certificates: Arc<Mutex<Vec<Vec<u8>>>>,
    pub hold_long_poll: Arc<AtomicBool>,
    pub long_polls: Arc<AtomicUsize>,
}

/// Each accepted TLS socket gets the same independently controllable endpoints.
#[derive(Clone)]
struct ControlHandlers {
    enrollment: EnrollmentHandler,
    keyring: KeyringHandler,
    publication: PublicationHandler,
}
#[derive(Clone)]
struct EnrollmentHandler {
    ca: Arc<rcgen::Certificate>,
    ca_key: Arc<rcgen::KeyPair>,
    binding: Arc<Mutex<NodeId>>,
    status: Arc<AtomicUsize>,
    age: Arc<AtomicUsize>,
    issued: Arc<AtomicUsize>,
    requests: Arc<Mutex<Vec<state::EnrollmentRequest>>>,
}
#[derive(Clone)]
struct KeyringHandler {
    bundle: Arc<Mutex<wire::KeyringBundle>>,
    response: Arc<Mutex<Option<(usize, Vec<u8>)>>>,
    tokens: Arc<Mutex<Vec<String>>>,
    reject_mtls: Arc<AtomicBool>,
}
#[derive(Clone)]
struct PublicationHandler {
    initial: state::Publication,
    published: Arc<Mutex<Option<state::Publication>>>,
    binding: Arc<Mutex<NodeId>>,
    status: Arc<AtomicUsize>,
    certificates: Arc<Mutex<Vec<Vec<u8>>>>,
    polls: Arc<AtomicUsize>,
    waiting: Arc<AtomicUsize>,
    held: Arc<AtomicBool>,
    stopping: Arc<AtomicBool>,
}
type ControlStream = rustls::StreamOwned<rustls::ServerConnection, std::net::TcpStream>;

fn error_response(status: usize) -> (usize, Vec<u8>) {
    let code = if status == 503 {
        "unavailable"
    } else {
        "forbidden"
    };
    (status, format!("{{\"code\":\"{code}\"}}").into_bytes())
}

impl EnrollmentHandler {
    fn respond(&self, head: &str, body: &[u8], stream: &ControlStream) -> (usize, Vec<u8>) {
        assert!(head.contains("Authorization: Bearer fixture.token"));
        assert!(stream.conn.peer_certificates().is_none());
        let request = state::decode_enrollment_request(body).unwrap();
        self.requests.lock().unwrap().push(request.clone());
        let status = self.status.load(Ordering::Acquire);
        if status != 200 {
            return error_response(status);
        }
        let node = self.binding.lock().unwrap().clone();
        let not_before = std::time::SystemTime::now()
            - Duration::from_secs(self.age.load(Ordering::Acquire) as u64);
        self.issued.fetch_add(1, Ordering::Release);
        (
            200,
            wire::encode_enrollment_response(&crate::control::testing::issue_at(
                &request,
                &self.ca,
                &self.ca_key,
                &node.0,
                not_before,
            ))
            .unwrap(),
        )
    }
}
impl KeyringHandler {
    fn respond(&self, head: &str, stream: &ControlStream) -> Option<(usize, Vec<u8>)> {
        let mtls = stream.conn.peer_certificates().is_some();
        assert!(mtls || head.contains("Authorization: Bearer fixture.token"));
        if let Some(token) = head
            .lines()
            .find_map(|line| line.strip_prefix("Authorization: Bearer "))
        {
            self.tokens.lock().unwrap().push(token.to_owned());
        }
        let bundle = self.bundle.lock().unwrap();
        if self.reject_mtls.load(Ordering::Acquire) && mtls {
            Some(error_response(403))
        } else if let Some(response) = self.response.lock().unwrap().clone() {
            (response.0 != 0).then_some(response)
        } else if head
            .lines()
            .next()
            .unwrap()
            .contains(&format!("?after={} ", bundle.generation.0))
        {
            Some((204, Vec::new()))
        } else {
            Some((200, wire::encode_bundle(&bundle).unwrap()))
        }
    }
}
impl PublicationHandler {
    fn respond(&self, head: &str, stream: &ControlStream) -> (usize, Vec<u8>) {
        let status = self.status.load(Ordering::Acquire);
        if status != 200 {
            return error_response(status);
        }
        let mut publication = self
            .published
            .lock()
            .unwrap()
            .clone()
            .unwrap_or_else(|| self.initial.clone());
        publication.members[0].node = self.binding.lock().unwrap().clone();
        assert!(stream.conn.peer_certificates().is_some());
        self.certificates
            .lock()
            .unwrap()
            .push(stream.conn.peer_certificates().unwrap()[0].to_vec());
        self.polls.fetch_add(1, Ordering::Release);
        if head
            .lines()
            .next()
            .unwrap()
            .contains(&format!("?after={} ", publication.sequence.0))
        {
            self.waiting.fetch_add(1, Ordering::Release);
            while self.held.load(Ordering::Acquire) && !self.stopping.load(Ordering::Acquire) {
                thread::sleep(Duration::from_millis(1));
            }
            (204, Vec::new())
        } else {
            (200, state::encode_publication(&publication).unwrap())
        }
    }
}
impl ControlHandlers {
    fn serve(&self, tls: Arc<rustls::ServerConfig>, socket: std::net::TcpStream) {
        let mut stream =
            rustls::StreamOwned::new(rustls::ServerConnection::new(tls).unwrap(), socket);
        let mut head = Vec::new();
        loop {
            let mut byte = [0];
            if stream.read_exact(&mut byte).is_err() {
                return;
            }
            head.push(byte[0]);
            if head.ends_with(b"\r\n\r\n") {
                break;
            }
            assert!(head.len() <= 32768);
        }
        let head = String::from_utf8(head).unwrap();
        let length: usize = head
            .lines()
            .find_map(|line| {
                line.split_once(':')
                    .filter(|(k, _)| k.eq_ignore_ascii_case("content-length"))
                    .map(|(_, v)| v.trim().parse().unwrap())
            })
            .unwrap_or(0);
        let mut body = vec![0; length];
        stream.read_exact(&mut body).unwrap();
        let response = if head.starts_with("GET /v1/keyring") {
            self.keyring.respond(&head, &stream)
        } else if head.starts_with("POST ") {
            Some(self.enrollment.respond(&head, &body, &stream))
        } else {
            Some(self.publication.respond(&head, &stream))
        };
        let Some((status, body)) = response else {
            return;
        };
        let response = format!(
            "HTTP/1.1 {status} Result\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
            body.len()
        );
        let _ = stream
            .write_all(response.as_bytes())
            .and_then(|()| stream.write_all(&body))
            .and_then(|()| stream.flush());
    }
}

impl ControlFixture {
    /// Enroll a normal node; failure and retained-identity scenarios call bootstrap
    /// directly so their result and on-disk identity assertions stay independent.
    pub(super) fn bootstrap_node(
        &mut self,
        workers: u16,
        timeout: Duration,
    ) -> (Config, Arc<NodeState>) {
        let mut config = self.config.take().unwrap();
        let node = Arc::new(NodeState::new((0..workers).map(WorkerId).collect(), 64).unwrap());
        config.node = bootstrap(&config, &node, &config.limits, &scope(timeout).unwrap()).unwrap();
        (config, node)
    }

    pub(super) fn new() -> Self {
        static NEXT: AtomicUsize = AtomicUsize::new(0);
        let directory = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("target")
            .join(format!(
                "app-fixture-{}-{}",
                std::process::id(),
                NEXT.fetch_add(1, Ordering::Relaxed)
            ));
        std::fs::create_dir_all(&directory).unwrap();
        let (ca, ca_key, tls) = control_tls();
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        listener.set_nonblocking(true).unwrap();
        let mut config = crate::test_support::cluster::config(false);
        config.control_endpoint = format!("https://{}", listener.local_addr().unwrap());
        config.trust_bundle = directory.join("trust.pem");
        config.service_account_token = directory.join("token");
        config.identity_directory = directory.join("identity");
        config.slab_directory = directory.join("slabs");
        config.slab_bytes = 256 * 1024 * 1024;
        config.limits.range_window_pages = NonZeroUsize::new(2).unwrap();
        config.limits.connections_per_neighbor = NonZeroUsize::new(2).unwrap();
        config.limits.queue_entries = NonZeroUsize::new(64).unwrap();
        config.shutdown_timeout = Duration::from_secs(2);
        std::fs::write(&config.trust_bundle, ca.pem()).unwrap();
        std::fs::write(&config.service_account_token, b"fixture.token").unwrap();
        let mut fixture = Self {
            bundle: Arc::new(Mutex::new(wire::KeyringBundle {
                schema_version: 1,
                cluster: config.cluster.clone(),
                generation: wire::BundleGeneration(1),
                peer_trust_roots: vec![ca.der().to_vec()],
                cache_keys: vec![],
            })),
            keyring_override: Arc::new(Mutex::new(None)),
            keyring_tokens: Arc::new(Mutex::new(Vec::new())),
            reject_keyring_mtls: Arc::new(AtomicBool::new(false)),
            handshake_alerts: Arc::new(Mutex::new(VecDeque::new())),
            directory,
            stop: Arc::new(AtomicBool::new(false)),
            server: None,
            enrollments: Arc::new(AtomicUsize::new(0)),
            polls: Arc::new(AtomicUsize::new(0)),
            binding: Arc::new(Mutex::new(config.node.clone())),
            bootstrap_status: Arc::new(AtomicUsize::new(200)),
            poll_status: Arc::new(AtomicUsize::new(200)),
            certificate_age: Arc::new(AtomicUsize::new(1)),
            publication: Arc::new(Mutex::new(None)),
            bootstrap_requests: Arc::new(Mutex::new(Vec::new())),
            poll_certificates: Arc::new(Mutex::new(Vec::new())),
            hold_long_poll: Arc::new(AtomicBool::new(false)),
            long_polls: Arc::new(AtomicUsize::new(0)),
            config: Some(config),
        };
        let handlers = ControlHandlers {
            enrollment: EnrollmentHandler {
                ca: Arc::new(ca),
                ca_key: Arc::new(ca_key),
                binding: fixture.binding.clone(),
                status: fixture.bootstrap_status.clone(),
                age: fixture.certificate_age.clone(),
                issued: fixture.enrollments.clone(),
                requests: fixture.bootstrap_requests.clone(),
            },
            keyring: KeyringHandler {
                bundle: fixture.bundle.clone(),
                response: fixture.keyring_override.clone(),
                tokens: fixture.keyring_tokens.clone(),
                reject_mtls: fixture.reject_keyring_mtls.clone(),
            },
            publication: PublicationHandler {
                initial: publication(fixture.config.as_ref().unwrap(), 1, vec![]),
                published: fixture.publication.clone(),
                binding: fixture.binding.clone(),
                status: fixture.poll_status.clone(),
                certificates: fixture.poll_certificates.clone(),
                polls: fixture.polls.clone(),
                waiting: fixture.long_polls.clone(),
                held: fixture.hold_long_poll.clone(),
                stopping: fixture.stop.clone(),
            },
        };
        let stopping = fixture.stop.clone();
        let alerts = fixture.handshake_alerts.clone();
        fixture.server = Some(thread::spawn(move || {
            serve_control(listener, tls, handlers, stopping, alerts)
        }));
        fixture
    }
}

fn control_tls() -> (
    rcgen::Certificate,
    rcgen::KeyPair,
    Arc<rustls::ServerConfig>,
) {
    let (ca, ca_key) = crate::control::testing::ca();
    let server_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
    let mut params = rcgen::CertificateParams::new(vec!["127.0.0.1".into()]).unwrap();
    params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ServerAuth];
    let cert = params.signed_by(&server_key, &ca, &ca_key).unwrap();
    let mut roots = rustls::RootCertStore::empty();
    roots.add(ca.der().clone()).unwrap();
    let verifier = rustls::server::WebPkiClientVerifier::builder_with_provider(
        Arc::new(roots),
        Arc::new(rustls::crypto::ring::default_provider()),
    )
    .allow_unauthenticated()
    .build()
    .unwrap();
    let tls = Arc::new(
        rustls::ServerConfig::builder_with_provider(Arc::new(
            rustls::crypto::ring::default_provider(),
        ))
        .with_safe_default_protocol_versions()
        .unwrap()
        .with_client_cert_verifier(verifier)
        .with_single_cert(
            vec![cert.der().clone()],
            rustls::pki_types::PrivatePkcs8KeyDer::from(server_key.serialize_der()).into(),
        )
        .unwrap(),
    );
    (ca, ca_key, tls)
}

fn serve_control(
    listener: std::net::TcpListener,
    tls: Arc<rustls::ServerConfig>,
    endpoints: ControlHandlers,
    stopping: Arc<AtomicBool>,
    alerts: Arc<Mutex<VecDeque<u8>>>,
) {
    let mut handlers = Vec::new();
    while !stopping.load(Ordering::Acquire) {
        let (mut socket, _) = match listener.accept() {
            Ok(pair) => pair,
            Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                thread::sleep(Duration::from_millis(1));
                continue;
            }
            Err(e) => panic!("fixture accept: {e}"),
        };
        socket
            .set_read_timeout(Some(Duration::from_secs(3)))
            .unwrap();
        socket
            .set_write_timeout(Some(Duration::from_secs(3)))
            .unwrap();
        if let Some(alert) = alerts.lock().unwrap().pop_front() {
            // Read the complete ClientHello before sending Go's fatal
            // pre-handshake alert, avoiding a reset from unread TCP data.
            let mut header = [0; 5];
            socket.read_exact(&mut header).unwrap();
            let mut hello = vec![0; u16::from_be_bytes([header[3], header[4]]) as usize];
            socket.read_exact(&mut hello).unwrap();
            socket.write_all(&[21, 3, 3, 0, 2, 2, alert]).unwrap();
            continue;
        }
        let (tls, endpoints) = (tls.clone(), endpoints.clone());
        handlers.push(thread::spawn(move || endpoints.serve(tls, socket)));
        let mut i = 0;
        while i < handlers.len() {
            if handlers[i].is_finished() {
                handlers.swap_remove(i).join().unwrap();
            } else {
                i += 1;
            }
        }
    }
    for handler in handlers {
        handler.join().unwrap();
    }
}

impl Drop for ControlFixture {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Release);
        if let Some(server) = self.server.take() {
            server.join().unwrap();
        }
        std::fs::remove_dir_all(&self.directory).unwrap();
    }
}

pub(super) fn local_worker(
    config: &Config,
    node: &Arc<NodeState>,
    id: u16,
) -> (WorkerApplication, WorkerRuntime, PageCryptoEngine) {
    local_worker_with_fabric(config, node, id, Vec::new())
}

pub(super) fn local_worker_with_fabric(
    config: &Config,
    node: &Arc<NodeState>,
    id: u16,
    discovered_nics: Vec<crate::topology::rails::RailMapping>,
) -> (WorkerApplication, WorkerRuntime, PageCryptoEngine) {
    let worker = WorkerId(id);
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        config.limits.clone(),
    )));
    let (io, engine) = crate::runtime::crypto::pair(worker, 0, config.limits.queue_entries);
    let runtime = WorkerRuntime {
        reactor: Rc::new(Reactor::new(admission.clone())),
        admission,
        crypto: Rc::new(crate::runtime::crypto::CryptoClient::new(io)),
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
        PageCryptoEngine::new(CryptoRuntime { port: engine }),
    )
}

pub(super) fn definition() -> crate::control::state::CacheDefinition {
    let (client_socket, origin_socket) =
        crate::control::state::canonical_socket_paths("app-lifecycle").unwrap();
    crate::control::state::CacheDefinition {
        id: crate::model::CacheId("33333333-3333-4333-8333-333333333333".into()),
        name: "app-lifecycle".into(),
        client_socket,
        origin_socket,
    }
}

pub(super) fn publication(
    config: &Config,
    sequence: u64,
    caches: Vec<crate::control::state::CacheDefinition>,
) -> state::Publication {
    state::Publication {
        schema_version: 1,
        cluster: config.cluster.clone(),
        sequence: wire::PublicationSequence(sequence),
        membership_version: crate::model::MembershipVersion(1),
        members: vec![crate::topology::membership::Member {
            node: config.node.clone(),
            shares: std::num::NonZeroU32::new(1).unwrap(),
            peer_endpoint: "127.0.0.1:7443".into(),
            rails: vec![],
            site: String::new(),
        }],
        caches,
    }
}

pub(super) fn page(app: &WorkerApplication) -> crate::memory::page::PageResult {
    use crate::memory::{VerifiedBytes, VerifiedPage};
    use crate::model::{ResourceClass, VersionMetadata, *};
    let version = ObjectVersion {
        object: ObjectId {
            cache: definition().id,
            key: CacheKey([0; 32]),
        },
        etag: StrongEtag::test_value("one"),
    };
    let id = PageId {
        version: version.clone(),
        number: PageNumber(0),
    };
    let cache = &version.object.cache;
    let plaintext = VerifiedPage {
        inner: Arc::new(VerifiedBytes {
            page: id.clone(),
            bytes: vec![1; 3],
            reservation: app
                .runtime
                .admission
                .reserve(Some(cache), ResourceClass::Plaintext, 3)
                .unwrap(),
        }),
    };
    let ciphertext = BufferPool::new(app.runtime.admission.clone())
        .ciphertext(
            app.runtime
                .admission
                .reserve(Some(cache), ResourceClass::Ciphertext, 19)
                .unwrap(),
            PageEnvelope {
                page: id,
                key_id: app
                    .keys
                    .active(&definition().id, racer_identity::KeyPurpose::Page)
                    .map(|key| key.id())
                    .unwrap_or_else(|_| crate::model::key_id_from_generation(2, 7).unwrap()),
                nonce: Nonce([2; 24]),
                plaintext_length: 3,
                ciphertext_length: 19,
            },
            vec![2; 19],
        )
        .unwrap();
    crate::memory::page::PageResult {
        plaintext,
        ciphertext,
        metadata: VersionMetadata {
            version,
            length: 3,
            content_type: None,
        }
        .for_pin(),
    }
}

mod peer;

use crate::runtime::{
    admission::{AdmissionExt, AdmissionPolicy},
    crypto::{self, CryptoClient},
    reactor::Reactor,
};

#[test]
fn worker_sizing_reports_specific_resource_floor() {
    let base = Config::from_lookup(|name| {
        Ok(match name {
            "RACER_CLUSTER_ID" => Some("00000000-0000-4000-8000-000000000001".into()),
            "RACER_CONTROL_ENDPOINT" => Some("https://control.example".into()),
            "RACER_ENABLE_RDMA" => Some("false".into()),
            _ => None,
        })
    })
    .unwrap()
    .limits;
    assert!(partition_limits_with_cause(&base, 1, false).is_ok());
    assert_eq!(
        partition_limits_with_cause(&base, 0, false).err(),
        Some(("io_workers", Error::InvalidConfiguration))
    );
    for dimension in [
        "plaintext_bytes",
        "ciphertext_bytes",
        "dirty_bytes",
        "request_context_bytes",
        "queue_entries",
        "client_connections",
        "registered_bytes",
        "pipes",
    ] {
        let mut limits = base.clone();
        let value = match dimension {
            "plaintext_bytes" => &mut limits.plaintext_bytes,
            "ciphertext_bytes" => &mut limits.ciphertext_bytes,
            "dirty_bytes" => &mut limits.dirty_bytes,
            "request_context_bytes" => &mut limits.request_context_bytes,
            "queue_entries" => &mut limits.queue_entries,
            "client_connections" => &mut limits.client_connections,
            "registered_bytes" => &mut limits.registered_bytes,
            _ => &mut limits.pipes,
        };
        *value = NonZeroUsize::new(1).unwrap();
        let workers = if dimension == "pipes" { 2 } else { 1 };
        let rdma = dimension == "registered_bytes";
        assert_eq!(
            partition_limits_with_cause(&limits, workers, rdma).err(),
            Some((dimension, Error::InvalidConfiguration)),
            "{dimension}"
        );
        if rdma {
            assert!(partition_limits_with_cause(&limits, workers, false).is_ok());
        }
    }
}

#[test]
fn worker_sizing_funds_derived_connection_pools() {
    use crate::model::ResourceClass;
    use uring_runtime::affinity::{CpuLocation, EffectiveTopology};
    let config = Config::from_lookup(|name| {
        Ok(match name {
            "RACER_CLUSTER_ID" => Some("00000000-0000-4000-8000-000000000001".into()),
            "RACER_CONTROL_ENDPOINT" => Some("https://control.example".into()),
            "RACER_ENABLE_RDMA" => Some("false".into()),
            "RACER_CLIENT_CONNECTIONS" => Some("16".into()),
            _ => None,
        })
    })
    .unwrap();
    let make_plan = || {
        AffinityPlan::from_topology(
            &config,
            EffectiveTopology {
                cpus: (0..8)
                    .map(|cpu| CpuLocation {
                        cpu,
                        package: 0,
                        core: cpu,
                        numa_node: Some(0),
                    })
                    .collect(),
                quota: None,
                nics: vec![],
            },
            &[],
        )
        .unwrap()
    };
    let mut plan = make_plan();
    assert_eq!(plan.pairs.len(), 5);
    for workers in 2..=5 {
        // This cause feeds the production worker-sizing diagnostic, including
        // the last rejected count before reduction succeeds.
        assert_eq!(
            partition_limits_with_cause(&config.limits, workers, false).err(),
            Some(("control_connections", Error::InvalidConfiguration))
        );
    }
    let limits = size_workers(&config.limits, &mut plan, false).unwrap();
    assert_eq!(plan.pairs.len(), 1);
    assert_eq!(plan.crypto_groups(), vec![vec![0]]);
    assert_eq!(limits.client_connections.get(), 16);

    for (connections, neighbor, cause) in [
        (3, 2, Some("control_connections")),
        (11, 2, Some("control_connections")),
        (12, 2, None),
        (15, 4, None),
        (16, 4, None),
    ] {
        let mut node = config.limits.clone();
        node.client_connections = NonZeroUsize::new(connections).unwrap();
        node.connections_per_neighbor = NonZeroUsize::new(neighbor).unwrap();
        let result = partition_limits_with_cause(&node, 1, false);
        if let Some(cause) = cause {
            assert_eq!(result.err(), Some((cause, Error::InvalidConfiguration)));
            assert!(matches!(
                size_workers(&node, &mut make_plan(), false),
                Err(Error::InvalidConfiguration)
            ));
            continue;
        }
        let admission = flow_control::Quotas::new(AdmissionPolicy::new(result.unwrap()));
        let control = (0..3)
            .map(|_| {
                admission
                    .reserve_connection(ResourceClass::ControlConnection)
                    .unwrap()
            })
            .collect::<Vec<_>>();
        let outbound_count = neighbor.min(admission.limit(ResourceClass::OutboundConnection));
        let outbound = (0..outbound_count)
            .map(|_| {
                admission
                    .reserve_connection(ResourceClass::OutboundConnection)
                    .unwrap()
            })
            .collect::<Vec<_>>();
        let ingress = admission
            .reserve_connection(ResourceClass::IngressConnection)
            .unwrap();
        assert_eq!(
            admission.used(ResourceClass::Connection),
            3 + outbound_count + 1
        );
        assert!(admission.used(ResourceClass::Connection) <= connections);
        drop((control, outbound, ingress));
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
}

pub(crate) fn wake_test_worker() -> WorkerApplication {
    let config = crate::test_support::cluster::config(false);
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        config.limits.clone(),
    )));
    let (io, _engine) = crypto::pair(WorkerId(0), 0, config.limits.queue_entries);
    WorkerApplication::assemble(
        &config,
        Arc::new(NodeState::default()),
        WorkerId(0),
        WorkerRuntime {
            reactor: Rc::new(Reactor::new(admission.clone())),
            admission,
            crypto: Rc::new(CryptoClient::new(io)),
        },
        Vec::new(),
    )
    .unwrap()
}

pub(crate) fn wake_test_coordinator() -> Rc<Coordinator> {
    wake_test_worker().coordinator
}

#[test]
fn assembled_worker_exports_live_quota_gauges() {
    use crate::model::ResourceClass;
    let worker = wake_test_worker();
    let relay = worker
        .runtime
        .admission
        .reserve(None, ResourceClass::Relay, 1)
        .unwrap();
    let ciphertext = worker
        .runtime
        .admission
        .reserve(None, ResourceClass::Ciphertext, 17)
        .unwrap();
    let mut output = String::new();
    worker
        .telemetry
        .metrics
        .write_prometheus(&mut output)
        .unwrap();
    assert!(output.contains("racer_worker_relay_used{worker=\"0\"} 1\n"));
    assert!(output.contains("racer_worker_ciphertext_used_bytes{worker=\"0\"} 17\n"));
    for (name, class) in [
        ("relay_limit", ResourceClass::Relay),
        ("ciphertext_limit_bytes", ResourceClass::Ciphertext),
    ] {
        assert!(output.contains(&format!(
            "racer_worker_{name}{{worker=\"0\"}} {}\n",
            worker.runtime.admission.limit(class)
        )));
    }
    drop((relay, ciphertext));
}

#[test]
fn application_budget_poll_preserves_cooperative_and_completion_wakes() {
    let mut worker = wake_test_worker();
    // Exercise the production WorkerService entry point with side-effect-free
    // tasks. Stopping bypasses control publication and snapshot requirements.
    worker.started = true;
    worker.stopping = true;
    let count = Arc::new(crate::test_support::WakeCounter::default());
    let waker = std::task::Waker::from(count.clone());
    let mut cx = Context::from_waker(&waker);
    let (send, receive) = futures::channel::oneshot::channel::<()>();
    worker.peer_task = Some(Box::pin(async move {
        receive.await.map_err(|_| Error::Unavailable)
    }));
    let mut yielded = false;
    worker.diagnostic_task = Some(Box::pin(std::future::poll_fn(move |cx| {
        if yielded {
            Poll::Ready(Ok(()))
        } else {
            yielded = true;
            cx.waker().wake_by_ref();
            Poll::Pending
        }
    })));
    worker.poll_budgeted(&mut cx, 0).unwrap();
    assert_eq!(count.count(), 0);
    worker.poll_budgeted(&mut cx, 1).unwrap();
    assert_eq!(count.count(), 1, "cooperative continuation reaches driver");
    worker.poll_budgeted(&mut cx, 1).unwrap();
    assert_eq!(count.count(), 1, "blocked task does not spin");
    std::thread::spawn(move || send.send(()).unwrap())
        .join()
        .unwrap();
    assert_eq!(
        count.count(),
        2,
        "registered completion wakes driver across threads"
    );
    worker.poll_budgeted(&mut cx, 1).unwrap();
    assert!(worker.peer_task.is_none());
    assert!(worker.diagnostic_task.is_none());
}

#[test]
fn application_metadata_deadline_hook_is_budgeted_and_precedes_peer_polling() {
    use futures::{Stream, stream::FuturesUnordered};
    let mut worker = wake_test_worker();
    worker.started = true;
    worker.stopping = true;
    assert_eq!(
        Rc::strong_count(&worker.metadata),
        2,
        "worker and coordinator retain the same metadata service"
    );
    let count = Arc::new(crate::test_support::WakeCounter::default());
    let waker = std::task::Waker::from(count.clone());
    let mut cx = Context::from_waker(&waker);
    let mut children = FuturesUnordered::new();
    for _ in 0..65 {
        children.push(crate::read::metadata::tests::deadline_probe(
            &worker.metadata,
            Instant::now(),
        ));
    }
    assert!(
        std::pin::Pin::new(&mut children)
            .poll_next(&mut cx)
            .is_pending()
    );
    let completed = Rc::new(std::cell::Cell::new(0));
    let observed = completed.clone();
    worker.peer_task = Some(Box::pin(std::future::poll_fn(move |cx| {
        while let Poll::Ready(Some(result)) = std::pin::Pin::new(&mut children).poll_next(cx) {
            assert_eq!(result, Err(Error::DeadlineExceeded));
            observed.set(observed.get() + 1);
        }
        Poll::Pending
    })));
    worker.poll_budgeted(&mut cx, 0).unwrap();
    assert_eq!(completed.get(), 0);
    worker.poll_budgeted(&mut cx, usize::MAX).unwrap();
    assert_eq!(
        completed.get(),
        64,
        "deadline hook clamps before polling peers"
    );
    worker.poll_budgeted(&mut cx, 1).unwrap();
    assert_eq!(completed.get(), 65);
    let settled = count.count();
    worker.poll_budgeted(&mut cx, 64).unwrap();
    assert_eq!(count.count(), settled);
}

#[test]
fn composes_http_and_optional_rdma_without_operational_side_effects() {
    for enable_rdma in [false, true] {
        let config = crate::test_support::cluster::config(enable_rdma);
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            config.limits.clone(),
        )));
        let (io, engine) = crypto::pair(WorkerId(0), 0, config.limits.queue_entries);
        let crypto = Rc::new(CryptoClient::new(io));
        let runtime = WorkerRuntime {
            reactor: Rc::new(Reactor::new(admission.clone())),
            admission,
            crypto: crypto.clone(),
        };
        let application =
            Application::assemble(crate::test_support::cluster::config(enable_rdma)).unwrap();
        let _engine = application
            .build_crypto(WorkerId(0), CryptoRuntime { port: engine })
            .unwrap();
        let node = Arc::new(NodeState::default());
        let mut worker =
            WorkerApplication::assemble(&config, node.clone(), WorkerId(0), runtime, Vec::new())
                .expect("valid side-effect-free worker composition");
        assert!(worker.control.is_some());
        assert_eq!(worker.rdma.is_some(), enable_rdma);
        assert_eq!(
            Rc::strong_count(&worker.flights),
            2,
            "worker lifecycle and fill share one flight table"
        );
        assert_eq!(
            Rc::strong_count(&crypto),
            3,
            "runtime and page facade share one local crypto client"
        );
        let second_admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            config.limits.clone(),
        )));
        let (second_io, second_engine) = crypto::pair(WorkerId(1), 0, config.limits.queue_entries);
        let second_runtime = WorkerRuntime {
            reactor: Rc::new(Reactor::new(second_admission.clone())),
            admission: second_admission,
            crypto: Rc::new(CryptoClient::new(second_io)),
        };
        let _second_engine = application
            .build_crypto(
                WorkerId(1),
                CryptoRuntime {
                    port: second_engine,
                },
            )
            .unwrap();
        let second = WorkerApplication::assemble(
            &config,
            node.clone(),
            WorkerId(1),
            second_runtime,
            Vec::new(),
        )
        .expect("valid second worker composition");
        assert!(
            second.control.is_none(),
            "only one worker enrolls and publishes"
        );
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert_eq!(worker.poll_budgeted(&mut cx, 0), Err(Error::Unavailable));
        assert_eq!(worker.poll_budgeted(&mut cx, 1), Err(Error::Unavailable));
        // Admission bounds retained generations once across the node.
        let mut publication = crate::control::state::decode_publication(include_bytes!(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../../internal/racer/wire/testdata/publication.json"
        )))
        .unwrap();
        publication.cluster = config.cluster.clone();
        let mut requests = Vec::new();
        for version in 1..=config.limits.retained_snapshots.get() + 1 {
            publication.sequence.0 = version as u64;
            publication.membership_version.0 = version as u64;
            let snapshot = worker.snapshots.publish(publication.clone()).unwrap();
            requests.push(snapshot.membership.clone());
        }
        publication.sequence.0 += 1;
        publication.membership_version.0 += 1;
        assert!(matches!(
            worker.snapshots.publish(publication),
            Err(Error::Overloaded)
        ));
    }
}

#[test]
fn shared_factory_is_send_and_sync_without_moving_worker_graphs() {
    fn shared<T: Send + Sync>() {}
    shared::<Application>();
    shared::<NodeState>();
}

#[test]
fn multiworker_memberships_retire_after_request_leases_and_reuse_capacity() {
    use crate::{model::MembershipVersion, peer::PeerNetwork};
    let mut publication = crate::control::state::decode_publication(include_bytes!(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../../internal/racer/wire/testdata/publication.json"
    )))
    .unwrap();
    let local = publication.members[0].node.clone();
    let neighbor = publication.members[1].node.clone();
    let published = Arc::new(PublishedState::default());
    let store = SnapshotStore::new(publication.cluster.clone(), published.clone(), 1);
    let networks = [
        PeerNetwork::new(local.clone(), published.clone()).unwrap(),
        PeerNetwork::new(local, published).unwrap(),
    ];
    let mut publish = |sequence, version| {
        publication.sequence.0 = sequence;
        publication.membership_version.0 = version;
        store.publish(publication.clone())
    };
    let first = publish(1, 1).unwrap();
    // A delayed worker holds a publication, while a read starts from the
    // cache-only replacement. Both must own the same canonical generation.
    let cache_only = publish(2, 1).unwrap();
    assert!(Arc::ptr_eq(&first.membership, &cache_only.membership));
    let request = cache_only.membership.clone();
    let weak = Arc::downgrade(&request);
    drop(cache_only);
    let current = publish(3, 2).unwrap();
    for network in &networks {
        assert!(Arc::ptr_eq(
            &request,
            &network.membership(MembershipVersion(1)).unwrap()
        ));
        assert!(network.endpoint(&request, &neighbor).is_ok());
    }
    assert!(matches!(publish(4, 3), Err(Error::Overloaded)));
    // Even at capacity, arbitrarily many cache-only publications fit.
    for sequence in 4..30 {
        publish(sequence, 2).unwrap();
    }
    drop(current);
    drop(request);
    assert!(
        weak.upgrade().is_some(),
        "delayed worker still holds the generation"
    );
    drop(first);
    assert!(weak.upgrade().is_none());
    for network in &networks {
        assert!(network.membership(MembershipVersion(1)).is_err());
    }
    for version in 3..20 {
        let current = publish(version + 30, version).unwrap();
        for network in &networks {
            assert!(Arc::ptr_eq(
                &current.membership,
                &network.membership(MembershipVersion(version)).unwrap()
            ));
        }
    }
    // Only one worker observes an intermediate publication. Neither requires
    // an installation/retirement poll to resolve the next accepted version.
    publish(50, 20).unwrap();
    let delayed = networks[0].membership(MembershipVersion(20)).unwrap();
    publish(51, 21).unwrap();
    for worker in [1, 0] {
        assert!(networks[worker].membership(MembershipVersion(21)).is_ok());
    }
    drop(delayed);
    publish(52, 22).unwrap();
    assert!(networks[0].membership(MembershipVersion(20)).is_err());
}

#[test]
fn removal_visibility_changes_without_worker_or_checkpoint_barriers() {
    let config = crate::test_support::cluster::config(false);
    let node = Arc::new(NodeState::default());
    let (worker, _, _) = local_worker(&config, &node, 0);
    let cache = definition();
    let availability =
        crate::control::state::Availability::new(node.publications.clone(), worker.keys.clone());
    for (sequence, present) in [(1, true), (2, false), (3, true)] {
        worker
            .snapshots
            .publish(publication(
                &config,
                sequence,
                if present { vec![cache.clone()] } else { vec![] },
            ))
            .unwrap();
        assert_eq!(availability.cache(&cache.id), present);
    }
}

#[test]
fn non_listener_worker_installs_nonempty_cache_set_without_binding_paths() {
    let config = crate::test_support::cluster::config(false);
    let node = Arc::new(NodeState::default());
    let (mut worker, _, _) = local_worker(&config, &node, 1);
    let definition = definition();
    worker
        .snapshots
        .publish(publication(&config, 1, vec![definition.clone()]))
        .unwrap();
    worker
        .refresh_snapshot(&scope(Duration::from_secs(1)).unwrap())
        .unwrap();
    assert_eq!(worker.caches, vec![definition]);
    assert!(worker.peer_task.is_none());
    assert!(worker.diagnostic_task.is_none());
    assert!(worker.prepared_listeners.borrow().is_none());
}

#[test]
fn snapshot_refresh_retries_canceled_publication_and_applies_skipped_removal() {
    use racer_control_wire as wire;
    let config = crate::test_support::cluster::config(false);
    let node = Arc::new(NodeState::default());
    let (mut worker, _, _) = local_worker(&config, &node, 1);
    let original = definition();
    let current_scope = scope(Duration::from_secs(1)).unwrap();
    assert_eq!(
        worker.refresh_snapshot(&current_scope),
        Err(Error::Unavailable)
    );
    worker
        .snapshots
        .publish(publication(&config, 1, vec![original.clone()]))
        .unwrap();
    worker.refresh_snapshot(&current_scope).unwrap();
    assert_eq!(worker.caches, vec![original.clone()]);
    let (_, _, roots) = crate::security::test_support::issued();
    worker
        .keys
        .install(wire::KeyringBundle {
            schema_version: wire::SCHEMA_VERSION,
            cluster: config.cluster.clone(),
            generation: wire::BundleGeneration(1),
            peer_trust_roots: roots,
            cache_keys: vec![wire::CacheEncryptionKey::new(
                wire::CacheKeyRef {
                    cache: original.id.clone(),
                    id: crate::model::key_id_from_generation(1, 7).unwrap(),
                    purpose: wire::CacheKeyPurpose::Page,
                },
                wire::CacheKeyState::Active,
                zeroize::Zeroizing::new([19; 32]),
            )],
        })
        .unwrap();
    let page = page(&worker);
    let page_id = page.plaintext.page().clone();
    let retained_plaintext = Arc::downgrade(&page.plaintext.inner);
    let retained_ciphertext = Arc::downgrade(&page.ciphertext.inner);
    let metadata = page.metadata.immutable();
    let index = worker.store.writer.index().clone();
    index.publish_version(metadata.clone()).unwrap();
    worker.memory.publish(page).unwrap();
    assert!(worker.memory.get(&page_id).unwrap().is_some());
    assert_eq!(retained_plaintext.strong_count(), 1);
    assert_eq!(retained_ciphertext.strong_count(), 1);
    assert_eq!(index.snapshot_metadata(), vec![metadata.clone()]);

    // Skip the empty publication: the next refresh must still retire the old UID.
    worker
        .snapshots
        .publish(publication(&config, 2, vec![]))
        .unwrap();
    let mut replacement = original.clone();
    replacement.id = crate::model::CacheId("55555555-5555-4555-8555-555555555555".into());
    worker
        .snapshots
        .publish(publication(&config, 3, vec![replacement.clone()]))
        .unwrap();
    assert_eq!(retained_plaintext.strong_count(), 1);
    assert_eq!(retained_ciphertext.strong_count(), 1);
    assert_eq!(index.snapshot_metadata(), vec![metadata]);
    let canceled = scope(Duration::from_secs(1)).unwrap();
    canceled.cancel().unwrap();
    assert_eq!(worker.refresh_snapshot(&canceled), Err(Error::Cancelled));
    assert_eq!(worker.caches, vec![original]);
    assert_eq!(worker.snapshot_sequence, Some(wire::PublicationSequence(1)));
    // Removal precedes the scope check, even though installation must retry.
    assert!(retained_plaintext.upgrade().is_none());
    assert!(retained_ciphertext.upgrade().is_none());
    assert!(index.snapshot_metadata().is_empty());
    worker.refresh_snapshot(&current_scope).unwrap();
    assert_eq!(worker.caches, vec![replacement]);
    assert_eq!(worker.snapshot_sequence, Some(wire::PublicationSequence(3)));
    worker.refresh_snapshot(&canceled).unwrap();
    assert!(worker.memory.get(&page_id).unwrap().is_none());
    assert!(index.snapshot_metadata().is_empty());
    assert!(worker.peer_task.is_none());
    assert!(worker.prepared_listeners.borrow().is_none());
}

#[test]
fn two_worker_removal_preserves_late_driver_and_blocks_late_memory_and_disk_fill() {
    let mut fixture = ControlFixture::new();
    let (config, node) = fixture.bootstrap_node(2, Duration::from_secs(15));
    let (mut first, rt0, mut crypto0) = local_worker(&config, &node, 0);
    let (mut second, rt1, mut crypto1) = local_worker(&config, &node, 1);
    first
        .telemetry
        .attach_io(rt0.reactor.clone(), rt0.admission.clone())
        .unwrap();
    for app in [&mut first, &mut second] {
        futures::executor::block_on(app.store.writer.open()).unwrap();
        app.caches = vec![definition()];
    }
    first
        .snapshots
        .publish(publication(&config, 1, vec![definition()]))
        .unwrap();
    first
        .keys
        .install(wire::KeyringBundle {
            schema_version: 1,
            cluster: config.cluster.clone(),
            generation: wire::BundleGeneration(2),
            peer_trust_roots: (*first.keys.peer_trust_roots().unwrap()).clone(),
            cache_keys: vec![wire::CacheEncryptionKey::new(
                wire::CacheKeyRef {
                    cache: definition().id,
                    id: crate::model::key_id_from_generation(2, 7).unwrap(),
                    purpose: wire::CacheKeyPurpose::Page,
                },
                wire::CacheKeyState::Active,
                zeroize::Zeroizing::new([19; 32]),
            )],
        })
        .unwrap();
    let late0 = page(&first);
    let late1 = page(&second);
    first.memory.publish(late0.clone()).unwrap();
    second.memory.publish(late1.clone()).unwrap();
    let adapter = caches::CachePublication {
        node: node.clone(),
        listeners: first.prepared_listeners.clone(),
        capacity: config.limits.metadata_entries.get(),
    };
    assert!(matches!(adapter.stage(&[]), Err(Error::Unavailable)));
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    first.poll_cache_preparation(&mut cx).unwrap();
    let (release, receive) = futures::channel::oneshot::channel::<()>();
    let memory = second.memory.clone();
    let late = late1.clone();
    let owner = second.drivers.enter();
    uring_runtime::drivers::reserve()
        .unwrap()
        .submit_detached(Box::pin(async move {
            receive.await.map_err(|_| Error::Cancelled)?;
            assert_eq!(memory.publish(late), Err(Error::Unavailable));
            Ok::<_, Error>(())
        }));
    drop(owner);
    for _ in 0..4 {
        first.poll_cache_preparation(&mut cx).unwrap();
        second.poll_cache_preparation(&mut cx).unwrap();
    }
    assert_eq!(second.drivers.pending(), 1);
    assert_eq!(
        first.snapshots.cursor().unwrap(),
        Some(wire::PublicationSequence(1))
    );
    assert!(first.memory.get(late0.plaintext.page()).unwrap().is_some());
    // Avoid a real control poll here: drive the exact stage/acceptance handoff.
    first.control_task = Some(Box::pin(std::future::pending()));
    for _ in 0..8 {
        first.poll_cache_preparation(&mut cx).unwrap();
        second.poll_cache_preparation(&mut cx).unwrap();
    }
    let rejected = adapter.stage(&[]).unwrap();
    let mut invalid = publication(&config, 2, vec![]);
    invalid.members[0].peer_endpoint = "127.0.0.1:7444".into();
    assert!(matches!(
        first.snapshots.publish_staged(invalid, Some(rejected)),
        Err(Error::IncompatibleMembership)
    ));
    assert_eq!(
        first.snapshots.cursor().unwrap(),
        Some(wire::PublicationSequence(1))
    );
    assert!(first.memory.get(late0.plaintext.page()).unwrap().is_some());
    for _ in 0..4 {
        first.poll_cache_preparation(&mut cx).unwrap();
        second.poll_cache_preparation(&mut cx).unwrap();
    }
    first
        .snapshots
        .publish_staged(
            publication(&config, 2, vec![]),
            Some(adapter.stage(&[]).unwrap()),
        )
        .unwrap();
    assert_eq!(
        second.drivers.pending(),
        1,
        "removal cannot cancel an accepted driver"
    );
    assert!(second.memory.get(late1.plaintext.page()).unwrap().is_none());
    release.send(()).unwrap();
    second.drivers.poll(&mut cx, 64);
    assert_eq!(second.drivers.pending(), 0);
    for (app, late) in [(&first, late0), (&second, late1)] {
        assert!(app.memory.get(late.plaintext.page()).unwrap().is_none());
        assert_eq!(app.memory.publish(late.clone()), Err(Error::Unavailable));
        let dirty = app
            .runtime
            .admission
            .reserve(
                Some(&definition().id),
                crate::model::ResourceClass::DirtyCiphertext,
                19,
            )
            .unwrap();
        assert!(matches!(
            app.store.writer.enqueue(late.copy(), dirty),
            Err(Error::MissingKey)
        ));
    }
    assert!(second.peer_task.is_none() && second.diagnostic_task.is_none());
    // UID reuse is staged normally and consumes no cumulative tombstones.
    assert!(matches!(
        adapter.stage(&[definition()]),
        Err(Error::Unavailable)
    ));
    for (app, runtime, engine) in [
        (&mut first, &rt0, &mut crypto0),
        (&mut second, &rt1, &mut crypto1),
    ] {
        app.stop_admission().unwrap();
        if let Some(s) = &app.diagnostic_scope {
            s.cancel().unwrap();
        }
        app.peer_task.take();
        app.diagnostic_task.take();
        app.control_task.take();
        drive(runtime, engine, runtime.reactor.drain()).unwrap();
    }
}

#[test]
fn two_workers_start_from_real_control_and_checkpoint_one_complete_cut() {
    let mut fixture = ControlFixture::new();
    let (config, node) = fixture.bootstrap_node(2, Duration::from_secs(15));
    let ready = std::sync::Barrier::new(2);
    thread::scope(|threads| {
        for id in 0..2 {
            let (config, node, ready) = (&config, &node, &ready);
            threads.spawn(move || {
                let (mut app, runtime, mut engine) = local_worker(config, node, id);
                use crate::telemetry::metrics::Event;
                app.telemetry
                    .metrics
                    .record(Event::MemoryHit, u64::from(id) + 1)
                    .unwrap();
                drive(
                    &runtime,
                    &mut engine,
                    app.start(&scope(Duration::from_secs(15)).unwrap()),
                )
                .unwrap();
                ready.wait();
                assert!(node.observations.health.ready());
                assert_eq!(app.telemetry.metrics.count(Event::MemoryHit), 3);
                assert_eq!(app.peer_task.is_some(), id == 0);
                ready.wait();
                stop_worker(&mut app, &runtime, &mut engine);
            });
        }
    });
    let images = crate::store::checkpoint::read_candidates(&config.slab_directory).unwrap();
    assert_eq!(images.len(), 1);
    assert_eq!(images[0].1.shards.len(), 2);
    assert_eq!(fixture.enrollments.load(Ordering::Acquire), 2);
}

#[test]
fn startup_finishes_local_snapshot_installation_while_next_long_poll_is_held() {
    let mut fixture = ControlFixture::new();
    fixture.hold_long_poll.store(true, Ordering::Release);
    let (config, node) = fixture.bootstrap_node(2, Duration::from_secs(5));
    let finished = std::sync::Barrier::new(2);
    let results = thread::scope(|threads| {
        let mut handles = Vec::new();
        for id in 0..2 {
            let (config, node, waiting, finished) =
                (&config, &node, &fixture.long_polls, &finished);
            handles.push(threads.spawn(move || {
                let (mut worker, runtime, mut engine) = local_worker(config, node, id);
                let startup = scope(Duration::from_secs(3)).unwrap();
                // Force the first publication to wait for the second worker,
                // then allow local preparation after the next poll is held.
                if id == 1 {
                    while waiting.load(Ordering::Acquire) == 0 {
                        if startup.check().is_err() {
                            break;
                        }
                        thread::sleep(Duration::from_millis(1));
                    }
                }
                let result = drive(&runtime, &mut engine, worker.start(&startup));
                finished.wait();
                if result.is_ok() {
                    assert!(node.observations.health.ready());
                    assert!(worker.started);
                    assert!(worker.snapshots.current().is_ok());
                }
                finished.wait();
                let shutdown = scope(Duration::from_secs(5)).unwrap();
                drive(&runtime, &mut engine, worker.drain(&shutdown)).unwrap();
                drive(&runtime, &mut engine, worker.shutdown(&shutdown)).unwrap();
                result
            }));
        }
        handles
            .into_iter()
            .map(|h| h.join().unwrap())
            .collect::<Vec<_>>()
    });
    assert_eq!(results, vec![Ok(()), Ok(())]);
    assert!(fixture.hold_long_poll.load(Ordering::Acquire));
    assert_eq!(fixture.long_polls.load(Ordering::Acquire), 1);
    assert_eq!(fixture.enrollments.load(Ordering::Acquire), 2);
    assert!(node.cache_cut.lock().unwrap().committed);
    fixture.hold_long_poll.store(false, Ordering::Release);
}

#[test]
fn same_node_renewal_backs_off_expires_closed_and_recovers() {
    use crate::telemetry::health::State;
    use uring_runtime::environment::{SimulationClock, now, wall_now};

    // Complete each real control turn through the application's error handling.
    // Leave the next turn unsubmitted so the test controls all retry boundaries.
    fn turn(
        worker: &mut WorkerApplication,
        runtime: &WorkerRuntime,
        engine: &mut PageCryptoEngine,
    ) {
        let done = Rc::new(std::cell::Cell::new(false));
        let completed = done.clone();
        let control = worker.control.clone().unwrap();
        let request_scope = scope(Duration::from_secs(40)).unwrap();
        assert!(worker.control_task.is_none());
        worker.control_task = Some(Box::pin(async move {
            let result = control.progress(&request_scope).await;
            completed.set(true);
            result.map(|_| ())
        }));
        drive(
            runtime,
            engine,
            Box::pin(std::future::poll_fn(|cx| {
                worker.poll_control(cx)?;
                if done.get() {
                    worker.control_task.take();
                    Poll::Ready(Ok(()))
                } else {
                    Poll::Pending
                }
            })),
        )
        .unwrap();
        worker.observe_health().unwrap();
        assert!(worker.started && !worker.stopping);
        assert!(!runtime.admission.is_stopped());
    }

    let mut fixture = ControlFixture::new();
    let (config, node) = fixture.bootstrap_node(1, Duration::from_secs(15));
    // An already renewal-due, still valid certificate avoids advancing the TLS
    // server's host clock. A fresh 24-hour certificate covers the later virtual
    // expiry of this old certificate, and is also valid for real TLS handshakes.
    fixture
        .certificate_age
        .store(16 * 3600 + 60, Ordering::Release);
    let (mut worker, runtime, mut engine) = local_worker(&config, &node, 0);
    drive(
        &runtime,
        &mut engine,
        worker.start(&scope(Duration::from_secs(15)).unwrap()),
    )
    .unwrap();
    assert!(node.observations.health.ready());
    // Retained-publication startup completes staging without another HTTP turn.
    // Establish a successful control turn before measuring renewal-only backoff.
    turn(&mut worker, &runtime, &mut engine);
    let control = worker.control.clone().unwrap();
    let old = control.identity().unwrap();
    assert!(old.renewal_due() && old.valid_now());
    let old_signing = worker.keys.signing_identity().unwrap();
    let committed = std::fs::read(config.identity_directory.join("identity.json")).unwrap();
    let baseline = fixture.bootstrap_requests.lock().unwrap().len();
    let polls = fixture.polls.load(Ordering::Acquire);

    let clock = SimulationClock::new_at(3, Instant::now(), std::time::SystemTime::now());
    let environment = clock.environment(0);
    let _clock = environment.enter();
    fixture.bootstrap_status.store(503, Ordering::Release);
    turn(&mut worker, &runtime, &mut engine);
    assert_eq!(control.renewal_error(), Some(Error::Unavailable));
    assert_eq!(
        fixture.bootstrap_requests.lock().unwrap().len(),
        baseline + 1
    );
    assert_eq!(fixture.polls.load(Ordering::Acquire), polls + 1);
    let pending = std::fs::read(config.identity_directory.join("pending.json")).unwrap();

    // Even many successful 204 polls must neither reset renewal backoff nor
    // replace the accepted certificate. Check both sides of the 1-second retry.
    clock.advance(Duration::from_millis(999));
    for _ in 0..16 {
        turn(&mut worker, &runtime, &mut engine);
        assert!(node.observations.health.ready());
        assert!(old.valid_now());
        assert!(Arc::ptr_eq(
            &old_signing,
            &worker.keys.signing_identity().unwrap()
        ));
        assert_eq!(
            control.identity().unwrap().certificate_chain(),
            old.certificate_chain()
        );
        assert_eq!(control.renewal_error(), Some(Error::Unavailable));
    }
    assert_eq!(
        fixture.bootstrap_requests.lock().unwrap().len(),
        baseline + 1
    );
    assert_eq!(fixture.polls.load(Ordering::Acquire), polls + 17);
    clock.advance(Duration::from_millis(1));
    turn(&mut worker, &runtime, &mut engine);
    assert_eq!(
        fixture.bootstrap_requests.lock().unwrap().len(),
        baseline + 2
    );
    assert_eq!(fixture.polls.load(Ordering::Acquire), polls + 18);
    // This seed selects a second jittered delay greater than one second.
    // Successful polls must retain the failure count so retries grow rather
    // than restarting at the first-failure delay on every turn.
    clock.advance(Duration::from_secs(1));
    turn(&mut worker, &runtime, &mut engine);
    assert_eq!(
        fixture.bootstrap_requests.lock().unwrap().len(),
        baseline + 2
    );
    assert_eq!(fixture.polls.load(Ordering::Acquire), polls + 19);
    for cert in &fixture.poll_certificates.lock().unwrap()[polls..] {
        assert_eq!(cert, &old.certificate_chain()[0]);
    }
    assert_eq!(
        std::fs::read(config.identity_directory.join("identity.json")).unwrap(),
        committed
    );
    assert_eq!(
        std::fs::read(config.identity_directory.join("pending.json")).unwrap(),
        pending
    );

    // Refresh the observation just before expiry: readiness must fail because of
    // credentials, not because its independent 2-second observation lease aged.
    clock.advance(old.expires_at().duration_since(wall_now()).unwrap() - Duration::from_millis(1));
    worker.observe_health().unwrap();
    assert!(node.observations.health.ready());
    clock.advance(Duration::from_millis(1));
    assert!(!old.valid_now());
    assert!(!node.observations.health.ready());
    worker.observe_health().unwrap();
    assert_eq!(node.observations.health.state(), Ok(State::Degraded));
    let polls_at_expiry = fixture.polls.load(Ordering::Acquire);
    assert!(matches!(
        drive(
            &runtime,
            &mut engine,
            control.poll(
                wire::SnapshotRequest { after: None },
                &scope(Duration::from_secs(10)).unwrap()
            )
        ),
        Err(Error::Unauthorized)
    ));
    turn(&mut worker, &runtime, &mut engine);
    assert_eq!(
        fixture.bootstrap_requests.lock().unwrap().len(),
        baseline + 3
    );
    assert_eq!(fixture.polls.load(Ordering::Acquire), polls_at_expiry);
    assert_eq!(control.renewal_error(), Some(Error::Unavailable));
    let retry = control.next_attempt().unwrap();
    assert!(
        (Duration::from_secs(1)..=Duration::from_secs(30)).contains(&retry.duration_since(now()))
    );
    clock.advance(retry.duration_since(now()) - Duration::from_millis(1));
    for _ in 0..16 {
        turn(&mut worker, &runtime, &mut engine);
        assert!(!node.observations.health.ready());
    }
    assert_eq!(
        fixture.bootstrap_requests.lock().unwrap().len(),
        baseline + 3
    );
    assert_eq!(fixture.polls.load(Ordering::Acquire), polls_at_expiry);
    assert_eq!(
        std::fs::read(config.identity_directory.join("identity.json")).unwrap(),
        committed
    );

    fixture.certificate_age.store(1, Ordering::Release);
    fixture.bootstrap_status.store(200, Ordering::Release);
    clock.advance(Duration::from_millis(1));
    turn(&mut worker, &runtime, &mut engine);
    let fresh = control.identity().unwrap();
    assert_eq!(fresh.node(), old.node());
    assert!(fresh.valid_now() && !fresh.renewal_due());
    assert_ne!(fresh.certificate_chain(), old.certificate_chain());
    assert_ne!(fresh.private_key_der(), old.private_key_der());
    assert_eq!(
        worker.keys.signing_identity().unwrap().certificate_chain(),
        fresh.certificate_chain()
    );
    assert_eq!(control.renewal_error(), None);
    assert_eq!(control.next_attempt(), Some(now()));
    assert!(node.observations.health.ready());
    assert!(!config.identity_directory.join("pending.json").exists());
    assert_ne!(
        std::fs::read(config.identity_directory.join("identity.json")).unwrap(),
        committed
    );
    assert_eq!(
        fixture.bootstrap_requests.lock().unwrap().len(),
        baseline + 4
    );
    let requests = fixture.bootstrap_requests.lock().unwrap();
    for request in &requests[baseline..] {
        assert_eq!(request.enrollment, requests[baseline].enrollment);
        assert_eq!(request.csr_der, requests[baseline].csr_der);
    }
    assert_ne!(requests[baseline].csr_der, requests[baseline - 1].csr_der);
    drop(requests);
    turn(&mut worker, &runtime, &mut engine);
    assert_eq!(
        fixture.bootstrap_requests.lock().unwrap().len(),
        baseline + 4
    );
    assert_eq!(fixture.polls.load(Ordering::Acquire), polls_at_expiry + 2);
    for cert in &fixture.poll_certificates.lock().unwrap()[polls_at_expiry..] {
        assert_eq!(cert, &fresh.certificate_chain()[0]);
    }
    assert!(node.observations.health.ready());
    assert_eq!(
        worker.snapshots.cursor().unwrap(),
        Some(wire::PublicationSequence(1))
    );

    drop(_clock);
    stop_worker(&mut worker, &runtime, &mut engine);
}

#[test]
fn removal_publication_finishes_locally_after_controller_disappears() {
    use crate::model::{VersionMetadata, *};
    let mut fixture = ControlFixture::new();
    let mut config = fixture.config.take().unwrap();
    let diagnostic_address = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    config.diagnostics_listen = diagnostic_address.local_addr().unwrap();
    drop(diagnostic_address);
    let keep = crate::control::state::CacheDefinition {
        id: CacheId("44444444-4444-4444-8444-444444444444".into()),
        name: "keep".into(),
        client_socket: "/run/racer/keep/client/socket".into(),
        origin_socket: "/run/racer/keep/origin/socket".into(),
    };
    *fixture.publication.lock().unwrap() =
        Some(publication(&config, 1, vec![definition(), keep.clone()]));
    let node = Arc::new(NodeState::new(vec![WorkerId(0)], 64).unwrap());
    config.node = bootstrap(
        &config,
        &node,
        &config.limits,
        &scope(Duration::from_secs(15)).unwrap(),
    )
    .unwrap();
    let (mut worker, runtime, mut engine) = local_worker(&config, &node, 0);
    Rc::get_mut(&mut worker.clients)
        .unwrap()
        .set_root(fixture.directory.join("sockets"));
    drive(
        &runtime,
        &mut engine,
        worker.start(&scope(Duration::from_secs(15)).unwrap()),
    )
    .unwrap();
    worker
        .keys
        .install(wire::KeyringBundle {
            schema_version: 1,
            cluster: config.cluster.clone(),
            generation: wire::BundleGeneration(2),
            peer_trust_roots: (*worker.keys.peer_trust_roots().unwrap()).clone(),
            cache_keys: vec![wire::CacheEncryptionKey::new(
                wire::CacheKeyRef {
                    cache: keep.id.clone(),
                    id: crate::model::key_id_from_generation(2, 9).unwrap(),
                    purpose: wire::CacheKeyPurpose::Page,
                },
                wire::CacheKeyState::Active,
                zeroize::Zeroizing::new([29; 32]),
            )],
        })
        .unwrap();
    let metadata = VersionMetadata {
        content_type: None,
        version: ObjectVersion {
            object: ObjectId {
                cache: keep.id.clone(),
                key: CacheKey([0; 32]),
            },
            etag: StrongEtag::test_value("kept"),
        },
        length: 0,
    };
    worker
        .store
        .writer
        .index()
        .publish_version(metadata.clone())
        .unwrap();
    // A full removal snapshot also carries a new topology; retain both locally.
    let mut next = publication(&config, 2, vec![keep.clone()]);
    next.membership_version = MembershipVersion(2);
    next.members[0].peer_endpoint = "127.0.0.1:7555".into();
    *fixture.publication.lock().unwrap() = Some(next);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let until = Instant::now() + Duration::from_secs(10);
    while node.cache_cut.lock().unwrap().definitions.len() != 1 {
        runtime.reactor.poll_budgeted(64).unwrap();
        worker.poll_control(&mut cx).unwrap();
        runtime.reactor.wait(Duration::from_millis(1)).unwrap();
        assert!(Instant::now() < until);
    }
    assert_eq!(
        worker.snapshots.cursor().unwrap(),
        Some(wire::PublicationSequence(1))
    );
    fixture.stop.store(true, Ordering::Release);
    fixture.server.take().unwrap().join().unwrap();
    let polls = fixture.polls.load(Ordering::Acquire);
    while worker.snapshots.cursor().unwrap() != Some(wire::PublicationSequence(2)) {
        runtime.reactor.poll_budgeted(64).unwrap();
        worker.poll_budgeted(&mut cx, 64).unwrap();
        assert!(
            Instant::now() < until,
            "accepted publication depended on a second HTTP poll"
        );
    }
    assert_eq!(fixture.polls.load(Ordering::Acquire), polls);
    let current = worker.snapshots.current().unwrap();
    assert_eq!(current.membership.version, MembershipVersion(2));
    assert_eq!(
        current
            .membership
            .member(&config.node)
            .unwrap()
            .peer_endpoint,
        "127.0.0.1:7555"
    );
    assert!(worker.peer_task.is_some() && worker.diagnostic_task.is_some());
    assert!(worker.listener_scope.as_ref().unwrap().check().is_ok());
    assert!(worker.diagnostic_scope.as_ref().unwrap().check().is_ok());
    assert_eq!(
        worker
            .store
            .writer
            .index()
            .version(&metadata.version)
            .unwrap(),
        Some(metadata.clone())
    );
    // An unrelated cache still serves a pinned HEAD over its real owned UDS.
    let directory = std::fs::File::open(fixture.directory.join("sockets/keep/client")).unwrap();
    use std::os::fd::AsRawFd;
    let mut client = std::os::unix::net::UnixStream::connect(format!(
        "/proc/self/fd/{}/socket",
        directory.as_raw_fd()
    ))
    .unwrap();
    client.set_nonblocking(true).unwrap();
    client.write_all(format!("HEAD /v2/objects/{} HTTP/1.1\r\nHost: racer\r\nIf-Match: \"kept\"\r\nConnection: close\r\n\r\n", "0".repeat(64)).as_bytes()).unwrap();
    let mut response = Vec::new();
    while !response.windows(4).any(|w| w == b"\r\n\r\n") {
        runtime.reactor.poll_budgeted(64).unwrap();
        worker.poll_budgeted(&mut cx, 64).unwrap();
        let mut bytes = [0; 1024];
        match client.read(&mut bytes) {
            Ok(n) => response.extend_from_slice(&bytes[..n]),
            Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => (),
            Err(e) => panic!("{e}"),
        }
        assert!(Instant::now() < until, "unrelated cache stopped serving");
    }
    assert!(
        response.starts_with(b"HTTP/1.1 200"),
        "{}",
        String::from_utf8_lossy(&response)
    );
    let mut diagnostic = std::net::TcpStream::connect(config.diagnostics_listen).unwrap();
    diagnostic.set_nonblocking(true).unwrap();
    diagnostic
        .write_all(b"GET /healthz HTTP/1.1\r\nHost: racer\r\nConnection: close\r\n\r\n")
        .unwrap();
    let mut response = Vec::new();
    while !response.windows(4).any(|w| w == b"\r\n\r\n") {
        runtime.reactor.poll_budgeted(64).unwrap();
        worker.poll_budgeted(&mut cx, 64).unwrap();
        let mut bytes = [0; 1024];
        match diagnostic.read(&mut bytes) {
            Ok(n) => response.extend_from_slice(&bytes[..n]),
            Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => (),
            Err(e) => panic!("{e}"),
        }
        assert!(Instant::now() < until, "diagnostics stopped during removal");
    }
    assert!(response.starts_with(b"HTTP/1.1 200"));
    let shutdown = scope(Duration::from_secs(5)).unwrap();
    drive(&runtime, &mut engine, worker.drain(&shutdown)).unwrap();
    drive(&runtime, &mut engine, runtime.reactor.drain()).unwrap();
    drive(&runtime, &mut engine, worker.shutdown(&shutdown)).unwrap();
}

#[test]
fn startup_retries_tls_internal_error_before_enrollment_and_worker_snapshot() {
    let mut fixture = ControlFixture::new();
    fixture.handshake_alerts.lock().unwrap().push_back(80);
    let (config, node) = fixture.bootstrap_node(1, Duration::from_secs(15));
    assert_eq!(fixture.enrollments.load(Ordering::Acquire), 1);
    assert_eq!(fixture.polls.load(Ordering::Acquire), 0);
    fixture.handshake_alerts.lock().unwrap().push_back(80);
    let (mut worker, runtime, mut engine) = local_worker(&config, &node, 0);
    drive(
        &runtime,
        &mut engine,
        worker.start(&scope(Duration::from_secs(15)).unwrap()),
    )
    .unwrap();
    assert!(node.observations.health.ready());
    assert_eq!(fixture.enrollments.load(Ordering::Acquire), 2);
    assert!(fixture.polls.load(Ordering::Acquire) > 0);
    assert!(!fixture.poll_certificates.lock().unwrap().is_empty());
    let shutdown = scope(Duration::from_secs(5)).unwrap();
    drive(&runtime, &mut engine, worker.drain(&shutdown)).unwrap();
    drive(&runtime, &mut engine, worker.shutdown(&shutdown)).unwrap();
}

#[test]
fn startup_tls_authentication_alert_remains_terminal() {
    let mut fixture = ControlFixture::new();
    let config = fixture.config.take().unwrap();
    let node = NodeState::new(vec![WorkerId(0)], 64).unwrap();
    fixture.handshake_alerts.lock().unwrap().push_back(42);
    assert_eq!(
        bootstrap(
            &config,
            &node,
            &config.limits,
            &scope(Duration::from_secs(5)).unwrap()
        ),
        Err(Error::Unauthorized)
    );
    assert_eq!(fixture.enrollments.load(Ordering::Acquire), 0);
    assert_eq!(fixture.polls.load(Ordering::Acquire), 0);
    assert!(!config.identity_directory.join("identity.json").exists());
    assert!(!node.observations.health.ready());
}

#[test]
fn startup_tls_internal_error_respects_deadline() {
    let mut fixture = ControlFixture::new();
    let config = fixture.config.take().unwrap();
    let node = NodeState::new(vec![WorkerId(0)], 64).unwrap();
    fixture.handshake_alerts.lock().unwrap().extend([80; 16]);
    assert_eq!(
        bootstrap(
            &config,
            &node,
            &config.limits,
            &scope(Duration::from_millis(500)).unwrap()
        ),
        Err(Error::DeadlineExceeded)
    );
    assert_eq!(fixture.enrollments.load(Ordering::Acquire), 0);
    assert_eq!(fixture.polls.load(Ordering::Acquire), 0);
    assert!(!config.identity_directory.join("identity.json").exists());
    assert!(!node.observations.health.ready());
}

#[test]
fn startup_reauthenticates_retained_identity_and_fails_closed() {
    let mut fixture = ControlFixture::new();
    let config = fixture.config.take().unwrap();
    let start = || {
        let node = NodeState::new(vec![WorkerId(0)], 64).unwrap();
        bootstrap(
            &config,
            &node,
            &config.limits,
            &scope(Duration::from_secs(5)).unwrap(),
        )
    };
    let old = start().unwrap();
    let committed = std::fs::read(config.identity_directory.join("identity.json")).unwrap();
    let new = NodeId("99999999-9999-4999-8999-999999999999".into());
    *fixture.binding.lock().unwrap() = new.clone();
    fixture.bootstrap_status.store(403, Ordering::Release);
    assert_eq!(start(), Err(Error::Unauthorized));
    assert_eq!(
        std::fs::read(config.identity_directory.join("identity.json")).unwrap(),
        committed
    );
    assert!(config.identity_directory.join("pending.json").exists());
    assert_eq!(fixture.polls.load(Ordering::Acquire), 0);
    fixture.bootstrap_status.store(200, Ordering::Release);
    assert_eq!(start().unwrap(), new);
    assert_ne!(old, new);
    assert!(!config.identity_directory.join("pending.json").exists());
    assert_eq!(start().unwrap(), new);
    assert_eq!(fixture.enrollments.load(Ordering::Acquire), 3);
}

#[test]
fn node_replacement_drains_all_workers_and_restart_converges() {
    use crate::runtime::affinity::WorkerPair;
    use uring_runtime::affinity::EffectiveTopology;
    for renewal_due in [false, true] {
        let mut fixture = ControlFixture::new();
        let (config, node) = fixture.bootstrap_node(2, Duration::from_secs(15));
        if renewal_due {
            fixture
                .certificate_age
                .store(16 * 3600 + 60, Ordering::Release);
        }
        let app = Arc::new(Application {
            limits: config.limits.clone(),
            config: Arc::new(config),
            node: node.clone(),
            discovered_nics: vec![],
        });
        let cpu = EffectiveTopology::discover().unwrap().cpus[0].clone();
        let mut group = WorkerGroup::new(AffinityPlan {
            max_threads: 5,
            pairs: (0..2)
                .map(|id| WorkerPair {
                    worker: WorkerId(id),
                    io: cpu.clone(),
                    crypto: cpu.clone(),
                    nic: None,
                })
                .collect(),
        });
        group
            .start(app.clone(), &scope(Duration::from_secs(15)).unwrap())
            .unwrap();
        assert!(node.observations.health.ready());
        let new = NodeId("99999999-9999-4999-8999-999999999999".into());
        *fixture.binding.lock().unwrap() = new.clone();
        if !renewal_due {
            // Only explicit binding rejection forces early enrollment. A 503
            // can come from a lagging replica and must retain the current state.
            fixture.poll_status.store(403, Ordering::Release);
        }
        let until = Instant::now() + Duration::from_secs(15);
        while node.observations.health.ready() {
            assert!(
                Instant::now() < until,
                "replacement did not stop the worker graph"
            );
            thread::sleep(Duration::from_millis(1));
        }
        assert_eq!(group.join(), Err(Error::NodeIdentityChanged));
        assert!(!node.observations.health.ready());
        // The old shared signing epoch was never rebound, even after committing
        // the replacement to disk. Both workers completed their checkpoint cut.
        let old_keys = Keyring::new(
            app.config.cluster.clone(),
            app.config.node.clone(),
            node.keys.clone(),
        );
        assert_eq!(
            old_keys.signing_identity().unwrap().node(),
            &app.config.node
        );
        let images = crate::store::checkpoint::read_candidates(&app.config.slab_directory).unwrap();
        assert_eq!(images[0].1.shards.len(), 2);
        fixture.poll_status.store(200, Ordering::Release);
        fixture.certificate_age.store(1, Ordering::Release);
        let fresh = Arc::new(NodeState::new(vec![WorkerId(0)], 64).unwrap());
        drop(group);
        let app = Arc::try_unwrap(app).ok().unwrap();
        let mut config = Arc::try_unwrap(app.config).ok().unwrap();
        config.node = bootstrap(
            &config,
            &fresh,
            &config.limits,
            &scope(Duration::from_secs(15)).unwrap(),
        )
        .unwrap();
        assert_eq!(config.node, new);
        let (mut worker, runtime, mut engine) = local_worker(&config, &fresh, 0);
        drive(
            &runtime,
            &mut engine,
            worker.start(&scope(Duration::from_secs(15)).unwrap()),
        )
        .unwrap();
        assert!(fresh.observations.health.ready());
        assert_eq!(worker.keys.node(), &new);
        assert_eq!(
            worker
                .snapshots
                .current()
                .unwrap()
                .membership
                .member(&new)
                .unwrap()
                .node,
            new
        );
        stop_worker(&mut worker, &runtime, &mut engine);
    }
}

#[test]
fn two_worker_real_control_key_lease_drain_and_checkpoint_cut() {
    use crate::{runtime::affinity::WorkerPair, store::checkpoint};
    let mut fixture = ControlFixture::new();
    let (config, node) = fixture.bootstrap_node(2, Duration::from_secs(15));
    let keys = Keyring::new(
        config.cluster.clone(),
        config.node.clone(),
        node.keys.clone(),
    );
    let limits = config.limits.clone();
    let app = Arc::new(Application {
        config: Arc::new(config),
        node: node.clone(),
        limits,
        discovered_nics: vec![],
    });
    let cpu = EffectiveTopology::discover().unwrap().cpus[0].clone();
    let plan = AffinityPlan {
        max_threads: 5,
        pairs: (0..2)
            .map(|id| WorkerPair {
                worker: WorkerId(id),
                io: cpu.clone(),
                crypto: cpu.clone(),
                nic: None,
            })
            .collect(),
    };
    let mut group = WorkerGroup::new(plan);
    group
        .start(app.clone(), &scope(Duration::from_secs(15)).unwrap())
        .unwrap();
    assert_eq!(node.prepared.load(Ordering::Acquire), 2);
    assert!(node.observations.health.ready());
    let cache = crate::model::CacheId("33333333-3333-4333-8333-333333333333".into());
    let key = wire::CacheKeyRef {
        cache: cache.clone(),
        id: crate::model::key_id_from_generation(2, 8).unwrap(),
        purpose: wire::CacheKeyPurpose::Page,
    };
    let roots = (*keys.peer_trust_roots().unwrap()).clone();
    keys.install(wire::KeyringBundle {
        schema_version: 1,
        cluster: app.config.cluster.clone(),
        generation: wire::BundleGeneration(2),
        peer_trust_roots: roots.clone(),
        cache_keys: vec![wire::CacheEncryptionKey::new(
            key.clone(),
            wire::CacheKeyState::Active,
            zeroize::Zeroizing::new([21; 32]),
        )],
    })
    .unwrap();
    let lease = keys.lease(Some(&cache), key.id, KeyPurpose::Page).unwrap();
    keys.install(wire::KeyringBundle {
        schema_version: 1,
        cluster: app.config.cluster.clone(),
        generation: wire::BundleGeneration(3),
        peer_trust_roots: roots,
        cache_keys: vec![],
    })
    .unwrap();
    let until = Instant::now() + Duration::from_secs(10);
    assert!(keys.lease(Some(&cache), key.id, KeyPurpose::Page).is_err());
    crate::security::test_support::assert_page_key(&lease, &[21; 32]);
    assert!(node.observations.health.ready());
    drop(lease);
    while !node.observations.health.ready() {
        assert!(
            Instant::now() < until,
            "two-worker readiness did not progress"
        );
        thread::sleep(Duration::from_millis(1));
    }
    assert!(keys.active(&cache, KeyPurpose::Page).is_err());
    let shutdown = scope(Duration::from_secs(10)).unwrap();
    group.drain(&shutdown).unwrap();
    group.shutdown(&shutdown).unwrap();
    group.join().unwrap();
    let bytes = std::fs::read(fixture.directory.join("slabs/checkpoint.0")).unwrap();
    let image = checkpoint::decode(&bytes).unwrap();
    let mut workers: Vec<_> = image.shards.iter().map(|shard| shard.worker.0).collect();
    workers.sort_unstable();
    assert_eq!(workers, vec![0, 1]);
    assert!(!node.observations.health.ready());
}
fn stop_worker(
    worker: &mut WorkerApplication,
    runtime: &WorkerRuntime,
    engine: &mut dyn CryptoService,
) {
    drive(
        runtime,
        engine,
        worker.drain(&scope(Duration::from_secs(5)).unwrap()),
    )
    .unwrap();
    drive(runtime, engine, runtime.reactor.drain()).unwrap();
    drive(
        runtime,
        engine,
        worker.shutdown(&scope(Duration::from_secs(5)).unwrap()),
    )
    .unwrap();
}

fn drive<T>(
    runtime: &WorkerRuntime,
    engine: &mut dyn CryptoService,
    future: Operation<'_, T>,
) -> Result<T> {
    let mut future = future;
    let until = Instant::now() + Duration::from_secs(15);
    loop {
        runtime.reactor.poll_budgeted(64)?;
        engine.poll_budgeted(64)?;
        runtime.crypto.poll_budgeted(64)?;
        if let Poll::Ready(result) = future
            .as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
        {
            return result;
        }
        assert!(
            Instant::now() < until,
            "application lifecycle did not progress"
        );
        runtime.reactor.wait(Duration::from_millis(1))?;
    }
}
#[test]
fn blocked_publication_is_superseded_while_projection_rotates() {
    let mut fixture = ControlFixture::new();
    let (config, node) = fixture.bootstrap_node(1, Duration::from_secs(15));
    let (mut worker, runtime, mut engine) = local_worker(&config, &node, 0);
    drive(
        &runtime,
        &mut engine,
        worker.start(&scope(Duration::from_secs(15)).unwrap()),
    )
    .unwrap();
    let control = worker.control.clone().unwrap();
    // Drive control without polling worker preparation. The real rendezvous must
    // keep publications pending until the worker prepares their resources.
    // This test drives key delivery explicitly below rather than the worker task.
    worker.keyring_task.take();
    *fixture.publication.lock().unwrap() = Some(publication(&config, 2, vec![definition()]));
    drive(
        &runtime,
        &mut engine,
        control.progress(&scope(Duration::from_secs(5)).unwrap()),
    )
    .unwrap();
    assert_eq!(
        worker.snapshots.cursor().unwrap(),
        Some(wire::PublicationSequence(1))
    );
    {
        let mut bundle = fixture.bundle.lock().unwrap();
        bundle.generation = wire::BundleGeneration(2);
        bundle.cache_keys.clear();
    }
    drive(
        &runtime,
        &mut engine,
        control.keyring_progress(&scope(Duration::from_secs(5)).unwrap()),
    )
    .unwrap();
    *fixture.publication.lock().unwrap() = Some(publication(&config, 3, vec![]));
    drive(
        &runtime,
        &mut engine,
        control.progress(&scope(Duration::from_secs(5)).unwrap()),
    )
    .unwrap();
    assert!(
        worker
            .keys
            .active(&definition().id, KeyPurpose::Page)
            .is_err()
    );
    assert_eq!(control.projection_error(), None);
    assert_eq!(
        worker.snapshots.cursor().unwrap(),
        Some(wire::PublicationSequence(1))
    );
    // Resume worker preparation. Only the newer publication may commit.
    let until = Instant::now() + Duration::from_secs(5);
    while worker.snapshots.cursor().unwrap() != Some(wire::PublicationSequence(3)) {
        assert!(Instant::now() < until);
        runtime.reactor.poll_budgeted(64).unwrap();
        worker
            .poll_budgeted(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                64,
            )
            .unwrap();
        engine.poll_budgeted(64).unwrap();
    }
    assert!(worker.snapshots.current().unwrap().caches.is_empty());
    stop_worker(&mut worker, &runtime, &mut engine);
}

#[test]
fn network_keyring_bootstrap_rotation_recovery_and_failure_retention() {
    let mut fixture = ControlFixture::new();
    let mut config = fixture.config.take().unwrap();
    let node = Arc::new(NodeState::new(vec![WorkerId(0)], 64).unwrap());
    {
        let mut bundle = fixture.bundle.lock().unwrap();
        *bundle =
            crate::security::test_support::rotation_bundle(1, bundle.peer_trust_roots.clone());
        bundle.cluster = config.cluster.clone();
    }
    config.node = bootstrap(
        &config,
        &node,
        &config.limits,
        &scope(Duration::from_secs(15)).unwrap(),
    )
    .unwrap();
    assert_eq!(
        *fixture.keyring_tokens.lock().unwrap(),
        vec!["fixture.token"]
    );
    assert!(!fixture.directory.join("secrets").exists());
    let (mut worker, runtime, mut engine) = local_worker(&config, &node, 0);
    drive(
        &runtime,
        &mut engine,
        worker.start(&scope(Duration::from_secs(15)).unwrap()),
    )
    .unwrap();
    worker.keyring_task.take();
    let control = worker.control.clone().unwrap();
    let cache = fixture.bundle.lock().unwrap().cache_keys[0]
        .key
        .cache
        .clone();
    let lease = worker.keys.active(&cache, KeyPurpose::Page).unwrap();
    let old_id = lease.id();
    let mut old_sealed = [0; 19];
    lease
        .seal_page(&cache, &[1; 24], b"retained", b"abc", &mut old_sealed)
        .unwrap();
    fixture.hold_long_poll.store(true, Ordering::Release);
    let poll_scope = scope(Duration::from_secs(30)).unwrap();
    let mut topology = control.progress(&poll_scope);
    let key_scope = scope(Duration::from_secs(15)).unwrap();
    {
        let mut bundle = fixture.bundle.lock().unwrap();
        *bundle =
            crate::security::test_support::rotation_bundle(2, bundle.peer_trust_roots.clone());
        bundle.cluster = config.cluster.clone();
    }
    let mut rotation = control.keyring_progress(&key_scope);
    drive(
        &runtime,
        &mut engine,
        Box::pin(std::future::poll_fn(|cx| {
            assert!(topology.as_mut().poll(cx).is_pending());
            rotation.as_mut().poll(cx)
        })),
    )
    .unwrap();
    assert_eq!(control.projection_error(), None);
    assert_ne!(
        worker.keys.active(&cache, KeyPurpose::Page).unwrap().id(),
        old_id
    );
    assert!(
        worker
            .keys
            .lease(Some(&cache), old_id, KeyPurpose::Page)
            .is_err()
    );
    let mut opened = [0; 3];
    lease
        .open_page(
            &cache,
            old_id,
            &[1; 24],
            b"retained",
            &old_sealed,
            &mut opened,
        )
        .unwrap();
    assert_eq!(&opened, b"abc");
    drop(topology);
    fixture.hold_long_poll.store(false, Ordering::Release);
    let accepted = worker.keys.active(&cache, KeyPurpose::Page).unwrap().id();
    for response in [
        (200, b"{}".to_vec()),
        (200, vec![b' '; wire::MAX_BUNDLE_BYTES + 1]),
        (0, Vec::new()),
        (409, br#"{"code":"conflict"}"#.to_vec()),
    ] {
        *fixture.keyring_override.lock().unwrap() = Some(response);
        drive(
            &runtime,
            &mut engine,
            control.keyring_progress(&scope(Duration::from_secs(40)).unwrap()),
        )
        .unwrap();
        assert!(control.projection_error().is_some());
        assert_eq!(
            worker.keys.active(&cache, KeyPurpose::Page).unwrap().id(),
            accepted
        );
        *fixture.keyring_override.lock().unwrap() = None;
        drive(
            &runtime,
            &mut engine,
            control.keyring_progress(&scope(Duration::from_secs(10)).unwrap()),
        )
        .unwrap();
        assert_eq!(control.projection_error(), None);
    }
    *fixture.keyring_override.lock().unwrap() = None;
    fixture.reject_keyring_mtls.store(true, Ordering::Release);
    std::fs::write(&config.service_account_token, b"fixture.token.fresh").unwrap();
    {
        let mut bundle = fixture.bundle.lock().unwrap();
        *bundle =
            crate::security::test_support::rotation_bundle(3, bundle.peer_trust_roots.clone());
        bundle.cluster = config.cluster.clone();
    }
    drive(
        &runtime,
        &mut engine,
        control.keyring_progress(&scope(Duration::from_secs(40)).unwrap()),
    )
    .unwrap();
    assert_eq!(control.projection_error(), None);
    assert_eq!(
        fixture.keyring_tokens.lock().unwrap().last().unwrap(),
        "fixture.token.fresh"
    );
    assert_ne!(
        worker.keys.active(&cache, KeyPurpose::Page).unwrap().id(),
        accepted
    );
    fixture.reject_keyring_mtls.store(false, Ordering::Release);
    fixture.handshake_alerts.lock().unwrap().push_back(42); // bad_certificate
    std::fs::write(&config.service_account_token, b"fixture.token.newer").unwrap();
    drive(
        &runtime,
        &mut engine,
        control.keyring_progress(&scope(Duration::from_secs(10)).unwrap()),
    )
    .unwrap();
    assert_eq!(control.projection_error(), None);
    assert_eq!(
        fixture.keyring_tokens.lock().unwrap().last().unwrap(),
        "fixture.token.newer"
    );
    let tokens = fixture.keyring_tokens.lock().unwrap().len();
    let untrusted = rcgen::generate_simple_self_signed(vec!["untrusted.invalid".into()])
        .unwrap()
        .cert;
    std::fs::write(&config.trust_bundle, untrusted.pem()).unwrap();
    drive(
        &runtime,
        &mut engine,
        control.keyring_progress(&scope(Duration::from_secs(10)).unwrap()),
    )
    .unwrap();
    assert_eq!(control.projection_error(), Some(Error::Unauthorized));
    assert_eq!(
        fixture.keyring_tokens.lock().unwrap().len(),
        tokens,
        "never disclose token to an untrusted TLS endpoint"
    );
    assert!(!fixture.directory.join("secrets").exists());
}

#[test]
fn real_control_bootstrap_recovery_publication_readiness_and_shutdown() {
    let mut fixture = ControlFixture::new();
    let mut config = fixture.config.take().unwrap();
    let node = Arc::new(NodeState::new(vec![WorkerId(0)], 64).unwrap());
    let startup = scope(Duration::from_secs(15)).unwrap();
    config.node = bootstrap(&config, &node, &config.limits, &startup).unwrap();
    assert_eq!(fixture.enrollments.load(Ordering::Acquire), 1);
    let (mut worker, runtime, mut engine) = local_worker(&config, &node, 0);
    let startup = scope(Duration::from_secs(15)).unwrap();
    drive(&runtime, &mut engine, worker.start(&startup)).unwrap();
    assert!(node.observations.health.ready());
    assert!(fixture.polls.load(Ordering::Acquire) >= 1);
    assert_eq!(
        worker.snapshots.current().unwrap().sequence,
        racer_control_wire::PublicationSequence(1)
    );
    assert_eq!(
        fixture.enrollments.load(Ordering::Acquire),
        2,
        "worker reauthenticates the current binding before serving"
    );
    for _ in 0..8 {
        runtime.reactor.poll_budgeted(64).unwrap();
        worker
            .poll_budgeted(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                64,
            )
            .unwrap();
    }
    // Retire an omitted epoch through the actual serving loop. Accepted crypto
    // retains its key and buffers, without pausing listeners or deleting checkpoints.
    let cache = crate::model::CacheId("33333333-3333-4333-8333-333333333333".into());
    let reference = wire::CacheKeyRef {
        cache: cache.clone(),
        id: crate::model::key_id_from_generation(2, 7).unwrap(),
        purpose: wire::CacheKeyPurpose::Page,
    };
    let roots = (*worker.keys.peer_trust_roots().unwrap()).clone();
    worker
        .keys
        .install(wire::KeyringBundle {
            schema_version: 1,
            cluster: config.cluster.clone(),
            generation: wire::BundleGeneration(2),
            peer_trust_roots: roots.clone(),
            cache_keys: vec![wire::CacheEncryptionKey::new(
                reference.clone(),
                wire::CacheKeyState::Active,
                zeroize::Zeroizing::new([19; 32]),
            )],
        })
        .unwrap();
    let lease = worker
        .keys
        .lease(Some(&cache), reference.id, KeyPurpose::Page)
        .unwrap();
    let page = crate::model::PageId {
        version: crate::model::ObjectVersion {
            object: crate::model::ObjectId {
                cache: cache.clone(),
                key: crate::model::CacheKey([4; 32]),
            },
            etag: crate::model::StrongEtag::test_value("accepted"),
        },
        number: crate::model::PageNumber(0),
    };
    let buffers = BufferPool::new(runtime.admission.clone());
    let plaintext = buffers
        .plaintext(
            runtime
                .admission
                .reserve(Some(&cache), crate::model::ResourceClass::Plaintext, 8)
                .unwrap(),
            8,
        )
        .unwrap();
    let ciphertext = runtime
        .admission
        .reserve(Some(&cache), crate::model::ResourceClass::Ciphertext, 24)
        .unwrap();
    let accepted_scope = scope(Duration::from_secs(10)).unwrap();
    let mut accepted = runtime.crypto.execute(
        crate::runtime::crypto::CryptoInput::Encrypt {
            page,
            plaintext,
            ciphertext,
        },
        worker
            .keys
            .lease(Some(&cache), reference.id, KeyPurpose::Page)
            .unwrap(),
        &accepted_scope,
    );
    assert!(
        accepted
            .as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
            .is_pending()
    );
    assert_eq!(runtime.crypto.outstanding(), 1);
    drop(accepted);
    accepted_scope.cancel().unwrap();
    let shard = futures::executor::block_on(worker.store.checkpoint.snapshot_shard()).unwrap();
    futures::executor::block_on(worker.store.checkpoint.publish(vec![shard])).unwrap();
    worker.store.checkpoint.finish_snapshot();
    assert!(fixture.directory.join("slabs/checkpoint.0").is_file());
    let shard = futures::executor::block_on(worker.store.checkpoint.snapshot_shard()).unwrap();
    futures::executor::block_on(worker.store.checkpoint.publish(vec![shard])).unwrap();
    worker.store.checkpoint.finish_snapshot();
    assert!(fixture.directory.join("slabs/checkpoint.1").is_file());
    worker
        .keys
        .install(wire::KeyringBundle {
            schema_version: 1,
            cluster: config.cluster.clone(),
            generation: wire::BundleGeneration(3),
            peer_trust_roots: roots,
            cache_keys: vec![],
        })
        .unwrap();
    let until = Instant::now() + Duration::from_secs(5);
    for _ in 0..8 {
        runtime.reactor.poll_budgeted(64).unwrap();
        worker
            .poll_budgeted(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                64,
            )
            .unwrap();
    }
    assert!(
        fixture.directory.join("slabs/checkpoint.0").is_file(),
        "historical checkpoints remain disposable cache"
    );
    assert_eq!(runtime.crypto.outstanding(), 1);
    assert!(worker.peer_task.is_some());
    assert!(worker.diagnostic_task.is_some());
    crate::security::test_support::assert_page_key(&lease, &[19; 32]);
    assert!(
        worker
            .keys
            .lease(Some(&cache), reference.id, KeyPurpose::Page)
            .is_err()
    );
    while runtime.crypto.outstanding() != 0 {
        engine.poll_budgeted(64).unwrap();
        runtime.crypto.poll_budgeted(64).unwrap();
        runtime.reactor.poll_budgeted(64).unwrap();
        worker
            .poll_budgeted(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                64,
            )
            .unwrap();
        assert!(Instant::now() < until, "accepted crypto completion stalled");
    }
    // Continued serving does not need an invalidation CQE or final key release.
    for _ in 0..4 {
        worker
            .poll_budgeted(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                64,
            )
            .unwrap();
        assert!(worker.peer_task.is_some());
        assert!(worker.diagnostic_task.is_some());
    }
    assert!(fixture.directory.join("slabs/checkpoint.0").exists());
    assert!(fixture.directory.join("slabs/checkpoint.1").exists());
    assert!(worker.keys.active(&cache, KeyPurpose::Page).is_err());
    let late = self::page(&worker);
    assert!(worker.memory.publish(late.clone()).is_err());
    let dirty = runtime
        .admission
        .reserve(
            Some(&cache),
            crate::model::ResourceClass::DirtyCiphertext,
            19,
        )
        .unwrap();
    assert!(matches!(
        worker.store.writer.enqueue(late.copy(), dirty),
        Err(Error::MissingKey)
    ));
    assert_eq!(runtime.crypto.outstanding(), 0);
    drop(lease);
    runtime.reactor.poll_budgeted(64).unwrap();
    worker
        .poll_budgeted(
            &mut Context::from_waker(futures::task::noop_waker_ref()),
            64,
        )
        .unwrap();
    assert!(node.observations.health.ready());
    let shutdown = scope(Duration::from_secs(5)).unwrap();
    drive(&runtime, &mut engine, worker.drain(&shutdown)).unwrap();
    assert!(!node.observations.health.ready());
    drive(&runtime, &mut engine, runtime.reactor.drain()).unwrap();
    drive(&runtime, &mut engine, worker.shutdown(&shutdown)).unwrap();
    assert_eq!(runtime.reactor.in_flight(), 0);
    assert_eq!(runtime.crypto.outstanding(), 0);
    assert!(fixture.directory.join("slabs/checkpoint.0").is_file());
}
use uring_runtime::affinity::EffectiveTopology;
