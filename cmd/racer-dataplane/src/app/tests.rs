//! Application lifecycle tests and shared assembled-worker fixtures.

use super::*;
use crate::admission::AdmissionPolicy;
use crate::admission::ResourceClass;
use crate::admission::reserve_connection;
use crate::http::Codec;
use crate::model::ExpiresAt;
use crate::model::MetadataSelector;
use crate::model::ObjectMetadata;
use crate::model::*;
use crate::peer::protocol;
use crate::peer::protocol as connection;
use crate::peer::protocol::FetchMode;
use crate::peer::protocol::Operation as PeerOperation;
use crate::peer::protocol::PeerRequest;
use crate::peer::protocol::PeerResponse;
use crate::peer::protocol::decode_envelope;
use crate::peer::protocol::encode_envelope;
use crate::runtime::Reactor;
use crate::security;
use crate::security::CryptoClient;
use crate::test_support::security::network;
use crate::test_support::security::node;
use crate::test_support::wake_test_worker;
use crate::topology::RouteBudget;
use http1::Header;
use http1::MessageHead;
use http1::StartLine;
use racer_control_wire as state;
use racer_control_wire as wire;
use racer_control_wire::CacheId;
use racer_control_wire::MembershipVersion;
use std::collections::VecDeque;
use std::future::Future;
use std::io::Read;
use std::io::Write;
use std::num::NonZeroUsize;
use std::os::unix::net::UnixStream;
use std::path::PathBuf;
use std::thread;
use std::time::Instant;
#[cfg(test)]
use uring_runtime::group::Service;
use uring_runtime::group::affinity::EffectiveTopology;

/// Controllable TLS endpoints and observations for application lifecycle tests.
pub(super) struct ControlFixture {
    pub bundle: Arc<Mutex<wire::KeyringBundle>>,

    // Keep the optional status/body override explicit without a fixture-only alias.
    #[allow(clippy::type_complexity)]
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

    pub hold_renewal: Arc<AtomicBool>,

    pub held_renewals: Arc<AtomicUsize>,

    /// Delay enrollment handler entry independently of publication handlers.
    pub pause_renewal: Arc<AtomicBool>,

    pub publication_retry_after: Arc<AtomicUsize>,

    pub publication_failures: Arc<AtomicUsize>,
}

/// Each accepted TLS socket gets the same independently controllable endpoints.
#[derive(Clone)]
struct ControlHandlers {
    enrollment: EnrollmentHandler,

    keyring: KeyringHandler,

    publication: PublicationHandler,
}

/// Certificate issuance controls and captured requests shared by fixture sockets.
#[derive(Clone)]
struct EnrollmentHandler {
    ca: Arc<rcgen::Certificate>,

    ca_key: Arc<rcgen::KeyPair>,

    binding: Arc<Mutex<NodeId>>,

    status: Arc<AtomicUsize>,

    age: Arc<AtomicUsize>,

    issued: Arc<AtomicUsize>,

    requests: Arc<Mutex<Vec<state::EnrollmentRequest>>>,

    held: Arc<AtomicBool>,

    waiting: Arc<AtomicUsize>,

    paused: Arc<AtomicBool>,

    stopping: Arc<AtomicBool>,
}

/// Shared key response and authentication controls for each accepted socket.
#[derive(Clone)]
struct KeyringHandler {
    bundle: Arc<Mutex<wire::KeyringBundle>>,

    // Match the fixture's optional wire status/body override directly.
    #[allow(clippy::type_complexity)]
    response: Arc<Mutex<Option<(usize, Vec<u8>)>>>,

    tokens: Arc<Mutex<Vec<String>>>,

    reject_mtls: Arc<AtomicBool>,
}

/// Publication responses and long-poll controls shared by fixture sockets.
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

    retry_after: Arc<AtomicUsize>,

    failures: Arc<AtomicUsize>,
}

/// Blocking server-side TLS stream used by the fixture handlers.
type ControlStream = rustls::StreamOwned<rustls::ServerConnection, std::net::TcpStream>;

/// Encode the fixture's supported structured failure statuses.
fn error_response(status: usize) -> (usize, Vec<u8>) {
    let code = if status == 503 {
        "unavailable"
    } else {
        "forbidden"
    };
    (status, format!("{{\"code\":\"{code}\"}}").into_bytes())
}

impl EnrollmentHandler {
    /// Validate the request and issue a certificate after any configured hold.
    fn respond(&self, head: &str, body: &[u8], stream: &ControlStream) -> (usize, Vec<u8>) {
        assert!(head.contains("Authorization: Bearer fixture.token"));
        assert!(stream.conn.peer_certificates().is_none());
        let request = state::decode_enrollment_request(body).unwrap();
        let until = Instant::now() + Duration::from_secs(10);
        while self.paused.load(Ordering::Acquire) && !self.stopping.load(Ordering::Acquire) {
            assert!(Instant::now() < until, "renewal handler entry not released");
            thread::sleep(Duration::from_millis(1));
        }
        self.requests.lock().unwrap().push(request.clone());
        if self.held.load(Ordering::Acquire) {
            self.waiting.fetch_add(1, Ordering::Release);
            let until = Instant::now() + Duration::from_secs(10);
            while self.held.load(Ordering::Acquire) && !self.stopping.load(Ordering::Acquire) {
                assert!(Instant::now() < until, "renewal fixture not released");
                thread::sleep(Duration::from_millis(1));
            }
        }
        let status = self.status.load(Ordering::Acquire);
        if status != 200 {
            return error_response(status);
        }
        let node = self.binding.lock().unwrap().clone();
        let not_before = std::time::SystemTime::now()
            - Duration::from_secs(self.age.load(Ordering::Acquire) as u64);
        self.issued.fetch_add(1, Ordering::Release);
        let mut response = crate::test_support::enrollment::issue_at(
            &request,
            &self.ca,
            &self.ca_key,
            &node.0,
            not_before,
        );
        response.block_devices = Some("nvme-cache".into());
        (200, wire::encode_enrollment_response(&response).unwrap())
    }
}

impl KeyringHandler {
    /// Return the configured keyring response or close the socket on demand.
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
    /// Record client authentication and serve the current publication or long poll.
    fn respond(&self, head: &str, stream: &ControlStream) -> (usize, Vec<u8>) {
        let status = self.status.load(Ordering::Acquire);
        if status != 200 {
            self.failures.fetch_add(1, Ordering::Release);
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
    /// Read and dispatch one HTTP request over an accepted TLS socket.
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
        let retry = if head.starts_with("GET /v1/snapshot") && status == 503 {
            format!(
                "Retry-After: {}\r\n",
                self.publication.retry_after.load(Ordering::Acquire)
            )
        } else {
            String::new()
        };
        let response = format!(
            "HTTP/1.1 {status} Result\r\n{retry}Content-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
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
        config.node = bootstrap(&config, &node, &config.limits, &scope(timeout).unwrap())
            .unwrap()
            .0;
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
            hold_renewal: Arc::new(AtomicBool::new(false)),
            held_renewals: Arc::new(AtomicUsize::new(0)),
            pause_renewal: Arc::new(AtomicBool::new(false)),
            publication_retry_after: Arc::new(AtomicUsize::new(0)),
            publication_failures: Arc::new(AtomicUsize::new(0)),
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
                held: fixture.hold_renewal.clone(),
                waiting: fixture.held_renewals.clone(),
                paused: fixture.pause_renewal.clone(),
                stopping: fixture.stop.clone(),
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
                retry_after: fixture.publication_retry_after.clone(),
                failures: fixture.publication_failures.clone(),
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
    let (ca, ca_key) = crate::test_support::enrollment::ca();
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
    discovered_nics: Vec<racer_control_wire::RailMapping>,
) -> (WorkerApplication, WorkerRuntime, PageCryptoEngine) {
    let worker = WorkerId(id);
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        config.limits.clone(),
    )));
    let (io, engine) = crate::security::pair(worker, 0, config.limits.queue_entries);
    let runtime = WorkerRuntime {
        reactor: Rc::new(Reactor::new(admission.clone())),
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
        PageCryptoEngine::new(CryptoRuntime { port: engine }),
    )
}

pub(super) fn definition() -> racer_control_wire::CacheDefinition {
    let (client_socket, origin_socket) =
        racer_control_wire::canonical_socket_paths("app-lifecycle").unwrap();
    racer_control_wire::CacheDefinition {
        id: racer_control_wire::CacheId("33333333-3333-4333-8333-333333333333".into()),
        name: "app-lifecycle".into(),
        client_socket,
        origin_socket,
    }
}

pub(super) fn publication(
    config: &Config,
    sequence: u64,
    caches: Vec<racer_control_wire::CacheDefinition>,
) -> state::Publication {
    state::Publication {
        schema_version: 1,
        cluster: config.cluster.clone(),
        sequence: wire::PublicationSequence(sequence),
        membership_version: racer_control_wire::MembershipVersion(1),
        members: vec![racer_control_wire::Member {
            node: config.node.clone(),
            shares: std::num::NonZeroU32::new(1).unwrap(),
            peer_endpoint: "127.0.0.1:7443".into(),
            rails: vec![],
            site: String::new(),
        }],
        caches,
    }
}

