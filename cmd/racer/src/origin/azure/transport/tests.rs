//! Real reactor, pool, and sockets against an in-process Azure Blob fake.

use super::super::identity;
use super::super::tokens::{AzureTokenCaches, AzureTokens};
use super::super::{AnonymousOnly, AzureLimits, AzureOrigin, AzureTokenSource};
use super::*;
use crate::admission::{AdmissionPolicy, ResourceClass};
use crate::config::Limits;
use crate::control::Snapshot;
use crate::control::cell::PublicationCell;
use crate::memory::BufferPool;
use crate::model::{
    MetadataSelector, ObjectId, ObjectVersion, PAGE_BYTES, PageId, PageNumber, StrongEtag,
};
use crate::origin::MetadataReply;
use crate::origin::azure_auth::{AuthEndpoints, IMDS_ADDRESS};
use crate::remote::tests::{MapResolver, serve, test_identity};
use crate::runtime::Reactor;
use crate::security::{OpaqueMetadata, OriginContext};
use crate::test_support::enrollment::{Directory, drive, scope};
use racer_io::ring::IoBuffer;
use racer_wire::control::{AzureCredential, CacheId, OriginDefinition};
use std::num::NonZeroUsize;
use std::sync::Mutex;
use std::sync::atomic::Ordering;
use std::time::Instant;

const SIZE: usize = 2 * PAGE_BYTES as usize + 5;

/// Blob contents and version shared with the fake server.
struct Blobs {
    etag: String,
}

fn byte(index: usize) -> u8 {
    (index % 251) as u8
}

fn header<'a>(head: &'a str, name: &str) -> Option<&'a str> {
    head.lines().find_map(|line| {
        let (field, value) = line.split_once(':')?;
        field.eq_ignore_ascii_case(name).then(|| value.trim())
    })
}

/// An error response; responses to HEAD carry the length but no body.
fn error(request: &str, status: u16, code: &str, extra: &str) -> Vec<u8> {
    let mut body = format!("<?xml version=\"1.0\"?><Error><Code>{code}</Code></Error>");
    let length = body.len();
    if request.starts_with("HEAD ") {
        body.clear();
    }
    format!(
        "HTTP/1.1 {status} X\r\nx-ms-error-code: {code}\r\n{extra}Content-Length: {length}\r\n\r\n{body}"
    )
    .into_bytes()
}

/// A faithful-enough Get Blob / Get Blob Properties for the container
/// `/account/models`: `big` (two pages and five bytes), `small` (five bytes),
/// and `empty`; `forbidden` and `busy` fail; anything else is missing.
fn azure(head: &str, blobs: &Mutex<Blobs>) -> Vec<u8> {
    let mut words = head.split(' ');
    let (method, target) = (words.next().unwrap(), words.next().unwrap());
    assert_eq!(header(head, "x-ms-version"), Some("2026-12-06"));
    let key = target.strip_prefix("/account/models/").unwrap();
    let size = match key {
        "big" => SIZE,
        "small" => 5,
        "empty" => 0,
        "forbidden" => return error(head, 403, "AuthorizationPermissionMismatch", ""),
        "busy" => return error(head, 503, "ServerBusy", ""),
        _ => return error(head, 404, "BlobNotFound", ""),
    };
    let etag = blobs.lock().unwrap().etag.clone();
    if header(head, "If-Match").is_some_and(|tag| tag != etag) {
        return error(head, 412, "ConditionNotMet", "");
    }
    let fields = format!(
        "ETag: {etag}\r\nx-ms-blob-type: BlockBlob\r\nContent-Type: application/octet-stream\r\nx-ms-version: 2026-12-06\r\n"
    );
    if method == "HEAD" {
        return format!("HTTP/1.1 200 OK\r\n{fields}Content-Length: {size}\r\n\r\n").into_bytes();
    }
    let range = header(head, "x-ms-range")
        .unwrap()
        .strip_prefix("bytes=")
        .unwrap();
    let (first, last) = range.split_once('-').unwrap();
    let first: usize = first.parse().unwrap();
    if first >= size {
        return error(
            head,
            416,
            "InvalidRange",
            &format!("Content-Range: bytes */{size}\r\n"),
        );
    }
    let last = last.parse::<usize>().unwrap().min(size - 1);
    let mut response = format!(
        "HTTP/1.1 206 Partial Content\r\n{fields}Content-Range: bytes {first}-{last}/{size}\r\nContent-Length: {}\r\n\r\n",
        last - first + 1
    )
    .into_bytes();
    response.extend((first..=last).map(byte));
    response
}

