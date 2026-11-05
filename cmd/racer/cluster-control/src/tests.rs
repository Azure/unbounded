//! End-to-end tests of the runtime against the fake Kubernetes API.
//!
//! Most run on the simulated driver in [`sim`]. The tests here use real
//! sockets and threads on purpose: they cover [`crate::start`], the kernel
//! driver, and TLS over loopback.

mod sim;

use crate::{
    Config, SeatPolicy, annotations,
    config::Rotation,
    conn::{self as http, Conn},
    kube::{Kube, KubeConfig, fake},
    rt::{self, Executor, Tcp},
    tls::TlsStream,
};
use base64::{
    Engine,
    engine::general_purpose::{STANDARD, URL_SAFE_NO_PAD},
};
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
        &format!("/apis/apps/v1/namespaces/{NS}/daemonsets/racer"),
        json!({"metadata":{"name":"racer","uid":DS_UID,"annotations":{crate::control::STATE_UID:SECRET_UID}}}),
    );
    api.put_object(
        &format!("/api/v1/namespaces/{NS}/serviceaccounts/racer"),
        json!({"metadata":{"name":"racer","uid":SA_UID,"annotations":{crate::control::STATE_UID:SECRET_UID}}}),
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
                    // Serving dataplanes have reported.
                    "annotations":{annotations::REPORT:"{\"shares\":1,\"rdma_nics\":[]}"},
                    "ownerReferences":[{"apiVersion":"apps/v1","kind":"DaemonSet","name":"racer","uid":DS_UID,"controller":true}]},
                "spec":{"nodeName":format!("n{i}"),"serviceAccountName":"racer"},
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
            "user": {"username": format!("system:serviceaccount:{NS}:racer"), "uid": SA_UID, "extra": {
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
    stop: Arc<AtomicBool>,
    thread: Option<std::thread::JoinHandle<()>>,
    dir: PathBuf,
}

impl FakeCluster {
    fn start(pods: usize) -> Self {
        let dir = fake::scratch();
        let stop = Arc::new(AtomicBool::new(false));
        let (tx, rx) = std::sync::mpsc::channel();
        let (d, s) = (dir.clone(), stop.clone());
        let thread = std::thread::spawn(move || {
            let mut ex = Executor::new(4096).unwrap();
            ex.block_on(async move {
                let api = fake::FakeApi::new();
                seed(&api, pods);
                tx.send(fake::serve(api, &d.join("api"))).unwrap();
                while !s.load(Ordering::SeqCst) {
                    rt::sleep(Duration::from_millis(50)).await;
                }
            });
        });
        let kube = rx.recv().unwrap();
        Self {
            kube,
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
            daemonset: "racer".into(),
            service_account: "racer".into(),
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
