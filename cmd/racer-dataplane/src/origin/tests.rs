use super::*;
use crate::{
    control::snapshot::PublishedState,
    http::codec::Codec,
    model::{
        context::{Authorization, OpaqueMetadata},
        identity::{CacheId, CacheKey, ClusterId, ObjectId, RequestId, StrongEtag},
        limits::Limits,
    },
    runtime::reactor::Reactor,
};
use futures::executor::block_on;
use std::{
    future::Future,
    io::{Read, Write},
    num::NonZeroUsize,
    os::unix::net::{UnixListener, UnixStream},
    path::PathBuf,
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
    task::{Context, Poll},
    thread,
    time::{Duration, Instant},
};

fn context() -> OriginContext {
    OriginContext {
        object: ObjectId {
            cache: CacheId("cache".into()),
            key: CacheKey([0xab; 32]),
        },
        metadata: Some(OpaqueMetadata::from_header(b"opaque\xff").unwrap()),
        authorization: Some(Authorization::from_header(b"credential\xfe").unwrap()),
    }
}
fn scope() -> RequestScope {
    RequestScope::new(RequestId([1; 16]), Instant::now() + Duration::from_secs(5)).unwrap()
}
fn client() -> (OriginClient, Rc<Admission>, Rc<Reactor>) {
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
        pipes: n,
        range_window_pages: n,
        replay_entries: n,
        header_bytes: NonZeroUsize::new(32768).unwrap(),
        route_search_work: n,
        cached_rankings: n,
        cached_paths: n,
        retained_snapshots: n,
        metadata_entries: n,
        relay_transfers: n,
    };
    let admission = Rc::new(Admission::new(limits));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let pool = Rc::new(HttpPool::new(reactor.clone(), admission.clone(), 2));
    let io = Rc::new(HttpIo::with_admission(
        reactor.clone(),
        Codec::new(32768, PAGE_BYTES),
        admission.clone(),
    ));
    let snapshots = Rc::new(SnapshotStore::new(
        ClusterId("cluster".into()),
        Arc::new(PublishedState::default()),
        2,
    ));
    let buffers = Rc::new(BufferPool::new(admission.clone()));
    (
        OriginClient::new(snapshots, pool, io).with_buffers(admission.clone(), buffers),
        admission,
        reactor,
    )
}

fn drive<T>(reactor: &Reactor, future: impl Future<Output = T>) -> T {
    let mut future = std::pin::pin!(future);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let deadline = Instant::now() + Duration::from_secs(7);
    loop {
        if let Poll::Ready(result) = future.as_mut().poll(&mut cx) {
            return result;
        }
        assert!(
            Instant::now() < deadline,
            "origin operation made no progress"
        );
        reactor.poll_budgeted(128).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
}

struct SocketPath(PathBuf);
impl Drop for SocketPath {
    fn drop(&mut self) {
        let _ = std::fs::remove_file(&self.0);
    }
}
fn listen() -> (SocketPath, UnixListener, Endpoint) {
    static NEXT: AtomicUsize = AtomicUsize::new(0);
    // A short relative socket path stays inside the owned source directory and
    // below sockaddr_un's limit even when the worktree's absolute path is long.
    let path = PathBuf::from(format!(
        "src/origin/.origin-{}-{}.sock",
        std::process::id(),
        NEXT.fetch_add(1, Ordering::Relaxed)
    ));
    let listener = UnixListener::bind(&path).unwrap();
    listener.set_nonblocking(true).unwrap();
    (SocketPath(path.clone()), listener, Endpoint::Unix(path))
}
fn accept(listener: &UnixListener) -> UnixStream {
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        match listener.accept() {
            Ok((stream, _)) => {
                stream
                    .set_read_timeout(Some(Duration::from_secs(5)))
                    .unwrap();
                stream
                    .set_write_timeout(Some(Duration::from_secs(5)))
                    .unwrap();
                return stream;
            }
            Err(error)
                if error.kind() == std::io::ErrorKind::WouldBlock && Instant::now() < deadline =>
            {
                thread::sleep(Duration::from_millis(1))
            }
            Err(error) => panic!("origin accept failed: {error}"),
        }
    }
}
fn receive(stream: &mut UnixStream) -> MessageHead {
    let mut raw = Vec::new();
    while !raw.ends_with(b"\r\n\r\n") {
        assert!(raw.len() < 32768);
        let mut byte = [0];
        stream.read_exact(&mut byte).unwrap();
        raw.push(byte[0]);
    }
    Codec::new(32768, PAGE_BYTES)
        .decode_head(&raw)
        .unwrap()
        .unwrap()
        .0
}
fn check_request(
    head: &MessageHead,
    method: &str,
    pin: Option<&[u8]>,
    range: Option<&[u8]>,
    credentials: bool,
) {
    match &head.start {
        StartLine::Request {
            method: actual,
            target,
        } => {
            assert_eq!(actual, method);
            assert_eq!(target, &format!("/v1/objects/{}", "ab".repeat(32)));
        }
        _ => panic!("expected request"),
    }
    assert_eq!(head.unique("Host").unwrap(), Some(b"racer".as_slice()));
    assert_eq!(head.unique("If-Match").unwrap(), pin);
    assert_eq!(head.unique("Range").unwrap(), range);
    assert_eq!(
        head.unique("Authorization").unwrap(),
        credentials.then_some(b"credential\xfe".as_slice())
    );
    assert_eq!(
        head.unique("Racer-Metadata").unwrap(),
        credentials.then_some(b"opaque\xff".as_slice())
    );
}

