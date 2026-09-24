// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Production process: direct Kubernetes authorization, Lease election, fenced
//! security, warm replica TLS, and the desired-state runtime's Axum Router.
use anyhow::{Context, Result, bail, ensure};
use axum::{
    Router,
    body::Body,
    extract::{DefaultBodyLimit, Path, State},
    http::{Request, StatusCode},
    response::{IntoResponse, Response},
    routing::{get, post},
};
use base64::Engine;
use k8s_openapi::{
    api::{
        apps::v1::{DaemonSet, Deployment, ReplicaSet},
        authentication::v1::{TokenReview, TokenReviewSpec},
        coordination::v1::{Lease, LeaseSpec},
        core::v1::{ConfigMap, Node, Pod, Service},
    },
    apimachinery::pkg::apis::meta::v1::{MicroTime, ObjectMeta},
};
use kube::{
    Api, Client, ResourceExt,
    api::{
        ApiResource, DeleteParams, DynamicObject, ListParams, Patch, PatchParams, PostParams,
        Preconditions,
    },
};
use serde::{Deserialize, Serialize};
use std::{
    collections::HashMap,
    net::{IpAddr, SocketAddr},
    sync::{
        Arc, RwLock,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    time::{Duration, Instant},
};
use tokio::{
    io::AsyncWriteExt,
    net::TcpListener,
    sync::{Mutex, Semaphore},
    task::JoinSet,
};
use tokio_util::sync::CancellationToken;

use crate::{
    kubernetes::{Runtime, RuntimeOptions, SecurityContext},
    security::{
        self,
        kubernetes::{BUNDLE_KEY, KubernetesCaStore, LEASE, TRUST_MAP},
        *,
    },
};

const COMPONENT: &str = "racer-controlplane";
const COMPONENT_LABEL: &str = "racer.unbounded-cloud.io/component";
const SERVING_LABEL: &str = "racer.unbounded-cloud.io/serving-leader";
const ROUTING_BOOT: &str = "racer.unbounded-cloud.io/routing-boot";

/// Programmatic election timing; the production CLI always uses these defaults.
#[derive(Clone, Copy, Debug)]
pub struct LeaseTiming {
    pub duration_seconds: i32,
    pub renew_deadline: Duration,
    pub retry_period: Duration,
}

impl Default for LeaseTiming {
    fn default() -> Self {
        Self {
            duration_seconds: 15,
            renew_deadline: Duration::from_secs(10),
            retry_period: Duration::from_secs(2),
        }
    }
}

impl LeaseTiming {
    fn validate(self) -> Result<()> {
        ensure!(
            self.duration_seconds > 0
                && self.retry_period > Duration::ZERO
                && self.retry_period < self.renew_deadline
                && self.renew_deadline < Duration::from_secs(self.duration_seconds as u64),
            "require 0 < lease retry period < renewal deadline < lease duration"
        );
        Ok(())
    }
}

#[derive(Clone, Debug)]
pub struct Options {
    pub listen: String,
    pub enroll_listen: String,
    pub health_listen: String,
    pub replica_proof_listen: String,
    pub trust_proof_listen: String,
    pub namespace: String,
    pub socket_root: String,
    pub review_qps: f64,
    pub review_burst: usize,
    pub rotation_interval: Duration,
    pub leaf_lifetime: Duration,
    pub clock_skew: Duration,
    pub lease_timing: LeaseTiming,
    pub overlap_delay: Duration,
    pub pod_name: String,
    pub pod_uid: String,
    pub bootstrap_node: String,
    pub bootstrap_namespace: String,
    pub bootstrap_service: String,
    pub bootstrap_port: String,
    pub version: bool,
}

impl Default for Options {
    fn default() -> Self {
        Self {
            listen: ":8443".into(),
            enroll_listen: ":8444".into(),
            health_listen: ":8081".into(),
            replica_proof_listen: ":8445".into(),
            trust_proof_listen: ":8446".into(),
            namespace: "racer-system".into(),
            socket_root: "/dev/racer".into(),
            review_qps: 20.,
            review_burst: 30,
            rotation_interval: Duration::from_secs(30 * 86400),
            leaf_lifetime: Duration::from_secs(86400),
            clock_skew: Duration::from_secs(300),
            lease_timing: LeaseTiming::default(),
            overlap_delay: Duration::from_secs(300),
            pod_name: std::env::var("RACER_POD_NAME").unwrap_or_default(),
            pod_uid: std::env::var("RACER_POD_UID").unwrap_or_default(),
            bootstrap_node: String::new(),
            bootstrap_namespace: "racer-system".into(),
            bootstrap_service: COMPONENT.into(),
            bootstrap_port: "8443".into(),
            version: false,
        }
    }
}

impl Options {
    /// Accept Go operator flags (-listen=x) and conventional --listen x.
    pub fn parse(args: impl IntoIterator<Item = String>) -> Result<Self> {
        let mut options = Self::default();
        let mut args = args.into_iter();
        while let Some(arg) = args.next() {
            if arg == "version" || arg == "--version" || arg == "-version" {
                options.version = true;
                continue;
            }
            ensure!(arg.starts_with('-'), "unexpected positional argument {arg}");
            let (key, inline) = arg
                .trim_start_matches('-')
                .split_once('=')
                .map_or((arg.trim_start_matches('-'), None), |(k, v)| {
                    (k, Some(v.to_owned()))
                });
            let value = inline
                .or_else(|| args.next())
                .with_context(|| format!("missing value for {key}"))?;
            match key {
                "listen" => options.listen = value,
                "enroll-listen" => options.enroll_listen = value,
                "health-listen" => options.health_listen = value,
                "replica-proof-listen" => options.replica_proof_listen = value,
                "trust-proof-listen" => options.trust_proof_listen = value,
                "state-namespace" => options.namespace = value,
                "socket-root" => options.socket_root = value,
                "token-review-qps" => options.review_qps = value.parse()?,
                "token-review-burst" => options.review_burst = value.parse()?,
                "ca-rotation-interval" => options.rotation_interval = parse_duration(&value)?,
                "leaf-lifetime" => options.leaf_lifetime = parse_duration(&value)?,
                "clock-skew" => options.clock_skew = parse_duration(&value)?,
                "ca-overlap-delay" => options.overlap_delay = parse_duration(&value)?,
                "bootstrap-node" => options.bootstrap_node = value,
                "bootstrap-namespace" => options.bootstrap_namespace = value,
                "bootstrap-service" => options.bootstrap_service = value,
                "bootstrap-port" => options.bootstrap_port = value,
                "pod-name" => options.pod_name = value,
                "pod-uid" => options.pod_uid = value,
                "zap-log-level"
                | "zap-encoder"
                | "zap-time-encoding"
                | "zap-stacktrace-level"
                | "zap-devel" => (),
                _ => bail!("unknown flag {key}"),
            }
        }
        ensure!(
            options.review_qps.is_finite()
                && options.review_qps > 0.
                && options.review_burst > 0
                && options.review_burst <= 10000,
            "invalid TokenReview rate"
        );
        ensure!(
            options.rotation_interval > Duration::ZERO
                && options.rotation_interval < Duration::from_secs(360 * 86400),
            "invalid CA rotation interval"
        );
        crate::model::cache_sockets(&options.socket_root, &"a".repeat(63))?;
        ensure!(
            options.leaf_lifetime.as_secs() >= 60
                && options.leaf_lifetime.as_secs() < 360 * 86400
                && options.clock_skew.as_secs() >= 1
                && options.clock_skew.as_secs() <= 86400,
            "invalid leaf lifetime or clock skew"
        );
        ensure!(
            options.overlap_delay.as_secs() >= 1 && options.overlap_delay.as_secs() <= 86400,
            "invalid CA overlap delay"
        );
        Ok(options)
    }
}

fn parse_duration(value: &str) -> Result<Duration> {
    let mut total = 0f64;
    let mut rest = value;
    while !rest.is_empty() {
        let end = rest
            .find(|c: char| !c.is_ascii_digit() && c != '.')
            .context("duration requires units")?;
        ensure!(end > 0, "invalid duration");
        let number: f64 = rest[..end].parse()?;
        rest = &rest[end..];
        let (unit, factor) = if rest.starts_with("ms") {
            (2, 0.001)
        } else {
            (
                1,
                match rest.as_bytes()[0] {
                    b's' => 1.,
                    b'm' => 60.,
                    b'h' => 3600.,
                    b'd' => 86400.,
                    _ => bail!("unsupported duration unit"),
                },
            )
        };
        total += number * factor;
        rest = &rest[unit..];
    }
    ensure!(total.is_finite() && total > 0., "invalid duration");
    Ok(Duration::try_from_secs_f64(total)?)
}

pub fn version() -> String {
    format!(
        "racer-controlplane {} ({})",
        option_env!("RACER_VERSION").unwrap_or(env!("CARGO_PKG_VERSION")),
        option_env!("RACER_COMMIT").unwrap_or("development")
    )
}

fn address(value: &str) -> String {
    if value.starts_with(':') {
        format!("0.0.0.0{value}")
    } else {
        value.into()
    }
}

/// Both kubelet projection and direct API reads use this unchanged public key.
pub const TRUST_PROJECTION_PATH: &str = "/var/run/racer-trust/bundle.json";
fn now_micro() -> Result<MicroTime> {
    Ok(serde_json::from_value(serde_json::Value::String(
        time::OffsetDateTime::now_utc().format(&time::format_description::well_known::Rfc3339)?,
    ))?)
}

pub async fn bootstrap(client: Client, options: &Options, pod_ip: &str) -> Result<String> {
    let node = Api::<Node>::all(client.clone())
        .get(&options.bootstrap_node)
        .await?;
    let metadata = object_metadata(&node.metadata);
    let site = security::node_site(&metadata);
    ensure!(
        !metadata.uid.is_empty()
            && !metadata.deleting
            && !site.is_empty()
            && metadata
                .labels
                .get("racer.unbounded-cloud.io/exclude")
                .map(String::as_str)
                != Some("true")
            && metadata.labels.get("kubernetes.io/os").map(String::as_str) == Some("linux"),
        "Node not eligible for bootstrap universe"
    );
    let service = Api::<Service>::namespaced(client, &options.bootstrap_namespace)
        .get(&options.bootstrap_service)
        .await?;
    let spec = service.spec.context("Service has no spec")?;
    let pod_ip: IpAddr = pod_ip.parse()?;
    ensure!(
        !pod_ip.is_unspecified() && !pod_ip.is_loopback() && !pod_ip.is_multicast(),
        "POD_IP must be usable"
    );
    let ips = spec
        .cluster_ips
        .unwrap_or_else(|| spec.cluster_ip.into_iter().collect());
    let ip = ips
        .iter()
        .filter_map(|s| s.parse::<IpAddr>().ok())
        .find(|ip| ip.is_ipv4() == pod_ip.is_ipv4() && !ip.is_unspecified())
        .context("Service has no matching ClusterIP")?;
    let port = spec
        .ports
        .unwrap_or_default()
        .into_iter()
        .find(|p| {
            p.name.as_deref() == Some(&options.bootstrap_port)
                || p.port.to_string() == options.bootstrap_port
        })
        .context("Service port not found")?;
    ensure!(
        port.protocol.as_deref().unwrap_or("TCP") == "TCP" && (1..=65535).contains(&port.port),
        "invalid Service port"
    );
    Ok(format!(
        "export RACER_UNIVERSE={}\nexport RACER_NODE={}\nexport RACER_CONTROL_ADDRESS='{}'\n",
        racer_identity("universe", &security::universe_for_site(site)),
        racer_identity("node", &metadata.uid),
        SocketAddr::new(ip, port.port as u16)
    ))
}

pub fn object_metadata(metadata: &ObjectMeta) -> security::ObjectMetadata {
    security::ObjectMetadata {
        namespace: metadata.namespace.clone().unwrap_or_default(),
        name: metadata.name.clone().unwrap_or_default(),
        uid: metadata.uid.clone().unwrap_or_default(),
        deleting: metadata.deletion_timestamp.is_some(),
        labels: metadata.labels.clone().unwrap_or_default(),
        owners: metadata
            .owner_references
            .as_deref()
            .unwrap_or_default()
            .iter()
            .map(|o| security::OwnerReference {
                api_version: o.api_version.clone(),
                kind: o.kind.clone(),
                name: o.name.clone(),
                uid: o.uid.clone(),
                controller: o.controller == Some(true),
            })
            .collect(),
    }
}

pub fn pod_data(pod: &Pod) -> PodData {
    let spec = pod.spec.as_ref();
    let container = if spec.and_then(|s| s.service_account_name.as_deref()) == Some(COMPONENT) {
        "controller"
    } else {
        "dataplane"
    };
    PodData {
        metadata: object_metadata(&pod.metadata),
        service_account: spec
            .and_then(|s| s.service_account_name.clone())
            .unwrap_or_default(),
        node_name: spec.and_then(|s| s.node_name.clone()).unwrap_or_default(),
        running_container_id: pod
            .status
            .as_ref()
            .and_then(|s| s.container_statuses.as_ref())
            .and_then(|s| {
                s.iter().find(|c| {
                    c.name == container && c.state.as_ref().is_some_and(|s| s.running.is_some())
                })
            })
            .and_then(|s| s.container_id.clone())
            .unwrap_or_default(),
    }
}

fn daemon_data(daemon: &DaemonSet) -> WorkloadData {
    let template = daemon.spec.as_ref().map(|s| &s.template);
    WorkloadData {
        metadata: object_metadata(&daemon.metadata),
        template_service_account: template
            .and_then(|t| t.spec.as_ref())
            .and_then(|s| s.service_account_name.clone())
            .unwrap_or_default(),
        template_labels: template
            .and_then(|t| t.metadata.as_ref())
            .and_then(|m| m.labels.clone())
            .unwrap_or_default(),
    }
}

async fn replica_identity(
    client: Client,
    namespace: &str,
    pod: &Pod,
    boot: &str,
) -> Result<Identity> {
    let owner = pod
        .metadata
        .owner_references
        .as_deref()
        .unwrap_or_default()
        .iter()
        .find(|o| {
            o.controller == Some(true) && o.kind == "ReplicaSet" && o.api_version == "apps/v1"
        })
        .context("CP Pod lacks ReplicaSet owner")?;
    let rs = Api::<ReplicaSet>::namespaced(client.clone(), namespace)
        .get(&owner.name)
        .await?;
    let owner = rs
        .metadata
        .owner_references
        .as_deref()
        .unwrap_or_default()
        .iter()
        .find(|o| {
            o.controller == Some(true) && o.kind == "Deployment" && o.api_version == "apps/v1"
        })
        .context("CP ReplicaSet lacks Deployment owner")?;
    let deployment = Api::<Deployment>::namespaced(client, namespace)
        .get(&owner.name)
        .await?;
    let template = deployment.spec.as_ref().map(|s| &s.template);
    authorize_replica(
        namespace,
        &pod_data(pod),
        &WorkloadData {
            metadata: object_metadata(&rs.metadata),
            ..Default::default()
        },
        &WorkloadData {
            metadata: object_metadata(&deployment.metadata),
            template_service_account: template
                .and_then(|t| t.spec.as_ref())
                .and_then(|s| s.service_account_name.clone())
                .unwrap_or_default(),
            template_labels: template
                .and_then(|t| t.metadata.as_ref())
                .and_then(|m| m.labels.clone())
                .unwrap_or_default(),
        },
        boot,
    )
}

#[derive(Clone)]
struct Active {
    manager: Arc<CaManager<KubernetesCaStore>>,
    store: KubernetesCaStore,
    term: Leadership,
    state: Arc<CaState>,
    last_renewal: Instant,
}

struct Shared {
    client: Client,
    options: Options,
    boot: String,
    active: RwLock<Option<Active>>,
    hot: HotTls,
    local_key: LocalKey,
    installed_ack: RwLock<Option<ReplicaAcknowledgment>>,
    listeners_ready: AtomicBool,
    serving: AtomicBool,
    changed: tokio::sync::Notify,
    enrollment_capacity: Arc<Semaphore>,
    reviews: ReviewCache,
    refresh: Mutex<()>,
    replica_cursor: AtomicUsize,
    stop: CancellationToken,
}

impl Shared {
    fn active(&self) -> Option<Active> {
        self.active
            .read()
            .unwrap()
            .as_ref()
            .filter(|a| {
                a.term.is_active()
                    && a.last_renewal.elapsed() < self.options.lease_timing.renew_deadline
                    && !self.stop.is_cancelled()
            })
            .cloned()
    }
    fn security_context(&self) -> Option<SecurityContext> {
        self.active().map(|a| SecurityContext {
            fence: a.term.token().into(),
            state: a.state,
        })
    }
    fn serving(&self) -> bool {
        self.serving.load(Ordering::Acquire) && self.active().is_some()
    }
    fn lose_leadership(&self) {
        let mut current = self.active.write().unwrap();
        self.serving.store(false, Ordering::Release);
        if let Some(active) = current.take() {
            active.term.cancel();
        }
        self.changed.notify_waiters();
    }
    async fn refresh_state(&self, active: &Active) -> Result<()> {
        let _refresh = self.refresh.lock().await;
        let state = Arc::new(active.manager.committed_state()?);
        let mut current = self.active.write().unwrap();
        if let Some(current) = current
            .as_mut()
            .filter(|a| a.term.token() == active.term.token())
        {
            current.state = state;
        }
        Ok(())
    }
    fn maps(&self) -> Api<ConfigMap> {
        Api::namespaced(self.client.clone(), &self.options.namespace)
    }
    fn pods(&self) -> Api<Pod> {
        Api::namespaced(self.client.clone(), &self.options.namespace)
    }
}

struct ReviewEntry {
    review: TokenReviewResult,
    until: Instant,
    expiry: i64,
}
struct ReviewCache {
    entries: Mutex<HashMap<String, ReviewEntry>>,
    limiter: Mutex<(f64, Instant)>,
    qps: f64,
    burst: usize,
    flights: Vec<Mutex<()>>,
}

impl ReviewCache {
    fn new(qps: f64, burst: usize) -> Self {
        Self {
            entries: Mutex::new(HashMap::new()),
            limiter: Mutex::new((burst as f64, Instant::now())),
            qps,
            burst,
            flights: (0..256).map(|_| Mutex::new(())).collect(),
        }
    }
    async fn authenticate(&self, client: Client, token: &str) -> Result<TokenReviewResult> {
        self.authenticate_with_clock(client, token, || (Instant::now(), unix_now()))
            .await
    }

    // Only cache validity uses this clock. Network deadlines and rate limiting
    // continue to use real elapsed time, including in clock-controlled tests.
    async fn authenticate_with_clock(
        &self,
        client: Client,
        token: &str,
        now: impl Fn() -> (Instant, i64),
    ) -> Result<TokenReviewResult> {
        ensure!(
            !token.is_empty() && token.len() <= 16384,
            "invalid bearer token size"
        );
        let key = digest(token.as_bytes());
        let bucket = usize::from_str_radix(&key[..2], 16).expect("SHA-256 hex");
        let _flight = self.flights[bucket].lock().await;
        {
            let entries = self.entries.lock().await;
            let (instant, unix) = now();
            if let Some(entry) = entries
                .get(&key)
                .filter(|e| e.until > instant && e.expiry > unix)
            {
                return Ok(entry.review.clone());
            }
        }
        // The outer eight-request pipeline also bounds cache-miss work. Tokens
        // are never cached before successful TokenReview. JWT exp can only
        // shorten cache lifetime, never authenticate or extend it.
        loop {
            let wait = {
                let mut limit = self.limiter.lock().await;
                limit.0 =
                    (limit.0 + limit.1.elapsed().as_secs_f64() * self.qps).min(self.burst as f64);
                limit.1 = Instant::now();
                if limit.0 >= 1. {
                    limit.0 -= 1.;
                    None
                } else {
                    Some(Duration::from_secs_f64((1. - limit.0) / self.qps))
                }
            };
            match wait {
                None => break,
                Some(wait) => tokio::time::sleep(wait).await,
            }
        }
        let review = TokenReview {
            spec: TokenReviewSpec {
                token: Some(token.into()),
                audiences: Some(vec![CONTROL_AUDIENCE.into()]),
            },
            ..Default::default()
        };
        let review = tokio::time::timeout(
            Duration::from_secs(1),
            Api::<TokenReview>::all(client).create(&PostParams::default(), &review),
        )
        .await??;
        let status = review.status.context("TokenReview lacks status")?;
        let result = TokenReviewResult {
            authenticated: status.authenticated == Some(true),
            audiences: status.audiences.unwrap_or_default(),
            error: status.error.unwrap_or_default(),
            pod_uids: status
                .user
                .and_then(|u| u.extra)
                .and_then(|e| e.get("authentication.kubernetes.io/pod-uid").cloned())
                .unwrap_or_default(),
        };
        reviewed_pod_uid(&result)?;
        if let Some(expiry) = token
            .split('.')
            .nth(1)
            .and_then(|part| {
                base64::engine::general_purpose::URL_SAFE_NO_PAD
                    .decode(part)
                    .ok()
            })
            .and_then(|bytes| serde_json::from_slice::<serde_json::Value>(&bytes).ok())
            .and_then(|v| v["exp"].as_i64())
        {
            let (instant, unix) = now();
            ensure!(expiry > unix, "expired credential");
            let mut entries = self.entries.lock().await;
            entries.retain(|_, e| e.until > instant && e.expiry > unix);
            if entries.len() >= 32768
                && let Some(old) = entries.keys().next().cloned()
            {
                entries.remove(&old);
            }
            entries.insert(
                key,
                ReviewEntry {
                    review: result.clone(),
                    until: instant + Duration::from_secs(5),
                    expiry,
                },
            );
        }
        Ok(result)
    }
}

#[derive(Clone)]
struct HttpState {
    shared: Arc<Shared>,
    runtime: Arc<Runtime>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct EnrollmentRequest {
    csr: String,
    pod_namespace: String,
    pod_name: String,
    expected_universe: String,
    expected_node: String,
}
#[derive(Serialize)]
struct EnrollmentResponse {
    certificate: String,
    generation: u64,
    issuer: String,
}

#[derive(Debug, thiserror::Error)]
#[error("certificate issuance unavailable: {0}")]
struct IssuanceUnavailable(anyhow::Error);

async fn enroll(State(state): State<HttpState>, request: Request<Body>) -> Response {
    if !state.shared.serving() {
        return StatusCode::SERVICE_UNAVAILABLE.into_response();
    }
    let Ok(_permit) = state.shared.enrollment_capacity.clone().try_acquire_owned() else {
        return (
            StatusCode::SERVICE_UNAVAILABLE,
            [("Retry-After", "1")],
            "enrollment busy",
        )
            .into_response();
    };
    let result = tokio::time::timeout(Duration::from_secs(4), enroll_inner(&state, request)).await;
    let mut response = match result {
        Ok(Ok(body)) => axum::Json(body).into_response(),
        Ok(Err(error)) => {
            tracing::debug!(%error, "enrollment rejected");
            if error.downcast_ref::<kube::Error>().is_some()
                || error
                    .downcast_ref::<tokio::time::error::Elapsed>()
                    .is_some()
                || error.downcast_ref::<IssuanceUnavailable>().is_some()
            {
                StatusCode::SERVICE_UNAVAILABLE.into_response()
            } else {
                StatusCode::FORBIDDEN.into_response()
            }
        }
        Err(_) => StatusCode::SERVICE_UNAVAILABLE.into_response(),
    };
    response
        .headers_mut()
        .insert("cache-control", "no-store".parse().unwrap());
    response
}

async fn enroll_inner(state: &HttpState, request: Request<Body>) -> Result<EnrollmentResponse> {
    let active = state.shared.active().context("not leader")?;
    let token = request
        .headers()
        .get("authorization")
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.strip_prefix("Bearer "))
        .context("Pod token required")?
        .to_owned();
    let boot = request
        .headers()
        .get("x-racer-boot")
        .and_then(|v| v.to_str().ok())
        .context("boot required")?
        .to_owned();
    ensure!(
        boot.len() == 64
            && boot
                .bytes()
                .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b)),
        "invalid boot"
    );
    let bytes = axum::body::to_bytes(request.into_body(), 32768).await?;
    let body: EnrollmentRequest = serde_json::from_slice(&bytes)?;
    ensure!(
        body.pod_namespace == state.shared.options.namespace && !body.pod_name.is_empty(),
        "wrong Pod namespace"
    );
    let review = state
        .shared
        .reviews
        .authenticate(state.shared.client.clone(), &token)
        .await?;
    let pod = state.shared.pods().get(&body.pod_name).await?;
    let pod_data = pod_data(&pod);
    let identity = {
        let owner = pod
            .metadata
            .owner_references
            .as_deref()
            .unwrap_or_default()
            .iter()
            .find(|o| {
                o.controller == Some(true) && o.kind == "DaemonSet" && o.api_version == "apps/v1"
            })
            .context("Pod not DaemonSet owned")?;
        let daemon = Api::<DaemonSet>::namespaced(state.shared.client.clone(), &body.pod_namespace)
            .get(&owner.name)
            .await?;
        security::authorize_managed_dataplane(
            &body.pod_namespace,
            &body.pod_name,
            &review,
            &pod_data,
            &daemon_data(&daemon),
        )?;
        let node = Api::<Node>::all(state.shared.client.clone())
            .get_opt(&pod_data.node_name)
            .await?;
        let Some(node) = node else {
            delete_managed_pod(&state.shared, &active, &pod).await?;
            anyhow::bail!("bootstrap Node absent; replacing Pod");
        };
        let node = object_metadata(&node.metadata);
        let site_name = security::node_site(&node);
        let sites: Api<DynamicObject> =
            Api::all_with(state.shared.client.clone(), &site_resource());
        let site = if site_name.is_empty() {
            None
        } else {
            sites.get_opt(site_name).await?
        };
        let Some(site) = site else {
            delete_managed_pod(&state.shared, &active, &pod).await?;
            anyhow::bail!("bootstrap Site absent; replacing Pod");
        };
        let site = SiteData {
            metadata: object_metadata(&site.metadata),
        };
        let identity = authorize_enrollment(&EnrollmentData {
            namespace: &body.pod_namespace,
            pod_name: &body.pod_name,
            boot: &boot,
            review: &review,
            pod: &pod_data,
            daemon_set: &daemon_data(&daemon),
            node: &node,
            site: &site,
        });
        let identity = match identity {
            Ok(identity) => identity,
            Err(error) => {
                delete_managed_pod(&state.shared, &active, &pod).await?;
                return Err(error);
            }
        };
        if body.expected_universe != identity.universe || body.expected_node != identity.node {
            delete_managed_pod(&state.shared, &active, &pod).await?;
            anyhow::bail!("bootstrap identity changed; replacing Pod");
        }
        identity
    };
    // Every issuance, including renewal, requires current live selection.
    if let Some(selected) = state.runtime.selection(&identity.universe, &identity.node) {
        ensure!(
            selected.pod_uid == identity.pod_uid
                && selected.pod_name == identity.pod_name
                && selected.pod_namespace == body.pod_namespace,
            "committed Pod identity mismatch"
        );
    } else {
        bail!("identity not selected by committed topology");
    }
    let issued = active
        .manager
        .issue(body.csr.as_bytes(), identity, false, unix_now())
        .await
        .map_err(IssuanceUnavailable)?;
    state
        .shared
        .refresh_state(&active)
        .await
        .map_err(IssuanceUnavailable)?;
    ensure!(
        state.shared.serving() && active.term.is_active(),
        "leadership ended during enrollment"
    );
    Ok(EnrollmentResponse {
        certificate: String::from_utf8(issued.chain_pem()?)?,
        generation: issued.bundle.generation,
        issuer: issued.root_digest,
    })
}

