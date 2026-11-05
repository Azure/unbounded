use super::response::response;
use super::*;
use crate::config::Limits;
use crate::model::PAGE_BYTES;
use crate::model::{CacheKey, ContentType, RequestId};
use crate::security::OpaqueMetadata;
use futures::executor::block_on;
use racer_io::ring::IoBuffer;
use racer_wire::control::{AzureOriginDefinition, CacheId};
use std::cell::{Cell, RefCell};
use std::collections::VecDeque;
use std::num::NonZeroUsize;
use std::time::{Duration, Instant};

const ETAG: &[u8] = b"\"0x8DC\"";

/// One scripted response: head, body bytes, and how the exchange ended.
struct Scripted {
    head: MessageHead,
    body: Vec<u8>,
}

#[derive(Default)]
struct Fake {
    script: RefCell<VecDeque<Scripted>>,
    sent: RefCell<Vec<MessageHead>>,
    finished: Rc<Cell<usize>>,
    read: Rc<Cell<usize>>,
    discarded: Rc<Cell<usize>>,
    /// Live origin count of each `retain` call.
    retained: RefCell<Vec<usize>>,
}

struct FakeExchange {
    head: MessageHead,
    body: Vec<u8>,
    finished: Rc<Cell<usize>>,
    read: Rc<Cell<usize>>,
    discarded: Rc<Cell<usize>>,
}

impl AzureTransport for Rc<Fake> {
    type Exchange = FakeExchange;
    fn exchange<'a>(
        &'a self,
        url: &'a AzureContainerUrl,
        head: MessageHead,
        scope: &'a RequestScope,
    ) -> Operation<'a, FakeExchange> {
        Box::pin(async move {
            scope.check()?;
            assert_eq!(url.container(), "models");
            self.sent.borrow_mut().push(head);
            let next = self.script.borrow_mut().pop_front().ok_or(Error::Io)?;
            Ok(FakeExchange {
                head: next.head,
                body: next.body,
                finished: self.finished.clone(),
                read: self.read.clone(),
                discarded: self.discarded.clone(),
            })
        })
    }

    fn retain(&self, live: &[&OriginDefinition]) {
        self.retained.borrow_mut().push(live.len());
    }
}

impl AzureExchange for FakeExchange {
    fn head(&self) -> &MessageHead {
        &self.head
    }
    fn finish(self) -> Result<()> {
        self.finished.set(self.finished.get() + 1);
        Ok(())
    }
    fn read_body<'a>(
        self,
        mut buffer: PlaintextBuffer,
        _scope: &'a RequestScope,
    ) -> Operation<'a, PlaintextBuffer>
    where
        Self: 'a,
    {
        Box::pin(async move {
            let target = buffer.bytes_mut()?;
            // A connection that closes early leaves a short body.
            if self.body.len() < target.len() {
                return Err(Error::Io);
            }
            target.copy_from_slice(&self.body[..target.len()]);
            self.read.set(self.read.get() + 1);
            Ok(buffer)
        })
    }
    fn discard<'a>(self, _scope: &'a RequestScope) -> Operation<'a, ()>
    where
        Self: 'a,
    {
        self.discarded.set(self.discarded.get() + 1);
        Box::pin(async { Ok(()) })
    }
}

/// Records credentials and returns a token for every non-anonymous one.
/// `rejections` records `rejected` calls; `retry` is its answer.
#[derive(Default)]
struct Tokens(
    RefCell<Vec<AzureCredential>>,
    RefCell<Vec<(Option<Vec<u8>>, bool)>>,
    Cell<bool>,
);

impl AzureTokenSource for Rc<Tokens> {
    fn bearer<'a>(
        &'a self,
        credential: &'a AzureCredential,
        _scope: &'a RequestScope,
    ) -> Operation<'a, Option<BearerToken>> {
        Box::pin(async move {
            self.0.borrow_mut().push(credential.clone());
            Ok(match credential {
                AzureCredential::Anonymous => None,
                _ => Some(BearerToken::new(b"secret-token".to_vec())?),
            })
        })
    }
    fn rejected(&self, _: &AzureCredential, challenge: Option<&[u8]>, sent: Option<&[u8]>) -> bool {
        if let Some(sent) = sent {
            assert_eq!(sent, b"secret-token");
        }
        self.1
            .borrow_mut()
            .push((challenge.map(<[u8]>::to_vec), sent.is_some()));
        self.2.get()
    }
}

struct Harness {
    origin: AzureOrigin<Rc<Fake>, Rc<Tokens>>,
    fake: Rc<Fake>,
    tokens: Rc<Tokens>,
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    limits: Arc<AzureLimits>,
    definition: OriginDefinition,
    snapshots: Arc<PublicationCell<Snapshot>>,
}

