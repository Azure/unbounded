//! End-to-end tests of the runtime against the fake Kubernetes API.

use crate::{
    Config, SeatPolicy, annotations,
    config::Rotation,
    exec::{self, Executor, Tcp},
    http::{self, Conn, TlsStream},
    kube::{Kube, KubeConfig, fake, str_at},
};
use base64::{
    Engine,
    engine::general_purpose::{STANDARD, URL_SAFE_NO_PAD},
};
use racer_control_wire as wire;
use serde_json::{Value, json};
use std::{
    path::PathBuf,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    time::{Duration, Instant},
};

const NS: &str = "racer-system";
const SECRET_UID: &str = "c1c1c1c1-0000-4000-8000-000000000001";
const DS_UID: &str = "d5d5d5d5-0000-4000-8000-000000000001";
const SA_UID: &str = "5a5a5a5a-0000-4000-8000-000000000001";

fn node_uid(i: usize) -> String {
    format!("0000000{i}-0000-4000-8000-0000000000aa")
}

fn pod_uid(i: usize) -> String {
    format!("0000000{i}-0000-4000-8000-0000000000bb")
}

/// Unsigned test JWT naming a Pod.
fn token(pod: usize) -> String {
    let exp = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs()
        + 3600;
    format!(
        "e30.{}.sig",
        URL_SAFE_NO_PAD.encode(json!({"exp": exp, "pod": pod}).to_string())
    )
}

fn seed(api: &fake::FakeApi, pods: usize) {
    let mut secret = json!({"apiVersion":"v1","kind":"Secret","type":"Opaque","metadata":{"name":"racer-state","namespace":NS,"uid":SECRET_UID}});
    secret["data"] = json!({});
    api.put_object(
        &format!("/api/v1/namespaces/{NS}/secrets/racer-state"),
        secret,
    );
    for name in (0..16)
        .map(annotations::seat_lease)
        .chain([annotations::PRIMARY_LEASE.to_owned()])
    {
        api.put_object(
            &format!("/apis/coordination.k8s.io/v1/namespaces/{NS}/leases/{name}"),
            json!({"metadata":{"name":name,"namespace":NS},"spec":{}}),
        );
    }
    api.put_object(
        &format!("/apis/apps/v1/namespaces/{NS}/daemonsets/racer-dataplane"),
        json!({"metadata":{"name":"racer-dataplane","uid":DS_UID}}),
    );
    api.put_object(
        &format!("/api/v1/namespaces/{NS}/serviceaccounts/racer-dataplane"),
        json!({"metadata":{"name":"racer-dataplane","uid":SA_UID}}),
    );
    for i in 1..=pods {
        api.put_object(
            &format!("/api/v1/nodes/n{i}"),
            json!({"metadata":{"name":format!("n{i}"),"uid":node_uid(i),"annotations":{annotations::BLOCK_DEVICES:"nvme-.*"}}}),
        );
        api.put_object(
            &format!("/api/v1/namespaces/{NS}/pods/p{i}"),
            json!({
                "metadata":{"name":format!("p{i}"),"namespace":NS,"uid":pod_uid(i),"creationTimestamp":"2026-01-01T00:00:00Z",
                    "ownerReferences":[{"apiVersion":"apps/v1","kind":"DaemonSet","name":"racer-dataplane","uid":DS_UID,"controller":true}]},
                "spec":{"nodeName":format!("n{i}"),"serviceAccountName":"racer-dataplane"},
                "status":{"podIP":format!("127.0.0.{}", i + 1),"phase":"Running"}
            }),
        );
    }
    api.on_post(|path, body| {
        if path != crate::kube::paths::TOKEN_REVIEWS {
            return None;
        }
        let token = body["spec"]["token"].as_str().unwrap_or_default();
        let payload = token.split('.').nth(1).and_then(|p| URL_SAFE_NO_PAD.decode(p).ok());
        let Some(claims) = payload.and_then(|p| serde_json::from_slice::<Value>(&p).ok()) else {
            return Some((201, json!({"status":{"authenticated":false}})));
        };
        let i = claims["pod"].as_u64().unwrap_or(0) as usize;
        Some((201, json!({"status":{
            "authenticated": true,
            "audiences": ["racer-control"],
            "user": {"username": format!("system:serviceaccount:{NS}:racer-dataplane"), "uid": SA_UID, "extra": {
                "authentication.kubernetes.io/pod-name": [format!("p{i}")],
                "authentication.kubernetes.io/pod-uid": [pod_uid(i)],
                "authentication.kubernetes.io/node-name": [format!("n{i}")],
                "authentication.kubernetes.io/node-uid": [node_uid(i)],
            }}
        }})))
    });
}

