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
        atomic::{AtomicBool, AtomicU64, Ordering},
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
        json!({"metadata":{"name":"racer-dataplane","uid":DS_UID,"annotations":{crate::control::STATE_UID:SECRET_UID}}}),
    );
    api.put_object(
        &format!("/api/v1/namespaces/{NS}/serviceaccounts/racer-dataplane"),
        json!({"metadata":{"name":"racer-dataplane","uid":SA_UID,"annotations":{crate::control::STATE_UID:SECRET_UID}}}),
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
        Self::start_with(pods, |_| {})
    }

    /// Start with `setup` applied to the seeded API.
    fn start_with(pods: usize, setup: impl FnOnce(&fake::FakeApi) + Send + 'static) -> Self {
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
                setup(&api);
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
            webhook_alternate: None,
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
    let mut timeline = Timeline::default();
    let ok = wait_for(Duration::from_secs(60), || {
        timeline.sample(&cluster, &[&h1, &h2]);
        h1.endpoints().read().unwrap().len() == 2 && h2.endpoints().read().unwrap().len() == 2
    });
    assert!(ok, "{timeline}");
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

/// Lease name, holder, ready sequence, and published endpoint count. Only
/// discovery metadata: no tokens or key material.
async fn lease_summary(k: &Kube, name: &str) -> (String, String, usize) {
    let v = k
        .get(&format!(
            "/apis/coordination.k8s.io/v1/namespaces/{NS}/leases/{name}"
        ))
        .await
        .unwrap_or_default();
    let endpoints = str_at(&v, &["metadata", "annotations", annotations::ENDPOINTS]);
    (
        str_at(&v, &["spec", "holderIdentity"]).to_owned(),
        str_at(&v, &["metadata", "annotations", annotations::READY]).to_owned(),
        crate::seat::parse_endpoints(Some(&endpoints.to_owned())).len(),
    )
}

/// Timestamped changes of roles, readiness, and discovery Leases, reported
/// when convergence fails so the slow hop is visible.
#[derive(Default)]
struct Timeline {
    start: Option<Instant>,
    last: String,
    lines: Vec<String>,
}

impl Timeline {
    fn sample(&mut self, cluster: &FakeCluster, handles: &[&crate::Handle]) {
        let start = *self.start.get_or_insert_with(Instant::now);
        let mut now = String::new();
        for h in handles {
            let d = h.diagnostics();
            now += &format!(
                "{:?}/ready={}/endpoints={} ",
                d.role,
                d.ready,
                h.endpoints().read().unwrap().len()
            );
        }
        now += &cluster.inspect(async |k| {
            let mut s = String::new();
            let names = (0..3)
                .map(annotations::seat_lease)
                .chain([annotations::PRIMARY_LEASE.to_owned()]);
            for name in names {
                let (holder, ready, endpoints) = lease_summary(k, &name).await;
                s += &format!("{name}:{holder}/r{ready}/e{endpoints} ");
            }
            s
        });
        if now != self.last {
            self.lines
                .push(format!("{:.1}s {now}", start.elapsed().as_secs_f64()));
            self.last = now;
        }
    }
}

impl std::fmt::Display for Timeline {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.lines.join("\n"))
    }
}

