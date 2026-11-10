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
                let count = u32::from_be_bytes(
                    self.response[cursor + 17..cursor + 21].try_into().unwrap(),
                ) as usize;
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
            let count =
                u32::from_be_bytes(self.body[cursor + 17..cursor + 21].try_into().unwrap()) as usize;
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
        let sim = SimOs::new();
        let clock = racer_io::SimClock::new(seed);
        let env = racer_io::Env::sim(&sim, clock.ctx(0));
        let mut harness = Harness::new(seed, sim, clock, false);
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
    let created = c.put(0, OBJECT, &body, "Racer-Content-Type: application/x-test\r\n");
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
        assert_eq!(read.header("content-type"), Some("application/octet-stream"));
        let got = read.object();
        if got != body {
            let mut diagnostics = String::new();
            for node in &c.harness.nodes {
                for worker in &node.workers {
                    worker.app.telemetry.failures.write(&mut diagnostics).unwrap();
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
        assert_eq!(head.header("racer-content-type"), Some("application/x-test"));
        assert_eq!(head.header("racer-upload-state"), Some("never"));
    }
    let metrics = c.harness.nodes[0].workers[0].app.telemetry.metrics.writes().clone();
    assert_eq!(
        metrics.requests(&c.object(OBJECT).cache.0, crate::write::metrics::Outcome::Created),
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
        assert_eq!(created.status, 201, "replicas={replicas}: {:?}", created.headers);
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
    assert_eq!(c.head(1, OBJECT).header("racer-upload-state"), Some("manual"));
    // A never object refuses persist.
    assert_eq!(c.put(0, OBJECT + 1, &manual, "Racer-Upload: never\r\n").status, 201);
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
    assert!(c.holders(&version, pages(manual.len())).iter().all(Vec::is_empty));
    let refused = c.delete(1, OBJECT);
    assert_eq!(refused.status, 409);
    assert_eq!(refused.header("racer-write-error"), Some("uploaded"));
    // Automatic upload starts at commit.
    let auto = bytes(8, 90_000);
    let created = c.put(1, OBJECT + 2, &auto, "Racer-Upload: auto\r\n");
    assert_eq!(created.status, 201);
    c.wait(Duration::from_secs(1));
    assert!(c.sink.committed(&c.object(OBJECT + 2)).expect("auto blob").data == auto);
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
    assert!(c.sink.committed(&c.object(OBJECT + 3)).expect("origin blob").data == origin);
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
    assert_eq!(c.put(0, OBJECT, &body, "Racer-Hold-Seconds: 30\r\n").status, 201);
    assert!(c.written() > 0);
    c.wait(Duration::from_secs(31));
    c.wait(Duration::from_secs(1));
    assert_eq!(c.written(), 0);
    assert_eq!(c.sessions(), 0);
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
        assert_eq!(c.holders(&version, pages(body.len())), holders, "node {index}");
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