async fn readiness(State(state): State<HttpState>) -> Response {
    let shared = &state.shared;
    if !shared.hot.ready(
        unix_now(),
        shared.listeners_ready.load(Ordering::Acquire),
        true,
    ) {
        return (StatusCode::SERVICE_UNAVAILABLE, "replica TLS not ready").into_response();
    }
    let check = async {
        let service = Api::<Service>::namespaced(shared.client.clone(), &shared.options.namespace)
            .get(COMPONENT)
            .await?;
        ensure!(
            service
                .spec
                .and_then(|s| s.selector)
                .and_then(|s| s.get(SERVING_LABEL).cloned())
                .as_deref()
                == Some("true"),
            "Service lacks leader-only selector"
        );
        let pod = shared.pods().get(&shared.options.pod_name).await?;
        ensure!(
            pod.uid().as_deref() == Some(&shared.options.pod_uid),
            "local Pod replaced"
        );
        if pod.labels().get(SERVING_LABEL).map(String::as_str) == Some("true") {
            ensure!(
                shared.serving()
                    && pod.annotations().get(ROUTING_BOOT).map(String::as_str)
                        == Some(shared.boot.as_str()),
                "stale routing hint"
            );
        }
        Ok::<_, anyhow::Error>(())
    };
    match tokio::time::timeout(Duration::from_secs(1), check).await {
        Ok(Ok(())) => (StatusCode::OK, "[+]replica-tls ok\nreadyz check passed\n").into_response(),
        _ => (StatusCode::SERVICE_UNAVAILABLE, "routing not ready").into_response(),
    }
}

