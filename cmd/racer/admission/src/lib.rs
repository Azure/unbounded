//! Mutating admission logic for Racer pods. [`ENABLED_LABEL`] injects the
//! racer-object sidecar as a native sidecar (a restartable init container).
//! [`CLIENT_SOCKET_LABEL`] mounts the Racer client socket into init and app containers.
//! The two opt-ins are independent and both apply when a pod sets both labels.
//!
//! It reads an `admission.k8s.io/v1` `AdmissionReview` and returns the
//! complete response review. A separate HTTP server serves [`PATH`].
//!
//! Outcomes:
//! - Not a v1 Pod `CREATE`, not opted in, or already injected: allowed, no patch.
//! - Bad pod settings: denied with code 403 and a message naming the field.
//! - Pod object that cannot be read: denied with code 400.
//! - Named ClusterCache missing, or its node selector invalid: denied with 403.
//! - ClusterCache lookup failed: denied with 503 so the client retries. The
//!   pod is never admitted without its cache's node affinity.
//! - Opted-in pod: allowed with a JSON Patch. When the cache has a node
//!   selector, the patch adds it to the pod's required node affinity.
//!
//! [`admit`] returns [`Error`] only when the review itself cannot be answered,
//! for example when it is not JSON or has no request UID.

mod mutate;
mod quantity;
mod selector;
mod strict;

use mutate::mutate;

use std::fmt;

use base64::Engine as _;
use serde_json::{Map, Value, json};

/// HTTP path the webhook is served on.
pub const PATH: &str = "/mutate-racer-object-sidecar";
/// Largest accepted review body.
pub const MAX_REQUEST_BYTES: usize = 7 * 1024 * 1024;

/// UID and GID the sidecar runs as.
pub const SIDECAR_UID: i64 = 65532;
pub const CONTAINER_NAME: &str = "racer-object";
pub const VOLUME_NAME: &str = "racer-object-client";
pub const ENABLED_LABEL: &str = "racer.unbounded-cloud.io/object-sidecar";
pub const ENABLED_VALUE: &str = "enabled";
pub const CACHE_ANNOTATION: &str = "racer.unbounded-cloud.io/object-cache";
pub const API_ANNOTATION: &str = "racer.unbounded-cloud.io/object-api";
pub const LISTEN_ANNOTATION: &str = "racer.unbounded-cloud.io/object-listen";
pub const CPU_REQUEST_ANNOTATION: &str = "racer.unbounded-cloud.io/object-cpu-request";
pub const CPU_LIMIT_ANNOTATION: &str = "racer.unbounded-cloud.io/object-cpu-limit";
pub const MEMORY_REQUEST_ANNOTATION: &str = "racer.unbounded-cloud.io/object-memory-request";
pub const MEMORY_LIMIT_ANNOTATION: &str = "racer.unbounded-cloud.io/object-memory-limit";
pub const INJECTED_ANNOTATION: &str = "racer.unbounded-cloud.io/object-sidecar-injected";
/// Opts a pod into a read-only mount of the Racer client socket directory,
/// independent of the racer-object sidecar.
pub const CLIENT_SOCKET_LABEL: &str = "racer.unbounded-cloud.io/client-socket";
/// Names the ClusterCache whose client socket is mounted.
pub const CLIENT_CACHE_ANNOTATION: &str = "racer.unbounded-cloud.io/client-cache";
/// Optionally lists, comma-separated, the init and app containers that get the
/// mount. The default is every init and app container except racer-object.
pub const CLIENT_CONTAINERS_ANNOTATION: &str = "racer.unbounded-cloud.io/client-containers";
/// Records the mounted cache.
pub const CLIENT_SOCKET_INJECTED_ANNOTATION: &str =
    "racer.unbounded-cloud.io/client-socket-injected";
/// Volume added when no volume already mounts the cache's client directory.
pub const CLIENT_VOLUME_NAME: &str = "racer-client-socket";
/// Tells clients where the socket is.
pub const SOCKET_ENV: &str = "RACER_SOCKET";
/// Node label that keeps Racer workloads off a node. Same as Go `wire.ExclusionLabel`.
pub const EXCLUSION_LABEL: &str = "racer.unbounded-cloud.io/exclude";