fn harness() -> Harness {
    let n = NonZeroUsize::new(32).unwrap();
    let bytes = NonZeroUsize::new(4 * PAGE_BYTES as usize).unwrap();
    let limits = Limits {
        plaintext_bytes: bytes,
        ciphertext_bytes: bytes,
        dirty_bytes: bytes,
        registered_bytes: bytes,
        request_context_bytes: bytes,
        flights: n,
        waiters_per_flight: n,
        queue_entries: n,
        connections_per_neighbor: n,
        client_connections: n,
        peer_connections: n,
        outbound_connections: n,
        pipes: n,
        range_window_pages: n,
        header_bytes: NonZeroUsize::new(32768).unwrap(),
        placement_cache_bytes: n,
        path_cache_bytes: n,
        active_path_searches: n,
        cached_paths: n,
        retained_snapshots: n,
        metadata_entries: n,
        relay_transfers: n,
    };
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits)));
    let fake = Rc::new(Fake::default());
    let tokens = Rc::new(Tokens::default());
    let shared = Arc::new(AzureLimits::default());
    let snapshots = Arc::new(PublicationCell::new(Snapshot::retention(2)));
    let origin = AzureOrigin::new(
        fake.clone(),
        tokens.clone(),
        snapshots.clone(),
        admission.clone(),
        BufferPool::new(admission.clone()),
        shared.clone(),
    );
    let mut definition = OriginDefinition::azure(
        AzureContainerUrl::parse("https://acct.blob.core.windows.net/models").unwrap(),
        AzureCredential::ManagedIdentity { client_id: None },
    );
    definition.metadata_ttl = Duration::from_secs(30);
    Harness {
        origin,
        fake,
        tokens,
        admission,
        limits: shared,
        definition,
        snapshots,
    }
}

fn cache() -> CacheId {
    CacheId("00000000-0000-4000-8000-000000000001".into())
}

fn context(key: &str, version: &str) -> OriginContext {
    let (raw, hash) = identity::canonical("models", key, version).unwrap();
    OriginContext {
        object: ObjectId {
            cache: cache(),
            key: hash,
        },
        metadata: Some(OpaqueMetadata::from_header(&raw).unwrap()),
        authorization: None,
    }
}

fn scope() -> RequestScope {
    RequestScope::host(RequestId([1; 16]), Instant::now() + Duration::from_secs(5)).unwrap()
}

fn page(context: &OriginContext, number: u64) -> PageId {
    PageId {
        version: ObjectVersion {
            object: context.object.clone(),
            etag: StrongEtag::parse(ETAG).unwrap(),
        },
        number: PageNumber(number),
    }
}

impl Harness {
    fn reserve(&self) -> flow_control::Charge<AdmissionPolicy> {
        self.admission
            .reserve(
                Some(&cache()),
                ResourceClass::Plaintext,
                PAGE_BYTES as usize,
            )
            .unwrap()
    }

    fn push(&self, status: u16, fields: &[(&str, &[u8])], body: Vec<u8>) {
        self.fake.script.borrow_mut().push_back(Scripted {
            head: response(status, fields),
            body,
        });
    }

    /// A well-formed blob response with `Content-Length` and optional range.
    fn blob(&self, status: u16, length: u64, range: Option<String>, extra: &[(&str, &[u8])]) {
        let length_text = length.to_string();
        let mut fields: Vec<(&str, &[u8])> = vec![
            ("Content-Length", length_text.as_bytes()),
            ("x-ms-blob-type", b"BlockBlob"),
            ("ETag", ETAG),
            ("Content-Type", b"application/x-model"),
        ];
        if let Some(range) = &range {
            fields.push(("Content-Range", range.as_bytes()));
        }
        // Extra fields replace defaults of the same name.
        fields.retain(|field| !extra.iter().any(|e| e.0.eq_ignore_ascii_case(field.0)));
        fields.extend_from_slice(extra);
        let body = if status == 200 || status == 206 {
            (0..length).map(|i| i as u8).collect()
        } else {
            Vec::new()
        };
        self.push(status, &fields, body);
    }

    fn error(&self, status: u16, code: &str) {
        self.push(
            status,
            &[
                ("Content-Length", b"200"),
                ("x-ms-error-code", code.as_bytes()),
            ],
            vec![b'x'; 200],
        );
    }

    fn sent(&self, index: usize) -> std::cell::Ref<'_, MessageHead> {
        std::cell::Ref::map(self.fake.sent.borrow(), |sent| &sent[index])
    }

    fn bootstrap(&self, context: &OriginContext) -> Result<MetadataReply> {
        block_on(
            self.origin
                .bootstrap_with(&self.definition, context, self.reserve(), &scope()),
        )
    }

    fn page(&self, context: &OriginContext, number: u64) -> Result<OriginPage> {
        block_on(self.origin.page_with(
            &self.definition,
            context,
            &page(context, number),
            self.reserve(),
            &scope(),
        ))
    }

    fn metadata(
        &self,
        context: &OriginContext,
        selector: MetadataSelector,
    ) -> Result<MetadataReply> {
        block_on(
            self.origin
                .metadata_with(&self.definition, context, selector, &scope()),
        )
    }
}

