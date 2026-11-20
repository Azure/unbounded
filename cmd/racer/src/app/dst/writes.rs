//! Generated write traffic (WP11): seeded PUT, read, HEAD, DELETE, persist,
//! create-once races, and concurrent readers on the simulated cluster, mixed
//! with crash restarts, partitions, client disconnects, slow disks, and
//! upload faults. Every check is a panic, which [`super::run`] reports as a
//! failed scenario.
//!
//! Crashes restart from disk and partitions heal; the holder-loss and
//! membership-churn scenarios also remove and add nodes, which exercises
//! repair, rank-0 rebalancing, and coordinator failover (WP8).

use super::*;
use crate::origin::upload::CommitConflict;
use crate::origin::upload::MemoryUploadSink;
use crate::origin::upload::UploadCall;
use crate::origin::upload::UploadSink;
use crate::security::OriginContext;
use racer_wire::control::WriteDefinition;
use racer_wire::object::write::UploadMode;
use std::collections::BTreeSet;

/// Write scenarios that need unfinished features: runnable by name, but
/// not in [`super::SCENARIOS`], so the default run and CI skip them. Empty
/// since WP8 landed.
pub const GATED: &[&str] = &[];

/// Orphan age the simulated nodes use (production default: one hour).
const ORPHAN_AFTER: Duration = Duration::from_secs(8);
/// Holder grace for random runs (`RACER_WRITE_HOLDER_GRACE_SECS`).
const HOLDER_GRACE: Duration = Duration::from_secs(3);

/// Generated actions per run when the caller passes 0 steps.
const DEFAULT_STEPS: usize = 24;
/// Written keys start here, away from the read corpus objects.
const FIRST: usize = 0x200;
/// Distinct keys the generator draws from, so keys repeat and conflict.
const KEYS: usize = 6;
/// Re-reads (with two seconds of virtual time between them) a reader gets
/// after a fault before a retryable answer counts as lost data.
const READ_RETRIES: usize = 12;

/// 64 KiB pages, so multi-page objects stay small.
fn page_size() -> racer_wire::object::PageSize {
    racer_wire::object::PageSize::MIN
}

fn page_bytes() -> usize {
    page_size().get() as usize
}

/// Objects the origin fixture reports as existing (create-once checks).
#[derive(Default)]
struct Existing(RefCell<BTreeSet<CacheKey>>);

/// The origin create-once checks ask: `404` unless the key was planted.
struct FixtureOrigin(Rc<Existing>);

impl crate::origin::Origin for FixtureOrigin {
    fn bootstrap_reserved<'a>(
        &'a self,
        _: &'a crate::read::candidates::OriginAuthority,
        _: &'a OriginContext,
        _: flow_control::Charge<AdmissionPolicy>,
        _: &'a RequestScope,
    ) -> Operation<'a, crate::origin::MetadataReply> {
        Box::pin(async { Err(Error::Unavailable) })
    }

    fn page_reserved<'a>(
        &'a self,
        _: &'a crate::read::candidates::OriginAuthority,
        _: &'a OriginContext,
        _: &'a PageId,
        _: flow_control::Charge<AdmissionPolicy>,
        _: &'a RequestScope,
    ) -> Operation<'a, crate::origin::OriginPage> {
        Box::pin(async { Err(Error::Unavailable) })
    }

    fn metadata<'a>(
        &'a self,
        _: &'a crate::read::candidates::OriginAuthority,
        context: &'a OriginContext,
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

/// A [`MemoryUploadSink`] that counts successful and refused commits per
/// object, so the run can prove every upload commits at most once.
#[derive(Default)]
struct RecordingSink {
    inner: MemoryUploadSink,
    /// A committed blob exists at the origin, so create-once sees it.
    existing: Rc<Existing>,
    commits: RefCell<BTreeMap<CacheKey, usize>>,
    conflicts: RefCell<BTreeMap<CacheKey, usize>>,
}

impl UploadSink for RecordingSink {
    fn put_block<'a>(
        &'a self,
        context: &'a OriginContext,
        block_id: &'a [u8; 64],
        plaintext: &'a [u8],
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        self.inner.put_block(context, block_id, plaintext, scope)
    }

    fn put_block_list<'a>(
        &'a self,
        context: &'a OriginContext,
        block_ids: &'a [[u8; 64]],
        content_type: Option<&'a str>,
        scope: &'a RequestScope,
    ) -> Operation<'a, std::result::Result<StrongEtag, CommitConflict>> {
        Box::pin(async move {
            let result = self
                .inner
                .put_block_list(context, block_ids, content_type, scope)
                .await;
            let key = context.object.key;
            match &result {
                Ok(Ok(_)) => {
                    *self.commits.borrow_mut().entry(key).or_default() += 1;
                    self.existing.0.borrow_mut().insert(key);
                }
                Ok(Err(CommitConflict)) => {
                    *self.conflicts.borrow_mut().entry(key).or_default() += 1;
                }
                Err(_) => {}
            }
            result
        })
    }
}

/// A raw client exchange: one request, one response, optionally cut short.
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
    done: bool,
}

impl Raw {
    fn new(fd: Descriptor, request: Vec<u8>, cut: Option<usize>) -> Self {
        Self {
            fd: Some(fd),
            request,
            sent: 0,
            cut,
            response: Vec::new(),
            releases: Vec::new(),
            released: 0,
            cursor: None,
            done: false,
        }
    }

    /// Advance; true once the response is complete or the socket closed.
    fn poll(&mut self) -> bool {
        if self.done {
            return true;
        }
        self.done = self.step();
        if self.done {
            self.fd.take();
        }
        self.done
    }