const OS_LABEL: &str = "kubernetes.io/os";
const API_VERSION: &str = "admission.k8s.io/v1";
const REVIEW_KIND: &str = "AdmissionReview";
const UID_MAX: usize = 256;

/// Scope annotations from before each ClusterCache mapped to one bucket.
const REMOVED_ANNOTATIONS: [&str; 2] = [
    "racer.unbounded-cloud.io/object-namespace",
    "racer.unbounded-cloud.io/object-buckets",
];

/// Length limit for a client socket path, as in the Go SDK.
const SOCKET_PATH_LIMIT: usize = 107;

/// A review that cannot be answered. The server should reply with HTTP 400.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Error {
    /// The body is larger than [`MAX_REQUEST_BYTES`].
    TooLarge(usize),
    /// The body is not valid JSON, or has duplicate keys.
    Json(String),
    /// The JSON is not an `admission.k8s.io/v1` `AdmissionReview` request.
    Review(String),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::TooLarge(n) => write!(
                f,
                "admission review is {n} bytes, limit is {MAX_REQUEST_BYTES}"
            ),
            Error::Json(e) => write!(f, "admission review is not valid JSON: {e}"),
            Error::Review(e) => write!(f, "invalid admission review: {e}"),
        }
    }
}

impl std::error::Error for Error {}

/// Result of looking up a ClusterCache by name.
#[derive(Debug, Clone, PartialEq)]
pub enum CacheLookup {
    /// The cache exists. Holds its `spec.nodeSelector`, or `None` when unset.
    Found(Option<Value>),
    /// No ClusterCache has this name.
    NotFound,
    /// The lookup failed, for example with an API error. The pod is denied
    /// with a retryable status rather than admitted without its pool affinity.
    Unavailable,
}

/// A parsed `AdmissionReview` request. [`Review::caches`] lists the
/// ClusterCaches the pod may need, so the caller can look them up before
/// [`Review::respond`].
pub struct Review {
    uid: String,
    req: Map<String, Value>,
}

/// Answers one `AdmissionReview` and returns the response review as JSON.
///
/// `image` is the racer-object sidecar image. A blank image denies every
/// v1 Pod `CREATE`, as the Go handler does. `lookup` resolves the
/// ClusterCache named by the pod.
pub fn admit(
    review: &[u8],
    image: &str,
    lookup: &dyn Fn(&str) -> CacheLookup,
) -> Result<Vec<u8>, Error> {
    Ok(Review::parse(review)?.respond(image, lookup))
}

impl Review {
    /// Parses and checks the review envelope.
    pub fn parse(review: &[u8]) -> Result<Self, Error> {
        if review.len() > MAX_REQUEST_BYTES {
            return Err(Error::TooLarge(review.len()));
        }
        let review = strict::parse(review).map_err(|e| Error::Json(e.to_string()))?;
        let Value::Object(mut review) = review else {
            return Err(Error::Review("not an object".into()));
        };

        if review.get("apiVersion").and_then(Value::as_str) != Some(API_VERSION) {
            return Err(Error::Review(format!("apiVersion must be {API_VERSION}")));
        }
        if review.get("kind").and_then(Value::as_str) != Some(REVIEW_KIND) {
            return Err(Error::Review(format!("kind must be {REVIEW_KIND}")));
        }
        let Some(Value::Object(req)) = review.remove("request") else {
            return Err(Error::Review("request is missing".into()));
        };
        let uid = match req.get("uid") {
            Some(Value::String(u)) if !u.is_empty() && u.len() <= UID_MAX => u.clone(),
            _ => {
                return Err(Error::Review(
                    "request.uid must be a non-empty string".into(),
                ));
            }
        };
        Ok(Review { uid, req })
    }

    /// Names of the ClusterCaches the opted-in pod names, valid names only,
    /// without duplicates. Empty when no lookup is needed.
    pub fn caches(&self) -> Vec<String> {
        let annotations = self
            .req
            .get("object")
            .and_then(|o| o.get("metadata"))
            .and_then(|m| m.get("annotations"))
            .and_then(Value::as_object);
        let labels = self
            .req
            .get("object")
            .and_then(|o| o.get("metadata"))
            .and_then(|m| m.get("labels"))
            .and_then(Value::as_object);
        let mut out: Vec<String> = Vec::new();
        for (label, annotation) in [
            (ENABLED_LABEL, CACHE_ANNOTATION),
            (CLIENT_SOCKET_LABEL, CLIENT_CACHE_ANNOTATION),
        ] {
            if get_str(labels, label) != Some(ENABLED_VALUE) {
                continue;
            }
            if let Some(c) = get_str(annotations, annotation)
                && valid_cache(c)
                && !out.iter().any(|o| o == c)
            {
                out.push(c.to_owned());
            }
        }
        out
    }