#[test]
fn real_uds_bootstrap_head_and_pinned_page_reuse_without_context_retention() {
    let (_path, listener, endpoint) = listen();
    let server = thread::spawn(move || {
        // All three exchanges must use this one accepted connection.
        let mut stream = accept(&listener);
        check_request(
            &receive(&mut stream),
            "GET",
            None,
            Some(b"bytes=0-16777215"),
            true,
        );
        stream.write_all(b"HTTP/1.1 206 Partial Content\r\nContent-Length: 3\r\nContent-Type: application/octet-stream\r\nContent-Range: bytes 0-2/3\r\nETag: \"v\"\r\nRacer-Expires-At: 1234\r\n\r\na").unwrap();
        thread::sleep(Duration::from_millis(10));
        stream.write_all(b"bc").unwrap();
        check_request(&receive(&mut stream), "HEAD", Some(b"\"v\""), None, false);
        stream.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 3\r\nETag: \"v\"\r\nRacer-Expires-At: 1234\r\n\r\n").unwrap();
        check_request(
            &receive(&mut stream),
            "GET",
            Some(b"\"v\""),
            Some(b"bytes=0-16777215"),
            false,
        );
        stream.write_all(b"HTTP/1.1 206 Partial Content\r\nContent-Length: 3\r\nContent-Type: application/octet-stream\r\nContent-Range: bytes 0-2/3\r\nETag: \"v\"\r\nRacer-Expires-At: 1234\r\n\r\nab").unwrap();
        thread::sleep(Duration::from_millis(10));
        stream.write_all(b"c").unwrap();
    });
    let (client, admission, reactor) = client();
    let mut context = context();
    let scope = scope();
    let reply = drive(&reactor, client.bootstrap_at(&endpoint, &context, &scope)).unwrap();
    assert_eq!(reply.metadata.expires_at.to_unix_millis().unwrap(), 1234);
    assert_eq!(
        reply.page_zero.as_ref().unwrap().plaintext.bytes().unwrap(),
        b"abc"
    );
    let page = PageId {
        version: reply.metadata.version.clone(),
        number: PageNumber(0),
    };
    drop(reply);
    context.authorization = None;
    context.metadata = None;
    assert_eq!(
        drive(
            &reactor,
            client.metadata_at(
                &endpoint,
                &context,
                MetadataSelector::Pinned(page.version.etag.clone()),
                &scope
            )
        )
        .unwrap()
        .metadata
        .length,
        3
    );
    let received = drive(&reactor, client.page_at(&endpoint, &context, &page, &scope)).unwrap();
    assert_eq!(received.plaintext.bytes().unwrap(), b"abc");
    drop(received);
    assert_eq!(admission.used(ResourceClass::Plaintext), 0);
    client.pool.close();
    assert_eq!(admission.used(ResourceClass::Connection), 0);
    server.join().unwrap();
}

