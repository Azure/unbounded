//! Small Kubernetes API client over the control reactor.
//!
//! It speaks JSON over HTTP/1.1 and TLS 1.2+ with the service account CA. The
//! API token is reread from disk at most every minute, so rotation works. Each
//! request has a deadline. Watches use a dedicated connection and report
//! `410 Gone` as [`KubeError::Gone`] so callers relist.

use crate::{
    exec::{self, Slots, Tcp},
    http::{self, Conn, TlsStream},
    limits,
};
use rustls::pki_types::{CertificateDer, ServerName};
use serde_json::Value;
use std::{
    cell::RefCell,
    net::{SocketAddr, ToSocketAddrs},
    path::PathBuf,
    sync::Arc,
    time::{Duration, Instant},
};

/// Payload-free API failures.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum KubeError {
    NotFound,
    Conflict,
    /// The resourceVersion is too old. Relist.
    Gone,
    Unauthorized,
    Forbidden,
    Invalid,
    TooLarge,
    /// 429: the API server is shedding load. Callers slow down.
    Throttled,
    Unavailable,
}

impl From<KubeError> for crate::Error {
    fn from(_: KubeError) -> Self {
        crate::Error::Unavailable
    }
}

/// Map an HTTP status to a result class.
pub fn status_error(status: u16) -> Option<KubeError> {
    Some(match status {
        200..=299 => return None,
        400 | 422 => KubeError::Invalid,
        401 => KubeError::Unauthorized,
        403 => KubeError::Forbidden,
        404 => KubeError::NotFound,
        409 => KubeError::Conflict,
        410 => KubeError::Gone,
        413 => KubeError::TooLarge,
        429 => KubeError::Throttled,
        _ => KubeError::Unavailable,
    })
}

/// Client settings.
#[derive(Clone)]
pub struct KubeConfig {
    /// `host:port`.
    pub server: String,
    pub ca: PathBuf,
    pub token: PathBuf,
}

struct Credentials {
    token: zeroize::Zeroizing<String>,
    ca: Vec<u8>,
    tls: Arc<rustls::ClientConfig>,
    read_at: Instant,
}

/// Kubernetes API client. Not `Send`; it lives on the control thread.
pub struct Kube {
    config: KubeConfig,
    address: SocketAddr,
    name: ServerName<'static>,
    host: String,
    credentials: RefCell<Option<Credentials>>,
    idle: RefCell<Vec<Conn>>,
    slots: Slots,
}

const MAX_IDLE: usize = 4;

/// Items per list page.
const LIST_PAGE: usize = 500;

/// Most items one list may return: 100k Nodes plus their Pods with headroom.
pub const MAX_LIST_ITEMS: usize = 400_000;
const CREDENTIAL_TTL: Duration = Duration::from_secs(60);

/// Build a client TLS config that trusts only `ca_pem`.
pub fn tls_config(ca_pem: &[u8]) -> Result<Arc<rustls::ClientConfig>, KubeError> {
    let mut roots = rustls::RootCertStore::empty();
    let mut reader = ca_pem;
    for cert in rustls_pemfile::certs(&mut reader) {
        let cert: CertificateDer<'static> = cert.map_err(|_| KubeError::Invalid)?;
        roots.add(cert).map_err(|_| KubeError::Invalid)?;
    }
    if roots.is_empty() {
        return Err(KubeError::Invalid);
    }
    let config = rustls::ClientConfig::builder_with_provider(Arc::new(
        rustls::crypto::ring::default_provider(),
    ))
    .with_safe_default_protocol_versions()
    .map_err(|_| KubeError::Invalid)?
    .with_root_certificates(roots)
    .with_no_client_auth();
    Ok(Arc::new(config))
}

impl Kube {
    /// Resolve the server address once. The service address is stable.
    pub fn new(config: KubeConfig) -> Result<Self, KubeError> {
        let address = config
            .server
            .to_socket_addrs()
            .map_err(|_| KubeError::Invalid)?
            .next()
            .ok_or(KubeError::Invalid)?;
        let host = config
            .server
            .rsplit_once(':')
            .map(|(h, _)| h)
            .unwrap_or(&config.server);
        let bare = host.trim_start_matches('[').trim_end_matches(']');
        let name = ServerName::try_from(bare.to_owned()).map_err(|_| KubeError::Invalid)?;
        Ok(Self {
            host: config.server.clone(),
            config,
            address,
            name,
            credentials: RefCell::new(None),
            idle: RefCell::new(Vec::new()),
            slots: Slots::new(limits::MAX_API_REQUESTS),
        })
    }

