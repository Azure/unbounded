//! End-to-end write tests on a simulated cluster: the production worker graph
//! of every node, the client and peer sockets, and the simulated disks.

use super::*;
use crate::origin::upload::MemoryUploadSink;
use crate::origin::upload::UploadSink;
use racer_wire::control::WriteDefinition;
use racer_wire::object::write::UploadMode;
use std::collections::BTreeSet;
use std::time::Duration;

/// 64 KiB pages, so multi-page objects stay small.
fn small_pages() -> racer_wire::object::PageSize {
    racer_wire::object::PageSize::MIN
}

fn writes(replicas: u8) -> WriteDefinition {
    WriteDefinition {
        replicas,
        max_dirty_percent: 30,
        hold_ttl: Duration::from_secs(3600),
        upload_mode: UploadMode::Never,
        upload_max_bytes_per_second: 0,
        upload_max_concurrent_blocks: 16,
    }
}

/// Objects the origin fixture reports as existing (create-once checks).
#[derive(Default)]
struct Existing(RefCell<BTreeSet<CacheKey>>);

struct TestOrigin(Rc<Existing>);

impl crate::origin::Origin for TestOrigin {
    fn bootstrap_reserved<'a>(
        &'a self,
        _: &'a crate::read::candidates::OriginAuthority,
        _: &'a crate::security::OriginContext,
        _: flow_control::Charge<AdmissionPolicy>,
        _: &'a RequestScope,
    ) -> Operation<'a, crate::origin::MetadataReply> {
        Box::pin(async { Err(Error::Unavailable) })
    }

    fn page_reserved<'a>(
        &'a self,
        _: &'a crate::read::candidates::OriginAuthority,
        _: &'a crate::security::OriginContext,
        _: &'a PageId,
        _: flow_control::Charge<AdmissionPolicy>,
        _: &'a RequestScope,
    ) -> Operation<'a, crate::origin::OriginPage> {
        Box::pin(async { Err(Error::Unavailable) })
    }

    fn metadata<'a>(
        &'a self,
        _: &'a crate::read::candidates::OriginAuthority,
        context: &'a crate::security::OriginContext,
        _: MetadataSelector,
        _: &'a RequestScope,
    ) -> Operation<'a, crate::origin::MetadataReply> {
        let exists = self.0.0.borrow().contains(&context.object.key);
        let object = context.object.clone();
        Box::pin(async move {
            if !exists {
                return Err(Error::NotFound);
            }
            Ok(crate::origin::MetadataReply {
                metadata: ObjectMetadata {
                    content_type: None,
                    version: ObjectVersion {
                        object,
                        etag: StrongEtag::parse(b"\"origin\"").unwrap(),
                    },
                    length: 1,
                    expires_at: ExpiresAt::from_system_time(std::time::UNIX_EPOCH).unwrap(),
                },
                page_zero: None,
            })
        })
    }
}

/// A raw client exchange: one request, one response.
struct Raw {
    fd: Option<Descriptor>,
    request: Vec<u8>,
    sent: usize,
    /// Stop sending after this many bytes, then close (`None`: send all).
    cut: Option<usize>,
    response: Vec<u8>,
    releases: Vec<u8>,
    released: usize,
    cursor: Option<usize>,
}

impl Raw {
    /// Advance; true once the response is complete or the socket closed.
    fn poll(&mut self) -> bool {
        let Some(fd) = self.fd.as_ref() else {
            return true;
        };
        let fd = handle(fd);
        if self.released < self.releases.len()
            && let Ok(n) = fd.send(&self.releases[self.released..])
        {
            self.released += n;
        }
        let limit = self.cut.unwrap_or(self.request.len());
        if self.sent < limit {
            match fd.send(&self.request[self.sent..limit]) {
                Ok(n) => self.sent += n,
                Err(e) if would_block(&e) => {}
                // The server answered early and stopped reading; read on.
                Err(_) => self.sent = limit,
            }
        }
        if self.cut.is_some_and(|cut| self.sent >= cut) {
            // A client that dies mid-body.
            self.fd = None;
            return true;
        }
        let mut bytes = [0; 65536];
        match fd.recv(&mut bytes) {
            Ok(0) => return true,
            Ok(n) => self.response.extend_from_slice(&bytes[..n]),
            Err(e) if would_block(&e) => return false,
            Err(_) => return !self.response.is_empty(),
        }
        let Some(end) = self.response.windows(4).position(|w| w == b"\r\n\r\n") else {
            return false;
        };
        let (_, h, end) = headers(&self.response[..end + 4]);
        let length: usize = h.get("content-length").map_or(0, |l| l.parse().unwrap());
        if h.contains_key("racer-range-start") {
            let mut cursor = self.cursor.unwrap_or(end);
            while cursor + 21 <= self.response.len() {
                let count =
                    u32::from_be_bytes(self.response[cursor + 17..cursor + 21].try_into().unwrap())
                        as usize;
                if cursor + 21 + count > self.response.len() {
                    break;
                }
                if self.response[cursor] == 1 {
                    self.releases
                        .extend_from_slice(&self.response[cursor + 1..cursor + 9]);
                    self.releases
                        .extend_from_slice(&(count as u32).to_be_bytes());
                }
                cursor += 21 + count;
            }
            self.cursor = Some(cursor);
        }
        self.response.len() >= end + length
    }
}

/// A parsed response: status, lowercase headers, body.
struct Response {
    status: u16,
    headers: BTreeMap<String, String>,
    body: Vec<u8>,
}

impl Response {
    fn header(&self, name: &str) -> Option<&str> {
        self.headers.get(name).map(String::as_str)
    }

    /// The object bytes of a framed subscription response.
    fn object(&self) -> Vec<u8> {
        let mut out = Vec::new();
        let mut cursor = 0;
        while cursor + 21 <= self.body.len() {
            let kind = self.body[cursor];
            let count = u32::from_be_bytes(self.body[cursor + 17..cursor + 21].try_into().unwrap())
                as usize;
            cursor += 21;
            if kind == 2 {
                break;
            }
            out.extend_from_slice(&self.body[cursor..cursor + count]);
            cursor += count;
        }
        out
    }
}

/// Deterministic object bytes.
fn bytes(seed: u64, length: usize) -> Vec<u8> {
    let mut random = Random(seed);
    (0..length).map(|_| random.next() as u8).collect()
}

/// Write test objects are numbered from here, away from the origin fixture.
const OBJECT: usize = 0x100;

struct Cluster {
    harness: Harness,
    env: racer_io::Env,
    existing: Rc<Existing>,
    sink: Rc<MemoryUploadSink>,
}

impl Cluster {
    fn new(seed: u64, nodes: usize, writes: Option<WriteDefinition>) -> Self {
        Self::build(seed, nodes, writes, false)
    }

    fn build(seed: u64, nodes: usize, writes: Option<WriteDefinition>, native: bool) -> Self {
        let sim = SimOs::new();
        let clock = racer_io::SimClock::new(seed);
        let env = racer_io::Env::sim(&sim, clock.ctx(0));
        let mut harness = Harness::new(seed, sim, clock, native);
        harness.writes = writes;
        harness.page_size = small_pages();
        // A fixed worker count keeps each node's storage layout, and so its
        // write journal, across restarts.
        harness.workers = Some(2);
        let mut cluster = Self {
            harness,
            env,
            existing: Rc::default(),
            sink: Rc::new(MemoryUploadSink::new()),
        };
        for _ in 0..nodes {
            cluster.harness.add(&cluster.env, None);
        }
        cluster.configure();
        cluster.harness.publish();
        cluster.harness.settle();
        // Drop missing holders on the next repair turn unless a test asks
        // for a grace period.
        cluster.settings(|s| s.holder_grace = Duration::ZERO);
        cluster
    }

    /// Install the upload sink and the origin fixture on every worker.
    fn configure(&mut self) {
        for node in &self.harness.nodes {
            for worker in &node.workers {
                let writes = worker.app.coordinator.writes();
                writes.set_sink(Some(self.sink.clone() as Rc<dyn UploadSink>));
                writes.set_test_origin(Some(Rc::new(TestOrigin(self.existing.clone()))));
            }
        }
    }

    fn settings(&mut self, change: impl Fn(&mut crate::config::WriteSettings)) {
        for node in &self.harness.nodes {
            for worker in &node.workers {
                let writes = worker.app.coordinator.writes();
                let mut settings = writes.settings();
                change(&mut settings);
                writes.set_settings(settings);
            }
        }
    }

