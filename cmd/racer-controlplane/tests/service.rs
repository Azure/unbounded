// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Tests use a real HTTP Kubernetes API fixture and the production kube client,
//! service election, native TLS listeners, enrollment and v4 handler. The fixture
//! models resourceVersion CAS and watch events; it is not a real API server.
use anyhow::{Context, Result, ensure};
use axum::{
    Router,
    body::Body,
    extract::State,
    http::{Method, Request, StatusCode},
    response::{IntoResponse, Response},
};
use futures::stream;
use kube::Client;
use racer_controlplane::{
    security::{
        kubernetes::{KubernetesCaStore, LEASE},
        *,
    },
    service::{self, LeaseTiming, Options},
};
use serde_json::{Value, json};
use std::{
    collections::BTreeMap,
    convert::Infallible,
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::{TcpListener, TcpStream},
    sync::broadcast,
};
use tokio_util::sync::CancellationToken;

#[derive(Default)]
struct Objects {
    values: BTreeMap<String, Value>,
    revision: u64,
    uncertain_secret: bool,
    reviews: usize,
    review_delay: Duration,
    immutable_metadata_updates: usize,
}
#[derive(Clone)]
struct Fixture {
    objects: Arc<Mutex<Objects>>,
    events: broadcast::Sender<(String, Value)>,
    pod_list_pause: Arc<Mutex<Option<Arc<ListPause>>>>,
}

#[derive(Default)]
struct ListPause {
    captured: tokio::sync::Notify,
    resume: tokio::sync::Notify,
}

fn collection(path: &str) -> &str {
    path.rsplit_once('/').map(|p| p.0).unwrap_or(path)
}
fn list_kind(path: &str) -> &str {
    match path.rsplit('/').next().unwrap_or("") {
        "pods" => "PodList",
        "nodes" => "NodeList",
        "configmaps" => "ConfigMapList",
        "secrets" => "SecretList",
        "leases" => "LeaseList",
        "sites" => "SiteList",
        "racercaches" => "RacerCacheList",
        "p2pcaches" => "P2PCacheList",
        "caches" => "RacerCacheList",
        _ => "List",
    }
}
fn status(code: StatusCode, reason: &str) -> Response {
    (code, axum::Json(json!({"apiVersion":"v1","kind":"Status","status":"Failure","reason":reason,"message":reason,"code":code.as_u16()}))).into_response()
}
fn merge(target: &mut Value, patch: Value) {
    if let Value::Object(map) = patch {
        if !target.is_object() {
            *target = json!({});
        }
        for (key, value) in map {
            if value.is_null() {
                target.as_object_mut().unwrap().remove(&key);
            } else {
                merge(
                    target
                        .as_object_mut()
                        .unwrap()
                        .entry(key)
                        .or_insert(Value::Null),
                    value,
                );
            }
        }
    } else {
        *target = patch;
    }
}