fn header<'a>(head: &'a MessageHead, name: &str) -> Option<&'a [u8]> {
    head.unique(name).unwrap()
}

fn target(head: &MessageHead) -> (&str, &str) {
    match &head.start {
        http1::StartLine::Request { method, target } => (method, target),
        _ => panic!("request expected"),
    }
}

#[test]
fn bootstrap_reads_metadata_and_page_zero_in_one_request() {
    let h = harness();
    let total = 3 * PAGE_BYTES + 7;
    h.blob(
        206,
        PAGE_BYTES,
        Some(format!("bytes 0-{}/{total}", PAGE_BYTES - 1)),
        &[],
    );
    let context = context("dir/model.bin", "");
    let before = SystemTime::now();
    let reply = h.bootstrap(&context).unwrap();
    assert_eq!(reply.metadata.length, total);
    assert_eq!(reply.metadata.version.etag.as_bytes(), ETAG);
    assert_eq!(reply.metadata.version.object, context.object);
    assert_eq!(
        reply.metadata.content_type,
        Some(ContentType::parse(b"application/x-model").unwrap())
    );
    let expires = reply.metadata.expires_at.as_system_time();
    assert!(
        expires >= before + Duration::from_secs(29)
            && expires <= SystemTime::now() + Duration::from_secs(30)
    );
    reply.metadata.expires_at.to_unix_millis().unwrap();
    let page = reply.page_zero.unwrap();
    assert_eq!(page.metadata, reply.metadata);
    let bytes = page.plaintext.bytes().unwrap();
    assert_eq!(bytes.len(), PAGE_BYTES as usize);
    assert_eq!(bytes[257], 1);
    let sent = h.sent(0);
    assert_eq!(target(&sent), ("GET", "/models/dir%2Fmodel.bin"));
    assert_eq!(header(&sent, "x-ms-range"), Some(&b"bytes=0-16777215"[..]));
    assert_eq!(
        header(&sent, "Authorization"),
        Some(&b"Bearer secret-token"[..])
    );
    assert_eq!(header(&sent, "If-Match"), None);
    assert_eq!(
        header(&sent, "Host"),
        Some(&b"acct.blob.core.windows.net"[..])
    );
    assert_eq!(h.fake.sent.borrow().len(), 1);
    assert_eq!(h.fake.read.get(), 1);
    assert_eq!(
        *h.tokens.0.borrow(),
        vec![AzureCredential::ManagedIdentity { client_id: None }]
    );
    assert_eq!(h.limits.in_flight(&cache()), 0);
}

#[test]
fn bootstrap_small_object_and_version_id() {
    let h = harness();
    h.blob(
        206,
        5,
        Some("bytes 0-4/5".into()),
        &[("x-ms-version-id", b"2024-01-01T00:00:00.0000000Z")],
    );
    let versioned = context("k", "2024-01-01T00:00:00.0000000Z");
    let reply = h.bootstrap(&versioned).unwrap();
    assert_eq!(reply.metadata.length, 5);
    assert_eq!(
        reply.page_zero.unwrap().plaintext.bytes().unwrap(),
        &[0, 1, 2, 3, 4]
    );
    assert_eq!(
        target(&h.sent(0)).1,
        "/models/k?versionid=2024-01-01T00%3A00%3A00.0000000Z"
    );

    // A service that ignores the range still serves a page-sized body.
    h.blob(200, 3, None, &[]);
    let reply = h.bootstrap(&context("k", "")).unwrap();
    assert_eq!(reply.metadata.length, 3);
    assert_eq!(reply.page_zero.unwrap().plaintext.bytes().unwrap().len(), 3);
    h.blob(200, 0, None, &[]);
    let reply = h.bootstrap(&context("k", "")).unwrap();
    assert_eq!(reply.metadata.length, 0);
    assert!(reply.page_zero.is_none());
    assert_eq!(h.fake.finished.get(), 1);
    h.blob(200, PAGE_BYTES + 1, None, &[]);
    assert_eq!(
        h.bootstrap(&context("k", "")).map(|_| ()),
        Err(Error::BadGateway)
    );
}

#[test]
fn empty_blob_falls_back_to_head() {
    let h = harness();
    h.error(416, "InvalidRange");
    h.blob(200, 0, None, &[]);
    let reply = h.bootstrap(&context("empty", "")).unwrap();
    assert_eq!(reply.metadata.length, 0);
    assert!(reply.page_zero.is_none());
    assert_eq!(target(&h.sent(1)), ("HEAD", "/models/empty"));
    assert_eq!(header(&h.sent(1), "x-ms-range"), None);
    assert_eq!(h.fake.finished.get(), 1);
    // Replaced by a nonempty blob between the two requests.
    h.error(416, "InvalidRange");
    h.blob(200, 9, None, &[]);
    assert_eq!(
        h.bootstrap(&context("empty", "")).map(|_| ()),
        Err(Error::Unavailable)
    );
    // Deleted between the two requests.
    h.error(416, "InvalidRange");
    h.error(404, "BlobNotFound");
    assert_eq!(
        h.bootstrap(&context("empty", "")).map(|_| ()),
        Err(Error::NotFound)
    );
}