    fn connect(&mut self, node: usize) -> Descriptor {
        self.harness
            .sim
            .connect(SocketAddress::Unix(
                cache(self.harness.nodes[node].id).client_socket,
            ))
            .unwrap()
    }

    fn exchange(&mut self, node: usize, request: Vec<u8>) -> Response {
        let fd = self.connect(node);
        let mut raw = Raw {
            fd: Some(fd),
            request,
            sent: 0,
            cut: None,
            response: Vec::new(),
            releases: Vec::new(),
            released: 0,
            cursor: None,
        };
        for _ in 0..MAX_TURNS {
            self.harness.tick();
            if raw.poll() {
                raw.fd.take();
                self.harness.settle();
                let (start, headers, end) = headers(&raw.response);
                let status = start.split_whitespace().nth(1).unwrap().parse().unwrap();
                return Response {
                    status,
                    headers,
                    body: raw.response[end..].to_vec(),
                };
            }
        }
        panic!("write exchange stalled");
    }

    fn put(&mut self, node: usize, object: usize, body: &[u8], fields: &str) -> Response {
        let mut request = format!(
            "PUT /v1/objects/{} HTTP/1.1\r\nHost: racer\r\nContent-Length: {}\r\nIf-None-Match: *\r\nRacer-Metadata: dst opaque metadata\r\nAuthorization: Bearer dst-fixture\r\n{fields}Connection: close\r\n\r\n",
            key(object),
            body.len()
        )
        .into_bytes();
        request.extend_from_slice(body);
        self.exchange(node, request)
    }

    fn simple(&mut self, node: usize, method: &str, path: &str) -> Response {
        let request = format!(
            "{method} /v1/objects/{path} HTTP/1.1\r\nHost: racer\r\nRacer-Metadata: dst opaque metadata\r\nAuthorization: Bearer dst-fixture\r\nConnection: close\r\n\r\n"
        );
        self.exchange(node, request.into_bytes())
    }

    fn head(&mut self, node: usize, object: usize) -> Response {
        self.simple(node, "HEAD", &key(object))
    }

    fn delete(&mut self, node: usize, object: usize) -> Response {
        self.simple(node, "DELETE", &key(object))
    }

    fn persist(&mut self, node: usize, object: usize) -> Response {
        let request = format!(
            "POST /v1/objects/{}/persist HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\nRacer-Metadata: dst opaque metadata\r\nAuthorization: Bearer dst-fixture\r\nConnection: close\r\n\r\n",
            key(object)
        );
        self.exchange(node, request.into_bytes())
    }

    fn get(&mut self, node: usize, object: usize, length: usize) -> Response {
        let request = format!(
            "POST /v1/objects/{} HTTP/1.1\r\nHost: racer\r\n{SUBSCRIPTION_FIELDS}Range: bytes=0-{}\r\nRacer-Metadata: dst opaque metadata\r\nAuthorization: Bearer dst-fixture\r\nConnection: close\r\n\r\n",
            key(object),
            length - 1
        );
        self.exchange(node, request.into_bytes())
    }

    fn object(&self, object: usize) -> ObjectId {
        ObjectId {
            cache: self.harness.definition(self.harness.nodes[0].id).id,
            key: CacheKey::parse_hex(key(object).as_bytes()).unwrap(),
        }
    }

    /// Nodes holding a live written copy of each page of `version`.
    fn holders(&self, version: &ObjectVersion, pages: u64) -> Vec<Vec<usize>> {
        (0..pages)
            .map(|number| {
                let page = PageId {
                    version: version.clone(),
                    number: PageNumber(number),
                };
                self.harness
                    .nodes
                    .iter()
                    .enumerate()
                    .filter(|(_, node)| {
                        node.workers
                            .iter()
                            .any(|w| w.app.store.writer.written(&page).is_some())
                    })
                    .map(|(i, _)| i)
                    .collect()
            })
            .collect()
    }

    /// Live written pages on every node.
    fn written(&self) -> usize {
        self.harness
            .nodes
            .iter()
            .flat_map(|n| &n.workers)
            .map(|w| w.app.store.writer.written_pages().len())
            .sum()
    }

    /// Written sessions known to any worker.
    fn sessions(&self) -> usize {
        self.harness
            .nodes
            .iter()
            .flat_map(|n| &n.workers)
            .map(|w| w.app.coordinator.writes().directory().len())
            .sum()
    }

    /// Per node, the role, phase, and upload state of every tracked session.
    fn roles(
        &self,
    ) -> Vec<(
        usize,
        crate::write::directory::Role,
        racer_core::write::Phase,
        racer_core::write::UploadState,
    )> {
        let mut out = Vec::new();
        for (index, node) in self.harness.nodes.iter().enumerate() {
            for worker in &node.workers {
                let directory = worker.app.coordinator.writes().directory().clone();
                for object in directory.objects() {
                    directory.with(&object, |e| {
                        let e = e.unwrap();
                        out.push((index, e.role, e.session.phase(), e.session.state()));
                    });
                }
            }
        }
        out
    }

    fn version(&self, object: usize, response: &Response) -> ObjectVersion {
        ObjectVersion {
            object: self.object(object),
            etag: StrongEtag::parse(response.header("etag").unwrap().as_bytes()).unwrap(),
        }
    }

    /// Let maintenance turns run for `duration` of virtual time.
    fn wait(&mut self, duration: Duration) {
        self.harness.clock.advance(duration);
        for _ in 0..400 {
            self.harness.tick();
        }
        self.harness.settle();
    }
}

fn pages(length: usize) -> u64 {
    (length as u64).div_ceil(small_pages().get())
}

#[test]
fn put_then_read_round_trips_on_every_node() {
    let mut c = Cluster::new(11, 3, Some(writes(2)));
    let body = bytes(1, 200_000);
    let created = c.put(
        0,
        OBJECT,
        &body,
        "Racer-Content-Type: application/x-test\r\n",
    );
    assert_eq!(created.status, 201, "{:?}", created.headers);
    let etag = created.header("etag").unwrap().to_owned();
    assert!(etag.starts_with("\"racer-"), "{etag}");
    assert_eq!(created.header("racer-upload-state"), Some("never"));
    let version = c.version(OBJECT, &created);
    let holders = c.holders(&version, pages(body.len()));
    assert!(holders.iter().all(|h| h.len() == 2), "{holders:?}");
    for node in 0..3 {
        let read = c.get(node, OBJECT, body.len());
        assert_eq!(read.status, 200, "node {node}");
        assert_eq!(read.header("etag"), Some(etag.as_str()));
        assert_eq!(
            read.header("content-type"),
            Some("application/octet-stream")
        );
        let got = read.object();
        if got != body {
            let mut diagnostics = String::new();
            for node in &c.harness.nodes {
                for worker in &node.workers {
                    worker
                        .app
                        .telemetry
                        .failures
                        .write(&mut diagnostics)
                        .unwrap();
                }
            }
            eprintln!("{diagnostics}");
        }
        assert!(
            got == body,
            "node {node} bytes: got {} want {} first diff {:?} headers {:?} body {}",
            got.len(),
            body.len(),
            got.iter().zip(&body).position(|(a, b)| a != b),
            read.headers,
            read.body.len()
        );
        let head = c.head(node, OBJECT);
        assert_eq!(head.status, 200);
        assert_eq!(head.header("etag"), Some(etag.as_str()));
        assert_eq!(head.header("content-length"), Some("200000"));
        assert_eq!(
            head.header("racer-content-type"),
            Some("application/x-test")
        );
        assert_eq!(head.header("racer-upload-state"), Some("never"));
    }
    let metrics = c.harness.nodes[0].workers[0]
        .app
        .telemetry
        .metrics
        .writes()
        .clone();
    assert_eq!(
        metrics.requests(
            &c.object(OBJECT).cache.0,
            crate::write::metrics::Outcome::Created
        ),
        1
    );
}