async fn api(State(fixture): State<Fixture>, request: Request<Body>) -> Response {
    let path = request.uri().path().to_owned();
    let query = request.uri().query().unwrap_or("").to_owned();
    let method = request.method().clone();
    if method == Method::GET && path == "/api/v1/namespaces/system/pods" && query.is_empty() {
        let pause = fixture.pod_list_pause.lock().unwrap().take();
        if let Some(pause) = pause {
            let response = {
                let objects = fixture.objects.lock().unwrap();
                let items: Vec<_> = objects
                    .values
                    .iter()
                    .filter(|(key, _)| collection(key) == path)
                    .map(|(_, value)| value.clone())
                    .collect();
                json!({"apiVersion":"v1","kind":"PodList","metadata":{"resourceVersion":objects.revision.to_string()},"items":items})
            };
            pause.captured.notify_one();
            pause.resume.notified().await;
            return axum::Json(response).into_response();
        }
    }
    if path.ends_with("/tokenreviews") && method == Method::POST {
        let delay = fixture.objects.lock().unwrap().review_delay;
        tokio::time::sleep(delay).await;
    }
    if method == Method::GET && query.split('&').any(|s| s == "watch=true") {
        let receiver = fixture.events.subscribe();
        let initial = {
            let objects = fixture.objects.lock().unwrap();
            let mut lines: Vec<String> = objects
                .values
                .iter()
                .filter(|(key, _)| collection(key) == path)
                .map(|(_, value)| format!("{}\n", json!({"type":"ADDED","object":value})))
                .collect();
            lines.push(format!("{}\n", json!({"type":"BOOKMARK","object":{"apiVersion":"v1","kind":list_kind(&path).trim_end_matches("List"),"metadata":{"resourceVersion":objects.revision.to_string(),"annotations":{"k8s.io/initial-events-end":"true"}}}})));
            lines
        };
        let stream = stream::unfold((receiver, path), |(mut receiver, path)| async move {
            loop {
                match receiver.recv().await {
                    Ok((event_path, event)) if collection(&event_path) == path => {
                        return Some((
                            Ok::<_, Infallible>(format!("{}\n", event)),
                            (receiver, path),
                        ));
                    }
                    Ok(_) | Err(broadcast::error::RecvError::Lagged(_)) => (),
                    Err(_) => return None,
                }
            }
        });
        use futures::StreamExt;
        return Response::new(Body::from_stream(
            futures::stream::iter(initial.into_iter().map(Ok::<_, Infallible>)).chain(stream),
        ));
    }
    let bytes = match axum::body::to_bytes(request.into_body(), 2 * 1024 * 1024).await {
        Ok(b) => b,
        Err(_) => return status(StatusCode::BAD_REQUEST, "body"),
    };
    let mut body: Value = if bytes.is_empty() {
        Value::Null
    } else {
        match serde_json::from_slice(&bytes) {
            Ok(v) => v,
            Err(_) => return status(StatusCode::BAD_REQUEST, "json"),
        }
    };
    let mut objects = fixture.objects.lock().unwrap();
    if path.ends_with("/tokenreviews") && method == Method::POST {
        objects.reviews += 1;
        let accepted = body["spec"]["audiences"] == json!(["racer-control"])
            && body["spec"]["token"]
                .as_str()
                .is_some_and(|t| t.starts_with("fixture."));
        return axum::Json(json!({"apiVersion":"authentication.k8s.io/v1","kind":"TokenReview","metadata":{},"spec":body["spec"],"status":{"authenticated":accepted,"audiences":["racer-control"],"user":{"username":"system:serviceaccount:system:racer-dataplane","extra":{"authentication.kubernetes.io/pod-uid":["worker-pod"]}}}})).into_response();
    }
    if method == Method::GET {
        if let Some(value) = objects.values.get(&path) {
            return axum::Json(value.clone()).into_response();
        }
        let plural = path.rsplit('/').next().unwrap_or("");
        if [
            "pods",
            "nodes",
            "configmaps",
            "secrets",
            "leases",
            "sites",
            "racercaches",
            "p2pcaches",
            "caches",
            "daemonsets",
            "replicasets",
            "deployments",
        ]
        .contains(&plural)
        {
            let selector = query
                .split('&')
                .find_map(|p| p.strip_prefix("labelSelector="))
                .map(|s| {
                    s.replace("%2F", "/")
                        .replace("%2f", "/")
                        .replace("%3D", "=")
                        .replace("%3d", "=")
                        .replace("%2E", ".")
                });
            let items: Vec<_> = objects
                .values
                .iter()
                .filter(|(key, value)| {
                    collection(key) == path
                        && selector.as_ref().is_none_or(|s| {
                            s.split_once('=').is_none_or(|(k, v)| {
                                value["metadata"]["labels"][k].as_str() == Some(v)
                            })
                        })
                })
                .map(|(_, value)| value.clone())
                .collect();
            return axum::Json(json!({"apiVersion":if path.starts_with("/api/") {"v1"} else if path.contains("/coordination.k8s.io/") {"coordination.k8s.io/v1"} else if path.contains("/racer.unbounded-cloud.io/") {"racer.unbounded-cloud.io/v1alpha1"} else {"unbounded-cloud.io/v1alpha3"},"kind":list_kind(&path),"metadata":{"resourceVersion":objects.revision.to_string()},"items":items})).into_response();
        }
        return status(StatusCode::NOT_FOUND, "NotFound");
    }
    let key = if method == Method::POST {
        format!(
            "{}/{}",
            path,
            body["metadata"]["name"].as_str().unwrap_or("")
        )
    } else {
        path.strip_suffix("/status").unwrap_or(&path).to_owned()
    };
    let old = objects.values.get(&key).cloned();
    if method == Method::POST && old.is_some() {
        return status(StatusCode::CONFLICT, "AlreadyExists");
    }
    if method != Method::POST && old.is_none() {
        return status(StatusCode::NOT_FOUND, "NotFound");
    }
    if let Some(old) = &old {
        let metadata = if method == Method::DELETE {
            &body["preconditions"]
        } else {
            &body["metadata"]
        };
        if metadata["resourceVersion"]
            .as_str()
            .is_some_and(|rv| Some(rv) != old["metadata"]["resourceVersion"].as_str())
            || metadata["uid"]
                .as_str()
                .is_some_and(|uid| Some(uid) != old["metadata"]["uid"].as_str())
        {
            return status(StatusCode::CONFLICT, "Conflict");
        }
    }
    if method == Method::DELETE {
        let old = objects.values.remove(&key).unwrap();
        let _ = fixture
            .events
            .send((key, json!({"type":"DELETED","object":old})));
        return axum::Json(
            json!({"apiVersion":"v1","kind":"Status","status":"Success","code":200}),
        )
        .into_response();
    }
    if method == Method::PATCH {
        let mut merged = old.clone().unwrap();
        merge(&mut merged, body);
        body = merged;
    }
    if let Some(old) = &old
        && old["immutable"] == true
    {
        // Kubernetes freezes ConfigMap payloads and the immutable flag, not
        // metadata. RecordStore deliberately touches metadata to fence GC when
        // reusing a content-addressed chunk in a new leadership term.
        if body["immutable"] != true
            || body["data"] != old["data"]
            || body["binaryData"] != old["binaryData"]
        {
            return status(StatusCode::UNPROCESSABLE_ENTITY, "Immutable");
        }
        objects.immutable_metadata_updates += 1;
    }
    objects.revision += 1;
    body["metadata"]["resourceVersion"] = objects.revision.to_string().into();
    if body["metadata"]["uid"].is_null() {
        body["metadata"]["uid"] = format!("object-{}", objects.revision).into();
    }
    if body["metadata"]["namespace"].is_null() && key.contains("/namespaces/system/") {
        body["metadata"]["namespace"] = "system".into();
    }
    objects.values.insert(key.clone(), body.clone());
    let _ = fixture.events.send((
        key.clone(),
        json!({"type":if old.is_some() {"MODIFIED"} else {"ADDED"},"object":body}),
    ));
    if key.ends_with("/secrets/racer-ca") && objects.uncertain_secret {
        objects.uncertain_secret = false;
        return status(StatusCode::GATEWAY_TIMEOUT, "Timeout");
    }
    axum::Json(body).into_response()
}