#[test]
fn page_reads_pinned_range() {
    let h = harness();
    let total = 2 * PAGE_BYTES + 10;
    let first = 2 * PAGE_BYTES;
    h.blob(
        206,
        10,
        Some(format!("bytes {first}-{}/{total}", first + 9)),
        &[],
    );
    let context = context("k", "");
    let page = h.page(&context, 2).unwrap();
    assert_eq!(page.metadata.length, total);
    assert_eq!(page.plaintext.bytes().unwrap().len(), 10);
    let sent = h.sent(0);
    assert_eq!(target(&sent), ("GET", "/models/k"));
    assert_eq!(
        header(&sent, "x-ms-range").unwrap(),
        format!("bytes={first}-{}", first + PAGE_BYTES - 1).as_bytes()
    );
    assert_eq!(header(&sent, "If-Match"), Some(ETAG));
    drop(sent);

    // Middle page of a larger object.
    h.blob(
        206,
        PAGE_BYTES,
        Some(format!("bytes {PAGE_BYTES}-{}/{total}", 2 * PAGE_BYTES - 1)),
        &[],
    );
    assert_eq!(
        h.page(&context, 1)
            .unwrap()
            .plaintext
            .bytes()
            .unwrap()
            .len(),
        PAGE_BYTES as usize
    );
}

#[test]
fn page_rejects_version_changes_and_bad_framing() {
    let h = harness();
    let context = context("k", "");
    let range = || Some("bytes 0-4/5".to_string());
    let fake_head = |etag: &'static [u8]| {
        vec![
            ("Content-Length", &b"5"[..]),
            ("x-ms-blob-type", b"BlockBlob"),
            ("ETag", etag),
            ("Content-Range", b"bytes 0-4/5"),
        ]
    };
    // ETag changed even though the service ignored If-Match.
    h.push(206, &fake_head(b"\"other\""), vec![0; 5]);
    assert_eq!(
        h.page(&context, 0).map(|_| ()),
        Err(Error::VersionUnavailable)
    );
    h.error(412, "ConditionNotMet");
    assert_eq!(
        h.page(&context, 0).map(|_| ()),
        Err(Error::VersionUnavailable)
    );
    // A pinned miss means the version is gone.
    h.error(404, "BlobNotFound");
    assert_eq!(
        h.page(&context, 0).map(|_| ()),
        Err(Error::VersionUnavailable)
    );
    for bad in [
        "bytes 1-5/6",
        "bytes 0-3/5",
        "bytes 0-4/6",
        "bytes 0-4/4",
        "bytes */5",
    ] {
        h.blob(206, 5, Some(bad.into()), &[]);
        assert_eq!(
            h.page(&context, 0).map(|_| ()),
            Err(Error::BadGateway),
            "{bad}"
        );
    }
    // Content-Length disagrees with the range.
    h.blob(206, 4, range(), &[]);
    assert_eq!(h.page(&context, 0).map(|_| ()), Err(Error::BadGateway));
    // Short body: the connection closed early.
    h.push(206, &fake_head(ETAG), vec![0; 3]);
    assert_eq!(h.page(&context, 0).map(|_| ()), Err(Error::Unavailable));
    // Chunked bodies have no fixed length.
    h.blob(206, 5, range(), &[("Transfer-Encoding", b"chunked")]);
    assert_eq!(h.page(&context, 0).map(|_| ()), Err(Error::BadGateway));
    h.blob(200, 5, None, &[]);
    assert_eq!(h.page(&context, 0).map(|_| ()), Err(Error::BadGateway));
    h.blob(206, 5, range(), &[("x-ms-access-tier", b"Archive")]);
    assert_eq!(h.page(&context, 0).map(|_| ()), Err(Error::Internal));
    h.blob(206, 5, range(), &[("Content-Encoding", b"gzip")]);
    assert_eq!(h.page(&context, 0).map(|_| ()), Err(Error::Internal));
    h.error(409, "BlobArchived");
    assert_eq!(h.page(&context, 0).map(|_| ()), Err(Error::Internal));
    assert_eq!(h.fake.read.get(), 0);
    assert_eq!(h.fake.script.borrow().len(), 0);
    // Pages beyond the addressable range never reach the transport.
    let sent = h.fake.sent.borrow().len();
    assert_eq!(
        h.page(&context, u64::MAX / PAGE_BYTES).map(|_| ()),
        Err(Error::InvalidRange)
    );
    assert_eq!(h.fake.sent.borrow().len(), sent);
}