    /// Reread the token and CA when stale. A changed CA drops idle connections.
    fn credentials(
        &self,
    ) -> Result<(zeroize::Zeroizing<String>, Arc<rustls::ClientConfig>), KubeError> {
        let mut slot = self.credentials.borrow_mut();
        if slot
            .as_ref()
            .is_none_or(|c| c.read_at.elapsed() >= CREDENTIAL_TTL)
        {
            let token =
                std::fs::read_to_string(&self.config.token).map_err(|_| KubeError::Unauthorized)?;
            let token = zeroize::Zeroizing::new(token.trim().to_owned());
            if token.is_empty()
                || token.len() > limits::MAX_TOKEN_BYTES
                || token.bytes().any(|b| !b.is_ascii_graphic())
            {
                return Err(KubeError::Unauthorized);
            }
            let ca = std::fs::read(&self.config.ca).map_err(|_| KubeError::Unavailable)?;
            let tls = match slot.as_ref() {
                Some(old) if old.ca == ca => old.tls.clone(),
                _ => {
                    self.idle.borrow_mut().clear();
                    tls_config(&ca)?
                }
            };
            *slot = Some(Credentials {
                token,
                ca,
                tls,
                read_at: Instant::now(),
            });
        }
        let c = slot.as_ref().expect("set above");
        Ok((c.token.clone(), c.tls.clone()))
    }

    async fn connect(&self, tls: Arc<rustls::ClientConfig>) -> Result<Conn, KubeError> {
        let tcp = Tcp::connect(self.address)
            .await
            .map_err(|_| KubeError::Unavailable)?;
        let stream = TlsStream::client(tcp, tls, self.name.clone())
            .await
            .map_err(|_| KubeError::Unavailable)?;
        Ok(Conn::new(stream))
    }

    /// Send one request and return status and body, under [`limits::API_TIMEOUT`].
    pub async fn call(
        &self,
        method: &str,
        path: &str,
        content_type: Option<&str>,
        body: &[u8],
        limit: usize,
    ) -> Result<(u16, Vec<u8>), KubeError> {
        let _slot = self.slots.try_acquire().ok_or(KubeError::Unavailable)?;
        exec::timeout(
            limits::API_TIMEOUT,
            self.call_inner(method, path, content_type, body, limit),
        )
        .await
        .map_err(|_| KubeError::Unavailable)?
    }

    async fn call_inner(
        &self,
        method: &str,
        path: &str,
        content_type: Option<&str>,
        body: &[u8],
        limit: usize,
    ) -> Result<(u16, Vec<u8>), KubeError> {
        let (token, tls) = self.credentials()?;
        let bearer = format!("Bearer {}", token.as_str());
        let mut headers = vec![
            ("Authorization", bearer.as_str()),
            ("Accept", "application/json"),
        ];
        if let Some(ct) = content_type {
            headers.push(("Content-Type", ct));
        }
        let request =
            zeroize::Zeroizing::new(http::request(method, &self.host, path, &headers, body));
        // A pooled connection may have been closed by the server. Retry once on
        // a fresh connection. Every write here is idempotent or CAS-guarded.
        for attempt in 0..2 {
            let pooled = if attempt == 0 {
                self.idle.borrow_mut().pop()
            } else {
                None
            };
            let reused = pooled.is_some();
            let mut conn = match pooled {
                Some(c) => c,
                None => self.connect(tls.clone()).await?,
            };
            let result = async {
                conn.tls
                    .write_all(&request)
                    .await
                    .map_err(|_| KubeError::Unavailable)?;
                let head = conn
                    .read_head(false, limits::MAX_HEAD_BYTES)
                    .await
                    .map_err(|_| KubeError::Unavailable)?
                    .ok_or(KubeError::Unavailable)?;
                let body = conn
                    .read_response_body(&head, limit)
                    .await
                    .map_err(|_| KubeError::TooLarge)?;
                Ok::<_, KubeError>((head, body))
            }
            .await;
            match result {
                Ok((head, body)) => {
                    let close = head
                        .one("connection")
                        .ok()
                        .flatten()
                        .is_some_and(|v| v.eq_ignore_ascii_case("close"));
                    if !close && self.idle.borrow().len() < MAX_IDLE {
                        self.idle.borrow_mut().push(conn);
                    }
                    if head.status == 401 {
                        // Force a token reread on the next call.
                        if let Some(c) = self.credentials.borrow_mut().as_mut() {
                            c.read_at = Instant::now() - CREDENTIAL_TTL;
                        }
                    }
                    return Ok((head.status, body));
                }
                Err(KubeError::Unavailable) if reused => continue,
                Err(e) => return Err(e),
            }
        }
        Err(KubeError::Unavailable)
    }