    /// Returns the response review as JSON.
    pub fn respond(&self, image: &str, lookup: &dyn Fn(&str) -> CacheLookup) -> Vec<u8> {
        let uid = &self.uid;
        let req = &self.req;
        let response = match handle(req, image, lookup) {
            Ok(patch) if patch.is_empty() => {
                json!({"uid": uid, "allowed": true, "status": {"code": 200}})
            }
            Ok(patch) => {
                let patch =
                    serde_json::to_vec(&patch).expect("serializing a JSON value cannot fail");
                json!({
                    "uid": uid,
                    "allowed": true,
                    "status": {"code": 200},
                    "patchType": "JSONPatch",
                    "patch": base64::engine::general_purpose::STANDARD.encode(patch),
                })
            }
            Err(Reject::Denied(msg)) => json!({
                "uid": uid,
                "allowed": false,
                "status": {"code": 403, "reason": "Forbidden", "message": msg},
            }),
            Err(Reject::BadRequest(msg)) => json!({
                "uid": uid,
                "allowed": false,
                "status": {"code": 400, "message": msg},
            }),
            Err(Reject::Unavailable(msg)) => json!({
                "uid": uid,
                "allowed": false,
                "status": {"code": 503, "reason": "ServiceUnavailable", "message": msg},
            }),
        };

        let out = json!({"apiVersion": API_VERSION, "kind": REVIEW_KIND, "response": response});
        serde_json::to_vec(&out).expect("serializing a JSON value cannot fail")
    }
}

#[derive(Debug, PartialEq, Eq)]
enum Reject {
    Denied(String),
    BadRequest(String),
    /// Retryable: the pod may be admitted once the dependency recovers.
    Unavailable(String),
}

fn bad(msg: impl Into<String>) -> Reject {
    Reject::BadRequest(msg.into())
}

fn deny(msg: impl Into<String>) -> Reject {
    Reject::Denied(msg.into())
}

/// Returns JSON Patch operations, empty when the pod is left unchanged.
fn handle(
    req: &Map<String, Value>,
    image: &str,
    lookup: &dyn Fn(&str) -> CacheLookup,
) -> Result<Vec<Value>, Reject> {
    let kind = req.get("kind").and_then(Value::as_object);
    let field = |name: &str| {
        kind.and_then(|k| k.get(name))
            .and_then(Value::as_str)
            .unwrap_or("")
    };
    let op = req.get("operation").and_then(Value::as_str).unwrap_or("");
    if op != "CREATE"
        || !field("group").is_empty()
        || field("version") != "v1"
        || field("kind") != "Pod"
    {
        return Ok(Vec::new());
    }
    // dryRun needs no special handling: this webhook has no side effects.
    match req.get("object") {
        Some(pod @ Value::Object(_)) => mutate(pod, image, lookup),
        _ => Err(bad("request.object must be a Pod object")),
    }
}

/// Typed read-only view of the pod fields the injector uses.
struct Pod<'a> {
    labels: Option<&'a Map<String, Value>>,
    annotations: Option<&'a Map<String, Value>>,
    init_containers: Option<&'a Vec<Value>>,
    containers: Option<&'a Vec<Value>>,
    volumes: Option<&'a Vec<Value>>,
    host_network: bool,
    os_name: Option<&'a str>,
    node_selector: Option<&'a Map<String, Value>>,
    affinity: Option<&'a Map<String, Value>>,
}

/// Returns `None` for an absent or null field, or an error for a non-object.
fn object<'a>(
    m: &'a Map<String, Value>,
    key: &str,
    path: &str,
) -> Result<Option<&'a Map<String, Value>>, Reject> {
    match m.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Object(o)) => Ok(Some(o)),
        Some(_) => Err(bad(format!("{path} must be an object"))),
    }
}