#[test]
fn real_uds_empty_bootstrap_and_truncated_page() {
    let (_path, listener, endpoint) = listen();
    let server = thread::spawn(move || {
        let mut stream = accept(&listener);
        receive(&mut stream);
        stream.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\nContent-Type: application/octet-stream\r\nETag: \"v\"\r\nRacer-Expires-At: 0\r\n\r\n").unwrap();
        receive(&mut stream);
        stream.write_all(b"HTTP/1.1 206 Partial Content\r\nContent-Length: 3\r\nContent-Type: application/octet-stream\r\nContent-Range: bytes 0-2/3\r\nETag: \"v\"\r\nRacer-Expires-At: 0\r\n\r\nab").unwrap();
    });
    let (client, admission, reactor) = client();
    let context = context();
    let scope = scope();
    let reply = drive(&reactor, client.bootstrap_at(&endpoint, &context, &scope)).unwrap();
    assert_eq!(reply.metadata.length, 0);
    assert!(reply.page_zero.is_none());
    let page = PageId {
        version: reply.metadata.version,
        number: PageNumber(0),
    };
    assert!(matches!(
        drive(&reactor, client.page_at(&endpoint, &context, &page, &scope)),
        Err(Error::BadGateway)
    ));
    assert_eq!(admission.used(ResourceClass::Plaintext), 0);
    assert_eq!(admission.used(ResourceClass::Connection), 0);
    server.join().unwrap();
}

#[test]
fn real_uds_errors_preserve_credential_and_version_contracts() {
    for (status, fields, expected) in [
        (401, "", Error::OriginRejected),
        (403, "", Error::OriginForbidden),
        (404, "", Error::BadGateway),
        (412, "", Error::VersionUnavailable),
        (
            416,
            "Content-Range: bytes */27\r\n",
            Error::UnsatisfiableRangeWithLength(27),
        ),
        (503, "", Error::Unavailable),
        (
            200,
            "ETag: \"wrong\"\r\nRacer-Expires-At: 0\r\n",
            Error::BadGateway,
        ),
        (401, "Range: x\r\nrange: x\r\n", Error::BadGateway),
    ] {
        let (_path, listener, endpoint) = listen();
        let server = thread::spawn(move || {
            let mut stream = accept(&listener);
            receive(&mut stream);
            write!(
                stream,
                "HTTP/1.1 {status} Response\r\nContent-Length: 0\r\n{fields}\r\n"
            )
            .unwrap();
        });
        let (client, admission, reactor) = client();
        let result = drive(
            &reactor,
            client.metadata_at(
                &endpoint,
                &context(),
                MetadataSelector::Pinned(StrongEtag::parse(b"\"v\"").unwrap()),
                &scope(),
            ),
        );
        assert!(matches!(result, Err(error) if error == expected));
        assert_eq!(admission.used(ResourceClass::Connection), 0);
        server.join().unwrap();
    }
}

