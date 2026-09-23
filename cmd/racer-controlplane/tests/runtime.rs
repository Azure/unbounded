// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use anyhow::Result;
use axum::{
    Router,
    body::{Body, to_bytes},
    extract::State,
    http::{Request, StatusCode},
    response::Response,
};
use futures::stream;
use kube::{Client, Config, api::DynamicObject, runtime::watcher::Event};
use prost::Message;
use racer_controlplane::{
    kubernetes::{InventoryIndex, Kind, RecordStore, Runtime, RuntimeOptions, SecurityContext},
    model::identity,
    proto::DesiredState,
    security::*,
    status::Observation,
};
use serde_json::{Value, json};
use std::{
    collections::{BTreeMap, BTreeSet},
    sync::{Arc, Mutex, RwLock},
    time::Duration,
};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::{TcpListener, TcpStream},
    sync::broadcast,
};
use tokio_util::sync::CancellationToken;
use tower::ServiceExt;

#[derive(Clone, Default)]
struct CaMemory(Arc<Mutex<StoreSnapshot>>);
impl CaStore for CaMemory {
    async fn read(&self) -> Result<StoreSnapshot> {
        Ok(self.0.lock().unwrap().clone())
    }
    async fn commit(&self, expected: &StoreSnapshot, next: &StateImage) -> Result<CommitOutcome> {
        let mut value = self.0.lock().unwrap();
        if value.resource_version != expected.resource_version {
            return Ok(CommitOutcome::Conflict);
        }
        value.resource_version = Some(
            value
                .resource_version
                .as_deref()
                .unwrap_or("0")
                .parse::<u64>()?
                .saturating_add(1)
                .to_string(),
        );
        value.image = Some(next.clone());
        Ok(CommitOutcome::Committed)
    }
    async fn publish(
        &self,
        expected: &StoreSnapshot,
        fence: &str,
        bytes: &[u8],
    ) -> Result<CommitOutcome> {
        let mut value = self.0.lock().unwrap();
        if value.resource_version != expected.resource_version {
            return Ok(CommitOutcome::Conflict);
        }
        value.publication = Some(Publication {
            resource_version: value.resource_version.clone().unwrap(),
            fence: fence.into(),
            bytes: bytes.into(),
        });
        Ok(CommitOutcome::Committed)
    }
}

struct Credentials {
    context: SecurityContext,
    server: Arc<TlsSnapshot>,
    client: Arc<rustls::ClientConfig>,
    peer: VerifiedPeer,
    boot: String,
}
async fn credentials() -> Credentials {
    let manager = CaManager::acquire(
        CaMemory::default(),
        Leadership::new("term-a".into()).unwrap(),
        SecurityOptions::new("system"),
        unix_now(),
    )
    .await
    .unwrap();
    let boot = "c".repeat(64);
    let node = Identity {
        kind: IdentityKind::Node,
        universe: identity("universe", "site-a"),
        node: identity("node", "node-uid"),
        pod_uid: "pod-uid".into(),
        boot_id: boot.clone(),
        pod_name: "racer-a".into(),
        container_id: "".into(),
    };
    let cp = Identity {
        kind: IdentityKind::ControlPlane,
        universe: "".into(),
        node: "".into(),
        pod_uid: "control-uid".into(),
        boot_id: "d".repeat(64),
        pod_name: "control".into(),
        container_id: "".into(),
    };
    let nk = generate_local_key().unwrap();
    let ck = generate_local_key().unwrap();
    let nl = manager
        .issue(&nk.csr_pem, node, false, unix_now())
        .await
        .unwrap();
    let cl = manager
        .issue(&ck.csr_pem, cp, false, unix_now())
        .await
        .unwrap();
    let state = manager.state().await.unwrap();
    let bundle = state.bundle();
    let server =
        Arc::new(TlsSnapshot::new(&bundle.json(), &cl.certificate_pem, &ck.key_pem).unwrap());
    let mut roots = rustls::RootCertStore::empty();
    for cert in openssl::x509::X509::stack_from_pem(bundle.certificates.as_bytes()).unwrap() {
        roots
            .add(rustls::pki_types::CertificateDer::from(
                cert.to_der().unwrap(),
            ))
            .unwrap();
    }
    let cert = openssl::x509::X509::from_pem(&nl.certificate_pem)
        .unwrap()
        .to_der()
        .unwrap();
    let key = openssl::pkey::PKey::private_key_from_pem(&nk.key_pem)
        .unwrap()
        .private_key_to_pkcs8()
        .unwrap();
    let client = Arc::new(
        rustls::ClientConfig::builder_with_provider(Arc::new(
            rustls::crypto::ring::default_provider(),
        ))
        .with_protocol_versions(&[&rustls::version::TLS13])
        .unwrap()
        .with_root_certificates(roots)
        .with_client_auth_cert(
            vec![cert.into()],
            rustls::pki_types::PrivateKeyDer::Pkcs8(key.into()),
        )
        .unwrap(),
    );
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let accept = {
        let server = server.clone();
        tokio::spawn(async move {
            server
                .accept(listener.accept().await.unwrap().0, true)
                .await
                .unwrap()
                .1
                .unwrap()
        })
    };
    let _stream = tokio_rustls::TlsConnector::from(client.clone())
        .connect(
            rustls::pki_types::ServerName::try_from("racer-controlplane.system.svc").unwrap(),
            TcpStream::connect(address).await.unwrap(),
        )
        .await
        .unwrap();
    let peer = accept.await.unwrap();
    Credentials {
        context: SecurityContext {
            fence: "term-a".into(),
            state: Arc::new(state),
        },
        server,
        client,
        peer,
        boot,
    }
}