#[test]
fn replicas_place_exactly_that_many_holders() {
    let mut c = Cluster::new(12, 3, Some(writes(2)));
    for replicas in 1..=3u8 {
        let object = OBJECT + usize::from(replicas);
        let body = bytes(u64::from(replicas), 150_000);
        let created = c.put(
            usize::from(replicas) % 3,
            object,
            &body,
            &format!("Racer-Durability: replicas={replicas}\r\n"),
        );
        assert_eq!(
            created.status, 201,
            "replicas={replicas}: {:?}",
            created.headers
        );
        let version = c.version(object, &created);
        let holders = c.holders(&version, pages(body.len()));
        assert!(
            holders.iter().all(|h| h.len() == usize::from(replicas)),
            "replicas={replicas}: {holders:?}"
        );
        if replicas == 3 {
            // With three nodes, the writer is always one of the store targets.
            let writer = usize::from(replicas) % 3;
            assert!(holders.iter().all(|h| h.contains(&writer)));
        }
        let read = c.get((usize::from(replicas) + 1) % 3, object, body.len());
        assert!(read.object() == body);
    }
}

#[test]
fn create_once_conflicts_are_412() {
    let mut c = Cluster::new(13, 3, Some(writes(2)));
    let body = bytes(2, 70_000);
    assert_eq!(c.put(0, OBJECT, &body, "").status, 201);
    // A second write of the key, from any node.
    let again = c.put(1, OBJECT, &body, "");
    assert_eq!(again.status, 412);
    assert_eq!(again.header("content-length"), Some("0"));
    // The origin already has another key.
    c.existing.0.borrow_mut().insert(c.object(OBJECT + 1).key);
    assert_eq!(c.put(2, OBJECT + 1, &body, "").status, 412);
    assert_eq!(c.written(), 2 * pages(body.len()) as usize);
    // Missing If-None-Match is 428.
    let request = format!(
        "PUT /v1/objects/{} HTTP/1.1\r\nHost: racer\r\nContent-Length: 1\r\nConnection: close\r\n\r\nx",
        key(OBJECT + 2)
    );
    assert_eq!(c.exchange(0, request.into_bytes()).status, 428);
}

#[test]
fn never_writes_without_metadata_skip_the_origin_check() {
    let mut c = Cluster::new(14, 3, Some(writes(2)));
    let body = bytes(3, 70_000);
    // The origin reports both keys as existing.
    for object in [OBJECT, OBJECT + 1] {
        c.existing.0.borrow_mut().insert(c.object(object).key);
    }
    // Without `Racer-Metadata` a `never` write cannot reach the origin, so
    // the origin create-once check is skipped.
    let request = |object| {
        let mut request = format!(
            "PUT /v1/objects/{} HTTP/1.1\r\nHost: racer\r\nContent-Length: {}\r\nIf-None-Match: *\r\nRacer-Upload: never\r\nConnection: close\r\n\r\n",
            key(object),
            body.len()
        )
        .into_bytes();
        request.extend_from_slice(&body);
        request
    };
    assert_eq!(c.exchange(0, request(OBJECT)).status, 201);
    // Cache-level create-once still applies.
    assert_eq!(c.exchange(1, request(OBJECT)).status, 412);
    // With metadata, the origin check runs.
    assert_eq!(
        c.put(2, OBJECT + 1, &body, "Racer-Upload: never\r\n")
            .status,
        412
    );
    assert_eq!(c.written(), 2 * pages(body.len()) as usize);
}

#[test]
fn writes_disabled_or_passthrough_are_405() {
    for passthrough in [false, true] {
        let sim = SimOs::new();
        let clock = racer_io::SimClock::new(14);
        let env = racer_io::Env::sim(&sim, clock.ctx(0));
        let mut harness = Harness::new(14, sim, clock, false);
        if passthrough {
            harness.mode = racer_wire::control::CacheMode::PassthroughLocal;
        }
        let mut c = Cluster {
            harness,
            env,
            existing: Rc::default(),
            sink: Rc::new(MemoryUploadSink::new()),
        };
        c.harness.add(&c.env, None);
        c.harness.settle();
        // Exactly what a node without writes answers.
        for response in [
            c.put(0, OBJECT, b"abc", ""),
            c.delete(0, OBJECT),
            c.persist(0, OBJECT),
        ] {
            assert_eq!(response.status, 405);
            assert_eq!(response.header("allow"), Some("HEAD, POST"));
        }
    }
}

#[test]
fn a_full_dirty_cap_is_503() {
    let mut definition = writes(3);
    definition.max_dirty_percent = 1;
    let mut c = Cluster::new(15, 3, Some(definition));
    let body = bytes(3, 8 * 1024 * 1024);
    let response = c.put(0, OBJECT, &body, "");
    assert_eq!(response.status, 503);
    assert_eq!(response.header("racer-write-error"), Some("dirty-cap"));
    assert_eq!(response.header("retry-after"), Some("1"));
    // The aborted write released what it stored and freed the key.
    assert_eq!(c.written(), 0);
    assert_eq!(c.sessions(), 0);
    assert_eq!(c.put(0, OBJECT, &bytes(4, 1000), "").status, 201);
}

#[test]
fn a_client_disconnect_mid_body_aborts_and_releases() {
    let mut c = Cluster::new(16, 3, Some(writes(2)));
    let body = bytes(5, 400_000);
    let mut request = format!(
        "PUT /v1/objects/{} HTTP/1.1\r\nHost: racer\r\nContent-Length: {}\r\nIf-None-Match: *\r\nConnection: close\r\n\r\n",
        key(OBJECT),
        body.len()
    )
    .into_bytes();
    let cut = request.len() + 300_000;
    request.extend_from_slice(&body);
    let fd = c.connect(0);
    let mut raw = Raw {
        fd: Some(fd),
        request,
        sent: 0,
        cut: Some(cut),
        response: Vec::new(),
        releases: Vec::new(),
        released: 0,
        cursor: None,
    };
    for _ in 0..MAX_TURNS {
        c.harness.tick();
        if raw.poll() {
            break;
        }
    }
    c.harness.settle();
    assert_eq!(c.written(), 0);
    assert_eq!(c.sessions(), 0);
    let again = c.put(1, OBJECT, &body, "");
    assert_eq!(again.status, 201, "{:?}", again.headers);
}

#[test]
fn an_expired_lease_aborts_the_write() {
    let mut c = Cluster::new(17, 3, Some(writes(2)));
    c.settings(|s| s.lease = Duration::from_secs(5));
    let body = bytes(6, 300_000);
    let mut request = format!(
        "PUT /v1/objects/{} HTTP/1.1\r\nHost: racer\r\nContent-Length: {}\r\nIf-None-Match: *\r\nConnection: close\r\n\r\n",
        key(OBJECT),
        body.len()
    )
    .into_bytes();
    let head = request.len();
    request.extend_from_slice(&body);
    let fd = c.connect(0);
    let mut raw = Raw {
        fd: Some(fd),
        request,
        sent: 0,
        cut: None,
        response: Vec::new(),
        releases: Vec::new(),
        released: 0,
        cursor: None,
    };
    // Send the head and part of the body, then stall past the lease.
    raw.request.truncate(head + 100_000);
    for _ in 0..2_000 {
        c.harness.tick();
        raw.poll();
    }
    c.harness.clock.advance(Duration::from_secs(10));
    for _ in 0..2_000 {
        c.harness.tick();
        raw.poll();
    }
    // The coordinator aborted: no session remains.
    assert_eq!(c.sessions(), 0);
    raw.request.extend_from_slice(&body[100_000..]);
    for _ in 0..MAX_TURNS {
        c.harness.tick();
        if raw.poll() {
            break;
        }
    }
    raw.fd.take();
    c.harness.settle();
    let (start, headers, _) = headers(&raw.response);
    assert!(start.contains(" 503"), "{start}");
    assert!(
        matches!(
            headers.get("racer-write-error").map(String::as_str),
            Some("lease" | "coordinator")
        ),
        "{headers:?}"
    );
    assert_eq!(c.written(), 0);
    assert_eq!(c.sessions(), 0);
}