#[test]
fn public_operations_reject_wrong_authority_before_io() {
    use crate::{
        model::identity::{MembershipVersion, NodeId},
        peer::{
            requester::PeerClient,
            wire::{FetchMode, Operation as PeerOperation, PeerRequest, VerifiedResponse},
        },
        read::candidates::{CandidatePolicy, CandidateResolution},
        topology::{
            membership::{Member, Membership},
            placement::Placement,
        },
    };
    struct NoPeers;
    impl PeerClient for NoPeers {
        fn request<'a>(
            &'a self,
            _: PeerRequest,
            _: &'a RequestScope,
        ) -> Operation<'a, VerifiedResponse> {
            panic!("rank zero must not probe peers")
        }
    }
    let node = NodeId("node".into());
    let membership = Arc::new(
        Membership::validate(
            MembershipVersion(1),
            vec![Member {
                node: node.clone(),
                shares: std::num::NonZeroU32::new(1).unwrap(),
                peer_endpoint: "127.0.0.1:1234".into(),
                rails: vec![],
                alignment_enabled: false,
            }],
        )
        .unwrap(),
    );
    let policy = CandidatePolicy::new(node, Rc::new(Placement::new(2)), Rc::new(NoPeers));
    let context = context();
    let scope = scope();
    let candidates = policy
        .candidates(membership, &context.object, PageNumber(0))
        .unwrap();
    let resolution = block_on(policy.resolve(
        candidates,
        &context,
        PeerOperation::Metadata {
            object: context.object.clone(),
            selector: MetadataSelector::Fresh,
            mode: FetchMode::Acquire,
        },
        &scope,
    ))
    .unwrap();
    let CandidateResolution::Origin(authority) = resolution else {
        panic!("expected local authority")
    };
    let (client, admission, _) = client();
    let mut wrong = OriginContext {
        object: context.object.clone(),
        metadata: None,
        authorization: None,
    };
    wrong.object.key.0[0] ^= 1;
    assert!(matches!(
        block_on(client.metadata(&authority, &wrong, MetadataSelector::Fresh, &scope)),
        Err(Error::Unauthorized)
    ));
    assert!(matches!(
        block_on(client.bootstrap(&authority, &wrong, &scope)),
        Err(Error::Unauthorized)
    ));
    let page = PageId {
        version: crate::model::identity::ObjectVersion {
            object: context.object.clone(),
            etag: StrongEtag::parse(b"\"v\"").unwrap(),
        },
        number: PageNumber(1),
    };
    assert!(matches!(
        block_on(client.page(&authority, &context, &page, &scope)),
        Err(Error::Unauthorized)
    ));
    assert_eq!(admission.used(ResourceClass::Connection), 0);
    assert_eq!(admission.used(ResourceClass::Plaintext), 0);
}

#[test]
fn real_uds_final_page_consumes_supplied_budget_without_second_charge() {
    let (_path, listener, endpoint) = listen();
    let server = thread::spawn(move || {
        let mut stream = accept(&listener);
        check_request(
            &receive(&mut stream),
            "GET",
            Some(b"\"v\""),
            Some(b"bytes=16777216-33554431"),
            true,
        );
        stream.write_all(b"HTTP/1.1 206 Partial Content\r\nContent-Length: 3\r\nContent-Type: application/octet-stream\r\nContent-Range: bytes 16777216-16777218/16777219\r\nETag: \"v\"\r\nRacer-Expires-At: 1234\r\n\r\nend").unwrap();
    });
    let (client, admission, reactor) = client();
    let context = context();
    let held = admission
        .reserve(
            Some(&context.object.cache),
            ResourceClass::Plaintext,
            3 * PAGE_BYTES as usize,
        )
        .unwrap();
    let supplied = admission
        .reserve(
            Some(&context.object.cache),
            ResourceClass::Plaintext,
            PAGE_BYTES as usize,
        )
        .unwrap();
    assert!(matches!(
        admission.reserve(Some(&context.object.cache), ResourceClass::Plaintext, 1),
        Err(Error::Overloaded)
    ));
    let page = PageId {
        version: crate::model::identity::ObjectVersion {
            object: context.object.clone(),
            etag: StrongEtag::parse(b"\"v\"").unwrap(),
        },
        number: PageNumber(1),
    };
    let result = drive(
        &reactor,
        client.page_reserved_at(&endpoint, &context, &page, supplied, &scope()),
    )
    .unwrap();
    assert_eq!(result.metadata.length, PAGE_BYTES + 3);
    assert_eq!(result.plaintext.bytes().unwrap(), b"end");
    drop(result);
    drop(held);
    client.pool.close();
    assert_eq!(admission.used(ResourceClass::Plaintext), 0);
    assert_eq!(admission.used(ResourceClass::Connection), 0);
    server.join().unwrap();
}

