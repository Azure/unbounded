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
    time::{Duration, Instant},
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
    route_patches: usize,
    route_lists: usize,
    get_errors: BTreeMap<String, StatusCode>,
    requests: u64,
    in_flight: BTreeMap<u64, (String, Instant)>,
    write_fault: Option<(String, Method, bool, bool)>,
    secret_read_pause: Option<(usize, Arc<RoutePause>)>,
    stalled_gets: BTreeMap<String, CancellationToken>,
    stalled_get_attempts: usize,
}
#[derive(Clone)]
struct Fixture {
    objects: Arc<Mutex<Objects>>,
    events: broadcast::Sender<(String, Value)>,
    pod_list_pause: Arc<Mutex<Option<Arc<ListPause>>>>,
    route_fault: Arc<Mutex<Option<RouteFault>>>,
}

enum RouteFault {
    Fail,
    Pause(Arc<RoutePause>, bool),
}

#[derive(Default)]
struct RoutePause {
    captured: tokio::sync::Notify,
    resume: tokio::sync::Notify,
    completed: tokio::sync::Notify,
    status: Mutex<Option<StatusCode>>,
}

#[derive(Default)]
struct ListPause {
    captured: tokio::sync::Notify,
    resume: tokio::sync::Notify,
}

struct ApiRequestGuard {
    objects: Arc<Mutex<Objects>>,
    id: u64,
}

impl Drop for ApiRequestGuard {
    fn drop(&mut self) {
        self.objects.lock().unwrap().in_flight.remove(&self.id);
    }
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
    let guard = {
        let mut objects = fixture.objects.lock().unwrap();
        objects.requests += 1;
        let id = objects.requests;
        objects.in_flight.insert(
            id,
            (
                format!("{} {}", request.method(), request.uri()),
                Instant::now(),
            ),
        );
        ApiRequestGuard {
            objects: fixture.objects.clone(),
            id,
        }
    };
    let (parts, body) = request.into_parts();
    let bytes = axum::body::to_bytes(body, 2 * 1024 * 1024).await.unwrap();
    let patch: Value = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
    let stalled = if parts.method == Method::GET {
        let mut objects = fixture.objects.lock().unwrap();
        let stalled = objects.stalled_gets.get(parts.uri.path()).cloned();
        if stalled.is_some() {
            objects.stalled_get_attempts += 1;
        }
        stalled
    } else {
        None
    };
    if let Some(stalled) = stalled {
        stalled.cancelled().await;
    }
    let pause = if parts.method == Method::GET && parts.uri.path().ends_with("/secrets/racer-ca") {
        let mut objects = fixture.objects.lock().unwrap();
        if let Some((remaining, _)) = objects.secret_read_pause.as_mut() {
            if *remaining == 0 {
                objects.secret_read_pause.take().map(|(_, p)| p)
            } else {
                *remaining -= 1;
                None
            }
        } else {
            None
        }
    } else {
        None
    };
    if let Some(pause) = pause {
        pause.captured.notify_one();
        pause.resume.notified().await;
    }
    let write_fault = {
        let mut objects = fixture.objects.lock().unwrap();
        if objects
            .write_fault
            .as_ref()
            .is_some_and(|(name, method, _, _)| {
                *method == parts.method && patch["metadata"]["name"] == name.as_str()
            })
        {
            objects.write_fault.take()
        } else {
            None
        }
    };
    if let Some((_, _, false, _)) = write_fault {
        return status(StatusCode::GATEWAY_TIMEOUT, "InjectedBeforeWrite");
    }
    let route_patch = parts.method == Method::PATCH
        && parts.uri.path().contains("/pods/")
        && patch["metadata"]["labels"]
            .get("racer.unbounded-cloud.io/serving-leader")
            .is_some();
    if route_patch {
        fixture.objects.lock().unwrap().route_patches += 1;
    }
    if parts.method == Method::GET
        && parts.uri.path().ends_with("/pods")
        && parts
            .uri
            .query()
            .is_some_and(|q| q.contains("labelSelector="))
    {
        fixture.objects.lock().unwrap().route_lists += 1;
    }
    let fault = if route_patch
        && patch["metadata"]["labels"]["racer.unbounded-cloud.io/serving-leader"] == "true"
    {
        fixture.route_fault.lock().unwrap().take()
    } else {
        None
    };
    if matches!(fault, Some(RouteFault::Fail)) {
        return status(StatusCode::INTERNAL_SERVER_ERROR, "InjectedRouteFailure");
    }
    let request = Request::from_parts(parts, Body::from(bytes));
    if fault.is_some() {
        // An accepted API write can outlive the client's canceled HTTP request.
        return tokio::spawn(async move {
            let _guard = guard;
            paused_route(fixture, request, fault).await
        })
        .await
        .unwrap();
    }
    let response = api_inner(State(fixture.clone()), request).await;
    if let Some((name, _, true, fail_readback)) = write_fault {
        if fail_readback {
            let kind = if name == "racer-ca" {
                "secrets"
            } else {
                "configmaps"
            };
            fixture.objects.lock().unwrap().get_errors.insert(
                format!("/api/v1/namespaces/system/{kind}/{name}"),
                StatusCode::SERVICE_UNAVAILABLE,
            );
        }
        return status(StatusCode::GATEWAY_TIMEOUT, "InjectedAfterWrite");
    }
    response
}

async fn paused_route(
    fixture: Fixture,
    request: Request<Body>,
    fault: Option<RouteFault>,
) -> Response {
    if let Some(RouteFault::Pause(pause, false)) = &fault {
        pause.captured.notify_one();
        pause.resume.notified().await;
    }
    let response = api_inner(State(fixture), request).await;
    if let Some(RouteFault::Pause(pause, committed)) = fault {
        if committed {
            pause.captured.notify_one();
            pause.resume.notified().await;
        }
        *pause.status.lock().unwrap() = Some(response.status());
        pause.completed.notify_one();
    }
    response
}