#[test]
fn metadata_fresh_and_pinned() {
    let h = harness();
    let context = context("k", "");
    h.blob(200, 1234, None, &[]);
    let reply = h.metadata(&context, MetadataSelector::Fresh).unwrap();
    assert_eq!(reply.metadata.length, 1234);
    assert!(reply.page_zero.is_none());
    let sent = h.sent(0);
    assert_eq!(target(&sent), ("HEAD", "/models/k"));
    assert_eq!(header(&sent, "If-Match"), None);
    drop(sent);
    assert_eq!(h.fake.finished.get(), 1);

    let pin = StrongEtag::parse(ETAG).unwrap();
    h.blob(200, 1234, None, &[]);
    h.metadata(&context, MetadataSelector::Pinned(pin.clone()))
        .unwrap();
    assert_eq!(header(&h.sent(1), "If-Match"), Some(ETAG));

    h.push(
        200,
        &[
            ("Content-Length", b"1"),
            ("x-ms-blob-type", b"BlockBlob"),
            ("ETag", b"\"new\""),
        ],
        Vec::new(),
    );
    assert_eq!(
        h.metadata(&context, MetadataSelector::Pinned(pin.clone()))
            .map(|_| ()),
        Err(Error::VersionUnavailable)
    );
    h.error(404, "BlobNotFound");
    assert_eq!(
        h.metadata(&context, MetadataSelector::Fresh).map(|_| ()),
        Err(Error::NotFound)
    );
    h.error(404, "BlobNotFound");
    assert_eq!(
        h.metadata(&context, MetadataSelector::Pinned(pin))
            .map(|_| ()),
        Err(Error::VersionUnavailable)
    );
    // HEAD error responses carry no error code body, only the status.
    h.push(404, &[("Content-Length", b"0")], Vec::new());
    assert_eq!(
        h.metadata(&context, MetadataSelector::Fresh).map(|_| ()),
        Err(Error::NotFound)
    );
}

#[test]
fn errors_are_classified() {
    let h = harness();
    let context = context("k", "");
    for (status, code, want) in [
        (
            403,
            "AuthorizationPermissionMismatch",
            Error::OriginForbidden,
        ),
        (401, "InvalidAuthenticationInfo", Error::OriginRejected),
        (503, "ServerBusy", Error::Unavailable),
        (500, "InternalError", Error::Unavailable),
        (404, "ContainerNotFound", Error::NotFound),
        (409, "BlobArchived", Error::Internal),
        (400, "InvalidHeaderValue", Error::Internal),
    ] {
        h.error(status, code);
        assert_eq!(h.bootstrap(&context).map(|_| ()), Err(want), "{code}");
    }
    h.blob(
        206,
        5,
        Some("bytes 0-4/5".into()),
        &[("x-ms-archive-status", b"rehydrate-pending-to-hot")],
    );
    assert_eq!(h.bootstrap(&context).map(|_| ()), Err(Error::Internal));
    h.blob(
        206,
        5,
        Some("bytes 0-4/5".into()),
        &[("x-ms-blob-type", b"AppendBlob")],
    );
    assert_eq!(h.bootstrap(&context).map(|_| ()), Err(Error::Internal));
    h.push(
        206,
        &[
            ("Content-Length", b"5"),
            ("x-ms-blob-type", b"BlockBlob"),
            ("ETag", ETAG),
            ("ETag", ETAG),
            ("Content-Range", b"bytes 0-4/5"),
        ],
        vec![0; 5],
    );
    assert_eq!(h.bootstrap(&context).map(|_| ()), Err(Error::BadGateway));
    // Transport failure.
    assert_eq!(h.bootstrap(&context).map(|_| ()), Err(Error::Unavailable));
    assert_eq!(h.limits.in_flight(&cache()), 0);
}

#[test]
fn identity_is_checked_before_io() {
    let h = harness();
    let mut wrong_key = context("k", "");
    wrong_key.object.key = CacheKey([0; 32]);
    let mut absent = context("k", "");
    absent.metadata = None;
    let alternate = br#"{"schema":1,"bucket":"models","key":"\u006b"}"#;
    let mut spelled = context("k", "");
    spelled.metadata = Some(OpaqueMetadata::from_header(alternate).unwrap());
    spelled.object.key = CacheKey(sha2::Sha256::digest(alternate).into());
    for context in [&wrong_key, &absent, &spelled] {
        assert_eq!(h.bootstrap(context).map(|_| ()), Err(Error::InvalidRequest));
        assert_eq!(
            h.metadata(context, MetadataSelector::Fresh).map(|_| ()),
            Err(Error::InvalidRequest)
        );
    }
    let (raw, key) = identity::canonical("other", "k", "").unwrap();
    let other = OriginContext {
        object: ObjectId {
            cache: cache(),
            key,
        },
        metadata: Some(OpaqueMetadata::from_header(&raw).unwrap()),
        authorization: None,
    };
    assert_eq!(h.bootstrap(&other).map(|_| ()), Err(Error::OriginForbidden));
    assert!(h.fake.sent.borrow().is_empty());
    assert!(h.tokens.0.borrow().is_empty());
}

use sha2::Digest;

