//! In-memory Kubernetes API server for tests.
//!
//! It serves a plain-value [`Store`] (GET, list, PUT with resourceVersion
//! CAS, POST, merge and JSON PATCH, watch with compaction (410 Gone), field
//! selectors, fault injection) over TLS on loopback.

use super::{Kube, KubeConfig};
use crate::{
    exec::{self, Executor, Listener, Signal},
    http::{self, Conn, TlsStream},
};
use serde_json::{Value, json};
use std::{
    cell::RefCell,
    collections::{BTreeMap, VecDeque},
    future::Future,
    rc::Rc,
    sync::{
        Arc,
        atomic::{AtomicU64, AtomicUsize, Ordering::SeqCst},
    },
    time::Duration,
};

/// Shared fake server state: a [`Store`] plus wakeups and test hooks.
#[derive(Clone)]
pub struct FakeApi(Rc<Inner>);

struct Inner {
    store: RefCell<Store>,
    changed: Signal,
    requests: RefCell<Vec<(String, String)>>,
    counter: RefCell<Option<Arc<AtomicUsize>>>,
    put_delay: RefCell<Option<Arc<AtomicU64>>>,
    get_delay: RefCell<Option<Arc<AtomicU64>>>,
}

impl FakeApi {
    pub fn new() -> Self {
        Self(Rc::new(Inner {
            store: RefCell::new(Store::new()),
            changed: Signal::default(),
            requests: RefCell::new(Vec::new()),
            counter: RefCell::new(None),
            put_delay: RefCell::new(None),
            get_delay: RefCell::new(None),
        }))
    }

    /// Count every request in `counter`, readable from other threads.
    pub fn count_into(&self, counter: Arc<AtomicUsize>) {
        *self.0.counter.borrow_mut() = Some(counter);
    }

    /// Hold each PUT until it is `delay_ms` old; `u64::MAX` hangs PUTs as a
    /// stuck API server would.
    pub fn delay_puts(&self, delay_ms: Arc<AtomicU64>) {
        *self.0.put_delay.borrow_mut() = Some(delay_ms);
    }

    /// Answer each GET with the object as it was when the GET arrived, but
    /// only `delay_ms` later.
    pub fn delay_gets(&self, delay_ms: Arc<AtomicU64>) {
        *self.0.get_delay.borrow_mut() = Some(delay_ms);
    }

    /// Delays of a `method` request before and after it is handled.
    fn delays(&self, method: &str) -> (Duration, Duration) {
        let ms = |d: &RefCell<Option<Arc<AtomicU64>>>, m: &str| {
            let ms = d
                .borrow()
                .as_ref()
                .filter(|_| method == m)
                .map_or(0, |d| d.load(SeqCst));
            Duration::from_millis(ms)
        };
        (ms(&self.0.put_delay, "PUT"), ms(&self.0.get_delay, "GET"))
    }

    /// Store an object, set its resourceVersion and a UID if missing.
    pub fn put_object(&self, path: &str, object: Value) -> Value {
        let o = self.0.store.borrow_mut().put_object(path, object);
        self.0.changed.notify();
        o
    }

    pub fn delete_object(&self, path: &str) {
        if self.0.store.borrow_mut().delete_object(path).is_some() {
            self.0.changed.notify();
        }
    }

    pub fn object(&self, path: &str) -> Option<Value> {
        self.0.store.borrow().object(path).cloned()
    }

    /// Forget watch history so older resourceVersions get 410.
    pub fn compact(&self) {
        self.0.store.borrow_mut().compact();
    }

    /// Handle POSTs, for example TokenReview.
    pub fn on_post(&self, hook: impl Fn(&str, &Value) -> Option<(u16, Value)> + 'static) {
        self.0.store.borrow_mut().on_post(hook);
    }

    /// Fail the next requests with these statuses.
    pub fn fail_next(&self, statuses: &[u16]) {
        self.0.store.borrow_mut().fail_next(statuses);
    }