fn reactor(env: &racer_io::Env) -> Option<Rc<Reactor>> {
    // Probe io_uring availability the same way other socket tests do.
    crate::test_support::enrollment::reactor(env)?;
    let n = NonZeroUsize::new(1024).unwrap();
    let bytes = NonZeroUsize::new(4 * PAGE_BYTES as usize).unwrap();
    let reactor = Rc::new(Reactor::new(
        Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(Limits {
            plaintext_bytes: bytes,
            ciphertext_bytes: bytes,
            dirty_bytes: bytes,
            registered_bytes: bytes,
            request_context_bytes: bytes,
            flights: n,
            waiters_per_flight: n,
            queue_entries: NonZeroUsize::new(32).unwrap(),
            connections_per_neighbor: n,
            client_connections: n,
            peer_connections: n,
            outbound_connections: n,
            pipes: n,
            range_window_pages: n,
            header_bytes: NonZeroUsize::new(64 * 1024).unwrap(),
            placement_cache_bytes: n,
            path_cache_bytes: n,
            active_path_searches: n,
            cached_paths: n,
            retained_snapshots: n,
            metadata_entries: n,
            relay_transfers: n,
        }))),
        env,
    ));
    reactor.init().unwrap();
    Some(reactor)
}

struct Fixture<K> {
    reactor: Rc<Reactor>,
    origin: AzureOrigin<PoolTransport, K>,
    definition: OriginDefinition,
}

fn cache() -> CacheId {
    CacheId("44444444-4444-4444-8444-444444444444".into())
}

fn fixture<K: AzureTokenSource>(
    reactor: Rc<Reactor>,
    url: &str,
    credential: AzureCredential,
    resolver: MapResolver,
    trust: TrustRoots,
    tokens: K,
) -> Fixture<K> {
    let admission = reactor.admission().clone();
    let transport = PoolTransport::new(
        Rc::new(crate::http::new_pool(reactor.clone(), admission.clone(), 4)),
        Rc::new(crate::http::client_io(reactor.clone(), admission.clone())),
        Rc::new(resolver),
        Arc::new(trust),
    );
    let origin = AzureOrigin::new(
        transport,
        tokens,
        Arc::new(PublicationCell::new(Snapshot::retention(2))),
        admission.clone(),
        BufferPool::new(admission),
        Arc::new(AzureLimits::default()),
    );
    Fixture {
        reactor,
        origin,
        definition: OriginDefinition::azure(AzureContainerUrl::parse(url).unwrap(), credential),
    }
}

fn context(key: &str) -> OriginContext {
    let (raw, hash) = identity::canonical("models", key, "").unwrap();
    OriginContext {
        object: ObjectId {
            cache: cache(),
            key: hash,
        },
        metadata: Some(OpaqueMetadata::from_header(&raw).unwrap()),
        authorization: None,
    }
}

impl<K: AzureTokenSource> Fixture<K> {
    fn reserve(&self) -> flow_control::Charge<AdmissionPolicy> {
        self.reactor
            .admission()
            .reserve(
                Some(&cache()),
                ResourceClass::Plaintext,
                PAGE_BYTES as usize,
            )
            .unwrap()
    }

    fn bootstrap(&self, key: &str) -> Result<MetadataReply> {
        let context = context(key);
        let scope = scope();
        drive(
            &self.reactor,
            Box::pin(self.origin.bootstrap_with(
                &self.definition,
                &context,
                self.reserve(),
                &scope,
            )),
        )
    }