    async fn json(
        &self,
        method: &str,
        path: &str,
        content_type: Option<&str>,
        body: Option<&Value>,
    ) -> Result<Value, KubeError> {
        let encoded = match body {
            Some(v) => serde_json::to_vec(v).map_err(|_| KubeError::Invalid)?,
            None => Vec::new(),
        };
        let (status, body) = self
            .call(
                method,
                path,
                content_type,
                &encoded,
                limits::MAX_API_RESPONSE,
            )
            .await?;
        if let Some(e) = status_error(status) {
            return Err(e);
        }
        serde_json::from_slice(&body).map_err(|_| KubeError::Invalid)
    }

    pub async fn get(&self, path: &str) -> Result<Value, KubeError> {
        self.json("GET", path, None, None).await
    }

    /// Replace an object. The body must carry `metadata.resourceVersion` for CAS.
    pub async fn put(&self, path: &str, object: &Value) -> Result<Value, KubeError> {
        self.json("PUT", path, Some("application/json"), Some(object))
            .await
    }

    pub async fn post(&self, path: &str, object: &Value) -> Result<Value, KubeError> {
        self.json("POST", path, Some("application/json"), Some(object))
            .await
    }

    /// JSON merge patch.
    pub async fn merge_patch(&self, path: &str, patch: &Value) -> Result<Value, KubeError> {
        self.json(
            "PATCH",
            path,
            Some("application/merge-patch+json"),
            Some(patch),
        )
        .await
    }

    /// JSON patch (RFC 6902).
    pub async fn json_patch(&self, path: &str, patch: &Value) -> Result<Value, KubeError> {
        self.json(
            "PATCH",
            path,
            Some("application/json-patch+json"),
            Some(patch),
        )
        .await
    }

    /// List all pages. Returns items and the list resourceVersion.
    ///
    /// Every page is bounded by [`limits::MAX_API_RESPONSE`] and the whole
    /// list by [`MAX_LIST_ITEMS`], so a misbehaving server that never stops
    /// returning `continue` cannot grow memory without bound. An expired
    /// continue token is `410` and surfaces as [`KubeError::Gone`].
    pub async fn list(&self, path: &str, query: &str) -> Result<(Vec<Value>, String), KubeError> {
        let mut items = Vec::new();
        let mut cont = String::new();
        let mut version = String::new();
        for _ in 0..MAX_LIST_ITEMS.div_ceil(LIST_PAGE) + 1 {
            let mut target = format!("{path}?limit={LIST_PAGE}");
            if !query.is_empty() {
                target.push('&');
                target.push_str(query);
            }
            if !cont.is_empty() {
                target.push_str("&continue=");
                target.push_str(&escape(&cont));
            }
            let page = self.get(&target).await?;
            cont = absorb_page(&mut items, &mut version, page)?;
            if cont.is_empty() {
                return Ok((items, version));
            }
        }
        Err(KubeError::TooLarge)
    }