/// Run the fake API on its own thread.
struct FakeCluster {
    kube: KubeConfig,
    requests: Arc<std::sync::atomic::AtomicUsize>,
    stop: Arc<AtomicBool>,
    thread: Option<std::thread::JoinHandle<()>>,
    dir: PathBuf,
}

impl FakeCluster {
    fn start(pods: usize) -> Self {
        let dir = fake::scratch();
        let stop = Arc::new(AtomicBool::new(false));
        let (tx, rx) = std::sync::mpsc::channel();
        let requests = Arc::new(std::sync::atomic::AtomicUsize::new(0));
        let (d, s, r) = (dir.clone(), stop.clone(), requests.clone());
        let thread = std::thread::spawn(move || {
            let mut ex = Executor::new(4096).unwrap();
            ex.block_on(async move {
                let api = fake::FakeApi::new();
                seed(&api, pods);
                api.count_into(r);
                tx.send(fake::serve(api, &d.join("api"))).unwrap();
                while !s.load(Ordering::SeqCst) {
                    exec::sleep(Duration::from_millis(50)).await;
                }
            });
        });
        let kube = rx.recv().unwrap();
        Self {
            kube,
            requests,
            stop,
            thread: Some(thread),
            dir,
        }
    }

    fn config(&self, i: usize) -> Config {
        let token = self.dir.join(format!("control-token-{i}"));
        std::fs::write(&token, token_of(i)).unwrap();
        Config {
            namespace: NS.into(),
            pod_name: format!("p{i}"),
            pod_uid: pod_uid(i),
            node_name: format!("n{i}"),
            pod_ip: format!("127.0.0.{}", i + 1).parse().unwrap(),
            seats: SeatPolicy::Auto,
            control_listen: format!("127.0.0.{}:0", i + 1).parse().unwrap(),
            webhook_listen: format!("127.0.0.{}:0", i + 1).parse().unwrap(),
            object_image: Some("example.com/racer-object:1".into()),
            api_server: self.kube.server.clone(),
            api_ca: self.kube.ca.clone(),
            api_token: self.kube.token.clone(),
            control_token: token,
            daemonset: "racer-dataplane".into(),
            service_account: "racer-dataplane".into(),
            peer_port: 8082,
            rotation: Rotation::default(),
        }
    }

    /// Run `f` with a fresh client to the fake API.
    fn inspect<T>(&self, f: impl AsyncFnOnce(&Kube) -> T) -> T {
        let mut ex = Executor::new(64).unwrap();
        let kube = Kube::new(self.kube.clone()).unwrap();
        ex.block_on(f(&kube))
    }
}

fn token_of(i: usize) -> String {
    token(i)
}

impl Drop for FakeCluster {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        if let Some(t) = self.thread.take() {
            let _ = t.join();
        }
        let _ = std::fs::remove_dir_all(&self.dir);
    }
}

fn wait_for(limit: Duration, mut f: impl FnMut() -> bool) -> bool {
    let end = Instant::now() + limit;
    while Instant::now() < end {
        if f() {
            return true;
        }
        std::thread::sleep(Duration::from_millis(100));
    }
    false
}

/// One HTTPS request to a pinned endpoint.
async fn call(
    url: &str,
    pin: &[u8],
    client_cert: Option<(Vec<Vec<u8>>, Vec<u8>)>,
    method: &str,
    target: &str,
    headers: &[(&str, &str)],
    body: &[u8],
) -> (u16, Vec<u8>) {
    let address: std::net::SocketAddr = url.strip_prefix("https://").unwrap().parse().unwrap();
    let builder = rustls::ClientConfig::builder_with_provider(Arc::new(
        rustls::crypto::ring::default_provider(),
    ))
    .with_protocol_versions(&[&rustls::version::TLS13])
    .unwrap()
    .dangerous()
    .with_custom_certificate_verifier(pin_verifier(pin));
    let config = match client_cert {
        Some((chain, key)) => builder
            .with_client_auth_cert(
                chain
                    .into_iter()
                    .map(rustls::pki_types::CertificateDer::from)
                    .collect(),
                rustls::pki_types::PrivateKeyDer::Pkcs8(key.into()),
            )
            .unwrap(),
        None => builder.with_no_client_auth(),
    };
    let tcp = Tcp::connect(address).await.unwrap();
    let name = rustls::pki_types::ServerName::IpAddress(address.ip().into());
    let mut conn = Conn::new(
        TlsStream::client(tcp, Arc::new(config), name)
            .await
            .unwrap(),
    );
    let mut h = headers.to_vec();
    if !body.is_empty() {
        h.push(("Content-Type", "application/json"));
    }
    conn.tls
        .write_all(&http::request(method, "x", target, &h, body))
        .await
        .unwrap();
    let head = conn.read_head(false, 1 << 16).await.unwrap().unwrap();
    let body = conn.read_response_body(&head, 64 << 20).await.unwrap();
    (head.status, body)
}

