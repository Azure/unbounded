//! Put Block and Put Block List against a scripted transport.

use super::super::*;
use super::block_id;
use crate::config::Limits;
use crate::model::{PAGE_BYTES, RequestId};
use crate::origin::upload::{CommitConflict, RetryPolicy, UploadSink};
use crate::security::OpaqueMetadata;
use futures::executor::block_on;
use std::cell::RefCell;
use std::collections::VecDeque;
use std::num::NonZeroUsize;
use std::time::{Duration, Instant};

#[derive(Default)]
struct Fake {
    script: RefCell<VecDeque<MessageHead>>,
    sent: RefCell<Vec<(MessageHead, Option<Vec<u8>>)>>,
}

struct FakeExchange(MessageHead);

impl AzureTransport for Rc<Fake> {
    type Exchange = FakeExchange;

    fn exchange<'a>(
        &'a self,
        _url: &'a AzureContainerUrl,
        head: MessageHead,
        _scope: &'a RequestScope,
    ) -> Operation<'a, FakeExchange> {
        self.sent.borrow_mut().push((head, None));
        let next = self.script.borrow_mut().pop_front();
        Box::pin(async move { next.map(FakeExchange).ok_or(Error::Io) })
    }

    fn exchange_body<'a>(
        &'a self,
        _url: &'a AzureContainerUrl,
        head: MessageHead,
        body: &'a [u8],
        _scope: &'a RequestScope,
    ) -> Operation<'a, FakeExchange> {
        self.sent.borrow_mut().push((head, Some(body.to_vec())));
        let next = self.script.borrow_mut().pop_front();
        Box::pin(async move { next.map(FakeExchange).ok_or(Error::Io) })
    }
}

impl AzureExchange for FakeExchange {
    fn head(&self) -> &MessageHead {
        &self.0
    }
    fn finish(self) -> Result<()> {
        Ok(())
    }
    fn read_body<'a>(
        self,
        _: PlaintextBuffer,
        _: &'a RequestScope,
    ) -> Operation<'a, PlaintextBuffer>
    where
        Self: 'a,
    {
        Box::pin(async { Err(Error::Internal) })
    }
    fn discard<'a>(self, _: &'a RequestScope) -> Operation<'a, ()>
    where
        Self: 'a,
    {
        Box::pin(async { Ok(()) })
    }
}

/// Bearer token `t1`, then `t2` after a rejection when `refresh` is set.
#[derive(Default)]
struct Tokens {
    refresh: bool,
    rejected: RefCell<u32>,
}

impl AzureTokenSource for Rc<Tokens> {
    fn bearer<'a>(
        &'a self,
        credential: &'a AzureCredential,
        _: &'a RequestScope,
    ) -> Operation<'a, Option<BearerToken>> {
        let n = *self.rejected.borrow();
        Box::pin(async move {
            Ok(match credential {
                AzureCredential::Anonymous => None,
                _ => Some(BearerToken::new(format!("t{}", n + 1).into_bytes())?),
            })
        })
    }
    fn rejected(&self, _: &AzureCredential, _: Option<&[u8]>, _: Option<&[u8]>) -> bool {
        *self.rejected.borrow_mut() += 1;
        self.refresh
    }
}

struct Harness {
    origin: AzureOrigin<Rc<Fake>, Rc<Tokens>>,
    fake: Rc<Fake>,
    definition: OriginDefinition,
    snapshots: Arc<PublicationCell<Snapshot>>,
}

fn harness(credential: AzureCredential, refresh: bool) -> Harness {
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
    let snapshots = Arc::new(PublicationCell::new(Snapshot::retention(2)));
    let origin = AzureOrigin::new(
        fake.clone(),
        Rc::new(Tokens {
            refresh,
            ..Tokens::default()
        }),
        snapshots.clone(),
        admission.clone(),
        BufferPool::new(admission),
        Arc::new(AzureLimits::default()),
    );
    let definition = OriginDefinition::azure(
        AzureContainerUrl::parse("https://acct.blob.core.windows.net/models").unwrap(),
        credential,
    );
    Harness {
        origin,
        fake,
        definition,
        snapshots,
    }
}