    /// Start a watch from `resource_version`.
    pub async fn watch(
        &self,
        path: &str,
        query: &str,
        resource_version: &str,
    ) -> Result<Watch, KubeError> {
        let (token, tls) = self.credentials()?;
        let mut target = format!(
            "{path}?watch=1&allowWatchBookmarks=true&timeoutSeconds={}&resourceVersion={}",
            limits::WATCH_TIMEOUT.as_secs(),
            escape(resource_version)
        );
        if !query.is_empty() {
            target.push('&');
            target.push_str(query);
        }
        let bearer = format!("Bearer {}", token.as_str());
        let request = zeroize::Zeroizing::new(http::request(
            "GET",
            &self.host,
            &target,
            &[("Authorization", &bearer), ("Accept", "application/json")],
            &[],
        ));
        exec::timeout(limits::API_TIMEOUT, async {
            let mut conn = self.connect(tls).await?;
            conn.tls
                .write_all(&request)
                .await
                .map_err(|_| KubeError::Unavailable)?;
            let head = conn
                .read_head(false, limits::MAX_HEAD_BYTES)
                .await
                .map_err(|_| KubeError::Unavailable)?
                .ok_or(KubeError::Unavailable)?;
            if let Some(e) = status_error(head.status) {
                return Err(e);
            }
            let chunked = http::is_chunked(&head).map_err(|_| KubeError::Invalid)?;
            Ok(Watch {
                conn,
                chunked,
                pending: Vec::new(),
                done: false,
                deadline: Instant::now() + limits::WATCH_TIMEOUT + Duration::from_secs(30),
            })
        })
        .await
        .map_err(|_| KubeError::Unavailable)?
    }
}

/// An open watch stream.
pub struct Watch {
    conn: Conn,
    chunked: bool,
    pending: Vec<u8>,
    done: bool,
    deadline: Instant,
}

/// One watch event.
#[derive(Debug)]
pub struct Event {
    pub kind: String,
    pub object: Value,
}

impl Watch {
    /// Next event. `Ok(None)` when the server ended the watch.
    pub async fn next(&mut self) -> Result<Option<Event>, KubeError> {
        loop {
            if let Some(i) = self.pending.iter().position(|b| *b == b'\n') {
                let line: Vec<u8> = self.pending.drain(..=i).collect();
                if line.iter().all(u8::is_ascii_whitespace) {
                    continue;
                }
                return parse_event(&line).map(Some);
            }
            if self.pending.len() > limits::MAX_WATCH_EVENT {
                return Err(KubeError::TooLarge);
            }
            if self.done {
                return Ok(None);
            }
            let chunk = exec::deadline(self.deadline, async {
                if self.chunked {
                    self.conn.read_chunk(limits::MAX_WATCH_EVENT).await
                } else {
                    let mut buf = vec![0u8; 64 * 1024];
                    let n = self.conn.tls.read(&mut buf).await?;
                    buf.truncate(n);
                    Ok(buf)
                }
            })
            .await
            .map_err(|_| KubeError::Unavailable)?
            .map_err(|_| KubeError::Unavailable)?;
            if chunk.is_empty() {
                self.done = true;
            }
            self.pending.extend(chunk);
        }
    }
}

/// Add one list page to `items`. Returns its continue token, empty on the
/// last page. The first page's resourceVersion is the list's.
pub fn absorb_page(
    items: &mut Vec<Value>,
    version: &mut String,
    mut page: Value,
) -> Result<String, KubeError> {
    match page.get_mut("items").map(Value::take) {
        Some(Value::Array(a)) => items.extend(a),
        Some(Value::Null) | None => {}
        Some(_) => return Err(KubeError::Invalid),
    }
    if items.len() > MAX_LIST_ITEMS {
        return Err(KubeError::TooLarge);
    }
    if version.is_empty() {
        *version = str_at(&page, &["metadata", "resourceVersion"]).to_owned();
    }
    let cont = str_at(&page, &["metadata", "continue"]).to_owned();
    if cont.is_empty() && version.is_empty() {
        // A watch needs an opaque resourceVersion to start from.
        return Err(KubeError::Invalid);
    }
    Ok(cont)
}