fn pin_verifier(pin: &[u8]) -> Arc<dyn rustls::client::danger::ServerCertVerifier> {
    #[derive(Debug)]
    struct V(Vec<u8>, rustls::crypto::WebPkiSupportedAlgorithms);
    impl rustls::client::danger::ServerCertVerifier for V {
        fn verify_server_cert(
            &self,
            e: &rustls::pki_types::CertificateDer<'_>,
            _: &[rustls::pki_types::CertificateDer<'_>],
            _: &rustls::pki_types::ServerName<'_>,
            _: &[u8],
            _: rustls::pki_types::UnixTime,
        ) -> Result<rustls::client::danger::ServerCertVerified, rustls::Error> {
            if e.as_ref() == self.0.as_slice() {
                Ok(rustls::client::danger::ServerCertVerified::assertion())
            } else {
                Err(rustls::Error::General("pin".into()))
            }
        }
        fn verify_tls12_signature(
            &self,
            m: &[u8],
            c: &rustls::pki_types::CertificateDer<'_>,
            d: &rustls::DigitallySignedStruct,
        ) -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
            rustls::crypto::verify_tls12_signature(m, c, d, &self.1)
        }
        fn verify_tls13_signature(
            &self,
            m: &[u8],
            c: &rustls::pki_types::CertificateDer<'_>,
            d: &rustls::DigitallySignedStruct,
        ) -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
            rustls::crypto::verify_tls13_signature(m, c, d, &self.1)
        }
        fn supported_verify_schemes(&self) -> Vec<rustls::SignatureScheme> {
            self.1.supported_schemes()
        }
    }
    Arc::new(V(
        pin.to_vec(),
        rustls::crypto::ring::default_provider().signature_verification_algorithms,
    ))
}