fn cache() -> CacheId {
    CacheId("44444444-4444-4444-8444-444444444444".into())
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

const VERSION: &[u8; 32] = b"0123456789abcdef0123456789abcdef";

impl Harness {
    /// Queue a response written as raw HTTP/1.1 head text.
    fn script(&self, raw: &[u8]) {
        let text = std::str::from_utf8(raw).unwrap();
        let mut lines = text.split("\r\n");
        let status = lines
            .next()
            .unwrap()
            .split(' ')
            .nth(1)
            .unwrap()
            .parse()
            .unwrap();
        let headers = lines
            .take_while(|line| !line.is_empty())
            .map(|line| {
                let (name, value) = line.split_once(':').unwrap();
                http1::Header::new(name, value.trim().as_bytes())
            })
            .collect();
        self.fake.script.borrow_mut().push_back(MessageHead {
            start: http1::StartLine::Response { status },
            headers,
        });
    }

    fn put_block(&self, ctx: &OriginContext, page: u64, data: &[u8]) -> Result<()> {
        let id = block_id(VERSION, page);
        block_on(
            self.origin
                .put_block_with(&self.definition, ctx, &id, data, &scope()),
        )
    }

    fn commit(
        &self,
        ctx: &OriginContext,
        pages: u64,
        content_type: Option<&str>,
    ) -> Result<std::result::Result<StrongEtag, CommitConflict>> {
        let ids: Vec<_> = (0..pages).map(|p| block_id(VERSION, p)).collect();
        block_on(self.origin.put_block_list_with(
            &self.definition,
            ctx,
            &ids,
            content_type,
            &scope(),
        ))
    }

    fn last(&self) -> (MessageHead, Option<Vec<u8>>) {
        let sent = self.fake.sent.borrow();
        let (head, body) = sent.last().unwrap();
        (crate::http::copy_head(head), body.clone())
    }
}

fn target(head: &MessageHead) -> String {
    match &head.start {
        http1::StartLine::Request { target, .. } => target.clone(),
        http1::StartLine::Response { .. } => panic!("request"),
    }
}

#[test]
fn block_ids_match_wire_format() {
    use base64::Engine;
    let id = block_id(VERSION, 0x1234);
    let raw = base64::engine::general_purpose::STANDARD
        .decode(id)
        .unwrap();
    assert_eq!(
        raw,
        b"0123456789abcdef0123456789abcdef0000000000001234".as_slice()
    );
    // Every ID of a blob has the same length.
    assert!(
        (0..1000)
            .chain([u64::MAX])
            .all(|p| block_id(VERSION, p).len() == 64)
    );
}

#[test]
fn put_block_sends_body_crc_and_token() {
    let h = harness(AzureCredential::ManagedIdentity { client_id: None }, false);
    let ctx = context("dir/obj", "");
    let crc = super::super::crc64::header_value(super::super::crc64::checksum(b"page"));
    h.script(
        format!("HTTP/1.1 201 Created\r\nx-ms-content-crc64: {crc}\r\nContent-Length: 0\r\n\r\n")
            .as_bytes(),
    );
    h.put_block(&ctx, 3, b"page").unwrap();
    let (head, body) = h.last();
    assert_eq!(body.as_deref(), Some(&b"page"[..]));
    assert!(target(&head).starts_with("/models/dir%2Fobj?comp=block&blockid="));
    assert_eq!(
        head.unique("Authorization").unwrap(),
        Some(&b"Bearer t1"[..])
    );
    assert_eq!(head.unique("Content-Length").unwrap(), Some(&b"4"[..]));
    assert_eq!(
        head.unique("x-ms-content-crc64").unwrap(),
        Some(crc.as_bytes())
    );

    // Anonymous credential: no Authorization header.
    let anon = harness(AzureCredential::Anonymous, false);
    anon.script(b"HTTP/1.1 201 Created\r\nContent-Length: 0\r\n\r\n");
    anon.put_block(&ctx, 0, b"x").unwrap();
    assert_eq!(anon.last().0.unique("Authorization").unwrap(), None);

    // A response echoing a different CRC is not for this body.
    h.script(
        b"HTTP/1.1 201 Created\r\nx-ms-content-crc64: AAAAAAAAAAA=\r\nContent-Length: 0\r\n\r\n",
    );
    assert_eq!(h.put_block(&ctx, 3, b"page").err(), Some(Error::BadGateway));
}

#[test]
fn put_block_errors_classify() {
    let h = harness(AzureCredential::ManagedIdentity { client_id: None }, false);
    let ctx = context("obj", "");
    for (raw, expected) in [
        (
            &b"HTTP/1.1 403 Forbidden\r\nx-ms-error-code: AuthorizationPermissionMismatch\r\nContent-Length: 0\r\n\r\n"[..],
            Error::OriginForbidden,
        ),
        (
            b"HTTP/1.1 503 Busy\r\nx-ms-error-code: ServerBusy\r\nContent-Length: 0\r\n\r\n",
            Error::Unavailable,
        ),
        (
            b"HTTP/1.1 500 Err\r\nx-ms-error-code: InternalError\r\nContent-Length: 0\r\n\r\n",
            Error::Unavailable,
        ),
        (b"HTTP/1.1 429 Slow\r\nContent-Length: 0\r\n\r\n", Error::Unavailable),
        (b"HTTP/1.1 401 No\r\nContent-Length: 0\r\n\r\n", Error::OriginRejected),
        (
            b"HTTP/1.1 400 Bad\r\nx-ms-error-code: InvalidBlockId\r\nContent-Length: 0\r\n\r\n",
            Error::Internal,
        ),
    ] {
        h.script(raw);
        assert_eq!(h.put_block(&ctx, 0, b"x").err(), Some(expected));
    }
    // Versioned identities cannot be written.
    let versioned = context("obj", "2024-01-01T00:00:00.0000000Z");
    assert_eq!(
        h.put_block(&versioned, 0, b"x").err(),
        Some(Error::InvalidRequest)
    );
    // Missing identity metadata.
    let mut bare = context("obj", "");
    bare.metadata = None;
    assert_eq!(
        h.put_block(&bare, 0, b"x").err(),
        Some(Error::InvalidRequest)
    );
}

#[test]
fn put_block_retries_once_with_refreshed_token() {
    let h = harness(AzureCredential::ManagedIdentity { client_id: None }, true);
    let ctx = context("obj", "");
    h.script(b"HTTP/1.1 401 Expired\r\nContent-Length: 0\r\n\r\n");
    h.script(b"HTTP/1.1 201 Created\r\nContent-Length: 0\r\n\r\n");
    h.put_block(&ctx, 0, b"body").unwrap();
    let sent = h.fake.sent.borrow();
    assert_eq!(sent.len(), 2);
    assert_eq!(
        sent[0].0.unique("Authorization").unwrap(),
        Some(&b"Bearer t1"[..])
    );
    assert_eq!(
        sent[1].0.unique("Authorization").unwrap(),
        Some(&b"Bearer t2"[..])
    );
    // The body is resent with the retried head.
    assert_eq!(sent[1].1.as_deref(), Some(&b"body"[..]));
}

#[test]
fn put_block_retry_policy_rides_out_503() {
    let h = harness(AzureCredential::ManagedIdentity { client_id: None }, false);
    let ctx = context("obj", "");
    h.script(b"HTTP/1.1 503 Busy\r\nx-ms-error-code: ServerBusy\r\nContent-Length: 0\r\n\r\n");
    h.script(b"HTTP/1.1 500 Err\r\nContent-Length: 0\r\n\r\n");
    h.script(b"HTTP/1.1 201 Created\r\nContent-Length: 0\r\n\r\n");
    let policy = RetryPolicy {
        attempts: 5,
        base: Duration::from_millis(1),
        cap: Duration::from_millis(2),
    };
    let scope = scope();
    let id = block_id(VERSION, 0);
    block_on(policy.run(
        &scope,
        1,
        |_| async { Ok(()) },
        |_| {
            h.origin
                .put_block_with(&h.definition, &ctx, &id, b"x", &scope)
        },
    ))
    .unwrap();
    assert_eq!(h.fake.sent.borrow().len(), 3);
}

#[test]
fn put_block_list_commits_and_returns_etag() {
    let h = harness(AzureCredential::ManagedIdentity { client_id: None }, false);
    let ctx = context("obj", "");
    h.script(b"HTTP/1.1 201 Created\r\nETag: \"0x8DCAFE\"\r\nTransfer-Encoding: chunked\r\n\r\n");
    let etag = h
        .commit(&ctx, 2, Some("application/json"))
        .unwrap()
        .unwrap();
    assert_eq!(etag.as_str(), "\"0x8DCAFE\"");
    let (head, body) = h.last();
    assert_eq!(target(&head), "/models/obj?comp=blocklist");
    let body = body.unwrap();
    let ids: Vec<_> = (0..2).map(|p| block_id(VERSION, p)).collect();
    assert_eq!(body, super::super::request::block_list_xml(&ids).unwrap());
    assert_eq!(head.unique("If-None-Match").unwrap(), Some(&b"*"[..]));
    assert_eq!(
        head.unique("x-ms-blob-content-type").unwrap(),
        Some(&b"application/json"[..])
    );
    assert_eq!(
        head.unique("Content-Length").unwrap(),
        Some(body.len().to_string().as_bytes())
    );
}

#[test]
fn put_block_list_conflicts_and_errors() {
    let h = harness(AzureCredential::ManagedIdentity { client_id: None }, false);
    let ctx = context("obj", "");
    h.script(
        b"HTTP/1.1 409 Conflict\r\nx-ms-error-code: BlobAlreadyExists\r\nContent-Length: 0\r\n\r\n",
    );
    assert_eq!(h.commit(&ctx, 1, None).unwrap(), Err(CommitConflict));
    h.script(b"HTTP/1.1 412 Precondition Failed\r\nx-ms-error-code: ConditionNotMet\r\nContent-Length: 0\r\n\r\n");
    assert_eq!(h.commit(&ctx, 1, None).unwrap(), Err(CommitConflict));
    // Other 409s are not a lost race.
    h.script(
        b"HTTP/1.1 409 Conflict\r\nx-ms-error-code: LeaseIdMissing\r\nContent-Length: 0\r\n\r\n",
    );
    assert_eq!(h.commit(&ctx, 1, None).err(), Some(Error::Internal));
    h.script(b"HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n");
    assert_eq!(h.commit(&ctx, 1, None).err(), Some(Error::OriginForbidden));
    h.script(b"HTTP/1.1 503 Busy\r\nx-ms-error-code: ServerBusy\r\nContent-Length: 0\r\n\r\n");
    assert_eq!(h.commit(&ctx, 1, None).err(), Some(Error::Unavailable));
    // 400 InvalidBlockList (a block was never staged).
    h.script(b"HTTP/1.1 400 Bad\r\nx-ms-error-code: InvalidBlockList\r\nContent-Length: 0\r\n\r\n");
    assert_eq!(h.commit(&ctx, 1, None).err(), Some(Error::Internal));
    // Success without a usable ETag is a gateway fault.
    h.script(b"HTTP/1.1 201 Created\r\nContent-Length: 0\r\n\r\n");
    assert_eq!(h.commit(&ctx, 1, None).err(), Some(Error::BadGateway));
    h.script(b"HTTP/1.1 201 Created\r\nETag: W/\"weak\"\r\nContent-Length: 0\r\n\r\n");
    assert_eq!(h.commit(&ctx, 1, None).err(), Some(Error::BadGateway));
    // An empty list and an invalid content type never reach the wire.
    let before = h.fake.sent.borrow().len();
    assert_eq!(h.commit(&ctx, 0, None).err(), Some(Error::InvalidRequest));
    assert_eq!(
        h.commit(&ctx, 1, Some("bad\r\nvalue")).err(),
        Some(Error::InvalidRequest)
    );
    assert_eq!(h.fake.sent.borrow().len(), before);
}

#[test]
fn trait_uses_published_definition() {
    let h = harness(AzureCredential::Anonymous, false);
    let ctx = context("obj", "");
    let id = block_id(VERSION, 0);
    // Nothing published: unavailable.
    assert_eq!(
        block_on(UploadSink::put_block(&h.origin, &ctx, &id, b"x", &scope())).err(),
        Some(Error::Unavailable)
    );
    let mut publication = racer_wire::control::decode_publication(include_bytes!(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/wire/testdata/publication.json"
    )))
    .unwrap();
    publication.caches[0].origin = Some(h.definition.clone());
    let target = crate::control::publication::PublicationTarget::new(
        publication.cluster.clone(),
        h.snapshots.clone(),
        racer_io::Clock::host(),
    );
    target.apply(publication).unwrap();
    let published = h.snapshots.current().unwrap().unwrap().caches[0].id.clone();
    let mut ctx = ctx;
    ctx.object.cache = published;
    h.script(b"HTTP/1.1 201 Created\r\nContent-Length: 0\r\n\r\n");
    block_on(UploadSink::put_block(&h.origin, &ctx, &id, b"x", &scope())).unwrap();
    h.script(b"HTTP/1.1 201 Created\r\nETag: \"e\"\r\nContent-Length: 0\r\n\r\n");
    let etag = block_on(UploadSink::put_block_list(
        &h.origin,
        &ctx,
        &[id],
        None,
        &scope(),
    ))
    .unwrap()
    .unwrap();
    assert_eq!(etag.as_str(), "\"e\"");
}