    fn metadata(&self, key: &str) -> Result<MetadataReply> {
        let context = context(key);
        let scope = scope();
        drive(
            &self.reactor,
            Box::pin(self.origin.metadata_with(
                &self.definition,
                &context,
                MetadataSelector::Fresh,
                &scope,
            )),
        )
    }

    fn page(&self, key: &str, etag: &StrongEtag, number: u64) -> Result<Vec<u8>> {
        let context = context(key);
        let page = PageId {
            version: ObjectVersion {
                object: context.object.clone(),
                etag: etag.clone(),
            },
            number: PageNumber(number),
        };
        let scope = scope();
        let page = drive(
            &self.reactor,
            Box::pin(self.origin.page_with(
                &self.definition,
                &context,
                &page,
                self.reserve(),
                &scope,
            )),
        )?;
        Ok(page.plaintext.bytes()?.to_vec())
    }
}

#[test]
fn plain_http_reads_pages_and_classifies_errors_on_one_connection() {
    let env = racer_io::Env::host();
    let Some(reactor) = reactor(&env) else { return };
    let blobs = Arc::new(Mutex::new(Blobs {
        etag: "\"v1\"".into(),
    }));
    let state = blobs.clone();
    let (address, accepted, server) = serve(None, 1, move |head, _| azure(head, &state));
    let url = format!("http://127.0.0.1:{}/account/models", address.port());
    let f = fixture(
        reactor,
        &url,
        AzureCredential::Anonymous,
        MapResolver::new(vec![]),
        TrustRoots::unavailable("/nonexistent/racer-bundle.pem".into()),
        AnonymousOnly,
    );

    // Empty blob: the ranged GET is unsatisfiable, so a HEAD follows.
    let empty = f.bootstrap("empty").unwrap();
    assert_eq!(empty.metadata.length, 0);
    assert!(empty.page_zero.is_none());

    let reply = f.bootstrap("big").unwrap();
    assert_eq!(reply.metadata.length, SIZE as u64);
    let etag = reply.metadata.version.etag.clone();
    assert_eq!(etag.as_str(), "\"v1\"");
    let zero = reply.page_zero.unwrap();
    let zero = zero.plaintext.bytes().unwrap();
    assert_eq!(zero.len(), PAGE_BYTES as usize);
    assert!(zero.iter().enumerate().all(|(i, b)| *b == byte(i)));
    let one = f.page("big", &etag, 1).unwrap();
    assert_eq!(one.len(), PAGE_BYTES as usize);
    assert!(
        one.iter()
            .enumerate()
            .all(|(i, b)| *b == byte(PAGE_BYTES as usize + i))
    );
    let two = f.page("big", &etag, 2).unwrap();
    assert_eq!(
        two,
        (2 * PAGE_BYTES as usize..SIZE)
            .map(byte)
            .collect::<Vec<_>>()
    );

    let small = f.bootstrap("small").unwrap();
    assert_eq!(small.metadata.length, 5);
    assert_eq!(
        small.page_zero.unwrap().plaintext.bytes().unwrap(),
        &[0, 1, 2, 3, 4]
    );

    // The blob is overwritten mid-read.
    blobs.lock().unwrap().etag = "\"v2\"".into();
    assert_eq!(
        f.page("big", &etag, 1).err(),
        Some(Error::VersionUnavailable)
    );
    assert_eq!(f.metadata("missing").err(), Some(Error::NotFound));
    assert_eq!(
        f.page("missing", &etag, 0).err(),
        Some(Error::VersionUnavailable)
    );
    assert_eq!(f.bootstrap("forbidden").err(), Some(Error::OriginForbidden));
    assert_eq!(f.bootstrap("busy").err(), Some(Error::Unavailable));
    assert_eq!(
        f.metadata("big").unwrap().metadata.version.etag.as_str(),
        "\"v2\""
    );

    // Error bodies were drained, so every request reused one connection.
    assert_eq!(accepted.load(Ordering::SeqCst), 1);
    drop(f);
    server.join().unwrap();
}