#[test]
fn cold_start_elects_commits_serves_and_fails_closed() {
    let cluster = FakeCluster::start(2);
    let h1 = crate::start(cluster.config(1)).unwrap();
    let h2 = crate::start(cluster.config(2)).unwrap();
    let b = h1.bootstrap(Duration::from_secs(20)).unwrap();
    assert_eq!(b.cluster.0, SECRET_UID);
    assert_eq!(b.block_devices.as_deref(), Some("nvme-.*"));

    // Both Pods take seats on distinct Nodes; one becomes primary; both get
    // ready and are published as endpoints.
    assert!(
        wait_for(Duration::from_secs(60), || h1
            .endpoints()
            .read()
            .unwrap()
            .len()
            == 2
            && h2.endpoints().read().unwrap().len() == 2),
        "{:?} {:?}",
        h1.diagnostics(),
        h2.diagnostics()
    );
    let roles = [h1.diagnostics().role, h2.diagnostics().role];
    assert_eq!(
        roles
            .iter()
            .filter(|r| matches!(r, crate::Role::Primary(_)))
            .count(),
        1,
        "{roles:?}"
    );

    // Durable state is initialized and bound to the Secret UID.
    let secret = cluster.inspect(async |k| {
        k.get(&format!("/api/v1/namespaces/{NS}/secrets/racer-state"))
            .await
            .unwrap()
    });
    assert_eq!(
        str_at(
            &secret,
            &["metadata", "annotations", annotations::INITIALIZED]
        ),
        SECRET_UID
    );
    assert_eq!(
        str_at(
            &secret,
            &["metadata", "annotations", annotations::CLUSTER_ID]
        ),
        SECRET_UID
    );
    let record: Value = serde_json::from_slice(
        &STANDARD
            .decode(secret["data"]["state"].as_str().unwrap())
            .unwrap(),
    )
    .unwrap();
    assert_eq!(record["cluster"], SECRET_UID);
    assert_eq!(record["sequence"], 1);
    assert!(record["keyring"]["roots"].as_array().unwrap().len() >= 2);
    let primary = cluster.inspect(async |k| {
        k.get(&format!(
            "/apis/coordination.k8s.io/v1/namespaces/{NS}/leases/racer-primary"
        ))
        .await
        .unwrap()
    });
    assert!(matches!(
        str_at(&primary, &["spec", "holderIdentity"]),
        "p1" | "p2"
    ));
    assert!(primary["spec"]["leaseTransitions"].as_u64().unwrap() >= 1);

    // A node enrolls through any seat, then reads a snapshot and keyring.
    let endpoint = h1.endpoints().read().unwrap()[0].clone();
    let mut ex = Executor::new(64).unwrap();
    ex.block_on(async {
        let node_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
        let csr = rcgen::CertificateParams::new(Vec::<String>::new())
            .unwrap()
            .serialize_request(&node_key)
            .unwrap();
        let request = wire::EnrollmentRequest {
            shares: 4,
            rdma_nics: vec![],
            schema_version: wire::SCHEMA_VERSION,
            cluster: wire::ClusterId(SECRET_UID.into()),
            enrollment: wire::EnrollmentId("12345678-1234-4234-8234-123456789012".into()),
            csr_der: csr.der().to_vec(),
        };
        let body = wire::encode_enrollment_request(&request).unwrap();
        let bearer = format!("Bearer {}", token(1));
        let (status, out) = call(
            &endpoint.url,
            &endpoint.certificate,
            None,
            "POST",
            wire::BOOTSTRAP_PATH,
            &[("Authorization", &bearer)],
            &body,
        )
        .await;
        assert_eq!(status, 200, "{}", String::from_utf8_lossy(&out));
        let response = wire::decode_enrollment_response(&out).unwrap();
        assert_eq!(response.node.0, node_uid(1));
        assert_eq!(response.block_devices.as_deref(), Some("nvme-.*"));

        // Bad token: unauthenticated.
        let (status, _) = call(
            &endpoint.url,
            &endpoint.certificate,
            None,
            "POST",
            wire::BOOTSTRAP_PATH,
            &[("Authorization", "Bearer x")],
            &body,
        )
        .await;
        assert_eq!(status, 401);

        let cert = Some((response.certificate_chain.clone(), node_key.serialize_der()));
        let (status, out) = call(
            &endpoint.url,
            &endpoint.certificate,
            cert.clone(),
            "GET",
            wire::SNAPSHOT_PATH,
            &[],
            &[],
        )
        .await;
        assert_eq!(status, 200);
        let p = wire::decode_publication(&out).unwrap();
        assert_eq!(p.sequence.0, 1);
        assert_eq!(p.members.len(), 2);
        let (status, _) = call(
            &endpoint.url,
            &endpoint.certificate,
            None,
            "GET",
            wire::SNAPSHOT_PATH,
            &[],
            &[],
        )
        .await;
        assert_eq!(status, 401);
        let (status, out) = call(
            &endpoint.url,
            &endpoint.certificate,
            cert.clone(),
            "GET",
            wire::KEYRING_PATH,
            &[],
            &[],
        )
        .await;
        assert_eq!(status, 200);
        let bundle = wire::decode_bundle(&out).unwrap();
        assert!(bundle.peer_trust_roots.len() >= 2);
        let (status, _) = call(
            &endpoint.url,
            &endpoint.certificate,
            None,
            "GET",
            wire::KEYRING_PATH,
            &[("Authorization", &bearer)],
            &[],
        )
        .await;
        assert_eq!(status, 200);
        let (status, _) = call(
            &endpoint.url,
            &endpoint.certificate,
            cert.clone(),
            "GET",
            "/v1/snapshot?after=x",
            &[],
            &[],
        )
        .await;
        assert_eq!(status, 400);
        // A cursor from a seat that is ahead of this one: retry or fail
        // over, as the Go controller answered.
        for path in [wire::SNAPSHOT_PATH, wire::KEYRING_PATH] {
            let (status, out) = call(
                &endpoint.url,
                &endpoint.certificate,
                cert.clone(),
                "GET",
                &format!("{path}?after=999999"),
                &[],
                &[],
            )
            .await;
            assert_eq!(status, 503, "{path}");
            assert_eq!(
                wire::decode_error(&out).unwrap().code,
                wire::ProtocolFailure::Unavailable
            );
        }
        let (status, _) = call(
            &endpoint.url,
            &endpoint.certificate,
            cert,
            "GET",
            "/v1/snapshot?after=0",
            &[],
            &[],
        )
        .await;
        assert_eq!(status, 409);
    });

    // A report reaches this Pod's annotation and the next publication.
    h2.report(9, vec![]).unwrap();
    assert!(wait_for(Duration::from_secs(30), || {
        let pod = cluster.inspect(async |k| {
            k.get(&format!("/api/v1/namespaces/{NS}/pods/p2"))
                .await
                .unwrap()
        });
        str_at(&pod, &["metadata", "annotations", annotations::REPORT]).contains("\"shares\":9")
    }));
    assert!(wait_for(Duration::from_secs(60), || h1
        .diagnostics()
        .sequence
        >= 2
        && h2.diagnostics().sequence >= 2));

    // Steady state API load is small and bounded.
    let before = cluster.requests.load(Ordering::SeqCst);
    std::thread::sleep(Duration::from_secs(10));
    let steady = cluster.requests.load(Ordering::SeqCst) - before;
    assert!(steady < 150, "{steady} requests in 10s");

    // Graceful primary shutdown hands over to the other seat with a new term.
    let (primary, other) = if matches!(h1.diagnostics().role, crate::Role::Primary(_)) {
        (h1, h2)
    } else {
        (h2, h1)
    };
    let before = record_of(&cluster);
    primary.shutdown();
    assert!(
        wait_for(Duration::from_secs(60), || matches!(
            other.diagnostics().role,
            crate::Role::Primary(_)
        ) && other.diagnostics().ready),
        "{:?}",
        other.diagnostics()
    );
    assert!(wait_for(
        Duration::from_secs(30),
        || record_of(&cluster)["term"]["transitions"].as_u64()
            > before["term"]["transitions"].as_u64()
    ));
    let after = record_of(&cluster);
    assert_ne!(after["term"]["holder"], before["term"]["holder"]);
    assert!(after["sequence"].as_u64() >= before["sequence"].as_u64());
    assert_eq!(after["term"]["lease_uid"], before["term"]["lease_uid"]);
    let h1 = other;

    // Losing the Secret data fails closed: endpoints are withdrawn, nothing resets.
    cluster.inspect(async |k| {
        let path = format!("/api/v1/namespaces/{NS}/secrets/racer-state");
        let mut s = k.get(&path).await.unwrap();
        s["data"] = json!({});
        k.put(&path, &s).await.unwrap();
    });
    assert!(wait_for(Duration::from_secs(30), || !h1
        .diagnostics()
        .ready));
    let secret = cluster.inspect(async |k| {
        k.get(&format!("/api/v1/namespaces/{NS}/secrets/racer-state"))
            .await
            .unwrap()
    });
    assert!(
        secret["data"].get("state").is_none(),
        "runtime must never recreate state"
    );
    drop(h1);
}