#[test]
fn reservations_must_belong_to_the_cache() {
    let h = harness();
    let context = context("k", "");
    let other = h
        .admission
        .reserve(
            Some(&CacheId("00000000-0000-4000-8000-000000000002".into())),
            ResourceClass::Plaintext,
            PAGE_BYTES as usize,
        )
        .unwrap();
    assert_eq!(
        block_on(
            h.origin
                .bootstrap_with(&h.definition, &context, other, &scope())
        )
        .map(|_| ()),
        Err(Error::InvalidConfiguration)
    );
    let mut stranger = context.clone_for_test();
    stranger.object.cache = CacheId("00000000-0000-4000-8000-000000000002".into());
    assert_eq!(
        block_on(h.origin.page_with(
            &h.definition,
            &stranger,
            &page(&context, 0),
            h.reserve(),
            &scope()
        ))
        .map(|_| ()),
        Err(Error::Unauthorized)
    );
    assert!(h.fake.sent.borrow().is_empty());
}

impl OriginContext {
    fn clone_for_test(&self) -> Self {
        Self {
            object: self.object.clone(),
            metadata: self
                .metadata
                .as_ref()
                .map(|value| OpaqueMetadata::from_header(value.as_header()).unwrap()),
            authorization: None,
        }
    }
}

#[test]
fn concurrency_limit_and_timeout() {
    let mut h = harness();
    h.definition.max_concurrent_requests = 2;
    let held = [
        h.limits.acquire(&cache(), 2).unwrap(),
        h.limits.acquire(&cache(), 2).unwrap(),
    ];
    assert_eq!(
        h.metadata(&context("k", ""), MetadataSelector::Fresh)
            .map(|_| ()),
        Err(Error::Overloaded)
    );
    // Other caches are independent.
    let other = CacheId("other".into());
    drop(h.limits.acquire(&other, 1).unwrap());
    drop(held);
    assert_eq!(h.limits.in_flight(&cache()), 0);
    h.blob(200, 1, None, &[]);
    h.metadata(&context("k", ""), MetadataSelector::Fresh)
        .unwrap();

    // request_timeout narrows the caller's deadline.
    h.definition.request_timeout = Duration::ZERO;
    assert_eq!(
        h.metadata(&context("k", ""), MetadataSelector::Fresh)
            .map(|_| ()),
        Err(Error::DeadlineExceeded)
    );
    assert_eq!(h.fake.sent.borrow().len(), 1);
}

#[test]
fn anonymous_credentials_send_no_authorization() {
    let mut h = harness();
    h.definition.azure = AzureOriginDefinition {
        container_url: AzureContainerUrl::parse("http://127.0.0.1:10000/devstoreaccount1/models")
            .unwrap(),
        credential: AzureCredential::Anonymous,
    };
    h.blob(200, 1, None, &[]);
    h.metadata(&context("a b", ""), MetadataSelector::Fresh)
        .unwrap();
    let sent = h.sent(0);
    assert_eq!(header(&sent, "Authorization"), None);
    assert_eq!(header(&sent, "Host"), Some(&b"127.0.0.1:10000"[..]));
    assert_eq!(target(&sent).1, "/devstoreaccount1/models/a%20b");
    assert!(
        block_on(AnonymousOnly.bearer(&AzureCredential::Anonymous, &scope()))
            .unwrap()
            .is_none()
    );
    assert!(matches!(
        block_on(AnonymousOnly.bearer(
            &AzureCredential::ManagedIdentity { client_id: None },
            &scope()
        )),
        Err(Error::Unavailable)
    ));
    assert_eq!(
        format!("{:?}", BearerToken::new(b"t".to_vec()).unwrap()),
        "BearerToken([redacted])"
    );
    assert!(BearerToken::new(b"a b".to_vec()).is_err());
}

#[test]
fn lookup_requires_a_published_origin() {
    let h = harness();
    assert_eq!(
        h.origin.definition(&context("k", "").object).map(|_| ()),
        Err(Error::Unavailable)
    );
}

mod router {
    use super::*;
    use crate::control::publication::PublicationTarget;
    use crate::origin::router::OriginRouter;
    use crate::read::candidates::CandidatePolicy;
    use crate::topology::{Member, Membership, Placement};
    use racer_wire::control::{CacheMode, ClusterId, MembershipVersion, NodeId};

    /// Counts adapter calls; never performs I/O.
    #[derive(Default)]
    struct Adapter(Cell<usize>);