pub(super) fn page(app: &WorkerApplication) -> crate::memory::PageResult {
    use crate::admission::ResourceClass;
    use crate::memory::VerifiedBytes;
    use crate::memory::VerifiedPage;
    use crate::model::VersionMetadata;
    use crate::model::*;
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
            storage: flow_control::ChargedBytes {
                bytes: vec![1; 3],
                reservation: app
                    .runtime
                    .admission
                    .reserve(Some(cache), ResourceClass::Plaintext, 3)
                    .unwrap(),
            },
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
                    .active(&definition().id, racer_crypto::identity::KeyPurpose::Page)
                    .map(|key| key.id())
                    .unwrap_or_else(|_| crate::model::key_id_from_generation(2, 7).unwrap()),
                nonce: Nonce([2; 24]),
                plaintext_length: 3,
                ciphertext_length: 19,
            },
            vec![2; 19],
        )
        .unwrap();
    crate::memory::PageResult {
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
        "placement_cache_bytes",
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
            "placement_cache_bytes" => &mut limits.placement_cache_bytes,
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
fn topology_budget_partition_preserves_bytes_and_per_worker_search_limit() {
    let mut limits = Config::from_lookup(|name| {
        Ok(match name {
            "RACER_CLUSTER_ID" => Some("00000000-0000-4000-8000-000000000001".into()),
            "RACER_CONTROL_ENDPOINT" => Some("https://control.example".into()),
            "RACER_ENABLE_RDMA" => Some("false".into()),
            _ => None,
        })
    })
    .unwrap()
    .limits;
    limits.placement_cache_bytes = NonZeroUsize::new(2049).unwrap();
    limits.path_cache_bytes = NonZeroUsize::new(8193).unwrap();
    let partition = partition_limits(&limits, 2, false).unwrap();
    assert_eq!(partition.placement_cache_bytes.get(), 1024);
    assert_eq!(partition.path_cache_bytes.get(), 4096);
    assert_eq!(partition.active_path_searches, limits.active_path_searches);
    limits.placement_cache_bytes = NonZeroUsize::new(2047).unwrap();
    assert_eq!(
        partition_limits_with_cause(&limits, 2, false).err(),
        Some(("placement_cache_bytes", Error::InvalidConfiguration))
    );
}

#[test]
fn worker_sizing_funds_diagnostics_and_ordinary_progress() {
    use uring_runtime::group::affinity::CpuLocation;
    use uring_runtime::reactor::simulation::Simulation;
    let config = Config::from_lookup(|name| {
        Ok(match name {
            "RACER_CLUSTER_ID" => Some("00000000-0000-4000-8000-000000000001".into()),
            "RACER_CONTROL_ENDPOINT" => Some("https://control.example".into()),
            "RACER_ENABLE_RDMA" => Some("false".into()),
            "RACER_QUEUE_ENTRIES" => Some("16".into()),
            _ => None,
        })
    })
    .unwrap();
    let mut plan = AffinityPlan::from_topology(
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
    .unwrap();
    assert_eq!(plan.pairs.len(), 5);
    for workers in 3..=5 {
        assert_eq!(
            partition_limits_with_cause(&config.limits, workers, false).err(),
            Some(("queue_entries", Error::InvalidConfiguration))
        );
    }
    let limits = size_workers(&config.limits, &mut plan, false).unwrap();
    assert_eq!(plan.pairs.len(), 2);
    assert_eq!(limits.queue_entries.get(), 8);

    let minimum = crate::telemetry::CONTROL_SLOTS + 2;
    for entries in 2..=minimum + 1 {
        let mut node = config.limits.clone();
        node.queue_entries = NonZeroUsize::new(entries).unwrap();
        let result = partition_limits_with_cause(&node, 1, false);
        if entries < minimum {
            assert_eq!(
                result.err(),
                Some(("queue_entries", Error::InvalidConfiguration)),
                "queue_entries={entries}"
            );
            let mut rejected_plan = AffinityPlan {
                pairs: plan.pairs.clone(),
                max_threads: plan.max_threads,
            };
            assert!(matches!(
                size_workers(&node, &mut rejected_plan, false),
                Err(Error::InvalidConfiguration)
            ));
            continue;
        }

        let sim = Simulation::new();
        let _environment = sim.enter();
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            result.unwrap(),
        )));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        reactor.init().unwrap();
        let baseline = admission.used(ResourceClass::RequestContext);
        let diagnostics =
            crate::telemetry::DiagnosticIo::attach(reactor.clone(), admission.clone()).unwrap();
        assert_eq!(
            admission.used(ResourceClass::ControlProgress),
            crate::telemetry::CONTROL_SLOTS
        );
        assert_eq!(
            admission.used(ResourceClass::RequestContext),
            baseline + crate::telemetry::RESERVED_BYTES
        );
        let ordinary = admission
            .reserve(None, ResourceClass::ControlProgress, 2)
            .unwrap();
        let (reader, _writer) = sim.socket_pair();
        let reader = Rc::new(reader);
        let scope = scope(Duration::from_secs(30)).unwrap();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut pending = Vec::new();
        for _ in 0..entries - crate::telemetry::CONTROL_SLOTS {
            let mut wait = reactor.readiness(reader.clone(), libc::POLLIN as u32, &scope);
            assert!(wait.as_mut().poll(&mut cx).is_pending());
            pending.push(wait);
        }
        let mut excess = reactor.readiness(reader, libc::POLLIN as u32, &scope);
        assert!(matches!(
            excess.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Overloaded))
        ));
        drop((excess, pending));
        drive_peer(&reactor, reactor.drain()).unwrap();
        drop((ordinary, diagnostics));
        assert_eq!(admission.used(ResourceClass::ControlProgress), 0);
        assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
        drop(reactor);
        assert_eq!(admission.used(ResourceClass::RequestContext), 0);
    }
}

#[test]
fn worker_sizing_rejects_four_queue_entries_with_two_threads() {
    use uring_runtime::group::affinity::CpuLocation;
    let config = Config::from_lookup(|name| {
        Ok(match name {
            "RACER_CLUSTER_ID" => Some("00000000-0000-4000-8000-000000000001".into()),
            "RACER_CONTROL_ENDPOINT" => Some("https://control.example".into()),
            "RACER_ENABLE_RDMA" => Some("false".into()),
            "RACER_QUEUE_ENTRIES" => Some("4".into()),
            "RACER_MAX_THREADS" => Some("2".into()),
            _ => None,
        })
    })
    .unwrap();
    let mut plan = AffinityPlan::from_topology(
        &config,
        EffectiveTopology {
            cpus: (0..8)
                .map(|cpu| CpuLocation {
                    cpu,
                    package: 0,
                    core: cpu,
                    numa_node: None,
                })
                .collect(),
            quota: None,
            nics: vec![],
        },
        &[],
    )
    .unwrap();
    assert_eq!(plan.pairs.len(), 1);
    assert_eq!(
        partition_limits_with_cause(&config.limits, 1, false).err(),
        Some(("queue_entries", Error::InvalidConfiguration))
    );
    assert!(matches!(
        size_workers(&config.limits, &mut plan, false),
        Err(Error::InvalidConfiguration)
    ));
}

#[test]
fn worker_sizing_funds_derived_connection_pools() {
    use crate::admission::ResourceClass;
    use uring_runtime::group::affinity::CpuLocation;
    use uring_runtime::group::affinity::EffectiveTopology;
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
            .map(|_| reserve_connection(&admission, ResourceClass::ControlConnection).unwrap())
            .collect::<Vec<_>>();
        let outbound_count = neighbor.min(admission.limit(ResourceClass::OutboundConnection));
        let outbound = (0..outbound_count)
            .map(|_| reserve_connection(&admission, ResourceClass::OutboundConnection).unwrap())
            .collect::<Vec<_>>();
        let ingress = reserve_connection(&admission, ResourceClass::IngressConnection).unwrap();
        assert_eq!(
            admission.used(ResourceClass::Connection),
            3 + outbound_count + 1
        );
        assert!(admission.used(ResourceClass::Connection) <= connections);
        drop((control, outbound, ingress));
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
}

impl WorkerApplication {
    // Keep private-field access local; shared fixture assembly belongs to test_support.
    pub(crate) fn into_test_coordinator(self) -> Rc<Coordinator> {
        self.coordinator
    }
}