    /// Requests seen, as (method, target).
    pub fn requests(&self) -> Vec<(String, String)> {
        self.0.requests.borrow().clone()
    }

    fn handle(&self, method: &str, target: &str, body: &[u8]) -> (u16, Value) {
        self.0
            .requests
            .borrow_mut()
            .push((method.into(), target.into()));
        if let Some(c) = self.0.counter.borrow().as_ref() {
            c.fetch_add(1, SeqCst);
        }
        let (status, value) = self.0.store.borrow_mut().handle(method, target, body);
        if (200..300).contains(&status) && method != "GET" {
            self.0.changed.notify();
        }
        (status, value)
    }

    async fn serve_watch(&self, conn: &mut Conn, path: &str, query: &str) {
        let from: u64 = query
            .split('&')
            .find_map(|kv| kv.strip_prefix("resourceVersion="))
            .and_then(|v| v.parse().ok())
            .unwrap_or(0);
        let name = field_name(query);
        let head = b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n";
        if conn.tls.write_all(head).await.is_err() {
            return;
        }
        let send = |v: Value| {
            let mut line = serde_json::to_vec(&v).unwrap();
            line.push(b'\n');
            let mut out = format!("{:x}\r\n", line.len()).into_bytes();
            out.extend(line);
            out.extend(b"\r\n");
            out
        };
        let mut seen = from;
        loop {
            let generation = self.0.changed.generation();
            let events = self
                .0
                .store
                .borrow()
                .events_since(path, name.as_deref(), seen);
            let Ok(events) = events else {
                let _ = conn
                    .tls
                    .write_all(&send(
                        json!({"type":"ERROR","object":{"kind":"Status","code":410}}),
                    ))
                    .await;
                let _ = conn.tls.write_all(b"0\r\n\r\n").await;
                return;
            };
            for e in events {
                seen = e.version;
                if conn
                    .tls
                    .write_all(&send(json!({"type":e.kind.as_str(),"object":e.object})))
                    .await
                    .is_err()
                {
                    return;
                }
            }
            seen = seen.max(self.0.store.borrow().version());
            self.0.changed.changed(generation).await;
        }
    }
}

/// Serve the fake API on loopback and return client settings for it.
pub fn serve(api: FakeApi, dir: &std::path::Path) -> KubeConfig {
    let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256).unwrap();
    let mut params = rcgen::CertificateParams::new(vec![]).unwrap();
    params.subject_alt_names = vec![rcgen::SanType::IpAddress("127.0.0.1".parse().unwrap())];
    let cert = params.self_signed(&key).unwrap();
    std::fs::create_dir_all(dir).unwrap();
    let ca = dir.join("ca.crt");
    let token = dir.join("token");
    std::fs::write(&ca, cert.pem()).unwrap();
    std::fs::write(&token, "api-token").unwrap();
    let config = rustls::ServerConfig::builder_with_provider(Arc::new(
        rustls::crypto::ring::default_provider(),
    ))
    .with_safe_default_protocol_versions()
    .unwrap()
    .with_no_client_auth()
    .with_single_cert(
        vec![cert.der().clone()],
        rustls::pki_types::PrivateKeyDer::Pkcs8(key.serialize_der().into()),
    )
    .unwrap();
    let config = Arc::new(config);
    let listener = Rc::new(Listener::bind("127.0.0.1:0".parse().unwrap()).unwrap());
    let address = listener.local_addr().unwrap();
    exec::spawn(async move {
        loop {
            let Ok((tcp, _)) = listener.accept().await else {
                return;
            };
            let api = api.clone();
            let config = config.clone();
            let _ = exec::spawn(async move {
                let Ok(tls) = TlsStream::server(tcp, config).await else {
                    return;
                };
                let mut conn = Conn::new(tls);
                while let Ok(Some(head)) = conn.read_head(true, 1 << 20).await {
                    let Ok(body) = conn.read_request_body(&head, 64 << 20).await else {
                        return;
                    };
                    if head.one("authorization").ok().flatten() != Some("Bearer api-token") {
                        let _ = conn
                            .tls
                            .write_all(&http::response_head(401, &[], 0, false))
                            .await;
                        continue;
                    }
                    let (path, query) = http::split_target(&head.target);
                    if head.method == "GET"
                        && query.is_some_and(|q| q.split('&').any(|kv| kv == "watch=1"))
                    {
                        api.serve_watch(&mut conn, path, query.unwrap()).await;
                        return;
                    }
                    // Delays are re-read while waiting.
                    let received = std::time::Instant::now();
                    while received.elapsed() < api.delays(&head.method).0 {
                        exec::sleep(Duration::from_millis(20)).await;
                    }
                    let (status, value) = api.handle(&head.method, &head.target, &body);
                    let answered = std::time::Instant::now();
                    while answered.elapsed() < api.delays(&head.method).1 {
                        exec::sleep(Duration::from_millis(20)).await;
                    }
                    let body = serde_json::to_vec(&value).unwrap();
                    let mut out = http::response_head(
                        status,
                        &[("Content-Type", "application/json")],
                        body.len(),
                        false,
                    );
                    out.extend(body);
                    if conn.tls.write_all(&out).await.is_err() {
                        return;
                    }
                }
            });
        }
    })
    .unwrap();
    KubeConfig {
        server: address.to_string(),
        ca,
        token,
    }
}