// Management-only metadata, using the existing plain HTTP health listener.
// Never forward this request through the process-bound configuration handler.
async fn product_catalog(State(state): State<HttpState>, Path(universe): Path<String>) -> Response {
    if !state.shared.serving() || !state.runtime.ready() {
        return StatusCode::SERVICE_UNAVAILABLE.into_response();
    }
    let result = state.runtime.subscriptions.product_catalog(&universe);
    if !state.shared.serving() || !state.runtime.ready() {
        return StatusCode::SERVICE_UNAVAILABLE.into_response();
    }
    match result {
        Ok(body) => (
            [
                ("content-type", "application/json"),
                ("cache-control", "no-store"),
            ],
            body,
        )
            .into_response(),
        Err(status) => status.into_response(),
    }
}

async fn replica_ack(State(shared): State<Arc<Shared>>) -> Response {
    if !shared.hot.ready(
        unix_now(),
        shared.listeners_ready.load(Ordering::Acquire),
        true,
    ) {
        return StatusCode::SERVICE_UNAVAILABLE.into_response();
    }
    let Some(mut ack) = shared.installed_ack.read().unwrap().clone() else {
        return StatusCode::SERVICE_UNAVAILABLE.into_response();
    };
    ack.old_connections_drained = shared.hot.drained();
    ([("cache-control", "no-store")], axum::Json(ack)).into_response()
}