    impl Origin for Rc<Adapter> {
        fn bootstrap_reserved<'a>(
            &'a self,
            _: &'a OriginAuthority,
            _: &'a OriginContext,
            _: flow_control::Charge<AdmissionPolicy>,
            _: &'a RequestScope,
        ) -> Operation<'a, MetadataReply> {
            self.0.set(self.0.get() + 1);
            Box::pin(async { Err(Error::NotImplemented) })
        }
        fn page_reserved<'a>(
            &'a self,
            _: &'a OriginAuthority,
            _: &'a OriginContext,
            _: &'a PageId,
            _: flow_control::Charge<AdmissionPolicy>,
            _: &'a RequestScope,
        ) -> Operation<'a, OriginPage> {
            self.0.set(self.0.get() + 1);
            Box::pin(async { Err(Error::NotImplemented) })
        }
        fn metadata<'a>(
            &'a self,
            _: &'a OriginAuthority,
            _: &'a OriginContext,
            _: MetadataSelector,
            _: &'a RequestScope,
        ) -> Operation<'a, MetadataReply> {
            self.0.set(self.0.get() + 1);
            Box::pin(async { Err(Error::NotImplemented) })
        }
    }

    /// Publish the shared fixture, with an Azure origin when `native`.
    fn publish(h: &Harness, native: bool) -> CacheId {
        let mut publication = racer_wire::control::decode_publication(include_bytes!(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/wire/testdata/publication.json"
        )))
        .unwrap();
        if native {
            publication.caches[0].origin = Some(h.definition.clone());
        }
        let target = PublicationTarget::new(
            publication.cluster.clone(),
            h.snapshots.clone(),
            racer_io::Clock::host(),
        );
        target.apply(publication).unwrap();
        h.snapshots.current().unwrap().unwrap().caches[0].id.clone()
    }

    fn authority(
        env: &racer_io::Env,
        h: &Harness,
        object: &ObjectId,
        page: u64,
    ) -> OriginAuthority {
        let node = NodeId("node".into());
        let membership = Arc::new(
            Membership::validate(
                MembershipVersion(1),
                vec![Member {
                    node: node.clone(),
                    shares: std::num::NonZeroU32::new(1).unwrap(),
                    peer_endpoint: "127.0.0.1:1234".into(),
                    rails: vec![],
                    site: String::new(),
                }],
            )
            .unwrap(),
        );
        let credentials = Rc::new(crate::security::CredentialCrypto::new(
            Rc::new(racer_crypto::identity::Keyring::new(
                ClusterId("cluster".into()),
                node.clone(),
                Arc::new(racer_crypto::identity::KeyEpochs::with_clock(
                    crate::enrollment::wall_clock(&racer_io::Clock::host()),
                )),
            )),
            h.admission.clone(),
            racer_io::WorkerCtx::host().rng,
        ));
        CandidatePolicy::new(
            node,
            Rc::new(Placement::new(2)),
            crate::test_support::NoPeers::requester(env),
            credentials,
            h.snapshots.clone(),
            racer_io::WorkerCtx::host(),
        )
        .local_authority(
            membership,
            object,
            PageNumber(page),
            CacheMode::PassthroughLocal,
        )
        .unwrap()
    }

    fn in_cache(cache: &CacheId) -> OriginContext {
        let mut context = context("k", "");
        context.object.cache = cache.clone();
        context
    }

    type Router = OriginRouter<Rc<Adapter>, Rc<Fake>, Rc<Tokens>>;

    fn router(h: Harness, native: bool) -> (Router, Rc<Adapter>, Rc<Fake>) {
        let adapter = Rc::new(Adapter::default());
        let fake = h.fake.clone();
        let router = OriginRouter::new(
            h.snapshots.clone(),
            adapter.clone(),
            native.then_some(h.origin),
        );
        (router, adapter, fake)
    }

    #[test]
    fn new_snapshots_prune_worker_state_once() {
        let h = harness();
        let apply = |native: bool, sequence: u64| {
            let mut publication = racer_wire::control::decode_publication(include_bytes!(concat!(
                env!("CARGO_MANIFEST_DIR"),
                "/wire/testdata/publication.json"
            )))
            .unwrap();
            publication.sequence = racer_wire::control::PublicationSequence(sequence);
            if native {
                publication.caches[0].origin = Some(h.definition.clone());
            }
            PublicationTarget::new(
                publication.cluster.clone(),
                h.snapshots.clone(),
                racer_io::Clock::host(),
            )
            .apply(publication)
            .unwrap();
            h.snapshots.current().unwrap().unwrap().caches[0].id.clone()
        };
        let object = in_cache(&apply(true, 1)).object;
        h.origin.definition(&object).unwrap();
        h.origin.definition(&object).unwrap();
        // One prune per generation, against its one native origin.
        assert_eq!(*h.fake.retained.borrow(), [1]);

        // The cache loses its origin in a new generation: nothing is live.
        apply(false, 2);
        assert_eq!(
            h.origin.definition(&object).map(|_| ()),
            Err(Error::Unavailable)
        );
        assert_eq!(*h.fake.retained.borrow(), [1, 0]);
    }

    #[test]
    fn native_caches_use_azure_and_others_use_the_adapter() {
        let env = racer_io::Env::host();
        let h = harness();
        let cache = publish(&h, true);
        let context = in_cache(&cache);
        let authority = authority(&env, &h, &context.object, 0);
        let reserve = || {
            h.admission
                .reserve(Some(&cache), ResourceClass::Plaintext, PAGE_BYTES as usize)
                .unwrap()
        };
        h.blob(206, 5, Some("bytes 0-4/5".into()), &[]);
        h.blob(200, 5, None, &[]);
        h.blob(206, 5, Some("bytes 0-4/5".into()), &[]);
        let (bootstrap, page_reservation) = (reserve(), reserve());
        let page = page(&context, 0);
        let (router, adapter, fake) = router(h, true);
        let reply =
            block_on(router.bootstrap_reserved(&authority, &context, bootstrap, &scope())).unwrap();
        assert_eq!(reply.metadata.length, 5);
        block_on(router.metadata(&authority, &context, MetadataSelector::Fresh, &scope())).unwrap();
        block_on(router.page_reserved(&authority, &context, &page, page_reservation, &scope()))
            .unwrap();
        assert_eq!(fake.sent.borrow().len(), 3);
        assert_eq!(adapter.0.get(), 0);

        // Authority is checked before Azure I/O.
        let mut other = context.clone_for_test();
        other.object.key.0[0] ^= 1;
        assert_eq!(
            block_on(router.metadata(&authority, &other, MetadataSelector::Fresh, &scope()))
                .map(|_| ()),
            Err(Error::Unauthorized)
        );
        // A cache UID the snapshot does not know goes to the adapter.
        let stranger = in_cache(&CacheId("55555555-5555-4555-8555-555555555555".into()));
        let _ = block_on(router.metadata(&authority, &stranger, MetadataSelector::Fresh, &scope()));
        assert_eq!(adapter.0.get(), 1);
        assert_eq!(fake.sent.borrow().len(), 3);
    }

    #[test]
    fn caches_without_an_origin_use_the_adapter() {
        let env = racer_io::Env::host();
        let h = harness();
        let cache = publish(&h, false);
        let context = in_cache(&cache);
        let authority = authority(&env, &h, &context.object, 0);
        let (router, adapter, fake) = router(h, true);
        let _ = block_on(router.metadata(&authority, &context, MetadataSelector::Fresh, &scope()));
        assert_eq!(adapter.0.get(), 1);
        assert!(fake.sent.borrow().is_empty());
    }

    #[test]
    fn without_azure_every_cache_uses_the_adapter() {
        let env = racer_io::Env::host();
        let h = harness();
        let cache = publish(&h, true);
        let context = in_cache(&cache);
        let authority = authority(&env, &h, &context.object, 0);
        let (router, adapter, fake) = router(h, false);
        let _ = block_on(router.metadata(&authority, &context, MetadataSelector::Fresh, &scope()));
        assert_eq!(adapter.0.get(), 1);
        assert!(fake.sent.borrow().is_empty());
    }
}