#[test]
fn head_reports_upload_states_and_uploads_commit() {
    let mut c = Cluster::new(18, 3, Some(writes(2)));
    let manual = bytes(7, 130_000);
    let created = c.put(0, OBJECT, &manual, "Racer-Upload: manual\r\n");
    assert_eq!(created.status, 201);
    assert_eq!(created.header("racer-upload-state"), Some("manual"));
    assert_eq!(
        c.head(1, OBJECT).header("racer-upload-state"),
        Some("manual")
    );
    // A never object refuses persist.
    assert_eq!(
        c.put(0, OBJECT + 1, &manual, "Racer-Upload: never\r\n")
            .status,
        201
    );
    let refused = c.persist(2, OBJECT + 1);
    assert_eq!(refused.status, 409);
    assert_eq!(refused.header("racer-write-error"), Some("not-manual"));
    assert_eq!(c.persist(2, OBJECT + 9).status, 404);
    // Persist uploads every page and commits the block list.
    let persisted = c.persist(2, OBJECT);
    assert_eq!(persisted.status, 204);
    assert_eq!(persisted.header("racer-upload-state"), Some("uploading"));
    c.wait(Duration::from_secs(1));
    let blob = c.sink.committed(&c.object(OBJECT)).expect("blob committed");
    assert!(blob.data == manual);
    // After the upload the holders keep no write pins and the object is not
    // deletable.
    let version = c.version(OBJECT, &created);
    assert!(
        c.holders(&version, pages(manual.len()))
            .iter()
            .all(Vec::is_empty)
    );
    let refused = c.delete(1, OBJECT);
    assert_eq!(refused.status, 409);
    assert_eq!(refused.header("racer-write-error"), Some("uploaded"));
    // Automatic upload starts at commit.
    let auto = bytes(8, 90_000);
    let created = c.put(1, OBJECT + 2, &auto, "Racer-Upload: auto\r\n");
    assert_eq!(created.status, 201);
    c.wait(Duration::from_secs(1));
    assert!(
        c.sink
            .committed(&c.object(OBJECT + 2))
            .expect("auto blob")
            .data
            == auto
    );
    // `origin` durability acknowledges after the commit.
    let origin = bytes(9, 70_000);
    let created = c.put(
        2,
        OBJECT + 3,
        &origin,
        "Racer-Upload: auto\r\nRacer-Durability: origin\r\n",
    );
    assert_eq!(created.status, 201);
    assert_eq!(created.header("racer-upload-state"), Some("committed"));
    assert!(
        c.sink
            .committed(&c.object(OBJECT + 3))
            .expect("origin blob")
            .data
            == origin
    );
}

#[test]
fn head_after_an_upload_finished_reports_committed() {
    let mut c = Cluster::new(23, 3, Some(writes(2)));
    let body = bytes(11, 90_000);
    let created = c.put(0, OBJECT, &body, "Racer-Upload: auto\r\n");
    assert_eq!(created.status, 201);
    let racer_etag = created.header("etag").map(str::to_owned);
    // The upload finishes before anyone polls.
    c.wait(Duration::from_secs(2));
    let blob = c.sink.committed(&c.object(OBJECT)).expect("blob committed");
    let blob_etag = String::from_utf8(blob.etag.as_bytes().to_vec()).unwrap();
    // The origin now serves the uploaded blob under its Blob ETag.
    c.harness.catalog.borrow_mut().current.insert(
        OBJECT,
        super::Version {
            tag: blob_etag.clone(),
            bytes: blob.data,
        },
    );
    for node in 0..3 {
        // Fresh metadata is the origin's: the Blob ETag, still committed.
        let head = c.head(node, OBJECT);
        assert_eq!(head.status, 200, "node {node}");
        assert_eq!(head.header("etag"), Some(blob_etag.as_str()));
        assert_ne!(head.header("etag").map(str::to_owned), racer_etag);
        assert_eq!(head.header("racer-upload-state"), Some("committed"));
    }
    // An origin version no session committed as reports no upload state.
    c.harness.catalog.borrow_mut().current.insert(
        OBJECT,
        super::Version {
            tag: "\"replaced\"".into(),
            bytes: bytes(12, 10),
        },
    );
    c.wait(Duration::from_secs(2));
    let head = c.head(0, OBJECT);
    assert_eq!(head.status, 200);
    assert_eq!(head.header("etag"), Some("\"replaced\""));
    assert_eq!(head.header("racer-upload-state"), None);
}

#[test]
fn head_succeeds_without_upload_state_when_the_coordinator_stalls() {
    let mut c = Cluster::new(24, 3, Some(writes(2)));
    let body = bytes(13, 70_000);
    let created = c.put(0, OBJECT, &body, "Racer-Upload: manual\r\n");
    assert_eq!(created.status, 201);
    let etag = created.header("etag").map(str::to_owned);
    // Every coordinator answers Status only well past the lookup's
    // allowance. An unbounded lookup would hold the HEAD for the whole stall
    // and report the state; past the request timeout it would fail the HEAD.
    for node in &c.harness.nodes {
        for worker in &node.workers {
            let writes = worker.app.coordinator.writes();
            writes.set_status_stall(Some(Duration::from_secs(10)));
        }
    }
    for node in 0..3 {
        let head = c.head(node, OBJECT);
        assert_eq!(head.status, 200, "node {node}");
        assert_eq!(head.header("etag").map(str::to_owned), etag);
        assert_eq!(head.header("racer-upload-state"), None, "node {node}");
    }
    // Once the coordinator answers again, so does the header.
    for node in &c.harness.nodes {
        for worker in &node.workers {
            worker.app.coordinator.writes().set_status_stall(None);
        }
    }
    c.wait(Duration::from_secs(2));
    assert_eq!(
        c.head(1, OBJECT).header("racer-upload-state"),
        Some("manual")
    );
}

#[test]
fn a_retried_block_list_after_a_lost_response_commits() {
    let mut c = Cluster::new(19, 3, Some(writes(2)));
    // The first Put Block List commits but its 201 never arrives; the retry
    // conflicts with the blob it created and must recognize it as its own.
    c.sink.lose_next_commit_response();
    let body = bytes(10, 90_000);
    let created = c.put(0, OBJECT, &body, "Racer-Upload: auto\r\n");
    assert_eq!(created.status, 201);
    c.wait(Duration::from_secs(5));
    assert!(block_list_commits(&c.sink) >= 2);
    let blob = c.sink.committed(&c.object(OBJECT)).expect("blob committed");
    assert!(blob.data == body);
    // Committed, not failed: the object is uploaded and not deletable.
    let refused = c.delete(1, OBJECT);
    assert_eq!(refused.status, 409);
    assert_eq!(refused.header("racer-write-error"), Some("uploaded"));
}

#[test]
fn delete_releases_holders() {
    let mut c = Cluster::new(19, 3, Some(writes(2)));
    let body = bytes(10, 140_000);
    let created = c.put(0, OBJECT, &body, "");
    assert_eq!(created.status, 201);
    assert_eq!(c.written(), 2 * pages(body.len()) as usize);
    let deleted = c.delete(1, OBJECT);
    assert_eq!(deleted.status, 204);
    c.wait(Duration::from_secs(2));
    assert_eq!(c.written(), 0);
    assert_eq!(c.sessions(), 0);
    assert_eq!(c.delete(2, OBJECT).status, 404);
    // Fresh reads no longer find it, and the key can be written again.
    assert_eq!(c.head(2, OBJECT).status, 404);
    assert_eq!(c.put(2, OBJECT, &body, "").status, 201);
}

#[test]
fn holds_expire() {
    let mut c = Cluster::new(20, 3, Some(writes(2)));
    let body = bytes(11, 100_000);
    assert_eq!(
        c.put(0, OBJECT, &body, "Racer-Hold-Seconds: 30\r\n").status,
        201
    );
    assert!(c.written() > 0);
    c.wait(Duration::from_secs(31));
    c.wait(Duration::from_secs(1));
    assert_eq!(c.written(), 0);
    assert_eq!(c.sessions(), 0, "{:?}", c.roles());
    assert_eq!(c.head(1, OBJECT).status, 404);
}