/// Native rustls listener: identity enters Axum extensions only after handshake.
/// Connections are closed on trust/issuer change, expiry, election loss or stop.
async fn tls_listener(
    shared: Arc<Shared>,
    listener: TcpListener,
    router: Router,
    mutual: bool,
    replica: bool,
) -> Result<()> {
    let capacity = Arc::new(Semaphore::new(if replica { 128 } else { 16384 }));
    let mut workers = JoinSet::new();
    loop {
        tokio::select! {
            _ = shared.stop.cancelled() => break,
            Some(result) = workers.join_next(), if !workers.is_empty() => { if let Err(e) = result { tracing::warn!(%e, "TLS worker failed"); } },
            accepted = listener.accept() => {
                let (raw, _) = accepted?;
                let Ok(permit) = capacity.clone().try_acquire_owned() else { continue; };
                let Ok((snapshot, guard)) = shared.hot.production_connection() else { continue; };
                let shared = shared.clone(); let router = router.clone();
                workers.spawn(async move {
                    let _permit = permit; let _guard = guard;
                    let tls = if replica { &snapshot.proof } else { &snapshot.production };
                    let Ok((stream, peer)) = tls.accept(raw, mutual).await else { return; };
                    let serving_at_start = shared.active().map(|a| a.term.token().to_owned());
                    let connection_peer = peer.clone();
                    let request_shared = shared.clone();
                    let accepted = Instant::now();
                    let service = hyper::service::service_fn(move |mut req: Request<hyper::body::Incoming>| {
                        let router = router.clone(); let peer = peer.clone(); let shared = request_shared.clone();
                        async move {
                            if !replica && !shared.serving() { return Ok::<_, std::convert::Infallible>(StatusCode::SERVICE_UNAVAILABLE.into_response()); }
                            if let Some(peer) = peer {
                                if peer.valid_at(unix_now()).is_err() { return Ok(StatusCode::FORBIDDEN.into_response()); }
                                req.extensions_mut().insert(peer);
                            }
                            let req = req.map(Body::new);
                            use tower::Service;
                            let mut router = router;
                            router.call(req).await
                        }
                    });
                    let io = hyper_util::rt::TokioIo::new(stream);
                    let mut builder = hyper::server::conn::http1::Builder::new();
                    builder.timer(hyper_util::rt::TokioTimer::new()).header_read_timeout(Duration::from_secs(5)).max_buf_size(32768).keep_alive(!replica);
                    let connection = builder.serve_connection(io, service);
                    tokio::pin!(connection);
                    let mut ticker = tokio::time::interval(Duration::from_secs(1));
                    loop {
                        tokio::select! {
                            _ = shared.stop.cancelled() => break,
                            _ = &mut connection => break,
                            _ = shared.changed.notified() => {
                                if !shared.hot.is_current_epoch(&snapshot) { break; }
                            },
                            _ = ticker.tick() => {
                                if replica && accepted.elapsed() > Duration::from_secs(10) { break; }
                                 if !tls.ready(unix_now()) || !shared.hot.is_current_epoch(&snapshot) { break; }
                                 if connection_peer.as_ref().is_some_and(|p| p.valid_at(unix_now()).is_err()) { break; }
                                if !replica && serving_at_start.is_some() && (!shared.serving() || shared.active().map(|a| a.term.token().to_owned()) != serving_at_start) { break; }
                                if !replica && serving_at_start.is_none() && accepted.elapsed() > Duration::from_secs(5) { break; }
                            }
                        }
                    }
                });
            }
        }
    }
    workers.abort_all();
    while workers.join_next().await.is_some() {}
    Ok(())
}