    fn step(&mut self) -> bool {
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
            return true;
        }
        let mut bytes = [0; 65536];
        match fd.recv(&mut bytes) {
            Ok(0) => return true,
            Ok(n) => self.response.extend_from_slice(&bytes[..n]),
            Err(e) if would_block(&e) => return false,
            Err(_) => return true,
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

    /// The parsed response, or `None` when the connection closed before
    /// the headers (a faulted exchange).
    fn response(&self) -> Option<Response> {
        let end = self.response.windows(4).position(|w| w == b"\r\n\r\n")?;
        let (start, headers, end) = headers(&self.response[..end + 4]);
        let status = start.split_whitespace().nth(1)?.parse().ok()?;
        Some(Response {
            status,
            headers,
            body: self.response[end..].to_vec(),
        })
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

    /// The object bytes of a framed subscription response, and whether the
    /// stream ended with its end frame.
    fn object(&self) -> (Vec<u8>, bool) {
        let mut out = Vec::new();
        let mut cursor = 0;
        while cursor + 21 <= self.body.len() {
            let kind = self.body[cursor];
            let count = u32::from_be_bytes(self.body[cursor + 17..cursor + 21].try_into().unwrap())
                as usize;
            cursor += 21;
            if kind == 2 {
                return (out, true);
            }
            if cursor + count > self.body.len() {
                break;
            }
            out.extend_from_slice(&self.body[cursor..cursor + count]);
            cursor += count;
        }
        (out, false)
    }
}

/// What the oracle knows about one acknowledged write.
#[derive(Clone)]
struct Written {
    bytes: Vec<u8>,
    etag: String,
    replicas: usize,
    mode: UploadMode,
    /// The upload was requested (`auto` at commit, or an accepted persist).
    uploading: bool,
}

/// Seeded generator knobs for one scenario.
#[derive(Clone, Copy)]
struct Profile {
    nodes: usize,
    /// Permanent holder loss (needs WP8 repair).
    holder_loss: bool,
    /// Membership churn that moves coordinators (needs WP8 failover).
    churn: bool,
}

/// The write world: the harness plus the oracle.
struct World {
    harness: Harness,
    env: racer_io::Env,
    existing: Rc<Existing>,
    sink: Rc<RecordingSink>,
    /// Acknowledged, not deleted or expired writes by key index.
    live: BTreeMap<usize, Written>,
    /// Etags of deleted or expired versions; never uploaded.
    gone: BTreeSet<String>,
    /// The current faulted read may answer 404 (see `crash_restart`).
    tolerate_404: bool,
    /// Keys whose only copies were lost with a removed node: their state
    /// is `failed` and reads may answer anything but their old bytes.
    lost: BTreeSet<usize>,
    /// Etags of the versions lost with a removed node. Their surviving
    /// pages may stay pinned (until the orphan check or their hold drops
    /// them) even after a new version of the key is written.
    /// A surviving cached copy may still serve them: by etag, the key and
    /// the acknowledged bytes.
    lost_versions: BTreeMap<String, (usize, Vec<u8>)>,
    /// Etags of every acknowledged version.
    acked: BTreeSet<String>,
    /// Upload faults injected and not yet consumed.
    upload_faults: usize,
    profile: Profile,
    actions: BTreeMap<&'static str, usize>,
}

fn body(random: &mut Random, length: usize) -> Vec<u8> {
    (0..length).map(|_| random.next() as u8).collect()
}

impl World {
    fn new(seed: u64, profile: Profile) -> Self {
        let sim = SimOs::new();
        let clock = racer_io::SimClock::new(seed);
        let env = racer_io::Env::sim(&sim, clock.ctx(0));
        let mut harness = Harness::new(seed, sim, clock, false);
        harness.writes = Some(WriteDefinition {
            replicas: 2,
            max_dirty_percent: 30,
            hold_ttl: Duration::from_secs(3600),
            upload_mode: UploadMode::Never,
            upload_max_bytes_per_second: 0,
            upload_max_concurrent_blocks: 16,
        });
        harness.page_size = page_size();
        // A fixed worker count keeps each node's storage layout, and so its
        // write journal, across restarts.
        harness.workers = Some(2);
        let existing = Rc::<Existing>::default();
        let sink = Rc::new(RecordingSink {
            existing: existing.clone(),
            ..RecordingSink::default()
        });
        let mut world = Self {
            harness,
            env,
            existing,
            sink,
            live: BTreeMap::new(),
            gone: BTreeSet::new(),
            lost: BTreeSet::new(),
            lost_versions: BTreeMap::new(),
            tolerate_404: false,
            acked: BTreeSet::new(),
            upload_faults: 0,
            profile,
            actions: BTreeMap::new(),
        };
        for _ in 0..profile.nodes {
            world.harness.add(&world.env, None);
        }
        world.configure();
        world.harness.publish();
        world.harness.settle();
        world
    }

    fn note(&mut self, action: &'static str) {
        *self.actions.entry(action).or_default() += 1;
        if std::env::var_os("RACER_DST_WRITES_TRACE").is_some() {
            eprintln!("write action {action} live={:x?}", self.live.keys());
        }
        self.harness
            .coverage
            .trace
            .record(format!("write:{action}"));
    }

    /// Install the upload sink and the origin fixture on every worker.
    fn configure(&mut self) {
        for node in &self.harness.nodes {
            for worker in &node.workers {
                let writes = worker.app.coordinator.writes();
                writes.set_sink(Some(self.sink.clone() as Rc<dyn UploadSink>));
                writes.set_test_origin(Some(Rc::new(FixtureOrigin(self.existing.clone()))));
                // Short orphan checks (every 2 s) so garbage collection and
                // missed-release recovery run inside a run.
                let mut settings = writes.settings();
                settings.orphan_after = ORPHAN_AFTER;
                // Shorter than a fault's settle wait, so restarts inside it
                // keep their holders and removals are repaired in the run.
                settings.holder_grace = HOLDER_GRACE;
                writes.set_settings(settings);
            }
        }
    }

    fn pick(&mut self, n: usize) -> usize {
        self.harness.rng.pick(n)
    }

    fn connect(&self, node: usize) -> Descriptor {
        self.harness
            .sim
            .connect(SocketAddress::Unix(
                cache(self.harness.nodes[node].id).client_socket,
            ))
            .unwrap()
    }

    /// Drive `raws` to completion together, checking invariants each turn.
    fn drive(&mut self, raws: &mut [Raw]) {
        for _ in 0..MAX_TURNS {
            self.harness.tick();
            self.check_dirty_cap();
            let start = self.pick(raws.len());
            for offset in 0..raws.len() {
                raws[(start + offset) % raws.len()].poll();
            }
            if raws.iter().all(|r| r.done) {
                self.harness.settle();
                return;
            }
        }
        panic!("seed={} write exchange stalled", self.harness.seed);
    }

    fn exchange(&mut self, node: usize, request: Vec<u8>) -> Option<Response> {
        let mut raws = [Raw::new(self.connect(node), request, None)];
        self.drive(&mut raws);
        raws[0].response()
    }

    fn put_request(object: usize, body: &[u8], fields: &str) -> Vec<u8> {
        let mut request = format!(
            "PUT /v1/objects/{} HTTP/1.1\r\nHost: racer\r\nContent-Length: {}\r\nIf-None-Match: *\r\nRacer-Metadata: dst opaque metadata\r\nAuthorization: Bearer dst-fixture\r\n{fields}Connection: close\r\n\r\n",
            key(object),
            body.len()
        )
        .into_bytes();
        request.extend_from_slice(body);
        request
    }

    fn get_request(object: usize, length: usize) -> Vec<u8> {
        format!(
            "POST /v1/objects/{} HTTP/1.1\r\nHost: racer\r\n{SUBSCRIPTION_FIELDS}Range: bytes=0-{}\r\nRacer-Metadata: dst opaque metadata\r\nAuthorization: Bearer dst-fixture\r\nConnection: close\r\n\r\n",
            key(object),
            length.max(1) - 1
        )
        .into_bytes()
    }

    fn simple(&mut self, node: usize, method: &str, path: &str) -> Option<Response> {
        let request = format!(
            "{method} /v1/objects/{path} HTTP/1.1\r\nHost: racer\r\nRacer-Metadata: dst opaque metadata\r\nAuthorization: Bearer dst-fixture\r\nConnection: close\r\n\r\n"
        );
        self.exchange(node, request.into_bytes())
    }

    fn object(&self, object: usize) -> ObjectId {
        ObjectId {
            cache: self.harness.definition(self.harness.nodes[0].id).id,
            key: CacheKey::parse_hex(key(object).as_bytes()).unwrap(),
        }
    }

    /// Let maintenance turns run for `duration` of virtual time.
    fn wait(&mut self, duration: Duration) {
        self.harness.clock.advance(duration);
        for _ in 0..400 {
            self.harness.tick();
        }
        self.harness.settle();
        self.check_dirty_cap();
    }

    /// Invariant: no worker holds more live written bytes than its cap.
    fn check_dirty_cap(&self) {
        let cache = self.harness.definition(self.harness.nodes[0].id).id;
        for node in &self.harness.nodes {
            for worker in &node.workers {
                let writer = &worker.app.store.writer;
                let (dirty, limit) = (writer.dirty_bytes(&cache), writer.dirty_limit(&cache));
                assert!(
                    dirty <= limit,
                    "seed={} node {} dirty {dirty} over cap {limit}",
                    self.harness.seed,
                    node.id
                );
            }
        }
    }

    /// Nodes holding a live written copy of each page of `version`.
    fn holders(&self, version: &ObjectVersion, pages: u64) -> Vec<usize> {
        (0..pages)
            .map(|number| {
                let page = PageId {
                    version: version.clone(),
                    number: PageNumber(number),
                };
                self.harness
                    .nodes
                    .iter()
                    .filter(|node| {
                        node.workers
                            .iter()
                            .any(|w| w.app.store.writer.written(&page).is_some())
                    })
                    .count()
            })
            .collect()
    }

    fn version(&self, object: usize, etag: &str) -> ObjectVersion {
        ObjectVersion {
            object: self.object(object),
            etag: StrongEtag::parse(etag.as_bytes()).unwrap(),
        }
    }

    // Actions.

    /// PUT a fresh object (or race one into an existing key) with random
    /// size, replicas, and upload mode; concurrent readers on other nodes
    /// see nothing or the whole object, never a prefix.
    fn put(&mut self) {
        let object = FIRST + self.pick(KEYS);
        let nodes = self.harness.nodes.len();
        let page = page_bytes();
        let length = match self.pick(5) {
            0 => 1 + self.pick(page),
            1 => page,
            2 => page + 1,
            3 => 2 * page - 1,
            _ => 1 + self.pick(3 * page),
        };
        let replicas = 1 + self.pick(nodes.min(3));
        let mode = [UploadMode::Never, UploadMode::Manual, UploadMode::Auto][self.pick(3)];
        let mode_name = match mode {
            UploadMode::Never => "never",
            UploadMode::Manual => "manual",
            UploadMode::Auto => "auto",
        };
        let fields =
            format!("Racer-Durability: replicas={replicas}\r\nRacer-Upload: {mode_name}\r\n");
        let bytes = body(&mut self.harness.rng, length);
        let writer = self.pick(nodes);
        let racing = self.pick(4) == 0;
        let mut raws = vec![Raw::new(
            self.connect(writer),
            Self::put_request(object, &bytes, &fields),
            None,
        )];
        let mut rival = None;
        if racing {
            // A create-once race: another writer, different bytes.
            let other = body(&mut self.harness.rng, length);
            let node = self.pick(nodes);
            raws.push(Raw::new(
                self.connect(node),
                Self::put_request(object, &other, &fields),
                None,
            ));
            rival = Some(other);
            self.note("put-race");
        }
        let readers = self.pick(3);
        for _ in 0..readers {
            let node = self.pick(nodes);
            raws.push(Raw::new(
                self.connect(node),
                Self::get_request(object, length),
                None,
            ));
        }
        self.note("put");
        let existed = self.live.get(&object).cloned();
        self.drive(&mut raws);
        let writers = if racing { 2 } else { 1 };
        if std::env::var_os("RACER_DST_WRITES_TRACE").is_some() {
            for raw in &raws[..writers] {
                eprintln!(
                    "put {object:x} -> {:?}",
                    raw.response().map(|r| (r.status, r.headers))
                );
            }
        }
        let mut won = Vec::new();
        for (index, raw) in raws[..writers].iter().enumerate() {
            let response = raw
                .response()
                .unwrap_or_else(|| panic!("seed={} PUT closed early", self.harness.seed));
            match response.status {
                201 => {
                    assert!(
                        existed.is_none(),
                        "seed={} create-once: key {object:x} written twice",
                        self.harness.seed
                    );
                    let etag = response.header("etag").unwrap().to_owned();
                    let state = response.header("racer-upload-state").unwrap_or("");
                    assert!(
                        matches!(mode, UploadMode::Auto) || state == mode_name,
                        "seed={} upload state {state} for {mode_name}",
                        self.harness.seed
                    );
                    let written = if index == 0 {
                        bytes.clone()
                    } else {
                        rival.clone().unwrap()
                    };
                    won.push((etag, written));
                }
                412 => {}
                // Store targets busy, dirty cap, or a conflicting session.
                409 | 503 => {}
                status => panic!(
                    "seed={} PUT unexpected {status} {:?}",
                    self.harness.seed, response.headers
                ),
            }
        }
        assert!(
            won.len() <= 1,
            "seed={} create-once: {} writers won key {object:x}",
            self.harness.seed,
            won.len()
        );
        if existed.is_none() && won.is_empty() && !racing {
            self.note("put-refused");
        }
        let acked = won.pop().map(|(etag, bytes)| {
            self.acked.insert(etag.clone());
            Written {
                bytes,
                etag,
                replicas,
                mode,
                uploading: matches!(mode, UploadMode::Auto),
            }
        });
        // Concurrent readers: 404, a retryable answer, or the whole object.
        // A key lost with its holder still names its failed version.
        let lost = self.lost.contains(&object) && acked.is_none();
        for raw in raws[writers..].iter().filter(|_| !lost) {
            let Some(response) = raw.response() else {
                continue;
            };
            match response.status {
                200 => {
                    let etag = response.header("etag").unwrap_or("").to_owned();
                    let Some(want) = acked
                        .as_ref()
                        .filter(|w| w.etag == etag)
                        .or(existed.as_ref().filter(|w| w.etag == etag))
                    else {
                        assert!(
                            existed.as_ref().is_some_and(|w| w.uploading),
                            "seed={} reader saw unacknowledged version {etag}",
                            self.harness.seed
                        );
                        continue;
                    };
                    // The range covers the new write's length; an earlier
                    // version answers its prefix of that range.
                    let expected = &want.bytes[..length.min(want.bytes.len())];
                    let (got, complete) = response.object();
                    assert!(
                        complete && got == expected,
                        "seed={} concurrent reader saw wrong or partial bytes for {object:x}: {} of {}",
                        self.harness.seed,
                        got.len(),
                        expected.len()
                    );
                    self.note("concurrent-read-hit");
                }
                404 | 409 | 412 | 502 | 503 => {}
                status => panic!("seed={} concurrent reader {status}", self.harness.seed),
            }
        }
        if let Some(written) = acked {
            self.lost.remove(&object);
            self.live.insert(object, written);
            self.check_holders(object);
        }
    }

    /// Read `object` on `node`: the oracle's bytes, or `404` when it is not
    /// live. Retryable answers are retried after virtual time passes; a
    /// live object must never read as `404`.
    fn read(&mut self, node: usize, object: usize) {
        if self.lost.contains(&object) && !self.live.contains_key(&object) {
            // Lost with its holder: any answer but the old bytes is fine,
            // and nothing here can tell them apart cheaply.
            return;
        }
        let want = self.live.get(&object).cloned();
        let length = want.as_ref().map_or(1, |w| w.bytes.len());
        for attempt in 0..READ_RETRIES {
            let response = self.exchange(node, Self::get_request(object, length));
            let Some(response) = response else {
                self.wait(Duration::from_secs(2));
                continue;
            };
            if want.as_ref().is_some_and(|w| w.uploading) && response.status != 200 {
                // Uploaded objects read from the origin, which this fixture
                // does not serve.
                self.note("read-after-upload");
                return;
            }
            match (&want, response.status) {
                (Some(want), 200) => {
                    let etag = response.header("etag").unwrap_or("");
                    if etag != want.etag && want.uploading {
                        // After the upload commits, reads go to the origin,
                        // which this fixture does not serve.
                        self.note("read-after-upload");
                        return;
                    }
                    assert_eq!(etag, want.etag, "seed={} etag", self.harness.seed);
                    let (got, complete) = response.object();
                    if !complete && want.uploading {
                        // Pages released after the upload refill from the
                        // origin, which this fixture does not serve.
                        self.note("read-after-upload");
                        return;
                    }
                    if !complete && want.bytes.starts_with(&got) {
                        // The stream failed after its head: a failed read.
                        let mut diagnostics = String::new();
                        for worker in &self.harness.nodes[node].workers {
                            worker
                                .app
                                .telemetry
                                .failures
                                .write(&mut diagnostics)
                                .unwrap();
                        }
                        if std::env::var_os("RACER_DST_WRITES_TRACE").is_some() {
                            eprintln!("incomplete read attempt {attempt}: {diagnostics}");
                        }
                        self.note("read-incomplete");
                        self.wait(Duration::from_secs(2));
                        continue;
                    }
                    assert!(
                        complete && got == want.bytes,
                        "seed={} node {node} read wrong bytes of {object:x}: {} of {} complete={complete} prefix={}",
                        self.harness.seed,
                        got.len(),
                        want.bytes.len(),
                        want.bytes.starts_with(&got)
                    );
                    self.note("read-hit");
                    return;
                }
                (Some(want), 404) => {
                    assert!(
                        want.uploading,
                        "seed={} node {node} lost acknowledged {object:x} (404)",
                        self.harness.seed
                    );
                    // Uploaded: the fixture origin has no read path.
                    return;
                }
                (None, 404) => {
                    self.note("read-miss");
                    return;
                }
                (None, 200) => {
                    let etag = response.header("etag").unwrap_or("").to_owned();
                    if std::env::var_os("RACER_DST_WRITES_TRACE").is_some() {
                        let id = self.object(object);
                        let now = self.harness.clock.ctx(0).clock.wall_now();
                        let known: Vec<_> = self
                            .harness
                            .nodes
                            .iter()
                            .map(|n| {
                                n.workers
                                    .iter()
                                    .map(|w| {
                                        let d = w.app.coordinator.writes().directory().clone();
                                        let current = d.current(&id, now).is_some();
                                        let role = d.with(&id, |e| e.map(|e| e.role));
                                        (current, role)
                                    })
                                    .collect::<Vec<_>>()
                            })
                            .collect();
                        eprintln!(
                            "reader {node} ranked {:?} directories {known:?}",
                            self.harness.ranked_nodes(&id)
                        );
                    }
                    assert!(
                        !self.gone.contains(&etag),
                        "seed={} deleted version {etag} still readable",
                        self.harness.seed
                    );
                    panic!("seed={} unknown object readable", self.harness.seed);
                }
                (_, 502 | 503) => {
                    self.note("read-retry");
                    let _ = attempt;
                    self.wait(Duration::from_secs(2));
                }
                (_, status) => panic!(
                    "seed={} node {node} read {object:x}: {status} {:?}",
                    self.harness.seed, response.headers
                ),
            }
        }
        let detail = self.live.get(&object).map(|w| {
            let version = self.version(object, &w.etag);
            let pages = w.bytes.len().div_ceil(page_bytes()) as u64;
            let membership = self.harness.nodes[0].workers[0]
                .app
                .snapshots
                .current()
                .unwrap()
                .membership
                .clone();
            (0..pages)
                .map(|number| {
                    let page = PageId {
                        version: version.clone(),
                        number: PageNumber(number),
                    };
                    let holders: Vec<_> = self
                        .harness
                        .nodes
                        .iter()
                        .enumerate()
                        .filter(|(_, n)| {
                            n.workers
                                .iter()
                                .any(|w| w.app.store.writer.written(&page).is_some())
                        })
                        .map(|(i, _)| i)
                        .collect();
                    let owners: Vec<_> = Placement::new(1)
                        .rank(membership.clone(), &version.object, PageNumber(number))
                        .ordered
                        .iter()
                        .map(|id| self.harness.nodes.iter().position(|n| &n.config.node == id))
                        .collect();
                    format!("page {number}: holders {holders:?} owners {owners:?}")
                })
                .collect::<Vec<_>>()
        });
        if std::env::var_os("RACER_DST_WRITES_TRACE").is_some() {
            let mut diagnostics = String::new();
            for worker in &self.harness.nodes[node].workers {
                worker
                    .app
                    .telemetry
                    .failures
                    .write(&mut diagnostics)
                    .unwrap();
            }
            eprintln!("{diagnostics}");
        }
        panic!(
            "seed={} node {node} read of {object:x} never recovered: {detail:?}",
            self.harness.seed
        );
    }

    fn read_some(&mut self) {
        let object = FIRST + self.pick(KEYS);
        let node = self.pick(self.harness.nodes.len());
        self.note("read");
        self.read(node, object);
    }

    fn delete(&mut self) {
        let object = FIRST + self.pick(KEYS);
        if self.lost.contains(&object) && !self.live.contains_key(&object) {
            // Lost with its holder: any answer but the old bytes is fine,
            // and nothing here can tell them apart cheaply.
            return;
        }
        let node = self.pick(self.harness.nodes.len());
        self.note("delete");
        let mut response = self
            .simple(node, "DELETE", &key(object))
            .expect("DELETE answered");
        // A `503` delete of a live object may have taken effect without
        // reaching its tombstone quorum: retry it like a client would. A
        // retry answers `404` once the earlier attempt's delete stands.
        let mut retried = false;
        if response.status == 503 && self.live.contains_key(&object) {
            for _ in 0..3 {
                self.wait(Duration::from_secs(2));
                let node = self.pick(self.harness.nodes.len());
                self.note("delete-retry");
                response = self
                    .simple(node, "DELETE", &key(object))
                    .expect("DELETE answered");
                retried = true;
                if response.status != 503 {
                    break;
                }
            }
        }
        match (self.live.get(&object).cloned(), response.status) {
            (Some(written), 404) if retried => {
                self.note("delete-retry-404");
                self.live.remove(&object);
                self.gone.insert(written.etag);
                self.wait(Duration::from_secs(2));
                for reader in 0..self.harness.nodes.len() {
                    self.read(reader, object);
                }
            }
            (Some(written), 503) if retried => {
                // Still unacknowledged: deleted or not, reads may answer
                // the acknowledged bytes or `404`, never anything else.
                self.note("delete-unacknowledged");
                self.live.remove(&object);
                self.lost_versions
                    .insert(written.etag.clone(), (object, written.bytes.clone()));
                if !written.uploading {
                    self.gone.insert(written.etag);
                }
                self.lost.insert(object);
            }
            (Some(written), 204) => {
                self.live.remove(&object);
                self.gone.insert(written.etag);
                // Readers revalidate within METADATA_TTL.
                self.wait(Duration::from_secs(2));
                for reader in 0..self.harness.nodes.len() {
                    self.read(reader, object);
                }
            }
            (Some(written), 409) => {
                assert!(
                    written.uploading,
                    "seed={} DELETE 409 of a non-uploading object",
                    self.harness.seed
                );
            }
            // Uploaded, and the member asked no longer tracks it (a node
            // joined ahead of the coordinator, which does not hand over
            // uploaded objects): still not deleted.
            (Some(written), 404)
                if written.uploading
                    && self.sink.inner.committed(&self.object(object)).is_some() => {}
            (None, 404) => {}
            (_, 503) => {}
            (state, status) => panic!(
                "seed={} DELETE {object:x} live={} -> {status} {:?}",
                self.harness.seed,
                state.is_some(),
                response.headers
            ),
        }
    }

    fn persist(&mut self) {
        let object = FIRST + self.pick(KEYS);
        if self.lost.contains(&object) && !self.live.contains_key(&object) {
            // Lost with its holder: any answer but the old bytes is fine,
            // and nothing here can tell them apart cheaply.
            return;
        }
        let node = self.pick(self.harness.nodes.len());
        self.note("persist");
        let request = format!(
            "POST /v1/objects/{}/persist HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\nRacer-Metadata: dst opaque metadata\r\nAuthorization: Bearer dst-fixture\r\nConnection: close\r\n\r\n",
            key(object)
        );
        let response = self
            .exchange(node, request.into_bytes())
            .expect("persist answered");
        let uploaded = self.sink.inner.committed(&self.object(object)).is_some();
        let seed = self.harness.seed;
        match (self.live.get_mut(&object), response.status) {
            (Some(written), 204 | 202 | 200) => {
                assert_eq!(written.mode, UploadMode::Manual);
                written.uploading = true;
            }
            (Some(written), 409) => {
                assert!(written.mode != UploadMode::Manual || written.uploading);
            }
            (Some(written), 404) if written.uploading && uploaded => {}
            (None, 404) | (_, 503) => {}
            (state, status) => panic!(
                "seed={} persist {object:x} live={} -> {status}",
                seed,
                state.is_some()
            ),
        }
        self.wait(Duration::from_secs(1));
    }

    /// A writer that dies mid-body: the write aborts, nothing is visible,
    /// and its staged pages are released.
    fn disconnect(&mut self) {
        let object = FIRST + KEYS + self.pick(KEYS);
        let length = 3 * page_bytes();
        let bytes = body(&mut self.harness.rng, length);
        let request = Self::put_request(object, &bytes, "");
        let cut = request.len() - length + self.pick(length);
        let node = self.pick(self.harness.nodes.len());
        self.note("put-disconnect");
        let mut raws = [Raw::new(self.connect(node), request, Some(cut))];
        self.drive(&mut raws);
        self.wait(Duration::from_secs(1));
        let reader = self.pick(self.harness.nodes.len());
        self.read(reader, object);
        self.check_pages(false, "aborted write left pages");
    }

    /// A short hold expires: the object becomes unreadable and is never
    /// uploaded, even if persist is asked afterwards.
    fn expire(&mut self) {
        let object = FIRST + KEYS * 2 + self.pick(KEYS);
        if self.live.contains_key(&object) {
            return;
        }
        let length = 1 + self.pick(2 * page_bytes());
        let bytes = body(&mut self.harness.rng, length);
        let node = self.pick(self.harness.nodes.len());
        self.note("hold-expiry");
        let response = self
            .exchange(
                node,
                Self::put_request(
                    object,
                    &bytes,
                    "Racer-Upload: manual\r\nRacer-Hold-Seconds: 30\r\n",
                ),
            )
            .expect("PUT answered");
        if response.status != 201 {
            return;
        }
        let etag = response.header("etag").unwrap().to_owned();
        self.acked.insert(etag.clone());
        self.wait(Duration::from_secs(31));
        self.wait(Duration::from_secs(2));
        self.gone.insert(etag);
        self.read(node, object);
        let request = format!(
            "POST /v1/objects/{}/persist HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\nRacer-Metadata: dst opaque metadata\r\nAuthorization: Bearer dst-fixture\r\nConnection: close\r\n\r\n",
            key(object)
        );
        let persisted = self.exchange(node, request.into_bytes()).map(|r| r.status);
        assert!(
            persisted.is_none_or(|status| status == 404 || status == 503),
            "seed={} persist of an expired object: {persisted:?}",
            self.harness.seed
        );
    }

    /// Crash a node (power loss) and restart it from its disk. While it is
    /// down, reads of objects never answer a wrong version or `404`; after
    /// restart the journal restores every holder and reads recover.
    fn crash_restart(&mut self) {
        let nodes = self.harness.nodes.len();
        let index = self.pick(nodes);
        self.note("crash-restart");
        let object = FIRST + self.pick(KEYS);
        // With `replicas=1` the coordinator keeps the only manifest: while
        // it is down, no member knows the object, and 404 is the design's
        // answer (`replicate_manifest` sends `replicas - 1` copies).
        let sole_manifest = self.live.get(&object).is_some_and(|w| w.replicas == 1)
            && self.harness.ranked_nodes(&self.object(object)).first() == Some(&index);
        let id = self.harness.retire(&self.env, index, true);
        if self.pick(2) == 0 {
            // Readers during the outage: correct bytes or retryable.
            let reader = self.pick(nodes - 1);
            self.tolerate_404 = sole_manifest;
            self.read_during_fault(reader, object);
            self.tolerate_404 = false;
        }
        self.harness.add(&self.env, Some(id));
        let restarted = self.harness.nodes.pop().unwrap();
        self.harness.nodes.insert(index, restarted);
        self.configure();
        self.harness.publish();
        self.harness.settle();
        self.wait(Duration::from_secs(2));
        for object in self.live.keys().copied().collect::<Vec<_>>() {
            self.check_holders(object);
            let reader = self.pick(nodes);
            self.read(reader, object);
        }
    }

    /// One read under an active fault: the oracle's bytes, or any non-`200`
    /// failure. Never a wrong or partial object.
    fn read_during_fault(&mut self, node: usize, object: usize) {
        let want = self.live.get(&object).cloned();
        let length = want.as_ref().map_or(1, |w| w.bytes.len());
        let mut raws = [Raw::new(
            self.connect(node),
            Self::get_request(object, length),
            None,
        )];
        for _ in 0..MAX_TURNS {
            self.harness.tick();
            raws[0].poll();
            if raws[0].done {
                break;
            }
        }
        let Some(response) = raws[0].response() else {
            return;
        };
        if response.status == 200 && want.is_none() && self.lost.contains(&object) {
            // Lost with its holder: the design keeps such a version (its
            // page waits for a holder), and a surviving copy may serve it.
            // It must be that version's acknowledged bytes.
            let etag = response.header("etag").unwrap_or("");
            let lost = self
                .lost_versions
                .get(etag)
                .filter(|(key, _)| *key == object);
            let Some((_, bytes)) = lost else {
                panic!(
                    "seed={} faulted read of lost {object:x} served unknown etag {etag:?}",
                    self.harness.seed
                );
            };
            let (got, _) = response.object();
            assert!(
                got.len() <= bytes.len() && got == bytes[..got.len()],
                "seed={} faulted read of lost {object:x} served wrong bytes",
                self.harness.seed
            );
            self.note("faulted-read-lost-hit");
        } else if response.status == 200 {
            let want = want.unwrap_or_else(|| {
                panic!(
                    "seed={} faulted read of missing object {object:x} (etag {:?}, lost {})",
                    self.harness.seed,
                    response.header("etag"),
                    self.lost.contains(&object)
                )
            });
            if response.header("etag") == Some(want.etag.as_str()) {
                let (got, complete) = response.object();
                assert!(
                    got == want.bytes[..got.len().min(want.bytes.len())],
                    "seed={} faulted read wrong bytes",
                    self.harness.seed
                );
                if complete {
                    assert_eq!(got.len(), want.bytes.len());
                }
            } else {
                assert!(
                    want.uploading,
                    "seed={} faulted read etag",
                    self.harness.seed
                );
            }
            self.note("faulted-read-hit");
        } else if response.status == 404
            && want.as_ref().is_some_and(|w| !w.uploading)
            && !self.tolerate_404
        {
            // The read-path gap WP11 fixed: a reader that skips a silent
            // coordinator must not answer an authoritative 404.
            let ranked = self.harness.ranked_nodes(&self.object(object));
            panic!(
                "seed={} live object {object:x} read 404 under a fault (reader {node}, page-zero ranks {ranked:?}, nodes {})",
                self.harness.seed,
                self.harness.nodes.len()
            );
        } else if response.status == 404 && self.tolerate_404 {
            self.note("faulted-read-404-sole-manifest");
        }
    }

    /// Partition one node from the others, read through the cut, then heal.
    fn partition(&mut self) {
        let nodes = self.harness.nodes.len();
        let isolated = self.pick(nodes);
        self.note("partition");
        let mut links = Vec::new();
        for (index, node) in self.harness.nodes.iter().enumerate() {
            if index == isolated {
                continue;
            }
            let pair = (
                SocketAddress::Inet(self.harness.nodes[isolated].config.peer_listen),
                SocketAddress::Inet(node.config.peer_listen),
            );
            self.harness.sim.partition(pair.0.clone(), pair.1.clone());
            links.push(pair);
        }
        for _ in 0..2 {
            let object = FIRST + self.pick(KEYS);
            let reader = self.pick(nodes);
            self.read_during_fault(reader, object);
        }
        for (a, b) in links {
            self.harness.sim.heal(a, b);
        }
        self.harness.settle();
        self.wait(Duration::from_secs(2));
        let object = FIRST + self.pick(KEYS);
        let reader = self.pick(nodes);
        self.read(reader, object);
    }

    /// Slow the next disk writes, then write through them.
    fn slow_disk(&mut self) {
        self.note("slow-disk");
        for _ in 0..1 + self.pick(4) {
            let turns = 16 + self.pick(64);
            self.harness
                .sim
                .inject("writev", racer_io::sim::Fault::Delay(turns))
                .unwrap();
        }
        self.put();
    }

    /// Fail the next upload calls with a retryable error; the upload still
    /// commits exactly once.
    fn upload_fault(&mut self) {
        self.note("upload-fault");
        let call = if self.pick(2) == 0 {
            UploadCall::PutBlock
        } else {
            UploadCall::PutBlockList
        };
        self.sink.inner.fail_next(call, Error::Unavailable);
        self.upload_faults += 1;
    }

    /// Permanently remove a holder (gated: needs WP8 repair).
    fn holder_loss(&mut self) {
        // Keep three nodes, so `replicas=3` objects can be repaired.
        if self.harness.nodes.len() <= 3 {
            return;
        }
        self.note("holder-loss");
        let index = self.pick(self.harness.nodes.len());
        // A write whose every copy of some page was on the lost node is
        // gone (its upload state becomes `failed`); the oracle forgets it.
        // Every other acknowledged write must stay readable.
        let lost: Vec<usize> = self
            .live
            .iter()
            .filter(|(_, w)| !w.uploading)
            .filter(|&(&object, w)| {
                let version = self.version(object, &w.etag);
                (0..w.bytes.len().div_ceil(page_bytes()) as u64).any(|number| {
                    let page = PageId {
                        version: version.clone(),
                        number: PageNumber(number),
                    };
                    self.harness.nodes.iter().enumerate().all(|(i, n)| {
                        (i == index)
                            || n.workers
                                .iter()
                                .all(|w| w.app.store.writer.written(&page).is_none())
                    })
                })
            })
            .map(|(&object, _)| object)
            .collect();
        for object in lost {
            let written = self.live.remove(&object).unwrap();
            self.lost_versions
                .insert(written.etag.clone(), (object, written.bytes.clone()));
            self.gone.insert(written.etag);
            self.lost.insert(object);
            self.note("write-lost-with-holder");
        }
        self.harness.retire(&self.env, index, true);
        self.harness.publish();
        self.harness.settle();
        // The holder grace runs before repair starts.
        self.wait(HOLDER_GRACE);
        self.wait(Duration::from_secs(5));
        for object in self.live.keys().copied().collect::<Vec<_>>() {
            let reader = self.pick(self.harness.nodes.len());
            self.read(reader, object);
        }
    }

    /// Add a node, moving some coordinators (gated: needs WP8 failover).
    fn churn(&mut self) {
        if self.harness.nodes.len() >= 5 {
            return;
        }
        self.note("membership-add");
        self.harness.add(&self.env, None);
        self.configure();
        self.harness.publish();
        self.harness.settle();
        self.wait(Duration::from_secs(5));
        for object in self.live.keys().copied().collect::<Vec<_>>() {
            let reader = self.pick(self.harness.nodes.len());
            self.read(reader, object);
        }
    }

    // Invariants at quiescence.

    /// Live written pages the oracle expects: every page of every live,
    /// not-uploading object, once per acknowledged replica.
    fn expected_pages(&self) -> usize {
        self.live
            .values()
            .filter(|w| !w.uploading)
            .map(|w| w.bytes.len().div_ceil(page_bytes()) * w.replicas)
            .sum()
    }

    /// Leak and loss check over every written page: each belongs to a live
    /// version the oracle expects pinned (uploading ones too, unless
    /// `settled`). Without membership changes the count is exact; with them,
    /// rank-0 rebalancing may add copies beyond `replicas`.
    fn check_pages(&self, settled: bool, what: &str) {
        let mut stray = Vec::new();
        for (index, node) in self.harness.nodes.iter().enumerate() {
            for worker in &node.workers {
                for (page, _) in worker.app.store.writer.written_pages() {
                    let expected = self.live.iter().any(|(&object, w)| {
                        self.object(object) == page.version.object
                            && page.version.etag.as_bytes() == w.etag.as_bytes()
                            && (!w.uploading || !settled)
                    });
                    // A version lost with its holder keeps its surviving
                    // pages until DELETE, the orphan check, or its hold
                    // ends, also after a new version of the key is
                    // written (racer-dst `app-writes-holder-loss` seeds 152
                    // and 159).
                    let failed = self
                        .lost_versions
                        .keys()
                        .any(|etag| page.version.etag.as_bytes() == etag.as_bytes());
                    if !expected && !failed {
                        stray.push((index, page.version.object.key, page.number.0));
                    }
                }
            }
        }
        assert!(
            stray.is_empty(),
            "seed={} {what}: stray written pages {stray:?}",
            self.harness.seed
        );
        if settled {
            assert!(
                self.written_pages() >= self.expected_pages(),
                "seed={} {what}",
                self.harness.seed
            );
        }
    }

    fn written_pages(&self) -> usize {
        self.harness
            .nodes
            .iter()
            .flat_map(|n| &n.workers)
            .map(|w| w.app.store.writer.written_pages().len())
            .sum()
    }

    /// Every page of a live, not-uploading object has exactly its acked
    /// replica count of pinned holders: none was evicted, and none lost
    /// across restarts.
    fn check_holders(&self, object: usize) {
        let Some(written) = self.live.get(&object) else {
            return;
        };
        if written.uploading {
            return;
        }
        let version = self.version(object, &written.etag);
        let pages = written.bytes.len().div_ceil(page_bytes()) as u64;
        let holders = self.holders(&version, pages);
        assert!(
            // Crashes and joins change the membership, and rank-0
            // rebalancing may add copies beyond `replicas`.
            holders
                .iter()
                .all(|&h| h >= written.replicas.min(self.harness.nodes.len())),
            "seed={} {object:x} replicas={} holders per page {holders:?}",
            self.harness.seed,
            written.replicas
        );
    }

    /// End of run: let uploads finish, then check upload and holder
    /// invariants across the whole oracle.
    fn finish(&mut self) {
        // Long enough for uploads, one orphan age, and missed-release checks.
        for _ in 0..6 {
            self.wait(Duration::from_secs(3));
        }
        let commits = self.sink.commits.borrow().clone();
        for (key, count) in &commits {
            assert_eq!(
                *count, 1,
                "seed={} {key:?} committed {count} times",
                self.harness.seed
            );
        }
        assert!(
            self.sink.conflicts.borrow().is_empty(),
            "seed={} upload commit conflicts {:?}",
            self.harness.seed,
            self.sink.conflicts.borrow()
        );
        let objects: Vec<_> = self.live.keys().copied().collect();
        for object in objects {
            let written = self.live[&object].clone();
            let id = self.object(object);
            let blob = self.sink.inner.committed(&id);
            if written.uploading {
                let blob = blob.unwrap_or_else(|| {
                    panic!(
                        "seed={} upload of {object:x} never committed",
                        self.harness.seed
                    )
                });
                assert!(
                    blob.data == written.bytes,
                    "seed={} committed bytes of {object:x} differ",
                    self.harness.seed
                );
            } else {
                assert!(
                    blob.is_none(),
                    "seed={} {object:x} ({:?}) uploaded without a request",
                    self.harness.seed,
                    written.mode
                );
            }
        }
        // Deleted or expired versions never reached the sink: every commit
        // belongs to a live, uploading object.
        for key in commits.keys() {
            let live = self
                .live
                .iter()
                .any(|(&object, w)| w.uploading && self.object(object).key == *key);
            assert!(
                live,
                "seed={} a deleted or expired object was uploaded",
                self.harness.seed
            );
        }
        {
            self.check_pages(true, "written pages leak or loss");
            for object in self.live.keys().copied().collect::<Vec<_>>() {
                self.check_holders(object);
            }
        }
        for object in self.live.keys().copied().collect::<Vec<_>>() {
            for reader in 0..self.harness.nodes.len() {
                self.read(reader, object);
            }
        }
        self.check_dirty_cap();
    }

    fn step(&mut self) {
        let choice = self.pick(20);
        match choice {
            0..=5 => self.put(),
            6..=8 => self.read_some(),
            9 => self.delete(),
            10 | 11 => self.persist(),
            12 => self.disconnect(),
            13 => self.crash_restart(),
            14 => self.partition(),
            15 => self.slow_disk(),
            16 => self.upload_fault(),
            17 => self.expire(),
            _ if self.profile.holder_loss && choice == 18 => self.holder_loss(),
            _ if self.profile.churn && choice == 19 => self.churn(),
            _ => self.put(),
        }
        self.check_dirty_cap();
    }
}

/// Run one generated write scenario.
pub(super) fn run(name: &str, seed: u64, steps: usize) {
    let profile = match name {
        "writes" => Profile {
            nodes: 3,
            holder_loss: false,
            churn: false,
        },
        "writes-four-nodes" => Profile {
            nodes: 4,
            holder_loss: false,
            churn: false,
        },
        "writes-holder-loss" => Profile {
            nodes: 4,
            holder_loss: true,
            churn: false,
        },
        "writes-membership-churn" => Profile {
            nodes: 3,
            holder_loss: false,
            churn: true,
        },
        other => panic!("unknown write scenario {other}"),
    };
    let steps = if steps == 0 { DEFAULT_STEPS } else { steps };
    let mut world = World::new(seed, profile);
    for step in 0..steps {
        // Every holder-loss or churn run exercises its fault at least once.
        if step == steps / 2 {
            if profile.holder_loss {
                world.holder_loss();
            }
            if profile.churn {
                world.churn();
            }
        }
        world.step();
    }
    world.finish();
    let actions = std::mem::take(&mut world.actions);
    world
        .harness
        .coverage
        .trace
        .record(format!("write-actions:{actions:?}"));
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn generated_writes_hold_their_invariants() {
        for seed in 1..=3 {
            run("writes", seed, 0);
        }
    }

    /// Replay one generated run: `RACER_DST_WRITES_REPLAY=<scenario>:<seed>`
    /// (the app seed a racer-dst failure prints).
    #[test]
    #[ignore = "replays a racer-dst failure on demand"]
    fn replay() {
        let spec = std::env::var("RACER_DST_WRITES_REPLAY").expect("RACER_DST_WRITES_REPLAY");
        let (name, seed) = spec.split_once(':').expect("<scenario>:<seed>");
        run(name, seed.parse().unwrap(), 0);
    }

    #[test]
    fn generated_writes_on_four_nodes() {
        run("writes-four-nodes", 7, 0);
    }

    #[test]
    fn generated_writes_with_holder_loss() {
        for seed in 1..=2 {
            run("writes-holder-loss", seed, 0);
        }
    }

    #[test]
    fn generated_writes_with_membership_churn() {
        for seed in 1..=2 {
            run("writes-membership-churn", seed, 0);
        }
    }

    /// A reader that is neither the coordinator nor a manifest replica asks
    /// the coordinator with a copy-only metadata probe. After the
    /// coordinator's published catalog pointer lapses (METADATA_TTL), the
    /// probe must still find the written object in its write directory;
    /// a miss would send the reader to the origin for an authoritative 404.
    #[test]
    fn a_coordinator_answers_metadata_probes_after_its_pointer_lapses() {
        for seed in 1..=4 {
            let mut world = World::new(
                60 + seed,
                Profile {
                    nodes: 3,
                    holder_loss: false,
                    churn: false,
                },
            );
            let object = FIRST;
            let bytes = body(&mut world.harness.rng, page_bytes() + 7);
            let created = world
                .exchange(
                    0,
                    World::put_request(object, &bytes, "Racer-Durability: replicas=1\r\n"),
                )
                .unwrap();
            assert_eq!(created.status, 201);
            world.live.insert(
                object,
                Written {
                    bytes,
                    etag: created.header("etag").unwrap().to_owned(),
                    replicas: 1,
                    mode: UploadMode::Never,
                    uploading: false,
                },
            );
            for round in 0..3 {
                world.wait(Duration::from_secs(3 + round));
                for reader in 0..3 {
                    world.read(reader, object);
                }
            }
        }
    }

    /// Nodes that join become rank-0 owners of some written pages. A rank-0
    /// owner is its own origin authority for a page, and the origin does
    /// not know a written object before it uploads, so the coordinator
    /// copies each page to its new rank-0 owner and hands page zero's new
    /// rank 0 the manifest. Every node, old and new, reads every object.
    #[test]
    fn joined_nodes_read_written_objects() {
        for seed in 1..=3 {
            let mut world = World::new(
                90 + seed,
                Profile {
                    nodes: 3,
                    holder_loss: false,
                    churn: true,
                },
            );
            for index in 0..4 {
                let object = FIRST + index;
                let bytes = body(&mut world.harness.rng, 3 * page_bytes() - 5);
                let created = world
                    .exchange(
                        index % 3,
                        World::put_request(object, &bytes, "Racer-Durability: replicas=1\r\n"),
                    )
                    .unwrap();
                assert_eq!(created.status, 201);
                world.live.insert(
                    object,
                    Written {
                        bytes,
                        etag: created.header("etag").unwrap().to_owned(),
                        replicas: 1,
                        mode: UploadMode::Never,
                        uploading: false,
                    },
                );
            }
            world.churn();
            world.churn();
            for object in world.live.keys().copied().collect::<Vec<_>>() {
                for reader in 0..world.harness.nodes.len() {
                    world.read(reader, object);
                }
                world.check_holders(object);
            }
            // A delete reaches every member that may coordinate or serve
            // the manifest after the joins; none keeps serving the version.
            for object in world.live.keys().copied().collect::<Vec<_>>() {
                let node = object % world.harness.nodes.len();
                let deleted = world.simple(node, "DELETE", &key(object)).unwrap();
                assert_eq!(deleted.status, 204, "seed {seed}");
                let written = world.live.remove(&object).unwrap();
                world.gone.insert(written.etag);
            }
            world.wait(Duration::from_secs(2));
            for object in FIRST..FIRST + 4 {
                for reader in 0..world.harness.nodes.len() {
                    world.read(reader, object);
                }
            }
        }
    }

    /// A page's rank 0 that lost its written copy misses at the origin and
    /// falls back to a copy-only probe of the other owners. Peer reception
    /// reserves a page from the same Ciphertext budget as origin encryption,
    /// so the unused encryption reservation must be released first: with
    /// room for one page, the read still succeeds from the surviving replica,
    /// and a never-written key still answers `404`.
    #[test]
    fn copy_only_fallback_fits_a_one_page_ciphertext_budget() {
        let mut fallbacks = 0;
        for seed in 1..=4 {
            let mut world = World::new(
                120 + seed,
                Profile {
                    nodes: 3,
                    holder_loss: false,
                    churn: false,
                },
            );
            for index in 0..4 {
                let object = FIRST + index;
                let bytes = body(&mut world.harness.rng, 3 * page_bytes() - 5);
                let created = world
                    .exchange(
                        index % 3,
                        World::put_request(object, &bytes, "Racer-Durability: replicas=2\r\n"),
                    )
                    .unwrap();
                assert_eq!(created.status, 201);
                world.live.insert(
                    object,
                    Written {
                        bytes,
                        etag: created.header("etag").unwrap().to_owned(),
                        replicas: 2,
                        mode: UploadMode::Never,
                        uploading: false,
                    },
                );
            }
            world.harness.settle();
            let membership = world.harness.nodes[0].workers[0]
                .app
                .snapshots
                .current()
                .unwrap()
                .membership
                .clone();
            // Drop each page's copy on its rank 0 when another node keeps one.
            let mut readers = Vec::new();
            for (&object, written) in &world.live {
                let version = world.version(object, &written.etag);
                for number in 0..written.bytes.len().div_ceil(page_bytes()) as u64 {
                    let page = PageId {
                        version: version.clone(),
                        number: PageNumber(number),
                    };
                    let holds = |node: &Node| {
                        node.workers
                            .iter()
                            .any(|w| w.app.store.writer.written(&page).is_some())
                    };
                    let rank0 = Placement::new(1)
                        .rank(membership.clone(), &version.object, page.number)
                        .ordered[0]
                        .clone();
                    let rank0 = world
                        .harness
                        .nodes
                        .iter()
                        .position(|n| n.config.node == rank0)
                        .unwrap();
                    let elsewhere = world
                        .harness
                        .nodes
                        .iter()
                        .enumerate()
                        .any(|(index, node)| index != rank0 && holds(node));
                    if !elsewhere || !holds(&world.harness.nodes[rank0]) {
                        continue;
                    }
                    for worker in &mut world.harness.nodes[rank0].workers {
                        if worker.app.store.writer.written(&page).is_some() {
                            drive_local(
                                &worker.runtime,
                                worker.crypto.as_mut(),
                                worker.app.store.writer.drop_written(&page),
                            )
                            .unwrap();
                        }
                    }
                    readers.push((rank0, object));
                }
            }
            let mut charges = Vec::new();
            for node in &world.harness.nodes {
                for worker in &node.workers {
                    worker.app.memory.evict_idle(usize::MAX).unwrap();
                    let admission = &worker.runtime.admission;
                    let free = admission.limit(ResourceClass::Ciphertext)
                        - admission.used(ResourceClass::Ciphertext);
                    let pinned = free.saturating_sub(page_bytes() + 16);
                    if pinned != 0 {
                        charges.push(
                            admission
                                .reserve(None, ResourceClass::Ciphertext, pinned)
                                .unwrap(),
                        );
                    }
                }
            }
            fallbacks += readers.len();
            for (reader, object) in readers {
                let retries = world.actions.get("read-retry").copied().unwrap_or(0);
                world.read(reader, object);
                // The first attempt succeeds: a retry would let a later
                // rebalance hide a fallback that could not reserve its page.
                assert_eq!(
                    world.actions.get("read-retry").copied().unwrap_or(0),
                    retries,
                    "seed={seed} node {reader} retried {object:x}"
                );
            }
            // Never written: the fallback misses and the origin's answer stands.
            world.read(0, FIRST + 100);
            drop(charges);
        }
        assert!(fallbacks > 0, "no rank 0 lost a copy another owner keeps");
    }

    /// A reader that cannot reach page zero's rank 0 (the coordinator)
    /// answers a retryable error for a written object, never `404`.
    #[test]
    fn a_reader_cut_off_from_the_coordinator_is_not_its_own_origin() {
        for seed in 1..=4 {
            let mut world = World::new(
                40 + seed,
                Profile {
                    nodes: 3,
                    holder_loss: false,
                    churn: false,
                },
            );
            let object = FIRST;
            let bytes = body(&mut world.harness.rng, 2 * page_bytes());
            let writer = 0;
            let created = world
                .exchange(
                    writer,
                    World::put_request(object, &bytes, "Racer-Durability: replicas=1\r\n"),
                )
                .unwrap();
            assert_eq!(created.status, 201);
            let id = world.object(object);
            let ranked = world.harness.ranked_nodes(&id);
            let coordinator = ranked[0];
            // A reader that coordinates nothing and keeps no manifest copy.
            let now = world.harness.clock.ctx(0).clock.wall_now();
            let Some(reader) = (0..3).find(|&n| {
                n != coordinator
                    && world.harness.nodes[n].workers.iter().all(|w| {
                        w.app
                            .coordinator
                            .writes()
                            .directory()
                            .current(&id, now)
                            .is_none()
                    })
            }) else {
                continue;
            };
            // Cut the reader off from every other node: every page-zero
            // owner is silent to it.
            let pairs: Vec<_> = (0..3)
                .filter(|&n| n != reader)
                .map(|n| {
                    (
                        SocketAddress::Inet(world.harness.nodes[reader].config.peer_listen),
                        SocketAddress::Inet(world.harness.nodes[n].config.peer_listen),
                    )
                })
                .collect();
            for (a, b) in &pairs {
                world.harness.sim.partition(a.clone(), b.clone());
            }
            let mut seen = BTreeSet::new();
            for _ in 0..4 {
                let response = world
                    .exchange(reader, World::get_request(object, bytes.len()))
                    .unwrap();
                assert_ne!(response.status, 404, "seed {seed}: authoritative 404");
                seen.insert(response.status);
                world.wait(Duration::from_secs(2));
            }
            eprintln!("seed {seed}: statuses under the cut {seen:?}");
            for (a, b) in pairs {
                world.harness.sim.heal(a, b);
            }
            world.harness.settle();
            world.wait(Duration::from_secs(2));
            world.live.insert(
                object,
                Written {
                    bytes: bytes.clone(),
                    etag: created.header("etag").unwrap().to_owned(),
                    replicas: 1,
                    mode: UploadMode::Never,
                    uploading: false,
                },
            );
            world.read(reader, object);
        }
    }
}