#[test]
fn written_objects_survive_a_crash_restart() {
    let mut c = Cluster::new(21, 3, Some(writes(2)));
    let body = bytes(12, 260_000);
    let created = c.put(0, OBJECT, &body, "");
    assert_eq!(created.status, 201);
    let etag = created.header("etag").unwrap().to_owned();
    let version = c.version(OBJECT, &created);
    let holders = c.holders(&version, pages(body.len()));
    // Crash every node in turn (power loss), then restart it from its disk.
    for index in 0..3 {
        let id = c.harness.retire(&c.env, index, true);
        c.harness.add(&c.env, Some(id));
        let restarted = c.harness.nodes.pop().unwrap();
        c.harness.nodes.insert(index, restarted);
        c.configure();
        c.harness.publish();
        c.harness.settle();
        // The journal restored every durable copy the node held.
        assert_eq!(
            c.holders(&version, pages(body.len())),
            holders,
            "node {index}"
        );
        // Readers on the restarted node, then everywhere, see the object
        // once peers answer the restarted node again.
        for reader in [index, (index + 1) % 3, (index + 2) % 3] {
            let mut read = c.get(reader, OBJECT, body.len());
            for _ in 0..10 {
                if read.status == 200 {
                    break;
                }
                c.wait(Duration::from_secs(2));
                read = c.get(reader, OBJECT, body.len());
            }
            assert_eq!(read.status, 200, "restarted {index}, reader {reader}");
            assert_eq!(read.header("etag"), Some(etag.as_str()));
            assert!(read.object() == body, "restarted {index}, reader {reader}");
        }
    }
}

/// Store targets pull written pages with the same directed copy-only page
/// request reads use, so a pull rides an RDMA link when one is up.
#[test]
fn store_targets_pull_over_rdma_links() {
    let mut c = Cluster::build(22, 3, Some(writes(2)), true);
    for a in 0..3 {
        for b in 0..3 {
            if a != b {
                c.harness.connect_links(&c.env.clone(), a, b);
            }
        }
    }
    let before = c.harness.coverage.native_writes;
    let body = bytes(13, 200_000);
    let created = c.put(0, OBJECT, &body, "Racer-Durability: replicas=3\r\n");
    assert_eq!(created.status, 201);
    // Two remote targets pulled every page from the writer over RDMA.
    assert!(
        c.harness.coverage.native_writes >= before + 2 * pages(body.len()) as usize,
        "{} -> {}",
        before,
        c.harness.coverage.native_writes
    );
    let read = c.get(1, OBJECT, body.len());
    assert!(read.object() == body);
}

// WP8: repair, coordinator failover, rekey, and garbage collection.

impl Cluster {
    /// Crash node `index` (power loss) and drop it from the membership.
    fn crash(&mut self, index: usize) {
        self.harness.retire(&self.env, index, true);
        self.harness.settle();
    }

    /// The node index that coordinates the written object, if exactly one.
    fn coordinator(&self) -> Option<usize> {
        let coordinators: Vec<usize> = self
            .roles()
            .into_iter()
            .filter(|(_, role, ..)| *role == crate::write::directory::Role::Coordinator)
            .map(|(node, ..)| node)
            .collect();
        (coordinators.len() == 1).then(|| coordinators[0])
    }

    /// One cache key of epoch `epoch`, as `Harness::bundle` mints it.
    fn cache_key(
        &self,
        epoch: u8,
        index: usize,
        state: wire::CacheKeyState,
    ) -> wire::CacheEncryptionKey {
        let purpose = [
            wire::CacheKeyPurpose::Page,
            wire::CacheKeyPurpose::OriginCredentials,
        ][index];
        wire::CacheEncryptionKey::new(
            wire::CacheKeyRef {
                cache: self.harness.definition(0).id,
                id: crate::model::key_id_from_generation(
                    u64::from(epoch) + 1,
                    7 + index as u32 + u32::from(epoch) * 2,
                )
                .unwrap(),
                purpose,
            },
            state,
            zeroize::Zeroizing::new([19 + index as u8 + epoch * 2; 32]),
        )
    }

    /// Rotate the cache keys: a new active page key, keeping the page keys
    /// of the previous `keep` epochs as decrypt keys (the keyring keeps
    /// `retained_page_keys - 1`).
    fn rotate_keys(&mut self, keep: u8) {
        self.harness.key_epoch += 1;
        let epoch = self.harness.key_epoch;
        let mut keys = vec![
            self.cache_key(epoch, 0, wire::CacheKeyState::Active),
            self.cache_key(epoch, 1, wire::CacheKeyState::Active),
        ];
        for old in epoch.saturating_sub(keep)..epoch {
            keys.push(self.cache_key(old, 0, wire::CacheKeyState::Decrypt));
        }
        for node in &mut self.harness.nodes {
            let bundle = wire::KeyringBundle {
                schema_version: racer_wire::control::KEYRING_SCHEMA_VERSION,
                cluster: node.config.cluster.clone(),
                generation: wire::BundleGeneration(u64::from(epoch) + 1),
                peer_trust_roots: vec![self.harness.ca.der().to_vec()],
                cache_keys: keys.clone(),
            };
            node.workers[0].app.keys.install(bundle).unwrap();
        }
        self.harness.tick();
    }

    /// The page key of every live written page, per node.
    fn written_keys(&self) -> Vec<racer_wire::control::KeyId> {
        self.harness
            .nodes
            .iter()
            .flat_map(|n| &n.workers)
            .flat_map(|w| {
                let writer = &w.app.store.writer;
                writer
                    .written_pages()
                    .into_iter()
                    .filter_map(|(page, _)| writer.index().lookup(&page).ok().flatten())
                    .map(|indexed| indexed.key_id)
                    .collect::<Vec<_>>()
            })
            .collect()
    }

    fn read_everywhere(&mut self, object: usize, body: &[u8], etag: &str) {
        for reader in 0..self.harness.nodes.len() {
            let mut read = self.get(reader, object, body.len());
            for _ in 0..10 {
                if read.status == 200 && read.object() == body {
                    break;
                }
                self.wait(Duration::from_secs(2));
                read = self.get(reader, object, body.len());
            }
            if read.status != 200 {
                let mut diagnostics = String::new();
                for node in &self.harness.nodes {
                    for worker in &node.workers {
                        worker
                            .app
                            .telemetry
                            .failures
                            .write(&mut diagnostics)
                            .unwrap();
                    }
                }
                eprintln!("{diagnostics}");
            }
            assert_eq!(read.status, 200, "reader {reader}: {:?}", read.headers);
            assert_eq!(read.header("etag"), Some(etag), "reader {reader}");
            assert!(read.object() == body, "reader {reader}");
        }
    }
}

fn block_list_commits(sink: &MemoryUploadSink) -> usize {
    sink.calls()
        .into_iter()
        .filter(|c| *c == crate::origin::upload::UploadCall::PutBlockList)
        .count()
}

/// Losing a holder before upload restores `replicas` copies from the
/// survivors on new ranked targets; the object stays readable and uploads.
#[test]
fn a_lost_holder_is_repaired_before_upload() {
    let mut c = Cluster::new(31, 5, Some(writes(2)));
    let body = bytes(31, 330_000);
    let created = c.put(0, OBJECT, &body, "Racer-Upload: manual\r\n");
    assert_eq!(created.status, 201, "{:?}", created.headers);
    let etag = created.header("etag").unwrap().to_owned();
    let version = c.version(OBJECT, &created);
    let count = pages(body.len());
    let coordinator = c.coordinator().expect("one coordinator");
    // A holder that is not the coordinator, so only repair is exercised.
    let holders = c.holders(&version, count);
    let victim = (0..c.harness.nodes.len())
        .find(|n| *n != coordinator && holders.iter().any(|h| h.contains(n)))
        .expect("a holder other than the coordinator");
    let lost = holders.iter().filter(|h| h.contains(&victim)).count();
    let victim_id = c.harness.nodes[victim].id;
    c.crash(victim);
    c.wait(Duration::from_secs(2));
    c.wait(Duration::from_secs(2));
    let repaired = c.holders(&version, count);
    assert!(
        repaired.iter().all(|h| h.len() == 2),
        "{holders:?} -> {repaired:?}"
    );
    assert!(c.harness.nodes.iter().all(|n| n.id != victim_id));
    // The coordinator's table names exactly the live holders.
    let coordinator = c.coordinator().expect("one coordinator");
    let table = c.harness.nodes[coordinator]
        .workers
        .iter()
        .find_map(|w| {
            w.app
                .coordinator
                .writes()
                .directory()
                .with(&c.object(OBJECT), |e| {
                    e.map(|e| e.session.holders().clone())
                })
        })
        .unwrap();
    for (page, nodes) in table.0.iter().enumerate() {
        let mut named: Vec<usize> = nodes
            .iter()
            .map(|n| {
                let id = String::from_utf8(n.clone()).unwrap();
                c.harness
                    .nodes
                    .iter()
                    .position(|node| node.config.node.0 == id)
                    .expect("holder is a member")
            })
            .collect();
        named.sort();
        assert_eq!(named, repaired[page], "page {page}");
    }
    let metrics = c.harness.nodes[coordinator].workers[0]
        .app
        .telemetry
        .metrics
        .writes()
        .clone();
    let mut text = String::new();
    metrics.write(&mut text).unwrap();
    assert!(
        text.contains(&format!(
            "racer_write_repairs_total{{cache=\"{}\"}} {lost}",
            c.object(OBJECT).cache.0
        )),
        "{text}"
    );
    c.read_everywhere(OBJECT, &body, &etag);
    let persisted = c.persist(1, OBJECT);
    assert_eq!(persisted.status, 204, "{:?}", persisted.headers);
    c.wait(Duration::from_secs(1));
    let blob = c.sink.committed(&c.object(OBJECT)).expect("uploaded");
    assert!(blob.data == body);
}