#[test]
fn https_with_a_private_ca_by_name() {
    let env = racer_io::Env::host();
    let Some(reactor) = reactor(&env) else { return };
    let identity = test_identity(&["account.blob.example"]);
    let blobs = Arc::new(Mutex::new(Blobs {
        etag: "\"v1\"".into(),
    }));
    let state = blobs.clone();
    let (address, accepted, server) = serve(Some(identity.server.clone()), 1, move |head, _| {
        assert!(head.contains("\r\nHost: account.blob.example:"));
        azure(head, &state)
    });
    let url = format!(
        "https://account.blob.example:{}/account/models",
        address.port()
    );
    let f = fixture(
        reactor,
        &url,
        AzureCredential::Anonymous,
        MapResolver::new(vec![address]),
        TrustRoots::fixed(http1::tls::client_config(identity.roots.clone())),
        AnonymousOnly,
    );
    let reply = f.bootstrap("big").unwrap();
    assert_eq!(reply.metadata.length, SIZE as u64);
    let two = f.page("big", &reply.metadata.version.etag, 2).unwrap();
    assert_eq!(
        two,
        (2 * PAGE_BYTES as usize..SIZE)
            .map(byte)
            .collect::<Vec<_>>()
    );
    assert_eq!(f.metadata("missing").err(), Some(Error::NotFound));
    assert_eq!(accepted.load(Ordering::SeqCst), 1);
    drop(f);
    server.join().unwrap();
}

#[test]
fn https_without_a_trust_bundle_fails_per_request() {
    let env = racer_io::Env::host();
    // The load happens once, at construction; requests never reread it.
    let trust = TrustRoots::load("/nonexistent/racer-bundle.pem".into());
    assert_eq!(trust.config().err(), Some(Error::Unavailable));
    assert_eq!(trust.config().err(), Some(Error::Unavailable));
    let Some(reactor) = reactor(&env) else { return };
    let f = fixture(
        reactor,
        "https://account.blob.example/models",
        AzureCredential::Anonymous,
        MapResolver::new(vec![]),
        trust,
        AnonymousOnly,
    );
    assert_eq!(f.bootstrap("big").err(), Some(Error::Unavailable));
}

/// Workload identity without a tenant: the first request goes out anonymously,
/// the storage challenge names the tenant, and the retry carries a token.
#[test]
fn workload_identity_learns_its_tenant_from_the_storage_challenge() {
    let env = racer_io::Env::host();
    let Some(reactor) = reactor(&env) else { return };
    let identity = test_identity(&["login.example"]);
    let directory = Directory::new();
    let token_file = directory.0.join("token");
    std::fs::write(&token_file, "assertion").unwrap();
    let (login, _, login_server) = serve(Some(identity.server.clone()), 1, |head, _| {
        assert!(head.starts_with("POST /tenant-1/oauth2/v2.0/token "));
        let body = "{\"access_token\":\"wi-token\",\"expires_in\":3599,\"token_type\":\"Bearer\"}";
        format!(
            "HTTP/1.1 200 OK\r\nContent-Length: {}\r\n\r\n{body}",
            body.len()
        )
        .into_bytes()
    });
    let blobs = Arc::new(Mutex::new(Blobs {
        etag: "\"v1\"".into(),
    }));
    let seen = Arc::new(Mutex::new(Vec::new()));
    let (state, record) = (blobs.clone(), seen.clone());
    let (storage, accepted, storage_server) = serve(None, 1, move |head, _| {
        let authorization = header(head, "Authorization").map(str::to_owned);
        record.lock().unwrap().push(authorization.clone());
        match authorization.as_deref() {
            Some("Bearer wi-token") => azure(head, &state),
            _ => error(
                head,
                401,
                "NoAuthenticationInformation",
                "WWW-Authenticate: Bearer authorization_uri=https://login.microsoftonline.com/tenant-1/oauth2/authorize resource_id=https://storage.azure.com\r\n",
            ),
        }
    });
    let caches = Arc::new(AzureTokenCaches::new(AuthEndpoints {
        imds: crate::remote::RemoteEndpoint::http(IMDS_ADDRESS, 80).unwrap(),
        authority: format!("https://login.example:{}/", login.port()),
        federated_token_file: Some(token_file),
        tls: Some(http1::tls::client_config(identity.roots.clone())),
    }));
    let tokens = AzureTokens::new(caches, reactor.clone())
        .with_resolver(Rc::new(MapResolver::new(vec![login])));
    let f = fixture(
        reactor,
        &format!("http://127.0.0.1:{}/account/models", storage.port()),
        AzureCredential::WorkloadIdentity {
            client_id: "app-id".into(),
            tenant_id: None,
        },
        MapResolver::new(vec![]),
        TrustRoots::unavailable("/nonexistent/racer-bundle.pem".into()),
        tokens,
    );
    assert_eq!(f.metadata("small").unwrap().metadata.length, 5);
    assert_eq!(f.metadata("small").unwrap().metadata.length, 5);
    assert_eq!(
        *seen.lock().unwrap(),
        vec![
            None,
            Some("Bearer wi-token".to_owned()),
            Some("Bearer wi-token".to_owned())
        ]
    );
    assert_eq!(accepted.load(Ordering::SeqCst), 1);
    drop(f);
    storage_server.join().unwrap();
    login_server.join().unwrap();
}