async fn proof_listener(shared: Arc<Shared>, listener: TcpListener) -> Result<()> {
    let capacity = Arc::new(Semaphore::new(64));
    let mut workers = JoinSet::new();
    loop {
        tokio::select! {
            _ = shared.stop.cancelled() => break,
            Some(_) = workers.join_next(), if !workers.is_empty() => (),
            accepted = listener.accept() => {
                let (raw, _) = accepted?;
                if !shared.serving() { continue; }
                let Some(active) = shared.active() else { continue; };
                let Ok(permit) = capacity.clone().try_acquire_owned() else { continue; };
                let Ok(snapshot) = shared.hot.snapshot() else { continue; };
                let shared = shared.clone();
                workers.spawn(async move {
                    let _permit = permit;
                    if let Ok(mut exchange) = snapshot.proof.receive_node_proof(raw, &active.term).await {
                        let result = active.manager.record_proof(&exchange.key, exchange.proof, unix_now()).await;
                        let status = if result.is_ok() && shared.serving() { "204 No Content" } else { "409 Conflict" };
                        let _ = exchange.stream.write_all(format!("HTTP/1.1 {status}\r\nContent-Length: 0\r\nConnection: close\r\n\r\n").as_bytes()).await;
                    }
                });
            }
        }
    }
    workers.abort_all();
    Ok(())
}

async fn patch_route(
    shared: &Shared,
    pod: &Pod,
    serving: bool,
    active: Option<&Active>,
) -> Result<()> {
    let rv = pod
        .resource_version()
        .context("Pod missing resourceVersion")?;
    let uid = pod.uid().context("Pod missing UID")?;
    if let Some(active) = active {
        active.store.check_fence().await?;
    }
    if serving {
        ensure!(
            active.is_some_and(|active| shared
                .active()
                .is_some_and(|current| current.term.token() == active.term.token())),
            "not current leader"
        );
    }
    let patch = serde_json::json!({ "metadata": { "resourceVersion": rv, "uid": uid,
        "labels": { SERVING_LABEL: if serving { Some("true") } else { None } },
        "annotations": { ROUTING_BOOT: shared.boot }
    }});
    shared
        .pods()
        .patch(
            &pod.name_any(),
            &PatchParams::default(),
            &Patch::Merge(patch),
        )
        .await?;
    Ok(())
}

async fn clear_local_route(shared: &Shared, owned_only: bool) -> Result<()> {
    let pod = shared.pods().get(&shared.options.pod_name).await?;
    ensure!(
        pod.uid().as_deref() == Some(&shared.options.pod_uid),
        "local Pod UID changed"
    );
    if owned_only && pod.annotations().get(ROUTING_BOOT).map(String::as_str) != Some(&shared.boot) {
        return Ok(());
    }
    patch_route(shared, &pod, false, None).await
}

async fn publish_route(shared: &Shared, active: &Active) -> Result<()> {
    let pods = shared
        .pods()
        .list(&ListParams::default().labels(&format!("{COMPONENT_LABEL}={COMPONENT}")))
        .await?;
    for pod in &pods.items {
        patch_route(shared, pod, false, Some(active)).await?;
    }
    let pod = shared.pods().get(&shared.options.pod_name).await?;
    ensure!(
        pod.uid().as_deref() == Some(&shared.options.pod_uid)
            && pod.metadata.deletion_timestamp.is_none(),
        "leader Pod replaced or terminating"
    );
    patch_route(shared, &pod, true, Some(active)).await
}

async fn reconcile_route(shared: &Shared, active: &Active) -> Result<()> {
    // Observe the real hint even after a successful publication. A local flag
    // cannot detect label removal or an uncertain outcome from a canceled PATCH.
    let pod = shared.pods().get(&shared.options.pod_name).await?;
    ensure!(
        pod.uid().as_deref() == Some(&shared.options.pod_uid)
            && pod.metadata.deletion_timestamp.is_none(),
        "leader Pod replaced or terminating"
    );
    if shared.serving()
        && pod.labels().get(SERVING_LABEL).map(String::as_str) == Some("true")
        && pod.annotations().get(ROUTING_BOOT).map(String::as_str) == Some(&shared.boot)
    {
        return Ok(());
    }
    shared.serving.store(false, Ordering::Release);
    // Keep request serving disabled across every await. Errors and cancellation
    // leave publication retryable, including a PATCH committed without a reply.
    // The full sweep is needed only on publication/repair, but must still touch
    // unlabeled Pods to fence delayed predecessor writes via resourceVersion.
    publish_route(shared, active).await?;
    let current = shared.active.read().unwrap();
    ensure!(
        current.as_ref().is_some_and(|current| {
            current.term.token() == active.term.token()
                && current.term.is_active()
                && current.last_renewal.elapsed() < shared.options.lease_timing.renew_deadline
                && !shared.stop.is_cancelled()
        }),
        "leadership lost during route publication"
    );
    // Serialize activation with lose_leadership so a late reply cannot re-enable
    // request serving for an expired term or a successor still initializing.
    shared.serving.store(true, Ordering::Release);
    Ok(())
}

const REPLICA_REQUEST: &str = "racer.unbounded-cloud.io/pki-request";
const REPLICA_RESPONSE: &str = "racer.unbounded-cloud.io/pki-response";

#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct ReplicaRequest {
    pod_uid: String,
    boot: String,
    csr: String,
}

#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct ReplicaResponse {
    pod_uid: String,
    boot: String,
    csr_digest: String,
    certificate: String,
    proof_certificate: String,
}

async fn patch_replica_annotation(
    shared: &Shared,
    pod: &Pod,
    key: &str,
    value: String,
    active: Option<&Active>,
) -> Result<()> {
    ensure!(value.len() <= 16384, "replica annotation too large");
    let rv = pod.resource_version().context("Pod missing revision")?;
    let uid = pod.uid().context("Pod missing UID")?;
    if let Some(active) = active {
        active.store.check_fence().await?;
    }
    shared
        .pods()
        .patch(
            &pod.name_any(),
            &PatchParams::default(),
            &Patch::Merge(serde_json::json!({
                "metadata": { "resourceVersion": rv, "uid": uid, "annotations": { key: value } }
            })),
        )
        .await?;
    Ok(())
}

async fn reconcile_local(shared: &Shared) -> Result<()> {
    let pod = shared.pods().get(&shared.options.pod_name).await?;
    ensure!(
        pod.uid().as_deref() == Some(&shared.options.pod_uid),
        "local Pod UID changed"
    );
    let identity = replica_identity(
        shared.client.clone(),
        &shared.options.namespace,
        &pod,
        &shared.boot,
    )
    .await?;
    let csr = String::from_utf8(shared.local_key.csr_pem.clone())?;
    let request = serde_json::to_string(&ReplicaRequest {
        pod_uid: shared.options.pod_uid.clone(),
        boot: shared.boot.clone(),
        csr,
    })?;
    if pod.annotations().get(REPLICA_REQUEST) != Some(&request) {
        patch_replica_annotation(shared, &pod, REPLICA_REQUEST, request, None).await?;
        bail!("waiting for replica certificate issuance");
    }
    let response = pod
        .annotations()
        .get(REPLICA_RESPONSE)
        .context("waiting for replica response")?;
    ensure!(response.len() <= 16384, "replica response too large");
    let response: ReplicaResponse = serde_json::from_str(response)?;
    ensure!(
        response.pod_uid == shared.options.pod_uid
            && response.boot == shared.boot
            && response.csr_digest == digest(&shared.local_key.csr_pem),
        "waiting for boot-bound replica certificates"
    );
    let production = &response.certificate;
    let proof = &response.proof_certificate;
    ensure!(
        certificate_matches_csr(production.as_bytes(), &shared.local_key.csr_pem)?
            && certificate_matches_csr(proof.as_bytes(), &shared.local_key.csr_pem)?,
        "replica certificate key mismatch"
    );
    for pem in [production, proof] {
        let leaf = openssl::x509::X509::from_pem(pem.as_bytes())?;
        let sans = leaf.subject_alt_names().context("missing SAN")?;
        let uri = sans
            .iter()
            .find_map(|s| s.uri())
            .context("missing claims")?;
        let claims = SignedClaims::parse(uri)?;
        ensure!(
            claims.identity == identity && claims.namespace == shared.options.namespace,
            "replica signed identity mismatch"
        );
        validate_replica_leaf(pem.as_bytes(), &shared.options.namespace)?;
    }
    let trust = shared.maps().get(TRUST_MAP).await?;
    let bundle = trust
        .data
        .as_ref()
        .and_then(|d| d.get(BUNDLE_KEY))
        .context("missing trust bundle")?;
    let bundle = TrustBundle::parse(bundle.as_bytes())?;
    let production = TlsSnapshot::new(
        &bundle.json(),
        production.as_bytes(),
        &shared.local_key.key_pem,
    )?;
    let proof = TlsSnapshot::new(&bundle.json(), proof.as_bytes(), &shared.local_key.key_pem)?;
    let changed = shared.hot.snapshot().is_ok_and(|old| {
        old.production.bundle() != &bundle || old.production.issuer() != production.issuer()
    });
    shared.hot.install(production, proof)?;
    if changed {
        shared.changed.notify_waiters();
    }
    let ack = ReplicaAcknowledgment {
        pod_uid: shared.options.pod_uid.clone(),
        boot_id: shared.boot.clone(),
        csr_digest: digest(&shared.local_key.csr_pem),
        generation: bundle.generation,
        digest: bundle.digest(),
        old_connections_drained: shared.hot.drained(),
    };
    *shared.installed_ack.write().unwrap() = Some(ack.clone());
    Ok(())
}