#[derive(Clone)]
struct FakeApi {
    inner: Arc<Mutex<ApiData>>,
    events: broadcast::Sender<(String, Value)>,
}
#[derive(Default)]
struct ApiData {
    objects: BTreeMap<String, Value>,
    revision: u64,
    writes: usize,
    fail_after_pointer: bool,
    fail_delete_after: Option<usize>,
    list_pages: usize,
    fail_after_chunk: bool,
    pause_chunk: Option<Arc<tokio::sync::Notify>>,
}
impl FakeApi {
    fn new() -> Self {
        Self {
            inner: Default::default(),
            events: broadcast::channel(4096).0,
        }
    }
    fn put(&self, path: &str, mut object: Value) {
        let mut data = self.inner.lock().unwrap();
        data.revision += 1;
        object["metadata"]["resourceVersion"] = json!(data.revision.to_string());
        data.objects.insert(path.into(), object.clone());
        let _ = self.events.send((
            path.rsplit_once('/').unwrap().0.into(),
            json!({"type":"MODIFIED", "object":object}),
        ));
    }
    async fn serve(&self) -> (Client, tokio::task::JoinHandle<()>) {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let app = Router::new()
            .fallback(fake_request)
            .with_state(self.clone());
        let task = tokio::spawn(async move {
            axum::serve(listener, app).await.unwrap();
        });
        let client =
            Client::try_from(Config::new(format!("http://{address}").parse().unwrap())).unwrap();
        (client, task)
    }
}

