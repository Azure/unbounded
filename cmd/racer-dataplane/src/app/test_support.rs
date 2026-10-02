//! Shared application fixtures. Scenarios import these directly, not through other tests.
use super::*;
use crate::control::wire;
use std::{
    io::{Read, Write},
    path::PathBuf,
    thread,
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
    pub publication: Arc<Mutex<Option<wire::Publication>>>,
    pub bootstrap_requests: Arc<Mutex<Vec<wire::EnrollmentRequest>>>,
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
    requests: Arc<Mutex<Vec<wire::EnrollmentRequest>>>,
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
    initial: wire::Publication,
    published: Arc<Mutex<Option<wire::Publication>>>,
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
        let request = wire::decode_enrollment_request(body).unwrap();
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
            (200, wire::encode_publication(&publication).unwrap())
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
) -> wire::Publication {
    wire::Publication {
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
    use crate::memory::pool::{VerifiedBytes, VerifiedPage};
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
                    .active(
                        &definition().id,
                        crate::security::identity::KeyPurpose::Page,
                    )
                    .map(|key| key.id())
                    .unwrap_or_else(|_| KeyId::from_generation(2, 7).unwrap()),
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