fn validate_replica_leaf(pem: &[u8], namespace: &str) -> Result<i64> {
    let cert = openssl::x509::X509::from_pem(pem)?;
    let der = cert.to_der()?;
    let (_, parsed) = x509_parser::parse_x509_certificate(&der)
        .map_err(|e| anyhow::anyhow!("invalid CP leaf: {e}"))?;
    ensure!(!parsed.is_ca(), "CA used as replica leaf");
    let eku = parsed.extended_key_usage()?.context("missing CP EKU")?;
    ensure!(
        eku.value.server_auth
            && !eku.value.client_auth
            && !eku.value.any
            && !eku.value.code_signing
            && !eku.value.email_protection
            && !eku.value.time_stamping
            && !eku.value.ocsp_signing
            && eku.value.other.is_empty(),
        "CP leaf must be server-auth only"
    );
    let sans = cert.subject_alt_names().context("missing CP SAN")?;
    let uris: Vec<_> = sans.iter().filter_map(|s| s.uri()).collect();
    ensure!(uris.len() == 1, "CP requires one signed identity");
    let claims = SignedClaims::parse(uris[0])?;
    ensure!(
        claims.identity.kind == IdentityKind::ControlPlane && claims.namespace == namespace,
        "CP URI mismatch"
    );
    ensure!(
        sans.iter().filter_map(|s| s.dnsname()).collect::<Vec<_>>()
            == [format!("racer-controlplane.{namespace}.svc")],
        "CP DNS mismatch"
    );
    Ok(parsed.validity().not_after.timestamp())
}

async fn issue_replica(
    shared: &Shared,
    active: &Active,
    pod: &Pod,
    identity: Identity,
) -> Result<()> {
    let uid = pod.uid().context("missing Pod UID")?;
    let request = pod
        .annotations()
        .get(REPLICA_REQUEST)
        .context("replica request missing")?;
    ensure!(request.len() <= 16384, "replica request too large");
    let request: ReplicaRequest = serde_json::from_str(request)?;
    ensure!(request.pod_uid == uid, "replica request UID mismatch");
    let boot = request.boot;
    let csr = request.csr;
    ensure!(
        boot == identity.boot_id && boot != "pending",
        "replica boot changed"
    );
    let csr_digest = digest(csr.as_bytes());
    let state = active.manager.state().await?;
    let bundle = state.bundle();
    let old = pod
        .annotations()
        .get(REPLICA_RESPONSE)
        .filter(|s| s.len() <= 16384)
        .and_then(|s| serde_json::from_str::<ReplicaResponse>(s).ok());
    let bound = old
        .as_ref()
        .is_some_and(|r| r.pod_uid == uid && r.boot == boot && r.csr_digest == csr_digest);
    let mut response = ReplicaResponse {
        pod_uid: uid,
        boot,
        csr_digest,
        certificate: String::new(),
        proof_certificate: String::new(),
    };
    let mut changed = false;
    for (root, probe) in [
        (bundle.active.clone(), false),
        (state.proof_root().to_owned(), true),
    ] {
        let current = old.as_ref().map(|r| {
            if probe {
                &r.proof_certificate
            } else {
                &r.certificate
            }
        });
        let valid = bound
            && current.is_some_and(|pem| {
                certificate_matches_csr(pem.as_bytes(), csr.as_bytes()).unwrap_or(false)
                    && certificate_claims(pem.as_bytes()).is_ok_and(|claims| {
                        claims.identity == identity && claims.namespace == shared.options.namespace
                    })
                    && certificate_issuer(pem.as_bytes(), &bundle)
                        .is_ok_and(|issuer| issuer == root)
                    && validate_replica_leaf(pem.as_bytes(), &shared.options.namespace).is_ok_and(
                        |expiry| {
                            expiry
                                > unix_now()
                                    + (shared.options.leaf_lifetime.as_secs() / 4).min(3600) as i64
                        },
                    )
            });
        if !valid {
            let issued = active
                .manager
                .issue(csr.as_bytes(), identity.clone(), probe, unix_now())
                .await?;
            if probe {
                response.proof_certificate = String::from_utf8(issued.certificate_pem)?;
            } else {
                response.certificate = String::from_utf8(issued.certificate_pem)?;
            }
            changed = true;
        } else if probe {
            response.proof_certificate = current.unwrap().clone();
        } else {
            response.certificate = current.unwrap().clone();
        }
    }
    if changed {
        patch_replica_annotation(
            shared,
            pod,
            REPLICA_RESPONSE,
            serde_json::to_string(&response)?,
            Some(active),
        )
        .await?;
    }
    Ok(())
}

async fn reconcile_participants(shared: &Shared, active: &Active) -> Result<()> {
    use futures::{StreamExt, stream};
    const REPLICA_BATCH: usize = 8;
    const REPLICA_DEADLINE: Duration = Duration::from_secs(5);

    active.manager.publish().await?;
    let pods = shared.pods().list(&ListParams::default()).await?;
    let mut candidates: Vec<_> = pods
        .items
        .iter()
        .filter(|pod| {
            pod.labels().get(COMPONENT_LABEL).map(String::as_str) == Some(COMPONENT)
                && pod.annotations().contains_key(REPLICA_REQUEST)
        })
        .collect();
    candidates.sort_by(|a, b| a.metadata.name.cmp(&b.metadata.name));
    let count = candidates.len().min(REPLICA_BATCH);
    // Advance before awaiting work: cancellation and repeated slow owners must
    // not restart each pass at the same prefix. Bound the entire batch as well
    // as concurrency so route publication gets a turn even with many slow Pods.
    let start = shared.replica_cursor.fetch_add(count, Ordering::Relaxed);
    let mut work = stream::iter((0..count).map(|offset| {
        let pod = candidates[start.wrapping_add(offset) % candidates.len()];
        async move {
        let result = tokio::time::timeout(REPLICA_DEADLINE, async {
            let Some(request) = pod.annotations().get(REPLICA_REQUEST) else {
                return Ok(());
            };
            ensure!(request.len() <= 16384, "replica request too large");
            let request: ReplicaRequest = serde_json::from_str(request)?;
            let identity = replica_identity(
                shared.client.clone(),
                &shared.options.namespace,
                pod,
                &request.boot,
            )
            .await?;
            issue_replica(shared, active, pod, identity).await
        }).await;
        match result {
            Ok(Ok(())) => (),
            Ok(Err(error)) => tracing::debug!(pod = pod.name_any(), %error, "replica issuance pending"),
            Err(error) => tracing::debug!(pod = pod.name_any(), %error, "replica issuance deadline exceeded"),
        }
        }
    })).buffer_unordered(REPLICA_BATCH);
    while work.next().await.is_some() {}
    shared.refresh_state(active).await?;
    Ok(())
}

fn site_resource() -> ApiResource {
    ApiResource {
        group: "unbounded-cloud.io".into(),
        version: "v1alpha3".into(),
        api_version: "unbounded-cloud.io/v1alpha3".into(),
        kind: "Site".into(),
        plural: "sites".into(),
    }
}

// Callers must first authorize live managed ownership.
async fn delete_managed_pod(shared: &Shared, active: &Active, pod: &Pod) -> Result<()> {
    let uid = pod
        .metadata
        .uid
        .clone()
        .filter(|v| !v.is_empty())
        .context("Pod UID missing")?;
    let version = pod
        .resource_version()
        .context("Pod resourceVersion missing")?;
    active.store.check_fence().await?;
    shared
        .pods()
        .delete(
            &pod.name_any(),
            &DeleteParams {
                preconditions: Some(Preconditions {
                    uid: Some(uid),
                    resource_version: Some(version),
                }),
                ..Default::default()
            },
        )
        .await?;
    tracing::info!(
        pod = pod.name_any(),
        "replacing managed Pod with stale process identity"
    );
    Ok(())
}