#[test]
fn decoded_response_head_stays_charged_while_the_exchange_lives() {
    let env = racer_io::Env::host();
    let Some(reactor) = reactor(&env) else { return };
    let blobs = Arc::new(Mutex::new(Blobs {
        etag: "\"v1\"".into(),
    }));
    let state = blobs.clone();
    let (address, accepted, server) = serve(None, 1, move |head, _| azure(head, &state));
    let url = format!("http://127.0.0.1:{}/account/models", address.port());
    let f = fixture(
        reactor.clone(),
        &url,
        AzureCredential::Anonymous,
        MapResolver::new(vec![]),
        TrustRoots::unavailable("/nonexistent/racer-bundle.pem".into()),
        AnonymousOnly,
    );
    let container = AzureContainerUrl::parse(&url).unwrap();
    let request = |method: &str, key: &str, range: bool| {
        let mut headers = vec![
            http1::Header::new("Host", container.host_header().as_bytes()),
            http1::Header::new("x-ms-version", b"2026-12-06"),
        ];
        if range {
            headers.push(http1::Header::new("x-ms-range", b"bytes=0-4"));
        }
        MessageHead {
            start: StartLine::Request {
                method: method.into(),
                target: format!("/account/models/{key}"),
            },
            headers,
        }
    };
    let admission = reactor.admission().clone();
    let used = || admission.used(ResourceClass::RequestContext);
    let scope = scope();
    let exchange = |method: &str, key: &str, range: bool| {
        drive(
            &reactor,
            f.origin
                .transport
                .exchange(&container, request(method, key, range), &scope),
        )
        .unwrap()
    };
    // Prime the pool so connection and staging buffers are already counted.
    exchange("HEAD", "small", false).finish().unwrap();
    let baseline = used();

    // finish
    let held = exchange("HEAD", "small", false);
    assert!(used() > baseline);
    held.finish().unwrap();
    assert_eq!(used(), baseline);

    // read_body
    let held = exchange("GET", "small", true);
    assert!(used() > baseline);
    let buffer = f.origin.buffers.plaintext(f.reserve(), 5).unwrap();
    let body = drive(&reactor, held.read_body(buffer, &scope)).unwrap();
    assert_eq!(body.bytes().unwrap(), &[0, 1, 2, 3, 4]);
    drop(body);
    assert_eq!(used(), baseline);

    // discard of an error body
    let held = exchange("GET", "missing", true);
    assert!(used() > baseline);
    drive(&reactor, held.discard(&scope)).unwrap();
    assert_eq!(used(), baseline);

    // drop without finishing closes the connection and releases the head.
    let held = exchange("HEAD", "small", false);
    assert!(used() > baseline);
    drop(held);
    assert!(used() <= baseline);

    assert!(accepted.load(Ordering::SeqCst) <= 2);
    drop(f);
    server.join().unwrap();
}