/// A page whose only holder leaves cannot be repaired, but the object does
/// not fail: the holder stays in the table, reads answer retryably, and the
/// object reads and uploads again once the node returns with its disk.
#[test]
fn losing_every_holder_of_a_page_waits_for_it_to_return() {
    let mut c = Cluster::new(32, 4, Some(writes(1)));
    let body = bytes(32, 400_000);
    let created = c.put(0, OBJECT, &body, "Racer-Upload: manual\r\n");
    assert_eq!(created.status, 201);
    let etag = created.header("etag").unwrap().to_owned();
    let version = c.version(OBJECT, &created);
    let count = pages(body.len());
    let coordinator = c.coordinator().unwrap();
    let holders = c.holders(&version, count);
    let victim = (0..4)
        .find(|n| *n != coordinator && holders.iter().any(|h| h.contains(n)))
        .expect("a holder other than the coordinator");
    let id = c.harness.retire(&c.env, victim, true);
    c.harness.publish();
    c.harness.settle();
    c.wait(Duration::from_secs(2));
    c.wait(Duration::from_secs(2));
    let states: Vec<_> = c.roles().into_iter().map(|r| r.3).collect();
    assert_eq!(states, vec![racer_core::write::UploadState::Manual]);
    let head = c.head(0, OBJECT);
    assert_eq!(head.header("racer-upload-state"), Some("manual"));
    // Missing pages are unavailable: the read fails retryably, either
    // before the head or by ending the framed stream early (no end frame),
    // and never as a 404 or with wrong bytes.
    let mut short = false;
    for reader in 0..c.harness.nodes.len() {
        let read = c.get(reader, OBJECT, body.len());
        let got = read.object();
        short |= read.status == 200 && got.len() < body.len();
        assert!(
            matches!(read.status, 502 | 503) || (read.status == 200 && body.starts_with(&got)),
            "reader {reader}: {} {:?}",
            read.status,
            read.headers
        );
    }
    assert!(short, "no reader saw the missing pages");
    // The node returns with its disk: the object is whole again.
    c.harness.add(&c.env, Some(id));
    let restarted = c.harness.nodes.pop().unwrap();
    c.harness.nodes.insert(victim, restarted);
    c.configure();
    c.settings(|s| s.holder_grace = Duration::ZERO);
    c.harness.publish();
    c.harness.settle();
    c.wait(Duration::from_secs(2));
    assert_eq!(c.holders(&version, count), holders);
    c.read_everywhere(OBJECT, &body, &etag);
    let persisted = c.persist(0, OBJECT);
    assert_eq!(persisted.status, 204, "{:?}", persisted.headers);
    c.wait(Duration::from_secs(2));
    let blob = c.sink.committed(&c.object(OBJECT)).expect("uploaded");
    assert!(blob.data == body);
}

/// A holder that leaves the pool for less than the grace period is not
/// dropped from the holder table: nothing is repaired and nothing fails.
#[test]
fn a_holder_back_within_the_grace_is_kept() {
    let mut c = Cluster::new(40, 4, Some(writes(2)));
    c.settings(|s| s.holder_grace = Duration::from_secs(60));
    let body = bytes(40, 260_000);
    let created = c.put(0, OBJECT, &body, "Racer-Upload: manual\r\n");
    assert_eq!(created.status, 201);
    let etag = created.header("etag").unwrap().to_owned();
    let version = c.version(OBJECT, &created);
    let count = pages(body.len());
    let coordinator = c.coordinator().unwrap();
    let holders = c.holders(&version, count);
    let victim = (0..4)
        .find(|n| *n != coordinator && holders.iter().any(|h| h.contains(n)))
        .expect("a holder other than the coordinator");
    let id = c.harness.retire(&c.env, victim, true);
    c.harness.publish();
    c.harness.settle();
    c.wait(Duration::from_secs(2));
    c.wait(Duration::from_secs(2));
    // Still inside the grace: the table names the missing holder.
    let table = c.harness.nodes[c.coordinator().unwrap()]
        .workers
        .iter()
        .find_map(|w| {
            w.app
                .coordinator
                .writes()
                .directory()
                .with(&c.object(OBJECT), |e| {
                    e.map(|e| e.session.holders().clone())
                })
        })
        .unwrap();
    assert!(table.0.iter().all(|page| page.len() == 2), "{table:?}");
    c.harness.add(&c.env, Some(id));
    let restarted = c.harness.nodes.pop().unwrap();
    c.harness.nodes.insert(victim, restarted);
    c.configure();
    c.settings(|s| s.holder_grace = Duration::from_secs(60));
    c.harness.publish();
    c.harness.settle();
    c.wait(Duration::from_secs(2));
    assert_eq!(c.holders(&version, count), holders);
    let states: Vec<_> = c.roles().into_iter().map(|r| r.3).collect();
    assert!(
        states
            .iter()
            .all(|s| *s == racer_core::write::UploadState::Manual),
        "{states:?}"
    );
    c.read_everywhere(OBJECT, &body, &etag);
}

/// Losing the coordinator mid-upload: a manifest replica takes over with the
/// forwarded credentials and commits the block list exactly once. When the
/// old coordinator restarts with its stale journal, the roles converge to
/// one coordinator and every copy of the manifest reports the upload.
#[test]
fn a_manifest_replica_takes_over_a_lost_coordinator() {
    let mut c = Cluster::new(33, 4, Some(writes(2)));
    // The coordinator's first block-list tries fail, so it is still
    // uploading when it dies.
    for _ in 0..3 {
        c.sink.fail_next(
            crate::origin::upload::UploadCall::PutBlockList,
            Error::Unavailable,
        );
    }
    let body = bytes(33, 260_000);
    let request = {
        let mut request = format!(
            "PUT /v1/objects/{} HTTP/1.1\r\nHost: racer\r\nContent-Length: {}\r\nIf-None-Match: *\r\nRacer-Upload: auto\r\nRacer-Metadata: dst opaque metadata\r\nAuthorization: Bearer dst-fixture\r\nConnection: close\r\n\r\n",
            key(OBJECT),
            body.len()
        )
        .into_bytes();
        request.extend_from_slice(&body);
        request
    };
    let fd = c.connect(1);
    let mut raw = Raw {
        fd: Some(fd),
        request,
        sent: 0,
        cut: None,
        response: Vec::new(),
        releases: Vec::new(),
        released: 0,
        cursor: None,
    };
    let mut done = false;
    for _ in 0..MAX_TURNS {
        c.harness.tick();
        if raw.poll() {
            done = true;
            break;
        }
    }
    assert!(done);
    raw.fd.take();
    let (start, _, _) = headers(&raw.response);
    assert!(start.contains(" 201"), "{start}");
    assert!(c.sink.committed(&c.object(OBJECT)).is_none());
    let coordinator = c.coordinator().expect("one coordinator");
    let old_id = c.harness.nodes[coordinator].id;
    c.crash(coordinator);
    for _ in 0..10 {
        c.wait(Duration::from_secs(2));
        if c.sink.committed(&c.object(OBJECT)).is_some() {
            break;
        }
    }
    let blob = c
        .sink
        .committed(&c.object(OBJECT))
        .expect("takeover uploaded");
    assert!(blob.data == body);
    let successful = block_list_commits(&c.sink) - 3;
    assert_eq!(successful, 1, "{:?}", c.sink.calls());
    let survivor = c.coordinator().expect("a replica took over");
    assert_eq!(
        c.roles().iter().find(|r| r.0 == survivor).map(|r| r.3),
        Some(racer_core::write::UploadState::Committed)
    );
    // The old coordinator returns with its journal (upload pending, no
    // credentials) and its old rank.
    c.harness.add(&c.env, Some(old_id));
    c.configure();
    c.harness.publish();
    c.harness.settle();
    for _ in 0..3 {
        c.wait(Duration::from_secs(2));
    }
    c.wait(crate::write::failover::ROLE_RECHECK);
    c.wait(Duration::from_secs(2));
    let roles = c.roles();
    assert_eq!(
        roles
            .iter()
            .filter(|r| r.1 == crate::write::directory::Role::Coordinator)
            .count(),
        1,
        "{roles:?}"
    );
    assert!(
        roles
            .iter()
            .all(|r| r.3 == racer_core::write::UploadState::Committed),
        "{roles:?}"
    );
    // Still exactly one commit.
    assert_eq!(block_list_commits(&c.sink) - 3, 1, "{:?}", c.sink.calls());
}