#[test]
fn unauthorized_retries_once_when_the_token_source_allows() {
    let h = harness();
    let context = context("k", "");
    let challenge: &[u8] =
        b"Bearer authorization_uri=https://login.microsoftonline.com/t/oauth2/authorize";
    h.push(
        401,
        &[("Content-Length", b"0"), ("WWW-Authenticate", challenge)],
        Vec::new(),
    );
    assert_eq!(
        h.metadata(&context, MetadataSelector::Fresh).map(|_| ()),
        Err(Error::OriginRejected)
    );
    assert_eq!(*h.tokens.1.borrow(), vec![(Some(challenge.to_vec()), true)]);
    assert_eq!(h.fake.discarded.get(), 1);
    h.tokens.2.set(true);
    for _ in 0..2 {
        h.push(
            401,
            &[("Content-Length", b"0"), ("WWW-Authenticate", challenge)],
            Vec::new(),
        );
    }
    h.blob(200, 4, None, &[]);
    // Retried once, then the second 401 is final.
    assert_eq!(
        h.metadata(&context, MetadataSelector::Fresh).map(|_| ()),
        Err(Error::OriginRejected)
    );
    assert_eq!(
        h.metadata(&context, MetadataSelector::Fresh)
            .unwrap()
            .metadata
            .length,
        4
    );
    assert_eq!(h.tokens.1.borrow().len(), 2);
    assert_eq!(h.fake.sent.borrow().len(), 4);
}

#[test]
fn limits_drop_counters_for_caches_with_nothing_in_flight() {
    let limits = AzureLimits::default();
    let held = limits.acquire(&cache(), 1).unwrap();
    // Many short-lived caches, for example created and deleted over time.
    for index in 0..100 {
        drop(
            limits
                .acquire(&CacheId(format!("gone-{index}")), 1)
                .unwrap(),
        );
    }
    assert!(limits.tracked() <= 32, "{}", limits.tracked());
    // The cache with an operation in flight keeps its count and its limit.
    assert_eq!(limits.in_flight(&cache()), 1);
    assert_eq!(
        limits.acquire(&cache(), 1).map(|_| ()),
        Err(Error::Overloaded)
    );
    drop(held);
    assert_eq!(limits.in_flight(&cache()), 0);
    drop(limits.acquire(&cache(), 1).unwrap());
}
