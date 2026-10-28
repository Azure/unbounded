//! Plain-value Kubernetes API server model for tests and the simulator.
//!
//! [`Store`] keeps JSON objects by path and serves the API subset the control
//! plane uses: GET and list, PUT with resourceVersion CAS, POST, merge and
//! JSON PATCH, DELETE, `metadata.name` field selectors, and an event log for
//! watches with compaction (410 Gone). No sockets or clocks: callers pass
//! `now_ns`. Fault injection and an optional [`TokenBucket`] (429s, latency)
//! model a loaded server. The TLS test server (`kube::fake`) wraps it.

use serde_json::{Value, json};
use std::collections::{BTreeMap, VecDeque};

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

/// Token bucket applied before each request: `qps` sustained, `burst`
/// capacity, fixed latency.
#[derive(Clone, Debug)]
pub struct TokenBucket {
    qps: f64,
    burst: f64,
    latency_ns: u64,
    tokens: f64,
    at_ns: u64,
}

impl TokenBucket {
    pub fn new(qps: f64, burst: f64, latency_ns: u64) -> Self {
        Self {
            qps,
            burst,
            latency_ns,
            tokens: burst,
            at_ns: 0,
        }
    }

    /// Service latency, or `None` to throttle with 429.
    fn admit(&mut self, now_ns: u64) -> Option<u64> {
        let elapsed = now_ns.saturating_sub(self.at_ns) as f64 / 1e9;
        self.at_ns = self.at_ns.max(now_ns);
        self.tokens = (self.tokens + elapsed * self.qps).min(self.burst);
        if self.tokens >= 1.0 {
            self.tokens -= 1.0;
            Some(self.latency_ns)
        } else {
            None
        }
    }
}

/// Request counters.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Stats {
    pub requests: u64,
    pub throttled: u64,
}

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
    admission: Option<TokenBucket>,
    pub stats: Stats,
}

impl Default for Store {
    fn default() -> Self {
        Self::new()
    }
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
            admission: None,
            stats: Stats::default(),
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

    /// Objects stored.
    pub fn len(&self) -> usize {
        self.objects.len()
    }

    pub fn is_empty(&self) -> bool {
        self.objects.is_empty()
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

    /// Apply `admission` to every later request.
    pub fn set_admission(&mut self, admission: Option<TokenBucket>) {
        self.admission = admission;
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

    /// Serve one request. `target` is a path with an optional query. Returns
    /// the status, the body, and the modeled service latency.
    pub fn handle(
        &mut self,
        method: &str,
        target: &str,
        body: &[u8],
        now_ns: u64,
    ) -> (u16, Value, u64) {
        self.stats.requests += 1;
        if let Some(s) = self.faults.pop_front() {
            return (s, json!({"kind":"Status","code":s}), 0);
        }
        let (path, query) = match target.split_once('?') {
            Some((p, q)) => (p, Some(q)),
            None => (target, None),
        };
        let latency = match self.admission.as_mut().map(|a| a.admit(now_ns)) {
            Some(None) => {
                self.stats.throttled += 1;
                return (429, json!({"kind":"Status","code":429}), 0);
            }
            Some(Some(latency_ns)) => latency_ns,
            None => 0,
        };
        let request: Value = if body.is_empty() {
            Value::Null
        } else {
            match serde_json::from_slice(body) {
                Ok(v) => v,
                Err(_) => return (400, json!({}), latency),
            }
        };
        let (s, v) = self.apply(method, path, query, &request);
        (s, v, latency)
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

#[cfg(test)]
mod tests {
    use super::*;

    const LEASES: &str = "/apis/coordination.k8s.io/v1/namespaces/ns/leases";

    fn lease(name: &str) -> Value {
        json!({"metadata": {"name": name}, "spec": {}})
    }

    #[test]
    fn cas_put_and_create() {
        let mut s = Store::new();
        let body = serde_json::to_vec(&lease("a")).unwrap();
        let (st, created, _) = s.handle("POST", LEASES, &body, 0);
        assert_eq!(st, 201);
        assert_eq!(s.handle("POST", LEASES, &body, 0).0, 409);
        let path = format!("{LEASES}/a");
        let mut next = created.clone();
        next["spec"]["holderIdentity"] = "p".into();
        let (st, put, _) = s.handle("PUT", &path, &serde_json::to_vec(&next).unwrap(), 0);
        assert_eq!(st, 200);
        assert_eq!(put["metadata"]["uid"], created["metadata"]["uid"]);
        // Stale resourceVersion conflicts.
        assert_eq!(
            s.handle("PUT", &path, &serde_json::to_vec(&next).unwrap(), 0)
                .0,
            409
        );
        assert_eq!(s.handle("PUT", &format!("{LEASES}/x"), b"{}", 0).0, 404);
        assert_eq!(s.handle("GET", &format!("{LEASES}/x"), b"", 0).0, 404);
        assert_eq!(s.handle("GET", "/x/y", b"{", 0).0, 400);
    }

    #[test]
    fn list_selectors_and_patches() {
        let mut s = Store::new();
        s.put_object(&format!("{LEASES}/a"), lease("a"));
        s.put_object(&format!("{LEASES}/b"), lease("b"));
        s.put_object(&format!("{LEASES}/b/sub"), lease("sub"));
        let (_, l, _) = s.handle("GET", LEASES, b"", 0);
        assert_eq!(l["items"].as_array().unwrap().len(), 2);
        let (_, l, _) = s.handle(
            "GET",
            &format!("{LEASES}?fieldSelector=metadata.name%3Db"),
            b"",
            0,
        );
        assert_eq!(l["items"].as_array().unwrap().len(), 1);
        let p = format!("{LEASES}/a");
        assert_eq!(s.handle("PATCH", &p, br#"{"spec":{"x":1}}"#, 0).0, 200);
        assert_eq!(s.object(&p).unwrap()["spec"]["x"], 1);
        let ops = br#"[{"op":"test","path":"/spec/x","value":2}]"#;
        assert_eq!(s.handle("PATCH", &p, ops, 0).0, 422);
        let ops = br#"[{"op":"test","path":"/spec/x","value":1},{"op":"remove","path":"/spec/x"}]"#;
        assert_eq!(s.handle("PATCH", &p, ops, 0).0, 200);
        assert!(s.object(&p).unwrap()["spec"].get("x").is_none());
        assert_eq!(s.handle("DELETE", &p, b"", 0).0, 200);
        assert_eq!(s.handle("DELETE", &p, b"", 0).0, 404);
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
    fn faults_and_throttling() {
        let mut s = Store::new();
        s.fail_next(&[500, 503]);
        assert_eq!(s.handle("GET", LEASES, b"", 0).0, 500);
        assert_eq!(s.handle("GET", LEASES, b"", 0).0, 503);
        assert_eq!(s.handle("GET", LEASES, b"", 0).0, 200);
        s.set_admission(Some(TokenBucket::new(10.0, 2.0, 1_000)));
        assert_eq!(s.handle("GET", LEASES, b"", 0).2, 1_000);
        assert_eq!(s.handle("GET", LEASES, b"", 0).0, 200);
        assert_eq!(s.handle("GET", LEASES, b"", 0).0, 429);
        // 100 ms refills one token at 10 qps.
        assert_eq!(s.handle("GET", LEASES, b"", 100_000_000).0, 200);
        assert_eq!(s.stats.throttled, 1);
        assert_eq!(s.stats.requests, 7);
        s.on_post(|_, _| Some((201, json!({"status": {"authenticated": true}}))));
        assert_eq!(
            s.handle("POST", "/apis/review", b"{}", 1_000_000_000).0,
            201
        );
    }
}