fn array<'a>(
    m: &'a Map<String, Value>,
    key: &str,
    path: &str,
) -> Result<Option<&'a Vec<Value>>, Reject> {
    match m.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Array(a)) => Ok(Some(a)),
        Some(_) => Err(bad(format!("{path} must be an array"))),
    }
}

fn string<'a>(m: &'a Map<String, Value>, key: &str, path: &str) -> Result<Option<&'a str>, Reject> {
    match m.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(s)) => Ok(Some(s)),
        Some(_) => Err(bad(format!("{path} must be a string"))),
    }
}

fn string_map<'a>(
    m: &'a Map<String, Value>,
    key: &str,
    path: &str,
) -> Result<Option<&'a Map<String, Value>>, Reject> {
    let o = object(m, key, path)?;
    if let Some(o) = o
        && o.values().any(|v| !v.is_string())
    {
        return Err(bad(format!("{path} values must be strings")));
    }
    Ok(o)
}

/// Checks that every item is an object with an optional string name.
fn named_items(items: Option<&Vec<Value>>, path: &str) -> Result<(), Reject> {
    for item in items.into_iter().flatten() {
        let o = item
            .as_object()
            .ok_or_else(|| bad(format!("{path} items must be objects")))?;
        string(o, "name", &format!("{path}.name"))?;
    }
    Ok(())
}

fn has_name(items: Option<&Vec<Value>>, name: &str) -> bool {
    items
        .into_iter()
        .flatten()
        .any(|i| i.get("name").and_then(Value::as_str) == Some(name))
}

fn get_str<'a>(m: Option<&'a Map<String, Value>>, key: &str) -> Option<&'a str> {
    m.and_then(|m| m.get(key)).and_then(Value::as_str)
}

impl<'a> Pod<'a> {
    fn read(pod: &'a Value) -> Result<Self, Reject> {
        let pod = pod
            .as_object()
            .ok_or_else(|| bad("pod must be an object"))?;
        let empty = &EMPTY;
        let meta = object(pod, "metadata", "metadata")?.unwrap_or(empty);
        let spec = object(pod, "spec", "spec")?.unwrap_or(empty);
        let host_network = match spec.get("hostNetwork") {
            None | Some(Value::Null) => false,
            Some(Value::Bool(b)) => *b,
            Some(_) => return Err(bad("spec.hostNetwork must be a boolean")),
        };
        let os_name = match object(spec, "os", "spec.os")? {
            None => None,
            Some(os) => Some(string(os, "name", "spec.os.name")?.unwrap_or("")),
        };
        let p = Pod {
            labels: string_map(meta, "labels", "metadata.labels")?,
            annotations: string_map(meta, "annotations", "metadata.annotations")?,
            init_containers: array(spec, "initContainers", "spec.initContainers")?,
            containers: array(spec, "containers", "spec.containers")?,
            volumes: array(spec, "volumes", "spec.volumes")?,
            host_network,
            os_name,
            node_selector: string_map(spec, "nodeSelector", "spec.nodeSelector")?,
            affinity: object(spec, "affinity", "spec.affinity")?,
        };
        named_items(p.init_containers, "spec.initContainers")?;
        named_items(p.containers, "spec.containers")?;
        named_items(p.volumes, "spec.volumes")?;
        Ok(p)
    }
}

static EMPTY: std::sync::LazyLock<Map<String, Value>> = std::sync::LazyLock::new(Map::new);

/// Escapes a JSON Pointer reference token (RFC 6901).
fn escape(s: &str) -> String {
    s.replace('~', "~0").replace('/', "~1")
}

struct SidecarSpec {
    cache: String,
    api: Option<String>,
    listen: Option<String>,
    resources: Value,
}

impl SidecarSpec {
    fn args(&self) -> Vec<String> {
        // Kubelet expands $(VAR) in container args. Cache, api, and listen
        // values are validated and cannot contain "$", so no escaping is needed.
        let mut args = vec!["sidecar".to_owned(), format!("--cache={}", self.cache)];
        if let Some(api) = &self.api {
            args.push(format!("--api={api}"));
        }
        if let Some(listen) = &self.listen {
            args.push(format!("--listen={listen}"));
        }
        args
    }
}

