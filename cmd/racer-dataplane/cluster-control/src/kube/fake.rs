//! In-memory Kubernetes API server for tests.
//!
//! It stores JSON objects by path and supports GET, list, PUT with
//! resourceVersion CAS, POST, merge PATCH, JSON PATCH (add, replace, remove,
//! test), watch, `metadata.name` field selectors, and fault injection.

use super::{Kube, KubeConfig};
use crate::{
    exec::{self, Executor, Listener, Signal},
    http::{self, Conn, TlsStream},
    kube::str_at,
};
use serde_json::{Value, json};
use std::{
    cell::{Cell, RefCell},
    collections::BTreeMap,
    future::Future,
    rc::Rc,
    sync::Arc,
};

type Hook = Box<dyn Fn(&str, &Value) -> Option<(u16, Value)>>;

/// Shared fake server state.
#[derive(Clone)]
pub struct FakeApi(Rc<Inner>);

struct Inner {
    objects: RefCell<BTreeMap<String, Value>>,
    log: RefCell<Vec<(u64, String, String, Value)>>,
    version: Cell<u64>,
    compacted: Cell<u64>,
    changed: Signal,
    post_hook: RefCell<Option<Hook>>,
    fail_next: RefCell<Vec<u16>>,
    requests: RefCell<Vec<(String, String)>>,
    pub(crate) uids: Cell<u64>,
    counter: RefCell<Option<std::sync::Arc<std::sync::atomic::AtomicUsize>>>,
}

impl FakeApi {
    pub fn new() -> Self {
        Self(Rc::new(Inner {
            objects: RefCell::new(BTreeMap::new()),
            log: RefCell::new(Vec::new()),
            version: Cell::new(10),
            compacted: Cell::new(0),
            changed: Signal::default(),
            post_hook: RefCell::new(None),
            fail_next: RefCell::new(Vec::new()),
            requests: RefCell::new(Vec::new()),
            uids: Cell::new(1),
            counter: RefCell::new(None),
        }))
    }

    /// Count every request in `counter`, readable from other threads.
    pub fn count_into(&self, counter: std::sync::Arc<std::sync::atomic::AtomicUsize>) {
        *self.0.counter.borrow_mut() = Some(counter);
    }

    fn bump(&self) -> u64 {
        let v = self.0.version.get() + 1;
        self.0.version.set(v);
        v
    }

    /// Store an object, set its resourceVersion and a UID if missing.
    pub fn put_object(&self, path: &str, mut object: Value) -> Value {
        let v = self.bump();
        let existed = self.0.objects.borrow().contains_key(path);
        object["metadata"]["resourceVersion"] = v.to_string().into();
        if str_at(&object, &["metadata", "uid"]).is_empty() {
            let n = self.0.uids.get();
            self.0.uids.set(n + 1);
            object["metadata"]["uid"] = format!("00000000-0000-4000-8000-{n:012x}").into();
        }
        if str_at(&object, &["metadata", "name"]).is_empty() {
            object["metadata"]["name"] = path.rsplit('/').next().unwrap_or_default().into();
        }
        self.0
            .objects
            .borrow_mut()
            .insert(path.into(), object.clone());
        let kind = if existed { "MODIFIED" } else { "ADDED" };
        self.0
            .log
            .borrow_mut()
            .push((v, path.into(), kind.into(), object.clone()));
        self.0.changed.notify();
        object
    }

    #[allow(dead_code)]
    pub fn delete_object(&self, path: &str) {
        if let Some(o) = self.0.objects.borrow_mut().remove(path) {
            let v = self.bump();
            self.0
                .log
                .borrow_mut()
                .push((v, path.into(), "DELETED".into(), o));
            self.0.changed.notify();
        }
    }

    pub fn object(&self, path: &str) -> Option<Value> {
        self.0.objects.borrow().get(path).cloned()
    }

    /// Forget watch history so older resourceVersions get 410.
    pub fn compact(&self) {
        self.0.compacted.set(self.0.version.get());
    }

    /// Handle POSTs, for example TokenReview.
    pub fn on_post(&self, hook: impl Fn(&str, &Value) -> Option<(u16, Value)> + 'static) {
        *self.0.post_hook.borrow_mut() = Some(Box::new(hook));
    }

    /// Fail the next requests with these statuses.
    #[allow(dead_code)]
    pub fn fail_next(&self, statuses: &[u16]) {
        self.0.fail_next.borrow_mut().extend_from_slice(statuses);
    }

    /// Requests seen, as (method, target).
    pub fn requests(&self) -> Vec<(String, String)> {
        self.0.requests.borrow().clone()
    }