/// Coordinator loss before upload: the replica takes over and a later
/// persist uploads once.
#[test]
fn persist_after_a_coordinator_takeover_uploads_once() {
    let mut c = Cluster::new(34, 3, Some(writes(2)));
    let body = bytes(34, 200_000);
    let created = c.put(0, OBJECT, &body, "Racer-Upload: manual\r\n");
    assert_eq!(created.status, 201);
    let etag = created.header("etag").unwrap().to_owned();
    let coordinator = c.coordinator().unwrap();
    c.crash(coordinator);
    c.wait(Duration::from_secs(2));
    c.wait(Duration::from_secs(2));
    assert!(c.coordinator().is_some(), "{:?}", c.roles());
    assert_eq!(
        c.head(0, OBJECT).header("racer-upload-state"),
        Some("manual")
    );
    c.read_everywhere(OBJECT, &body, &etag);
    let persisted = c.persist(1, OBJECT);
    assert_eq!(persisted.status, 204, "{:?}", persisted.headers);
    c.wait(Duration::from_secs(2));
    assert!(c.sink.committed(&c.object(OBJECT)).unwrap().data == body);
    assert_eq!(block_list_commits(&c.sink), 1);
}

/// Written pages are resealed under the active key after each rotation, so
/// they stay readable and uploadable after their original key is dropped.
/// A manifest replica that membership changes displaced from page zero's
/// ranked members does not hear a delete (it goes to those members). Until
/// its role check drops the copy, it must not answer fresh reads for the
/// deleted version from its write directory or a cached current pointer.
#[test]
fn a_displaced_manifest_replica_does_not_serve_a_deleted_version() {
    let mut c = Cluster::new(36, 3, Some(writes(2)));
    let body = bytes(36, 140_000);
    let created = c.put(0, OBJECT, &body, "Racer-Upload: manual\r\n");
    assert_eq!(created.status, 201);
    let object = c.object(OBJECT);
    let (replica, worker) = c
        .harness
        .nodes
        .iter()
        .enumerate()
        .find_map(|(n, node)| {
            node.workers
                .iter()
                .position(|w| {
                    w.app.coordinator.writes().directory().with(&object, |e| {
                        e.is_some_and(|e| e.role == crate::write::directory::Role::Replica)
                    })
                })
                .map(|w| (n, w))
        })
        .expect("a manifest replica");
    // Hold the replica's entry aside while members join, standing in for a
    // role check that has not run yet, so the delete cannot reach it.
    let held = c.harness.nodes[replica].workers[worker]
        .app
        .coordinator
        .writes()
        .directory()
        .remove(&object)
        .unwrap();
    let replica_id = node_id(c.harness.nodes[replica].id);
    let mut displaced = false;
    for _ in 0..32 {
        c.harness.add(&c.env, None);
        c.configure();
        c.harness.publish();
        c.harness.settle();
        c.wait(Duration::from_secs(2));
        let coordinator = &c.harness.nodes[0].workers[0].app.coordinator;
        let membership = coordinator.current_membership().unwrap();
        let ranked = coordinator
            .fill
            .dependencies
            .candidates
            .candidates(membership.clone(), &object, PageNumber(0))
            .ordered;
        if !ranked
            .iter()
            .filter(|node| membership.serves(&object.cache, node))
            .take(crate::write::RANKED_DEPTH)
            .any(|node| *node == replica_id)
        {
            displaced = true;
            break;
        }
    }
    assert!(displaced, "the replica stayed a page-zero member");
    let deleted = c.delete(0, OBJECT);
    assert_eq!(deleted.status, 204, "{:?}", deleted.headers);
    c.wait(Duration::from_secs(2));
    c.harness.nodes[replica].workers[worker]
        .app
        .coordinator
        .writes()
        .directory()
        .replace(object.clone(), held);
    assert_eq!(c.head(replica, OBJECT).status, 404);
    assert_eq!(c.get(replica, OBJECT, body.len()).status, 404);
}

#[test]
fn rekeying_keeps_written_pages_readable_past_key_retention() {
    let mut c = Cluster::new(35, 3, Some(writes(2)));
    let body = bytes(35, 270_000);
    let created = c.put(2, OBJECT, &body, "Racer-Upload: manual\r\n");
    assert_eq!(created.status, 201);
    let etag = created.header("etag").unwrap().to_owned();
    let written = c.written();
    let original = c.cache_key(0, 0, wire::CacheKeyState::Active).key.id;
    assert!(c.written_keys().iter().all(|k| *k == original));
    // Retain one decrypt key: each rotation drops the key before last.
    for _ in 0..3 {
        c.rotate_keys(1);
        let active = c
            .cache_key(c.harness.key_epoch, 0, wire::CacheKeyState::Active)
            .key
            .id;
        c.wait(Duration::from_secs(1));
        c.wait(Duration::from_secs(1));
        let keys = c.written_keys();
        assert_eq!(keys.len(), written);
        assert!(keys.iter().all(|k| *k == active), "{keys:?}");
    }
    // The original key and the next one are gone: only rekeyed pages can
    // still be read.
    assert!(
        !c.harness.nodes[0].workers[0]
            .app
            .coordinator
            .availability
            .page(&c.object(OBJECT).cache, original)
    );
    assert_eq!(c.written(), written);
    c.read_everywhere(OBJECT, &body, &etag);
    assert_eq!(c.persist(0, OBJECT).status, 204);
    c.wait(Duration::from_secs(2));
    assert!(c.sink.committed(&c.object(OBJECT)).unwrap().data == body);
}

/// A page key withdrawn before rekeying (no decrypt key retained) leaves
/// written pages hidden from the index. The rekey scan counts them in
/// `racer_write_unreadable_pages` instead of losing them silently.
#[test]
fn written_pages_under_a_withdrawn_key_are_counted_unreadable() {
    let mut c = Cluster::new(38, 3, Some(writes(2)));
    let body = bytes(38, 150_000);
    assert_eq!(
        c.put(0, OBJECT, &body, "Racer-Upload: manual\r\n").status,
        201
    );
    let written = c.written();
    assert!(written > 0);
    c.rotate_keys(0);
    c.wait(Duration::from_secs(1));
    c.wait(Duration::from_secs(1));
    let cache = c.object(OBJECT).cache.0;
    let needle = format!("racer_write_unreadable_pages{{cache=\"{cache}\"}} ");
    let mut unreadable = 0;
    for node in &c.harness.nodes {
        let mut text = String::new();
        node.workers[0]
            .app
            .telemetry
            .metrics
            .writes()
            .write(&mut text)
            .unwrap();
        unreadable += text
            .lines()
            .find_map(|l| l.strip_prefix(needle.as_str()))
            .map_or(0, |v| v.parse::<usize>().unwrap());
    }
    assert_eq!(unreadable, written);
    assert!(c.written_keys().is_empty());
}

