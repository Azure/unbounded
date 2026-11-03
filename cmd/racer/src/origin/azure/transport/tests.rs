//! Real reactor, pool, and sockets against an in-process Azure Blob fake.

use super::super::identity;
use super::super::tokens::{AzureTokenCaches, AzureTokens};
use super::super::{AnonymousOnly, AzureLimits, AzureOrigin, AzureTokenSource};
use super::*;
use crate::admission::{AdmissionPolicy, ResourceClass};
use crate::config::Limits;
use crate::control::Snapshot;
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
use controlplane::Published;
use racer_control_wire::{AzureCredential, CacheId, OriginDefinition};
use std::num::NonZeroUsize;
use std::sync::atomic::Ordering;
use uring_runtime::reactor::IoBuffer;

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

fn reactor() -> Option<Rc<Reactor>> {
    // Probe io_uring availability the same way other socket tests do.
    crate::test_support::enrollment::reactor()?;
    let n = NonZeroUsize::new(1024).unwrap();
    let bytes = NonZeroUsize::new(4 * PAGE_BYTES as usize).unwrap();
    let reactor = Rc::new(Reactor::new(Rc::new(flow_control::Quotas::new(
        AdmissionPolicy::new(Limits {
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
        }),
    ))));
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
        Arc::new(Published::new(Snapshot::retention(2))),
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
    let Some(reactor) = reactor() else { return };
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
        TrustRoots::at("/nonexistent/racer-bundle.pem".into()),
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
    let Some(reactor) = reactor() else { return };
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
    let trust = TrustRoots::at("/nonexistent/racer-bundle.pem".into());
    assert_eq!(trust.config().err(), Some(Error::Unavailable));
    // Failures are remembered rather than reloaded on every request.
    assert_eq!(trust.config().err(), Some(Error::Unavailable));
    let Some(reactor) = reactor() else { return };
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
    let Some(reactor) = reactor() else { return };
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
        TrustRoots::at("/nonexistent/racer-bundle.pem".into()),
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
    let Some(reactor) = reactor() else { return };
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
        TrustRoots::at("/nonexistent/racer-bundle.pem".into()),
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