/// A seat that becomes ready writes that to its Lease right away, and the
/// primary publishes it within one reconcile period instead of waiting for
/// the next seat renewal and then the next primary renewal.
#[test]
fn readiness_is_published_without_waiting_for_renewals() {
    let cluster = FakeCluster::start(1);
    let mut config = cluster.config(1);
    config.seats = SeatPolicy::Fixed(1);
    let h = crate::start(config).unwrap();
    let mut timeline = Timeline::default();
    assert!(
        wait_for(Duration::from_secs(30), || {
            timeline.sample(&cluster, &[&h]);
            h.diagnostics().ready
        }),
        "{timeline}"
    );
    let ready = Instant::now();
    let seat = annotations::seat_lease(0);
    let written =
        |cluster: &FakeCluster| cluster.inspect(async |k| lease_summary(k, &seat).await.1 == "1");
    assert!(
        wait_for(Duration::from_secs(3), || {
            timeline.sample(&cluster, &[&h]);
            written(&cluster)
        }),
        "ready not written before the seat renewal:\n{timeline}"
    );
    // One reconcile period plus debounce and slack, short of the
    // renewal-paced path (seat renewal, then reconcile, then primary renewal).
    let budget = crate::limits::LEASE_RENEW + Duration::from_secs(5);
    assert!(
        wait_for(budget.saturating_sub(ready.elapsed()), || {
            timeline.sample(&cluster, &[&h]);
            h.endpoints().read().unwrap().len() == 1
        }),
        "endpoint not published within {budget:?} of ready:\n{timeline}"
    );
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

/// A fresh Node with no local files must not adopt a racer-state Secret the
/// installation did not record: bootstrap fails closed before any seat is
/// taken or state is initialized.
#[test]
fn bootstrap_requires_the_installation_state_binding() {
    const REPLACED: &str = "c1c1c1c1-0000-4000-8000-0000000000ff";
    let ds = format!("/apis/apps/v1/namespaces/{NS}/daemonsets/racer-dataplane");
    let sa = format!("/api/v1/namespaces/{NS}/serviceaccounts/racer-dataplane");
    let state = format!("/api/v1/namespaces/{NS}/secrets/racer-state");
    type Mutate = fn(&mut Value, &mut Value, &mut Value);
    let cases: [(&str, Mutate); 6] = [
        ("replaced secret", |s, _, _| {
            s["metadata"]["uid"] = REPLACED.into()
        }),
        ("unannotated daemonset", |_, d, _| {
            d["metadata"]["annotations"] = json!({})
        }),
        ("mismatched service account", |_, _, a| {
            a["metadata"]["annotations"][crate::control::STATE_UID] = REPLACED.into()
        }),
        ("terminating daemonset", |_, d, _| {
            d["metadata"]["deletionTimestamp"] = "2026-01-01T00:00:00Z".into()
        }),
        ("missing daemonset", |_, d, _| *d = Value::Null),
        ("missing service account", |_, _, a| *a = Value::Null),
    ];
    for (name, mutate) in cases {
        let (st, ds, sa) = (state.clone(), ds.clone(), sa.clone());
        let cluster = FakeCluster::start_with(1, move |api| {
            let mut s = api.object(&st).unwrap();
            let mut d = api.object(&ds).unwrap();
            let mut a = api.object(&sa).unwrap();
            mutate(&mut s, &mut d, &mut a);
            for (path, obj) in [(&st, s), (&ds, d), (&sa, a)] {
                if obj.is_null() {
                    api.delete_object(path);
                } else {
                    api.put_object(path, obj);
                }
            }
        });
        let h = crate::start(cluster.config(1)).unwrap();
        assert!(
            matches!(
                h.bootstrap(Duration::from_secs(20)),
                Err(crate::Error::InvalidState)
            ),
            "{name}"
        );
        std::thread::sleep(Duration::from_millis(500));
        assert_eq!(h.diagnostics().role, crate::Role::Member, "{name}");
        let secret = cluster.inspect(async |k| k.get(&state).await.unwrap());
        assert!(secret["data"].get("state").is_none(), "{name}");
        assert!(
            str_at(
                &secret,
                &["metadata", "annotations", annotations::INITIALIZED]
            )
            .is_empty(),
            "{name}"
        );
        drop(h);
    }
}

/// Start one Pod holding seat 0 and the primary Lease, wait until ready.
fn ready_single(cluster: &FakeCluster) -> crate::Handle {
    let mut config = cluster.config(1);
    config.seats = SeatPolicy::Fixed(1);
    let h = crate::start(config).unwrap();
    let mut timeline = Timeline::default();
    assert!(
        wait_for(Duration::from_secs(30), || {
            timeline.sample(cluster, &[&h]);
            h.diagnostics().ready && matches!(h.diagnostics().role, crate::Role::Primary(0))
        }),
        "{timeline}"
    );
    h
}

fn holders(cluster: &FakeCluster) -> (String, String) {
    let seat = annotations::seat_lease(0);
    cluster.inspect(async |k| {
        (
            lease_summary(k, &seat).await.0,
            lease_summary(k, annotations::PRIMARY_LEASE).await.0,
        )
    })
}

#[test]
fn shutdown_releases_seat_and_primary_leases() {
    let delay = Arc::new(AtomicU64::new(0));
    let d = delay.clone();
    let cluster = FakeCluster::start_with(1, move |api| api.delay_puts(d));
    let h = ready_single(&cluster);
    assert_eq!(holders(&cluster), ("p1".into(), "p1".into()));
    // Slow releases: a shutdown that does not wait for them returns first.
    delay.store(500, Ordering::SeqCst);
    let stopping = Instant::now();
    h.shutdown();
    let took = stopping.elapsed();
    assert!(took < Duration::from_secs(5), "{took:?}");
    // Released before the control thread exited, not left to expire.
    assert_eq!(
        holders(&cluster),
        (String::new(), String::new()),
        "{took:?}"
    );
}

#[test]
fn shutdown_is_bounded_when_releases_hang() {
    let delay = Arc::new(AtomicU64::new(0));
    let d = delay.clone();
    let cluster = FakeCluster::start_with(1, move |api| api.delay_puts(d));
    let h = ready_single(&cluster);
    delay.store(u64::MAX, Ordering::SeqCst);
    let stopping = Instant::now();
    h.shutdown();
    let took = stopping.elapsed();
    assert!(took < Duration::from_secs(8), "{took:?}");
    assert!(
        took >= Duration::from_secs(4),
        "did not wait for releases: {took:?}"
    );
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

/// `start` binds the webhook on both Pod IP families at one port, and each
/// serves the seat's published webhook certificate under the Service DNS
/// name, as the API server verifies it.
#[test]
fn start_serves_the_webhook_on_both_pod_ip_families() {
    let cluster = FakeCluster::start(1);
    let mut config = cluster.config(1);
    // Ephemeral ports differ per bind; use one port free on both families.
    let port = loop {
        let v4 = std::net::TcpListener::bind("127.0.0.2:0").unwrap();
        let port = v4.local_addr().unwrap().port();
        if std::net::TcpListener::bind(("::1", port)).is_ok() {
            break port;
        }
    };
    config.webhook_listen = std::net::SocketAddr::new(config.pod_ip, port);
    config.webhook_alternate = Some("::1".parse().unwrap());
    let h = crate::start(config).unwrap();
    h.bootstrap(Duration::from_secs(20)).unwrap();
    let mut published = None;
    assert!(
        wait_for(Duration::from_secs(60), || {
            published = cluster.inspect(async |k| {
                for seat in 0..annotations::MAX_SEATS {
                    let path = format!(
                        "/apis/coordination.k8s.io/v1/namespaces/{NS}/leases/{}",
                        annotations::seat_lease(seat)
                    );
                    let Ok(lease) = k.get(&path).await else {
                        continue;
                    };
                    let a = &lease["metadata"]["annotations"];
                    if let (Some(ips), Some(ca)) = (
                        a[annotations::WEBHOOK_ADDRESSES].as_str(),
                        a[annotations::WEBHOOK_CERTIFICATE].as_str(),
                    ) {
                        return Some((ips.to_owned(), STANDARD.decode(ca).unwrap()));
                    }
                }
                None
            });
            published.is_some()
        }),
        "{:?}",
        h.diagnostics()
    );
    let (ips, ca) = published.unwrap();
    assert_eq!(ips, "127.0.0.2,::1");
    let mut roots = rustls::RootCertStore::empty();
    roots
        .add(rustls::pki_types::CertificateDer::from(ca))
        .unwrap();
    let tls = Arc::new(
        rustls::ClientConfig::builder_with_provider(Arc::new(
            rustls::crypto::ring::default_provider(),
        ))
        .with_safe_default_protocol_versions()
        .unwrap()
        .with_root_certificates(roots)
        .with_no_client_auth(),
    );
    for ip in ips.split(',') {
        let address = std::net::SocketAddr::new(ip.parse().unwrap(), port);
        let tls = tls.clone();
        let status = Executor::new(64).unwrap().block_on(async move {
            let name =
                rustls::pki_types::ServerName::try_from(format!("racer-webhook.{NS}.svc")).unwrap();
            let tcp = Tcp::connect(address).await.unwrap();
            let mut conn = Conn::new(TlsStream::client(tcp, tls, name).await.unwrap());
            conn.tls
                .write_all(&http::request("POST", "x", "/other", &[], b""))
                .await
                .unwrap();
            conn.read_head(false, 4096).await.unwrap().unwrap().status
        });
        assert_eq!(status, 404, "{address}");
    }
    h.shutdown();
}