static DIRS: AtomicUsize = AtomicUsize::new(0);

/// Project-local scratch directory for one test.
pub fn scratch() -> std::path::PathBuf {
    let n = DIRS.fetch_add(1, SeqCst);
    let dir = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../../../tmp/racer-cluster-control-tests")
        .join(format!("{}-{n}", std::process::id()));
    std::fs::create_dir_all(&dir).unwrap();
    dir
}

/// Run a test body with a fake API and a connected client.
pub fn run<F, Fut>(body: F)
where
    F: FnOnce(FakeApi, Rc<Kube>) -> Fut,
    Fut: Future<Output = ()>,
{
    let mut ex = Executor::new(256).unwrap();
    let dir = scratch();
    ex.block_on(async {
        let api = FakeApi::new();
        let config = serve(api.clone(), &dir);
        let kube = Rc::new(Kube::new(config).unwrap());
        body(api, kube).await;
    });
    let _ = std::fs::remove_dir_all(&dir);
}

/// Watch event kind.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum EventKind {
    Added,
    Modified,
    Deleted,
}

impl EventKind {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Added => "ADDED",
            Self::Modified => "MODIFIED",
            Self::Deleted => "DELETED",
        }
    }
}

/// One logged change.
#[derive(Clone, Debug)]
pub struct Event {
    pub version: u64,
    pub path: String,
    pub kind: EventKind,
    pub object: Value,
}

/// The watch start is older than the retained log.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Gone;

type Hook = Box<dyn Fn(&str, &Value) -> Option<(u16, Value)>>;

/// The API server state.
pub struct Store {
    objects: BTreeMap<String, Value>,
    log: VecDeque<Event>,
    version: u64,
    compacted: u64,
    uids: u64,
    faults: VecDeque<u16>,
    post_hook: Option<Hook>,
}

impl Store {
    pub fn new() -> Self {
        Self {
            objects: BTreeMap::new(),
            log: VecDeque::new(),
            version: 10,
            compacted: 0,
            uids: 1,
            faults: VecDeque::new(),
            post_hook: None,
        }
    }

    /// Current resourceVersion.
    pub fn version(&self) -> u64 {
        self.version
    }

    fn bump(&mut self) -> u64 {
        self.version += 1;
        self.version
    }