#[test]
fn real_uds_bounds_raw_heads_even_with_a_larger_shared_codec() {
    let (_path, listener, endpoint) = listen();
    let server = thread::spawn(move || {
        let mut stream = accept(&listener);
        receive(&mut stream);
        let response = format!(
            "HTTP/1.1 200 {}\r\nContent-Length: 0\r\nETag: \"v\"\r\nRacer-Expires-At: 0\r\n\r\n",
            "x".repeat(32768)
        );
        // The receiver may close immediately upon reaching its raw-head limit.
        let _ = stream.write_all(response.as_bytes());
    });
    let (mut client, admission, reactor) = client();
    client.io = Rc::new(HttpIo::with_admission(
        reactor.clone(),
        Codec::new(65536, PAGE_BYTES),
        admission.clone(),
    ));
    reactor.init().unwrap();
    let infrastructure_bytes = admission.used(ResourceClass::RequestContext);
    assert!(matches!(
        drive(
            &reactor,
            client.metadata_at(&endpoint, &context(), MetadataSelector::Fresh, &scope())
        ),
        Err(Error::BadGateway)
    ));
    assert_eq!(
        admission.used(ResourceClass::RequestContext),
        infrastructure_bytes
    );
    assert_eq!(admission.used(ResourceClass::Connection), 0);
    server.join().unwrap();
}

#[test]
fn real_uds_cancelled_and_expired_reads_release_owned_resources() {
    for cancel in [false, true] {
        let (_path, listener, endpoint) = listen();
        let scope = RequestScope::new(
            RequestId([4; 16]),
            Instant::now() + Duration::from_millis(250),
        )
        .unwrap();
        let cancellation = scope.cancellation.clone();
        let (release, wait) = std::sync::mpsc::channel();
        let server = thread::spawn(move || {
            let mut stream = accept(&listener);
            receive(&mut stream);
            if cancel {
                cancellation.cancel().unwrap();
            }
            wait.recv_timeout(Duration::from_secs(5)).unwrap();
        });
        let (client, admission, reactor) = client();
        reactor.init().unwrap();
        let infrastructure_bytes = admission.used(ResourceClass::RequestContext);
        let result = drive(&reactor, client.bootstrap_at(&endpoint, &context(), &scope));
        assert!(
            matches!(result, Err(error) if error == if cancel { Error::Cancelled } else { Error::DeadlineExceeded })
        );
        release.send(()).unwrap();
        server.join().unwrap();
        drive(&reactor, reactor.drain()).unwrap();
        client.pool.expire_idle();
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
        assert_eq!(
            admission.used(ResourceClass::RequestContext),
            infrastructure_bytes
        );
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
}

#[test]
fn reserved_page_rejects_foreign_or_mismatched_admission_before_io() {
    let (client, admission, _) = client();
    let (_, foreign, _) = self::client();
    let context = context();
    let page = PageId {
        version: crate::model::identity::ObjectVersion {
            object: context.object.clone(),
            etag: StrongEtag::parse(b"\"v\"").unwrap(),
        },
        number: PageNumber(0),
    };
    let wrong_cache = CacheId("other".into());
    for (owner, cache) in [
        (&foreign, &context.object.cache),
        (&admission, &wrong_cache),
    ] {
        let supplied = owner
            .reserve(Some(cache), ResourceClass::Plaintext, PAGE_BYTES as usize)
            .unwrap();
        let result = block_on(client.page_reserved_at(
            &Endpoint::Unix("does-not-exist".into()),
            &context,
            &page,
            supplied,
            &scope(),
        ));
        assert!(matches!(result, Err(Error::InvalidConfiguration)));
        assert_eq!(owner.used(ResourceClass::Plaintext), 0);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
}

#[test]
fn opaque_request_values_reject_present_empty_and_keep_obs_text() {
    for bytes in [b"".as_slice(), b" x", b"x ", b"x\t", b"x\r\ny", b"x\x7f"] {
        assert!(opaque(bytes).is_err());
    }
    assert!(opaque(&vec![b'x'; 8193]).is_err());
    let head = request(&context(), "HEAD").unwrap();
    check_request(&head, "HEAD", None, None, true);
}
