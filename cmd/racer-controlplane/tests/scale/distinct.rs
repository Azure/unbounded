// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Opt-in measured TLS fanout against the production Router. The enrollment
//! fixture uses signed claims and bounded CA state; it does not benchmark issuance.
//!
//! Historical pre-stateless release runs (before the short-deadline hardening):
//! - 10,000 nodes / 1 universe: 12,375,143,725 bytes per fleet publication,
//!   initial 33.964s, publish plus TLS reconnect 34.502s, process peak 1,062,880KiB.
//! - 10,000 nodes / 10 universes: 15,149,268,618 bytes per publication,
//!   initial 65.940s, publish plus reconnect 68.897s, peak 1,288,972KiB.
//! Host: AMD EPYC 9V74 VM, 48 logical CPUs / 24 exposed cores; 16 Tokio workers.
//! CPU/RSS include BOTH client and server in one process; traffic is loopback.
//! This is not enrollment, Kubernetes-store, NIC, or remote-client throughput.
//! First-byte tails at 10k were not successfully measured: that rerun was
//! interrupted. The tightened stress deadlines intentionally reject the older
//! ten-universe timings rather than accepting minutes of silent waiting.
//! Production 35-second first-byte compliance and slow-store fleet convergence
//! remain unverified. Reproduction needs >20,000 file descriptors, sufficient
//! ephemeral ports, and RAM beyond the observed ~1.3GiB process peak (including
//! kernel socket buffers). A 24-node / 3-universe / 512-slot default scenario
//! covers the protocol without claiming full-geometry throughput.
//! See designs/racer-controlplane-scale-testing.md for capacity qualification,
//! bounded reproductions, and the shared-host first-byte deadline failure.

use super::*;
use crate::{
    kubernetes::RANGE_SIZE,
    model::{Member, SLOT_COUNT, Volume},
    publication::Publication,
    security::{
        Authority, CaState, Identity, IdentityKind, SignedClaims, StateImage, TlsSnapshot,
        certificate_claims,
    },
    topology::place_in_universe,
};
use openssl::{
    asn1::Asn1Time,
    bn::BigNum,
    ec::{EcGroup, EcKey},
    hash::MessageDigest,
    nid::Nid,
    pkey::{PKey, Private},
    x509::{
        X509, X509NameBuilder,
        extension::{BasicConstraints, ExtendedKeyUsage, KeyUsage, SubjectAlternativeName},
    },
};
use serde_json::{Value, json};
use std::sync::atomic::{AtomicUsize, Ordering};
use tokio::{
    io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader},
    net::{TcpListener, TcpStream},
    sync::mpsc,
};

const CLIENT_STAGES: [&str; 11] = [
    "not_started",
    "connection_admission",
    "tcp_connect",
    "tls_handshake",
    "request_write",
    "response_status_line",
    "response_headers",
    "response_body",
    "response_validation",
    "reconnect_admission",
    "finished",
];
const CLIENT_PHASES: [&str; 3] = ["initial", "publication", "reconnect"];

// Synthetic reserved revisions are distinct across universes and skip a range
// between publications. This router fixture does not exercise checkpoint CAS.
fn revisions(universe: usize) -> (u64, u64) {
    (
        RANGE_SIZE + universe as u64 + 1,
        2 * RANGE_SIZE + universe as u64 + 1,
    )
}

struct Progress {
    started: Instant,
    stage: Mutex<&'static str>,
    clients: Vec<AtomicUsize>,
    phases: Vec<AtomicUsize>,
    credentials: AtomicUsize,
    // Initial deliveries, successor deliveries, and reconnect request writes.
    completed: [AtomicUsize; 3],
    bytes: AtomicUsize,
    subscriptions: Mutex<Option<Arc<Subscriptions>>>,
}

impl Progress {
    fn new(nodes: usize) -> Arc<Self> {
        Arc::new(Self {
            started: Instant::now(),
            stage: Mutex::new("setup"),
            clients: (0..nodes).map(|_| AtomicUsize::new(0)).collect(),
            phases: (0..nodes).map(|_| AtomicUsize::new(0)).collect(),
            credentials: AtomicUsize::new(0),
            completed: std::array::from_fn(|_| AtomicUsize::new(0)),
            bytes: AtomicUsize::new(0),
            subscriptions: Mutex::new(None),
        })
    }

    fn enter(&self, stage: &'static str) {
        *self.stage.lock().unwrap() = stage;
        self.report("stage");
    }

    fn report(&self, reason: &str) {
        let mut clients = [[0usize; CLIENT_STAGES.len()]; CLIENT_PHASES.len()];
        for (state, phase) in self.clients.iter().zip(&self.phases) {
            clients[phase.load(Ordering::Relaxed)][state.load(Ordering::Relaxed)] += 1;
        }
        let states: BTreeMap<_, _> = CLIENT_PHASES
            .into_iter()
            .zip(clients.map(|counts| {
                CLIENT_STAGES
                    .into_iter()
                    .zip(counts)
                    .collect::<BTreeMap<_, _>>()
            }))
            .collect();
        // Failure reporting must not wait behind the very server locks whose
        // contention we are diagnosing, or panic again on a poisoned lock.
        let server = self
            .subscriptions
            .try_lock()
            .ok()
            .and_then(|subscriptions| {
                subscriptions.as_ref().map(|s| {
                    let waiters = s.universes.try_read().ok().map(|universes| {
                        universes
                            .values()
                            .map(|p| p.changes.receiver_count())
                            .sum::<usize>()
                    });
                    let cache = s
                        .cache
                        .try_lock()
                        .ok()
                        .map(|cache| (cache.bytes, cache.digests.len()));
                    json!({"waiters":waiters,"responses":s.response_count(),
                    "builder_permits_available":s.builders.available_permits(),"cache":cache})
                })
            });
        let stage = self.stage.try_lock().ok().map(|stage| *stage);
        eprintln!(
            "SCALE_PROGRESS {}",
            json!({"reason":reason,"stage":stage,
                "elapsed_seconds":self.started.elapsed().as_secs_f64(),
                "nodes":self.clients.len(),"credentials":self.credentials.load(Ordering::Relaxed),
                "initial_delivered":self.completed[0].load(Ordering::Relaxed),
                "published_delivered":self.completed[1].load(Ordering::Relaxed),
                "reconnect_requests_written":self.completed[2].load(Ordering::Relaxed),
                "validated_bytes":self.bytes.load(Ordering::Relaxed),
                "clients":states,"server":server,"resources":sample()})
        );
    }
}