    /// Store an object, set its resourceVersion and a UID and name if missing.
    pub fn put_object(&mut self, path: &str, mut object: Value) -> Value {
        let v = self.bump();
        let existed = self.objects.contains_key(path);
        object["metadata"]["resourceVersion"] = v.to_string().into();
        if str_at(&object, &["metadata", "uid"]).is_empty() {
            let n = self.uids;
            self.uids += 1;
            object["metadata"]["uid"] = format!("00000000-0000-4000-8000-{n:012x}").into();
        }
        if str_at(&object, &["metadata", "name"]).is_empty() {
            object["metadata"]["name"] = path.rsplit('/').next().unwrap_or_default().into();
        }
        self.objects.insert(path.into(), object.clone());
        let kind = if existed {
            EventKind::Modified
        } else {
            EventKind::Added
        };
        self.log.push_back(Event {
            version: v,
            path: path.into(),
            kind,
            object: object.clone(),
        });
        object
    }

    /// Delete an object. Returns it when it existed.
    pub fn delete_object(&mut self, path: &str) -> Option<Value> {
        let o = self.objects.remove(path)?;
        let v = self.bump();
        self.log.push_back(Event {
            version: v,
            path: path.into(),
            kind: EventKind::Deleted,
            object: o.clone(),
        });
        Some(o)
    }

    pub fn object(&self, path: &str) -> Option<&Value> {
        self.objects.get(path)
    }

    /// Forget watch history so older resourceVersions get 410.
    pub fn compact(&mut self) {
        self.compacted = self.version;
        self.log.clear();
    }

    /// Handle POSTs, for example TokenReview.
    pub fn on_post(&mut self, hook: impl Fn(&str, &Value) -> Option<(u16, Value)> + 'static) {
        self.post_hook = Some(Box::new(hook));
    }

    /// Fail the next requests with these statuses.
    pub fn fail_next(&mut self, statuses: &[u16]) {
        self.faults.extend(statuses.iter().copied());
    }

    /// Direct children of `collection`, optionally with this name.
    pub fn list(&self, collection: &str, name: Option<&str>) -> Vec<Value> {
        let prefix = format!("{collection}/");
        self.objects
            .range(prefix.clone()..)
            .take_while(|(k, _)| k.starts_with(&prefix))
            .filter(|(k, _)| !k[prefix.len()..].contains('/'))
            .filter(|(_, v)| name.is_none_or(|n| str_at(v, &["metadata", "name"]) == n))
            .map(|(_, v)| v.clone())
            .collect()
    }

    /// Serve one request. `target` is a path with an optional query.
    pub fn handle(&mut self, method: &str, target: &str, body: &[u8]) -> (u16, Value) {
        if let Some(s) = self.faults.pop_front() {
            return (s, json!({"kind":"Status","code":s}));
        }
        let (path, query) = match target.split_once('?') {
            Some((p, q)) => (p, Some(q)),
            None => (target, None),
        };
        let request: Value = if body.is_empty() {
            Value::Null
        } else {
            match serde_json::from_slice(body) {
                Ok(v) => v,
                Err(_) => return (400, json!({})),
            }
        };
        self.apply(method, path, query, &request)
    }