fn record_of(cluster: &FakeCluster) -> Value {
    let secret = cluster.inspect(async |k| {
        k.get(&format!("/api/v1/namespaces/{NS}/secrets/racer-state"))
            .await
            .unwrap()
    });
    serde_json::from_slice(
        &STANDARD
            .decode(secret["data"]["state"].as_str().unwrap())
            .unwrap(),
    )
    .unwrap()
}

#[test]
fn start_and_shutdown_are_bounded_without_an_api() {
    let cluster = FakeCluster::start(1);
    let mut config = cluster.config(1);
    // Nothing listens here: every API call fails and retries with backoff.
    config.api_server = "127.0.0.1:1".into();
    let started = Instant::now();
    let h = crate::start(config.clone()).unwrap();
    assert!(started.elapsed() < Duration::from_secs(5));
    assert!(matches!(
        h.bootstrap(Duration::from_millis(300)),
        Err(crate::Error::Timeout)
    ));
    let stopping = Instant::now();
    h.shutdown();
    assert!(
        stopping.elapsed() < Duration::from_secs(10),
        "{:?}",
        stopping.elapsed()
    );

    // A busy listen address fails start promptly instead of hanging.
    let busy = std::net::TcpListener::bind("127.0.0.2:0").unwrap();
    config.control_listen = busy.local_addr().unwrap();
    let started = Instant::now();
    assert!(crate::start(config).is_err());
    assert!(started.elapsed() < Duration::from_secs(5));
}