    fn list(&self, collection: &str, name: Option<&str>) -> Vec<Value> {
        let prefix = format!("{collection}/");
        self.0
            .objects
            .borrow()
            .iter()
            .filter(|(k, _)| k.starts_with(&prefix) && !k[prefix.len()..].contains('/'))
            .filter(|(_, v)| name.is_none_or(|n| str_at(v, &["metadata", "name"]) == n))
            .map(|(_, v)| v.clone())
            .collect()
    }

    fn handle(&self, method: &str, target: &str, body: &[u8]) -> (u16, Value) {
        self.0
            .requests
            .borrow_mut()
            .push((method.into(), target.into()));
        if let Some(c) = self.0.counter.borrow().as_ref() {
            c.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        }
        if let Some(s) = {
            let mut f = self.0.fail_next.borrow_mut();
            (!f.is_empty()).then(|| f.remove(0))
        } {
            return (s, json!({"kind":"Status","code":s}));
        }
        let (path, query) = http::split_target(target);
        let name = field_name(query.unwrap_or(""));
        let request: Value = if body.is_empty() {
            Value::Null
        } else {
            match serde_json::from_slice(body) {
                Ok(v) => v,
                Err(_) => return (400, json!({})),
            }
        };
        match method {
            "GET" => {
                if let Some(o) = self.object(path) {
                    return (200, o);
                }
                let items = self.list(path, name.as_deref());
                if items.is_empty() && !self.is_collection(path) {
                    return (404, json!({"code":404}));
                }
                (
                    200,
                    json!({"metadata":{"resourceVersion":self.0.version.get().to_string()},"items":items}),
                )
            }
            "PUT" => {
                let Some(current) = self.object(path) else {
                    return (404, json!({}));
                };
                let rv = str_at(&request, &["metadata", "resourceVersion"]);
                if rv != str_at(&current, &["metadata", "resourceVersion"]) {
                    return (409, json!({"code":409}));
                }
                let mut next = request.clone();
                next["metadata"]["uid"] = current["metadata"]["uid"].clone();
                (200, self.put_object(path, next))
            }
            "POST" => {
                if let Some(r) = self
                    .0
                    .post_hook
                    .borrow()
                    .as_ref()
                    .and_then(|h| h(path, &request))
                {
                    return r;
                }
                let name = str_at(&request, &["metadata", "name"]).to_owned();
                let full = format!("{path}/{name}");
                if name.is_empty() || self.object(&full).is_some() {
                    return (409, json!({}));
                }
                (201, self.put_object(&full, request))
            }
            "PATCH" => {
                let Some(mut current) = self.object(path) else {
                    return (404, json!({}));
                };
                if request.is_array() {
                    if let Err(s) = json_patch(&mut current, &request) {
                        return (s, json!({}));
                    }
                } else {
                    merge(&mut current, &request);
                }
                (200, self.put_object(path, current))
            }
            _ => (405, json!({})),
        }
    }

    fn is_collection(&self, path: &str) -> bool {
        path.ends_with('s')
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
        if from < self.0.compacted.get() {
            let _ = conn
                .tls
                .write_all(&send(
                    json!({"type":"ERROR","object":{"kind":"Status","code":410}}),
                ))
                .await;
            let _ = conn.tls.write_all(b"0\r\n\r\n").await;
            return;
        }
        let mut seen = from;
        let prefix = format!("{path}/");
        loop {
            let generation = self.0.changed.generation();
            let events: Vec<_> = self
                .0
                .log
                .borrow()
                .iter()
                .filter(|(v, p, _, o)| {
                    *v > seen
                        && p.starts_with(&prefix)
                        && name
                            .as_deref()
                            .is_none_or(|n| str_at(o, &["metadata", "name"]) == n)
                })
                .cloned()
                .collect();
            for (v, _, kind, object) in events {
                seen = v;
                if conn
                    .tls
                    .write_all(&send(json!({"type":kind,"object":object})))
                    .await
                    .is_err()
                {
                    return;
                }
            }
            seen = seen.max(self.0.version.get());
            self.0.changed.changed(generation).await;
        }
    }
}

fn field_name(query: &str) -> Option<String> {
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
        let t = target.as_object_mut().unwrap();
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

fn json_patch(target: &mut Value, ops: &Value) -> Result<(), u16> {
    for op in ops.as_array().ok_or(400u16)? {
        let path = op["path"].as_str().ok_or(400u16)?;
        let pointer = path.to_owned();
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
                let p = target.pointer_mut(parent).ok_or(422u16)?;
                match p {
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
                    let (status, value) = api.handle(&head.method, &head.target, &body);
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

static DIRS: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);

/// Project-local scratch directory for one test.
pub fn scratch() -> std::path::PathBuf {
    let n = DIRS.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
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