#[test]
fn assembled_worker_exports_live_quota_gauges() {
    use crate::admission::ResourceClass;
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
fn disk_usage_worker_samples_recovery_and_reset() {
    let simulation = uring_runtime::reactor::simulation::Simulation::new();
    let _environment = simulation.enter();
    let worker = wake_test_worker();
    let geometry = futures::executor::block_on(worker.prepare_storage()).unwrap();
    let mut image = futures::executor::block_on(worker.store.checkpoint.snapshot_shard()).unwrap();
    worker.store.checkpoint.finish_snapshot();
    image.segments[0].state = page_alloc::SegmentState::Sealed;
    image.segments[0].used_bytes = geometry.alignment().unwrap().length() as u64;
    let used = image.segments[0].used_bytes;
    futures::executor::block_on(worker.store.recovery.install_shard(Some(image))).unwrap();
    worker.observe_health().unwrap();
    let scrape = || {
        let mut output = String::new();
        worker
            .telemetry
            .metrics
            .write_prometheus(&mut output)
            .unwrap();
        output
    };
    let output = scrape();
    assert!(output.contains(&format!(
        "racer_disk_size_bytes{{disk=\"file:worker-0-slab-0.dat\"}} {}\n",
        geometry.slab_bytes
    )));
    assert!(output.contains(&format!(
        "racer_disk_used_bytes{{disk=\"file:worker-0-slab-0.dat\"}} {used}\n"
    )));
    futures::executor::block_on(worker.store.recovery.install_shard(None)).unwrap();
    assert_eq!(scrape(), output, "samples change only on observation");
    worker.observe_health().unwrap();
    assert!(scrape().contains("racer_disk_used_bytes{disk=\"file:worker-0-slab-0.dat\"} 0\n"));
}

#[test]
fn application_budget_poll_preserves_cooperative_and_completion_wakes() {
    let mut worker = wake_test_worker();
    // Exercise the production runtime Service entry point with side-effect-free
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
fn placement_maintenance_retries_pressure_without_spinning_or_log_floods() {
    use crate::topology::Maintenance;
    let mut worker = wake_test_worker();
    let count = Arc::new(crate::test_support::WakeCounter::default());
    let waker = std::task::Waker::from(count.clone());
    let mut cx = Context::from_waker(&waker);
    let now = uring_runtime::environment::now();
    worker
        .observe_placement_maintenance(Ok(Maintenance::Idle), now, &mut cx)
        .unwrap();
    assert_eq!(count.count(), 0);
    worker
        .observe_placement_maintenance(Ok(Maintenance::Progress), now, &mut cx)
        .unwrap();
    assert_eq!(count.count(), 1);
    worker
        .observe_placement_maintenance(Ok(Maintenance::Blocked), now, &mut cx)
        .unwrap();
    assert_eq!(worker.placement_retry, now + Duration::from_millis(100));
    assert_eq!(worker.placement_warning, Some(now));
    worker
        .observe_placement_maintenance(
            Err(Error::Overloaded),
            now + Duration::from_secs(1),
            &mut cx,
        )
        .unwrap();
    assert_eq!(worker.placement_warning, Some(now));
    assert_eq!(count.count(), 1);
    let later = now + Duration::from_secs(60);
    worker
        .observe_placement_maintenance(Ok(Maintenance::Blocked), later, &mut cx)
        .unwrap();
    assert_eq!(worker.placement_warning, Some(later));
    assert_eq!(
        worker.observe_placement_maintenance(Err(Error::Internal), later, &mut cx),
        Err(Error::Internal)
    );
}

#[test]
fn metadata_wait_uses_worker_clock_and_keeps_due_backlog_runnable() {
    use uring_runtime::environment::SimulationClock;
    let clock = SimulationClock::new(913);
    let worker = {
        let _environment = clock.environment(0).enter();
        let mut worker = wake_test_worker();
        // Settle the independently due ownership timer before isolating metadata
        // deadlines. Construct every worker timer in the same simulated clock.
        worker
            .ownership_maintenance
            .poll(uring_runtime::environment::now(), false, || {
                Ok(crate::topology::Maintenance::Idle)
            });
        worker
    };
    let maximum = Duration::from_millis(1);
    let now = {
        let _environment = worker.environment.enter();
        uring_runtime::environment::now()
    };
    // Require the hook to enter the worker environment, not sample host time.
    let _required = uring_runtime::environment::require_simulated();
    assert_eq!(worker.wait_timeout(maximum), maximum);
    let probes: Vec<_> = (0..65)
        .map(|_| crate::read::metadata::tests::deadline_probe(&worker.metadata, now + maximum * 2))
        .collect();
    assert_eq!(worker.wait_timeout(maximum), maximum);
    clock.advance(maximum + maximum / 2);
    assert_eq!(worker.wait_timeout(maximum), maximum / 2);
    assert_eq!(worker.wait_timeout(Duration::ZERO), Duration::ZERO);
    clock.advance(maximum / 2);
    assert_eq!(worker.wait_timeout(maximum), Duration::ZERO);
    assert_eq!(worker.metadata.poll_deadlines(now + maximum * 2, 64), 64);
    assert_eq!(worker.wait_timeout(maximum), Duration::ZERO);
    clock.advance(maximum);
    assert_eq!(worker.wait_timeout(maximum), Duration::ZERO);
    assert_eq!(worker.metadata.poll_deadlines(now + maximum * 3, 1), 1);
    assert_eq!(worker.wait_timeout(maximum), maximum);
    drop(probes);
}

#[test]
fn application_metadata_deadline_hook_is_budgeted_and_precedes_peer_polling() {
    use futures::Stream;
    use futures::stream::FuturesUnordered;
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
    assert_eq!(
        worker.wait_timeout(Duration::from_millis(1)),
        Duration::ZERO
    );
    worker.poll_budgeted(&mut cx, usize::MAX).unwrap();
    assert_eq!(
        completed.get(),
        64,
        "deadline hook clamps before polling peers"
    );
    assert_eq!(
        worker.wait_timeout(Duration::from_millis(1)),
        Duration::ZERO
    );
    worker.poll_budgeted(&mut cx, 1).unwrap();
    assert_eq!(completed.get(), 65);
    assert_eq!(
        worker.wait_timeout(Duration::from_millis(1)),
        Duration::from_millis(1)
    );
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
        let (io, engine) = security::pair(WorkerId(0), 0, config.limits.queue_entries);
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
        let state = NodeState {
            publications: Arc::new(Published::new(Snapshot::retention(
                config.limits.retained_snapshots.get(),
            ))),
            ..NodeState::default()
        };
        let node = Arc::new(state);
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
        let (second_io, second_engine) =
            security::pair(WorkerId(1), 0, config.limits.queue_entries);
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
        let mut publication = racer_control_wire::decode_publication(include_bytes!(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../../internal/racer/wire/testdata/publication.json"
        )))
        .unwrap();
        publication.cluster = config.cluster.clone();
        let mut requests = Vec::new();
        for version in 1..=config.limits.retained_snapshots.get() + 1 {
            publication.sequence.0 = version as u64;
            publication.membership_version.0 = version as u64;
            let snapshot = worker.snapshots.apply(publication.clone()).unwrap();
            requests.push(snapshot.membership.clone());
        }
        publication.sequence.0 += 1;
        publication.membership_version.0 += 1;
        assert!(matches!(
            worker.snapshots.apply(publication),
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
    use crate::peer::PeerNetwork;
    use racer_control_wire::MembershipVersion;
    let mut publication = racer_control_wire::decode_publication(include_bytes!(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../../internal/racer/wire/testdata/publication.json"
    )))
    .unwrap();
    let local = publication.members[0].node.clone();
    let neighbor = publication.members[1].node.clone();
    let published = Arc::new(Published::new(Snapshot::retention(1)));
    let store = PublicationTarget::new(publication.cluster.clone(), published.clone());
    let networks = [
        PeerNetwork::new(local.clone(), published.clone()).unwrap(),
        PeerNetwork::new(local, published).unwrap(),
    ];
    let mut publish = |sequence, version| {
        publication.sequence.0 = sequence;
        publication.membership_version.0 = version;
        store.apply(publication.clone())
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
        crate::control::Availability::new(node.publications.clone(), worker.keys.clone());
    for (sequence, present) in [(1, true), (2, false), (3, true)] {
        worker
            .snapshots
            .apply(publication(
                &config,
                sequence,
                if present { vec![cache.clone()] } else { vec![] },
            ))
            .unwrap();
        assert_eq!(availability.cache(&cache.id), present);
    }
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
        .apply(publication(&config, 1, vec![original.clone()]))
        .unwrap();
    worker.refresh_snapshot(&current_scope).unwrap();
    assert_eq!(worker.caches, vec![original.clone()]);
    assert!(worker.peer_task.is_none());
    assert!(worker.diagnostic_task.is_none());
    assert!(worker.prepared_listeners.borrow().is_none());
    let (_, _, roots) = crate::test_support::security::issued();
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
        .apply(publication(&config, 2, vec![]))
        .unwrap();
    let mut replacement = original.clone();
    replacement.id = racer_control_wire::CacheId("55555555-5555-4555-8555-555555555555".into());
    worker
        .snapshots
        .apply(publication(&config, 3, vec![replacement.clone()]))
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
        let _ = futures::executor::block_on(app.store.writer.open()).unwrap();
        app.caches = vec![definition()];
    }
    first
        .snapshots
        .apply(publication(&config, 1, vec![definition()]))
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
    let adapter = CachePublication {
        proposal: RefCell::new(None),
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
        first.snapshots.apply_staged(invalid, Some(rejected)),
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
        .apply_staged(
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
                crate::admission::ResourceClass::DirtyCiphertext,
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
                use crate::telemetry::Event;
                app.telemetry
                    .metrics
                    .record(Event::MemoryHit, u64::from(id) + 1);
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
    assert!(node.cache_rollout.pending(&WorkerId(0)).unwrap().is_none());
    assert!(node.cache_rollout.pending(&WorkerId(1)).unwrap().is_none());
    fixture.hold_long_poll.store(false, Ordering::Release);
}

/// A held renewal cannot stop acceptance of an already received cache removal.
#[test]
fn cache_removal_installs_while_renewal_and_next_publication_poll_are_held() {
    let mut fixture = ControlFixture::new();
    let (config, node) = fixture.bootstrap_node(1, Duration::from_secs(15));
    let definition = definition();
    *fixture.publication.lock().unwrap() = Some(publication(&config, 1, vec![definition.clone()]));
    fixture
        .certificate_age
        .store(16 * 3600 + 60, Ordering::Release);
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
    worker.control_task.take();
    let control = worker.control.clone().unwrap();
    assert!(control.identity().unwrap().renewal_due());
    assert!(control.identity().unwrap().valid_now());
    let accepted = fixture.enrollments.load(Ordering::Acquire);
    let baseline_polls = fixture.long_polls.load(Ordering::Acquire);
    fixture.hold_renewal.store(true, Ordering::Release);
    fixture.hold_long_poll.store(true, Ordering::Release);
    *fixture.publication.lock().unwrap() = Some(publication(&config, 2, vec![]));
    let turn_scope = scope(Duration::from_secs(15)).unwrap();
    let mut progress = control.progress(&turn_scope);
    let until = Instant::now() + Duration::from_secs(5);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    loop {
        runtime.reactor.poll_budgeted(64).unwrap();
        engine.poll_budgeted(&mut cx, 64).unwrap();
        runtime.crypto.poll_budgeted(64).unwrap();
        worker.poll_cache_preparation(&mut cx).unwrap();
        assert!(progress.as_mut().poll(&mut cx).is_pending());
        if worker.snapshots.cursor().unwrap() == Some(wire::PublicationSequence(2))
            && fixture.held_renewals.load(Ordering::Acquire) != 0
            && fixture.long_polls.load(Ordering::Acquire) > baseline_polls
        {
            break;
        }
        assert!(
            Instant::now() < until,
            "cache removal stalled behind held renewal"
        );
        runtime.reactor.wait(Duration::from_millis(1)).unwrap();
    }
    assert!(worker.snapshots.current().unwrap().caches.is_empty());
    // Atomic commit retires admission; the listener worker performs pathname
    // cleanup on its next poll, still independently of the held renewal.
    worker.clients.poll_budgeted(&mut cx, 64).unwrap();
    assert!(
        !fixture
            .directory
            .join("sockets")
            .join(&definition.name)
            .join("client/socket")
            .exists(),
        "removal must close listener admission before renewal completes"
    );
    assert_eq!(fixture.enrollments.load(Ordering::Acquire), accepted);
    let held_polls = fixture.long_polls.load(Ordering::Acquire);
    fixture.hold_renewal.store(false, Ordering::Release);
    while fixture.enrollments.load(Ordering::Acquire) == accepted {
        assert!(progress.as_mut().poll(&mut cx).is_pending());
        runtime.reactor.poll_budgeted(64).unwrap();
        runtime.reactor.wait(Duration::from_millis(1)).unwrap();
        assert!(Instant::now() < until);
    }
    // Renewal completion must not cancel/recreate the active publication long poll.
    for _ in 0..16 {
        assert!(progress.as_mut().poll(&mut cx).is_pending());
        runtime.reactor.poll_budgeted(64).unwrap();
    }
    assert_eq!(fixture.long_polls.load(Ordering::Acquire), held_polls);
    fixture.hold_long_poll.store(false, Ordering::Release);
    drive(&runtime, &mut engine, progress).unwrap();
    stop_worker(&mut worker, &runtime, &mut engine);
}

/// Local barrier completion must progress during remote Retry-After and held renewal.
#[test]
fn cache_removal_ack_installs_during_retry_after_and_held_renewal() {
    cache_removal_during_retry_after(false);
}

/// Publication acceptance need not wait for the renewal server to enter its handler.
#[test]
fn cache_removal_ack_installs_during_retry_after_before_renewal_handler_entry() {
    cache_removal_during_retry_after(true);
}

/// Exercise both server orderings without assuming renewal and publication latency.
fn cache_removal_during_retry_after(delay_renewal_entry: bool) {
    let mut fixture = ControlFixture::new();
    let (config, node) = fixture.bootstrap_node(1, Duration::from_secs(15));
    let definition = definition();
    *fixture.publication.lock().unwrap() = Some(publication(&config, 1, vec![definition.clone()]));
    fixture
        .certificate_age
        .store(16 * 3600 + 60, Ordering::Release);
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
    worker.control_task.take();
    let control = worker.control.clone().unwrap();
    assert!(control.identity().unwrap().renewal_due());
    assert!(control.identity().unwrap().valid_now());
    let accepted = fixture.enrollments.load(Ordering::Acquire);
    fixture.hold_renewal.store(true, Ordering::Release);
    fixture
        .pause_renewal
        .store(delay_renewal_entry, Ordering::Release);
    fixture.publication_retry_after.store(60, Ordering::Release);
    *fixture.publication.lock().unwrap() = Some(publication(&config, 2, vec![]));
    let turn_scope = scope(Duration::from_secs(15)).unwrap();
    let mut progress = control.progress(&turn_scope);
    let until = Instant::now() + Duration::from_secs(5);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let mut failed_at = None;
    let mut installed_before_renewal = false;
    loop {
        runtime.reactor.poll_budgeted(64).unwrap();
        engine.poll_budgeted(&mut cx, 64).unwrap();
        runtime.crypto.poll_budgeted(64).unwrap();
        assert!(progress.as_mut().poll(&mut cx).is_pending());
        if control.membership_diagnostic().unwrap().pending_sequence == 2 {
            fixture.poll_status.store(503, Ordering::Release);
        }
        if fixture.publication_failures.load(Ordering::Acquire) != 0 {
            let since = failed_at.get_or_insert_with(Instant::now);
            // Release local preparation during remote backoff. In the held
            // case, explicitly wait for renewal entry instead of assuming the
            // publication request and renewal handler complete in that order.
            if since.elapsed() >= Duration::from_millis(100)
                && (delay_renewal_entry || fixture.held_renewals.load(Ordering::Acquire) != 0)
            {
                worker.poll_cache_preparation(&mut cx).unwrap();
            } else {
                assert_eq!(
                    worker.snapshots.cursor().unwrap(),
                    Some(wire::PublicationSequence(1))
                );
            }
        }
        if worker.snapshots.cursor().unwrap() == Some(wire::PublicationSequence(2)) {
            if delay_renewal_entry && !installed_before_renewal {
                assert_eq!(fixture.held_renewals.load(Ordering::Acquire), 0);
                installed_before_renewal = true;
                fixture.pause_renewal.store(false, Ordering::Release);
            }
            if fixture.held_renewals.load(Ordering::Acquire) != 0 {
                break;
            }
        }
        assert!(
            Instant::now() < until,
            "control progress stalled: cursor={:?}, failures={}, held_renewals={}, renewal_error={:?}",
            worker.snapshots.cursor().unwrap(),
            fixture.publication_failures.load(Ordering::Acquire),
            fixture.held_renewals.load(Ordering::Acquire),
            control.renewal_error(),
        );
        runtime.reactor.wait(Duration::from_millis(1)).unwrap();
    }
    assert_eq!(
        fixture.publication_failures.load(Ordering::Acquire),
        1,
        "Retry-After must still prevent another fetch"
    );
    assert_eq!(installed_before_renewal, delay_renewal_entry);
    assert!(fixture.held_renewals.load(Ordering::Acquire) > 0);
    assert_eq!(fixture.enrollments.load(Ordering::Acquire), accepted);
    worker.clients.poll_budgeted(&mut cx, 64).unwrap();
    assert!(
        !fixture
            .directory
            .join("sockets")
            .join(&definition.name)
            .join("client/socket")
            .exists()
    );
    drop(progress);
    fixture.hold_renewal.store(false, Ordering::Release);
    stop_worker(&mut worker, &runtime, &mut engine);
}

#[test]
fn same_node_renewal_backs_off_expires_closed_and_recovers() {
    use crate::telemetry::State;
    use uring_runtime::environment::SimulationClock;
    use uring_runtime::environment::now;
    use uring_runtime::environment::wall_now;

    // Complete each real control turn through the application's error handling.
    // Leave the next turn unsubmitted so the test controls all retry boundaries.
    fn turn(
        worker: &mut WorkerApplication,
        runtime: &WorkerRuntime,
        engine: &mut PageCryptoEngine,
        clock: Option<&SimulationClock>,
    ) {
        let done = Rc::new(std::cell::Cell::new(false));
        let completed = done.clone();
        let control = worker.control.clone().unwrap();
        if let Some(clock) = clock
            && control.identity().is_some_and(|i| i.valid_now())
            && let Some(next) = control.next_attempt()
        {
            clock.advance(next.saturating_duration_since(now()));
        }
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
    turn(&mut worker, &runtime, &mut engine, None);
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
    turn(&mut worker, &runtime, &mut engine, Some(&clock));
    assert_eq!(control.renewal_error(), Some(Error::Unavailable));
    assert_eq!(
        fixture.bootstrap_requests.lock().unwrap().len(),
        baseline + 1
    );
    assert_eq!(fixture.polls.load(Ordering::Acquire), polls + 1);
    let pending = std::fs::read(config.identity_directory.join("pending.json")).unwrap();

    // Even many successful 204 polls must neither reset renewal backoff nor
    // replace the accepted certificate. Each successful feed turn now has a real
    // 10ms tick; leave room for those ticks before the independent issuance retry.
    clock.advance(Duration::from_millis(500));
    for _ in 0..16 {
        turn(&mut worker, &runtime, &mut engine, Some(&clock));
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
    clock.advance(
        control
            .renewal_attempt()
            .unwrap()
            .saturating_duration_since(now()),
    );
    turn(&mut worker, &runtime, &mut engine, Some(&clock));
    assert_eq!(
        fixture.bootstrap_requests.lock().unwrap().len(),
        baseline + 2
    );
    assert_eq!(fixture.polls.load(Ordering::Acquire), polls + 18);
    // This seed selects a second jittered delay greater than one second.
    // Successful polls must retain the failure count so retries grow rather
    // than restarting at the first-failure delay on every turn.
    let retry = control.renewal_attempt().unwrap();
    assert!(retry.duration_since(now()) > Duration::from_secs(1));
    clock.advance(Duration::from_millis(500));
    turn(&mut worker, &runtime, &mut engine, Some(&clock));
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
            control.progress(&scope(Duration::from_secs(10)).unwrap())
        ),
        Err(Error::Unavailable)
    ));
    turn(&mut worker, &runtime, &mut engine, Some(&clock));
    assert_eq!(
        fixture.bootstrap_requests.lock().unwrap().len(),
        baseline + 3
    );
    assert_eq!(fixture.polls.load(Ordering::Acquire), polls_at_expiry);
    assert_eq!(control.renewal_error(), Some(Error::Unavailable));
    let retry = control.renewal_attempt().unwrap();
    assert!(
        (Duration::from_secs(1)..=Duration::from_secs(30)).contains(&retry.duration_since(now()))
    );
    clock.advance(retry.duration_since(now()) - Duration::from_millis(1));
    for _ in 0..16 {
        turn(&mut worker, &runtime, &mut engine, Some(&clock));
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
    turn(&mut worker, &runtime, &mut engine, Some(&clock));
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
    assert!(control.renewal_attempt().is_none());
    assert_eq!(
        control.next_attempt(),
        Some(now() + Duration::from_millis(10))
    );
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
    turn(&mut worker, &runtime, &mut engine, Some(&clock));
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
    use crate::model::VersionMetadata;
    use crate::model::*;
    let mut fixture = ControlFixture::new();
    let mut config = fixture.config.take().unwrap();
    let diagnostic_address = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    config.diagnostics_listen = diagnostic_address.local_addr().unwrap();
    drop(diagnostic_address);
    let keep = racer_control_wire::CacheDefinition {
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
    .unwrap()
    .0;
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
    while node
        .cache_rollout
        .pending(&WorkerId(0))
        .unwrap()
        .is_none_or(|p| p.value.len() != 1)
    {
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
    let (old, selector) = start().unwrap();
    assert_eq!(selector.as_deref(), Some("nvme-cache"));
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
    assert_eq!(start().unwrap().0, new);
    assert_ne!(old, new);
    assert!(!config.identity_directory.join("pending.json").exists());
    assert_eq!(start().unwrap().0, new);
    assert_eq!(fixture.enrollments.load(Ordering::Acquire), 3);
}

#[test]
fn node_replacement_drains_all_workers_and_restart_converges() {
    use crate::worker::WorkerPair;
    use uring_runtime::group::affinity::EffectiveTopology;
    for renewal_due in [false, true] {
        let mut fixture = ControlFixture::new();
        let (config, node) = fixture.bootstrap_node(2, Duration::from_secs(15));
        if renewal_due {
            fixture
                .certificate_age
                .store(16 * 3600 + 60, Ordering::Release);
        }
        let app = Arc::new(Application {
            resources: std::sync::OnceLock::new(),
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
        app.prepare_workers().unwrap();
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
        .unwrap()
        .0;
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
    use crate::store::checkpoint;
    use crate::worker::WorkerPair;
    let mut fixture = ControlFixture::new();
    let (config, node) = fixture.bootstrap_node(2, Duration::from_secs(15));
    let keys = Keyring::new(
        config.cluster.clone(),
        config.node.clone(),
        node.keys.clone(),
    );
    let limits = config.limits.clone();
    let app = Arc::new(Application {
        resources: std::sync::OnceLock::new(),
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
    app.prepare_workers().unwrap();
    group
        .start(app.clone(), &scope(Duration::from_secs(15)).unwrap())
        .unwrap();
    assert_eq!(node.prepared.load(Ordering::Acquire), 2);
    assert!(node.observations.health.ready());
    let cache = racer_control_wire::CacheId("33333333-3333-4333-8333-333333333333".into());
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
    crate::test_support::security::assert_page_key(&lease, &[21; 32]);
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
    engine: &mut dyn uring_runtime::group::Service<RequestScope>,
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

#[test]
fn oversized_populated_shutdown_cut_thaws_and_finishes_drain_without_publication() {
    let mut fixture = ControlFixture::new();
    {
        let mut bundle = fixture.bundle.lock().unwrap();
        let cluster = bundle.cluster.clone();
        *bundle =
            crate::test_support::security::rotation_bundle(1, bundle.peer_trust_roots.clone());
        bundle.cluster = cluster;
    }
    let (config, node) = fixture.bootstrap_node(1, Duration::from_secs(15));
    *fixture.publication.lock().unwrap() = Some(publication(&config, 1, vec![definition()]));
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
    let index = worker.store.writer.index();
    index.set_page_capacity(1_000_000).unwrap();
    for number in 0u64..128 {
        let mut key = [0; 32];
        key[..8].copy_from_slice(&number.to_le_bytes());
        index
            .publish_version(crate::model::VersionMetadata {
                version: crate::model::ObjectVersion {
                    object: crate::model::ObjectId {
                        cache: worker.caches[0].id.clone(),
                        key: crate::model::CacheKey(key),
                    },
                    etag: crate::model::StrongEtag::parse(b"\"shutdown-budget\"").unwrap(),
                },
                length: 0,
                content_type: None,
            })
            .unwrap();
    }
    assert!(index.snapshot_metadata().len() >= 16);
    worker.checkpoint_budget = 32 * 1024;
    let deadline = scope(Duration::from_secs(5)).unwrap();
    assert_eq!(
        drive(&runtime, &mut engine, worker.drain(&deadline)),
        Err(Error::Overloaded)
    );
    assert!(worker.store.writer.is_idle());
    assert!(
        crate::store::checkpoint::read_candidates(&config.slab_directory)
            .unwrap()
            .is_empty()
    );
    let _ = futures::executor::block_on(worker.store.checkpoint.snapshot_shard()).unwrap();
    worker.store.checkpoint.finish_snapshot();
    drive(&runtime, &mut engine, runtime.reactor.drain()).unwrap();
    drive(&runtime, &mut engine, worker.shutdown(&deadline)).unwrap();
    assert_eq!(runtime.reactor.in_flight(), 0);
}

fn drive<T>(
    runtime: &WorkerRuntime,
    engine: &mut dyn uring_runtime::group::Service<RequestScope>,
    future: Operation<'_, T>,
) -> Result<T> {
    let mut future = future;
    let until = Instant::now() + Duration::from_secs(15);
    loop {
        runtime.reactor.poll_budgeted(64)?;
        engine.poll_budgeted(
            &mut Context::from_waker(futures::task::noop_waker_ref()),
            64,
        )?;
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
        engine
            .poll_budgeted(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                64,
            )
            .unwrap();
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
            crate::test_support::security::rotation_bundle(1, bundle.peer_trust_roots.clone());
        bundle.cluster = config.cluster.clone();
    }
    config.node = bootstrap(
        &config,
        &node,
        &config.limits,
        &scope(Duration::from_secs(15)).unwrap(),
    )
    .unwrap()
    .0;
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
            crate::test_support::security::rotation_bundle(2, bundle.peer_trust_roots.clone());
        bundle.cluster = config.cluster.clone();
    }
    let mut rotation = control.keyring_progress(&key_scope);
    drive(
        &runtime,
        &mut engine,
        Box::pin(std::future::poll_fn(|cx| {
            assert!(topology.as_mut().poll(cx).is_pending());
            if let Poll::Ready(result) = rotation.as_mut().poll(cx) {
                result?;
                rotation = control.keyring_progress(&key_scope);
            }
            if worker.keys.active(&cache, KeyPurpose::Page)?.id() != old_id {
                Poll::Ready(Ok(()))
            } else {
                cx.waker().wake_by_ref();
                Poll::Pending
            }
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
    drop(rotation);
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
            crate::test_support::security::rotation_bundle(3, bundle.peer_trust_roots.clone());
        bundle.cluster = config.cluster.clone();
    }
    let recovery_scope = scope(Duration::from_secs(40)).unwrap();
    let mut recovery = control.keyring_progress(&recovery_scope);
    drive(
        &runtime,
        &mut engine,
        Box::pin(std::future::poll_fn(|cx| {
            if let Poll::Ready(result) = recovery.as_mut().poll(cx) {
                result?;
                recovery = control.keyring_progress(&recovery_scope);
            }
            if worker.keys.active(&cache, KeyPurpose::Page)?.id() != accepted {
                Poll::Ready(Ok(()))
            } else {
                cx.waker().wake_by_ref();
                Poll::Pending
            }
        })),
    )
    .unwrap();
    drop(recovery);
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
    config.node = bootstrap(&config, &node, &config.limits, &startup)
        .unwrap()
        .0;
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
    let cache = racer_control_wire::CacheId("33333333-3333-4333-8333-333333333333".into());
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
                .reserve(Some(&cache), crate::admission::ResourceClass::Plaintext, 8)
                .unwrap(),
            8,
        )
        .unwrap();
    let ciphertext = runtime
        .admission
        .reserve(
            Some(&cache),
            crate::admission::ResourceClass::Ciphertext,
            24,
        )
        .unwrap();
    let accepted_scope = scope(Duration::from_secs(10)).unwrap();
    let mut accepted = runtime.crypto.execute(
        crate::security::CryptoInput::Encrypt {
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
    crate::test_support::security::assert_page_key(&lease, &[19; 32]);
    assert!(
        worker
            .keys
            .lease(Some(&cache), reference.id, KeyPurpose::Page)
            .is_err()
    );
    while runtime.crypto.outstanding() != 0 {
        engine
            .poll_budgeted(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                64,
            )
            .unwrap();
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
            crate::admission::ResourceClass::DirtyCiphertext,
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

#[test]
fn profiling_enabled_without_capture_stops_and_drains_without_timer() {
    let mut fixture = ControlFixture::new();
    let (mut config, node) = fixture.bootstrap_node(1, Duration::from_secs(15));
    config.pprof_enabled = true;
    let (mut worker, runtime, mut engine) = local_worker(&config, &node, 0);
    let startup = scope(Duration::from_secs(15)).unwrap();
    drive(&runtime, &mut engine, worker.start(&startup)).unwrap();
    let controller = node.profiling.get().unwrap();
    assert!(controller.poll_stopped());
    let deadline = scope(Duration::from_secs(5)).unwrap();
    // No helper means this fence completes in one poll, without a kernel timer.
    let mut fence = Box::pin(worker.stop_profiling(&deadline));
    assert_eq!(
        fence
            .as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref())),
        Poll::Ready(Ok(()))
    );
    drop(fence);
    assert!(controller.start(Duration::from_secs(1)).is_err());
    drive(&runtime, &mut engine, worker.drain(&deadline)).unwrap();
    drive(&runtime, &mut engine, runtime.reactor.drain()).unwrap();
    drive(&runtime, &mut engine, worker.shutdown(&deadline)).unwrap();
    assert_eq!(runtime.reactor.in_flight(), 0);
}

fn tcp_nodelay(fd: &impl std::os::fd::AsRawFd) -> i32 {
    let mut value: libc::c_int = -1;
    let mut length = std::mem::size_of_val(&value) as libc::socklen_t;
    // SAFETY: getsockopt writes only the supplied live integer and length.
    assert_eq!(
        unsafe {
            libc::getsockopt(
                fd.as_raw_fd(),
                libc::IPPROTO_TCP,
                libc::TCP_NODELAY,
                (&mut value as *mut libc::c_int).cast(),
                &mut length,
            )
        },
        0
    );
    value
}

#[test]
fn peer_tcp_nodelay_assembled_outbound_and_distributed_accept() {
    for enabled in [false, true] {
        let mut config = crate::test_support::cluster::config(false);
        config.peer_tcp_nodelay = enabled;
        let node = Arc::new(NodeState::default());
        let (app, runtime, _) = local_worker(&config, &node, 0);
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let address = listener.local_addr().unwrap();
        drop(listener);
        let scope = scope(Duration::from_secs(5)).unwrap();
        let mut serving = app.peers.listen(address, &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(serving.as_mut().poll(&mut cx).is_pending());
        let endpoint = crate::http::Endpoint::Peer(address.to_string());
        let mut connecting = app.http.checkout(&endpoint, &scope);
        let mut outbound = None;
        let mut inbound = None;
        let until = Instant::now() + Duration::from_secs(3);
        while outbound.is_none() || inbound.is_none() {
            if outbound.is_none()
                && let Poll::Ready(result) = connecting.as_mut().poll(&mut cx)
            {
                outbound = Some(result.unwrap());
            }
            assert!(serving.as_mut().poll(&mut cx).is_pending());
            runtime.reactor.poll_budgeted(64).unwrap();
            if inbound.is_none() {
                inbound = node
                    .ingress
                    .pop_batch::<1>(WorkerId(0), cx.waker(), 1)
                    .unwrap()[0]
                    .take();
            }
            assert!(Instant::now() < until, "peer socket setup stalled");
        }
        assert_eq!(
            tcp_nodelay(outbound.as_ref().unwrap().socket().as_ref()),
            i32::from(enabled)
        );
        assert_eq!(
            tcp_nodelay(&inbound.as_ref().unwrap().fd),
            i32::from(enabled)
        );
        drop(connecting);
        drop(outbound);
        drop(inbound);
        scope.cancel().unwrap();
        loop {
            runtime.reactor.poll_budgeted(64).unwrap();
            if let Poll::Ready(result) = serving.as_mut().poll(&mut cx) {
                assert_eq!(result, Err(Error::Cancelled));
                break;
            }
            assert!(Instant::now() < until, "peer cancellation stalled");
        }
        assert_eq!(
            runtime
                .admission
                .used(crate::admission::ResourceClass::Connection),
            0
        );
    }
}

#[test]
fn peer_tcp_nodelay_accept_failure_closes_owned_socket_and_false_preserves_policy() {
    use std::io::Read;
    let mut config = crate::test_support::cluster::config(false);
    config.peer_tcp_nodelay = true;
    let (app, _, _) = local_worker(&config, &Arc::new(NodeState::default()), 0);
    let (socket, mut peer) = std::os::unix::net::UnixStream::pair().unwrap();
    peer.set_read_timeout(Some(Duration::from_secs(1))).unwrap();
    assert!(matches!(
        app.peers.configure_accepted(socket.into()),
        Err(Error::Io)
    ));
    assert_eq!(peer.read(&mut [0]).unwrap(), 0);

    config.peer_tcp_nodelay = false;
    let (app, _, _) = local_worker(&config, &Arc::new(NodeState::default()), 0);
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let _peer = std::net::TcpStream::connect(listener.local_addr().unwrap()).unwrap();
    let (socket, _) = listener.accept().unwrap();
    socket.set_nodelay(true).unwrap();
    let socket = app.peers.configure_accepted(socket.into()).unwrap();
    assert_eq!(
        tcp_nodelay(&socket),
        1,
        "false must not rewrite existing policy"
    );
}

#[test]
fn assembly_applies_configured_client_request_timeout() {
    use crate::admission::ResourceClass;
    use std::sync::atomic::AtomicBool;
    for timeout in [Duration::from_millis(250), Duration::from_secs(45)] {
        let clock = uring_runtime::environment::SimulationClock::new(908);
        let _environment = clock.environment(0).enter();
        let mut config = crate::test_support::cluster::config(false);
        config.request_timeout = timeout;
        config.reader_stall_timeout = timeout;
        let (app, runtime, _) = local_worker(&config, &Arc::new(NodeState::default()), 0);
        let (server, mut client) = std::os::unix::net::UnixStream::pair().unwrap();
        client.set_nonblocking(true).unwrap();
        let reservation =
            reserve_connection(&runtime.admission, ResourceClass::IngressConnection).unwrap();
        let connection = crate::http::from_reserved(server.into(), reservation).unwrap();
        app.clients
            .install_connection(connection, definition().id, Arc::new(AtomicBool::default()))
            .unwrap();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        app.clients.poll_budgeted(&mut cx, 64).unwrap();
        clock.advance(timeout - Duration::from_millis(1));
        app.clients.poll_budgeted(&mut cx, 64).unwrap();
        assert_eq!(app.clients.active_connections(), 1);
        clock.advance(Duration::from_millis(2));
        for _ in 0..64 {
            runtime.reactor.poll_budgeted(64).unwrap();
            app.clients.poll_budgeted(&mut cx, 64).unwrap();
            if app.clients.active_connections() == 0 {
                break;
            }
        }
        assert_eq!(app.clients.active_connections(), 0);
        let mut byte = [0];
        assert_eq!(std::io::Read::read(&mut client, &mut byte).unwrap(), 0);
    }
}

#[test]
fn worker_requesters_share_configured_admission_and_production_metrics() {
    use crate::telemetry::Event;
    use crate::telemetry::Gauge;
    let mut config = crate::test_support::cluster::config(false);
    config.peer_admission = crate::peer::Config {
        total: 2,
        per_peer: 1,
    };
    let node = Arc::new(
        NodeState::with_peer_admission(vec![WorkerId(0), WorkerId(1)], 16, config.peer_admission)
            .unwrap(),
    );
    let (first, _, _) = local_worker(&config, &node, 0);
    let (second, _, _) = local_worker(&config, &node, 1);
    let a = first.peer_requester.admission();
    let b = second.peer_requester.admission();
    let peer = NodeId("test-peer".into());
    let permit = a.acquire(&peer).unwrap();
    assert!(matches!(b.acquire(&peer), Err(Error::Overloaded)));
    assert_eq!(node.metrics[1].1.count(Event::PeerAdmissionAccepted), 1);
    assert_eq!(node.metrics[1].1.count(Event::PeerAdmissionRejected), 1);
    assert_eq!(node.metrics[1].1.gauge(Gauge::PeerExchanges), 1);
    assert_eq!(node.metrics[1].1.gauge(Gauge::PeerAdmissionLimit), 2);
    permit.observe(crate::peer::Outcome::PeerFailure);
    assert!(!b.available(&peer));
    drop(permit);
    assert_eq!(node.metrics[1].1.gauge(Gauge::PeerExchanges), 0);
}

#[test]
fn receive_gate_two_workers_share_capacity_and_wake_expiry_without_cqe() {
    use futures::Stream;
    use futures::stream::FuturesUnordered;
    let clock = uring_runtime::environment::SimulationClock::new(919);
    let _env = clock.environment(0).enter();
    let mut config = crate::test_support::cluster::config(false);
    config.peer_receive.active = 1;
    config.peer_receive.wait = Duration::from_millis(10);
    let node = Arc::new(NodeState::new(vec![WorkerId(0), WorkerId(1)], 16).unwrap());
    let (mut first, _, _) = local_worker(&config, &node, 0);
    let (second, _, _) = local_worker(&config, &node, 1);
    let gate = node.peer_receive.get().unwrap().clone();
    assert!(Arc::ptr_eq(&gate, second.node.peer_receive.get().unwrap()));
    let scope = crate::runtime::RequestScope::new(
        crate::model::RequestId([19; 16]),
        uring_runtime::environment::now() + Duration::from_secs(1),
    )
    .unwrap();
    let permit = futures::executor::block_on(gate.acquire(&first.runtime.admission, &scope))
        .unwrap()
        .unwrap();
    let pending = async { gate.acquire(&second.runtime.admission, &scope).await };
    let mut children = FuturesUnordered::new();
    children.push(Box::pin(pending));
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(
        std::pin::Pin::new(&mut children)
            .poll_next(&mut cx)
            .is_pending()
    );
    first.started = true;
    first.stopping = true;
    clock.advance(Duration::from_millis(11));
    first.poll_budgeted(&mut cx, 0).unwrap();
    assert!(
        std::pin::Pin::new(&mut children)
            .poll_next(&mut cx)
            .is_pending()
    );
    first.poll_budgeted(&mut cx, 64).unwrap();
    assert!(matches!(
        std::pin::Pin::new(&mut children).poll_next(&mut cx),
        Poll::Ready(Some(Err(Error::DeadlineExceeded)))
    ));
    drop(children);
    drop(permit);
    assert!(
        futures::executor::block_on(gate.acquire(&second.runtime.admission, &scope))
            .unwrap()
            .is_some()
    );
}

#[test]
fn workers_share_configured_page_hedge_slots_and_bytes() {
    let clock = uring_runtime::environment::SimulationClock::new(907);
    let _environment = clock.environment(0).enter();
    let mut config = crate::test_support::cluster::config(false);
    config.page_hedge.slots = 1;
    let node = Arc::new(NodeState::default());
    let (first, _, _) = local_worker(&config, &node, 0);
    let owner = first.coordinator.hedge_owner().unwrap();
    let (mut second, _, _) = local_worker(&config, &node, 1);
    let permit = owner.acquire().unwrap();
    let wake = Arc::new(crate::test_support::WakeCounter::default());
    let waker = std::task::Waker::from(wake.clone());
    let mut cx = Context::from_waker(&waker);
    assert!(permit.delay(&mut cx).is_pending());
    clock.advance(config.page_hedge.delay);
    // No listener/checkpoint startup in this composition-only fixture. Alarms
    // must still wake before unrelated unstarted services report unavailable.
    assert_eq!(second.poll_services(&mut cx, 1), Err(Error::Unavailable));
    assert!(wake.count() > 0);
    assert!(permit.delay(&mut cx).is_ready());
    assert!(matches!(
        second.coordinator.hedge_owner().unwrap().acquire(),
        Err(Error::Overloaded)
    ));
    drop(permit);
    assert!(second.coordinator.hedge_owner().unwrap().acquire().is_ok());
}

#[test]
fn distributed_peer_listener_recovers_from_queue_pressure() {
    let mut config = crate::test_support::cluster::config(false);
    config.limits.queue_entries = NonZeroUsize::new(8).unwrap();
    let node = Arc::new(NodeState::default());
    let (app, runtime, _) = local_worker(&config, &node, 0);
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    drop(listener);
    let scope = scope(Duration::from_secs(5)).unwrap();
    let mut serving = app.peers.listen(address, &scope);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let (reader, _writer) = std::os::unix::net::UnixStream::pair().unwrap();
    let reader = Rc::new(uring_runtime::reactor::descriptor::Descriptor::from(reader));
    let mut pressure = Vec::new();
    for _ in 0..8 {
        let mut wait = runtime
            .reactor
            .readiness(reader.clone(), libc::POLLIN as u32, &scope);
        assert!(wait.as_mut().poll(&mut cx).is_pending());
        pressure.push(wait);
    }
    for _ in 0..32 {
        assert!(serving.as_mut().poll(&mut cx).is_pending());
        assert_eq!(runtime.reactor.in_flight(), 8);
    }
    let _client = std::net::TcpStream::connect(address).unwrap();
    drop(pressure);
    let until = Instant::now() + Duration::from_secs(2);
    loop {
        assert!(serving.as_mut().poll(&mut cx).is_pending());
        runtime.reactor.poll_budgeted(64).unwrap();
        if let Some(accepted) = node
            .ingress
            .pop_batch::<1>(WorkerId(0), cx.waker(), 1)
            .unwrap()[0]
            .take()
        {
            assert!(matches!(accepted.kind, crate::admission::Kind::Peer));
            break;
        }
        assert!(Instant::now() < until, "distributed peer accept stalled");
        runtime.reactor.wait(Duration::from_millis(1)).unwrap();
    }
    scope.cancel().unwrap();
    loop {
        runtime.reactor.poll_budgeted(64).unwrap();
        if let Poll::Ready(result) = serving.as_mut().poll(&mut cx) {
            assert_eq!(result, Err(Error::Cancelled));
            break;
        }
        assert!(Instant::now() < until, "peer cancellation stalled");
        runtime.reactor.wait(Duration::from_millis(1)).unwrap();
    }
}

#[test]
fn assembly_uses_node_metrics_for_sparse_worker_ids() {
    use crate::telemetry::Event;
    use crate::telemetry::Gauge;
    let config = crate::test_support::cluster::config(false);
    let node = Arc::new(NodeState::new(vec![WorkerId(9), WorkerId(2)], 16).unwrap());
    let (first, _, _) = local_worker(&config, &node, 9);
    let (second, _, _) = local_worker(&config, &node, 2);
    first.telemetry.metrics.record(Event::MemoryHit, 2);
    second.telemetry.metrics.record(Event::MemoryHit, 3);
    let request = first.telemetry.metrics.request().unwrap();
    assert_eq!(second.telemetry.metrics.count(Event::MemoryHit), 5);
    assert_eq!(second.telemetry.metrics.gauge(Gauge::ActiveRequests), 1);
    drop(first);
    drop(request);
    assert_eq!(second.telemetry.metrics.count(Event::RequestError), 1);
    assert_eq!(second.telemetry.metrics.gauge(Gauge::ActiveRequests), 0);
}

fn drive_peer<T>(reactor: &Reactor, future: impl Future<Output = T>) -> T {
    let mut future = std::pin::pin!(future);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let watchdog = Instant::now() + Duration::from_secs(30);
    loop {
        if let Poll::Ready(value) = future.as_mut().poll(&mut cx) {
            return value;
        }
        assert!(Instant::now() < watchdog);
        if reactor.poll_budgeted(128).unwrap() == 0 {
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
    }
}

fn application() -> (WorkerApplication, WorkerRuntime, PageCryptoEngine) {
    let mut config = crate::test_support::cluster::config(false);
    // One fixture represents both ends of eight nodes' sockets. Fund those
    // ingress leases plus the reserved outbound/control partition explicitly,
    // including the shared fixture's 16-connection neighbor cap.
    config.limits.client_connections = NonZeroUsize::new(64).unwrap();
    config.limits.header_bytes = NonZeroUsize::new(32 * 1024).unwrap();
    config.limits.range_window_pages = NonZeroUsize::new(1).unwrap();
    // Use the exact worker progress floor rather than the generous fixture budget.
    config.limits.request_context_bytes = NonZeroUsize::new(
        crate::telemetry::RESERVED_BYTES + protocol::MIN_REQUEST_CONTEXT_BYTES + 4 * 32 * 1024,
    )
    .unwrap();
    partition_limits(&config.limits, 1, false).unwrap();
    local_worker(&config, &Arc::new(NodeState::default()), 0)
}

#[test]
fn assembled_peer_io_carries_maximum_client_context_over_eight_signed_links() {
    let (app, runtime, _engine) = application();
    let io = app.peers.transport_io();
    let admission = &runtime.admission;
    let reactor = &runtime.reactor;
    reactor.init().unwrap();
    let baseline = admission.used(ResourceClass::RequestContext);
    let signers = network(protocol::MAX_HOPS + 1);
    let forwarding: Vec<_> = signers.iter().map(|s| Forwarding::new(s.clone())).collect();
    let codec = protocol::SecurityCodec::new(admission.clone(), BufferPool::new(admission.clone()));
    let crypto = CredentialCrypto::new(
        Rc::new(crate::test_support::security::keys()),
        admission.clone(),
    );
    let scope = scope(Duration::from_secs(30)).unwrap();
    let cache = CacheId(crate::test_support::security::CACHE.into());
    let metadata = vec![b'm'; 8192];
    let authorization = vec![b'a'; 8192];
    let etag = format!("\"{}\"", "v".repeat(8190));
    let head = MessageHead {
        start: StartLine::Request {
            method: "HEAD".into(),
            target: format!("/v2/objects/{}", "ab".repeat(32)),
        },
        headers: vec![
            Header {
                name: "Host".into(),
                value: b"racer".to_vec(),
            },
            Header {
                name: "If-Match".into(),
                value: etag.as_bytes().to_vec(),
            },
            Header {
                name: "Racer-Metadata".into(),
                value: metadata.clone(),
            },
            Header {
                name: "Authorization".into(),
                value: authorization.clone(),
            },
        ],
    };
    let client_codec = Codec::new(32 * 1024);
    let raw = client_codec.encode_head(&head).unwrap();
    let parsed = RequestParser::new(32 * 1024)
        .parse(&cache, client_codec.decode_head(&raw).unwrap().unwrap().0)
        .unwrap();
    let pin = parsed.kind.pin().unwrap().clone();
    let object = parsed.origin.object.clone();
    let attempt = AttemptId([2; 16]);
    let origin = crypto.seal(&parsed.origin, attempt, &scope).unwrap();
    assert_eq!(
        origin.authorization.as_ref().unwrap().ciphertext.len(),
        8208
    );
    let ciphertext = origin.authorization.as_ref().unwrap().ciphertext.clone();
    let request = PeerRequest {
        operation: PeerOperation::Metadata {
            object: object.clone(),
            selector: MetadataSelector::Pinned(pin.clone()),
            mode: FetchMode::Acquire,
        },
        origin,
        route: RouteBudget {
            membership: MembershipVersion(1),
            request: scope.request,
            attempt,
            destination: node(protocol::MAX_HOPS),
            visited: vec![node(0)],
            remaining_links: protocol::MAX_HOPS as u8,
            remaining_attempts: 1,
            deadline: scope.deadline,
        },
    };
    let (mut request, binding) = forwarding[0].sign_request_to(request, &node(1)).unwrap();
    assert!(
        encode_envelope(&request.authentication, false, 0)
            .unwrap()
            .unique("racer-original")
            .unwrap()
            .unwrap()
            .len()
            > 32 * 1024
    );
    let mut bindings = vec![binding];
    let mut sockets = Vec::new();
    let mut destination = None;
    for index in 1..=protocol::MAX_HOPS {
        let (left, right) = UnixStream::pair().unwrap();
        let left = crate::http::from_accepted(left.into(), admission).unwrap();
        let right = crate::http::from_accepted(right.into(), admission).unwrap();
        let next = node(index);
        let (left, right) = drive_peer(reactor, async {
            futures::try_join!(
                connection::connect(io, left, signers[index - 1].clone(), &next, &scope),
                connection::accept(io, right, signers[index].clone(), &scope)
            )
        })
        .unwrap();
        let head = encode_envelope(&request.authentication, false, 0).unwrap();
        let (sent, received) = drive_peer(reactor, async {
            futures::try_join!(
                io.send_head(left, head, &scope),
                io.receive_head(right, &scope)
            )
        })
        .unwrap();
        sockets.push((sent.connection, received.connection));
        let (auth, length) = decode_envelope(received.value, false).unwrap();
        assert_eq!(length, 0);
        assert_eq!(auth.hops.len(), index - 1);
        let decoded = codec.request(auth, &scope).unwrap();
        assert_eq!(
            decoded
                .request
                .origin
                .authorization
                .as_ref()
                .unwrap()
                .ciphertext,
            ciphertext
        );
        let verified = forwarding[index].verify_request(decoded).unwrap();
        bindings.push(verified.binding().clone());
        drop(request);
        if index == protocol::MAX_HOPS {
            destination = Some(verified);
            break;
        }
        let mut route = verified.request().route.clone();
        route.visited.push(node(index));
        route.remaining_links -= 1;
        request = forwarding[index]
            .append_request(verified, &node(index + 1), route)
            .unwrap();
    }
    let destination = destination.unwrap();
    let mut response = forwarding[protocol::MAX_HOPS]
        .sign_response(
            destination.binding(),
            PeerResponse::Metadata(ObjectMetadata {
                content_type: None,
                version: ObjectVersion { object, etag: pin },
                length: 42,
                expires_at: ExpiresAt::test_time(std::time::SystemTime::now()),
            }),
        )
        .unwrap();
    let opened = crypto
        .open_charged(
            destination.into_signed().request.origin,
            scope.request,
            attempt,
        )
        .unwrap();
    assert_eq!(opened.metadata.as_ref().unwrap().as_header(), metadata);
    assert_eq!(
        opened.authorization.as_ref().unwrap().expose_for_origin(),
        authorization
    );
    drop(opened);
    for index in (1..=protocol::MAX_HOPS).rev() {
        let (left, right) = sockets.pop().unwrap();
        let head = encode_envelope(&response.authentication, true, 0).unwrap();
        let (sent, received) = drive_peer(reactor, async {
            futures::try_join!(
                io.send_head(right, head, &scope),
                io.receive_head(left, &scope)
            )
        })
        .unwrap();
        let (auth, _) = decode_envelope(received.value, true).unwrap();
        let verified = forwarding[index - 1]
            .verify_response(
                codec.response(auth, vec![], &scope).unwrap(),
                &bindings[index - 1],
            )
            .unwrap();
        assert_eq!(
            verified.signed().authentication.hops.len(),
            protocol::MAX_HOPS - index
        );
        let PeerResponse::Metadata(result) = verified.response() else {
            panic!("expected metadata")
        };
        assert_eq!(result.version.etag.as_str(), etag);
        let mut left = received.connection;
        let mut right = sent.connection;
        left.finish_exchange().unwrap();
        right.finish_exchange().unwrap();
        if index > 1 {
            response = forwarding[index - 1]
                .append_response(verified, &node(index - 2))
                .unwrap();
        }
    }
    drive_peer(reactor, reactor.drain()).unwrap();
    io.reclaim_buffer();
    assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
    assert_eq!(admission.used(ResourceClass::Connection), 0);
}

#[test]
fn peer_worker_partition_rejects_underfunding_and_reduces_worker_count() {
    use uring_runtime::group::affinity::CpuLocation;
    use uring_runtime::group::affinity::EffectiveTopology;
    let mut config = crate::test_support::cluster::config(false);
    config.max_threads = 4;
    config.limits.range_window_pages = NonZeroUsize::new(1).unwrap();
    config.limits.connections_per_neighbor = NonZeroUsize::new(1).unwrap();
    // Fund control progress on all three planned shards so only context bytes
    // determine which worker counts pass the boundary assertions below.
    config.limits.client_connections = NonZeroUsize::new(36).unwrap();
    let ordinary_floor = protocol::MIN_REQUEST_CONTEXT_BYTES
        + 4 * config.limits.header_bytes.get().max(MAX_FIELD_BYTES);
    let floor = crate::telemetry::RESERVED_BYTES + ordinary_floor;
    for budget in [128 * 1024, floor - 1, floor, 2 * floor - 1, 2 * floor] {
        config.limits.request_context_bytes = NonZeroUsize::new(budget).unwrap();
        assert_eq!(
            partition_limits(&config.limits, 1, false).is_ok(),
            budget >= floor
        );
        assert_eq!(
            partition_limits(&config.limits, 2, false).is_ok(),
            budget >= 2 * floor
        );
        let mut plan = AffinityPlan::from_topology(
            &config,
            EffectiveTopology {
                cpus: (0..4)
                    .map(|cpu| CpuLocation {
                        cpu,
                        package: 0,
                        core: cpu,
                        numa_node: None,
                    })
                    .collect(),
                quota: None,
                nics: vec![],
            },
            &[],
        )
        .unwrap();
        assert_eq!(plan.pairs.len(), 3);
        let result = size_workers(&config.limits, &mut plan, false);
        if budget < floor {
            assert!(matches!(result, Err(Error::InvalidConfiguration)));
        } else {
            let limits = result.unwrap();
            assert_eq!(plan.pairs.len(), (budget / floor).min(2));
            assert!(limits.request_context_bytes.get() >= floor);
            let admission = flow_control::Quotas::new(AdmissionPolicy::new(limits));
            let diagnostics = admission
                .reserve(
                    None,
                    ResourceClass::RequestContext,
                    crate::telemetry::RESERVED_BYTES,
                )
                .unwrap();
            let ordinary = admission
                .reserve(None, ResourceClass::RequestContext, ordinary_floor)
                .unwrap();
            assert_eq!(admission.used(ResourceClass::RequestContext), floor);
            drop((diagnostics, ordinary));
            assert_eq!(admission.used(ResourceClass::RequestContext), 0);
        }
    }
}

#[test]
fn assembled_peer_io_rejects_oversize_and_admission_pressure_before_submission() {
    use std::io::Read;
    use std::io::Write;
    let (app, runtime, _engine) = application();
    let io = app.peers.transport_io();
    let admission = &runtime.admission;
    let reactor = &runtime.reactor;
    reactor.init().unwrap();
    let baseline = admission.used(ResourceClass::RequestContext);
    let scope = scope(Duration::from_secs(30)).unwrap();
    let head = || MessageHead {
        start: StartLine::Request {
            method: "POST".into(),
            target: protocol::REQUEST_TARGET.into(),
        },
        headers: vec![Header {
            name: "x".into(),
            value: vec![b'x'; protocol::MAX_ENVELOPE_HEAD],
        }],
    };
    let (socket, _other) = UnixStream::pair().unwrap();
    let conn = crate::http::from_accepted(socket.into(), admission).unwrap();
    assert!(matches!(
        drive_peer(reactor, io.send_head(conn, head(), &scope)),
        Err(Error::HeaderTooLarge)
    ));
    let (socket, mut other) = UnixStream::pair().unwrap();
    let conn = crate::http::from_accepted(socket.into(), admission).unwrap();
    let writer = std::thread::spawn(move || {
        let _ = other.write_all(&vec![b'x'; protocol::MAX_ENVELOPE_HEAD + 1]);
    });
    assert!(matches!(
        drive_peer(reactor, io.receive_head(conn, &scope)),
        Err(Error::HeaderTooLarge)
    ));
    writer.join().unwrap();
    io.reclaim_buffer();
    assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
    let response = || MessageHead {
        start: StartLine::Response { status: 200 },
        headers: vec![Header {
            name: "content-length".into(),
            value: b"0".to_vec(),
        }],
    };
    let encoded = Codec::new(protocol::MAX_ENVELOPE_HEAD)
        .encode_head(&response())
        .unwrap();
    let send_budget = protocol::MAX_ENVELOPE_HEAD + encoded.len();
    // Send scratch uses the head cap, but staging uses the actual encoded size.
    // Insufficient receive staging, send scratch, or send staging rejects before I/O.
    for (send, available, succeeds) in [
        (false, 4096 - 1, false),
        (true, protocol::MAX_ENVELOPE_HEAD - 1, false),
        (true, send_budget - 1, false),
        (true, send_budget, true),
    ] {
        let held = admission
            .reserve(
                None,
                ResourceClass::RequestContext,
                admission.policy().limits().request_context_bytes.get() - baseline - available,
            )
            .unwrap();
        let baseline = admission.used(ResourceClass::RequestContext);
        let (socket, mut other) = UnixStream::pair().unwrap();
        other
            .set_read_timeout(Some(Duration::from_secs(1)))
            .unwrap();
        let conn = crate::http::from_accepted(socket.into(), admission).unwrap();
        let operation = async {
            if send {
                io.send_head(conn, response(), &scope).await.map(|_| ())
            } else {
                io.receive_head(conn, &scope).await.map(|_| ())
            }
        };
        if succeeds {
            assert_eq!(drive_peer(reactor, operation), Ok(()));
            let mut received = vec![0; encoded.len()];
            other.read_exact(&mut received).unwrap();
            assert_eq!(received, encoded);
        } else {
            let mut operation = std::pin::pin!(operation);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert_eq!(
                operation.as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::Overloaded)),
                "send={send}, available={available}"
            );
            // The failed admission must close without sending even a partial head.
            let mut byte = [0];
            assert_eq!(other.read(&mut byte).unwrap(), 0);
        }
        assert_eq!(reactor.in_flight(), 0);
        io.reclaim_buffer();
        assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
        drop(held);
    }
}

mod cache_publication {
    use super::*;
    #[test]
    fn publication_waits_for_every_worker_and_failed_acceptance_rolls_back_preparation() {
        let config = crate::test_support::cluster::config(false);
        let node = Arc::new(NodeState::default());
        let (mut worker, _, _) = crate::app::tests::local_worker(&config, &node, 0);
        let (mut second, _, _) = crate::app::tests::local_worker(&config, &node, 1);
        let adapter = CachePublication {
            proposal: RefCell::new(None),
            node: node.clone(),
            listeners: worker.prepared_listeners.clone(),
            capacity: config.limits.metadata_entries.get(),
        };
        assert!(matches!(adapter.stage(&[]), Err(Error::Unavailable)));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        worker.poll_cache_preparation(&mut cx).unwrap();
        assert!(matches!(adapter.stage(&[]), Err(Error::Unavailable)));
        assert!(worker.prepared_listeners.borrow().is_some());
        second.poll_cache_preparation(&mut cx).unwrap();
        drop(adapter.stage(&[]).unwrap());
        assert!(matches!(adapter.stage(&[]), Err(Error::Unavailable)));
        worker.poll_cache_preparation(&mut cx).unwrap();
        second.poll_cache_preparation(&mut cx).unwrap();
        for _ in 0..2 {
            adapter.stage(&[]).unwrap().commit();
            assert!(
                node.cache_rollout
                    .committed(adapter.proposal.borrow().as_ref().unwrap().0)
                    .unwrap()
            );
        }
    }
    #[test]
    fn capacity_failure_keeps_last_good_generation_and_uid_reuse_needs_no_tombstones() {
        let config = crate::test_support::cluster::config(false);
        let node = Arc::new(NodeState::default());
        let definition = crate::app::tests::definition();
        let store = PublicationTarget::new(config.cluster.clone(), node.publications.clone());
        store
            .apply(crate::app::tests::publication(
                &config,
                1,
                vec![definition.clone()],
            ))
            .unwrap();
        let mut adapter = CachePublication {
            proposal: RefCell::new(None),
            node: node.clone(),
            listeners: Rc::new(RefCell::new(None)),
            capacity: 0,
        };
        assert!(matches!(
            adapter.stage(std::slice::from_ref(&definition)),
            Err(Error::Overloaded)
        ));
        assert!(adapter.proposal.borrow().is_none());
        adapter.capacity = 16;
        assert!(matches!(adapter.stage(&[]), Err(Error::Unavailable)));
        let previous = adapter.proposal.borrow().as_ref().unwrap().0;
        assert!(matches!(
            adapter.stage(&[definition]),
            Err(Error::Unavailable)
        ));
        assert_eq!(adapter.proposal.borrow().as_ref().unwrap().0, previous + 1);
        assert_eq!(
            store.cursor().unwrap(),
            Some(racer_control_wire::PublicationSequence(1))
        );
    }
}

mod lifecycle {
    use super::*;
    #[test]
    fn membership_attestation_requires_fresh_matching_workers() {
        use racer_control_wire::PublicationSequence;
        let observations = Observations::default();
        let now = uring_runtime::environment::now();
        let resources = Resources {
            workers_usable: true,
            observed_until: Some(now + Duration::from_secs(2)),
            ..Default::default()
        };
        let mut diagnostic = crate::telemetry::MembershipDiagnostic {
            accepted_sequence: 9,
            accepted_membership: 7,
            accepted_hash: [42; 32],
            ..Default::default()
        };
        observations
            .record_snapshot(WorkerId(0), resources, 2, Some(PublicationSequence(9)))
            .unwrap();
        observations
            .record_snapshot(WorkerId(1), resources, 2, Some(PublicationSequence(8)))
            .unwrap();
        observations.membership_workers(&mut diagnostic, 2).unwrap();
        assert_eq!(diagnostic.matching_workers, 1);
        assert!(!diagnostic.fully_applied());
        observations
            .record_snapshot(WorkerId(1), resources, 2, Some(PublicationSequence(9)))
            .unwrap();
        observations.membership_workers(&mut diagnostic, 2).unwrap();
        assert!(diagnostic.fully_applied());
        diagnostic.pending_sequence = 10;
        diagnostic.pending_membership = 8;
        assert!(!diagnostic.fully_applied());
        diagnostic.pending_sequence = 0;
        observations
            .record_snapshot(
                WorkerId(1),
                Resources {
                    observed_until: Some(now),
                    ..resources
                },
                2,
                Some(PublicationSequence(9)),
            )
            .unwrap();
        observations.membership_workers(&mut diagnostic, 2).unwrap();
        assert!(!diagnostic.fully_applied());
    }
    #[test]
    fn readiness_requires_every_worker_and_expires_without_progress() {
        let observations = Observations::default();
        let now = uring_runtime::environment::now();
        let resources = Resources {
            workers_usable: true,
            storage_usable: true,
            listeners_usable: true,
            membership_usable: true,
            admission_usable: true,
            credentials_valid_until: Some(now + Duration::from_secs(10)),
            observed_until: Some(now + Duration::from_secs(1)),
        };
        observations.record(WorkerId(0), resources, 2).unwrap();
        assert!(!observations.health.ready());
        observations.record(WorkerId(1), resources, 2).unwrap();
        assert!(observations.health.ready());
        assert_eq!(
            observations.health.state_at(now + Duration::from_secs(2)),
            Ok(State::Degraded)
        );
        observations
            .record(
                WorkerId(1),
                Resources {
                    storage_usable: false,
                    ..resources
                },
                2,
            )
            .unwrap();
        assert!(!observations.health.ready());
        observations.health.transition(State::Draining).unwrap();
        observations.record(WorkerId(1), resources, 2).unwrap();
        assert_eq!(observations.health.state(), Ok(State::Draining));
    }
}

mod dst;
