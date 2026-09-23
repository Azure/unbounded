// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Opt-in measured TLS fanout against the production Router. The enrollment
//! fixture restores validated durable shards; it does not benchmark issuance.
//!
//! Earlier completed release runs (before the short-deadline hardening):
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

use super::*;
use crate::{
    model::{Member, SLOT_COUNT, Volume},
    security::{Authority, CaState, Identity, IdentityKind, StateImage, TlsSnapshot},
    topology::place,
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
use tokio::{
    io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader},
    net::{TcpListener, TcpStream},
    sync::mpsc,
};

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
    let _watchdog = Watchdog::start(if stress { 150 } else { 20 });
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(if stress { 16 } else { 4 })
        .enable_all()
        .build()
        .unwrap();
    let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        runtime.block_on(bounded(
            if stress { 140 } else { 12 },
            "whole scenario",
            scenario(nodes, universes, slots, stress),
        ))
    }));
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
) -> (u32, usize, f64) {
    tokio::select! {
        event = receiver.recv() => event.unwrap_or_else(|| panic!("{stage}: event channel closed after {completed}")),
        result = workers.join_next() => panic!("{stage}: worker ended before stage completion ({completed}): {result:?}"),
        _ = tokio::time::sleep_until(deadline) => panic!("{stage}: stage deadline after {completed} completions"),
    }
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
    cert.set_not_after(&Asn1Time::from_unix(unix_now() + 3600).unwrap())
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
    cert.append_extension(
        ExtendedKeyUsage::new()
            .server_auth()
            .client_auth()
            .build()
            .unwrap(),
    )
    .unwrap();
    let mut san = SubjectAlternativeName::new();
    san.uri(&identity.uri().unwrap());
    if identity.kind == IdentityKind::ControlPlane {
        san.dns("racer-controlplane.system.svc");
    }
    cert.append_extension(
        san.build(&cert.x509v3_context(Some(&parent), None))
            .unwrap(),
    )
    .unwrap();
    cert.sign(&signer, MessageDigest::sha256()).unwrap();
    (
        cert.build().to_der().unwrap(),
        key.private_key_to_pkcs8().unwrap(),
    )
}

fn sample() -> Value {
    let status = std::fs::read_to_string("/proc/self/status").unwrap();
    let kb = |key: &str| {
        status
            .lines()
            .find_map(|line| line.strip_prefix(key))
            .and_then(|v| v.split_whitespace().next())
            .and_then(|s| s.parse::<u64>().ok())
            .unwrap_or(0)
    };
    let stat = std::fs::read_to_string("/proc/self/stat").unwrap();
    let fields: Vec<_> = stat
        .rsplit_once(')')
        .unwrap()
        .1
        .split_whitespace()
        .collect();
    // Linux USER_HZ is 100 on the supported x86_64 Linux benchmark host.
    let cpu =
        (fields[11].parse::<u64>().unwrap() + fields[12].parse::<u64>().unwrap()) as f64 / 100.;
    json!({"cpu_seconds":cpu,"rss_kib":kb("VmRSS:"),"peak_rss_kib":kb("VmHWM:")})
}

