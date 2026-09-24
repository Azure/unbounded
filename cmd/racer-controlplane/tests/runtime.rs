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
    kubernetes::{
        CHECKPOINT, InventoryIndex, Kind, RANGE_SIZE, RevisionStore, Runtime, RuntimeOptions,
        SecurityContext,
    },
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
    fail_after_reservation: bool,
    fail_before_reservation: bool,
    pause_reservation: Option<Arc<tokio::sync::Notify>>,
    pause_before_reservation: Option<(Arc<tokio::sync::Notify>, Arc<tokio::sync::Notify>)>,
    fail_checkpoint_reads: usize,
    reservation_requests: usize,
    reservation_completed: usize,
    fail_reservation_readback: bool,
    stall_checkpoint_get: Option<CancellationToken>,
    stall_status_patch: Option<CancellationToken>,
    stalled_requests: usize,
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
    // API writes can finish after the HTTP caller disconnects. Keep processing
    // independently so cancellation tests exercise those delayed CASes.
    tokio::spawn(fake_request_inner(api, request))
        .await
        .unwrap()
}

async fn fake_request_inner(api: FakeApi, request: Request<Body>) -> Response {
    let path = request.uri().path().to_string();
    let query = request.uri().query().unwrap_or_default().to_string();
    let method = request.method().clone();
    let stall = {
        let mut data = api.inner.lock().unwrap();
        let stall = if method == "GET" && path.ends_with(CHECKPOINT) {
            data.stall_checkpoint_get.clone()
        } else if method == "PATCH" {
            data.stall_status_patch.clone()
        } else {
            None
        };
        if stall.is_some() {
            data.stalled_requests += 1;
        }
        stall
    };
    if let Some(stall) = stall {
        stall.cancelled().await;
    }
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
        if path.ends_with(CHECKPOINT) && data.fail_checkpoint_reads > 0 {
            data.fail_checkpoint_reads -= 1;
            return api_error(StatusCode::GATEWAY_TIMEOUT);
        }
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
    let pause_before = if path.ends_with(CHECKPOINT) {
        let mut data = api.inner.lock().unwrap();
        data.reservation_requests += 1;
        data.pause_before_reservation.take()
    } else {
        None
    };
    if let Some((entered, release)) = pause_before {
        entered.notify_one();
        release.notified().await;
    }
    let (key, object, fail, pause) = {
        let mut data = api.inner.lock().unwrap();
        if path.ends_with(CHECKPOINT) {
            data.reservation_completed += 1;
        }
        if method == "DELETE" {
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
        if key.ends_with(CHECKPOINT) && data.fail_before_reservation {
            data.fail_before_reservation = false;
            return api_error(StatusCode::GATEWAY_TIMEOUT);
        }
        if object["metadata"]["uid"].is_null() {
            object["metadata"]["uid"] = json!(uuid::Uuid::new_v4().to_string());
        }
        data.revision += 1;
        data.writes += 1;
        object["metadata"]["resourceVersion"] = json!(data.revision.to_string());
        data.objects.insert(key.clone(), object.clone());
        if key.ends_with(CHECKPOINT) && data.fail_reservation_readback {
            data.fail_reservation_readback = false;
            data.fail_checkpoint_reads += 1;
            return api_error(StatusCode::GATEWAY_TIMEOUT);
        }
        let fail = data.fail_after_reservation && key.ends_with(CHECKPOINT);
        let pause = if key.ends_with(CHECKPOINT) {
            data.pause_reservation.take()
        } else {
            None
        };
        if fail {
            data.fail_after_reservation = false;
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

const CHECKPOINT_PATH: &str = "/api/v1/namespaces/system/configmaps/racer-runtime-revisions";

async fn gate_fixture() -> (FakeApi, RevisionStore, tokio::task::JoinHandle<()>) {
    let context = credentials().await.context;
    let api = FakeApi::new();
    seed(&api, &context.state);
    let (client, task) = api.serve().await;
    let store = RevisionStore::new(client, "system", Arc::new(move || Some(context.clone())));
    (api, store, task)
}

fn high_water(api: &FakeApi) -> u64 {
    api.inner.lock().unwrap().objects[CHECKPOINT_PATH]["data"]["high-water"]
        .as_str()
        .unwrap()
        .parse()
        .unwrap()
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn gate_release_recovers_outage_and_resource_version_conflict() {
    let (api, store, task) = gate_fixture().await;
    api.inner.lock().unwrap().fail_before_reservation = true;
    assert!(store.reserve("term-a").await.is_err());
    let entered = Arc::new(tokio::sync::Notify::new());
    let release = Arc::new(tokio::sync::Notify::new());
    api.inner.lock().unwrap().pause_before_reservation = Some((entered.clone(), release.clone()));
    let pending = {
        let store = store.clone();
        tokio::spawn(async move { store.reserve("term-a").await })
    };
    entered.notified().await;
    let mut object = api.inner.lock().unwrap().objects[CHECKPOINT_PATH].clone();
    object["metadata"]["annotations"] = json!({"external-update":"1"});
    api.put(CHECKPOINT_PATH, object);
    release.notify_one();
    assert_eq!(pending.await.unwrap().unwrap().take().unwrap(), 1);
    api.inner.lock().unwrap().fail_after_reservation = true;
    assert_eq!(
        store.reserve("term-a").await.unwrap().take().unwrap(),
        RANGE_SIZE + 1
    );
    store.verify("term-a").await.unwrap();
    task.abort();
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn gate_release_retry_exhaustion_is_recoverable_on_next_call() {
    let (api, store, task) = gate_fixture().await;
    api.inner.lock().unwrap().fail_reservation_readback = true;
    assert!(store.reserve("term-a").await.is_err());
    assert!(store.verify("term-a").await.is_err());
    let writes = api.inner.lock().unwrap().writes;
    tokio::time::sleep(Duration::from_millis(150)).await;
    assert_eq!(
        api.inner.lock().unwrap().writes,
        writes,
        "no orphan cleanup writer"
    );
    assert_eq!(
        store.reserve("term-a").await.unwrap().take().unwrap(),
        RANGE_SIZE + 1
    );
    task.abort();
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn gate_canceled_acquisition_fences_delayed_create_and_replace() {
    let (api, store, task) = gate_fixture().await;
    // The first reservation and subsequent replacements both use CAS against
    // the bootstrap checkpoint. Runtime never creates a missing checkpoint.
    for _ in [false, true] {
        let entered = Arc::new(tokio::sync::Notify::new());
        let release = Arc::new(tokio::sync::Notify::new());
        let completed = api.inner.lock().unwrap().reservation_completed;
        api.inner.lock().unwrap().pause_before_reservation =
            Some((entered.clone(), release.clone()));
        let acquiring = {
            let store = store.clone();
            tokio::spawn(async move { store.reserve("term-a").await })
        };
        entered.notified().await;
        acquiring.abort();
        assert!(acquiring.await.is_err_and(|error| error.is_cancelled()));
        assert!(store.verify("term-a").await.is_err());
        store.reserve("term-a").await.unwrap();
        let successor = api.inner.lock().unwrap().objects[CHECKPOINT_PATH].clone();
        release.notify_one();
        until(|| api.inner.lock().unwrap().reservation_completed > completed + 1).await;
        assert_eq!(
            api.inner.lock().unwrap().objects[CHECKPOINT_PATH],
            successor
        );
        store.verify("term-a").await.unwrap();
    }
    task.abort();
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn gate_canceled_persisted_acquisition_and_failed_readback_recover() {
    let (api, store, task) = gate_fixture().await;
    for _ in 0..2 {
        let pause = Arc::new(tokio::sync::Notify::new());
        api.inner.lock().unwrap().pause_reservation = Some(pause.clone());
        let acquiring = {
            let store = store.clone();
            tokio::spawn(async move { store.reserve("term-a").await })
        };
        until(|| api.inner.lock().unwrap().pause_reservation.is_none()).await;
        let burned = high_water(&api);
        acquiring.abort();
        assert!(acquiring.await.is_err_and(|error| error.is_cancelled()));
        assert!(store.verify("term-a").await.is_err());
        assert_eq!(
            store.reserve("term-a").await.unwrap().take().unwrap(),
            burned + 1
        );
        pause.notify_one();
    }
    api.inner.lock().unwrap().fail_reservation_readback = true;
    assert!(store.reserve("term-a").await.is_err());
    let burned = high_water(&api);
    assert_eq!(
        store.reserve("term-a").await.unwrap().take().unwrap(),
        burned + 1
    );
    task.abort();
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn gate_release_never_unlocks_successor_operation_or_fence() {
    for change_fence in [false, true] {
        let (api, store, task) = gate_fixture().await;
        let pause = Arc::new(tokio::sync::Notify::new());
        api.inner.lock().unwrap().pause_reservation = Some(pause.clone());
        let pending = {
            let store = store.clone();
            tokio::spawn(async move { store.reserve("term-a").await })
        };
        until(|| api.inner.lock().unwrap().pause_reservation.is_none()).await;
        let mut successor = api.inner.lock().unwrap().objects[CHECKPOINT_PATH].clone();
        successor["data"]["fence"] = json!(if change_fence {
            "term-b/successor"
        } else {
            "term-a/successor"
        });
        successor["data"]["high-water"] = json!((2 * RANGE_SIZE).to_string());
        api.put(CHECKPOINT_PATH, successor);
        let successor = api.inner.lock().unwrap().objects[CHECKPOINT_PATH].clone();
        pause.notify_one();
        assert!(pending.await.unwrap().is_err());
        assert!(store.verify("term-a").await.is_err());
        assert_eq!(
            api.inner.lock().unwrap().objects[CHECKPOINT_PATH],
            successor
        );
        assert_eq!(api.inner.lock().unwrap().reservation_requests, 1);
        task.abort();
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn gate_canceled_holder_recovers_without_overlapping_live_critical_sections() {
    let (api, store, task) = gate_fixture().await;
    let pause = Arc::new(tokio::sync::Notify::new());
    api.inner.lock().unwrap().pause_reservation = Some(pause.clone());
    let writer = {
        let store = store.clone();
        tokio::spawn(async move { store.reserve("term-a").await })
    };
    until(|| api.inner.lock().unwrap().pause_reservation.is_none()).await;
    let acquisitions = api.inner.lock().unwrap().reservation_requests;
    assert!(
        tokio::time::timeout(Duration::from_millis(150), store.reserve("term-a"))
            .await
            .is_err()
    );
    assert_eq!(api.inner.lock().unwrap().reservation_requests, acquisitions);
    writer.abort();
    assert!(writer.await.is_err_and(|error| error.is_cancelled()));
    assert_eq!(
        tokio::time::timeout(Duration::from_secs(5), store.reserve("term-a"))
            .await
            .unwrap()
            .unwrap()
            .take()
            .unwrap(),
        RANGE_SIZE + 1
    );
    pause.notify_one();
    store.verify("term-a").await.unwrap();
    task.abort();
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
    seed(&api, &credentials.context.state);
    let (client, task) = api.serve().await;
    let store = RevisionStore::new(client.clone(), "system", getter.clone());
    api.inner.lock().unwrap().fail_after_reservation = true;
    let mut first = store.reserve("term-a").await.unwrap();
    assert_eq!(
        first.take().unwrap(),
        1,
        "committed unknown outcome recovered by readback"
    );
    api.inner.lock().unwrap().fail_before_reservation = true;
    assert!(store.reserve("term-a").await.is_err());
    let mut second = store.reserve("term-a").await.unwrap();
    assert_eq!(second.take().unwrap(), RANGE_SIZE + 1);
    for _ in 1..RANGE_SIZE {
        second.take().unwrap();
    }
    assert!(second.exhausted());
    assert!(second.take().is_err());
    let pause = Arc::new(tokio::sync::Notify::new());
    api.inner.lock().unwrap().pause_reservation = Some(pause.clone());
    let writer = {
        let store = store.clone();
        tokio::spawn(async move { store.reserve("term-a").await })
    };
    until(|| {
        api.inner.lock().unwrap().objects
            [&format!("/api/v1/namespaces/system/configmaps/{CHECKPOINT}")]["data"]["high-water"]
            == json!((3 * RANGE_SIZE).to_string())
    })
    .await;
    // A new leader fences the delayed reservation response. No payload or
    // historical object survives, and the successor reserves above its numbers.
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
        state: Arc::new(state.clone()),
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
        json!(base64::engine::general_purpose::STANDARD.encode(state.to_image().unwrap().metadata));
    api.put("/api/v1/namespaces/system/secrets/racer-ca", secret);
    let successor = RevisionStore::new(client.clone(), "system", getter.clone());
    let mut current = successor.reserve("term-b").await.unwrap();
    assert_eq!(current.take().unwrap(), 3 * RANGE_SIZE + 1);
    pause.notify_one();
    assert!(writer.await.unwrap().is_err());
    assert!(store.reserve("term-a").await.is_err());

    // Delay a request before CAS, let another process reserve, then resume. The
    // stale RV conflicts and retry must use a disjoint, newly read range.
    let entered = Arc::new(tokio::sync::Notify::new());
    let release = Arc::new(tokio::sync::Notify::new());
    api.inner.lock().unwrap().pause_before_reservation = Some((entered.clone(), release.clone()));
    let contender = RevisionStore::new(client.clone(), "system", getter.clone());
    let pending = tokio::spawn(async move { contender.reserve("term-b").await });
    entered.notified().await;
    let mut winner = successor.reserve("term-b").await.unwrap();
    let winner_start = winner.take().unwrap();
    release.notify_one();
    let mut retried = pending.await.unwrap().unwrap();
    assert!(retried.take().unwrap() >= winner_start + RANGE_SIZE);
    assert!(successor.verify("term-b").await.is_err());

    // A persisted reservation with unreadable outcome cannot expose numbers.
    let pause = Arc::new(tokio::sync::Notify::new());
    api.inner.lock().unwrap().pause_reservation = Some(pause.clone());
    let uncertain = RevisionStore::new(client.clone(), "system", getter.clone());
    let unresolved = {
        let uncertain = uncertain.clone();
        tokio::spawn(async move { uncertain.reserve("term-b").await })
    };
    until(|| api.inner.lock().unwrap().pause_reservation.is_none()).await;
    api.inner.lock().unwrap().fail_checkpoint_reads = 1;
    pause.notify_one();
    assert!(unresolved.await.unwrap().is_err());
    assert!(uncertain.verify("term-b").await.is_err());
    for _ in 0..140 {
        successor.reserve("term-b").await.unwrap();
    }
    let data = api.inner.lock().unwrap();
    assert_eq!(
        data.objects
            .keys()
            .filter(|k| k.contains("/configmaps/"))
            .count(),
        1
    );
    let checkpoint =
        data.objects[&format!("/api/v1/namespaces/system/configmaps/{CHECKPOINT}")].clone();
    assert_eq!(checkpoint["data"].as_object().unwrap().len(), 3);
    assert!(checkpoint["data"].to_string().len() < 512);
    drop(data);
    let path = format!("/api/v1/namespaces/system/configmaps/{CHECKPOINT}");
    api.inner.lock().unwrap().objects.remove(&path);
    assert!(successor.reserve("term-b").await.is_err());
    let restarted = RevisionStore::new(client, "system", getter);
    assert!(
        restarted.reserve("term-b").await.is_err(),
        "restart must not recreate missing checkpoint"
    );
    let mut exhausted = checkpoint;
    exhausted["data"]["high-water"] = json!(u64::MAX.to_string());
    api.put(&path, exhausted);
    assert!(
        restarted
            .reserve("term-b")
            .await
            .unwrap_err()
            .to_string()
            .contains("exhausted")
    );
    let mut corrupted = api.inner.lock().unwrap().objects[&path].clone();
    corrupted["data"]["unexpected"] = json!("not-fixed-schema");
    api.put(&path, corrupted);
    assert!(restarted.reserve("term-b").await.is_err());
    let mut replaced = api.inner.lock().unwrap().objects[&path].clone();
    replaced["data"]
        .as_object_mut()
        .unwrap()
        .remove("unexpected");
    replaced["metadata"]["uid"] = json!("replacement-checkpoint");
    api.put(&path, replaced);
    assert!(
        successor
            .reserve("term-b")
            .await
            .unwrap_err()
            .to_string()
            .contains("replaced")
    );
    task.abort();
}

/// Revision checkpoint real API seam; the separate binary harness owns full service
/// startup. Opt in with assets and keep all API files under the crate target.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn record_store_real_kubernetes_api() {
    let Ok(assets) = std::env::var("KUBEBUILDER_ASSETS") else {
        eprintln!("KUBEBUILDER_ASSETS unavailable; real revision API scenario not run");
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
    let context = credentials().await.context;
    seed(&fixture, &context.state);
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
    let shared = Arc::new(RwLock::new(context.clone()));
    let getter: racer_controlplane::subscription::SecurityGetter = {
        let shared = shared.clone();
        Arc::new(move || Some(shared.read().unwrap().clone()))
    };
    RevisionStore::initialize(client.clone(), "system")
        .await
        .unwrap();
    let store = RevisionStore::new(client.clone(), "system", getter.clone());
    let mut first = store.reserve("term-a").await.unwrap();
    assert_eq!(first.take().unwrap(), 1);
    let maps = Api::<ConfigMap>::namespaced(client.clone(), "system");
    let stale = maps.get(CHECKPOINT).await.unwrap();
    let restarted = RevisionStore::new(client.clone(), "system", getter);
    let mut second = restarted.reserve("term-a").await.unwrap();
    assert_eq!(second.take().unwrap(), RANGE_SIZE + 1);
    assert!(
        maps.replace(CHECKPOINT, &PostParams::default(), &stale)
            .await
            .is_err()
    );
    assert!(store.verify("term-a").await.is_err());
    for _ in 0..16 {
        restarted.reserve("term-a").await.unwrap();
    }
    let checkpoints = maps.list(&kube::api::ListParams::default()).await.unwrap();
    assert_eq!(checkpoints.items.len(), 1);
    assert!(
        serde_json::to_vec(&checkpoints.items[0].data)
            .unwrap()
            .len()
            < 512
    );
    let leases = Api::<Lease>::namespaced(client.clone(), "system");
    let mut lease = leases.get("racer-controlplane").await.unwrap();
    lease.spec.as_mut().unwrap().holder_identity = Some("term-b".into());
    leases
        .replace("racer-controlplane", &PostParams::default(), &lease)
        .await
        .unwrap();
    assert!(
        restarted.reserve("term-a").await.is_err(),
        "stale local context cannot override live lease"
    );
    let mut image = context.state.to_image().unwrap();
    let mut metadata: Value = serde_json::from_slice(&image.metadata).unwrap();
    metadata["fence"] = json!("term-b");
    image.metadata = serde_json::to_vec(&metadata).unwrap();
    let state = CaState::from_image(&image).unwrap();
    let secrets = Api::<Secret>::namespaced(client.clone(), "system");
    let mut secret = secrets.get("racer-ca").await.unwrap();
    secret
        .data
        .as_mut()
        .unwrap()
        .insert("state.json".into(), k8s_openapi::ByteString(image.metadata));
    secrets
        .replace("racer-ca", &PostParams::default(), &secret)
        .await
        .unwrap();
    *shared.write().unwrap() = SecurityContext {
        fence: "term-b".into(),
        state: Arc::new(state),
    };
    let mut takeover = restarted.reserve("term-b").await.unwrap();
    assert!(takeover.take().unwrap() > second.take().unwrap());
    assert_eq!(
        maps.list(&kube::api::ListParams::default())
            .await
            .unwrap()
            .items
            .len(),
        1
    );
    maps.delete(CHECKPOINT, &kube::api::DeleteParams::default())
        .await
        .unwrap();
    assert!(restarted.reserve("term-b").await.is_err());
    drop(processes);
}

fn seed(api: &FakeApi, state: &CaState) {
    api.put(&format!("/api/v1/namespaces/system/configmaps/{CHECKPOINT}"), json!({"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":CHECKPOINT,"uid":"checkpoint-uid"},"data":{"format":"1","fence":"","high-water":"0"}}));
    api.put("/apis/coordination.k8s.io/v1/namespaces/system/leases/racer-controlplane", json!({"apiVersion":"coordination.k8s.io/v1","kind":"Lease","metadata":{"name":"racer-controlplane"},"spec":{"holderIdentity":"term-a","leaseDurationSeconds":3600,"renewTime":time::OffsetDateTime::now_utc().format(&time::format_description::well_known::Rfc3339).unwrap()}}));
    use base64::Engine;
    api.put("/api/v1/namespaces/system/secrets/racer-ca", json!({"apiVersion":"v1","kind":"Secret","metadata":{"name":"racer-ca"},"data":{"state.json":base64::engine::general_purpose::STANDARD.encode(state.to_image().unwrap().metadata)}}));
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
    seed(&api, &credentials.context.state);
    api.inner.lock().unwrap().fail_after_reservation = true;
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
    // Observe in-memory policy publication through the actual handler rather
    // than treating checkpoint or status writes as proof that it is installed.
    let current = tokio::time::timeout(Duration::from_secs(15), async {
        loop {
            let current = desired(runtime.router(), &credentials.peer, &credentials.boot, "").await;
            if current.storage_policy.is_some() {
                break current;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .expect("storage policy was not installed in the config handler");
    until(|| {
        api.inner
            .lock()
            .unwrap()
            .objects
            .get("/api/v1/nodes/node-a")
            .is_some_and(|n| {
                n.pointer("/metadata/annotations/racer.unbounded-cloud.io~1cache-status")
                    .is_some()
            })
    })
    .await;
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
    assert!(loaded.revision > changed.revision);
    assert_ne!(loaded.cursor, changed.cursor);
    assert_eq!(
        loaded.storage_policy.as_ref().unwrap().identity,
        changed.storage_policy.as_ref().unwrap().identity
    );
    assert!(
        loaded.storage_policy.as_ref().unwrap().version
            > changed.storage_policy.as_ref().unwrap().version
    );
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

    assert_eq!(
        api.inner
            .lock()
            .unwrap()
            .objects
            .keys()
            .filter(|k| k.contains("/configmaps/"))
            .count(),
        1
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
    // Live inventory selects the Pod in its new universe. Its old signed
    // identity still only authorizes deconfiguration in the old universe.
    tokio::time::sleep(Duration::from_millis(300)).await;
    assert!(
        restarted
            .selection(
                &identity("universe", "site-b"),
                &identity("node", "node-uid")
            )
            .is_some()
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

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn invalid_inventory_retains_only_running_memory_and_restart_withholds() {
    let credentials = credentials().await;
    let security = credentials.context.clone();
    let getter: racer_controlplane::subscription::SecurityGetter =
        Arc::new(move || Some(security.clone()));
    let api = FakeApi::new();
    seed(&api, &credentials.context.state);
    let (client, api_task) = api.serve().await;
    let mut options = RuntimeOptions::new("system");
    options.retry_interval = Duration::from_millis(50);
    options.long_poll = Duration::from_millis(50);
    let runtime = Runtime::new(options.clone(), getter.clone());
    let stop = CancellationToken::new();
    let task = tokio::spawn(runtime.clone().run(client.clone(), stop.clone()));
    until(|| runtime.ready()).await;
    let good = desired(runtime.router(), &credentials.peer, &credentials.boot, "").await;
    let good_policy = good.storage_policy.clone().unwrap();
    assert_eq!(
        good_policy.identity,
        racer_controlplane::model::identity_bytes("storage", "node-uid")
    );
    assert_ne!(
        good_policy.identity,
        racer_controlplane::storage::StoragePolicy::for_node("replacement-uid").identity
    );

    let mut wrong_boot = request(&credentials.peer, &"e".repeat(64), "");
    wrong_boot
        .headers_mut()
        .insert("x-racer-boot", "e".repeat(64).parse().unwrap());
    assert_eq!(
        runtime.router().oneshot(wrong_boot).await.unwrap().status(),
        StatusCode::FORBIDDEN
    );

    let mut node = api.inner.lock().unwrap().objects["/api/v1/nodes/node-a"].clone();
    node["metadata"]["annotations"]["racer.unbounded-cloud.io/cache-size"] = json!("bad");
    api.put("/api/v1/nodes/node-a", node);
    let invalid = desired(
        runtime.router(),
        &credentials.peer,
        &credentials.boot,
        &good.cursor,
    )
    .await;
    assert_eq!(invalid.revision, good.revision);
    assert!(
        invalid.storage_policy.is_none(),
        "invalid intent must omit replacement policy"
    );
    until(|| {
        api.inner.lock().unwrap().objects["/api/v1/nodes/node-a"]
            .pointer("/metadata/annotations/racer.unbounded-cloud.io~1cache-status")
            .and_then(Value::as_str)
            .and_then(|s| serde_json::from_str::<Value>(s).ok())
            .is_some_and(|s| {
                s["phase"] == "invalid" && s["effectiveBytes"] == good_policy.desired_bytes
            })
    })
    .await;

    api.put("/apis/racer.unbounded-cloud.io/v1alpha1/p2pcaches/bad", json!({"apiVersion":"racer.unbounded-cloud.io/v1alpha1","kind":"P2PCache","metadata":{"name":"bad","uid":"bad-uid"},"spec":{"cacheGeneration":-1}}));
    tokio::time::sleep(Duration::from_millis(150)).await;
    let retained = desired(runtime.router(), &credentials.peer, &credentials.boot, "").await;
    assert_eq!(retained.revision, good.revision);
    assert_eq!(retained.snapshot_digest, good.snapshot_digest);
    let mut malformed = api.inner.lock().unwrap().objects["/api/v1/nodes/node-a"].clone();
    malformed["metadata"]["annotations"]["racer.unbounded-cloud.io/fabric"] =
        json!("invalid fabric");
    api.put("/api/v1/nodes/node-a", malformed);
    tokio::time::sleep(Duration::from_millis(150)).await;
    assert!(
        runtime
            .selection(
                &identity("universe", "site-a"),
                &identity("node", "node-uid")
            )
            .is_some()
    );
    let retained = desired(runtime.router(), &credentials.peer, &credentials.boot, "").await;
    assert_eq!(retained.snapshot_digest, good.snapshot_digest);
    let mut replaced = api.inner.lock().unwrap().objects["/api/v1/nodes/node-a"].clone();
    replaced["metadata"]["uid"] = json!("replacement-uid");
    api.put("/api/v1/nodes/node-a", replaced.clone());
    until(|| {
        runtime
            .selection(
                &identity("universe", "site-a"),
                &identity("node", "node-uid"),
            )
            .is_none()
    })
    .await;
    assert_eq!(
        runtime
            .router()
            .oneshot(request(&credentials.peer, &credentials.boot, ""))
            .await
            .unwrap()
            .status(),
        StatusCode::FORBIDDEN,
        "last-good topology cannot authorize a stale Node UID binding"
    );
    replaced["metadata"]["uid"] = json!("node-uid");
    replaced["metadata"]["annotations"]
        .as_object_mut()
        .unwrap()
        .remove("racer.unbounded-cloud.io/fabric");
    api.put("/api/v1/nodes/node-a", replaced);
    stop.cancel();
    task.await.unwrap().unwrap();

    let restarted = Runtime::new(options, getter);
    let stop = CancellationToken::new();
    let task = tokio::spawn(restarted.clone().run(client, stop.clone()));
    until(|| {
        api.inner.lock().unwrap().objects
            [&format!("/api/v1/namespaces/system/configmaps/{CHECKPOINT}")]["data"]["high-water"]
            == json!((2 * RANGE_SIZE).to_string())
    })
    .await;
    tokio::time::sleep(Duration::from_millis(100)).await;
    assert!(!restarted.ready());
    assert_eq!(
        restarted
            .router()
            .oneshot(request(&credentials.peer, &credentials.boot, ""))
            .await
            .unwrap()
            .status(),
        StatusCode::SERVICE_UNAVAILABLE
    );
    let mut cache =
        api.inner.lock().unwrap().objects["/apis/racer.unbounded-cloud.io/v1alpha1/p2pcaches/bad"]
            .clone();
    cache["spec"]["cacheGeneration"] = json!(1);
    api.put(
        "/apis/racer.unbounded-cloud.io/v1alpha1/p2pcaches/bad",
        cache,
    );
    until(|| restarted.ready()).await;
    let fresh = desired(restarted.router(), &credentials.peer, &credentials.boot, "").await;
    assert!(fresh.revision > good.revision);
    assert!(fresh.storage_policy.is_none());
    until(|| {
        api.inner.lock().unwrap().objects["/api/v1/nodes/node-a"]
            .pointer("/metadata/annotations/racer.unbounded-cloud.io~1cache-status")
            .and_then(Value::as_str)
            .and_then(|s| serde_json::from_str::<Value>(s).ok())
            .is_some_and(|s| s["phase"] == "invalid" && s["effectiveBytes"] == 0)
    })
    .await;

    let mut node = api.inner.lock().unwrap().objects["/api/v1/nodes/node-a"].clone();
    node["metadata"]["annotations"]["racer.unbounded-cloud.io/cache-size"] = json!("512Mi");
    api.put("/api/v1/nodes/node-a", node);
    let recovered = desired(
        restarted.router(),
        &credentials.peer,
        &credentials.boot,
        &fresh.cursor,
    )
    .await;
    let policy = recovered.storage_policy.unwrap();
    assert_eq!(policy.identity, good_policy.identity);
    assert!(policy.version > good_policy.version);
    assert_eq!(policy.desired_bytes, 512 << 20);

    api.inner.lock().unwrap().objects.remove(&format!(
        "/api/v1/namespaces/system/configmaps/{CHECKPOINT}"
    ));
    until(|| !restarted.ready()).await;
    assert_eq!(
        restarted
            .router()
            .oneshot(request(&credentials.peer, &credentials.boot, ""))
            .await
            .unwrap()
            .status(),
        StatusCode::SERVICE_UNAVAILABLE
    );
    stop.cancel();
    task.await.unwrap().unwrap();
    api_task.abort();
}

#[test]
fn in_memory_publication_rejects_regression_and_preserves_last_good() {
    use racer_controlplane::{publication::Publication, storage::StoragePolicy};
    let desired = StoragePolicy::for_node("uid")
        .resolve("site-a", Some("1Gi"), None)
        .unwrap();
    let mut publication = Publication::default();
    let first = publication.publish(desired.clone(), 10).unwrap();
    assert!(publication.publish(desired.clone(), 10).is_err());
    assert!(publication.publish(desired.clone(), 9).is_err());
    let mut invalid = desired.clone();
    invalid.desired_bytes = 1;
    assert!(publication.publish(invalid, 11).is_err());
    assert_eq!(publication.published().unwrap(), &first);
    let replacement = StoragePolicy::for_node("replacement")
        .resolve("site-a", Some("1Gi"), None)
        .unwrap();
    assert!(publication.publish(replacement, 11).is_err());
    let mut restarted = Publication::default();
    assert!(restarted.published().is_none());
    let fresh = restarted.publish(desired, RANGE_SIZE + 1).unwrap();
    assert!(fresh.revision > first.revision);
    assert_eq!(fresh.identity, first.identity);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn authority_expires_during_checkpoint_or_status_stalls_with_healthy_lease() {
    for checkpoint in [true, false] {
        let credentials = credentials().await;
        let context = credentials.context.clone();
        let api = FakeApi::new();
        seed(&api, &context.state);
        let (client, api_task) = api.serve().await;
        let mut options = RuntimeOptions::new("system");
        options.retry_interval = Duration::from_millis(30);
        options.authority_timeout = Duration::from_millis(600);
        options.long_poll = Duration::from_secs(10);
        let runtime = Runtime::new(options, Arc::new(move || Some(context.clone())));
        let stop = CancellationToken::new();
        let task = tokio::spawn(runtime.clone().run(client.clone(), stop.clone()));
        until(|| runtime.ready()).await;
        let current = desired(runtime.router(), &credentials.peer, &credentials.boot, "").await;
        let waiter = tokio::spawn(runtime.router().oneshot(request(
            &credentials.peer,
            &credentials.boot,
            &current.cursor,
        )));
        until(|| runtime.subscriptions.waiter_count() == 1).await;
        let release = CancellationToken::new();
        if checkpoint {
            api.inner.lock().unwrap().stall_checkpoint_get = Some(release.clone());
        } else {
            api.inner.lock().unwrap().stall_status_patch = Some(release.clone());
            // Change only controller status, forcing a PATCH without publishing
            // different content or waking the unchanged subscription.
            let mut node = api.inner.lock().unwrap().objects["/api/v1/nodes/node-a"].clone();
            node["metadata"]["annotations"]["racer.unbounded-cloud.io/cache-status"] = json!("{}");
            api.put("/api/v1/nodes/node-a", node);
        }
        until(|| api.inner.lock().unwrap().stalled_requests > 0).await;
        let ca = racer_controlplane::security::kubernetes::KubernetesCaStore::new(
            client,
            "system".into(),
            Leadership::new("term-a".into()).unwrap(),
        );
        ca.check_fence().await.unwrap();
        tokio::time::timeout(Duration::from_secs(3), until(|| !runtime.ready()))
            .await
            .unwrap();
        assert_eq!(
            runtime
                .router()
                .oneshot(request(&credentials.peer, &credentials.boot, ""))
                .await
                .unwrap()
                .status(),
            StatusCode::SERVICE_UNAVAILABLE
        );
        assert_eq!(
            tokio::time::timeout(Duration::from_secs(2), waiter)
                .await
                .unwrap()
                .unwrap()
                .unwrap()
                .status(),
            StatusCode::SERVICE_UNAVAILABLE
        );
        assert!(!task.is_finished(), "the reconcile I/O is still stalled");
        {
            let mut data = api.inner.lock().unwrap();
            data.stall_checkpoint_get = None;
            data.stall_status_patch = None;
        }
        release.cancel();
        until(|| runtime.ready()).await;
        stop.cancel();
        task.await.unwrap().unwrap();
        api_task.abort();
    }
}

#[test]
fn live_selection_ignores_invalid_fabric_and_isolates_node_identity_changes() {
    let api = FakeApi::new();
    // Populate the membership inventory directly, without certificate state.
    let mut index = InventoryIndex::default();
    let mut dirty = BTreeSet::new();
    let mut storage = BTreeSet::new();
    let site = json!({"apiVersion":"unbounded-cloud.io/v1alpha3","kind":"Site","metadata":{"name":"a","uid":"site"}});
    index
        .event(
            Kind::Site,
            Event::Apply(serde_json::from_value(site).unwrap()),
            &mut dirty,
            &mut storage,
        )
        .unwrap();
    for i in 0..2 {
        let node = json!({"apiVersion":"v1","kind":"Node","metadata":{"name":format!("n{i}"),"uid":format!("uid{i}"),"labels":{"unbounded-cloud.io/site":"a","kubernetes.io/os":"linux"},"annotations":{"racer.unbounded-cloud.io/fabric":"invalid fabric"}},"status":{"conditions":[{"type":"Ready","status":"True"}]}});
        let pod = json!({"apiVersion":"v1","kind":"Pod","metadata":{"name":format!("p{i}"),"namespace":"system","uid":format!("pod{i}"),"labels":{"racer.unbounded-cloud.io/dataplane":"true","racer.unbounded-cloud.io/component":"racer-dataplane"},"ownerReferences":[{"apiVersion":"apps/v1","kind":"DaemonSet","name":"racer-dataplane","uid":"ds","controller":true}]},"spec":{"nodeName":format!("n{i}"),"serviceAccountName":"racer-dataplane"},"status":{"phase":"Running","podIP":format!("10.0.0.{}",i+1)}});
        api.put(&format!("/nodes/n{i}"), node.clone());
        index
            .event(
                Kind::Node,
                Event::Apply(serde_json::from_value(node).unwrap()),
                &mut dirty,
                &mut storage,
            )
            .unwrap();
        index
            .event(
                Kind::Pod,
                Event::Apply(serde_json::from_value(pod).unwrap()),
                &mut dirty,
                &mut storage,
            )
            .unwrap();
    }
    let original = index.live_selections();
    assert_eq!(original.len(), 2);
    assert!(
        racer_controlplane::topology::compile(&index.inventory("a", "/run/racer").unwrap(), None)
            .is_err()
    );
    let mut replaced = api.inner.lock().unwrap().objects["/nodes/n0"].clone();
    replaced["metadata"]["uid"] = json!("replacement");
    index
        .event(
            Kind::Node,
            Event::Apply(serde_json::from_value(replaced).unwrap()),
            &mut dirty,
            &mut storage,
        )
        .unwrap();
    let next = index.live_selections();
    assert!(!next.contains_key(&(identity("universe", "a"), identity("node", "uid0"))));
    let healthy = (identity("universe", "a"), identity("node", "uid1"));
    assert_eq!(next[&healthy], original[&healthy]);
}