/// Decode one watch line. Server `ERROR` events become errors.
pub fn parse_event(line: &[u8]) -> Result<Event, KubeError> {
    let mut v: Value = serde_json::from_slice(line).map_err(|_| KubeError::Invalid)?;
    let kind = v
        .get("type")
        .and_then(Value::as_str)
        .ok_or(KubeError::Invalid)?
        .to_owned();
    let object = v
        .get_mut("object")
        .map(Value::take)
        .ok_or(KubeError::Invalid)?;
    if kind == "ERROR" {
        let code = object.get("code").and_then(Value::as_u64).unwrap_or(500) as u16;
        return Err(status_error(code).unwrap_or(KubeError::Unavailable));
    }
    if !matches!(kind.as_str(), "ADDED" | "MODIFIED" | "DELETED" | "BOOKMARK") {
        return Err(KubeError::Invalid);
    }
    Ok(Event { kind, object })
}

/// Percent-encode a query value.
pub fn escape(value: &str) -> String {
    let mut out = String::with_capacity(value.len());
    for b in value.bytes() {
        if b.is_ascii_alphanumeric() || matches!(b, b'-' | b'_' | b'.' | b'~') {
            out.push(b as char);
        } else {
            out.push_str(&format!("%{b:02X}"));
        }
    }
    out
}

/// String at a JSON path, or empty.
pub fn str_at<'a>(v: &'a Value, path: &[&str]) -> &'a str {
    let mut v = v;
    for p in path {
        match v.get(p) {
            Some(n) => v = n,
            None => return "",
        }
    }
    v.as_str().unwrap_or("")
}

/// String map at a JSON path, such as labels or annotations.
pub fn map_at(v: &Value, path: &[&str]) -> std::collections::BTreeMap<String, String> {
    let mut v = v;
    for p in path {
        match v.get(p) {
            Some(n) => v = n,
            None => return Default::default(),
        }
    }
    v.as_object()
        .map(|m| {
            m.iter()
                .filter_map(|(k, v)| v.as_str().map(|s| (k.clone(), s.to_owned())))
                .collect()
        })
        .unwrap_or_default()
}

/// API paths for the resources this runtime uses.
pub mod paths {
    use crate::annotations;

    pub fn lease(ns: &str, name: &str) -> String {
        format!("/apis/coordination.k8s.io/v1/namespaces/{ns}/leases/{name}")
    }

    pub fn leases(ns: &str) -> String {
        format!("/apis/coordination.k8s.io/v1/namespaces/{ns}/leases")
    }

    pub fn state(ns: &str) -> String {
        format!(
            "/api/v1/namespaces/{ns}/secrets/{}",
            annotations::STATE_SECRET
        )
    }

    pub fn pods(ns: &str) -> String {
        format!("/api/v1/namespaces/{ns}/pods")
    }

    pub fn pod(ns: &str, name: &str) -> String {
        format!("/api/v1/namespaces/{ns}/pods/{name}")
    }

    pub const NODES: &str = "/api/v1/nodes";

    pub fn node(name: &str) -> String {
        format!("/api/v1/nodes/{name}")
    }

    pub const CACHES: &str = "/apis/racer.unbounded-cloud.io/v1alpha1/clustercaches";

    pub fn daemonset(ns: &str, name: &str) -> String {
        format!("/apis/apps/v1/namespaces/{ns}/daemonsets/{name}")
    }

    pub fn service_account(ns: &str, name: &str) -> String {
        format!("/api/v1/namespaces/{ns}/serviceaccounts/{name}")
    }

    pub const TOKEN_REVIEWS: &str = "/apis/authentication.k8s.io/v1/tokenreviews";

    pub fn webhook_configuration() -> String {
        format!(
            "/apis/admissionregistration.k8s.io/v1/mutatingwebhookconfigurations/{}",
            annotations::WEBHOOK_CONFIGURATION
        )
    }

    pub fn endpoint_slice(ns: &str) -> String {
        format!(
            "/apis/discovery.k8s.io/v1/namespaces/{ns}/endpointslices/{}",
            annotations::WEBHOOK_ENDPOINT_SLICE
        )
    }
}