type TlsClient = BufReader<tokio_rustls::client::TlsStream<TcpStream>>;
async fn connect(address: std::net::SocketAddr, config: Arc<rustls::ClientConfig>) -> TlsClient {
    let raw = bounded(3, "TCP connect", TcpStream::connect(address))
        .await
        .unwrap();
    raw.set_nodelay(true).unwrap();
    BufReader::new(
        bounded(
            3,
            "TLS handshake",
            tokio_rustls::TlsConnector::from(config).connect(
                rustls::pki_types::ServerName::try_from("racer-controlplane.system.svc").unwrap(),
                raw,
            ),
        )
        .await
        .unwrap(),
    )
}
async fn send(stream: &mut TlsClient, boot: &str, cursor: &str) {
    bounded(3,"request write",stream.write_all(format!("GET /v4/config HTTP/1.1\r\nHost: racer-controlplane.system.svc\r\nContent-Length: 0\r\nX-Racer-Boot: {boot}\r\nX-Racer-Profile: 1\r\nX-Racer-Cursor: {cursor}\r\nX-Racer-Applied-Revision: 0\r\nX-Racer-Local-State: failed\r\n\r\n").as_bytes())).await.unwrap();
}
async fn receive(stream: &mut TlsClient, node: &str) -> Option<(String, u64, usize, f64)> {
    let started = Instant::now();
    let mut line = String::new();
    assert!(
        bounded(35, "response first byte", stream.read_line(&mut line))
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
            bounded(2, "response header", stream.read_line(&mut line))
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
    bounded(5, "response body", stream.read_exact(&mut body))
        .await
        .unwrap();
    let desired = proto::DesiredState::decode(body.as_slice()).unwrap();
    assert_eq!(hex::encode(&desired.node), node);
    let proto::configuration::Contents::Snapshot(snapshot) =
        desired.configuration.unwrap().contents.unwrap();
    assert_eq!(
        Sha256::digest(snapshot.encode_to_vec()).as_slice(),
        desired.snapshot_digest
    );
    Some((desired.cursor, desired.revision, body.len(), first_byte))
}

/// Run separately for each geometry for meaningful process peak-RSS readings:
/// RACER_SCALE_NODES=10000 RACER_SCALE_UNIVERSES=1 cargo test --release --lib
/// distinct_tls_fanout -- --ignored --nocapture
#[test]
#[ignore = "opt-in distinct-node TLS load measurement"]
fn distinct_tls_fanout() {
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

async fn scenario(nodes: usize, universes: usize, slots: u32, stress: bool) {
    assert!(nodes >= universes && nodes <= 16000 && nodes.is_multiple_of(universes));
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
    let mut buckets: BTreeMap<String, BTreeMap<String, Value>> = BTreeMap::new();
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
        let hash = Sha256::digest(id.key().as_bytes());
        let bucket = format!("{:03x}", u16::from_be_bytes([hash[0], hash[1]]) & 1023);
        buckets.entry(bucket).or_default().insert(id.key(), json!({"identity":id,"leaves":{hex::encode(Sha256::digest(&cert)):{"root":ca.digest,"expiry":unix_now()+3600}}}));
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
    }
    let mut shards = BTreeMap::new();
    let mut references = BTreeMap::new();
    for (bucket, members) in buckets {
        let bytes = serde_json::to_vec(&json!({"members":members,"retired":[]})).unwrap();
        let digest = hex::encode(Sha256::digest(&bytes));
        references.insert(bucket, digest.clone());
        shards.insert(digest, bytes);
    }
    let image = StateImage { metadata: serde_json::to_vec(&json!({"version":4,"fence":"scale","fence_at":unix_now(),"generation":1,"active":ca.digest,"phase":"stable","authorities":[ca],"shards":references})).unwrap(), shards };
    let ca_state = Arc::new(CaState::from_image(&image).unwrap());
    assert_eq!(ca_state.members().count(), nodes);
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
    for generation in &mut generations {
        generation.revision = 1;
        let owners = place(
            slots,
            &generation.nodes.keys().cloned().collect::<Vec<_>>(),
            &[],
        )
        .unwrap();
        generation.volumes.push(Volume {
            id: "cache-uid".into(),
            name: "cache-a".into(),
            resource_generation: 1,
            cache_socket: "/dev/racer/cache-a/cache".into(),
            origin_socket: "/dev/racer/cache-a/origin".into(),
            slots,
            cache_generation: 1,
            routing_algorithm: 2,
            max_candidate_attempts: 3,
            owners,
        });
        subscriptions.install(Arc::new(generation.clone())).unwrap();
    }
    let setup = json!({"wall_seconds":start.elapsed().as_secs_f64(),"resources":sample()});
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let router = subscriptions.router();
    let accepts = Arc::new(Semaphore::new(128));
    let accept_task = tokio::spawn(async move {
        loop {
            let (raw, _) = listener.accept().await.unwrap();
            raw.set_nodelay(true).unwrap();
            let server = server.clone();
            let router = router.clone();
            let permits = accepts.clone();
            tokio::spawn(async move {
                let permit = permits.acquire_owned().await.unwrap();
                let (tls, peer) = server.accept(raw, true).await.unwrap();
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
    let (events, mut receiver) = mpsc::channel(nodes);
    let (finish, finished) = watch::channel(false);
    let connections = Arc::new(Semaphore::new(128));
    let mut workers = tokio::task::JoinSet::new();
    let phase_start = Instant::now();
    let cpu_start = sample();
    for (node, boot, config) in clients {
        let events = events.clone();
        let mut finished = finished.clone();
        let connections = connections.clone();
        workers.spawn(async move {
            let permit=bounded(10,"connection admission",connections.acquire()).await.unwrap(); let mut stream=connect(address,config.clone()).await; drop(permit);
            send(&mut stream,&boot,"").await;
            let (mut cursor,revision,bytes,latency)=receive(&mut stream,&node).await.unwrap(); assert_eq!(revision,1);
            events.send((0,bytes,latency)).await.unwrap();
            loop { send(&mut stream,&boot,&cursor).await; if let Some((next,revision,bytes,latency))=receive(&mut stream,&node).await { assert_eq!(revision,2);cursor=next;events.send((1,bytes,latency)).await.unwrap();break; } }
            drop(stream);
            let permit=bounded(10,"reconnect admission",connections.acquire()).await.unwrap(); let mut stream=connect(address,config).await; drop(permit);
            send(&mut stream,&boot,&cursor).await;
            events.send((2,0,0.)).await.unwrap();
            loop {
                tokio::select! {
                    _ = finished.changed() => break,
                    response = receive(&mut stream,&node) => { assert!(response.is_none()); send(&mut stream,&boot,&cursor).await; }
                }
            }
            // Cancellation closes the TLS stream with the unchanged poll pending.
            drop(stream);
        });
    }
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
        )
        .await;
        assert_eq!(phase, 0);
        initial_bytes += bytes;
        initial_latency.push(latency);
    }
    let initial = json!({"wall_seconds":phase_start.elapsed().as_secs_f64(),"cpu_start":cpu_start,"resources":sample(),"bytes":initial_bytes,"qps":nodes as f64/phase_start.elapsed().as_secs_f64(),"first_byte":latencies(initial_latency)});
    await_waiters(&subscriptions, nodes, 3, "initial held requests").await;
    let held_start = sample();
    tokio::time::sleep(Duration::from_millis(if stress { 1000 } else { 100 })).await;
    // A delayed persistence completion must not publish intent. This delay models
    // the runtime's commit seam; actual slow RecordStore behavior has its own test.
    assert!(receiver.try_recv().is_err());
    let held = json!({"before":held_start,"after":sample(),"waiters":subscriptions.waiter_count(),"cache":subscriptions.cache_usage()});
    let fanout_start = Instant::now();
    let fanout_cpu = sample();
    for generation in &mut generations {
        generation.revision = 2;
        generation.volumes[0].cache_generation = 2;
        subscriptions.install(Arc::new(generation.clone())).unwrap();
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
    await_waiters(&subscriptions, nodes, 3, "reconnected held requests").await;
    assert!(subscriptions.cache_usage().0 <= 64 * 1024 * 1024);
    assert_eq!(subscriptions.cache_usage().1, nodes);
    let reconnect = sample();
    tokio::time::sleep(Duration::from_millis(if stress { 1000 } else { 100 })).await;
    finish.send(true).unwrap();
    bounded(2, "client teardown", async {
        while let Some(result) = workers.join_next().await {
            result.unwrap();
        }
    })
    .await;
    await_waiters(&subscriptions, 0, 2, "server teardown").await;
    println!(
        "SCALE_RESULT {}",
        json!({"nodes":nodes,"universes":universes,"geometry_slots":slots,"transport":"real TLS1.3 HTTP1 loopback","runtime_workers":if stress {16} else {4},"baseline":baseline,"setup":setup,"initial":initial,"held":held,"fanout_and_reconnect":fanout,"reconnect_held":reconnect,"final":sample(),"cache":subscriptions.cache_usage()})
    );
}