fn parse_annotations(a: Option<&Map<String, Value>>) -> Result<SidecarSpec, Reject> {
    // The sidecar serves the whole keyspace of its cache and the origin
    // enforces the bucket. Reject the removed scope annotations so stale
    // manifests fail loudly instead of silently widening access.
    for name in REMOVED_ANNOTATIONS {
        if a.is_some_and(|a| a.contains_key(name)) {
            return Err(deny(format!(
                "annotation {name} is no longer supported; the ClusterCache origin selects the bucket"
            )));
        }
    }

    // There is no default cache, so a missing annotation cannot attach the
    // pod to an unrelated cache.
    let Some(cache) = get_str(a, CACHE_ANNOTATION) else {
        return Err(deny(format!(
            "annotation {CACHE_ANNOTATION} is required and must name the ClusterCache to bind to"
        )));
    };
    if !valid_cache(cache) {
        return Err(deny(format!(
            "annotation {CACHE_ANNOTATION} has invalid cache name {cache:?}"
        )));
    }

    let api = get_str(a, API_ANNOTATION);
    if let Some(api) = api
        && api != "s3"
        && api != "azure"
    {
        return Err(deny(format!(
            "annotation {API_ANNOTATION} must be s3 or azure"
        )));
    }

    let listen = get_str(a, LISTEN_ANNOTATION);
    if let Some(listen) = listen
        && !valid_listen(listen)
    {
        return Err(deny(format!(
            "annotation {LISTEN_ANNOTATION} must be a loopback IP literal and port (1-65535)"
        )));
    }

    Ok(SidecarSpec {
        cache: cache.to_owned(),
        api: api.map(str::to_owned),
        listen: listen.map(str::to_owned),
        resources: parse_resources(a)?,
    })
}

/// Same rules as the Go SDK: a lowercase DNS-style name whose socket path fits.
fn valid_cache(s: &str) -> bool {
    if s.is_empty()
        || s.len() > 253
        || format!("/run/racer/{s}/origin/socket").len() > SOCKET_PATH_LIMIT
    {
        return false;
    }
    s.split('.').all(|l| {
        !l.is_empty()
            && l.len() <= 63
            && !l.starts_with('-')
            && !l.ends_with('-')
            && l.bytes()
                .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == b'-')
    })
}

/// Follows Go `net.SplitHostPort`, `net.ParseIP`, and `IP.IsLoopback`.
fn valid_listen(v: &str) -> bool {
    let (host, port) = if let Some(rest) = v.strip_prefix('[') {
        let Some((host, port)) = rest.split_once("]:") else {
            return false;
        };
        (host, port)
    } else {
        let Some((host, port)) = v.rsplit_once(':') else {
            return false;
        };
        if host.contains(':') {
            return false;
        }
        (host, port)
    };
    if host.contains(['[', ']']) || port.contains([':', '[', ']']) {
        return false;
    }
    let loopback = match host.parse::<std::net::IpAddr>() {
        Ok(std::net::IpAddr::V4(ip)) => ip.is_loopback(),
        Ok(std::net::IpAddr::V6(ip)) => {
            ip.is_loopback() || ip.to_ipv4_mapped().is_some_and(|v4| v4.is_loopback())
        }
        Err(_) => false,
    };
    if !loopback || port.is_empty() || port.len() > 9 || !port.bytes().all(|c| c.is_ascii_digit()) {
        return false;
    }
    port.parse::<u32>().is_ok_and(|n| (1..=65535).contains(&n))
}

fn parse_resources(a: Option<&Map<String, Value>>) -> Result<Value, Reject> {
    let mut requests = Map::new();
    let mut limits = Map::new();
    for (name, req_anno, lim_anno) in [
        ("cpu", CPU_REQUEST_ANNOTATION, CPU_LIMIT_ANNOTATION),
        ("memory", MEMORY_REQUEST_ANNOTATION, MEMORY_LIMIT_ANNOTATION),
    ] {
        let req = parse_quantity(a, req_anno)?;
        let lim = parse_quantity(a, lim_anno)?;
        if let (Some((r, _)), Some((l, _))) = (&req, &lim)
            && r.cmp_positive(l).is_gt()
        {
            return Err(deny(format!(
                "annotation {req_anno} must not exceed {lim_anno}"
            )));
        }
        if let Some((_, s)) = req {
            requests.insert(name.into(), Value::String(s));
        }
        if let Some((_, s)) = lim {
            limits.insert(name.into(), Value::String(s));
        }
    }
    let mut r = Map::new();
    if !limits.is_empty() {
        r.insert("limits".into(), Value::Object(limits));
    }
    if !requests.is_empty() {
        r.insert("requests".into(), Value::Object(requests));
    }
    Ok(Value::Object(r))
}