fn refused_address() -> std::net::SocketAddr {
    // Bind then drop, so connects to this port are refused.
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    listener.local_addr().unwrap()
}

#[test]
fn connect_failure_fails_over_to_the_next_resolved_address() {
    let env = racer_io::Env::host();
    let Some(reactor) = reactor(&env) else { return };
    let identity = test_identity(&["account.blob.example"]);
    let blobs = Arc::new(Mutex::new(Blobs {
        etag: "\"v1\"".into(),
    }));
    let state = blobs.clone();
    let (address, accepted, server) = serve(Some(identity.server.clone()), 1, move |head, _| {
        azure(head, &state)
    });
    let dead = std::net::SocketAddr::new(address.ip(), refused_address().port());
    let url = format!(
        "https://account.blob.example:{}/account/models",
        address.port()
    );
    // The dead address is first, so the first connect is refused.
    let f = fixture(
        reactor,
        &url,
        AzureCredential::Anonymous,
        MapResolver::new(vec![dead, address]),
        TrustRoots::fixed(http1::tls::client_config(identity.roots.clone())),
        AnonymousOnly,
    );
    let reply = f.bootstrap("big").unwrap();
    assert_eq!(reply.metadata.length, SIZE as u64);
    for _ in 0..3 {
        f.page("big", &reply.metadata.version.etag, 2).unwrap();
    }
    assert_eq!(accepted.load(Ordering::SeqCst), 1);
    drop(f);
    server.join().unwrap();
}

#[test]
fn connect_failure_on_every_address_is_reported() {
    let env = racer_io::Env::host();
    let Some(reactor) = reactor(&env) else { return };
    let dead = refused_address();
    let url = format!("http://account.blob.example:{}/account/models", dead.port());
    let f = fixture(
        reactor,
        &url,
        AzureCredential::Anonymous,
        MapResolver::new(vec![dead, refused_address()]),
        TrustRoots::unavailable("/nonexistent/racer-bundle.pem".into()),
        AnonymousOnly,
    );
    assert!(f.bootstrap("big").is_err());
}

#[test]
fn attempt_scope_narrows_only_non_final_attempts() {
    let parent = scope();
    // Final attempt: the full request scope.
    assert_eq!(attempt_scope(&parent, 1).deadline.0, parent.deadline.0);
    // 10s over two attempts is capped at ATTEMPT_CAP.
    let now = Instant::now();
    let first = attempt_scope(&parent, 2).deadline.0;
    assert!(first <= Instant::now() + ATTEMPT_CAP);
    assert!(first >= now + ATTEMPT_CAP - Duration::from_millis(100));
    // 10s over 100 attempts is 100ms, raised to the floor.
    let many = attempt_scope(&parent, 100).deadline.0;
    assert!(many >= now + ATTEMPT_FLOOR - Duration::from_millis(10));
    assert!(many <= Instant::now() + ATTEMPT_FLOOR);
    // The floor never extends past the request deadline.
    let mut tight = scope();
    tight.deadline.0 = Instant::now() + Duration::from_millis(100);
    assert_eq!(attempt_scope(&tight, 2).deadline.0, tight.deadline.0);
    // Narrowed scopes share the request's cancellation.
    let narrowed = attempt_scope(&parent, 2);
    parent.cancel().unwrap();
    assert_eq!(narrowed.check(), Err(Error::Cancelled));
}