impl Fixture {
    fn remove(&self, path: &str) {
        let mut objects = self.objects.lock().unwrap();
        if let Some(value) = objects.values.remove(path) {
            let _ = self
                .events
                .send((path.into(), json!({"type":"DELETED","object":value})));
        }
    }
    async fn start() -> Result<(Self, Client, CancellationToken)> {
        let fixture = Self {
            objects: Default::default(),
            events: broadcast::channel(4096).0,
            pod_list_pause: Default::default(),
        };
        let listener = TcpListener::bind("127.0.0.1:0").await?;
        let address = listener.local_addr()?;
        let stop = CancellationToken::new();
        let stopped = stop.clone();
        let router = Router::new().fallback(api).with_state(fixture.clone());
        tokio::spawn(async move {
            axum::serve(listener, router)
                .with_graceful_shutdown(stopped.cancelled_owned())
                .await
                .unwrap();
        });
        let client = Client::try_from(kube::Config::new(format!("http://{address}").parse()?))?;
        Ok((fixture, client, stop))
    }
    fn put(&self, path: &str, mut value: Value) {
        let mut objects = self.objects.lock().unwrap();
        objects.revision += 1;
        value["metadata"]["resourceVersion"] = objects.revision.to_string().into();
        let prior = objects.values.insert(path.into(), value.clone());
        let _ = self.events.send((
            path.into(),
            json!({"type":if prior.is_some() {"MODIFIED"} else {"ADDED"},"object":value}),
        ));
    }
    fn get(&self, path: &str) -> Option<Value> {
        self.objects.lock().unwrap().values.get(path).cloned()
    }
    fn seed(&self) {
        let prefix = "/api/v1/namespaces/system";
        self.put(&format!("{prefix}/pods/controller"), json!({"apiVersion":"v1","kind":"Pod","metadata":{"name":"controller","namespace":"system","uid":"controller-pod","labels":{"racer.unbounded-cloud.io/component":"racer-controlplane"},"ownerReferences":[{"apiVersion":"apps/v1","kind":"ReplicaSet","name":"controllers","uid":"rs","controller":true}]},"spec":{"serviceAccountName":"racer-controlplane","containers":[{"name":"controller","image":"test"}]},"status":{"phase":"Running","podIP":"127.0.0.1"}}));
        self.put("/apis/apps/v1/namespaces/system/replicasets/controllers", json!({"apiVersion":"apps/v1","kind":"ReplicaSet","metadata":{"name":"controllers","namespace":"system","uid":"rs","ownerReferences":[{"apiVersion":"apps/v1","kind":"Deployment","name":"racer-controlplane","uid":"deployment","controller":true}]}}));
        self.put("/apis/apps/v1/namespaces/system/deployments/racer-controlplane", json!({"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"racer-controlplane","namespace":"system","uid":"deployment","labels":{"racer.unbounded-cloud.io/component":"racer-controlplane"}},"spec":{"selector":{"matchLabels":{"app":"controller"}},"template":{"metadata":{"labels":{"app":"controller"}},"spec":{"serviceAccountName":"racer-controlplane","containers":[{"name":"controller","image":"test"}]}}}}));
        self.put(&format!("{prefix}/services/racer-controlplane"), json!({"apiVersion":"v1","kind":"Service","metadata":{"name":"racer-controlplane","namespace":"system","uid":"service"},"spec":{"clusterIP":"10.0.0.1","clusterIPs":["10.0.0.1"],"selector":{"racer.unbounded-cloud.io/serving-leader":"true"},"ports":[{"name":"control","port":8443}]}}));
        self.put("/apis/unbounded-cloud.io/v1alpha3/sites/edge", json!({"apiVersion":"unbounded-cloud.io/v1alpha3","kind":"Site","metadata":{"name":"edge","uid":"site"},"spec":{"components":{"racer":{"enabled":true}}}}));
        self.put("/api/v1/nodes/worker", json!({"apiVersion":"v1","kind":"Node","metadata":{"name":"worker","uid":"node","labels":{"unbounded-cloud.io/site":"edge","kubernetes.io/os":"linux"}},"status":{"conditions":[{"type":"Ready","status":"True"}]}}));
        self.put("/apis/apps/v1/namespaces/system/daemonsets/dataplane", json!({"apiVersion":"apps/v1","kind":"DaemonSet","metadata":{"name":"dataplane","namespace":"system","uid":"ds","labels":{"racer.unbounded-cloud.io/component":"racer-dataplane"},"ownerReferences":[{"apiVersion":"unbounded-cloud.io/v1alpha3","kind":"Site","name":"edge","uid":"site"}]},"spec":{"selector":{"matchLabels":{"app":"worker"}},"template":{"metadata":{"labels":{"racer.unbounded-cloud.io/universe":"edge"}},"spec":{"serviceAccountName":"racer-dataplane","containers":[{"name":"dataplane","image":"test"}]}}}}));
        self.put(&format!("{prefix}/pods/worker"), json!({"apiVersion":"v1","kind":"Pod","metadata":{"name":"worker","namespace":"system","uid":"worker-pod","creationTimestamp":"2026-01-01T00:00:00Z","labels":{"racer.unbounded-cloud.io/dataplane":"true","racer.unbounded-cloud.io/universe":"edge"},"ownerReferences":[{"apiVersion":"apps/v1","kind":"DaemonSet","name":"dataplane","uid":"ds","controller":true}]},"spec":{"nodeName":"worker","serviceAccountName":"racer-dataplane","containers":[{"name":"dataplane","image":"test"}]},"status":{"phase":"Running","podIP":"10.0.0.2","conditions":[{"type":"Ready","status":"True"}]}}));
    }
}

async fn free_address() -> String {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    listener.local_addr().unwrap().to_string()
}

async fn request(
    address: &str,
    request: &str,
    tls: Option<Arc<rustls::ClientConfig>>,
) -> Result<Vec<u8>> {
    let raw = TcpStream::connect(address).await?;
    let mut stream: Box<dyn Io> = if let Some(config) = tls {
        Box::new(
            tokio_rustls::TlsConnector::from(config)
                .connect(
                    rustls::pki_types::ServerName::try_from("racer-controlplane.system.svc")?,
                    raw,
                )
                .await?,
        )
    } else {
        Box::new(raw)
    };
    stream.write_all(request.as_bytes()).await?;
    let mut response = Vec::new();
    loop {
        let mut bytes = [0; 4096];
        match stream.read(&mut bytes).await {
            Ok(0) => break,
            Ok(n) => response.extend_from_slice(&bytes[..n]),
            Err(error)
                if !response.is_empty() && error.kind() == std::io::ErrorKind::UnexpectedEof =>
            {
                break;
            }
            Err(error) => return Err(error.into()),
        }
    }
    Ok(response)
}
trait Io: tokio::io::AsyncRead + tokio::io::AsyncWrite + Unpin + Send {}
impl<T: tokio::io::AsyncRead + tokio::io::AsyncWrite + Unpin + Send> Io for T {}

fn tls_config(
    bundle: &TrustBundle,
    certificate: Option<(&str, &LocalKey)>,
) -> Arc<rustls::ClientConfig> {
    let mut roots = rustls::RootCertStore::empty();
    for cert in openssl::x509::X509::stack_from_pem(bundle.certificates.as_bytes()).unwrap() {
        roots.add(cert.to_der().unwrap().into()).unwrap();
    }
    let builder = rustls::ClientConfig::builder_with_provider(Arc::new(
        rustls::crypto::ring::default_provider(),
    ))
    .with_protocol_versions(&[&rustls::version::TLS13])
    .unwrap()
    .with_root_certificates(roots);
    let mut config = if let Some((certificate, key)) = certificate {
        let certs = openssl::x509::X509::stack_from_pem(certificate.as_bytes())
            .unwrap()
            .iter()
            .map(|c| c.to_der().unwrap().into())
            .collect();
        let key = openssl::pkey::PKey::private_key_from_pem(&key.key_pem)
            .unwrap()
            .private_key_to_pkcs8()
            .unwrap();
        builder
            .with_client_auth_cert(certs, rustls::pki_types::PrivateKeyDer::Pkcs8(key.into()))
            .unwrap()
    } else {
        builder.with_no_client_auth()
    };
    config.resumption = rustls::client::Resumption::disabled();
    Arc::new(config)
}

#[test]
fn cli_accepts_operator_flags_and_rejects_invalid_values() {
    let options = Options::parse(
        [
            "-listen=:9443",
            "--state-namespace",
            "system",
            "-ca-rotation-interval=720h",
            "-token-review-qps=2.5",
        ]
        .map(str::to_owned),
    )
    .unwrap();
    assert_eq!(options.listen, ":9443");
    assert_eq!(options.namespace, "system");
    assert_eq!(options.rotation_interval, Duration::from_secs(30 * 86400));
    assert_eq!(options.lease_timing.duration_seconds, 15);
    assert_eq!(options.lease_timing.renew_deadline, Duration::from_secs(10));
    assert_eq!(options.lease_timing.retry_period, Duration::from_secs(2));
    assert!(Options::parse(["-ca-rotation-interval=0s".into()]).is_err());
    assert!(Options::parse(["-token-review-qps=NaN".into()]).is_err());
    assert!(Options::parse(["-invented=true".into()]).is_err());
    assert!(Options::parse(["version".into()]).unwrap().version);
    let short = Options::parse(["--leaf-lifetime=120s".into(), "--clock-skew=1s".into()]).unwrap();
    assert_eq!(short.leaf_lifetime, Duration::from_secs(120));
    assert_eq!(short.clock_skew, Duration::from_secs(1));
    assert!(Options::parse(["--leaf-lifetime=1s".into()]).is_err());
    assert!(Options::parse(["--clock-skew=0s".into()]).is_err());
}

#[tokio::test]
async fn actual_kube_store_cas_uncertain_write_and_takeover() -> Result<()> {
    let (fixture, client, stop) = Fixture::start().await?;
    let renew =
        time::OffsetDateTime::now_utc().format(&time::format_description::well_known::Rfc3339)?;
    let lease_path = format!("/apis/coordination.k8s.io/v1/namespaces/system/leases/{LEASE}");
    fixture.put(&lease_path, json!({"apiVersion":"coordination.k8s.io/v1","kind":"Lease","metadata":{"name":LEASE,"namespace":"system","uid":"lease"},"spec":{"holderIdentity":"first","renewTime":renew,"leaseDurationSeconds":15}}));
    let term = Leadership::new("first".into())?;
    let store = KubernetesCaStore::new(client.clone(), "system".into(), term.clone());
    fixture.objects.lock().unwrap().uncertain_secret = true;
    let manager = CaManager::acquire(
        store.clone(),
        term,
        SecurityOptions::new("system"),
        unix_now(),
    )
    .await?;
    let key = generate_local_key()?;
    let identity = Identity {
        kind: IdentityKind::Node,
        universe: "a".repeat(64),
        node: "b".repeat(64),
        pod_uid: "pod".into(),
        boot_id: "c".repeat(64),
        pod_name: "pod".into(),
        container_id: String::new(),
    };
    fixture.objects.lock().unwrap().uncertain_secret = true;
    let issued = manager
        .issue(&key.csr_pem, identity.clone(), false, unix_now())
        .await?;
    assert!(manager.state().await?.member(&identity.key()).is_some());
    let old_snapshot = store.read().await?;
    let mut lease = fixture.get(&lease_path).unwrap();
    lease["spec"]["holderIdentity"] = "second".into();
    fixture.put(&lease_path, lease);
    let term = Leadership::new("second".into())?;
    let second_store = KubernetesCaStore::new(client, "system".into(), term.clone());
    let second = CaManager::acquire(
        second_store,
        term,
        SecurityOptions::new("system"),
        unix_now(),
    )
    .await?;
    assert_eq!(second.state().await?.bundle().active, issued.root_digest);
    assert!(!matches!(
        store
            .publish(&old_snapshot, "first", &issued.bundle.json())
            .await,
        Ok(CommitOutcome::Committed)
    ));
    assert!(
        manager
            .issue(&key.csr_pem, identity, false, unix_now())
            .await
            .is_err()
    );
    let old_image = old_snapshot.image.as_ref().unwrap();
    for id in old_image.shards.keys() {
        let name = racer_controlplane::security::kubernetes::participant_object_name("first", id);
        let path = format!("/api/v1/namespaces/system/configmaps/{name}");
        let mut object = fixture.get(&path).unwrap();
        object["metadata"]["creationTimestamp"] = "2026-01-01T00:00:00Z".into();
        fixture.put(&path, object);
    }
    second.collect().await?;
    for id in old_image.shards.keys() {
        let name = racer_controlplane::security::kubernetes::participant_object_name("first", id);
        assert!(
            fixture
                .get(&format!("/api/v1/namespaces/system/configmaps/{name}"))
                .is_none()
        );
    }
    assert!(
        second
            .state()
            .await?
            .member(&format!("pod/{}", "c".repeat(64)))
            .is_some(),
        "collection must retain successor shards"
    );
    stop.cancel();
    Ok(())
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn runnable_service_bootstraps_enrolls_and_serves_authenticated_v4() -> Result<()> {
    let _ = tracing_subscriber::fmt()
        .with_env_filter("racer_controlplane=debug,kube=warn")
        .with_test_writer()
        .try_init();
    let (fixture, client, api_stop) = Fixture::start().await?;
    fixture.seed();
    let options = Options {
        namespace: "system".into(),
        pod_name: "controller".into(),
        pod_uid: "controller-pod".into(),
        listen: free_address().await,
        enroll_listen: free_address().await,
        health_listen: free_address().await,
        replica_proof_listen: free_address().await,
        trust_proof_listen: free_address().await,
        ..Default::default()
    };
    let stop = CancellationToken::new();
    let run = tokio::spawn(service::run(client, options.clone(), stop.clone()));
    let ready = tokio::time::timeout(Duration::from_secs(45), async {
        loop {
            if run.is_finished() {
                bail_service(&fixture);
            }
            if fixture
                .get("/api/v1/namespaces/system/pods/controller")
                .is_some_and(|p| {
                    p["metadata"]["labels"]["racer.unbounded-cloud.io/serving-leader"] == "true"
                })
            {
                break;
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
    })
    .await;
    if ready.is_err() {
        stop.cancel();
        let _ = run.await;
        bail_service(&fixture);
    }
    let trust = fixture
        .get("/api/v1/namespaces/system/configmaps/racer-trust")
        .unwrap();
    let bundle = TrustBundle::parse(trust["data"]["bundle.json"].as_str().unwrap().as_bytes())?;
    let health = request(
        &options.health_listen,
        "GET /readyz HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n",
        None,
    )
    .await?;
    assert!(health.starts_with(b"HTTP/1.1 200"));
    // Withhold an old authoritative list that does not contain a not-yet-admitted
    // Pod. Its creation/watch publication and real HTTPS enrollment finish while
    // the retirement sweep is suspended. Completing that sweep must not revoke it.
    let worker_path = "/api/v1/namespaces/system/pods/worker";
    let worker = fixture.get(worker_path).unwrap();
    fixture.remove(worker_path);
    let pause = Arc::new(ListPause::default());
    *fixture.pod_list_pause.lock().unwrap() = Some(pause.clone());
    tokio::time::timeout(Duration::from_secs(10), pause.captured.notified()).await?;
    fixture.put(worker_path, worker);
    let key = generate_local_key()?;
    let body = serde_json::to_string(
        &json!({"csr":String::from_utf8(key.csr_pem.clone())?,"pod_namespace":"system","pod_name":"worker"}),
    )?;
    use base64::Engine;
    let token = format!(
        "fixture.{}.signature",
        base64::engine::general_purpose::URL_SAFE_NO_PAD
            .encode(format!("{{\"exp\":{}}}", unix_now() + 3600))
    );
    let boot = "a".repeat(64);
    let enrollment = format!(
        "POST /v3/enroll HTTP/1.1\r\nHost: racer-controlplane.system.svc\r\nAuthorization: Bearer {token}\r\nX-Racer-Boot: {boot}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
        body.len()
    );
    let mut attempts = Vec::new();
    let response = tokio::time::timeout(Duration::from_secs(10), async {
        loop {
            let response = request(
                &options.enroll_listen,
                &enrollment,
                Some(tls_config(&bundle, None)),
            )
            .await?;
            attempts.push((
                String::from_utf8_lossy(&response)
                    .lines()
                    .next()
                    .unwrap_or_default()
                    .to_owned(),
                fixture.objects.lock().unwrap().reviews,
            ));
            if response.starts_with(b"HTTP/1.1 200") {
                break Ok::<_, anyhow::Error>(response);
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
    })
    .await
    .with_context(|| {
        format!("enrollment readiness deadline; status/review counts: {attempts:?}")
    })??;
    ensure!(
        response.starts_with(b"HTTP/1.1 200"),
        "enrollment failed: {}",
        String::from_utf8_lossy(&response)
    );
    let start = response.windows(4).position(|b| b == b"\r\n\r\n").unwrap() + 4;
    // Readiness retries may have authenticated long before issuance succeeds.
    // Prime a distinct credential after readiness, then measure only its
    // immediate repeat, independently of retirement sweeps and prior reviews.
    let cached_enrollment = enrollment.replace(&token, &token.replace("signature", "cache-check"));
    let before = fixture.objects.lock().unwrap().reviews;
    let primed = request(
        &options.enroll_listen,
        &cached_enrollment,
        Some(tls_config(&bundle, None)),
    )
    .await?;
    assert!(primed.starts_with(b"HTTP/1.1 200"));
    let after_prime = fixture.objects.lock().unwrap().reviews;
    assert_eq!(after_prime - before, 1, "fresh credential must be reviewed");
    let renewed = request(
        &options.enroll_listen,
        &cached_enrollment,
        Some(tls_config(&bundle, None)),
    )
    .await?;
    assert!(renewed.starts_with(b"HTTP/1.1 200"));
    assert_eq!(
        fixture.objects.lock().unwrap().reviews - after_prime,
        0,
        "immediate repeat must use the successful TokenReview cache"
    );
    // Each capture is at the next unfiltered Pod list in the serial participant
    // reconciler. Two more captures prove both the withheld stale sweep and a
    // subsequent live sweep finished, including refreshing authorization state.
    let live_sweep = Arc::new(ListPause::default());
    *fixture.pod_list_pause.lock().unwrap() = Some(live_sweep.clone());
    pause.resume.notify_one();
    tokio::time::timeout(Duration::from_secs(10), live_sweep.captured.notified())
        .await
        .context("stale retirement sweep completion deadline")?;
    let next_sweep = Arc::new(ListPause::default());
    *fixture.pod_list_pause.lock().unwrap() = Some(next_sweep.clone());
    live_sweep.resume.notify_one();
    tokio::time::timeout(Duration::from_secs(10), next_sweep.captured.notified())
        .await
        .context("live retirement sweep completion deadline")?;
    let response: Value = serde_json::from_slice(&response[start..])?;
    let config = tls_config(
        &bundle,
        Some((response["certificate"].as_str().unwrap(), &key)),
    );
    let control = request(&options.listen, &format!("GET /v4/config HTTP/1.1\r\nHost: racer-controlplane.system.svc\r\nX-Racer-Boot: {boot}\r\nX-Racer-Profile: 1\r\nConnection: close\r\n\r\n"), Some(config)).await?;
    ensure!(
        control.starts_with(b"HTTP/1.1 200"),
        "control failed: {}",
        String::from_utf8_lossy(&control)
    );
    next_sweep.resume.notify_one();
    let proof = request(&options.trust_proof_listen, &format!("POST /v3/proof HTTP/1.1\r\nHost: racer-controlplane.system.svc\r\nX-Racer-Boot: {boot}\r\nX-Racer-Trust-Generation: {}\r\nX-Racer-Trust-Digest: {}\r\nX-Racer-Certificate-Issuer: {}\r\nX-Racer-Old-Connections: 0\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", bundle.generation, bundle.digest(), response["issuer"].as_str().unwrap()), Some(tls_config(&bundle, Some((response["certificate"].as_str().unwrap(), &key))))).await?;
    ensure!(
        proof.starts_with(b"HTTP/1.1 204"),
        "proof failed: {}",
        String::from_utf8_lossy(&proof)
    );
    // A caller-provided identity header cannot substitute for a client certificate.
    assert!(request(&options.listen, "GET /v4/config HTTP/1.1\r\nHost: x\r\nX-Racer-Pod: worker-pod\r\nConnection: close\r\n\r\n", Some(tls_config(&bundle, None))).await.is_err());
    let mut trust = fixture
        .get("/api/v1/namespaces/system/configmaps/racer-trust")
        .unwrap();
    trust["metadata"]["annotations"]["racer.unbounded-cloud.io/rotate-ca"] =
        "operator-request-1".into();
    fixture.put("/api/v1/namespaces/system/configmaps/racer-trust", trust);
    let overlap = tokio::time::timeout(Duration::from_secs(20), async {
        loop {
            let trust = fixture
                .get("/api/v1/namespaces/system/configmaps/racer-trust")
                .unwrap();
            let next =
                TrustBundle::parse(trust["data"]["bundle.json"].as_str().unwrap().as_bytes())
                    .unwrap();
            if next.generation > bundle.generation {
                break next;
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
    })
    .await?;
    assert_eq!(overlap.active, bundle.active);
    let proof_request = format!(
        "POST /v3/proof HTTP/1.1\r\nHost: racer-controlplane.system.svc\r\nX-Racer-Boot: {boot}\r\nX-Racer-Trust-Generation: {}\r\nX-Racer-Trust-Digest: {}\r\nX-Racer-Certificate-Issuer: {}\r\nX-Racer-Old-Connections: 0\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
        overlap.generation,
        overlap.digest(),
        response["issuer"].as_str().unwrap()
    );
    tokio::time::timeout(Duration::from_secs(20), async {
        loop {
            if request(
                &options.trust_proof_listen,
                &proof_request,
                Some(tls_config(
                    &overlap,
                    Some((response["certificate"].as_str().unwrap(), &key)),
                )),
            )
            .await
            .is_ok_and(|r| r.starts_with(b"HTTP/1.1 204"))
            {
                break;
            }
            tokio::time::sleep(Duration::from_millis(200)).await;
        }
    })
    .await?;
    let switched = tokio::time::timeout(Duration::from_secs(20), async {
        loop {
            let trust = fixture
                .get("/api/v1/namespaces/system/configmaps/racer-trust")
                .unwrap();
            let next =
                TrustBundle::parse(trust["data"]["bundle.json"].as_str().unwrap().as_bytes())
                    .unwrap();
            if next.active != bundle.active {
                break next;
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
    })
    .await?;
    assert_eq!(switched.generation, overlap.generation + 1);
    // A retry wave is bounded before TokenReview and live object authorization.
    // Use distinct credentials to exercise misses rather than completed cache hits.
    fixture.objects.lock().unwrap().review_delay = Duration::from_secs(2);
    let before = fixture.objects.lock().unwrap().reviews;
    let requests = (0..24).map(|n| {
        let enrollment = enrollment.replace(&token, &format!("fixture.miss{n}.signature"));
        let address = options.enroll_listen.clone();
        let config = tls_config(&switched, None);
        async move { request(&address, &enrollment, Some(config)).await }
    });
    let responses = futures::future::join_all(requests).await;
    assert!(
        responses
            .iter()
            .any(|r| r.as_ref().is_ok_and(|r| r.starts_with(b"HTTP/1.1 503"))),
        "overloaded enrollment should reject promptly"
    );
    // Delayed HTTP fixture operations may still complete after client timeout.
    tokio::time::sleep(Duration::from_secs(2)).await;
    assert!(
        fixture.objects.lock().unwrap().reviews - before <= 8,
        "TokenReview work exceeded pipeline bound"
    );
    fixture.objects.lock().unwrap().review_delay = Duration::ZERO;
    // A second boot of the same Pod cannot identify which process is dead.
    // The service replaces the managed Pod, then retires only after UID absence.
    let second_boot = "b".repeat(64);
    let second_enrollment = enrollment.replace(&boot, &second_boot);
    let admitted = request(
        &options.enroll_listen,
        &second_enrollment,
        Some(tls_config(&switched, None)),
    )
    .await?;
    ensure!(
        admitted.starts_with(b"HTTP/1.1 200"),
        "second boot enrollment failed: {}",
        String::from_utf8_lossy(&admitted)
    );
    tokio::time::timeout(Duration::from_secs(20), async {
        while fixture
            .get("/api/v1/namespaces/system/pods/worker")
            .is_some()
        {
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
    })
    .await?;
    tokio::time::timeout(Duration::from_secs(20), async {
        loop {
            let secret = fixture
                .get("/api/v1/namespaces/system/secrets/racer-ca")
                .unwrap();
            let metadata = base64::engine::general_purpose::STANDARD
                .decode(secret["data"]["state.json"].as_str().unwrap())
                .unwrap();
            let value: Value = serde_json::from_slice(&metadata).unwrap();
            let mut shards = BTreeMap::new();
            for id in value["shards"]
                .as_object()
                .unwrap()
                .values()
                .filter_map(Value::as_str)
            {
                let shard = fixture
                    .get(&format!(
                        "/api/v1/namespaces/system/configmaps/{}",
                        racer_controlplane::security::kubernetes::participant_object_name(
                            value["fence"].as_str().unwrap(),
                            id
                        )
                    ))
                    .unwrap();
                shards.insert(
                    id.into(),
                    shard["data"]["state.json"]
                        .as_str()
                        .unwrap()
                        .as_bytes()
                        .to_vec(),
                );
            }
            let state = CaState::from_image(&StateImage { metadata, shards }).unwrap();
            if state.member(&format!("worker-pod/{boot}")).is_none()
                && state.member(&format!("worker-pod/{second_boot}")).is_none()
            {
                let certificate = openssl::x509::X509::from_pem(
                    response["certificate"].as_str().unwrap().as_bytes(),
                )
                .unwrap();
                let der = certificate.to_der().unwrap();
                let (_, certificate) = x509_parser::parse_x509_certificate(&der).unwrap();
                assert!(
                    state.expiry_watermarks().any(|(issuer, expiry)| issuer
                        == response["issuer"].as_str().unwrap()
                        && expiry >= certificate.validity().not_after.timestamp()),
                    "retirement must preserve issued expiry watermark"
                );
                break;
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
    })
    .await?;
    stop.cancel();
    run.await??;
    api_stop.cancel();
    Ok(())
}

fn bail_service(fixture: &Fixture) -> ! {
    let objects = fixture.objects.lock().unwrap();
    let summaries: Vec<_> = objects
        .values
        .iter()
        .map(|(key, value)| {
            (
                key,
                value.get("metadata"),
                value
                    .get("data")
                    .and_then(|d| d.as_object())
                    .map(|d| d.keys().collect::<Vec<_>>()),
            )
        })
        .collect();
    panic!("service did not become ready; object summaries: {summaries:?}")
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn warm_standby_takes_over_without_regenerating_trust() -> Result<()> {
    let (fixture, client, api_stop) = Fixture::start().await?;
    fixture.seed();
    let proof_address = free_address().await;
    let port = proof_address.rsplit(':').next().unwrap();
    // Exercise real Lease CAS/expiry and TLS takeover with shorter timing.
    // The production-binary live campaign retains the default 15/10/2 seconds.
    let lease_timing = LeaseTiming {
        duration_seconds: 3,
        renew_deadline: Duration::from_secs(2),
        retry_period: Duration::from_millis(200),
    };
    let first = Options {
        namespace: "system".into(),
        pod_name: "controller".into(),
        pod_uid: "controller-pod".into(),
        listen: free_address().await,
        enroll_listen: free_address().await,
        health_listen: free_address().await,
        replica_proof_listen: proof_address.clone(),
        trust_proof_listen: free_address().await,
        lease_timing,
        ..Default::default()
    };
    let first_stop = CancellationToken::new();
    let first_run = tokio::spawn(service::run(
        client.clone(),
        first.clone(),
        first_stop.clone(),
    ));
    tokio::time::timeout(Duration::from_secs(30), async {
        while !fixture
            .get("/api/v1/namespaces/system/pods/controller")
            .is_some_and(|p| {
                p["metadata"]["labels"]["racer.unbounded-cloud.io/serving-leader"] == "true"
            })
        {
            ensure!(
                !first_run.is_finished(),
                "initial service exited before publishing its route"
            );
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
        Ok::<_, anyhow::Error>(())
    })
    .await
    .context("initial leader route deadline")??;
    let original = fixture
        .get("/api/v1/namespaces/system/configmaps/racer-trust")
        .unwrap()["data"]["bundle.json"]
        .as_str()
        .unwrap()
        .to_owned();
    let mut pod = fixture
        .get("/api/v1/namespaces/system/pods/controller")
        .unwrap();
    pod["metadata"]["name"] = "standby".into();
    pod["metadata"]["uid"] = "standby-pod".into();
    pod["metadata"]["labels"]
        .as_object_mut()
        .unwrap()
        .remove("racer.unbounded-cloud.io/serving-leader");
    pod["metadata"]["annotations"] = json!({});
    pod["status"]["podIP"] = "127.0.0.2".into();
    fixture.put("/api/v1/namespaces/system/pods/standby", pod);
    let second = Options {
        namespace: "system".into(),
        pod_name: "standby".into(),
        pod_uid: "standby-pod".into(),
        listen: free_address().await,
        enroll_listen: free_address().await,
        health_listen: free_address().await,
        replica_proof_listen: format!("127.0.0.2:{port}"),
        trust_proof_listen: free_address().await,
        lease_timing,
        ..Default::default()
    };
    let second_stop = CancellationToken::new();
    let second_run = tokio::spawn(service::run(client, second.clone(), second_stop.clone()));
    tokio::time::timeout(Duration::from_secs(30), async {
        loop {
            ensure!(
                !second_run.is_finished(),
                "standby service exited before becoming ready"
            );
            if request(
                &second.health_listen,
                "GET /readyz HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n",
                None,
            )
            .await
            .is_ok_and(|r| r.starts_with(b"HTTP/1.1 200"))
            {
                break Ok::<_, anyhow::Error>(());
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
    })
    .await
    .context("warm standby readiness deadline")??;
    let bundle = TrustBundle::parse(original.as_bytes())?;
    let rejected = request(
        &second.enroll_listen,
        "POST /v3/enroll HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
        Some(tls_config(&bundle, None)),
    )
    .await?;
    assert!(
        rejected.starts_with(b"HTTP/1.1 503"),
        "warm follower must reject enrollment"
    );
    let lease_path = format!("/apis/coordination.k8s.io/v1/namespaces/system/leases/{LEASE}");
    let original_lease = fixture.get(&lease_path).unwrap();
    assert_eq!(original_lease["spec"]["leaseDurationSeconds"], 3);
    first_stop.cancel();
    first_run.await??;
    fixture.remove("/api/v1/namespaces/system/pods/controller");
    let before_takeover = fixture.objects.lock().unwrap().immutable_metadata_updates;
    tokio::time::timeout(Duration::from_secs(15), async {
        while !fixture
            .get("/api/v1/namespaces/system/pods/standby")
            .is_some_and(|p| {
                p["metadata"]["labels"]["racer.unbounded-cloud.io/serving-leader"] == "true"
            })
        {
            ensure!(
                !second_run.is_finished(),
                "standby service exited during takeover"
            );
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
        Ok::<_, anyhow::Error>(())
    })
    .await
    .context("standby takeover route deadline")??;
    let successor_lease = fixture.get(&lease_path).unwrap();
    assert_ne!(
        original_lease["spec"]["holderIdentity"],
        successor_lease["spec"]["holderIdentity"]
    );
    assert_eq!(
        successor_lease["spec"]["leaseTransitions"]
            .as_i64()
            .unwrap(),
        original_lease["spec"]["leaseTransitions"].as_i64().unwrap() + 1
    );
    assert!(
        fixture.objects.lock().unwrap().immutable_metadata_updates > before_takeover,
        "takeover must reclaim existing immutable chunks through metadata fencing"
    );
    let current = fixture
        .get("/api/v1/namespaces/system/configmaps/racer-trust")
        .unwrap()["data"]["bundle.json"]
        .as_str()
        .unwrap()
        .to_owned();
    assert_eq!(
        original, current,
        "leadership takeover must reuse durable CA"
    );
    second_stop.cancel();
    second_run.await??;
    api_stop.cancel();
    Ok(())
}

#[tokio::test]
async fn immutable_configmap_fixture_allows_fenced_metadata_but_rejects_payload_changes()
-> Result<()> {
    use k8s_openapi::api::core::v1::ConfigMap;
    use kube::{
        Api,
        api::{Patch, PatchParams, PostParams},
    };
    let (fixture, client, stop) = Fixture::start().await?;
    let maps = Api::<ConfigMap>::namespaced(client, "system");
    let initial: ConfigMap = serde_json::from_value(json!({
        "apiVersion": "v1", "kind": "ConfigMap",
        "metadata": {"name": "immutable-chunk", "namespace": "system"},
        "immutable": true, "data": {"text": "original"}, "binaryData": {"content": "YWJj"}
    }))?;
    let mut current = maps.create(&PostParams::default(), &initial).await?;
    let stale = current.clone();
    current.metadata.annotations = Some([("fence".into(), "successor".into())].into());
    current = maps
        .replace("immutable-chunk", &PostParams::default(), &current)
        .await?;
    assert_ne!(
        stale.metadata.resource_version,
        current.metadata.resource_version
    );
    let current = maps.patch("immutable-chunk", &PatchParams::default(), &Patch::Merge(json!({
        "metadata": {"resourceVersion": current.metadata.resource_version, "annotations": {"fence": "next"}}
    }))).await?;
    assert_eq!(
        fixture.objects.lock().unwrap().immutable_metadata_updates,
        2
    );
    assert!(
        matches!(maps.replace("immutable-chunk", &PostParams::default(), &stale).await,
        Err(kube::Error::Api(error)) if error.code == 409)
    );
    for forbidden in [
        json!({"data":{"text":"changed"}}),
        json!({"binaryData":{"content":"ZGVm"}}),
        json!({"immutable":false}),
    ] {
        assert!(
            matches!(maps.patch("immutable-chunk", &PatchParams::default(), &Patch::Merge(forbidden)).await,
            Err(kube::Error::Api(error)) if error.code == 422)
        );
    }
    assert_eq!(maps.get("immutable-chunk").await?, current);
    stop.cancel();
    Ok(())
}

#[test]
fn production_binary_version_is_runnable_without_kubernetes() {
    let output = std::process::Command::new(env!("CARGO_BIN_EXE_racer-controlplane"))
        .arg("-version")
        .output()
        .unwrap();
    assert!(output.status.success());
    assert!(
        String::from_utf8(output.stdout)
            .unwrap()
            .starts_with("racer-controlplane ")
    );
}