fn parse_quantity(
    a: Option<&Map<String, Value>>,
    key: &str,
) -> Result<Option<(quantity::Quantity, String)>, Reject> {
    let Some(v) = get_str(a, key) else {
        return Ok(None);
    };
    let v = v.trim();
    match quantity::parse(v) {
        Some(q) if q.is_positive() => Ok(Some((q, v.to_owned()))),
        _ => Err(deny(format!(
            "annotation {key} must be a positive resource quantity"
        ))),
    }
}

fn exclusion() -> Value {
    json!({"key": EXCLUSION_LABEL, "operator": "DoesNotExist"})
}

/// Returns the pod affinity with the exclusion requirement and `extra` node
/// requirements ANDed into every required node selector term. Ports Go
/// `addExclusionAffinity`.
fn with_exclusion(affinity: Option<&Map<String, Value>>, extra: &[Value]) -> Result<Value, Reject> {
    let mut reqs = vec![exclusion()];
    reqs.extend_from_slice(extra);
    let mut aff = affinity.cloned().unwrap_or_default();
    let mut na = object(&aff, "nodeAffinity", "spec.affinity.nodeAffinity")?
        .cloned()
        .unwrap_or_default();
    let key = "requiredDuringSchedulingIgnoredDuringExecution";
    let path = "spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution";
    let sel = match object(&na, key, path)? {
        None => json!({"nodeSelectorTerms": [{"matchExpressions": reqs}]}),
        Some(sel) => {
            let mut sel = sel.clone();
            let tpath = format!("{path}.nodeSelectorTerms");
            if let Some(terms) = array(&sel, "nodeSelectorTerms", &tpath)? {
                let mut terms = terms.clone();
                for term in &mut terms {
                    add_to_term(term, &tpath, &reqs)?;
                }
                sel.insert("nodeSelectorTerms".into(), Value::Array(terms));
            }
            Value::Object(sel)
        }
    };
    na.insert(key.into(), sel);
    aff.insert("nodeAffinity".into(), Value::Object(na));
    Ok(Value::Object(aff))
}

/// Whether two node selector requirements are the same, treating absent
/// and empty values alike.
fn same_requirement(a: &Map<String, Value>, b: &Value) -> bool {
    let values = |v: Option<&Value>| match v {
        None | Some(Value::Null) => Vec::new(),
        Some(Value::Array(a)) => a.clone(),
        Some(other) => vec![other.clone()],
    };
    a.get("key") == b.get("key")
        && a.get("operator") == b.get("operator")
        && values(a.get("values")) == values(b.get("values"))
}

fn add_to_term(term: &mut Value, path: &str, reqs: &[Value]) -> Result<(), Reject> {
    let t = term
        .as_object_mut()
        .ok_or_else(|| bad(format!("{path} items must be objects")))?;
    let exprs = array(t, "matchExpressions", &format!("{path}.matchExpressions"))?;
    let fields = array(t, "matchFields", &format!("{path}.matchFields"))?;
    // An empty term matches no nodes. Adding requirements would make it
    // match nodes, so leave it unchanged.
    if exprs.is_none_or(Vec::is_empty) && fields.is_none_or(Vec::is_empty) {
        return Ok(());
    }
    let mut exprs = exprs.cloned().unwrap_or_default();
    let mut existing = Vec::with_capacity(exprs.len());
    for e in &exprs {
        let e = e
            .as_object()
            .ok_or_else(|| bad(format!("{path}.matchExpressions items must be objects")))?;
        string(e, "key", "matchExpressions.key")?;
        string(e, "operator", "matchExpressions.operator")?;
        array(e, "values", "matchExpressions.values")?;
        existing.push(e.clone());
    }
    let mut changed = false;
    for r in reqs {
        if !existing.iter().any(|e| same_requirement(e, r)) {
            exprs.push(r.clone());
            changed = true;
        }
    }
    if changed {
        t.insert("matchExpressions".into(), Value::Array(exprs));
    }
    Ok(())
}

#[cfg(test)]
mod tests;