#[test]
fn stalled_handshake_fails_over_before_the_request_deadline() {
    let env = racer_io::Env::host();
    let Some(reactor) = reactor(&env) else { return };
    let identity = test_identity(&["account.blob.example"]);
    let blobs = Arc::new(Mutex::new(Blobs {
        etag: "\"v1\"".into(),
    }));
    let state = blobs.clone();
    let (address, accepted, server) = serve(Some(identity.server.clone()), 1, move |head, _| {
        azure(head, &state)
    });
    // The kernel completes TCP into this listener's backlog, but nothing ever
    // answers the TLS ClientHello.
    let stalled = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let url = format!(
        "https://account.blob.example:{}/account/models",
        address.port()
    );
    let f = fixture(
        reactor.clone(),
        &url,
        AzureCredential::Anonymous,
        MapResolver::new(vec![stalled.local_addr().unwrap(), address]),
        TrustRoots::fixed(http1::tls::client_config(identity.roots.clone())),
        AnonymousOnly,
    );
    let container = AzureContainerUrl::parse(&url).unwrap();
    let head = MessageHead {
        start: StartLine::Request {
            method: "HEAD".into(),
            target: "/account/models/small".into(),
        },
        headers: vec![
            http1::Header::new("Host", container.host_header().as_bytes()),
            http1::Header::new("x-ms-version", b"2026-12-06"),
        ],
    };
    // Two attempts over 2s: the stalled one is cut off after about 1s.
    let mut scope = scope();
    let started = Instant::now();
    scope.deadline.0 = started + Duration::from_secs(2);
    let exchange = drive(
        &reactor,
        f.origin.transport.exchange(&container, head, &scope),
    )
    .unwrap();
    let elapsed = started.elapsed();
    assert_eq!(
        exchange.head().unique("Content-Length").unwrap(),
        Some(&b"5"[..])
    );
    exchange.finish().unwrap();
    assert!(elapsed < Duration::from_millis(1800), "took {elapsed:?}");
    assert!(scope.check().is_ok());
    assert_eq!(accepted.load(Ordering::SeqCst), 1);
    drop(f);
    drop(stalled);
    server.join().unwrap();
}

#[test]
fn endpoints_for_unpublished_origins_are_dropped() {
    let env = racer_io::Env::host();
    let Some(reactor) = reactor(&env) else { return };
    let admission = reactor.admission().clone();
    let transport = PoolTransport::new(
        Rc::new(crate::http::new_pool(reactor.clone(), admission.clone(), 4)),
        Rc::new(crate::http::client_io(reactor, admission)),
        Rc::new(MapResolver::new(Vec::new())),
        Arc::new(TrustRoots::unavailable(
            http1::tls::DEFAULT_TRUST_BUNDLE.into(),
        )),
    );
    let origin = |url: &str| {
        OriginDefinition::azure(
            AzureContainerUrl::parse(url).unwrap(),
            AzureCredential::Anonymous,
        )
    };
    let kept = origin("http://kept.example:10000/account/models");
    let gone = origin("http://gone.example:10000/account/models");
    transport.endpoint(&kept.azure.container_url).unwrap();
    // An exchange in flight holds its own clone of the endpoint.
    let held = transport.endpoint(&gone.azure.container_url).unwrap();
    assert_eq!(transport.endpoint_count(), 2);

    transport.retain(&[&kept]);
    assert_eq!(transport.endpoint_count(), 1);
    let Endpoint::Remote(remote) = &held else {
        panic!("remote endpoint");
    };
    assert_eq!(remote.address_count(), 0);
    // The live endpoint and its state survive; a re-published origin starts fresh.
    transport.endpoint(&kept.azure.container_url).unwrap();
    assert_eq!(transport.endpoint_count(), 1);
    transport.retain(&[]);
    assert_eq!(transport.endpoint_count(), 0);
}