async fn api_inner(State(fixture): State<Fixture>, request: Request<Body>) -> Response {
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
        if let Some(code) = objects.get_errors.get(&path) {
            return status(*code, "InjectedLookupFailure");
        }
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
            route_fault: Default::default(),
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
        self.put("/apis/apps/v1/namespaces/system/daemonsets/racer-dataplane", json!({"apiVersion":"apps/v1","kind":"DaemonSet","metadata":{"name":"racer-dataplane","namespace":"system","uid":"ds","labels":{"racer.unbounded-cloud.io/component":"racer-dataplane"}},"spec":{"selector":{"matchLabels":{"app":"worker"}},"template":{"metadata":{"labels":{"racer.unbounded-cloud.io/component":"racer-dataplane","racer.unbounded-cloud.io/dataplane":"true"}},"spec":{"serviceAccountName":"racer-dataplane","containers":[{"name":"dataplane","image":"test"}]}}}}));
        self.put(&format!("{prefix}/pods/worker"), json!({"apiVersion":"v1","kind":"Pod","metadata":{"name":"worker","namespace":"system","uid":"worker-pod","creationTimestamp":"2026-01-01T00:00:00Z","labels":{"racer.unbounded-cloud.io/dataplane":"true","racer.unbounded-cloud.io/component":"racer-dataplane"},"ownerReferences":[{"apiVersion":"apps/v1","kind":"DaemonSet","name":"racer-dataplane","uid":"ds","controller":true}]},"spec":{"nodeName":"worker","serviceAccountName":"racer-dataplane","containers":[{"name":"dataplane","image":"test"}]},"status":{"phase":"Running","podIP":"10.0.0.2","conditions":[{"type":"Ready","status":"True"}]}}));
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
    assert!(
        manager
            .state()
            .await?
            .expiry_watermarks()
            .any(|(_, expiry)| expiry == issued.not_after)
    );
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
    assert!(old_image.metadata.len() <= 16384);
    second.collect().await?;
    assert_eq!(
        fixture
            .objects
            .lock()
            .unwrap()
            .values
            .keys()
            .filter(|p| p.contains("/configmaps/"))
            .count(),
        2
    );
    assert!(second.state().await?.to_image()?.metadata.len() < 16384);
    // Established state never authorizes recreating a lost revision checkpoint.
    fixture.remove("/api/v1/namespaces/system/configmaps/racer-runtime-revisions");
    second.publish().await?;
    assert!(
        fixture
            .get("/api/v1/namespaces/system/configmaps/racer-runtime-revisions")
            .is_none()
    );
    stop.cancel();
    Ok(())
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn runnable_service_bootstraps_enrolls_and_serves_authenticated_v4() -> Result<()> {
    service_replacement_scenario("boot").await
}

#[tokio::test]
async fn uncertain_bootstrap_resumes_checkpoint_trust_and_pending_marker_without_reset()
-> Result<()> {
    for (name, method) in [
        ("racer-runtime-revisions", Method::POST),
        ("racer-trust", Method::POST),
        ("racer-ca", Method::PUT),
    ] {
        for (committed, fail_readback) in [(false, false), (true, false), (true, true)] {
            let (fixture, client, stop) = Fixture::start().await?;
            let lease_path =
                format!("/apis/coordination.k8s.io/v1/namespaces/system/leases/{LEASE}");
            let renew = time::OffsetDateTime::now_utc()
                .format(&time::format_description::well_known::Rfc3339)?;
            fixture.put(&lease_path, json!({"apiVersion":"coordination.k8s.io/v1","kind":"Lease","metadata":{"name":LEASE,"namespace":"system","uid":"lease"},"spec":{"holderIdentity":"first","renewTime":renew,"leaseDurationSeconds":15}}));
            fixture.objects.lock().unwrap().write_fault =
                Some((name.into(), method.clone(), committed, fail_readback));
            let term = Leadership::new("first".into())?;
            let result = CaManager::acquire(
                KubernetesCaStore::new(client.clone(), "system".into(), term.clone()),
                term,
                SecurityOptions::new("system"),
                unix_now(),
            )
            .await;
            assert_eq!(
                result.is_ok(),
                committed && !fail_readback,
                "{name}: committed={committed} readback={fail_readback}"
            );
            let secret_path = "/api/v1/namespaces/system/secrets/racer-ca";
            let secret = fixture
                .get(secret_path)
                .context("durable bootstrap Secret missing")?;
            let checkpoint =
                fixture.get("/api/v1/namespaces/system/configmaps/racer-runtime-revisions");
            fixture.objects.lock().unwrap().get_errors.clear();
            let original = participant_state(&client).await?.bundle();
            let mut lease = fixture.get(&lease_path).unwrap();
            lease["spec"]["holderIdentity"] = "second".into();
            fixture.put(&lease_path, lease);
            let term = Leadership::new("second".into())?;
            let next = CaManager::acquire(
                KubernetesCaStore::new(client, "system".into(), term.clone()),
                term,
                SecurityOptions::new("system"),
                unix_now(),
            )
            .await?;
            assert_eq!(next.state().await?.bundle(), original);
            let completed = fixture.get(secret_path).unwrap();
            assert_eq!(completed["metadata"]["uid"], secret["metadata"]["uid"]);
            assert!(
                completed["metadata"]["annotations"]["racer.unbounded.cloud/pki-bootstrap-pending"]
                    .is_null()
            );
            let current = fixture
                .get("/api/v1/namespaces/system/configmaps/racer-runtime-revisions")
                .unwrap();
            assert_eq!(current["data"]["high-water"], "0");
            if let Some(checkpoint) = checkpoint {
                assert_eq!(checkpoint["metadata"]["uid"], current["metadata"]["uid"]);
            }
            assert_eq!(
                fixture
                    .objects
                    .lock()
                    .unwrap()
                    .values
                    .keys()
                    .filter(|p| p.contains("/configmaps/"))
                    .count(),
                2
            );
            stop.cancel();
        }
    }
    Ok(())
}

#[tokio::test]
async fn no_op_issuance_commit_is_fenced_during_takeover_and_retirement() -> Result<()> {
    let (fixture, client, stop) = Fixture::start().await?;
    let lease_path = format!("/apis/coordination.k8s.io/v1/namespaces/system/leases/{LEASE}");
    let renew =
        time::OffsetDateTime::now_utc().format(&time::format_description::well_known::Rfc3339)?;
    fixture.put(&lease_path, json!({"apiVersion":"coordination.k8s.io/v1","kind":"Lease","metadata":{"name":LEASE,"namespace":"system","uid":"lease"},"spec":{"holderIdentity":"first","renewTime":renew,"leaseDurationSeconds":15}}));
    let options = SecurityOptions {
        leaf_lifetime: 100,
        clock_skew: 1,
        proof_lifetime: 1,
        ..SecurityOptions::new("system")
    };
    let term = Leadership::new("first".into())?;
    let store = KubernetesCaStore::new(client.clone(), "system".into(), term.clone());
    let first =
        Arc::new(CaManager::acquire(store.clone(), term, options.clone(), unix_now()).await?);
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
    let now = unix_now();
    let issued = first
        .issue(&key.csr_pem, identity.clone(), false, now)
        .await?;
    let rv = store.read().await?.resource_version;
    first
        .issue(&key.csr_pem, identity.clone(), false, now)
        .await?;
    assert_eq!(
        store.read().await?.resource_version,
        rv,
        "unchanged watermark must exercise no-op commit"
    );
    let pause = Arc::new(RoutePause::default());
    // load, base read, then the no-op branch's fence check.
    fixture.objects.lock().unwrap().secret_read_pause = Some((2, pause.clone()));
    let task = tokio::spawn(async move { first.issue(&key.csr_pem, identity, false, now).await });
    tokio::time::timeout(Duration::from_secs(5), pause.captured.notified()).await?;
    let mut lease = fixture.get(&lease_path).unwrap();
    lease["spec"]["holderIdentity"] = "second".into();
    fixture.put(&lease_path, lease);
    let term = Leadership::new("second".into())?;
    let second = CaManager::acquire(
        KubernetesCaStore::new(client, "system".into(), term.clone()),
        term,
        options,
        now,
    )
    .await?;
    second.begin_rotation(now).await?;
    let at = second.state().await?.published_at().unwrap();
    assert_eq!(second.advance_rotation(at + 2).await?, Phase::Switched);
    assert_eq!(
        second.advance_rotation(issued.not_after).await?,
        Phase::Switched
    );
    assert_eq!(
        second.advance_rotation(issued.not_after + 1).await?,
        Phase::Stable
    );
    pause.resume.notify_one();
    assert!(
        task.await?.is_err(),
        "no-op commit cannot skip the durable fence"
    );
    stop.cancel();
    Ok(())
}

#[tokio::test]
async fn runnable_service_replaces_enrolled_pod_after_site_change() -> Result<()> {
    service_replacement_scenario("site").await
}

#[tokio::test]
async fn runnable_service_replaces_enrolled_pod_after_node_recreation() -> Result<()> {
    service_replacement_scenario("node").await
}

async fn service_replacement_scenario(change: &str) -> Result<()> {
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
        overlap_delay: Duration::from_secs(2),
        clock_skew: Duration::from_secs(1),
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
    // Admission uses direct live authorization; no participant census is needed.
    // Hold replica discovery while enrollment completes, then explicitly observe
    // subsequent passes before checking signed-claim request authorization.
    let pause = Arc::new(ListPause::default());
    *fixture.pod_list_pause.lock().unwrap() = Some(pause.clone());
    tokio::time::timeout(Duration::from_secs(10), pause.captured.notified())
        .await
        .context("replica discovery capture deadline")?;
    let worker_path = "/api/v1/namespaces/system/pods/worker";
    let key = generate_local_key()?;
    let body = serde_json::to_string(
        &json!({"csr":String::from_utf8(key.csr_pem.clone())?,"pod_namespace":"system","pod_name":"worker", "expected_universe":racer_identity("universe", "edge"), "expected_node":racer_identity("node", "node")}),
    )?;
    use base64::Engine;
    let token = format!(
        "fixture.{}.signature",
        base64::engine::general_purpose::URL_SAFE_NO_PAD
            .encode(format!("{{\"exp\":{}}}", unix_now() + 3600))
    );
    let boot = "a".repeat(64);
    let enrollment = format!(
        "POST /v1/enroll HTTP/1.1\r\nHost: racer-controlplane.system.svc\r\nAuthorization: Bearer {token}\r\nX-Racer-Boot: {boot}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
        body.len()
    );
    // A stale init identity must not receive a leaf for the Node's new identity.
    // Only an authenticated live singleton Pod can trigger guarded replacement.
    let stale = enrollment.replace(&racer_identity("universe", "edge"), &"0".repeat(64));
    let unauthorized = stale.replace(&token, "invalid-token");
    let denied = request(
        &options.enroll_listen,
        &unauthorized,
        Some(tls_config(&bundle, None)),
    )
    .await?;
    assert!(!denied.starts_with(b"HTTP/1.1 200"));
    let worker = fixture.get(worker_path).unwrap();
    let denied = request(
        &options.enroll_listen,
        &stale,
        Some(tls_config(&bundle, None)),
    )
    .await?;
    assert!(!denied.starts_with(b"HTTP/1.1 200"));
    assert!(
        fixture.get(worker_path).is_none(),
        "stale bootstrap Pod must be replaced before first admission"
    );
    fixture.put(worker_path, worker);
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
    // immediate repeat, independently of discovery sweeps and prior reviews.
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
    // Each capture is at the next unfiltered Pod list in replica reconciliation.
    // Two more captures prove the withheld and subsequent live passes finished.
    // Neither can revoke a valid signed identity through participant history.
    let live_sweep = Arc::new(ListPause::default());
    *fixture.pod_list_pause.lock().unwrap() = Some(live_sweep.clone());
    pause.resume.notify_one();
    tokio::time::timeout(Duration::from_secs(10), live_sweep.captured.notified())
        .await
        .context("withheld discovery sweep completion deadline")?;
    let next_sweep = Arc::new(ListPause::default());
    *fixture.pod_list_pause.lock().unwrap() = Some(next_sweep.clone());
    live_sweep.resume.notify_one();
    tokio::time::timeout(Duration::from_secs(10), next_sweep.captured.notified())
        .await
        .context("live discovery sweep completion deadline")?;
    let response: Value = serde_json::from_slice(&response[start..])?;
    let config = tls_config(
        &bundle,
        Some((response["certificate"].as_str().unwrap(), &key)),
    );
    let control = request(&options.listen, &format!("GET /v1/config HTTP/1.1\r\nHost: racer-controlplane.system.svc\r\nX-Racer-Boot: {boot}\r\nX-Racer-Profile: 1\r\nConnection: close\r\n\r\n"), Some(config)).await?;
    ensure!(
        control.starts_with(b"HTTP/1.1 200"),
        "control failed: {}",
        String::from_utf8_lossy(&control)
    );
    next_sweep.resume.notify_one();
    let wrong_boot = request(&options.listen, &format!("GET /v1/config HTTP/1.1\r\nHost: racer-controlplane.system.svc\r\nX-Racer-Boot: {}\r\nX-Racer-Profile: 1\r\nConnection: close\r\n\r\n", "f".repeat(64)), Some(tls_config(&bundle, Some((response["certificate"].as_str().unwrap(), &key))))).await?;
    assert!(
        wrong_boot.starts_with(b"HTTP/1.1 403"),
        "header cannot replace signed boot"
    );
    let proof = request(&options.trust_proof_listen, &format!("POST /v1/proof HTTP/1.1\r\nHost: racer-controlplane.system.svc\r\nX-Racer-Boot: {boot}\r\nX-Racer-Trust-Generation: {}\r\nX-Racer-Trust-Digest: {}\r\nX-Racer-Certificate-Issuer: {}\r\nX-Racer-Old-Connections: 0\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", bundle.generation, bundle.digest(), response["issuer"].as_str().unwrap()), Some(tls_config(&bundle, Some((response["certificate"].as_str().unwrap(), &key))))).await?;
    ensure!(
        proof.starts_with(b"HTTP/1.1 204"),
        "proof failed: {}",
        String::from_utf8_lossy(&proof)
    );
    // A caller-provided identity header cannot substitute for a client certificate.
    assert!(request(&options.listen, "GET /v1/config HTTP/1.1\r\nHost: x\r\nX-Racer-Pod: worker-pod\r\nConnection: close\r\n\r\n", Some(tls_config(&bundle, None))).await.is_err());
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
        "POST /v1/proof HTTP/1.1\r\nHost: racer-controlplane.system.svc\r\nX-Racer-Boot: {boot}\r\nX-Racer-Trust-Generation: {}\r\nX-Racer-Trust-Digest: {}\r\nX-Racer-Certificate-Issuer: {}\r\nX-Racer-Old-Connections: 0\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
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
    // A second boot is independently signed. No boot history replaces live Pods.
    let second_boot = "b".repeat(64);
    if change == "boot" {
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
        assert!(fixture.get(worker_path).is_some());
    } else {
        let mut node = fixture.get("/api/v1/nodes/worker").unwrap();
        if change == "site" {
            fixture.put("/apis/unbounded-cloud.io/v1alpha3/sites/other", json!({"apiVersion":"unbounded-cloud.io/v1alpha3","kind":"Site","metadata":{"name":"other","uid":"other-site"},"spec":{"components":{"racer":{"enabled":false}}}}));
            node["metadata"]["labels"]["unbounded-cloud.io/site"] = json!("other");
        } else {
            node["metadata"]["uid"] = json!("replacement-node");
        }
        fixture.put("/api/v1/nodes/worker", node);
        let denied = request(
            &options.enroll_listen,
            &enrollment,
            Some(tls_config(&switched, None)),
        )
        .await?;
        assert!(
            !denied.starts_with(b"HTTP/1.1 200"),
            "historical identities cannot renew"
        );
        assert!(
            fixture.get(worker_path).is_none(),
            "stale bootstrap Pod replaced at live authorization"
        );
    }
    let secret = fixture
        .get("/api/v1/namespaces/system/secrets/racer-ca")
        .unwrap();
    let metadata = base64::engine::general_purpose::STANDARD
        .decode(secret["data"]["state.json"].as_str().unwrap())
        .unwrap();
    assert!(metadata.len() < 16384);
    let state = CaState::from_image(&StateImage { metadata }).unwrap();
    let certificate =
        openssl::x509::X509::from_pem(response["certificate"].as_str().unwrap().as_bytes())
            .unwrap();
    let der = certificate.to_der().unwrap();
    let (_, certificate) = x509_parser::parse_x509_certificate(&der).unwrap();
    assert!(
        state.expiry_watermarks().any(|(issuer, expiry)| issuer
            == response["issuer"].as_str().unwrap()
            && expiry >= certificate.validity().not_after.timestamp()),
        "retirement must preserve issued expiry watermark"
    );
    assert!(
        !fixture
            .objects
            .lock()
            .unwrap()
            .values
            .keys()
            .any(|p| p.contains("/configmaps/racer-pki-")
                || p.contains("/configmaps/racer-replica-"))
    );
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

const CONTROLLER_PATH: &str = "/api/v1/namespaces/system/pods/controller";
const SERVING_LABEL: &str = "racer.unbounded-cloud.io/serving-leader";
const ROUTING_BOOT: &str = "racer.unbounded-cloud.io/routing-boot";
const PKI_REQUEST: &str = "racer.unbounded-cloud.io/pki-request";
const PKI_RESPONSE: &str = "racer.unbounded-cloud.io/pki-response";
const TRUST_PATH: &str = "/api/v1/namespaces/system/configmaps/racer-trust";

fn replica_response(fixture: &Fixture, path: &str) -> Option<Value> {
    serde_json::from_str(fixture.get(path)?["metadata"]["annotations"][PKI_RESPONSE].as_str()?).ok()
}

fn attach_replica_request(pod: &mut Value, key: &LocalKey) {
    pod["metadata"]["annotations"][PKI_REQUEST] = serde_json::to_string(&json!({
        "pod_uid": pod["metadata"]["uid"], "boot": "a".repeat(64),
        "csr": String::from_utf8(key.csr_pem.clone()).unwrap()
    }))
    .unwrap()
    .into();
}

async fn participant_state(client: &Client) -> Result<CaState> {
    let store = KubernetesCaStore::new(
        client.clone(),
        "system".into(),
        Leadership::new("test-observer".into())?,
    );
    CaState::from_image(&store.read().await?.image.context("missing CA state")?)
}

fn replica_pod(fixture: &Fixture, name: &str) -> Value {
    let mut pod = fixture.get(CONTROLLER_PATH).unwrap();
    pod["metadata"]["name"] = name.into();
    pod["metadata"]["uid"] = format!("{name}-pod").into();
    pod["metadata"]["annotations"] = json!({});
    pod["metadata"]["labels"] = json!({"racer.unbounded-cloud.io/component":"racer-controlplane"});
    pod
}

fn request_rotation(fixture: &Fixture) {
    let mut trust = fixture.get(TRUST_PATH).unwrap();
    trust["metadata"]["annotations"]["racer.unbounded-cloud.io/rotate-ca"] =
        "participant-isolation".into();
    fixture.put(TRUST_PATH, trust);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn bad_replica_owners_do_not_block_healthy_startup_or_renewal() -> Result<()> {
    let (fixture, client, api_stop) = Fixture::start().await?;
    fixture.seed();
    // Put invalid Pods on both sides of the healthy Pod in the namespace list.
    let bad_names = ["a-orphan", "b-forged", "y-orphan", "z-forged"];
    let key = generate_local_key()?;
    for (index, name) in bad_names.iter().enumerate() {
        let mut pod = replica_pod(&fixture, name);
        match index {
            0 => pod["metadata"]["ownerReferences"][0]["name"] = "missing".into(),
            1 => pod["metadata"]["ownerReferences"][0]["uid"] = "forged".into(),
            _ => {
                let mut rs = fixture
                    .get("/apis/apps/v1/namespaces/system/replicasets/controllers")
                    .unwrap();
                rs["metadata"]["name"] = (*name).into();
                if index == 2 {
                    rs["metadata"]["ownerReferences"][0]["name"] = "missing".into();
                } else {
                    rs["metadata"]["ownerReferences"][0]["uid"] = "forged".into();
                }
                fixture.put(
                    &format!("/apis/apps/v1/namespaces/system/replicasets/{name}"),
                    rs,
                );
                pod["metadata"]["ownerReferences"][0]["name"] = (*name).into();
            }
        }
        attach_replica_request(&mut pod, &key);
        fixture.put(&format!("/api/v1/namespaces/system/pods/{name}"), pod);
    }
    let options = Options {
        leaf_lifetime: Duration::from_secs(60),
        clock_skew: Duration::from_secs(1),
        overlap_delay: Duration::from_secs(1),
        ..route_options().await
    };
    let stop = CancellationToken::new();
    let run = tokio::spawn(service::run(client.clone(), options.clone(), stop.clone()));
    wait_for_route(&fixture, &options, Duration::from_secs(30)).await?;
    let original = replica_response(&fixture, CONTROLLER_PATH).unwrap();
    let initial = participant_state(&client).await?.bundle();
    request_rotation(&fixture);
    // Exercise the actual expiry-based renewal window, with all bad Pods present.
    tokio::time::timeout(Duration::from_secs(60), async {
        loop {
            let current = replica_response(&fixture, CONTROLLER_PATH).unwrap();
            if current["certificate"] != original["certificate"]
                && current["proof_certificate"] != original["proof_certificate"]
            {
                assert_eq!(current["boot"], original["boot"]);
                assert_eq!(current["csr_digest"], original["csr_digest"]);
                break;
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
    })
    .await
    .context("healthy replica renewal stalled behind bad owners")?;
    let state = participant_state(&client).await?;
    assert_ne!(
        state.bundle().active,
        initial.active,
        "bad owners cannot block timed rotation"
    );
    assert!(state.to_image()?.metadata.len() <= 16384);
    for name in bad_names {
        assert!(
            replica_response(&fixture, &format!("/api/v1/namespaces/system/pods/{name}")).is_none()
        );
    }
    wait_for_route(&fixture, &options, Duration::from_secs(5)).await?;
    stop.cancel();
    run.await??;
    api_stop.cancel();
    Ok(())
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn stalled_replica_owners_do_not_starve_later_issuance_renewal_or_initial_route() -> Result<()>
{
    let (fixture, client, api_stop) = Fixture::start().await?;
    fixture.seed();
    let release = CancellationToken::new();
    let key = generate_local_key()?;
    // More stalled entries than the concurrency limit, sorted before both
    // healthy replicas. Every retry stalls; only owner GETs are held.
    for index in 0..9 {
        let name = format!("a-stalled-{index}");
        let mut pod = replica_pod(&fixture, &name);
        attach_replica_request(&mut pod, &key);
        pod["metadata"]["ownerReferences"][0]["name"] = name.clone().into();
        fixture.objects.lock().unwrap().stalled_gets.insert(
            format!("/apis/apps/v1/namespaces/system/replicasets/{name}"),
            release.clone(),
        );
        fixture.put(&format!("/api/v1/namespaces/system/pods/{name}"), pod);
    }
    let healthy_path = "/api/v1/namespaces/system/pods/z-healthy";
    let mut healthy = replica_pod(&fixture, "z-healthy");
    attach_replica_request(&mut healthy, &key);
    fixture.put(healthy_path, healthy);
    let options = Options {
        leaf_lifetime: Duration::from_secs(30),
        clock_skew: Duration::from_secs(1),
        ..route_options().await
    };
    let stop = CancellationToken::new();
    let run = tokio::spawn(service::run(client, options.clone(), stop.clone()));
    let result: Result<()> = async {
        wait_for_route(&fixture, &options, Duration::from_secs(25))
            .await
            .context("initial route starved by earlier owner GETs")?;
        let original =
            replica_response(&fixture, healthy_path).context("later healthy replica not issued")?;
        let controller = replica_response(&fixture, CONTROLLER_PATH).unwrap();
        let lease_path = format!("/apis/coordination.k8s.io/v1/namespaces/system/leases/{LEASE}");
        let lease = fixture.get(&lease_path).unwrap();
        tokio::time::timeout(Duration::from_secs(40), async {
            loop {
                let renewed = replica_response(&fixture, healthy_path).unwrap();
                let local = replica_response(&fixture, CONTROLLER_PATH).unwrap();
                if renewed["certificate"] != original["certificate"]
                    && local["certificate"] != controller["certificate"]
                {
                    assert_eq!(renewed["boot"], original["boot"]);
                    assert_eq!(renewed["csr_digest"], original["csr_digest"]);
                    break;
                }
                tokio::time::sleep(Duration::from_millis(100)).await;
            }
        })
        .await
        .context("healthy renewal starved by repeated owner GET stalls")?;
        let renewed_lease = fixture.get(&lease_path).unwrap();
        assert_eq!(
            renewed_lease["spec"]["holderIdentity"],
            lease["spec"]["holderIdentity"]
        );
        assert_ne!(
            renewed_lease["metadata"]["resourceVersion"],
            lease["metadata"]["resourceVersion"]
        );
        assert!(fixture.objects.lock().unwrap().stalled_get_attempts > 9);
        assert!(
            !release.is_cancelled(),
            "owner requests must stay stalled through renewal"
        );
        wait_for_route(&fixture, &options, Duration::from_secs(10)).await?;
        Ok(())
    }
    .await;
    stop.cancel();
    release.cancel();
    run.await??;
    api_stop.cancel();
    result
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn ownership_lookup_failure_preserves_pending_members_and_blocks_rotation() -> Result<()> {
    let (fixture, client, api_stop) = Fixture::start().await?;
    fixture.seed();
    let rs_path = "/apis/apps/v1/namespaces/system/replicasets/standbys";
    let mut rs = fixture
        .get("/apis/apps/v1/namespaces/system/replicasets/controllers")
        .unwrap();
    rs["metadata"]["name"] = "standbys".into();
    fixture.put(rs_path, rs);
    let mut pending = replica_pod(&fixture, "a-pending");
    let key = generate_local_key()?;
    attach_replica_request(&mut pending, &key);
    pending["metadata"]["ownerReferences"][0]["name"] = "standbys".into();
    let pending_path = "/api/v1/namespaces/system/pods/a-pending";
    fixture.put(pending_path, pending);
    let options = Options {
        overlap_delay: Duration::from_secs(1),
        clock_skew: Duration::from_secs(1),
        ..route_options().await
    };
    let stop = CancellationToken::new();
    let run = tokio::spawn(service::run(client.clone(), options.clone(), stop.clone()));
    wait_for_route(&fixture, &options, Duration::from_secs(30)).await?;
    let original = replica_response(&fixture, pending_path).context("standby not issued")?;

    // The pending Pod remains live but its ownership cannot currently be read.
    // A second Pod using that owner has never been authorized at all.
    fixture
        .objects
        .lock()
        .unwrap()
        .get_errors
        .insert(rs_path.into(), StatusCode::SERVICE_UNAVAILABLE);
    let mut unknown = replica_pod(&fixture, "b-unknown");
    attach_replica_request(&mut unknown, &key);
    unknown["metadata"]["ownerReferences"][0]["name"] = "standbys".into();
    let unknown_path = "/api/v1/namespaces/system/pods/b-unknown";
    fixture.put(unknown_path, unknown);
    request_rotation(&fixture);
    let initial = participant_state(&client).await?.bundle();
    // Force reissuance of the healthy replica while the lookup failure persists.
    let mut request = fixture.get(CONTROLLER_PATH).unwrap();
    request["metadata"]["annotations"]
        .as_object_mut()
        .unwrap()
        .remove(PKI_RESPONSE);
    fixture.put(CONTROLLER_PATH, request);
    tokio::time::timeout(Duration::from_secs(15), async {
        loop {
            if replica_response(&fixture, CONTROLLER_PATH).is_some() {
                break;
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
    })
    .await
    .context("healthy issuance stalled behind unavailable owner")?;
    assert_eq!(replica_response(&fixture, pending_path).unwrap(), original);
    assert!(replica_response(&fixture, unknown_path).is_none());
    tokio::time::timeout(Duration::from_secs(15), async {
        loop {
            let state = participant_state(&client).await?;
            if state.bundle().active != initial.active {
                assert!(state.to_image()?.metadata.len() <= 16384);
                break Ok::<_, anyhow::Error>(());
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
    })
    .await
    .context("lookup failure blocked timed rotation")??;
    fixture.objects.lock().unwrap().get_errors.remove(rs_path);
    tokio::time::timeout(Duration::from_secs(20), async {
        loop {
            if replica_response(&fixture, unknown_path).is_some()
                && replica_response(&fixture, pending_path).is_some_and(|r| r != original)
            {
                break Ok::<_, anyhow::Error>(());
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
    })
    .await
    .context("ownership lookup did not recover")??;
    stop.cancel();
    run.await??;
    api_stop.cancel();
    Ok(())
}

async fn route_options() -> Options {
    Options {
        namespace: "system".into(),
        pod_name: "controller".into(),
        pod_uid: "controller-pod".into(),
        listen: free_address().await,
        enroll_listen: free_address().await,
        health_listen: free_address().await,
        replica_proof_listen: free_address().await,
        trust_proof_listen: free_address().await,
        ..Default::default()
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn expired_controlplane_leaf_recovers_from_pod_csr_without_tls_bootstrap() -> Result<()> {
    let (fixture, client, api_stop) = Fixture::start().await?;
    fixture.seed();
    let options = Options {
        leaf_lifetime: Duration::from_secs(4),
        clock_skew: Duration::from_secs(1),
        ..route_options().await
    };
    let stop = CancellationToken::new();
    let run = tokio::spawn(service::run(client, options.clone(), stop.clone()));
    wait_for_route(&fixture, &options, Duration::from_secs(30)).await?;
    let old = replica_response(&fixture, CONTROLLER_PATH).unwrap();
    let rs = "/apis/apps/v1/namespaces/system/replicasets/controllers";
    fixture
        .objects
        .lock()
        .unwrap()
        .get_errors
        .insert(rs.into(), StatusCode::SERVICE_UNAVAILABLE);
    let cert = openssl::x509::X509::from_pem(old["certificate"].as_str().unwrap().as_bytes())?;
    let der = cert.to_der()?;
    let (_, parsed) = x509_parser::parse_x509_certificate(&der).unwrap();
    let expiry = parsed.validity().not_after.timestamp();
    while unix_now() <= expiry {
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
    let health = request(
        &options.health_listen,
        "GET /readyz HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n",
        None,
    )
    .await?;
    assert!(health.starts_with(b"HTTP/1.1 503"));
    fixture.objects.lock().unwrap().get_errors.remove(rs);
    tokio::time::timeout(Duration::from_secs(15), async {
        loop {
            if replica_response(&fixture, CONTROLLER_PATH)
                .is_some_and(|r| r["certificate"] != old["certificate"])
                && request(
                    &options.health_listen,
                    "GET /readyz HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n",
                    None,
                )
                .await?
                .starts_with(b"HTTP/1.1 200")
            {
                break Ok::<_, anyhow::Error>(());
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
    })
    .await??;
    let current = replica_response(&fixture, CONTROLLER_PATH).unwrap();
    assert_eq!(current["boot"], old["boot"]);
    assert_eq!(current["csr_digest"], old["csr_digest"]);
    stop.cancel();
    run.await??;
    api_stop.cancel();
    Ok(())
}

async fn enrollment_status(fixture: &Fixture, options: &Options) -> Result<Vec<u8>> {
    let trust = fixture
        .get("/api/v1/namespaces/system/configmaps/racer-trust")
        .context("missing trust")?;
    let bundle = TrustBundle::parse(trust["data"]["bundle.json"].as_str().unwrap().as_bytes())?;
    // An empty request reaches token validation only when request serving is enabled.
    request(
        &options.enroll_listen,
        "POST /v1/enroll HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
        Some(tls_config(&bundle, None)),
    )
    .await
}

async fn wait_for_route(fixture: &Fixture, options: &Options, deadline: Duration) -> Result<()> {
    tokio::time::timeout(deadline, async {
        loop {
            if fixture
                .get(CONTROLLER_PATH)
                .is_some_and(|pod| pod["metadata"]["labels"][SERVING_LABEL] == "true")
                && enrollment_status(fixture, options)
                    .await
                    .is_ok_and(|r| r.starts_with(b"HTTP/1.1 403"))
            {
                break;
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
    })
    .await
    .with_context(|| {
        let objects = fixture.objects.lock().unwrap();
        let in_flight: Vec<_> = objects.in_flight.values()
            .map(|(request, started)| (request, started.elapsed()))
            .collect();
        let pod = objects.values.get(CONTROLLER_PATH).map(|pod| &pod["metadata"]);
        let lease_path = format!("/apis/coordination.k8s.io/v1/namespaces/system/leases/{LEASE}");
        let lease = objects.values.get(&lease_path);
        format!(
            "route publication deadline: requests={}, route_lists={}, route_patches={}, in_flight={in_flight:?}, pod={pod:?}, lease={lease:?}",
            objects.requests, objects.route_lists, objects.route_patches,
        )
    })
}

async fn route_publication_retry(committed: bool) -> Result<()> {
    let (fixture, client, api_stop) = Fixture::start().await?;
    fixture.seed();
    // A returned API error must also leave publication retryable.
    *fixture.route_fault.lock().unwrap() = Some(RouteFault::Fail);
    let options = route_options().await;
    let stop = CancellationToken::new();
    let run = tokio::spawn(service::run(client, options.clone(), stop.clone()));
    wait_for_route(&fixture, &options, Duration::from_secs(30)).await?;
    let counts = {
        let objects = fixture.objects.lock().unwrap();
        assert!(
            objects.route_lists >= 2,
            "failed publication was not retried"
        );
        (objects.route_lists, objects.route_patches)
    };
    tokio::time::sleep(Duration::from_secs(5)).await;
    {
        let objects = fixture.objects.lock().unwrap();
        assert_eq!(
            counts,
            (objects.route_lists, objects.route_patches),
            "healthy reconciliation must not sweep or rewrite Pods"
        );
    }

    let pause = Arc::new(RoutePause::default());
    *fixture.route_fault.lock().unwrap() = Some(RouteFault::Pause(pause.clone(), committed));
    let mut pod = fixture.get(CONTROLLER_PATH).unwrap();
    pod["metadata"]["labels"]
        .as_object_mut()
        .unwrap()
        .remove(SERVING_LABEL);
    fixture.put(CONTROLLER_PATH, pod);
    tokio::time::timeout(Duration::from_secs(10), pause.captured.notified()).await?;
    let rejected = enrollment_status(&fixture, &options).await;
    assert!(
        rejected
            .as_ref()
            .is_ok_and(|r| r.starts_with(b"HTTP/1.1 503"))
            || rejected.as_ref().is_err_and(|error| {
                error
                    .downcast_ref::<std::io::Error>()
                    .is_some_and(|error| error.kind() == std::io::ErrorKind::UnexpectedEof)
            }),
        "unpublished leader must reject or close enrollment: {rejected:?}"
    );
    // Exercise the actual 60-second outer reconciliation timeout. The Lease
    // keeps renewing, but cancellation must not suppress subsequent publication.
    wait_for_route(&fixture, &options, Duration::from_secs(75))
        .await
        .with_context(|| {
            format!(
                "route retry: committed={committed}, service_finished={}, paused_status={:?}",
                run.is_finished(),
                *pause.status.lock().unwrap()
            )
        })?;
    let published = fixture.get(CONTROLLER_PATH).unwrap();
    pause.resume.notify_one();
    tokio::time::timeout(Duration::from_secs(5), pause.completed.notified()).await?;
    assert_eq!(
        *pause.status.lock().unwrap(),
        Some(if committed {
            StatusCode::OK
        } else {
            StatusCode::CONFLICT
        }),
        "retry must fence a withheld old PATCH even when its Pod had no hint"
    );
    assert_eq!(fixture.get(CONTROLLER_PATH).unwrap(), published);
    stop.cancel();
    run.await??;
    api_stop.cancel();
    Ok(())
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn canceled_route_publication_retries_and_repairs_removed_label() -> Result<()> {
    route_publication_retry(false).await
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn canceled_committed_route_publication_retries() -> Result<()> {
    route_publication_retry(true).await
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn stale_route_publication_cannot_overwrite_successor_or_enable_serving() -> Result<()> {
    for committed in [false, true] {
        let (fixture, client, api_stop) = Fixture::start().await?;
        fixture.seed();
        let pause = Arc::new(RoutePause::default());
        *fixture.route_fault.lock().unwrap() = Some(RouteFault::Pause(pause.clone(), committed));
        let options = route_options().await;
        let stop = CancellationToken::new();
        let run = tokio::spawn(service::run(client, options.clone(), stop.clone()));
        tokio::time::timeout(Duration::from_secs(30), pause.captured.notified()).await?;
        let lease_path = format!("/apis/coordination.k8s.io/v1/namespaces/system/leases/{LEASE}");
        let mut lease = fixture.get(&lease_path).unwrap();
        lease["spec"]["holderIdentity"] = "successor".into();
        fixture.put(&lease_path, lease);
        // Let the election loop observe the new holder before returning the old
        // publication response. Model the successor's CAS sweep/publication.
        tokio::time::sleep(Duration::from_secs(3)).await;
        let mut successor = fixture.get(CONTROLLER_PATH).unwrap();
        successor["metadata"]["annotations"][ROUTING_BOOT] = "successor-boot".into();
        successor["metadata"]["labels"][SERVING_LABEL] = "true".into();
        fixture.put(CONTROLLER_PATH, successor);
        let successor = fixture.get(CONTROLLER_PATH).unwrap();
        pause.resume.notify_one();
        tokio::time::timeout(Duration::from_secs(5), pause.completed.notified()).await?;
        assert_eq!(
            *pause.status.lock().unwrap(),
            Some(if committed {
                StatusCode::OK
            } else {
                StatusCode::CONFLICT
            })
        );
        tokio::time::sleep(Duration::from_secs(3)).await;
        assert!(
            enrollment_status(&fixture, &options)
                .await?
                .starts_with(b"HTTP/1.1 503")
        );
        assert_eq!(fixture.get(CONTROLLER_PATH).unwrap(), successor);
        stop.cancel();
        run.await??;
        assert_eq!(
            fixture.get(CONTROLLER_PATH).unwrap(),
            successor,
            "old boot shutdown must not clear the successor's hint"
        );
        api_stop.cancel();
    }
    Ok(())
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn warm_standby_takes_over_without_regenerating_trust() -> Result<()> {
    let (fixture, client, api_stop) = Fixture::start().await?;
    fixture.seed();
    fixture.put(
        "/apis/racer.unbounded-cloud.io/v1alpha1/p2pcaches/catalog",
        json!({
            "apiVersion": "racer.unbounded-cloud.io/v1alpha1", "kind": "P2PCache",
            "metadata": {"name": "catalog", "uid": "catalog-uid", "generation": 1},
            "spec": {"cacheGeneration": 1, "maxCandidateAttempts": 3, "siteSelector": {}}
        }),
    );
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
        "POST /v1/enroll HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
        Some(tls_config(&bundle, None)),
    )
    .await?;
    assert!(
        rejected.starts_with(b"HTTP/1.1 503"),
        "warm follower must reject enrollment"
    );
    let catalog_request = format!(
        "GET /debug/product-catalog/{} HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n",
        racer_controlplane::model::identity("universe", "edge")
    );
    let standby_catalog = request(&second.health_listen, &catalog_request, None).await?;
    assert!(
        standby_catalog.starts_with(b"HTTP/1.1 503"),
        "ready warm standby has no authority to expose a published catalog"
    );
    let leader_catalog = request(&first.health_listen, &catalog_request, None).await?;
    assert!(leader_catalog.starts_with(b"HTTP/1.1 200"));
    let leader_catalog = String::from_utf8(leader_catalog)?;
    let (headers, catalog) = leader_catalog.split_once("\r\n\r\n").unwrap();
    assert!(headers.contains("content-type: application/json"));
    assert!(headers.contains("cache-control: no-store"));
    let catalog: Value = serde_json::from_str(catalog)?;
    assert_eq!(catalog["catalog"]["selectedPodUIDs"], json!(["worker-pod"]));
    assert_eq!(catalog["catalog"]["members"].as_array().unwrap().len(), 1);
    let lease_path = format!("/apis/coordination.k8s.io/v1/namespaces/system/leases/{LEASE}");
    let original_lease = fixture.get(&lease_path).unwrap();
    assert_eq!(original_lease["spec"]["leaseDurationSeconds"], 3);
    first_stop.cancel();
    first_run.await??;
    fixture.remove("/api/v1/namespaces/system/pods/controller");
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
        !fixture
            .objects
            .lock()
            .unwrap()
            .values
            .keys()
            .any(|p| p.contains("/configmaps/racer-pki-")
                || p.contains("/configmaps/racer-replica-"))
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