// Independent of Tokio scheduling, so executor starvation still leaves evidence.
struct Reporter(std::sync::mpsc::Sender<()>, Arc<Progress>);
impl Reporter {
    fn start(progress: Arc<Progress>) -> Self {
        let (send, receive) = std::sync::mpsc::channel();
        let observer = progress.clone();
        std::thread::spawn(move || {
            while matches!(
                receive.recv_timeout(Duration::from_secs(5)),
                Err(std::sync::mpsc::RecvTimeoutError::Timeout)
            ) {
                observer.report("heartbeat");
            }
        });
        Self(send, progress)
    }
}
impl Drop for Reporter {
    fn drop(&mut self) {
        let _ = self.0.send(());
        self.1.report(if std::thread::panicking() {
            "unwinding"
        } else {
            "scenario_stopped"
        });
    }
}

struct ClientProgress {
    fleet: Arc<Progress>,
    index: usize,
}
impl ClientProgress {
    fn enter(&self, stage: usize) {
        self.fleet.clients[self.index].store(stage, Ordering::Relaxed);
    }

    async fn event(
        &self,
        events: &mpsc::Sender<(u32, usize, f64)>,
        event: (u32, usize, f64),
    ) -> bool {
        if events.send(event).await.is_err() {
            // The coordinator has already failed or canceled the scenario.
            return false;
        }
        self.fleet.completed[event.0 as usize].fetch_add(1, Ordering::Relaxed);
        self.fleet.bytes.fetch_add(event.1, Ordering::Relaxed);
        true
    }

    async fn bounded<T>(
        &self,
        seconds: u64,
        stage: usize,
        future: impl std::future::Future<Output = T>,
    ) -> T {
        self.enter(stage);
        tokio::time::timeout(Duration::from_secs(seconds), future)
            .await
            .unwrap_or_else(|_| {
                self.fleet.report("client_deadline");
                panic!(
                    "distinct TLS deadline: client={} phase={} operation={} ({seconds}s)",
                    self.index,
                    CLIENT_PHASES[self.fleet.phases[self.index].load(Ordering::Relaxed)],
                    CLIENT_STAGES[stage]
                )
            })
    }
}

// A real thread still fires if the executor or a blocking task deadlocks. The
// parent test process is terminated only after its own explicitly bounded test.
struct Watchdog(std::sync::mpsc::Sender<()>);
impl Watchdog {
    fn start(seconds: u64) -> Self {
        let (send, receive) = std::sync::mpsc::channel();
        std::thread::spawn(move || {
            if receive.recv_timeout(Duration::from_secs(seconds)).is_err() {
                eprintln!(
                    "distinct TLS test watchdog expired after {seconds}s; terminating this test process"
                );
                std::process::exit(124);
            }
        });
        Self(send)
    }
}
impl Drop for Watchdog {
    fn drop(&mut self) {
        let _ = self.0.send(());
    }
}

async fn bounded<T>(
    seconds: u64,
    operation: &str,
    future: impl std::future::Future<Output = T>,
) -> T {
    tokio::time::timeout(Duration::from_secs(seconds), future)
        .await
        .unwrap_or_else(|_| panic!("distinct TLS deadline: {operation} ({seconds}s)"))
}

fn run_bounded(nodes: usize, universes: usize, slots: u32, stress: bool) {
    run_bounded_churn(nodes, universes, slots, stress, false);
}

fn run_bounded_churn(nodes: usize, universes: usize, slots: u32, stress: bool, churn: bool) {
    assert!(
        universes > 0 && nodes >= universes && nodes <= 16000 && nodes.is_multiple_of(universes),
        "scale geometry requires 1 <= universes <= nodes <= 16000 and evenly divided universes"
    );
    let _watchdog = Watchdog::start(if stress { 150 } else { 20 });
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(if stress { 16 } else { 4 })
        .enable_all()
        .build()
        .unwrap();
    let progress = Progress::new(nodes);
    let _reporter = Reporter::start(progress.clone());
    let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        runtime.block_on(bounded(
            if stress { 140 } else { 12 },
            "whole scenario",
            scenario(nodes, universes, slots, stress, churn, progress.clone()),
        ))
    }));
    if result.is_err() {
        progress.report("failure");
    }
    runtime.shutdown_timeout(Duration::from_secs(2));
    if let Err(panic) = result {
        std::panic::resume_unwind(panic);
    }
}

async fn next_event(
    receiver: &mut mpsc::Receiver<(u32, usize, f64)>,
    workers: &mut tokio::task::JoinSet<()>,
    deadline: tokio::time::Instant,
    stage: &str,
    completed: usize,
    progress: &Progress,
) -> (u32, usize, f64) {
    let failure = tokio::select! {
        biased;
        result = workers.join_next() => format!("{stage}: worker ended before stage completion ({completed}): {result:?}"),
        _ = tokio::time::sleep_until(deadline) => format!("{stage}: stage deadline after {completed} completions"),
        event = receiver.recv() => match event {
            Some(event) => return event,
            None => format!("{stage}: event channel closed after {completed}"),
        },
    };
    progress.report("stage_failure");
    // Abort and reap siblings while the receiver is still alive. A failed stage
    // remains a failure even if cancellation itself cannot finish promptly.
    let cleanup = tokio::time::timeout(Duration::from_secs(2), workers.shutdown()).await;
    panic!(
        "{failure}; worker cancellation completed={}",
        cleanup.is_ok()
    );
}