#[cfg(test)]
pub(crate) mod fake;

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn events_and_errors() {
        let e = parse_event(br#"{"type":"ADDED","object":{"a":1}}"#).unwrap();
        assert_eq!(e.kind, "ADDED");
        assert_eq!(
            parse_event(br#"{"type":"ERROR","object":{"code":410}}"#).unwrap_err(),
            KubeError::Gone
        );
        assert_eq!(parse_event(b"{}").unwrap_err(), KubeError::Invalid);
        assert_eq!(
            parse_event(br#"{"type":"X","object":{}}"#).unwrap_err(),
            KubeError::Invalid
        );
        assert_eq!(status_error(409), Some(KubeError::Conflict));
        assert_eq!(status_error(429), Some(KubeError::Throttled));
        assert_eq!(status_error(503), Some(KubeError::Unavailable));
        assert_eq!(status_error(204), None);
        assert_eq!(escape("a b/c"), "a%20b%2Fc");
    }

    #[test]
    fn list_pages_are_bounded_and_strict() {
        let mut items = Vec::new();
        let mut rv = String::new();
        let page = serde_json::json!({"metadata":{"resourceVersion":"7","continue":"c1"},"items":[{"a":1}]});
        assert_eq!(absorb_page(&mut items, &mut rv, page).unwrap(), "c1");
        // Later pages keep the first page's opaque version.
        let page = serde_json::json!({"metadata":{"resourceVersion":"9"},"items":[{"a":2}]});
        assert_eq!(absorb_page(&mut items, &mut rv, page).unwrap(), "");
        assert_eq!((items.len(), rv.as_str()), (2, "7"));
        let mut rv = String::new();
        assert_eq!(
            absorb_page(&mut items, &mut rv, serde_json::json!({"items":[]})).unwrap_err(),
            KubeError::Invalid
        );
        assert_eq!(
            absorb_page(&mut items, &mut "1".into(), serde_json::json!({"items":{}})).unwrap_err(),
            KubeError::Invalid
        );
        let mut full = vec![Value::Null; MAX_LIST_ITEMS];
        assert_eq!(
            absorb_page(&mut full, &mut "1".into(), serde_json::json!({"items":[1]})).unwrap_err(),
            KubeError::TooLarge
        );
    }

    #[test]
    fn json_helpers() {
        let v = serde_json::json!({"metadata":{"name":"x","labels":{"a":"b","n":1}}});
        assert_eq!(str_at(&v, &["metadata", "name"]), "x");
        assert_eq!(str_at(&v, &["metadata", "missing"]), "");
        assert_eq!(map_at(&v, &["metadata", "labels"]).len(), 1);
    }

    #[test]
    fn client_against_fake_server() {
        fake::run(|server, kube| async move {
            server.put_object(
                "/api/v1/nodes/a",
                serde_json::json!({"metadata":{"name":"a","resourceVersion":"1"}}),
            );
            let v = kube.get("/api/v1/nodes/a").await.unwrap();
            assert_eq!(str_at(&v, &["metadata", "name"]), "a");
            assert_eq!(
                kube.get("/api/v1/nodes/b").await.unwrap_err(),
                KubeError::NotFound
            );
            // CAS.
            let mut stale = v.clone();
            stale["metadata"]["resourceVersion"] = "0".into();
            assert_eq!(
                kube.put("/api/v1/nodes/a", &stale).await.unwrap_err(),
                KubeError::Conflict
            );
            let updated = kube.put("/api/v1/nodes/a", &v).await.unwrap();
            assert_ne!(
                str_at(&updated, &["metadata", "resourceVersion"]),
                str_at(&v, &["metadata", "resourceVersion"])
            );
            let (items, rv) = kube.list("/api/v1/nodes", "").await.unwrap();
            assert_eq!(items.len(), 1);
            assert!(!rv.is_empty());
            let mut w = kube.watch("/api/v1/nodes", "", &rv).await.unwrap();
            let k2 = server.clone();
            exec::spawn(async move {
                exec::sleep(Duration::from_millis(10)).await;
                k2.put_object(
                    "/api/v1/nodes/c",
                    serde_json::json!({"metadata":{"name":"c"}}),
                );
            })
            .unwrap();
            let e = w.next().await.unwrap().unwrap();
            assert_eq!(e.kind, "ADDED");
            assert_eq!(str_at(&e.object, &["metadata", "name"]), "c");
            server.compact();
            let mut old = kube.watch("/api/v1/nodes", "", "0").await.unwrap();
            assert_eq!(old.next().await.unwrap_err(), KubeError::Gone);
        });
    }
}