async fn rotation_due(shared: &Shared, active: &Active) -> Result<bool> {
    let state = active.manager.state().await?;
    if state.phase() != Phase::Stable {
        return Ok(false);
    }
    let bundle = state.bundle();
    let cert = openssl::x509::X509::from_pem(bundle.certificates.as_bytes())?;
    let der = cert.to_der()?;
    let (_, parsed) = x509_parser::parse_x509_certificate(&der)
        .map_err(|e| anyhow::anyhow!("invalid root: {e}"))?;
    // NotBefore is backdated by the configured clock skew. Basing the
    // schedule on persisted root creation survives process and leader restarts.
    Ok(unix_now()
        >= parsed
            .validity()
            .not_before
            .timestamp()
            .saturating_add(shared.options.clock_skew.as_secs() as i64)
            .saturating_add(shared.options.rotation_interval.as_secs() as i64))
}

async fn leader_reconcile(shared: &Shared, runtime: &Runtime, active: &Active) -> Result<()> {
    // Rotation is independent of replica reachability and issuance success.
    active.manager.publish().await?;
    let trust = shared.maps().get(TRUST_MAP).await?;
    if let Some(nonce) = trust
        .annotations()
        .get("racer.unbounded-cloud.io/rotate-ca")
        .filter(|s| !s.is_empty())
    {
        active
            .manager
            .begin_rotation_requested(unix_now(), nonce)
            .await?;
    }
    if rotation_due(shared, active).await? {
        active.manager.begin_rotation(unix_now()).await?;
    }
    active.manager.advance_rotation(unix_now()).await?;
    shared.refresh_state(active).await?;
    reconcile_participants(shared, active).await?;
    if runtime.ready()
        && shared.hot.ready(
            unix_now(),
            shared.listeners_ready.load(Ordering::Acquire),
            true,
        )
    {
        reconcile_route(shared, active).await?;
    }
    Ok(())
}

/// Lease writes are CAS-only. A contender observes an unchanged resourceVersion
/// for the full lease duration before takeover, avoiding reliance on clock skew.
async fn election_loop(shared: Arc<Shared>) -> Result<()> {
    let leases = Api::<Lease>::namespaced(shared.client.clone(), &shared.options.namespace);
    let timing = shared.options.lease_timing;
    let mut observed: Option<(String, Instant)> = None;
    let mut ticker = tokio::time::interval(timing.retry_period);
    loop {
        tokio::select! { _ = shared.stop.cancelled() => break, _ = ticker.tick() => () }
        let attempt = tokio::time::timeout(Duration::from_secs(60), async {
        let old = tokio::time::timeout(Duration::from_secs(2), leases.get_opt(LEASE)).await??;
            if let Some(active) = shared.active() {
                let mut lease = old.context("Lease disappeared")?;
                if lease.spec.as_ref().and_then(|s| s.holder_identity.as_deref()) != Some(active.term.token()) {
                    shared.lose_leadership();
                    bail!("Lease holder changed");
                }
                lease.spec.as_mut().unwrap().renew_time = Some(now_micro()?);
                tokio::time::timeout(Duration::from_secs(2), leases.replace(LEASE, &PostParams::default(), &lease)).await??;
                if let Some(current) = shared.active.write().unwrap().as_mut().filter(|a| a.term.token() == active.term.token()) { current.last_renewal = Instant::now(); }
                return Ok::<_, anyhow::Error>(());
            }
            // No active manager may survive a renewal deadline. Never reacquire
            // with the same token; late operations remain fenced by the old one.
            shared.lose_leadership();
            let token = format!("{}-{}", shared.boot, uuid::Uuid::new_v4().simple());
            let mut lease = if let Some(mut lease) = old {
                let rv = lease.resource_version().context("Lease lacks revision")?;
                let duration = lease.spec.as_ref().and_then(|s| s.lease_duration_seconds).unwrap_or(timing.duration_seconds).max(1) as u64;
                let empty = lease.spec.as_ref().and_then(|s| s.holder_identity.as_deref()).is_none_or(str::is_empty);
                let elapsed = match &observed { Some((version, at)) if version == &rv => at.elapsed(), _ => { observed = Some((rv, Instant::now())); Duration::ZERO } };
                if !empty && elapsed < Duration::from_secs(duration) { return Ok(()); }
                let transitions = lease.spec.as_ref().and_then(|s| s.lease_transitions).unwrap_or(0).saturating_add(1);
                lease.spec = Some(LeaseSpec { holder_identity: Some(token.clone()), lease_duration_seconds: Some(timing.duration_seconds), acquire_time: Some(now_micro()?), renew_time: Some(now_micro()?), lease_transitions: Some(transitions), ..Default::default() });
                leases.replace(LEASE, &PostParams::default(), &lease).await?
            } else {
                let lease = Lease { metadata: ObjectMeta { name: Some(LEASE.into()), namespace: Some(shared.options.namespace.clone()), ..Default::default() }, spec: Some(LeaseSpec { holder_identity: Some(token.clone()), lease_duration_seconds: Some(timing.duration_seconds), acquire_time: Some(now_micro()?), renew_time: Some(now_micro()?), lease_transitions: Some(0), ..Default::default() }) };
                leases.create(&PostParams::default(), &lease).await?
            };
            // Initial CA load may exceed a renewal tick. Renew separately
            // while acquisition executes; activation happens only after both
            // Secret and trust fences are durable.
            let term = Leadership::new(token.clone())?;
            struct CancelAcquisition(Option<Leadership>);
            impl Drop for CancelAcquisition { fn drop(&mut self) { if let Some(term) = &self.0 { term.cancel(); } } }
            let mut cancel_acquisition = CancelAcquisition(Some(term.clone()));
            let store = KubernetesCaStore::new(shared.client.clone(), shared.options.namespace.clone(), term.clone());
            let mut options = SecurityOptions::new(&shared.options.namespace);
            options.ca_lifetime = 365 * 86400;
            options.leaf_lifetime = shared.options.leaf_lifetime.as_secs() as i64;
            options.clock_skew = shared.options.clock_skew.as_secs() as i64;
            options.proof_lifetime = shared.options.overlap_delay.as_secs() as i64;
            let acquisition = CaManager::acquire(store.clone(), term.clone(), options, unix_now());
            tokio::pin!(acquisition);
            let manager = loop {
                tokio::select! {
                    _ = shared.stop.cancelled() => { term.cancel(); bail!("shutdown during CA acquisition"); },
                    result = &mut acquisition => break result?,
                    _ = tokio::time::sleep(timing.retry_period) => {
                        lease.spec.as_mut().unwrap().renew_time = Some(now_micro()?);
                        match leases.replace(LEASE, &PostParams::default(), &lease).await {
                            Ok(next) => lease = next,
                            Err(error) => { term.cancel(); return Err(error.into()); }
                        }
                    }
                }
            };
            let state = Arc::new(manager.state().await?);
            cancel_acquisition.0 = None;
            *shared.active.write().unwrap() = Some(Active { manager: Arc::new(manager), store, term, state, last_renewal: Instant::now() });
            observed = None;
            tracing::info!(%token, "acquired fenced CA leadership");
            Ok(())
        }).await;
        if !matches!(attempt, Ok(Ok(()))) {
            if shared
                .active
                .read()
                .unwrap()
                .as_ref()
                .is_some_and(|a| a.last_renewal.elapsed() >= timing.renew_deadline)
            {
                shared.lose_leadership();
            }
            tracing::debug!(?attempt, "Lease reconciliation pending");
        }
    }
    shared.lose_leadership();
    Ok(())
}