    fn apply(
        &mut self,
        method: &str,
        path: &str,
        query: Option<&str>,
        request: &Value,
    ) -> (u16, Value) {
        let name = field_name(query.unwrap_or(""));
        match method {
            "GET" => {
                if let Some(o) = self.objects.get(path).cloned() {
                    return (200, o);
                }
                let items = self.list(path, name.as_deref());
                if items.is_empty() && !path.ends_with('s') {
                    return (404, json!({"code":404}));
                }
                (
                    200,
                    json!({"metadata":{"resourceVersion":self.version.to_string()},"items":items}),
                )
            }
            "PUT" => {
                let Some(current) = self.object(path) else {
                    return (404, json!({}));
                };
                let rv = str_at(request, &["metadata", "resourceVersion"]);
                if rv != str_at(current, &["metadata", "resourceVersion"]) {
                    return (409, json!({"code":409}));
                }
                let mut next = request.clone();
                next["metadata"]["uid"] = current["metadata"]["uid"].clone();
                (200, self.put_object(path, next))
            }
            "POST" => {
                if let Some(r) = self.post_hook.as_ref().and_then(|h| h(path, request)) {
                    return r;
                }
                let name = str_at(request, &["metadata", "name"]).to_owned();
                let full = format!("{path}/{name}");
                if name.is_empty() || self.object(&full).is_some() {
                    return (409, json!({}));
                }
                (201, self.put_object(&full, request.clone()))
            }
            "PATCH" => {
                let Some(mut current) = self.object(path).cloned() else {
                    return (404, json!({}));
                };
                if request.is_array() {
                    if let Err(s) = json_patch(&mut current, request) {
                        return (s, json!({}));
                    }
                } else {
                    merge(&mut current, request);
                }
                (200, self.put_object(path, current))
            }
            "DELETE" => match self.delete_object(path) {
                Some(o) => (200, o),
                None => (404, json!({"code":404})),
            },
            _ => (405, json!({})),
        }
    }

    /// Events under `collection` after `from`, optionally for one name.
    pub fn events_since(
        &self,
        collection: &str,
        name: Option<&str>,
        from: u64,
    ) -> Result<Vec<Event>, Gone> {
        if from < self.compacted {
            return Err(Gone);
        }
        let prefix = format!("{collection}/");
        Ok(self
            .log
            .iter()
            .filter(|e| {
                e.version > from
                    && e.path.starts_with(&prefix)
                    && name.is_none_or(|n| str_at(&e.object, &["metadata", "name"]) == n)
            })
            .cloned()
            .collect())
    }
}

fn str_at<'a>(v: &'a Value, path: &[&str]) -> &'a str {
    let mut cur = v;
    for p in path {
        cur = &cur[*p];
    }
    cur.as_str().unwrap_or("")
}

/// `metadata.name` from a field selector query.
pub fn field_name(query: &str) -> Option<String> {
    query
        .split('&')
        .find_map(|kv| {
            kv.strip_prefix("fieldSelector=metadata.name%3D")
                .or_else(|| kv.strip_prefix("fieldSelector=metadata.name="))
        })
        .map(str::to_owned)
}

/// RFC 7386 merge patch.
pub fn merge(target: &mut Value, patch: &Value) {
    if let Value::Object(p) = patch {
        if !target.is_object() {
            *target = json!({});
        }
        let t = target.as_object_mut().expect("object");
        for (k, v) in p {
            if v.is_null() {
                t.remove(k);
            } else {
                merge(t.entry(k.clone()).or_insert(Value::Null), v);
            }
        }
    } else {
        *target = patch.clone();
    }
}

/// RFC 6902 subset: add, replace, remove, test.
pub fn json_patch(target: &mut Value, ops: &Value) -> Result<(), u16> {
    for op in ops.as_array().ok_or(400u16)? {
        let pointer = op["path"].as_str().ok_or(400u16)?.to_owned();
        match op["op"].as_str().ok_or(400u16)? {
            "test" => {
                if target.pointer(&pointer) != Some(&op["value"]) {
                    return Err(422);
                }
            }
            "replace" => *target.pointer_mut(&pointer).ok_or(422u16)? = op["value"].clone(),
            "add" => {
                let (parent, key) = pointer.rsplit_once('/').ok_or(400u16)?;
                let key = key.replace("~1", "/").replace("~0", "~");
                match target.pointer_mut(parent).ok_or(422u16)? {
                    Value::Object(m) => {
                        m.insert(key, op["value"].clone());
                    }
                    Value::Array(a) if key == "-" => a.push(op["value"].clone()),
                    _ => return Err(422),
                }
            }
            "remove" => {
                let (parent, key) = pointer.rsplit_once('/').ok_or(400u16)?;
                let key = key.replace("~1", "/").replace("~0", "~");
                target
                    .pointer_mut(parent)
                    .and_then(Value::as_object_mut)
                    .and_then(|m| m.remove(&key))
                    .ok_or(422u16)?;
            }
            _ => return Err(400),
        }
    }
    Ok(())
}