async fn await_waiters(state: &Subscriptions, count: usize, seconds: u64, stage: &str) {
    let deadline = tokio::time::Instant::now() + Duration::from_secs(seconds);
    loop {
        let actual = state.waiter_count();
        if actual == count {
            return;
        }
        assert!(
            tokio::time::Instant::now() < deadline,
            "{stage}: waiters={actual}, expected={count}, responses={}, cache={:?}",
            state.response_count(),
            state.cache_usage()
        );
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
}

fn leaf(authority: &Authority, identity: &Identity, serial: usize) -> (Vec<u8>, Vec<u8>) {
    let parent = X509::from_pem(authority.certificate.as_bytes()).unwrap();
    let signer: PKey<Private> =
        PKey::private_key_from_pem(authority.private_key.as_bytes()).unwrap();
    let key = PKey::from_ec_key(
        EcKey::generate(&EcGroup::from_curve_name(Nid::X9_62_PRIME256V1).unwrap()).unwrap(),
    )
    .unwrap();
    let mut cert = X509::builder().unwrap();
    cert.set_version(2).unwrap();
    cert.set_serial_number(
        &BigNum::from_u32(serial as u32 + 1)
            .unwrap()
            .to_asn1_integer()
            .unwrap(),
    )
    .unwrap();
    cert.set_subject_name(&X509NameBuilder::new().unwrap().build())
        .unwrap();
    cert.set_issuer_name(parent.subject_name()).unwrap();
    cert.set_pubkey(&key).unwrap();
    cert.set_not_before(&Asn1Time::from_unix(unix_now() - 60).unwrap())
        .unwrap();
    // The fixture reserves one issuer expiry watermark for the entire fleet.
    // Every signed leaf is bounded by it, independent of credential count.
    let expiry = authority.last_issued_expiry - 3600;
    assert!(expiry > unix_now());
    cert.set_not_after(&Asn1Time::from_unix(expiry).unwrap())
        .unwrap();
    cert.append_extension(BasicConstraints::new().critical().build().unwrap())
        .unwrap();
    cert.append_extension(
        KeyUsage::new()
            .critical()
            .digital_signature()
            .build()
            .unwrap(),
    )
    .unwrap();
    let mut eku = ExtendedKeyUsage::new();
    eku.server_auth();
    if identity.kind == IdentityKind::Node {
        eku.client_auth();
    }
    cert.append_extension(eku.build().unwrap()).unwrap();
    let mut san = SubjectAlternativeName::new();
    let claims = SignedClaims {
        version: 1,
        namespace: "system".into(),
        identity: identity.clone(),
    };
    san.uri(&claims.uri().unwrap());
    if identity.kind == IdentityKind::ControlPlane {
        san.dns("racer-controlplane.system.svc");
    }
    cert.append_extension(
        san.critical()
            .build(&cert.x509v3_context(Some(&parent), None))
            .unwrap(),
    )
    .unwrap();
    cert.sign(&signer, MessageDigest::sha256()).unwrap();
    let cert = cert.build();
    assert_eq!(certificate_claims(&cert.to_pem().unwrap()).unwrap(), claims);
    (cert.to_der().unwrap(), key.private_key_to_pkcs8().unwrap())
}

fn sample() -> Value {
    // Diagnostics must not replace the original failure if procfs is unavailable.
    let read = |path| std::fs::read_to_string(path).ok();
    let status = read("/proc/self/status").unwrap_or_default();
    let kb = |key: &str| {
        status
            .lines()
            .find_map(|line| line.strip_prefix(key))
            .and_then(|v| v.split_whitespace().next())
            .and_then(|s| s.parse::<u64>().ok())
    };
    let stat = read("/proc/self/stat").unwrap_or_default();
    let fields: Vec<_> = stat
        .rsplit_once(')')
        .map(|(_, fields)| fields)
        .unwrap_or_default()
        .split_whitespace()
        .collect();
    let ticks = |index| fields.get(index).and_then(|s: &&str| s.parse::<u64>().ok());
    // Raw ticks avoid assuming USER_HZ for CPU-time diagnostics.
    json!({"cpu_user_ticks":ticks(11),"cpu_system_ticks":ticks(12),
        "rss_kib":kb("VmRSS:"),"peak_rss_kib":kb("VmHWM:"),"threads":kb("Threads:"),
        "cpu_affinity":status.lines().find(|line| line.starts_with("Cpus_allowed_list:")),
        "available_parallelism":std::thread::available_parallelism().ok().map(|n| n.get()),
        "open_fds":std::fs::read_dir("/proc/self/fd").ok().map(|entries| entries.count()),
        "loadavg":read("/proc/loadavg"),
        "mem_available":read("/proc/meminfo").and_then(|s| s.lines().find(|l| l.starts_with("MemAvailable:")).map(str::to_owned)),
        "cpu_pressure":read("/proc/pressure/cpu"),"memory_pressure":read("/proc/pressure/memory"),
        "cgroup":read("/proc/self/cgroup"),
        "cgroup_cpu_max":read("/sys/fs/cgroup/cpu.max"),
        "cgroup_cpu_stat":read("/sys/fs/cgroup/cpu.stat"),
        "cgroup_memory_max":read("/sys/fs/cgroup/memory.max"),
        "cgroup_memory_current":read("/sys/fs/cgroup/memory.current")})
}

type TlsClient = BufReader<tokio_rustls::client::TlsStream<TcpStream>>;
async fn connect(
    address: std::net::SocketAddr,
    config: Arc<rustls::ClientConfig>,
    progress: &ClientProgress,
) -> TlsClient {
    let raw = progress
        .bounded(3, 2, TcpStream::connect(address))
        .await
        .unwrap();
    raw.set_nodelay(true).unwrap();
    BufReader::new(
        progress
            .bounded(
                3,
                3,
                tokio_rustls::TlsConnector::from(config).connect(
                    rustls::pki_types::ServerName::try_from("racer-controlplane.system.svc")
                        .unwrap(),
                    raw,
                ),
            )
            .await
            .unwrap(),
    )
}
async fn send(stream: &mut TlsClient, boot: &str, cursor: &str, progress: &ClientProgress) {
    progress.bounded(3,4,stream.write_all(format!("GET /v1/config HTTP/1.1\r\nHost: racer-controlplane.system.svc\r\nContent-Length: 0\r\nX-Racer-Boot: {boot}\r\nX-Racer-Profile: 1\r\nX-Racer-Cursor: {cursor}\r\nX-Racer-Applied-Revision: 0\r\nX-Racer-Local-State: failed\r\n\r\n").as_bytes())).await.unwrap();
}
async fn receive(
    stream: &mut TlsClient,
    node: &str,
    progress: &ClientProgress,
) -> Option<(String, u64, usize, f64)> {
    let started = Instant::now();
    let mut line = String::new();
    assert!(
        progress
            .bounded(35, 5, stream.read_line(&mut line))
            .await
            .unwrap()
            > 0,
        "EOF before response status for {node}"
    );
    let first_byte = started.elapsed().as_secs_f64();
    assert!(
        line.starts_with("HTTP/1.1 200") || line.starts_with("HTTP/1.1 204"),
        "{line}"
    );
    let unchanged = line.starts_with("HTTP/1.1 204");
    let mut length = None;
    for _ in 0..64 {
        line.clear();
        assert!(
            progress
                .bounded(2, 6, stream.read_line(&mut line))
                .await
                .unwrap()
                > 0,
            "EOF inside headers for {node}"
        );
        if line == "\r\n" {
            break;
        }
        if let Some(s) = line.to_ascii_lowercase().strip_prefix("content-length:") {
            length = Some(s.trim().parse::<usize>().unwrap());
        }
    }
    assert_eq!(line, "\r\n", "too many response headers");
    if unchanged {
        assert!(length.is_none_or(|n| n == 0));
        return None;
    }
    let length = length.expect("200 requires content length");
    assert!(length <= 64 * 1024 * 1024);
    let mut body = vec![0; length];
    progress
        .bounded(5, 7, stream.read_exact(&mut body))
        .await
        .unwrap();
    progress.enter(8);
    let desired = proto::DesiredState::decode(body.as_slice()).unwrap();
    assert_eq!(hex::encode(&desired.node), node);
    let proto::configuration::Contents::Snapshot(snapshot) =
        desired.configuration.unwrap().contents.unwrap();
    assert_eq!(snapshot.node, desired.node);
    assert_eq!(snapshot.revision, desired.revision);
    assert_eq!(snapshot.epoch, desired.revision);
    assert_eq!(
        Sha256::digest(snapshot.encode_to_vec()).as_slice(),
        desired.snapshot_digest
    );
    Some((desired.cursor, desired.revision, body.len(), first_byte))
}

/// Run separately for each geometry for meaningful process peak-RSS readings:
/// RACER_SCALE_CAPACITY_QUALIFIED=1 RACER_SCALE_NODES=10000 RACER_SCALE_UNIVERSES=1 cargo test --release --lib
/// distinct_tls_fanout -- --ignored --nocapture
#[test]
#[ignore = "capacity-qualified release TLS load; see designs/racer-controlplane-scale-testing.md"]
fn distinct_tls_fanout() {
    assert!(
        !cfg!(debug_assertions),
        "distinct TLS scale measurement requires a release build; use distinct_tls_representative for routine coverage"
    );
    assert_eq!(
        std::env::var("RACER_SCALE_CAPACITY_QUALIFIED").as_deref(),
        Ok("1"),
        "explicit scale run requires RACER_SCALE_CAPACITY_QUALIFIED=1; see designs/racer-controlplane-scale-testing.md"
    );
    let nodes: usize = std::env::var("RACER_SCALE_NODES")
        .unwrap_or("10000".into())
        .parse()
        .unwrap();
    let universes: usize = std::env::var("RACER_SCALE_UNIVERSES")
        .unwrap_or("1".into())
        .parse()
        .unwrap();
    run_bounded(nodes, universes, SLOT_COUNT, true);
}

#[test]
fn distinct_tls_representative() {
    run_bounded(24, 3, 512, false);
}

#[test]
#[ignore = "1500-recipient full-geometry TLS churn; run separately in release"]
fn distinct_tls_rollout_churn() {
    assert!(!cfg!(debug_assertions), "run the churn scenario in release");
    run_bounded_churn(1500, 1, SLOT_COUNT, true, true);
}

#[tokio::test]
async fn queued_admission_refreshes_state_and_fails_closed() {
    let mut ca = Authority::generate(unix_now(), 86400, 60).unwrap();
    ca.last_issued_expiry = unix_now() + 7200;
    let identity = Identity {
        kind: IdentityKind::Node,
        universe: identity("universe", "queued"),
        node: identity("node", "queued"),
        pod_uid: "pod-queued".into(),
        pod_name: "queued".into(),
        boot_id: "a".repeat(64),
        container_id: String::new(),
    };
    let (cert, key) = leaf(&ca, &identity, 0);
    let (server_cert, server_key) = leaf(
        &ca,
        &Identity {
            kind: IdentityKind::ControlPlane,
            universe: String::new(),
            node: String::new(),
            pod_uid: "control".into(),
            pod_name: "control".into(),
            boot_id: "b".repeat(64),
            container_id: String::new(),
        },
        1,
    );
    let bundle = crate::security::TrustBundle {
        version: 1,
        generation: 1,
        active: ca.digest.clone(),
        certificates: ca.certificate.clone(),
    };
    let tls = TlsSnapshot::new(
        &bundle.json(),
        &X509::from_der(&server_cert).unwrap().to_pem().unwrap(),
        &PKey::private_key_from_pkcs8(&server_key)
            .unwrap()
            .private_key_to_pem_pkcs8()
            .unwrap(),
    )
    .unwrap();
    // Peer evidence must come from a real authenticated handshake, even though
    // the deterministic queue checks below call the handler directly.
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let accepting = tokio::spawn(async move {
        tls.accept(listener.accept().await.unwrap().0, true)
            .await
            .unwrap()
            .1
            .unwrap()
    });
    let mut roots = rustls::RootCertStore::empty();
    roots
        .add(
            X509::from_pem(ca.certificate.as_bytes())
                .unwrap()
                .to_der()
                .unwrap()
                .into(),
        )
        .unwrap();
    let client = rustls::ClientConfig::builder_with_provider(Arc::new(
        rustls::crypto::ring::default_provider(),
    ))
    .with_protocol_versions(&[&rustls::version::TLS13])
    .unwrap()
    .with_root_certificates(roots)
    .with_client_auth_cert(
        vec![cert.into()],
        rustls::pki_types::PrivateKeyDer::Pkcs8(key.into()),
    )
    .unwrap();
    let stream = tokio_rustls::TlsConnector::from(Arc::new(client))
        .connect(
            rustls::pki_types::ServerName::try_from("racer-controlplane.system.svc").unwrap(),
            TcpStream::connect(address).await.unwrap(),
        )
        .await
        .unwrap();
    let peer = accepting.await.unwrap();
    drop(stream);
    let image = StateImage {
        metadata: serde_json::to_vec(&json!({
            "version":5,"namespace":"system","fence":"scale","generation":1,
            "active":ca.digest,"phase":"stable","authorities":[ca],
            "rotation_nonce":"","published_at":null,"overlap_delay":60,"retirement_skew":60
        }))
        .unwrap(),
        shards: BTreeMap::new(),
    };
    let security = Arc::new(RwLock::new(Some(SecurityContext {
        fence: "scale".into(),
        state: Arc::new(CaState::from_image(&image).unwrap()),
    })));
    let getter = security.clone();
    let state = Subscriptions::new(
        Arc::new(move || getter.read().unwrap().clone()),
        1 << 20,
        Duration::from_millis(10),
    );
    state.set_fence(Some("scale".into()));
    let mut generation = Generation::empty("queued");
    generation.revision = 1;
    generation.nodes.insert(
        "queued".into(),
        Member {
            id: identity.node.clone(),
            ip: Some("10.0.0.1".parse().unwrap()),
            pod_uid: identity.pod_uid.clone(),
            pod_name: identity.pod_name.clone(),
            pod_namespace: "system".into(),
            fabric: String::new(),
        },
    );
    state.install(Arc::new(generation.clone())).unwrap();
    let mut headers = HeaderMap::new();
    headers.insert("x-racer-profile", "1".parse().unwrap());
    headers.insert("x-racer-boot", identity.boot_id.parse().unwrap());
    headers.insert("x-racer-storage-policy", "1".parse().unwrap());
    let policy = crate::storage::StoragePolicy::for_node("queued")
        .resolve("queued", Some("1Gi"), None)
        .unwrap();
    state.install_policy(Arc::new(policy));
    for action in ["publication", "revocation", "authority", "cancellation"] {
        state.set_live_selections(BTreeMap::from([(
            (identity.universe.clone(), identity.node.clone()),
            state.universes.read().unwrap()[&identity.universe].selections[&identity.node].clone(),
        )]));
        let held = state
            .responses
            .clone()
            .acquire_many_owned(16)
            .await
            .unwrap();
        let mut request = Box::pin(config_inner(
            state.clone(),
            Some(peer.clone()),
            headers.clone(),
        ));
        assert!(
            tokio::time::timeout(Duration::from_millis(10), &mut request)
                .await
                .is_err()
        );
        match action {
            "publication" => {
                generation.revision = 2;
                generation.nodes.get_mut("queued").unwrap().ip = Some("10.0.0.2".parse().unwrap());
                state.install(Arc::new(generation.clone())).unwrap();
                let mut policy = crate::storage::StoragePolicy::for_node("queued")
                    .resolve("queued", Some("2Gi"), None)
                    .unwrap();
                policy.version = 2;
                state.install_policy(Arc::new(policy));
            }
            "revocation" => state.set_live_selections(BTreeMap::new()),
            "authority" => *security.write().unwrap() = None,
            "cancellation" => {
                drop(request);
                drop(held);
                assert_eq!(state.response_count(), 0);
                break;
            }
            _ => unreachable!(),
        }
        drop(held);
        // A competing fleet fills the response queue behind this request. A
        // pre-admission revision mismatch must not yield this request's permit
        // and then wait behind that fleet a second time.
        let result = {
            let competitor = state.responses.clone().acquire_many_owned(16);
            tokio::pin!(competitor);
            let mut competing_permits = None;
            tokio::time::timeout(Duration::from_secs(2), async {
                tokio::select! {
                    biased;
                    result = &mut request => result,
                    held = &mut competitor => {
                        competing_permits = Some(held.unwrap());
                        request.await
                    }
                }
            })
            .await
            .expect("admitted request requeued behind the competing fleet")
        };
        match action {
            "publication" => {
                let body = axum::body::to_bytes(result.unwrap().into_body(), 1 << 20)
                    .await
                    .unwrap();
                let desired = proto::DesiredState::decode(body).unwrap();
                assert_eq!(desired.revision, 2);
                assert_eq!(desired.storage_policy.unwrap().desired_bytes, 2 << 30);
            }
            "revocation" => assert_eq!(result.unwrap_err(), StatusCode::FORBIDDEN),
            "authority" => {
                assert_eq!(result.unwrap_err(), StatusCode::SERVICE_UNAVAILABLE);
                *security.write().unwrap() = Some(SecurityContext {
                    fence: "scale".into(),
                    state: Arc::new(CaState::from_image(&image).unwrap()),
                });
            }
            _ => unreachable!(),
        }
        assert_eq!(state.response_count(), 0);
    }
}

#[tokio::test]
async fn scale_coordinator_cancels_siblings_on_failure() {
    use futures::FutureExt;

    let progress = Progress::new(2);
    let (_events, mut receiver) = mpsc::channel(2);
    let mut workers = tokio::task::JoinSet::new();
    let (alive, stopped) = tokio::sync::oneshot::channel::<()>();
    workers.spawn(async move {
        let _alive = alive;
        std::future::pending::<()>().await;
    });
    // A worker returning before completion is as fatal as a panic, without an
    // extra panic hook obscuring the coordinator's diagnostic in this regression.
    workers.spawn(async {});
    let result = std::panic::AssertUnwindSafe(next_event(
        &mut receiver,
        &mut workers,
        tokio::time::Instant::now() + Duration::from_secs(1),
        "injected worker exit",
        0,
        &progress,
    ))
    .catch_unwind()
    .await;
    assert!(result.is_err(), "worker loss must remain a failure");
    assert!(
        workers.is_empty(),
        "siblings must be reaped before unwinding"
    );
    assert!(
        stopped.await.is_err(),
        "cancellation must drop worker resources"
    );
}

#[tokio::test]
async fn scale_events_handle_success_and_receiver_cancellation() {
    let progress = ClientProgress {
        fleet: Progress::new(1),
        index: 0,
    };
    let (events, mut receiver) = mpsc::channel(1);
    assert!(progress.event(&events, (1, 42, 0.5)).await);
    assert_eq!(receiver.recv().await, Some((1, 42, 0.5)));
    assert_eq!(progress.fleet.completed[1].load(Ordering::Relaxed), 1);
    drop(receiver);
    assert!(!progress.event(&events, (2, 0, 0.)).await);
    assert_eq!(progress.fleet.completed[2].load(Ordering::Relaxed), 0);
}

#[test]
fn scale_diagnostics_do_not_wait_for_server_locks() {
    let progress = Progress::new(1);
    let subscriptions = Subscriptions::new(Arc::new(|| None), 1024, Duration::from_secs(1));
    *progress.subscriptions.lock().unwrap() = Some(subscriptions.clone());
    let _universes = subscriptions.universes.write().unwrap();
    let _cache = subscriptions.cache.lock().unwrap();
    let _stage = progress.stage.lock().unwrap();
    // Calling a blocking getter here would deadlock on this thread's own locks.
    progress.report("injected busy server locks");
}

#[tokio::test]
async fn scale_expired_stage_cancels_pending_worker() {
    use futures::FutureExt;

    let progress = Progress::new(1);
    let (_events, mut receiver) = mpsc::channel(1);
    let mut workers = tokio::task::JoinSet::new();
    workers.spawn(std::future::pending::<()>());
    let result = std::panic::AssertUnwindSafe(next_event(
        &mut receiver,
        &mut workers,
        tokio::time::Instant::now(),
        "injected expired stage",
        0,
        &progress,
    ))
    .catch_unwind()
    .await;
    let failure = result.expect_err("stage expiry must remain a failure");
    assert!(
        failure
            .downcast_ref::<String>()
            .unwrap()
            .contains("stage deadline"),
        "failure must identify the expired stage"
    );
    assert!(workers.is_empty(), "expired stages must reap their workers");
}

async fn scenario(
    nodes: usize,
    universes: usize,
    slots: u32,
    stress: bool,
    churn: bool,
    progress: Arc<Progress>,
) {
    progress.report("start");
    eprintln!(
        "SCALE_GEOMETRY {}",
        json!({"nodes":nodes,"universes":universes,
        "slots":slots,"stress":stress,"runtime_workers":if stress {16} else {4},
        "limits":std::fs::read_to_string("/proc/self/limits").ok(),
        "ephemeral_port_range":std::fs::read_to_string("/proc/sys/net/ipv4/ip_local_port_range").ok()})
    );
    let start = Instant::now();
    let baseline = sample();
    let mut ca = Authority::generate(unix_now(), 86400, 60).unwrap();
    ca.last_issued_expiry = unix_now() + 7200;
    let cp = Identity {
        kind: IdentityKind::ControlPlane,
        universe: String::new(),
        node: String::new(),
        pod_uid: "control".into(),
        boot_id: "d".repeat(64),
        pod_name: "control".into(),
        container_id: String::new(),
    };
    let (cert, key) = leaf(&ca, &cp, nodes);
    let bundle = crate::security::TrustBundle {
        version: 1,
        generation: 1,
        active: ca.digest.clone(),
        certificates: ca.certificate.clone(),
    };
    let server = Arc::new(
        TlsSnapshot::new(
            &bundle.json(),
            &X509::from_der(&cert).unwrap().to_pem().unwrap(),
            &PKey::private_key_from_pkcs8(&key)
                .unwrap()
                .private_key_to_pem_pkcs8()
                .unwrap(),
        )
        .unwrap(),
    );
    let mut roots = rustls::RootCertStore::empty();
    roots
        .add(
            X509::from_pem(ca.certificate.as_bytes())
                .unwrap()
                .to_der()
                .unwrap()
                .into(),
        )
        .unwrap();
    let roots = Arc::new(roots);
    let image = StateImage {
        metadata: serde_json::to_vec(&json!({
            "version": 1, "namespace": "system", "fence": "scale", "generation": 1,
            "active": ca.digest, "phase": "stable", "authorities": [ca],
            "rotation_nonce": "", "published_at": null, "overlap_delay": 60,
            "retirement_skew": 60
        }))
        .unwrap(),
    };
    let ca_state = Arc::new(CaState::from_image(&image).unwrap());
    let before_leaves = ca_state.to_image().unwrap();
    let mut clients = Vec::with_capacity(nodes);
    let mut generations: Vec<_> = (0..universes)
        .map(|i| Generation::empty(format!("scale-{i}")))
        .collect();
    for i in 0..nodes {
        let universe = i % universes;
        let node = identity("node", &format!("node-uid-{i}"));
        let boot = identity("boot", &i.to_string());
        let pod = format!("pod-uid-{i}");
        let id = Identity {
            kind: IdentityKind::Node,
            universe: identity("universe", &generations[universe].universe),
            node: node.clone(),
            pod_uid: pod.clone(),
            boot_id: boot.clone(),
            pod_name: format!("pod-{i}"),
            container_id: String::new(),
        };
        let (cert, key) = leaf(&ca, &id, i);
        let mut config = rustls::ClientConfig::builder_with_provider(Arc::new(
            rustls::crypto::ring::default_provider(),
        ))
        .with_protocol_versions(&[&rustls::version::TLS13])
        .unwrap()
        .with_root_certificates(roots.clone())
        .with_client_auth_cert(
            vec![cert.into()],
            rustls::pki_types::PrivateKeyDer::Pkcs8(key.into()),
        )
        .unwrap();
        config.resumption = rustls::client::Resumption::disabled();
        clients.push((node.clone(), boot, Arc::new(config)));
        generations[universe].nodes.insert(
            format!("node-{i:05}"),
            Member {
                id: node,
                ip: Some(
                    format!("10.{}.{}.{}", i / 65536, i / 256 % 256, i % 256)
                        .parse()
                        .unwrap(),
                ),
                fabric: "rack-a".into(),
                pod_uid: pod,
                pod_namespace: "system".into(),
                pod_name: format!("pod-{i}"),
            },
        );
        progress.credentials.store(i + 1, Ordering::Relaxed);
    }
    let after_leaves = ca_state.to_image().unwrap();
    assert!(
        after_leaves.metadata == before_leaves.metadata,
        "leaf count must not grow CA state"
    );
    assert!(after_leaves.metadata.len() <= 16384);
    assert!(after_leaves.metadata.len() <= 16 * 1024);
    assert_eq!(
        ca_state.expiry_watermarks().collect::<Vec<_>>(),
        vec![(ca.digest.as_str(), ca.last_issued_expiry)]
    );
    drop(image);
    let subscriptions = Subscriptions::new(
        Arc::new(move || {
            Some(SecurityContext {
                fence: "scale".into(),
                state: ca_state.clone(),
            })
        }),
        64 * 1024 * 1024,
        if stress {
            Duration::from_secs(28)
        } else {
            Duration::from_millis(500)
        },
    );
    subscriptions.set_fence(Some("scale".into()));
    *progress.subscriptions.lock().unwrap() = Some(subscriptions.clone());
    progress.enter("initial topology install");
    let mut publications: Vec<Publication<Generation>> =
        (0..universes).map(|_| Publication::default()).collect();
    for (universe, generation) in generations.iter_mut().enumerate() {
        let names: BTreeMap<_, _> = generation
            .nodes
            .iter()
            .map(|(name, member)| (member.id.clone(), name.clone()))
            .collect();
        let owners = place_in_universe(
            slots,
            &generation.universe,
            &names.keys().cloned().collect::<Vec<_>>(),
        )
        .unwrap()
        .into_iter()
        .map(|id| names[&id].clone())
        .collect();
        generation.volumes.push(Volume {
            id: "cache-uid".into(),
            name: "cache-a".into(),
            resource_generation: 1,
            cache_socket: "/dev/racer/cache-a/cache".into(),
            origin_socket: "/dev/racer/cache-a/origin".into(),
            slots,
            cache_generation: 1,
            routing_algorithm: crate::model::ROUTING_ALGORITHM,
            max_candidate_attempts: 3,
            owners,
        });
        let published = publications[universe]
            .publish(generation.clone(), revisions(universe).0)
            .unwrap();
        subscriptions.install(published).unwrap();
    }
    let setup = json!({"wall_seconds":start.elapsed().as_secs_f64(),"resources":sample()});
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let router = subscriptions.router();
    let accepts = Arc::new(Semaphore::new(128));
    let accept_task = tokio::spawn(async move {
        // Owning the connection tasks ensures aborting the listener also aborts
        // its children, including TLS handshakes queued behind admission.
        let mut servers = tokio::task::JoinSet::new();
        loop {
            let accepted = tokio::select! {
                result = servers.join_next(), if !servers.is_empty() => {
                    if let Some(Err(error)) = result {
                        eprintln!("SCALE_SERVER task failed: {error}");
                    }
                    continue;
                },
                accepted = listener.accept() => accepted,
            };
            let (raw, _) = accepted.unwrap();
            raw.set_nodelay(true).unwrap();
            let server = server.clone();
            let router = router.clone();
            let permits = accepts.clone();
            servers.spawn(async move {
                let permit = permits.acquire_owned().await.unwrap();
                let (tls, peer) = match server.accept(raw, true).await {
                    Ok(accepted) => accepted,
                    Err(error) => {
                        eprintln!("SCALE_SERVER TLS accept failed: {error}");
                        return;
                    }
                };
                drop(permit);
                let _ = hyper_util::server::conn::auto::Builder::new(
                    hyper_util::rt::TokioExecutor::new(),
                )
                .serve_connection(
                    hyper_util::rt::TokioIo::new(tls),
                    hyper_util::service::TowerToHyperService::new(
                        router.layer(Extension(peer.unwrap())),
                    ),
                )
                .await;
            });
        }
    });
    struct AbortOnDrop(tokio::task::JoinHandle<()>);
    impl Drop for AbortOnDrop {
        fn drop(&mut self) {
            self.0.abort();
        }
    }
    let _accept_task = AbortOnDrop(accept_task);
    // Change a real member endpoint while recipients are queued for their first
    // full snapshot. Keep publishing until every recipient makes progress, not
    // merely until a fixed burst ends and lets a starved queue recover.
    let (stop_churn, mut churn_stopped) = watch::channel(false);
    let churn_publications = Arc::new(AtomicUsize::new(0));
    let churn_task = if churn {
        let subscriptions = subscriptions.clone();
        let mut generation = generations[0].clone();
        let publications = churn_publications.clone();
        Some(AbortOnDrop(tokio::spawn(async move {
            loop {
                tokio::select! {
                    _ = churn_stopped.changed() => break,
                    _ = tokio::time::sleep(Duration::from_millis(100)) => {}
                }
                generation.revision =
                    revisions(0).0 + publications.load(Ordering::Relaxed) as u64 + 1;
                let member = generation.nodes.values_mut().next().unwrap();
                member.ip = Some(if generation.revision.is_multiple_of(2) {
                    "10.250.0.1".parse().unwrap()
                } else {
                    "10.250.0.2".parse().unwrap()
                });
                let state = subscriptions.clone();
                let next = Arc::new(generation.clone());
                tokio::task::spawn_blocking(move || state.install(next))
                    .await
                    .unwrap()
                    .unwrap();
                publications.fetch_add(1, Ordering::Relaxed);
            }
        })))
    } else {
        None
    };
    let (events, mut receiver) = mpsc::channel(nodes);
    let (finish, finished) = watch::channel(false);
    let connections = Arc::new(Semaphore::new(128));
    let mut workers = tokio::task::JoinSet::new();
    progress.enter("initial fanout");
    let phase_start = Instant::now();
    let cpu_start = sample();
    for (index, (node, boot, config)) in clients.into_iter().enumerate() {
        let events = events.clone();
        let mut finished = finished.clone();
        let connections = connections.clone();
        let progress = ClientProgress {
            fleet: progress.clone(),
            index,
        };
        let (initial_revision, successor_revision) = revisions(index % universes);
        workers.spawn(async move {
            let work = async {
                let permit = progress
                    .bounded(10, 1, connections.acquire())
                    .await
                    .unwrap();
                let mut stream = connect(address, config.clone(), &progress).await;
                drop(permit);
                send(&mut stream, &boot, "", &progress).await;
                let (mut cursor, revision, bytes, latency) =
                    receive(&mut stream, &node, &progress).await.unwrap();
                if churn {
                    assert!((initial_revision..successor_revision).contains(&revision));
                } else {
                    assert_eq!(revision, initial_revision);
                }
                if !progress.event(&events, (0, bytes, latency)).await {
                    return;
                }
                progress.fleet.phases[index].store(1, Ordering::Relaxed);
                loop {
                    send(&mut stream, &boot, &cursor, &progress).await;
                    if let Some((next, revision, bytes, latency)) =
                        receive(&mut stream, &node, &progress).await
                    {
                        if churn && revision < successor_revision {
                            assert!(revision >= initial_revision);
                            cursor = next;
                            continue;
                        }
                        assert_eq!(revision, successor_revision);
                        cursor = next;
                        if !progress.event(&events, (1, bytes, latency)).await {
                            return;
                        }
                        break;
                    }
                }
                drop(stream);
                progress.fleet.phases[index].store(2, Ordering::Relaxed);
                let permit = progress
                    .bounded(10, 9, connections.acquire())
                    .await
                    .unwrap();
                let mut stream = connect(address, config, &progress).await;
                drop(permit);
                send(&mut stream, &boot, &cursor, &progress).await;
                if !progress.event(&events, (2, 0, 0.)).await {
                    return;
                }
                loop {
                    assert!(receive(&mut stream, &node, &progress).await.is_none());
                    send(&mut stream, &boot, &cursor, &progress).await;
                }
            };
            // Sender drop cancels every phase, not just the final unchanged poll.
            tokio::select! {
                biased;
                _ = finished.changed() => {},
                _ = work => {},
            }
            progress.enter(10);
        });
    }
    drop(events);
    let latencies = |mut values: Vec<f64>| {
        values.sort_by(f64::total_cmp);
        json!({"p50_seconds":values[values.len()/2],"p99_seconds":values[values.len()*99/100],"max_seconds":values[values.len()-1],"over_35_seconds":values.iter().filter(|v| **v>35.).count()})
    };
    let mut initial_bytes = 0usize;
    let mut initial_latency = Vec::new();
    let stage_deadline =
        tokio::time::Instant::now() + Duration::from_secs(if stress { 60 } else { 4 });
    for completed in 0..nodes {
        let (phase, bytes, latency) = next_event(
            &mut receiver,
            &mut workers,
            stage_deadline,
            "initial fanout",
            completed,
            &progress,
        )
        .await;
        assert_eq!(phase, 0);
        initial_bytes += bytes;
        initial_latency.push(latency);
    }
    let initial = json!({"wall_seconds":phase_start.elapsed().as_secs_f64(),"cpu_start":cpu_start,"resources":sample(),"bytes":initial_bytes,"qps":nodes as f64/phase_start.elapsed().as_secs_f64(),"first_byte":latencies(initial_latency)});
    if let Some(mut task) = churn_task {
        stop_churn.send(true).unwrap();
        bounded(3, "stop churn publisher", async { (&mut task.0).await })
            .await
            .unwrap();
        eprintln!(
            "CHURN_RESULT publications={} initial={initial}",
            churn_publications.load(Ordering::Relaxed)
        );
        assert!(churn_publications.load(Ordering::Relaxed) >= 2);
    }
    progress.enter("initial held requests");
    await_waiters(&subscriptions, nodes, 3, "initial held requests").await;
    let held_start = sample();
    tokio::time::sleep(Duration::from_millis(if stress { 1000 } else { 100 })).await;
    // Desired state is invisible until the runtime installs a publication after
    // reserving its revision and verifying authority.
    assert!(receiver.try_recv().is_err());
    let held = json!({"before":held_start,"after":sample(),"waiters":subscriptions.waiter_count(),"cache":subscriptions.cache_usage()});
    let fanout_start = Instant::now();
    let fanout_cpu = sample();
    progress.enter("publish/reconnect");
    for (universe, generation) in generations.iter_mut().enumerate() {
        generation.volumes[0].cache_generation = 2;
        let published = publications[universe]
            .publish(generation.clone(), revisions(universe).1)
            .unwrap();
        subscriptions.install(published).unwrap();
    }
    let mut fanout_bytes = 0usize;
    let mut delivered = 0;
    let mut reconnected = 0;
    let mut fanout_latency = Vec::new();
    let stage_deadline =
        tokio::time::Instant::now() + Duration::from_secs(if stress { 60 } else { 4 });
    while delivered < nodes || reconnected < nodes {
        let (phase, bytes, latency) = next_event(
            &mut receiver,
            &mut workers,
            stage_deadline,
            "publish/reconnect",
            delivered + reconnected,
            &progress,
        )
        .await;
        match phase {
            1 => {
                delivered += 1;
                fanout_bytes += bytes;
                fanout_latency.push(latency)
            }
            2 => reconnected += 1,
            _ => panic!("unexpected phase"),
        };
    }
    let fanout = json!({"wall_seconds":fanout_start.elapsed().as_secs_f64(),"cpu_start":fanout_cpu,"resources":sample(),"bytes":fanout_bytes,"qps":nodes as f64/fanout_start.elapsed().as_secs_f64(),"first_byte_including_held_time":latencies(fanout_latency)});
    progress.enter("reconnected held requests");
    await_waiters(&subscriptions, nodes, 3, "reconnected held requests").await;
    assert!(subscriptions.cache_usage().0 <= 64 * 1024 * 1024);
    assert_eq!(subscriptions.cache_usage().1, nodes);
    let reconnect = sample();
    tokio::time::sleep(Duration::from_millis(if stress { 1000 } else { 100 })).await;
    progress.enter("teardown");
    finish.send(true).unwrap();
    bounded(2, "client teardown", async {
        while let Some(result) = workers.join_next().await {
            result.unwrap();
        }
    })
    .await;
    await_waiters(&subscriptions, 0, 2, "server teardown").await;
    progress.enter("complete");
    println!(
        "SCALE_RESULT {}",
        json!({"nodes":nodes,"universes":universes,"geometry_slots":slots,"transport":"real TLS1.3 HTTP1 loopback","runtime_workers":if stress {16} else {4},"baseline":baseline,"setup":setup,"initial":initial,"held":held,"fanout_and_reconnect":fanout,"reconnect_held":reconnect,"final":sample(),"cache":subscriptions.cache_usage()})
    );
}
