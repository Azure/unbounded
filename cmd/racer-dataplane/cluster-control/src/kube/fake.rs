//! In-memory Kubernetes API server for tests.
//!
//! It serves a plain-value [`crate::apiserver::Store`] (GET, list, PUT with
//! resourceVersion CAS, POST, merge and JSON PATCH, watch, field selectors,
//! fault injection) over TLS on loopback.

use super::{Kube, KubeConfig};
use crate::{
    apiserver::{Store, field_name},
    exec::{self, Executor, Listener, Signal},
    http::{self, Conn, TlsStream},
};
use serde_json::{Value, json};
use std::{cell::RefCell, future::Future, rc::Rc, sync::Arc};

/// Shared fake server state: a [`Store`] plus wakeups and test hooks.
#[derive(Clone)]
pub struct FakeApi(Rc<Inner>);

struct Inner {
    store: RefCell<Store>,
    changed: Signal,
    requests: RefCell<Vec<(String, String)>>,
    counter: RefCell<Option<std::sync::Arc<std::sync::atomic::AtomicUsize>>>,
    put_delay: RefCell<Option<std::sync::Arc<std::sync::atomic::AtomicU64>>>,
}

impl FakeApi {
    pub fn new() -> Self {
        Self(Rc::new(Inner {
            store: RefCell::new(Store::new()),
            changed: Signal::default(),
            requests: RefCell::new(Vec::new()),
            counter: RefCell::new(None),
            put_delay: RefCell::new(None),
        }))
    }

    /// Count every request in `counter`, readable from other threads.
    pub fn count_into(&self, counter: std::sync::Arc<std::sync::atomic::AtomicUsize>) {
        *self.0.counter.borrow_mut() = Some(counter);
    }

    /// Hold each PUT until it is `delay_ms` old, re-read while waiting;
    /// `u64::MAX` hangs PUTs as a stuck API server would.
    pub fn delay_puts(&self, delay_ms: std::sync::Arc<std::sync::atomic::AtomicU64>) {
        *self.0.put_delay.borrow_mut() = Some(delay_ms);
    }

    fn put_delay(&self, method: &str) -> std::time::Duration {
        let ms = self
            .0
            .put_delay
            .borrow()
            .as_ref()
            .filter(|_| method == "PUT")
            .map_or(0, |d| d.load(std::sync::atomic::Ordering::SeqCst));
        std::time::Duration::from_millis(ms)
    }

    /// Store an object, set its resourceVersion and a UID if missing.
    pub fn put_object(&self, path: &str, object: Value) -> Value {
        let o = self.0.store.borrow_mut().put_object(path, object);
        self.0.changed.notify();
        o
    }

    #[allow(dead_code)]
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
    #[allow(dead_code)]
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
            c.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        }
        let (status, value, _) = self.0.store.borrow_mut().handle(method, target, body, 0);
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
                    let received = std::time::Instant::now();
                    while received.elapsed() < api.put_delay(&head.method) {
                        exec::sleep(std::time::Duration::from_millis(20)).await;
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