#[test]
fn admission_wait_does_not_spend_an_address_attempt() {
    let env = racer_io::Env::host();
    let Some(reactor) = reactor(&env) else { return };
    let blobs = Arc::new(Mutex::new(Blobs {
        etag: "\"v1\"".into(),
    }));
    let state = blobs.clone();
    let (address, accepted, server) = serve(None, 2, move |head, _| azure(head, &state));
    let dead = std::net::SocketAddr::new(address.ip(), refused_address().port());
    let admission = reactor.admission().clone();
    // One connection per endpoint, so a second exchange waits for admission.
    let transport = PoolTransport::new(
        Rc::new(crate::http::new_pool(reactor.clone(), admission.clone(), 1)),
        Rc::new(crate::http::client_io(reactor.clone(), admission)),
        Rc::new(MapResolver::new(vec![address, dead])),
        Arc::new(TrustRoots::unavailable(
            "/nonexistent/racer-bundle.pem".into(),
        )),
    );
    let url = format!(
        "http://account.blob.example:{}/account/models",
        address.port()
    );
    let container = AzureContainerUrl::parse(&url).unwrap();
    let head = || MessageHead {
        start: StartLine::Request {
            method: "HEAD".into(),
            target: "/account/models/small".into(),
        },
        headers: vec![
            http1::Header::new("Host", container.host_header().as_bytes()),
            http1::Header::new("x-ms-version", b"2026-12-06"),
        ],
    };
    // The first exchange takes the healthy address and holds the only slot.
    let scope = scope();
    let held = drive(&reactor, transport.exchange(&container, head(), &scope)).unwrap();
    let held = std::cell::RefCell::new(Some(held));

    // The second exchange starts on the dead address. Its first narrowed
    // checkout (half of 2s) expires while waiting for admission; the slot
    // frees after that. The wait must not spend an attempt, so the refused
    // connect still fails over to the healthy address.
    let mut scope = scope.clone();
    let started = Instant::now();
    scope.deadline.0 = started + Duration::from_secs(2);
    let release = started + Duration::from_millis(1300);
    let waiting = transport.exchange(&container, head(), &scope);
    let freeing = std::future::poll_fn(|_| {
        if Instant::now() >= release {
            drop(held.borrow_mut().take());
            std::task::Poll::Ready(())
        } else {
            std::task::Poll::Pending
        }
    });
    let exchange = drive(
        &reactor,
        Box::pin(async move { futures::future::join(waiting, freeing).await.0 }),
    )
    .unwrap();
    assert_eq!(
        exchange.head().unique("Content-Length").unwrap(),
        Some(&b"5"[..])
    );
    exchange.finish().unwrap();
    assert!(started.elapsed() >= Duration::from_millis(1300));
    assert_eq!(accepted.load(Ordering::SeqCst), 2);
    drop(transport);
    server.join().unwrap();
}

/// Storage closes a pooled connection after the next request arrives but
/// before answering, as when its idle timeout races the request. The request
/// retries once on a fresh connection.
#[test]
fn reused_connection_closed_before_response_retries_on_fresh_connection() {
    use std::io::{Read as _, Write as _};
    let env = racer_io::Env::host();
    let Some(reactor) = reactor(&env) else { return };
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let blobs = Arc::new(Mutex::new(Blobs {
        etag: "\"v1\"".into(),
    }));
    let server = std::thread::spawn(move || {
        let (mut first, _) = listener.accept().unwrap();
        let (head, _) = crate::remote::tests::read_request(&mut first).unwrap();
        first.write_all(&azure(&head, &blobs)).unwrap();
        // The next request arrives and the socket closes unanswered.
        crate::remote::tests::read_request(&mut first).unwrap();
        drop(first);
        let (mut second, _) = listener.accept().unwrap();
        let (head, _) = crate::remote::tests::read_request(&mut second).unwrap();
        second.write_all(&azure(&head, &blobs)).unwrap();
        let _ = second.read_to_end(&mut Vec::new());
    });
    let url = format!("http://127.0.0.1:{}/account/models", address.port());
    let f = fixture(
        reactor,
        &url,
        AzureCredential::Anonymous,
        MapResolver::new(vec![]),
        TrustRoots::unavailable("/nonexistent/racer-bundle.pem".into()),
        AnonymousOnly,
    );
    assert_eq!(f.metadata("small").unwrap().metadata.length, 5);
    assert_eq!(f.metadata("small").unwrap().metadata.length, 5);
    drop(f);
    server.join().unwrap();
}