fn reply(code: StatusCode, value: Value) -> Response {
    Response::builder()
        .status(code)
        .header("content-type", "application/json")
        .body(Body::from(value.to_string()))
        .unwrap()
}
fn api_error(code: StatusCode) -> Response {
    reply(
        code,
        json!({"kind":"Status", "apiVersion":"v1", "status":"Failure", "message":"injected", "reason":if code == StatusCode::NOT_FOUND { "NotFound" } else { code.canonical_reason().unwrap_or("Unknown") }, "code":code.as_u16()}),
    )
}
async fn fake_request(State(api): State<FakeApi>, request: Request<Body>) -> Response {
    let path = request.uri().path().to_string();
    let query = request.uri().query().unwrap_or_default().to_string();
    let method = request.method().clone();
    if method == "GET" && query.contains("watch=true") {
        let receiver = api.events.subscribe();
        let initial = if query.contains("sendInitialEvents=true") {
            let data = api.inner.lock().unwrap();
            let mut events: Vec<_> = data
                .objects
                .iter()
                .filter(|(p, _)| p.rsplit_once('/').unwrap().0 == path)
                .map(|(_, o)| json!({"type":"ADDED", "object":o}))
                .collect();
            events.push(json!({"type":"BOOKMARK", "object":{"apiVersion":"v1", "kind":"PartialObjectMetadata", "metadata":{"resourceVersion":data.revision.to_string(),"annotations":{"k8s.io/initial-events-end":"true"}}}}));
            events
        } else {
            vec![]
        };
        let body = stream::unfold(
            (receiver, path, initial.into_iter()),
            |(mut receiver, path, mut initial)| async move {
                if let Some(value) = initial.next() {
                    return Some((
                        Ok::<_, std::io::Error>(format!("{value}\n")),
                        (receiver, path, initial),
                    ));
                }
                loop {
                    match receiver.recv().await {
                        Ok((scope, event)) if scope == path => {
                            return Some((Ok(format!("{event}\n")), (receiver, path, initial)));
                        }
                        Ok(_) => {}
                        Err(_) => return None,
                    }
                }
            },
        );
        return Response::builder()
            .header("content-type", "application/json")
            .body(Body::from_stream(body))
            .unwrap();
    }
    if method == "GET" {
        let mut data = api.inner.lock().unwrap();
        if let Some(object) = data.objects.get(&path) {
            return reply(StatusCode::OK, object.clone());
        }
        if ["nodes", "pods", "sites", "p2pcaches", "configmaps"]
            .iter()
            .any(|s| path.ends_with(&format!("/{s}")))
        {
            let mut items: Vec<_> = data
                .objects
                .iter()
                .filter(|(p, _)| p.rsplit_once('/').unwrap().0 == path)
                .map(|(_, o)| o.clone())
                .collect();
            if query.contains("labelSelector=") {
                let label = if query.contains("chunk") {
                    "chunk"
                } else {
                    "pointer"
                };
                items.retain(|o| {
                    o.pointer("/metadata/labels/racer.unbounded-cloud.io~1rust-state")
                        == Some(&json!(label))
                });
            }
            let parameter = |name: &str| {
                query
                    .split('&')
                    .find_map(|part| part.strip_prefix(&format!("{name}=")))
                    .and_then(|s| s.parse::<usize>().ok())
            };
            let start = parameter("continue").unwrap_or(0);
            let limit = parameter("limit").unwrap_or(items.len().max(1));
            let end = (start + limit).min(items.len());
            let continuation = if end < items.len() {
                end.to_string()
            } else {
                String::new()
            };
            items = items.into_iter().skip(start).take(limit).collect();
            data.list_pages += 1;
            return reply(
                StatusCode::OK,
                json!({"apiVersion":"v1", "kind":"List", "metadata":{"resourceVersion":data.revision.to_string(),"continue":continuation}, "items":items}),
            );
        }
        return api_error(StatusCode::NOT_FOUND);
    }
    let bytes = to_bytes(request.into_body(), 2 * 1024 * 1024)
        .await
        .unwrap();
    let mut object: Value = serde_json::from_slice(&bytes).unwrap();
    let (key, object, fail, pause) = {
        let mut data = api.inner.lock().unwrap();
        if method == "DELETE" {
            if data.fail_delete_after == Some(0) {
                data.fail_delete_after = None;
                return api_error(StatusCode::GATEWAY_TIMEOUT);
            }
            if let Some(remaining) = data.fail_delete_after.as_mut() {
                *remaining -= 1;
            }
            let Some(old) = data.objects.get(&path) else {
                return api_error(StatusCode::NOT_FOUND);
            };
            if old["metadata"]["resourceVersion"] != object["preconditions"]["resourceVersion"] {
                return api_error(StatusCode::CONFLICT);
            }
            data.objects.remove(&path);
            return reply(
                StatusCode::OK,
                json!({"apiVersion":"v1","kind":"Status","status":"Success","code":200}),
            );
        }
        let key = if method == "POST" {
            format!("{path}/{}", object["metadata"]["name"].as_str().unwrap())
        } else {
            path.trim_end_matches("/status").into()
        };
        if method == "POST" && data.objects.contains_key(&key) {
            return api_error(StatusCode::CONFLICT);
        }
        if method != "POST"
            && data.objects.get(&key).is_none_or(|old| {
                old["metadata"]["resourceVersion"] != object["metadata"]["resourceVersion"]
            })
        {
            return api_error(StatusCode::CONFLICT);
        }
        if method == "PATCH" {
            let mut old = data.objects[&key].clone();
            if let Some(status) = object.get("status") {
                old["status"] = status.clone();
            }
            if let Some(annotations) = object
                .pointer("/metadata/annotations")
                .and_then(Value::as_object)
            {
                if !old["metadata"]["annotations"].is_object() {
                    old["metadata"]["annotations"] = json!({});
                }
                old["metadata"]["annotations"]
                    .as_object_mut()
                    .unwrap()
                    .extend(annotations.clone());
            }
            object = old;
        }
        data.revision += 1;
        data.writes += 1;
        object["metadata"]["resourceVersion"] = json!(data.revision.to_string());
        data.objects.insert(key.clone(), object.clone());
        let fail = data.fail_after_pointer
            && key.contains("racer-v4-topology-")
            && object
                .pointer("/data/pointer")
                .and_then(Value::as_str)
                .and_then(|s| serde_json::from_str::<Value>(s).ok())
                .is_some_and(|p| p["proposed"] == json!([]));
        if key.contains("racer-v4-chunk-") && data.fail_after_chunk {
            data.fail_after_chunk = false;
            return api_error(StatusCode::GATEWAY_TIMEOUT);
        }
        let pause = if key.contains("racer-v4-chunk-") {
            data.pause_chunk.take()
        } else {
            None
        };
        if fail {
            data.fail_after_pointer = false;
        }
        (key, object, fail, pause)
    };
    if let Some(pause) = pause {
        pause.notified().await;
    }
    let _ = api.events.send((
        key.rsplit_once('/').unwrap().0.into(),
        json!({"type":"MODIFIED", "object":object}),
    ));
    if fail {
        api_error(StatusCode::GATEWAY_TIMEOUT)
    } else {
        reply(StatusCode::OK, object)
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn chunk_gc_preserves_staging_recovers_interruption_and_fences_takeover() {
    let credentials = credentials().await;
    let shared = Arc::new(RwLock::new(Some(credentials.context.clone())));
    let getter: racer_controlplane::subscription::SecurityGetter = {
        let shared = shared.clone();
        Arc::new(move || shared.read().unwrap().clone())
    };
    let api = FakeApi::new();
    seed(&api);
    let (client, task) = api.serve().await;
    let store = RecordStore::new(client.clone(), "system", getter.clone());
    let record = store
        .commit(
            "racer-v4-topology-gc",
            "topology",
            None,
            b"committed",
            "term-a",
        )
        .await
        .unwrap();
    let pause = Arc::new(tokio::sync::Notify::new());
    api.inner.lock().unwrap().pause_chunk = Some(pause.clone());
    let writer = {
        let store = store.clone();
        let record = record.clone();
        tokio::spawn(async move {
            store
                .commit(
                    "racer-v4-topology-gc",
                    "topology",
                    Some(&record),
                    b"proposed",
                    "term-a",
                )
                .await
        })
    };
    until(|| {
        api.inner.lock().unwrap().objects.values().any(|o| {
            o.get("binaryData")
                .is_some_and(|d| d.to_string().contains("cHJvcG9zZWQ="))
        })
    })
    .await;
    let gc = {
        let store = store.clone();
        tokio::spawn(async move { store.collect_garbage("term-a").await })
    };
    tokio::time::sleep(Duration::from_millis(50)).await;
    assert!(!gc.is_finished(), "GC must wait for proposed chunk staging");
    pause.notify_one();
    writer.await.unwrap().unwrap();
    gc.await.unwrap().unwrap();
    assert_eq!(
        store
            .load("racer-v4-topology-gc")
            .await
            .unwrap()
            .unwrap()
            .bytes,
        b"proposed"
    );

    // Crash after chunk persistence leaves an abandoned reservation. Clearing it
    // fences late completion; the last committed content survives collection.
    api.inner.lock().unwrap().fail_after_chunk = true;
    let old = store.load("racer-v4-topology-gc").await.unwrap().unwrap();
    assert!(
        store
            .commit(
                "racer-v4-topology-gc",
                "topology",
                Some(&old),
                b"abandoned",
                "term-a"
            )
            .await
            .is_err()
    );
    for i in 0..140 {
        let bytes = format!("orphan-{i}").into_bytes();
        use base64::Engine;
        use sha2::Digest;
        let digest = hex::encode(sha2::Sha256::digest(&bytes));
        api.put(&format!("/api/v1/namespaces/system/configmaps/racer-v4-chunk-{digest}"), json!({"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":format!("racer-v4-chunk-{digest}"),"labels":{"racer.unbounded-cloud.io/rust-state":"chunk"}},"immutable":true,"binaryData":{"content":base64::engine::general_purpose::STANDARD.encode(bytes)}}));
    }
    api.inner.lock().unwrap().fail_delete_after = Some(3);
    assert!(store.collect_garbage("term-a").await.is_err());
    assert_eq!(
        store
            .load("racer-v4-topology-gc")
            .await
            .unwrap()
            .unwrap()
            .bytes,
        b"proposed"
    );
    // New leader can claim a gate left by a crashed predecessor.
    let image = credentials.context.state.to_image().unwrap();
    let mut metadata: Value = serde_json::from_slice(&image.metadata).unwrap();
    metadata["fence"] = json!("term-b");
    let state = CaState::from_image(&StateImage {
        metadata: serde_json::to_vec(&metadata).unwrap(),
        shards: image.shards,
    })
    .unwrap();
    *shared.write().unwrap() = Some(SecurityContext {
        fence: "term-b".into(),
        state: Arc::new(state),
    });
    let mut lease = api.inner.lock().unwrap().objects["/apis/coordination.k8s.io/v1/namespaces/system/leases/racer-controlplane"].clone();
    lease["spec"]["holderIdentity"] = json!("term-b");
    api.put(
        "/apis/coordination.k8s.io/v1/namespaces/system/leases/racer-controlplane",
        lease,
    );
    use base64::Engine;
    let mut secret =
        api.inner.lock().unwrap().objects["/api/v1/namespaces/system/secrets/racer-ca"].clone();
    secret["data"]["state.json"] =
        json!(base64::engine::general_purpose::STANDARD.encode(br#"{"fence":"term-b"}"#));
    api.put("/api/v1/namespaces/system/secrets/racer-ca", secret);
    api.put("/api/v1/namespaces/system/configmaps/racer-v4-store-gate", json!({"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"racer-v4-store-gate","labels":{"racer.unbounded-cloud.io/rust-state":"gate"}},"data":{"fence":"term-a","operation":"crashed"}}));
    assert!(store.collect_garbage("term-a").await.is_err());
    // Fake pagination uses offsets, so retry from a fresh list after deletion.
    for _ in 0..4 {
        store.collect_garbage("term-b").await.unwrap();
    }
    let current = store.load("racer-v4-topology-gc").await.unwrap().unwrap();
    assert_eq!(current.bytes, b"proposed");
    assert!(
        store
            .commit(
                "racer-v4-topology-gc",
                "topology",
                Some(&old),
                b"late",
                "term-a"
            )
            .await
            .is_err()
    );
    let data = api.inner.lock().unwrap();
    assert_eq!(
        data.objects
            .keys()
            .filter(|k| k.contains("racer-v4-chunk-"))
            .count(),
        1
    );
    assert!(data.list_pages > 4);
    drop(data);
    task.abort();
}

/// RecordStore-only real API seam; the separate binary harness owns full service
/// startup. Opt in with assets and keep all API files under the crate target.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn record_store_real_kubernetes_api() {
    let Ok(assets) = std::env::var("KUBEBUILDER_ASSETS") else {
        eprintln!("KUBEBUILDER_ASSETS unavailable; real RecordStore API scenario not run");
        return;
    };
    fn port() -> u16 {
        std::net::TcpListener::bind("127.0.0.1:0")
            .unwrap()
            .local_addr()
            .unwrap()
            .port()
    }
    struct Processes {
        children: Vec<std::process::Child>,
        root: std::path::PathBuf,
    }
    impl Drop for Processes {
        fn drop(&mut self) {
            for child in &mut self.children {
                let _ = child.kill();
                let _ = child.wait();
            }
            if std::thread::panicking() {
                eprintln!(
                    "{}",
                    std::fs::read_to_string(self.root.join("apiserver.log")).unwrap_or_default()
                );
            }
            let _ = std::fs::remove_dir_all(&self.root);
        }
    }
    let root = std::env::current_dir()
        .unwrap()
        .join("target")
        .join(format!("record-api-{}", uuid::Uuid::new_v4()));
    std::fs::create_dir_all(&root).unwrap();
    let (etcd_port, peer_port, api_port) = (port(), port(), port());
    let token = "record-store-integration";
    std::fs::write(
        root.join("tokens.csv"),
        format!("{token},integration,1,system:masters\n"),
    )
    .unwrap();
    let key = openssl::pkey::PKey::from_rsa(openssl::rsa::Rsa::generate(2048).unwrap()).unwrap();
    std::fs::write(
        root.join("service.key"),
        key.private_key_to_pem_pkcs8().unwrap(),
    )
    .unwrap();
    let mut processes = Processes {
        children: vec![],
        root: root.clone(),
    };
    let mut etcd = std::process::Command::new(format!("{assets}/etcd"));
    etcd.args([
        "--name=record-api",
        &format!("--data-dir={}", root.join("etcd").display()),
        &format!("--listen-client-urls=http://127.0.0.1:{etcd_port}"),
        &format!("--advertise-client-urls=http://127.0.0.1:{etcd_port}"),
        &format!("--listen-peer-urls=http://127.0.0.1:{peer_port}"),
        &format!("--initial-advertise-peer-urls=http://127.0.0.1:{peer_port}"),
        &format!("--initial-cluster=record-api=http://127.0.0.1:{peer_port}"),
    ]);
    etcd.stdout(std::process::Stdio::null())
        .stderr(std::fs::File::create(root.join("etcd.log")).unwrap());
    processes.children.push(etcd.spawn().unwrap());
    let mut api = std::process::Command::new(format!("{assets}/kube-apiserver"));
    api.args([
        &format!("--etcd-servers=http://127.0.0.1:{etcd_port}"),
        &format!("--secure-port={api_port}"),
        "--bind-address=127.0.0.1",
        "--advertise-address=192.0.2.1",
        "--authorization-mode=AlwaysAllow",
        "--service-cluster-ip-range=10.99.0.0/24",
        &format!("--cert-dir={}", root.display()),
        &format!("--token-auth-file={}", root.join("tokens.csv").display()),
        "--service-account-issuer=https://integration.invalid",
        &format!(
            "--service-account-signing-key-file={}",
            root.join("service.key").display()
        ),
        &format!(
            "--service-account-key-file={}",
            root.join("service.key").display()
        ),
        "--disable-admission-plugins=ServiceAccount",
    ]);
    api.stdout(std::process::Stdio::null())
        .stderr(std::fs::File::create(root.join("apiserver.log")).unwrap());
    processes.children.push(api.spawn().unwrap());
    let mut config = Config::new(format!("https://127.0.0.1:{api_port}").parse().unwrap());
    config.accept_invalid_certs = true;
    config.auth_info.token = Some(token.to_string().into());
    let client = Client::try_from(config).unwrap();
    tokio::time::timeout(Duration::from_secs(45), async {
        loop {
            if client.apiserver_version().await.is_ok() {
                break;
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
    })
    .await
    .unwrap();
    use k8s_openapi::{
        api::{
            coordination::v1::Lease,
            core::v1::{ConfigMap, Namespace, Secret},
        },
        apimachinery::pkg::apis::meta::v1::ObjectMeta,
    };
    use kube::{Api, api::PostParams};
    Api::<Namespace>::all(client.clone())
        .create(
            &PostParams::default(),
            &Namespace {
                metadata: ObjectMeta {
                    name: Some("system".into()),
                    ..Default::default()
                },
                ..Default::default()
            },
        )
        .await
        .unwrap();
    let fixture = FakeApi::new();
    seed(&fixture);
    let objects = fixture.inner.lock().unwrap().objects.clone();
    let lease: Lease = serde_json::from_value(
        objects["/apis/coordination.k8s.io/v1/namespaces/system/leases/racer-controlplane"].clone(),
    )
    .unwrap();
    let mut lease = lease;
    lease.metadata.resource_version = None;
    Api::<Lease>::namespaced(client.clone(), "system")
        .create(&PostParams::default(), &lease)
        .await
        .unwrap();
    let mut secret: Secret =
        serde_json::from_value(objects["/api/v1/namespaces/system/secrets/racer-ca"].clone())
            .unwrap();
    secret.metadata.resource_version = None;
    Api::<Secret>::namespaced(client.clone(), "system")
        .create(&PostParams::default(), &secret)
        .await
        .unwrap();
    let context = credentials().await.context;
    let getter: racer_controlplane::subscription::SecurityGetter =
        Arc::new(move || Some(context.clone()));
    let store = RecordStore::new(client.clone(), "system", getter);
    let first = store
        .commit(
            "racer-v4-topology-real",
            "topology",
            None,
            &vec![1; 600_000],
            "term-a",
        )
        .await
        .unwrap();
    let second = store
        .commit(
            "racer-v4-topology-real",
            "topology",
            Some(&first),
            &vec![2; 700_000],
            "term-a",
        )
        .await
        .unwrap();
    assert!(
        store
            .commit(
                "racer-v4-topology-real",
                "topology",
                Some(&first),
                b"stale",
                "term-a"
            )
            .await
            .is_err()
    );
    assert_eq!(store.collect_garbage("term-a").await.unwrap(), 2);
    assert_eq!(
        store
            .load("racer-v4-topology-real")
            .await
            .unwrap()
            .unwrap()
            .bytes,
        second.bytes
    );
    let chunks = Api::<ConfigMap>::namespaced(client, "system")
        .list(&kube::api::ListParams::default().labels("racer.unbounded-cloud.io/rust-state=chunk"))
        .await
        .unwrap();
    assert_eq!(chunks.items.len(), 2);
    drop(processes);
}

fn seed(api: &FakeApi) {
    api.put("/apis/coordination.k8s.io/v1/namespaces/system/leases/racer-controlplane", json!({"apiVersion":"coordination.k8s.io/v1","kind":"Lease","metadata":{"name":"racer-controlplane"},"spec":{"holderIdentity":"term-a","leaseDurationSeconds":3600,"renewTime":time::OffsetDateTime::now_utc().format(&time::format_description::well_known::Rfc3339).unwrap()}}));
    use base64::Engine;
    api.put("/api/v1/namespaces/system/secrets/racer-ca", json!({"apiVersion":"v1","kind":"Secret","metadata":{"name":"racer-ca"},"data":{"state.json":base64::engine::general_purpose::STANDARD.encode(br#"{"fence":"term-a"}"#)}}));
    api.put("/apis/unbounded-cloud.io/v1alpha3/sites/site-a", json!({"apiVersion":"unbounded-cloud.io/v1alpha3","kind":"Site","metadata":{"name":"site-a","uid":"site-uid","labels":{"zone":"a"}},"spec":{"components":{"racer":{"enabled":true}}}}));
    api.put("/api/v1/nodes/node-a", json!({"apiVersion":"v1","kind":"Node","metadata":{"name":"node-a","uid":"node-uid","labels":{"unbounded-cloud.io/site":"site-a","kubernetes.io/os":"linux"}},"status":{"conditions":[{"type":"Ready","status":"True"}]}}));
    api.put("/api/v1/namespaces/system/pods/racer-a", json!({"apiVersion":"v1","kind":"Pod","metadata":{"name":"racer-a","namespace":"system","uid":"pod-uid","labels":{"racer.unbounded-cloud.io/dataplane":"true","racer.unbounded-cloud.io/component":"racer-dataplane"},"ownerReferences":[{"apiVersion":"apps/v1","kind":"DaemonSet","name":"racer-dataplane","uid":"ds-uid","controller":true}]},"spec":{"nodeName":"node-a","serviceAccountName":"racer-dataplane"},"status":{"phase":"Running","podIP":"10.0.0.1","conditions":[{"type":"Ready","status":"True"}]}}));
}

fn request(peer: &VerifiedPeer, boot: &str, cursor: &str) -> Request<Body> {
    let mut request = Request::builder()
        .uri("/v4/config")
        .header("x-racer-boot", boot)
        .header("x-racer-profile", "1")
        .header("x-racer-cursor", cursor)
        .header("x-racer-storage-policy", "1")
        .header("x-racer-applied-revision", "0")
        .header("x-racer-local-state", "failed")
        .body(Body::empty())
        .unwrap();
    request.extensions_mut().insert(peer.clone());
    request
}
async fn desired(router: Router, peer: &VerifiedPeer, boot: &str, cursor: &str) -> DesiredState {
    let response = tokio::time::timeout(Duration::from_secs(15), async {
        loop {
            let response = router
                .clone()
                .oneshot(request(peer, boot, cursor))
                .await
                .unwrap();
            if response.status() != StatusCode::NO_CONTENT {
                break response;
            }
        }
    })
    .await
    .unwrap();
    assert_eq!(response.status(), StatusCode::OK);
    DesiredState::decode(
        to_bytes(response.into_body(), 64 * 1024 * 1024)
            .await
            .unwrap(),
    )
    .unwrap()
}
async fn until(mut condition: impl FnMut() -> bool) {
    tokio::time::timeout(Duration::from_secs(15), async {
        while !condition() {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .unwrap();
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn real_watch_cas_restart_longpoll_races_and_ten_thousand_waiters() {
    let _ = tracing_subscriber::fmt()
        .with_env_filter("racer_controlplane=warn")
        .with_test_writer()
        .try_init();
    let credentials = credentials().await;
    let context = Arc::new(RwLock::new(Some(credentials.context.clone())));
    let getter: racer_controlplane::subscription::SecurityGetter = {
        let context = context.clone();
        Arc::new(move || context.read().unwrap().clone())
    };
    let api = FakeApi::new();
    seed(&api);
    api.inner.lock().unwrap().fail_after_pointer = true;
    let (client, api_task) = api.serve().await;
    let mut options = RuntimeOptions::new("system");
    options.retry_interval = Duration::from_millis(100);
    options.long_poll = Duration::from_secs(28);
    options.snapshot_cache_bytes = 1024;
    let runtime = Runtime::new(options.clone(), getter.clone());
    let stop = CancellationToken::new();
    let task = tokio::spawn(runtime.clone().run(client.clone(), stop.clone()));
    until(|| runtime.ready()).await;
    assert!(
        runtime
            .selection(
                &identity("universe", "site-a"),
                &identity("node", "node-uid")
            )
            .is_some()
    );
    let first = desired(runtime.router(), &credentials.peer, &credentials.boot, "").await;
    until(|| {
        api.inner
            .lock()
            .unwrap()
            .objects
            .keys()
            .any(|k| k.contains("racer-v4-storage-"))
    })
    .await;
    let current = desired(runtime.router(), &credentials.peer, &credentials.boot, "").await;
    assert_eq!(first.revision, current.revision);
    assert!(current.storage_policy.is_some());

    // Actual TLS plus HTTP/1.1 body framing and client cancellation.
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let server = credentials.server.clone();
    let router = runtime.router();
    let http_task = tokio::spawn(async move {
        loop {
            let (socket, _) = listener.accept().await.unwrap();
            let server = server.clone();
            let router = router.clone();
            tokio::spawn(async move {
                let (tls, peer) = server.accept(socket, true).await.unwrap();
                let router = router.layer(axum::Extension(peer.unwrap()));
                let _ = hyper_util::server::conn::auto::Builder::new(
                    hyper_util::rt::TokioExecutor::new(),
                )
                .serve_connection(
                    hyper_util::rt::TokioIo::new(tls),
                    hyper_util::service::TowerToHyperService::new(router),
                )
                .await;
            });
        }
    });
    let mut stream = tokio_rustls::TlsConnector::from(credentials.client.clone())
        .connect(
            rustls::pki_types::ServerName::try_from("racer-controlplane.system.svc").unwrap(),
            TcpStream::connect(address).await.unwrap(),
        )
        .await
        .unwrap();
    stream.write_all(format!("GET /v4/config HTTP/1.1\r\nHost: localhost\r\nContent-Length: 0\r\nX-Racer-Boot: {}\r\nX-Racer-Profile: 1\r\nX-Racer-Storage-Policy: 1\r\nX-Racer-Cursor: {}\r\n\r\n", credentials.boot, current.cursor).as_bytes()).await.unwrap();
    until(|| runtime.subscriptions.waiter_count() >= 1).await;
    assert!(
        tokio::time::timeout(Duration::from_millis(40), stream.read_u8())
            .await
            .is_err()
    );
    drop(stream);
    until(|| runtime.subscriptions.waiter_count() == 0).await;

    let mut waiters = tokio::task::JoinSet::new();
    for _ in 0..10_000 {
        let router = runtime.router();
        let req = request(&credentials.peer, &credentials.boot, &current.cursor);
        waiters.spawn(async move { router.oneshot(req).await.unwrap() });
    }
    until(|| runtime.subscriptions.waiter_count() == 10_000).await;
    let writes = api.inner.lock().unwrap().writes;
    tokio::time::sleep(Duration::from_millis(150)).await;
    assert_eq!(
        api.inner.lock().unwrap().writes,
        writes,
        "idle waiters must not persist heartbeat state"
    );
    assert!(runtime.subscriptions.cache_usage().0 <= 1024);
    let mut node = api.inner.lock().unwrap().objects["/api/v1/nodes/node-a"].clone();
    node["metadata"]["annotations"]["racer.unbounded-cloud.io/cache-size"] = json!("512Mi");
    api.put("/api/v1/nodes/node-a", node);
    let mut changed = None;
    while let Some(response) = tokio::time::timeout(Duration::from_secs(15), waiters.join_next())
        .await
        .unwrap()
    {
        let response = response.unwrap();
        assert_eq!(response.status(), StatusCode::OK);
        let next = DesiredState::decode(to_bytes(response.into_body(), 1024 * 1024).await.unwrap())
            .unwrap();
        assert_eq!(next.revision, current.revision);
        assert_ne!(next.cursor, current.cursor);
        assert_eq!(
            next.storage_policy.as_ref().unwrap().desired_bytes,
            512 << 20
        );
        changed = Some(next);
    }
    let changed = changed.unwrap();
    assert_eq!(runtime.subscriptions.waiter_count(), 0);
    // A canceled request releases its receiver; feedback failure does not release peers.
    let wait = tokio::spawn(runtime.router().oneshot(request(
        &credentials.peer,
        &credentials.boot,
        &changed.cursor,
    )));
    until(|| runtime.subscriptions.waiter_count() == 1).await;
    wait.abort();
    let _ = wait.await;
    until(|| runtime.subscriptions.waiter_count() == 0).await;

    stop.cancel();
    task.await.unwrap().unwrap();
    options.long_poll = Duration::from_millis(200);
    let restarted = Runtime::new(options, getter.clone());
    let stop2 = CancellationToken::new();
    let task2 = tokio::spawn(restarted.clone().run(client.clone(), stop2.clone()));
    until(|| restarted.ready()).await;
    let loaded = desired(restarted.router(), &credentials.peer, &credentials.boot, "").await;
    assert_eq!(loaded.revision, changed.revision);
    assert_eq!(loaded.cursor, changed.cursor);
    let unchanged = restarted
        .router()
        .oneshot(request(
            &credentials.peer,
            &credentials.boot,
            &loaded.cursor,
        ))
        .await
        .unwrap();
    assert_eq!(unchanged.status(), StatusCode::NO_CONTENT);
    assert_eq!(unchanged.headers()["content-length"], "0");

    // CAS uses the captured RV. A stale writer cannot overwrite a later pointer.
    let store = RecordStore::new(client, "system", getter);
    let name = format!("racer-v4-topology-{}", identity("universe", "site-a"));
    let old = store.load(&name).await.unwrap().unwrap();
    store
        .commit(&name, "topology", Some(&old), &old.bytes, "term-a")
        .await
        .unwrap();
    assert!(
        store
            .commit(&name, "topology", Some(&old), &old.bytes, "term-a")
            .await
            .is_err()
    );

    // Removing selection sends deconfiguration to the enrolled process without a ledger.
    let mut node = api.inner.lock().unwrap().objects["/api/v1/nodes/node-a"].clone();
    node["metadata"]["labels"]["racer.unbounded-cloud.io/exclude"] = json!("true");
    api.put("/api/v1/nodes/node-a", node);
    let removal = desired(
        restarted.router(),
        &credentials.peer,
        &credentials.boot,
        &loaded.cursor,
    )
    .await;
    let removal_cursor = removal.cursor.clone();
    let racer_controlplane::proto::configuration::Contents::Snapshot(snapshot) =
        removal.configuration.unwrap().contents.unwrap();
    assert!(!snapshot.idle && snapshot.volumes.is_empty());
    // Cache convergence is based on the exact offered snapshot plus Pod readiness.
    let mut node = api.inner.lock().unwrap().objects["/api/v1/nodes/node-a"].clone();
    node["metadata"]["labels"]["racer.unbounded-cloud.io/exclude"] = json!("false");
    api.put("/api/v1/nodes/node-a", node);
    api.put("/apis/racer.unbounded-cloud.io/v1alpha1/p2pcaches/cache-a", json!({"apiVersion":"racer.unbounded-cloud.io/v1alpha1","kind":"P2PCache","metadata":{"name":"cache-a","uid":"cache-uid","generation":1},"spec":{"cacheGeneration":1,"maxCandidateAttempts":3,"siteSelector":{"matchLabels":{"zone":"a"}}}}));
    let mut active = desired(
        restarted.router(),
        &credentials.peer,
        &credentials.boot,
        &removal_cursor,
    )
    .await;
    if active.configuration.as_ref().is_none_or(|c| matches!(c.contents.as_ref(), Some(racer_controlplane::proto::configuration::Contents::Snapshot(s)) if s.volumes.is_empty())) {
        active = desired(restarted.router(), &credentials.peer, &credentials.boot, &active.cursor).await;
    }
    let mut applied = request(&credentials.peer, &credentials.boot, &active.cursor);
    applied.headers_mut().insert(
        "x-racer-applied-revision",
        active.revision.to_string().parse().unwrap(),
    );
    applied.headers_mut().insert(
        "x-racer-applied-digest",
        hex::encode(&active.snapshot_digest).parse().unwrap(),
    );
    applied
        .headers_mut()
        .insert("x-racer-local-state", "applied".parse().unwrap());
    applied
        .headers_mut()
        .insert("x-racer-worker-healthy", "1".parse().unwrap());
    let feedback = tokio::spawn(restarted.router().oneshot(applied));
    until(|| api.inner.lock().unwrap().objects["/apis/racer.unbounded-cloud.io/v1alpha1/p2pcaches/cache-a"].pointer("/status/participants/ready") == Some(&json!(1))).await;
    feedback.abort();
    let _ = feedback.await;
    // Site switches are installation votes, not runtime membership filters.
    let mut site =
        api.inner.lock().unwrap().objects["/apis/unbounded-cloud.io/v1alpha3/sites/site-a"].clone();
    site["spec"]["components"]["racer"]["enabled"] = json!(false);
    api.put("/apis/unbounded-cloud.io/v1alpha3/sites/site-a", site);
    api.put("/apis/unbounded-cloud.io/v1alpha3/sites/site-b", json!({"apiVersion":"unbounded-cloud.io/v1alpha3","kind":"Site","metadata":{"name":"site-b","uid":"site-b-uid"},"spec":{"components":{"racer":{"enabled":false}}}}));
    let mut node = api.inner.lock().unwrap().objects["/api/v1/nodes/node-a"].clone();
    node["metadata"]["labels"]["unbounded-cloud.io/site"] = json!("site-b");
    api.put("/api/v1/nodes/node-a", node);
    let moved = desired(
        restarted.router(),
        &credentials.peer,
        &credentials.boot,
        &active.cursor,
    )
    .await;
    let racer_controlplane::proto::configuration::Contents::Snapshot(snapshot) =
        moved.configuration.unwrap().contents.unwrap();
    assert!(snapshot.volumes.is_empty());
    // The same historical Pod must never be selected into the destination universe.
    tokio::time::sleep(Duration::from_millis(300)).await;
    assert!(
        restarted
            .selection(
                &identity("universe", "site-b"),
                &identity("node", "node-uid")
            )
            .is_none()
    );
    *context.write().unwrap() = None;
    assert_eq!(
        restarted
            .router()
            .oneshot(request(&credentials.peer, &credentials.boot, ""))
            .await
            .unwrap()
            .status(),
        StatusCode::SERVICE_UNAVAILABLE
    );
    stop2.cancel();
    task2.await.unwrap().unwrap();
    http_task.abort();
    api_task.abort();
}

#[test]
fn old_new_scope_relist_and_process_bound_feedback() {
    let mut index = InventoryIndex::default();
    let mut dirty = BTreeSet::new();
    let mut storage = BTreeSet::new();
    let node: DynamicObject = serde_json::from_value(json!({"apiVersion":"v1","kind":"Node","metadata":{"name":"n","uid":"u","labels":{"unbounded-cloud.io/site":"a"}}})).unwrap();
    index
        .event(
            Kind::Node,
            Event::Apply(node.clone()),
            &mut dirty,
            &mut storage,
        )
        .unwrap();
    dirty.clear();
    let mut moved = node.clone();
    moved
        .metadata
        .labels
        .as_mut()
        .unwrap()
        .insert("unbounded-cloud.io/site".into(), "b".into());
    index
        .event(Kind::Node, Event::Init, &mut dirty, &mut storage)
        .unwrap();
    index
        .event(
            Kind::Node,
            Event::InitApply(moved),
            &mut dirty,
            &mut storage,
        )
        .unwrap();
    assert!(dirty.is_empty());
    assert!(index.universes().contains("a"));
    index
        .event(Kind::Node, Event::InitDone, &mut dirty, &mut storage)
        .unwrap();
    assert_eq!(dirty, BTreeSet::from(["a".into(), "b".into()]));
    let mut headers = http::HeaderMap::new();
    headers.insert("x-racer-storage-policy", "1".parse().unwrap());
    let now = std::time::Instant::now();
    let mut report = Observation::observe(None, "pod", "boot", &headers, now);
    let policy = racer_controlplane::storage::StoragePolicy::new(identity("node", "u"), [1; 32])
        .resolve("a", Some("512Mi"), None)
        .unwrap();
    report.offer(Some(&policy));
    for (name, value) in [
        ("x-racer-storage-identity", hex::encode([1; 32])),
        ("x-racer-storage-version", "1".into()),
        ("x-racer-storage-state", "applied".into()),
        ("x-racer-storage-applied-bytes", (512u64 << 20).to_string()),
    ] {
        headers.insert(name, value.parse().unwrap());
    }
    let accepted = Observation::observe(Some(&report), "pod", "boot", &headers, now);
    assert_eq!(accepted.applied_version, 1);
    assert_eq!(accepted.applied_bytes, 512 << 20);
    let replaced = Observation::observe(Some(&report), "pod", "new-boot", &headers, now);
    assert_eq!(replaced.applied_version, 0);
    for bytes in [32u64 << 20, 64 << 20, 96 << 20, 516 << 20] {
        headers.insert(
            "x-racer-storage-applied-bytes",
            bytes.to_string().parse().unwrap(),
        );
        let rejected = Observation::observe(Some(&report), "pod", "boot", &headers, now);
        assert_eq!(rejected.applied_version, 0, "bytes {bytes}");
        assert_eq!(rejected.storage_state, "pending");
        let retained = Observation::observe(Some(&accepted), "pod", "boot", &headers, now);
        assert_eq!(retained.applied_version, accepted.applied_version);
        assert_eq!(retained.applied_bytes, accepted.applied_bytes);
        assert_eq!(retained.storage_state, "applied");
    }
}