pub async fn run(client: Client, options: Options, shutdown: CancellationToken) -> Result<()> {
    options.lease_timing.validate()?;
    ensure!(
        !options.pod_name.is_empty() && !options.pod_uid.is_empty(),
        "RACER_POD_NAME and RACER_POD_UID required"
    );
    let shared = Arc::new(Shared {
        client: client.clone(),
        boot: hex::encode(rand::random::<[u8; 32]>()),
        active: RwLock::new(None),
        hot: HotTls::default(),
        local_key: generate_local_key()?,
        installed_ack: RwLock::new(None),
        listeners_ready: AtomicBool::new(false),
        serving: AtomicBool::new(false),
        changed: tokio::sync::Notify::new(),
        enrollment_capacity: Arc::new(Semaphore::new(8)),
        reviews: ReviewCache::new(options.review_qps, options.review_burst),
        refresh: Mutex::new(()),
        replica_cursor: AtomicUsize::new(0),
        options,
        stop: shutdown.child_token(),
    });
    clear_local_route(&shared, false).await?;
    let weak = Arc::downgrade(&shared);
    let security = Arc::new(move || weak.upgrade().and_then(|s| s.security_context()));
    let mut runtime_options = RuntimeOptions::new(&shared.options.namespace);
    runtime_options.socket_root = shared.options.socket_root.clone();
    let runtime = Arc::new(Runtime::new(runtime_options, security));
    let state = HttpState {
        shared: shared.clone(),
        runtime: runtime.clone(),
    };
    let control = runtime.router();
    let enrollment = Router::new()
        .route("/v1/enroll", post(enroll))
        .layer(DefaultBodyLimit::max(32768))
        .with_state(state.clone());
    let replica = Router::new()
        .route("/v1/replica-proof", get(replica_ack))
        .with_state(shared.clone());
    let health = Router::new()
        .route("/healthz", get(|| async { "ok\n" }))
        .route("/readyz", get(readiness))
        .route("/debug/product-catalog/{universe}", get(product_catalog))
        .with_state(state);
    let control_listener = TcpListener::bind(address(&shared.options.listen)).await?;
    let enroll_listener = TcpListener::bind(address(&shared.options.enroll_listen)).await?;
    let replica_listener = TcpListener::bind(address(&shared.options.replica_proof_listen)).await?;
    let trust_listener = TcpListener::bind(address(&shared.options.trust_proof_listen)).await?;
    let health_listener = TcpListener::bind(address(&shared.options.health_listen)).await?;
    let mut tasks = JoinSet::<Result<()>>::new();
    tasks.spawn(tls_listener(
        shared.clone(),
        control_listener,
        control,
        true,
        false,
    ));
    tasks.spawn(tls_listener(
        shared.clone(),
        enroll_listener,
        enrollment,
        false,
        false,
    ));
    tasks.spawn(tls_listener(
        shared.clone(),
        replica_listener,
        replica,
        false,
        true,
    ));
    tasks.spawn(proof_listener(shared.clone(), trust_listener));
    let stop = shared.stop.clone();
    tasks.spawn(async move {
        axum::serve(health_listener, health)
            .with_graceful_shutdown(stop.cancelled_owned())
            .await?;
        Ok(())
    });
    shared.listeners_ready.store(true, Ordering::Release);
    tasks.spawn(election_loop(shared.clone()));
    {
        let shared = shared.clone();
        tasks.spawn(async move {
            let mut ticker = tokio::time::interval(Duration::from_millis(100));
            loop {
                tokio::select! { _ = shared.stop.cancelled() => return Ok(()), _ = ticker.tick() => () }
                let expired = shared.active.read().unwrap().as_ref().is_some_and(|a| a.last_renewal.elapsed() >= shared.options.lease_timing.renew_deadline);
                if expired { shared.lose_leadership(); }
            }
        });
    }
    {
        let runtime = runtime.clone();
        let stop = shared.stop.clone();
        tasks.spawn(async move { runtime.as_ref().clone().run(client, stop).await });
    }
    {
        let shared = shared.clone();
        tasks.spawn(async move {
            let mut tick = tokio::time::interval(Duration::from_secs(2));
            loop {
                tokio::select! { _ = shared.stop.cancelled() => return Ok(()), _ = tick.tick() => () }
                match tokio::time::timeout(Duration::from_secs(15), reconcile_local(&shared)).await {
                    Ok(Ok(())) => (), other => tracing::debug!(?other, "local replica TLS pending"),
                }
            }
        });
    }
    {
        let shared = shared.clone();
        let runtime = runtime.clone();
        tasks.spawn(async move {
            let mut tick = tokio::time::interval(Duration::from_secs(2));
            loop {
                tokio::select! { _ = shared.stop.cancelled() => return Ok(()), _ = tick.tick() => () }
                let Some(active) = shared.active() else { continue; };
                match tokio::time::timeout(Duration::from_secs(60), leader_reconcile(&shared, &runtime, &active)).await {
                    Ok(Ok(())) => (), other => tracing::debug!(?other, "leader reconciliation pending"),
                }
                // Publish the latest confirmed trust state even after a failed
                // replica response CAS.
                let _ = shared.refresh_state(&active).await;
            }
        });
    }
    let outcome = tokio::select! {
        _ = shutdown.cancelled() => Ok(()),
        result = tasks.join_next() => match result { Some(Ok(Ok(()))) if shutdown.is_cancelled() => Ok(()), Some(Ok(Ok(()))) => Err(anyhow::anyhow!("production task stopped unexpectedly")), Some(Ok(Err(error))) => Err(error), Some(Err(error)) => Err(error.into()), None => Ok(()) }
    };
    shared.stop.cancel();
    shared.lose_leadership();
    shared.listeners_ready.store(false, Ordering::Release);
    let _ = tokio::time::timeout(Duration::from_secs(2), clear_local_route(&shared, true)).await;
    tasks.abort_all();
    while tasks.join_next().await.is_some() {}
    outcome
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::AtomicUsize;

    #[test]
    fn lease_timing_requires_ordered_positive_durations() {
        assert!(LeaseTiming::default().validate().is_ok());
        for timing in [
            LeaseTiming {
                duration_seconds: 0,
                ..Default::default()
            },
            LeaseTiming {
                duration_seconds: -1,
                ..Default::default()
            },
            LeaseTiming {
                retry_period: Duration::ZERO,
                ..Default::default()
            },
            LeaseTiming {
                retry_period: Duration::from_secs(10),
                ..Default::default()
            },
            LeaseTiming {
                renew_deadline: Duration::from_secs(15),
                ..Default::default()
            },
        ] {
            assert!(timing.validate().is_err(), "accepted {timing:?}");
        }
    }

    #[tokio::test]
    async fn review_cache_expiry_and_failed_authentication() -> Result<()> {
        let reviews = Arc::new(AtomicUsize::new(0));
        let authenticated = Arc::new(AtomicBool::new(true));
        let router = Router::new().route("/apis/authentication.k8s.io/v1/tokenreviews", post({
            let reviews = reviews.clone();
            let authenticated = authenticated.clone();
            move |axum::Json(request): axum::Json<serde_json::Value>| {
                let reviews = reviews.clone();
                let authenticated = authenticated.clone();
                async move {
                    reviews.fetch_add(1, Ordering::SeqCst);
                    assert_eq!(request["spec"]["audiences"], serde_json::json!([CONTROL_AUDIENCE]));
                    axum::Json(serde_json::json!({
                        "apiVersion": "authentication.k8s.io/v1", "kind": "TokenReview",
                        "metadata": {},
                        "spec": request["spec"],
                        "status": {
                            "authenticated": authenticated.load(Ordering::SeqCst),
                            "audiences": [CONTROL_AUDIENCE],
                            "user": {"extra": {"authentication.kubernetes.io/pod-uid": ["worker-pod"]}}
                        }
                    }))
                }
            }
        }));
        let listener = TcpListener::bind("127.0.0.1:0").await?;
        let client = Client::try_from(kube::Config::new(
            format!("http://{}", listener.local_addr()?).parse()?,
        ))?;
        let server = tokio::spawn(async move { axum::serve(listener, router).await });
        let cache = ReviewCache::new(100., 100);
        let base = Instant::now();
        let unix = unix_now();
        let token = |expiry| {
            format!(
                "fixture.{}.signature",
                base64::engine::general_purpose::URL_SAFE_NO_PAD
                    .encode(format!("{{\"exp\":{expiry}}}"))
            )
        };
        let credential = token(unix + 100);
        for (elapsed, expected) in [(0, 1), (0, 1), (4999, 1), (5000, 2), (5000, 2)] {
            let result = cache
                .authenticate_with_clock(client.clone(), &credential, || {
                    (base + Duration::from_millis(elapsed), unix)
                })
                .await?;
            assert_eq!(reviewed_pod_uid(&result)?, "worker-pod");
            assert_eq!(reviews.load(Ordering::SeqCst), expected);
        }
        // JWT expiry independently shortens the five-second monotonic TTL.
        let short = token(unix + 1);
        cache
            .authenticate_with_clock(client.clone(), &short, || (base, unix))
            .await?;
        assert_eq!(reviews.load(Ordering::SeqCst), 3);
        for expected in [4, 5] {
            assert!(
                cache
                    .authenticate_with_clock(client.clone(), &short, || (base, unix + 1))
                    .await
                    .is_err()
            );
            assert_eq!(reviews.load(Ordering::SeqCst), expected);
        }
        // An exp claim must never turn a rejected TokenReview into a cache hit.
        authenticated.store(false, Ordering::SeqCst);
        let rejected = token(unix + 200);
        for expected in [6, 7] {
            assert!(
                cache
                    .authenticate_with_clock(client.clone(), &rejected, || (base, unix))
                    .await
                    .is_err()
            );
            assert_eq!(reviews.load(Ordering::SeqCst), expected);
        }
        authenticated.store(true, Ordering::SeqCst);
        for expected in [8, 9] {
            cache
                .authenticate_with_clock(client.clone(), "fixture.no-exp.signature", || {
                    (base, unix)
                })
                .await?;
            assert_eq!(reviews.load(Ordering::SeqCst), expected);
        }
        server.abort();
        let _ = server.await;
        Ok(())
    }
}