/// Written pages whose session no member knows are dropped after
/// `orphan_after`, not before; known objects are kept.
#[test]
fn orphaned_written_pages_are_collected() {
    let mut c = Cluster::new(36, 3, Some(writes(2)));
    c.settings(|s| s.orphan_after = Duration::from_secs(60));
    let orphan = bytes(36, 150_000);
    let kept = bytes(37, 100_000);
    let created = c.put(0, OBJECT, &orphan, "");
    assert_eq!(created.status, 201);
    let version = c.version(OBJECT, &created);
    assert_eq!(c.put(1, OBJECT + 1, &kept, "").status, 201);
    let total = c.written();
    let orphaned = 2 * pages(orphan.len()) as usize;
    // Every manifest copy of the first object is lost.
    let object = c.object(OBJECT);
    for node in &c.harness.nodes {
        for worker in &node.workers {
            worker.app.coordinator.writes().directory().remove(&object);
        }
    }
    c.wait(Duration::from_secs(30));
    c.wait(Duration::from_secs(1));
    assert_eq!(c.written(), total, "collected too early");

    for _ in 0..4 {
        c.wait(Duration::from_secs(20));
    }
    assert_eq!(c.written(), total - orphaned);
    let etag = c.head(0, OBJECT + 1).header("etag").unwrap().to_owned();
    c.read_everywhere(OBJECT + 1, &kept, &etag);
    // The orphan is gone everywhere.
    assert!(
        c.holders(&version, pages(orphan.len()))
            .iter()
            .all(Vec::is_empty)
    );
}

/// Removing a cache releases its written pages and sessions.
#[test]
fn a_removed_cache_releases_its_written_pages() {
    let mut c = Cluster::new(38, 3, Some(writes(2)));
    c.settings(|s| s.orphan_after = Duration::from_secs(60));
    let body = bytes(38, 200_000);
    let manual = "Racer-Upload: manual\r\n";
    assert_eq!(c.put(0, OBJECT, &body, manual).status, 201);
    assert!(c.written() > 0);
    assert!(c.sessions() > 0);
    let env = c.env.clone();
    c.harness.cache_recreate(&env);
    c.configure();
    c.settings(|s| s.orphan_after = Duration::from_secs(60));
    c.wait(Duration::from_secs(5));
    // Still within the grace period.
    assert!(c.written() > 0);
    // Past the grace, but not uploaded and still held: kept until the
    // orphan age, so a republish that briefly omits a cache loses nothing.
    c.wait(crate::write::gc::REMOVED_CACHE_GRACE);
    c.wait(Duration::from_secs(1));
    assert!(c.written() > 0);
    assert!(c.sessions() > 0);
    c.wait(Duration::from_secs(30));
    c.wait(Duration::from_secs(1));
    assert_eq!(c.written(), 0);
    assert_eq!(c.sessions(), 0);
}

/// With no spare member, repair waits: the object stays committed and
/// readable with fewer copies, and a member that joins later receives them.
#[test]
fn repair_waits_for_a_spare_member() {
    let mut c = Cluster::new(39, 3, Some(writes(3)));
    let body = bytes(39, 200_000);
    let created = c.put(0, OBJECT, &body, "Racer-Durability: replicas=3\r\n");
    assert_eq!(created.status, 201);
    let etag = created.header("etag").unwrap().to_owned();
    let version = c.version(OBJECT, &created);
    let count = pages(body.len());
    let coordinator = c.coordinator().unwrap();
    c.crash((coordinator + 1) % 3);
    c.wait(Duration::from_secs(2));
    assert!(c.holders(&version, count).iter().all(|h| h.len() == 2));
    assert!(
        c.roles()
            .iter()
            .all(|r| r.3 == racer_core::write::UploadState::Never),
        "{:?}",
        c.roles()
    );
    c.read_everywhere(OBJECT, &body, &etag);
    let env = c.env.clone();
    c.harness.add(&env, None);
    c.configure();
    c.harness.publish();
    c.harness.settle();
    c.wait(Duration::from_secs(2));
    c.wait(Duration::from_secs(2));
    assert!(
        c.holders(&version, count).iter().all(|h| h.len() == 3),
        "{:?}",
        c.holders(&version, count)
    );
    c.read_everywhere(OBJECT, &body, &etag);
}

/// Kind e2e `survives a deleted racer pod`: write a multi-page object, then
/// restart the writer node from its disk (graceful or power loss) before any
/// other node read it, and require every node to read it back.
fn restart_writer_then_read(seed: u64, nodes: usize, crash: bool, rotations: u8) {
    let mut c = Cluster::new(seed, nodes, Some(writes(2)));
    for _ in 0..rotations {
        c.rotate_keys(1);
    }
    let body = bytes(seed, 2 * small_pages().get() as usize + 40_000);
    let created = c.put(0, OBJECT, &body, "");
    assert_eq!(created.status, 201);
    let etag = created.header("etag").unwrap().to_owned();
    let id = c.harness.retire(&c.env, 0, crash);
    c.harness.add(&c.env, Some(id));
    let restarted = c.harness.nodes.pop().unwrap();
    c.harness.nodes.insert(0, restarted);
    c.configure();
    c.harness.publish();
    c.harness.settle();
    c.read_everywhere(OBJECT, &body, &etag);
}

/// Kind e2e `TestWritesNeverUpload` once returned a lasting 503 here; every
/// combination of cluster size, restart kind, and key rotation must read.
#[test]
fn a_restarted_writer_keeps_its_object_readable_everywhere() {
    for seed in 40..44 {
        for nodes in [3, 4] {
            for crash in [false, true] {
                for rotations in [0, 2] {
                    restart_writer_then_read(seed, nodes, crash, rotations);
                }
            }
        }
    }
}

/// A manifest replica that joins displaced from page zero's ranked members
/// drops its session at the next role check, but it still holds a durable
/// copy of every page it stored: the coordinator counts it as a holder and,
/// within the holder grace, never repairs a restart. Its write journal must
/// keep those pages pinned, so a crash restart afterward restores them
/// (the `app-writes-membership-churn` failures for racer-dst seeds 100 and
/// 107 left pages with `replicas - 1` holders).
#[test]
fn a_displaced_manifest_replica_keeps_its_written_pages_across_a_restart() {
    let mut c = Cluster::new(37, 3, Some(writes(3)));
    let body = bytes(37, 140_000);
    let created = c.put(0, OBJECT, &body, "");
    assert_eq!(created.status, 201);
    let object = c.object(OBJECT);
    let version = c.version(OBJECT, &created);
    let holders = c.holders(&version, pages(body.len()));
    assert!(holders.iter().all(|h| h.len() == 3), "{holders:?}");
    let is_replica = |c: &Cluster, n: usize| {
        c.harness.nodes[n].workers.iter().any(|w| {
            w.app.coordinator.writes().directory().with(&object, |e| {
                e.is_some_and(|e| e.role == crate::write::directory::Role::Replica)
            })
        })
    };
    let replica = (0..3)
        .find(|&n| is_replica(&c, n))
        .expect("a manifest replica");
    let replica_id = node_id(c.harness.nodes[replica].id);
    let mut displaced = false;
    for _ in 0..32 {
        c.harness.add(&c.env, None);
        c.configure();
        c.harness.publish();
        c.harness.settle();
        c.wait(Duration::from_secs(2));
        let coordinator = &c.harness.nodes[0].workers[0].app.coordinator;
        let membership = coordinator.current_membership().unwrap();
        let ranked = coordinator
            .fill
            .dependencies
            .candidates
            .candidates(membership.clone(), &object, PageNumber(0))
            .ordered;
        if !ranked
            .iter()
            .filter(|node| membership.serves(&object.cache, node))
            .take(crate::write::RANKED_DEPTH)
            .any(|node| *node == replica_id)
        {
            displaced = true;
            break;
        }
    }
    assert!(displaced, "the replica stayed a page-zero member");
    // Let the role check drop the displaced replica's session.
    for _ in 0..3 {
        c.wait(Duration::from_secs(16));
    }
    assert!(!is_replica(&c, replica), "{:?}", c.roles());
    let holders = c.holders(&version, pages(body.len()));
    assert!(
        holders.iter().all(|h| h.contains(&replica)),
        "the displaced replica still holds its pages: {holders:?}"
    );
    // Crash it (power loss) and restart it from its disk.
    let id = c.harness.retire(&c.env, replica, true);
    c.harness.add(&c.env, Some(id));
    let restarted = c.harness.nodes.pop().unwrap();
    c.harness.nodes.insert(replica, restarted);
    c.configure();
    c.harness.publish();
    c.harness.settle();
    assert_eq!(c.holders(&version, pages(body.len())), holders);
    c.wait(Duration::from_secs(2));
    assert_eq!(c.holders(&version, pages(body.len())), holders);
}