mod store_tests {
    use super::*;

    const LEASES: &str = "/apis/coordination.k8s.io/v1/namespaces/ns/leases";

    fn lease(name: &str) -> Value {
        json!({"metadata": {"name": name}, "spec": {}})
    }

    #[test]
    fn cas_put_and_create() {
        let mut s = Store::new();
        let body = serde_json::to_vec(&lease("a")).unwrap();
        let (st, created) = s.handle("POST", LEASES, &body);
        assert_eq!(st, 201);
        assert_eq!(s.handle("POST", LEASES, &body).0, 409);
        let path = format!("{LEASES}/a");
        let mut next = created.clone();
        next["spec"]["holderIdentity"] = "p".into();
        let (st, put) = s.handle("PUT", &path, &serde_json::to_vec(&next).unwrap());
        assert_eq!(st, 200);
        assert_eq!(put["metadata"]["uid"], created["metadata"]["uid"]);
        // Stale resourceVersion conflicts.
        assert_eq!(
            s.handle("PUT", &path, &serde_json::to_vec(&next).unwrap())
                .0,
            409
        );
        assert_eq!(s.handle("PUT", &format!("{LEASES}/x"), b"{}").0, 404);
        assert_eq!(s.handle("GET", &format!("{LEASES}/x"), b"").0, 404);
        assert_eq!(s.handle("GET", "/x/y", b"{").0, 400);
    }

    #[test]
    fn list_selectors_and_patches() {
        let mut s = Store::new();
        s.put_object(&format!("{LEASES}/a"), lease("a"));
        s.put_object(&format!("{LEASES}/b"), lease("b"));
        s.put_object(&format!("{LEASES}/b/sub"), lease("sub"));
        let (_, l) = s.handle("GET", LEASES, b"");
        assert_eq!(l["items"].as_array().unwrap().len(), 2);
        let (_, l) = s.handle(
            "GET",
            &format!("{LEASES}?fieldSelector=metadata.name%3Db"),
            b"",
        );
        assert_eq!(l["items"].as_array().unwrap().len(), 1);
        let p = format!("{LEASES}/a");
        assert_eq!(s.handle("PATCH", &p, br#"{"spec":{"x":1}}"#).0, 200);
        assert_eq!(s.object(&p).unwrap()["spec"]["x"], 1);
        let ops = br#"[{"op":"test","path":"/spec/x","value":2}]"#;
        assert_eq!(s.handle("PATCH", &p, ops).0, 422);
        let ops = br#"[{"op":"test","path":"/spec/x","value":1},{"op":"remove","path":"/spec/x"}]"#;
        assert_eq!(s.handle("PATCH", &p, ops).0, 200);
        assert!(s.object(&p).unwrap()["spec"].get("x").is_none());
        assert_eq!(s.handle("DELETE", &p, b"").0, 200);
        assert_eq!(s.handle("DELETE", &p, b"").0, 404);
        assert!(matches!(&s.log.back().unwrap().kind, EventKind::Deleted));
        let start = 10;
        assert_eq!(
            s.events_since(LEASES, Some("b"), start).unwrap().len(),
            1,
            "history is kept until compaction"
        );
        s.compact();
        assert_eq!(s.events_since(LEASES, None, start).unwrap_err(), Gone);
    }

    #[test]
    fn faults_and_post_hook() {
        let mut s = Store::new();
        s.fail_next(&[500, 503]);
        assert_eq!(s.handle("GET", LEASES, b"").0, 500);
        assert_eq!(s.handle("GET", LEASES, b"").0, 503);
        assert_eq!(s.handle("GET", LEASES, b"").0, 200);
        s.on_post(|_, _| Some((201, json!({"status": {"authenticated": true}}))));
        assert_eq!(s.handle("POST", "/apis/review", b"{}").0, 201);
    }
}
