//! Plain-value Kubernetes API server model.
//!
//! [`Store`] keeps JSON objects by path and implements the subset of the
//! API the control plane uses: GET and list, PUT with resourceVersion CAS,
//! POST create, merge PATCH, JSON PATCH (add, replace, remove, test),
//! DELETE, `metadata.name` field selectors, and watches with a bounded event
//! log, compaction (410 Gone), and fan-out to many watchers. It has no
//! sockets, TLS, threads, or clocks: callers pass `now_ns` and drive
//! delivery, so the simulator can serve tens of thousands of clients from
//! one thread. The TLS test server (`kube::fake`) wraps it.
//!
//! Fault injection ([`Store::fail_next`]) and an optional [`TokenBucket`]
//! (API priority and fairness 429s, per-request latency) model a loaded API
//! server.

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

/// Watch handle.
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct WatchId(pub u64);

struct Watch {
    prefix: String,
    name: Option<String>,
    seen: u64,
}

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
    pub gets: u64,
    pub lists: u64,
    pub writes: u64,
    pub conflicts: u64,
    pub throttled: u64,
    pub injected: u64,
    pub watch_events: u64,
    pub gone: u64,
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
    watches: BTreeMap<WatchId, Watch>,
    next_watch: u64,
    /// Keep at most this many log events; older ones compact away.
    pub log_limit: usize,
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
            watches: BTreeMap::new(),
            next_watch: 0,
            log_limit: usize::MAX,
            stats: Stats::default(),
        }
    }

    /// Current resourceVersion.
    pub fn version(&self) -> u64 {
        self.version
    }

    /// Next UID counter, shared with callers that mint objects.
    pub fn next_uid(&mut self) -> u64 {
        let n = self.uids;
        self.uids += 1;
        n
    }

    fn bump(&mut self) -> u64 {
        self.version += 1;
        self.version
    }

    fn record(&mut self, event: Event) {
        self.log.push_back(event);
        while self.log.len() > self.log_limit {
            if let Some(e) = self.log.pop_front() {
                self.compacted = self.compacted.max(e.version);
            }
        }
    }

    /// Store an object, set its resourceVersion and a UID and name if missing.
    pub fn put_object(&mut self, path: &str, mut object: Value) -> Value {
        let v = self.bump();
        let existed = self.objects.contains_key(path);
        object["metadata"]["resourceVersion"] = v.to_string().into();
        if str_at(&object, &["metadata", "uid"]).is_empty() {
            let n = self.next_uid();
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
        self.record(Event {
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
        self.record(Event {
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
            self.stats.injected += 1;
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
                    self.stats.gets += 1;
                    return (200, o);
                }
                self.stats.lists += 1;
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
                    self.stats.conflicts += 1;
                    return (409, json!({"code":409}));
                }
                let mut next = request.clone();
                next["metadata"]["uid"] = current["metadata"]["uid"].clone();
                self.stats.writes += 1;
                (200, self.put_object(path, next))
            }
            "POST" => {
                if let Some(r) = self.post_hook.as_ref().and_then(|h| h(path, request)) {
                    return r;
                }
                let name = str_at(request, &["metadata", "name"]).to_owned();
                let full = format!("{path}/{name}");
                if name.is_empty() || self.object(&full).is_some() {
                    self.stats.conflicts += 1;
                    return (409, json!({}));
                }
                self.stats.writes += 1;
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
                self.stats.writes += 1;
                (200, self.put_object(path, current))
            }
            "DELETE" => match self.delete_object(path) {
                Some(o) => {
                    self.stats.writes += 1;
                    (200, o)
                }
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

    /// Open a watch from resourceVersion `from`.
    pub fn watch(
        &mut self,
        collection: &str,
        name: Option<&str>,
        from: u64,
    ) -> Result<WatchId, Gone> {
        if from < self.compacted {
            self.stats.gone += 1;
            return Err(Gone);
        }
        let id = WatchId(self.next_watch);
        self.next_watch += 1;
        self.watches.insert(
            id,
            Watch {
                prefix: format!("{collection}/"),
                name: name.map(Into::into),
                seen: from,
            },
        );
        Ok(id)
    }

    pub fn unwatch(&mut self, id: WatchId) {
        self.watches.remove(&id);
    }

    /// Open watches.
    pub fn watchers(&self) -> usize {
        self.watches.len()
    }

    /// Deliver new events to every watch. Watches that fell behind the log
    /// get `Err(Gone)` and close. Cost is O(watches x new events).
    pub fn deliver(&mut self) -> Vec<(WatchId, Result<Event, Gone>)> {
        let mut out = Vec::new();
        let mut gone = Vec::new();
        for (id, w) in &mut self.watches {
            if w.seen < self.compacted {
                gone.push(*id);
                out.push((*id, Err(Gone)));
                continue;
            }
            for e in self.log.iter().filter(|e| e.version > w.seen) {
                if e.path.starts_with(&w.prefix)
                    && w.name
                        .as_deref()
                        .is_none_or(|n| str_at(&e.object, &["metadata", "name"]) == n)
                {
                    out.push((*id, Ok(e.clone())));
                }
            }
            w.seen = self.version;
        }
        self.stats.watch_events += out.iter().filter(|(_, e)| e.is_ok()).count() as u64;
        self.stats.gone += gone.len() as u64;
        for id in gone {
            self.watches.remove(&id);
        }
        out
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
        assert_eq!(s.stats.conflicts, 2);
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
    }

    #[test]
    fn watches_fan_out_and_compact() {
        let mut s = Store::new();
        let start = s.version();
        let all = s.watch(LEASES, None, start).unwrap();
        let one = s.watch(LEASES, Some("b"), start).unwrap();
        s.put_object(&format!("{LEASES}/a"), lease("a"));
        s.put_object(&format!("{LEASES}/b"), lease("b"));
        s.put_object("/other/c", lease("c"));
        let got = s.deliver();
        assert_eq!(got.iter().filter(|(w, _)| *w == all).count(), 2);
        assert_eq!(got.iter().filter(|(w, _)| *w == one).count(), 1);
        assert!(s.deliver().is_empty(), "delivered once");
        s.delete_object(&format!("{LEASES}/b"));
        let got = s.deliver();
        assert!(matches!(&got[0].1, Ok(e) if e.kind == EventKind::Deleted));
        assert_eq!(
            s.events_since(LEASES, None, start).unwrap().len(),
            3,
            "history is kept until compaction"
        );
        s.compact();
        assert_eq!(s.events_since(LEASES, None, start).unwrap_err(), Gone);
        assert_eq!(s.watch(LEASES, None, start).unwrap_err(), Gone);
        // Live watches are caught up, so they stay open.
        assert!(s.deliver().is_empty());
        assert_eq!(s.watchers(), 2);
    }

    #[test]
    fn bounded_log_closes_lagging_watches() {
        let mut s = Store::new();
        s.log_limit = 2;
        let w = s.watch(LEASES, None, s.version()).unwrap();
        for n in 0..4 {
            s.put_object(&format!("{LEASES}/l{n}"), lease("l"));
        }
        let got = s.deliver();
        assert!(matches!(got.as_slice(), [(id, Err(Gone))] if *id == w));
        assert_eq!(s.watchers(), 0);
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
        assert_eq!(s.stats.injected, 2);
        s.on_post(|_, _| Some((201, json!({"status": {"authenticated": true}}))));
        assert_eq!(
            s.handle("POST", "/apis/review", b"{}", 1_000_000_000).0,
            201
        );
    }
}
