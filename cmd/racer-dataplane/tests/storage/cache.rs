// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

/// Adapter fixtures construct identities through the cache's public fault path.
#[cfg(test)]
pub(crate) mod adapter_fixture {
    use super::*;
    use crate::{allocator::Allocator, workers::ShardId};
    use std::sync::atomic::{AtomicU64, Ordering};

    static SERIAL: AtomicU64 = AtomicU64::new(0);
    pub(crate) fn cache(namespace: Namespace, shards: usize) -> Cache {
        let path = std::env::temp_dir().join(format!(
            "racer-handlers-{}-{}",
            std::process::id(),
            SERIAL.fetch_add(1, Ordering::Relaxed)
        ));
        let mut slab =
            allocator::Slab::create(&path, shards as u64 * 32 * 1024 * 1024, shards).unwrap();
        std::fs::remove_file(path).unwrap();
        let context = crate::sharding::WorkerContext::test(shards);
        let shards = (0..shards)
            .map(|i| {
                let capability = slab.take_shard(ShardId::at(i)).unwrap();
                let file = capability.file_identity();
                let allocator =
                    Allocator::open(&context, capability, allocator::Config::default()).unwrap();
                crate::sharding::ShardState::test(
                    ShardId::at(i),
                    context.identity().clone(),
                    file,
                    allocator,
                    8192 / shards + usize::from(i < 8192 % shards),
                )
            })
            .collect();
        Cache::new(&context, namespace, shards).unwrap()
    }
    #[derive(Default)]
    struct Seed {
        page: Option<UpstreamRequest>,
    }
    impl Upstream for Seed {
        type Exchange = (UpstreamRequest, Option<Destination>);
        fn start_metadata(
            &mut self,
            request: UpstreamRequest,
            _: Instant,
            _: &mut Ring,
        ) -> Result<Self::Exchange> {
            Ok((request, None))
        }
        fn start(
            &mut self,
            request: UpstreamRequest,
            destination: Destination,
            _: Instant,
            _: &mut Ring,
        ) -> Result<Self::Exchange> {
            if matches!(request, UpstreamRequest::BackendPage(_)) {
                self.page = Some(request.clone());
            }
            Ok((request, Some(destination)))
        }
        fn poll(
            &mut self,
            (request, _destination): Self::Exchange,
            _: &mut Ring,
        ) -> Result<ExchangeProgress<Self::Exchange>> {
            assert!(matches!(request, UpstreamRequest::BackendMetadata(_)));
            Ok(ExchangeProgress::Ready(UpstreamResult::Metadata(
                Record::from_backend(BackendMetadata {
                    len: 3,
                    checksum: Checksum(*blake3::hash(b"abc").as_bytes()),
                    policy: CachePolicy {
                        max_age: Some(60),
                        ..Default::default()
                    },
                }),
            )))
        }
    }
    pub(crate) fn page_request(cache: &mut Cache, ring: &mut Ring, target: &str) -> PageRequest {
        let deadline = || Instant::now() + Duration::from_secs(10);
        let mut seed = Seed::default();
        let mut fault = cache.metadata::<Seed>(target, deadline()).unwrap();
        let meta = loop {
            match cache.poll_metadata(fault, ring, &mut seed).unwrap() {
                Progress::Ready(meta) => break meta,
                Progress::Pending { fault: next, .. } => fault = next,
            }
        };
        let fault = cache.page::<Seed>(&meta, 0, deadline()).unwrap();
        drop(cache.poll_fault(fault, ring, &mut seed).unwrap());
        let Some(UpstreamRequest::BackendPage(page)) = seed.page else {
            panic!("missing page")
        };
        page
    }
}

#[cfg(test)]
pub(crate) mod tests {
    //! Cache contracts, metadata rejection and durable storage integration.
    use super::*;
    use std::sync::atomic::AtomicU64;
    static VERSION: AtomicU64 = AtomicU64::new(0);

    enum TestValue {
        Payload(Buffer),
        Metadata([u8; META_SIZE]),
    }
    impl TestValue {
        fn from_value(value: CachedValue) -> Self {
            match value {
                CachedValue::Metadata(record) => Self::Metadata(record.to_bytes()),
                CachedValue::Buffer(buffer) => Self::Payload(buffer),
                CachedValue::File(_) => unreachable!(),
            }
        }
        fn as_slice(&self) -> &[u8] {
            match self {
                Self::Payload(b) => b.as_slice(),
                Self::Metadata(b) => b,
            }
        }
        fn checksum(&self) -> Option<u64> {
            match self {
                Self::Payload(b) => b.checksum(),
                Self::Metadata(b) => Some(allocator::crc64(b)),
            }
        }
    }

    // Fake upstream revisions label deterministic fixture content, not wire ETags.
    fn fixture_checksum(revision: &str) -> Checksum {
        Checksum(*blake3::hash(revision.as_bytes()).as_bytes())
    }
    mod metadata_invalidation {
        use super::*;

        #[test]
        fn metadata_rejection_tcp() {
            crate::conformance::kernel_child(
                "cache::tests::metadata_invalidation::metadata_rejection_tcp_child",
                "RACER_METADATA_REJECTION_CHILD",
            );
        }

        #[test]
        #[ignore = "run via bounded metadata_rejection_tcp subprocess"]
        fn metadata_rejection_tcp_child() {
            if std::env::var_os("RACER_METADATA_REJECTION_CHILD").is_none() {
                return;
            }
            use crate::http_server::cache_responses::request;
            use crate::{
                handlers::{Backend, Handler, Peer},
                http_server as http,
            };
            use std::{
                io::{Read, Write},
                net::{TcpListener, TcpStream},
                num::NonZeroU32,
                sync::{
                    Arc,
                    atomic::{AtomicBool, AtomicUsize, Ordering},
                },
            };
            let origin = TcpListener::bind("127.0.0.1:0").unwrap();
            origin.set_nonblocking(true).unwrap();
            let url = origin.local_addr().unwrap().to_string();
            let revision = Arc::new(AtomicUsize::new(1));
            let heads = Arc::new(AtomicUsize::new(0));
            let pages = Arc::new(AtomicUsize::new(0));
            let stop = Arc::new(AtomicBool::new(false));
            let (rev, h, p, done) = (revision.clone(), heads.clone(), pages.clone(), stop.clone());
            let backend = thread::spawn(move || {
                while !done.load(Ordering::Acquire) {
                    let mut socket = match origin.accept() {
                        Ok((s, _)) => s,
                        Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                            thread::sleep(Duration::from_millis(1));
                            continue;
                        }
                        Err(e) => panic!("{e}"),
                    };
                    socket
                        .set_read_timeout(Some(Duration::from_secs(5)))
                        .unwrap();
                    let req = request(&mut socket);
                    let late = req.contains(" /late HTTP/1.1\r\n");
                    let len = if late { BUFFER_SIZE + 3 } else { 3 };
                    let v = rev.load(Ordering::Acquire);
                    let tag = if late {
                        crate::conformance::etag(&vec![if v == 1 { b'o' } else { b'N' }; len])
                    } else {
                        crate::conformance::etag(if v == 1 { b"old" } else { b"NEW" })
                    };
                    if req.starts_with("HEAD ") {
                        h.fetch_add(1, Ordering::Relaxed);
                        write!(socket, "HTTP/1.1 200 OK\r\nContent-Length: {len}\r\nETag: {tag}\r\nCache-Control: max-age=86400\r\nConnection: close\r\n\r\n").unwrap();
                    } else {
                        p.fetch_add(1, Ordering::Relaxed);
                        assert!(req.starts_with("GET "));
                        if late {
                            let range = req
                                .lines()
                                .find_map(|l| l.strip_prefix("Range: bytes="))
                                .unwrap();
                            let (start, end) = range.split_once('-').unwrap();
                            let (start, end): (usize, usize) =
                                (start.parse().unwrap(), end.parse().unwrap());
                            if start != 0 && v == 1 {
                                let until = Instant::now() + Duration::from_secs(5);
                                while rev.load(Ordering::Acquire) == 1 {
                                    assert!(
                                        Instant::now() < until,
                                        "client never received initial response"
                                    );
                                    thread::yield_now();
                                }
                                socket.write_all(b"HTTP/1.1 412 Precondition Failed\r\nContent-Length: 0\r\nConnection: close\r\n\r\n").unwrap();
                            } else {
                                write!(socket, "HTTP/1.1 206 Partial Content\r\nContent-Length: {}\r\nContent-Range: bytes {start}-{end}/{len}\r\nETag: {tag}\r\nConnection: close\r\n\r\n", end-start+1).unwrap();
                                socket
                                    .write_all(&vec![
                                        if v == 1 { b'o' } else { b'N' };
                                        end - start + 1
                                    ])
                                    .unwrap();
                            }
                            continue;
                        }
                        if !req.contains(&format!("If-Match: {tag}\r\n"))
                            && !req.contains("/mismatch")
                        {
                            socket.write_all(b"HTTP/1.1 412 Precondition Failed\r\nContent-Length: 0\r\nConnection: close\r\n\r\n").unwrap();
                        } else {
                            write!(socket, "HTTP/1.1 206 Partial Content\r\nContent-Length: 3\r\nContent-Range: bytes 0-2/3\r\nETag: {tag}\r\nConnection: close\r\n\r\n").unwrap();
                            socket
                                .write_all(if v == 1 { b"old" } else { b"NEW" })
                                .unwrap();
                        }
                    }
                }
            });
            // Three independent caches/pools model ingress, intermediate, and owner.
            let mut servers = Vec::new();
            let mut peer: Option<String> = None;
            let authority = crate::tls::tests::Authority::new();
            let identity = |node: u8| {
                crate::tls::PeerIdentity::new(
                    &"01".repeat(32),
                    &format!("{node:02x}").repeat(32),
                    "metadata-test-pod",
                )
                .unwrap()
            };
            for node in 1..=3 {
                let Some(ring) =
                    crate::conformance::kernel_ring(8, crate::uring::Config::default())
                else {
                    panic!("kernel ring required");
                };
                let backend = Backend::new(&url, "test-origin").unwrap();
                let cache = std::rc::Rc::new(std::cell::RefCell::new(
                    crate::cache::adapter_fixture::cache(backend.namespace(), 3),
                ));
                let context = authority.context(&identity(node), false);
                let provider = crate::control::credentials::Provider::for_test(
                    identity(node),
                    context.clone(),
                );
                let make_handler = || {
                    let backend = Backend::new(&url, "test-origin").unwrap();
                    let namespace = backend.namespace();
                    let mut handler = Handler::shared(cache.clone(), backend, namespace);
                    handler.test_authentication(node, &[1, 2, 3], (node > 1).then_some(node - 1));
                    if let Some(url) = &peer {
                        handler.set_peer(Peer::new(url, None).unwrap());
                        let remote = identity(node - 1);
                        handler.set_peer_tls(
                            "metadata-test",
                            provider.clone(),
                            &[(remote.node.clone(), remote)].into(),
                        );
                    }
                    handler
                };
                let handler = make_handler();
                let peer_handler = make_handler();
                let listener = http::Listener::bind(
                    "127.0.0.1:0".parse().unwrap(),
                    NonZeroU32::new(16).unwrap(),
                )
                .unwrap();
                let address = listener.local_addr().unwrap();
                let mut peer_listener = http::Listener::bind(
                    "127.0.0.1:0".parse().unwrap(),
                    NonZeroU32::new(16).unwrap(),
                )
                .unwrap();
                peer_listener.set_tls(context, crate::tls::ExpectedPeer::Universe("01".repeat(32)));
                peer = Some(peer_listener.local_addr().unwrap().to_string());
                servers.push((
                    ring,
                    http::Server::new(listener, handler, http::Config::default()),
                    http::Server::new(peer_listener, peer_handler, http::Config::default()),
                    address,
                ));
            }
            let addresses: Vec<_> = servers.iter().map(|(_, _, _, a)| *a).collect();
            let client = thread::spawn(move || {
                let check = |node: usize,
                             method: &str,
                             target: &str,
                             status: u16,
                             body: &[u8],
                             tag: Option<&str>| {
                    let mut socket = TcpStream::connect(addresses[node]).unwrap();
                    socket
                        .set_read_timeout(Some(Duration::from_secs(5)))
                        .unwrap();
                    write!(
                        socket,
                        "{method} {target} HTTP/1.1\r\nHost: cache\r\nConnection: close\r\n\r\n"
                    )
                    .unwrap();
                    let header = request(&mut socket);
                    assert!(
                        header.starts_with(&format!("HTTP/1.1 {status} ")),
                        "{header}"
                    );
                    if let Some(tag) = tag {
                        let tag = if target == "/late" {
                            crate::conformance::etag(&vec![
                                if tag == "\"v1\"" { b'o' } else { b'N' };
                                BUFFER_SIZE + 3
                            ])
                        } else {
                            crate::conformance::etag(if tag == "\"v1\"" { b"old" } else { b"NEW" })
                        };
                        assert!(header.contains(&format!("ETag: {tag}\r\n")), "{header}");
                    }
                    let mut bytes = Vec::new();
                    socket.read_to_end(&mut bytes).unwrap();
                    assert_eq!(bytes, body);
                };
                for target in ["/status", "/mismatch"] {
                    revision.store(1, Ordering::Release);
                    let before = heads.load(Ordering::Acquire);
                    check(2, "HEAD", target, 200, b"", Some("\"v1\""));
                    assert_eq!(heads.load(Ordering::Acquire), before + 1);
                    revision.store(2, Ordering::Release);
                    let before_pages = pages.load(Ordering::Acquire);
                    check(2, "GET", target, 412, b"", None);
                    assert_eq!(
                        pages.load(Ordering::Acquire),
                        before_pages + 1,
                        "no transparent retry"
                    );
                    assert_eq!(heads.load(Ordering::Acquire), before + 1);
                    check(2, "HEAD", target, 200, b"", Some("\"v2\""));
                    assert_eq!(
                        heads.load(Ordering::Acquire),
                        before + 2,
                        "long TTL must not prevent refresh"
                    );
                    check(2, "GET", target, 200, b"NEW", Some("\"v2\""));
                    for node in [0, 1] {
                        check(node, "HEAD", target, 200, b"", Some("\"v2\""));
                    }
                    assert!(heads.load(Ordering::Acquire) <= before + 3);
                }
                revision.store(1, Ordering::Release);
                check(2, "HEAD", "/late", 200, b"", Some("\"v1\""));
                let mut socket = TcpStream::connect(addresses[2]).unwrap();
                socket
                    .set_read_timeout(Some(Duration::from_secs(5)))
                    .unwrap();
                socket
                    .write_all(b"GET /late HTTP/1.1\r\nHost: cache\r\nConnection: close\r\n\r\n")
                    .unwrap();
                let header = request(&mut socket);
                assert!(header.starts_with("HTTP/1.1 200 "));
                revision.store(2, Ordering::Release);
                let mut partial = Vec::new();
                socket.read_to_end(&mut partial).unwrap();
                assert!(
                    partial.len() < BUFFER_SIZE + 3,
                    "late failure must truncate the response"
                );
                assert!(
                    partial.iter().all(|b| *b == b'o'),
                    "no mixed-version response"
                );
                check(2, "HEAD", "/late", 200, b"", Some("\"v2\""));
                check(
                    2,
                    "GET",
                    "/late",
                    200,
                    &vec![b'N'; BUFFER_SIZE + 3],
                    Some("\"v2\""),
                );
            });
            let end = Instant::now() + Duration::from_secs(25);
            while !client.is_finished() {
                assert!(Instant::now() < end);
                for (ring, server, peer_server, _) in &mut servers {
                    ring.progress().unwrap();
                    server.handler_mut().poll_background(ring, 32).unwrap();
                    server.poll(ring, 32).unwrap();
                    peer_server.handler_mut().poll_background(ring, 32).unwrap();
                    peer_server.poll(ring, 32).unwrap();
                }
                thread::yield_now();
            }
            client.join().unwrap();
            stop.store(true, Ordering::Release);
            backend.join().unwrap();
            for (ring, server, peer_server, _) in &mut servers {
                server.shutdown(ring).unwrap();
                server.handler_mut().shutdown(ring).unwrap();
                peer_server.shutdown(ring).unwrap();
                peer_server.handler_mut().shutdown(ring).unwrap();
                ring.shutdown().unwrap();
            }
        }
    }
    use crate::{allocator::Allocator, buffers, uring, workers::ShardId};
    use std::{
        collections::VecDeque,
        process::{Command, Stdio},
        thread,
    };

    pub(crate) fn cache(shards: usize) -> Cache {
        let path = std::env::temp_dir().join(format!(
            "racer-cache-{}-{}",
            std::process::id(),
            VERSION.fetch_add(1, Ordering::Relaxed)
        ));
        let mut slab =
            allocator::Slab::create(&path, shards as u64 * 32 * 1024 * 1024, shards).unwrap();
        std::fs::remove_file(path).unwrap();
        cache_from_slab(&mut slab, shards, allocator::Config::default())
    }
    pub(crate) fn cache_from_slab(
        slab: &mut allocator::Slab,
        shards: usize,
        config: allocator::Config,
    ) -> Cache {
        let context = WorkerContext::test(shards);
        let shards = (0..shards)
            .map(|i| {
                let capability = slab.take_shard(ShardId::at(i)).unwrap();
                let file = capability.file_identity();
                let allocator = Allocator::open(&context, capability, config).unwrap();
                ShardState::test(
                    ShardId::at(i),
                    context.identity().clone(),
                    file,
                    allocator,
                    8192 / shards + usize::from(i < 8192 % shards),
                )
            })
            .collect();
        Cache::new(&context, Namespace::new("127.0.0.1:1").unwrap(), shards).unwrap()
    }

    fn facts(len: u64, tag: &str, ttl: u64) -> BackendMetadata {
        BackendMetadata {
            len,
            checksum: fixture_checksum(tag),
            policy: CachePolicy {
                max_age: Some(ttl),
                ..CachePolicy::default()
            },
        }
    }

    #[test]
    fn local_replica_routing_and_admission_budget() {
        let mut cache = cache(3);
        for n in 0..100 {
            let target = format!("/local-{n}");
            let fault = cache.metadata::<Fake>(&target, deadline()).unwrap();
            let expected = (u64::from_le_bytes(fault.0.key[..8].try_into().unwrap()) % 3) as usize;
            assert_eq!(fault.0.shard.0, expected);
            assert_eq!(
                cache
                    .metadata::<Fake>(&target, deadline())
                    .unwrap()
                    .0
                    .shard
                    .0,
                expected
            );
        }
    }
    fn metadata(cache: &Cache, target: &str, len: u64, expires: u64) -> Metadata {
        let mut record = Record::from_backend(facts(len, "\"v1\"", 60));
        record.expires = expires;
        Metadata {
            object: Object::new(&cache.namespace, target).unwrap(),
            record: Rc::new(record),
            owner: cache.owner.clone(),
            context: Context::new(Namespace(cache.namespace)),
        }
    }
    fn deadline() -> Instant {
        Instant::now() + Duration::from_secs(10)
    }

    #[test]
    fn interleaved_contexts_isolate_identity_and_capture_crypto_generation() {
        let Some(mut ring) = crate::control::tests::ring() else {
            return;
        };
        let pool = crate::crypto::Pool::test_pool(ring.pool());
        let (first, first_source) = pool.attach_local(ring.pool(), ring.wake_handle()).unwrap();
        let (second, second_source) = pool.attach_local(ring.pool(), ring.wake_handle()).unwrap();
        let first = Rc::new(std::cell::RefCell::new(first));
        let second = Rc::new(std::cell::RefCell::new(second));
        let backend = Namespace::new("shared-origin").unwrap();
        let a = Context::new(Namespace::volume(b"universe", "a", 1, backend))
            .with_crypto(Some(first.clone()));
        let b = Context::new(Namespace::volume(b"universe", "b", 1, backend))
            .with_crypto(Some(second.clone()));
        let replacement = a.clone().with_crypto(Some(second.clone()));
        let mut cache = cache(1);
        let mut upstream = Fake::default();
        let a_fault = cache.metadata_in::<Fake>(&a, "/same", deadline()).unwrap();
        let b_fault = cache.metadata_in::<Fake>(&b, "/same", deadline()).unwrap();
        let replacement_fault = cache
            .metadata_in::<Fake>(&replacement, "/same", deadline())
            .unwrap();
        assert_ne!(a_fault.key(), b_fault.key());
        assert_eq!(
            a_fault.key(),
            replacement_fault.key(),
            "crypto workers are not value identities"
        );
        assert!(Rc::ptr_eq(a_fault.0.crypto.as_ref().unwrap(), &first));
        assert!(Rc::ptr_eq(
            replacement_fault.0.crypto.as_ref().unwrap(),
            &second
        ));
        let end = deadline();
        let resolve = |cache: &mut Cache, ring: &mut Ring, upstream: &mut Fake, mut fault| loop {
            assert!(Instant::now() < end);
            match cache.poll_metadata(fault, ring, upstream).unwrap() {
                Progress::Ready(meta) => break meta,
                Progress::Pending { fault: next, .. } => fault = next,
            }
        };
        let b_meta = resolve(&mut cache, &mut ring, &mut upstream, b_fault);
        let a_meta = resolve(&mut cache, &mut ring, &mut upstream, a_fault);
        let replacement_meta = resolve(&mut cache, &mut ring, &mut upstream, replacement_fault);
        assert_eq!(
            upstream.starts.len(),
            2,
            "same namespace shares resolved metadata"
        );
        let a_page = cache.page::<Fake>(&a_meta, 0, deadline()).unwrap();
        let b_page = cache.page::<Fake>(&b_meta, 0, deadline()).unwrap();
        let replacement_page = cache
            .page::<Fake>(&replacement_meta, 0, deadline())
            .unwrap();
        assert_ne!(a_page.key(), b_page.key());
        assert_eq!(a_page.key(), replacement_page.key());
        assert!(Rc::ptr_eq(a_page.crypto.as_ref().unwrap(), &first));
        assert!(Rc::ptr_eq(b_page.crypto.as_ref().unwrap(), &second));
        assert!(Rc::ptr_eq(
            replacement_page.crypto.as_ref().unwrap(),
            &second
        ));
        assert!(
            cache
                .peer_fault_in::<Fake>(
                    &b,
                    PeerDescriptor::metadata("/same")
                        .with_expected(a_meta.object.metadata_key().0, META_SIZE),
                    deadline()
                )
                .is_err()
        );
        assert!(
            cache
                .metadata_in::<Fake>(&a, "invalid", deadline())
                .is_err()
        );
        drop((
            a_page,
            b_page,
            replacement_page,
            a_meta,
            b_meta,
            replacement_meta,
        ));
        cache.shutdown(&mut ring).unwrap();
        drop((
            a,
            b,
            replacement,
            first,
            second,
            first_source,
            second_source,
        ));
        pool.shutdown().unwrap();
    }

    fn scoped_fake() -> Fake {
        Fake {
            scope: Some(crate::buffers::NetworkFlightKey {
                value: [0; 32],
                routing: [7; 32],
                version: 2,
                destination: 3,
                dependency: crate::buffers::NetworkDependency::Canonical { slot: 1 },
            }),
            ..Fake::default()
        }
    }

    #[test]
    fn scoped_metadata_page_cancellation_and_retry_keep_candidate() {
        let Some(mut ring) = crate::control::tests::ring() else {
            return;
        };
        let mut cache = cache(1);
        for page in [false, true] {
            let target = if page { "/scoped-page" } else { "/scoped-meta" };
            let meta = metadata(&cache, target, 3, now() + 60);
            let mut upstream = scoped_fake();
            upstream.replies.push_back(Reply::Hold);
            let make = |cache: &mut Cache| {
                if page {
                    cache.page::<Fake>(&meta, 0, deadline()).unwrap()
                } else {
                    cache.metadata::<Fake>(target, deadline()).unwrap().0
                }
            };
            let producer = make(&mut cache);
            let consumer = make(&mut cache);
            let (producer, _) = pending_fault(&mut cache, &mut ring, &mut upstream, producer);
            let (consumer, _) = pending_fault(&mut cache, &mut ring, &mut upstream, consumer);
            assert_eq!(upstream.starts.len(), 1);
            drop(producer);
            let (buffer, _) = resolve_checked(&mut cache, &mut ring, &mut upstream, consumer);
            assert_eq!(
                upstream.starts.len(),
                2,
                "survivor takes producer ownership"
            );
            drop(buffer);
            // An incompatible candidate/generation still shares validated values.
            upstream.scope.as_mut().unwrap().routing[0] += 1;
            upstream.scope.as_mut().unwrap().destination += 1;
            let fault = make(&mut cache);
            let _ = resolve_checked(&mut cache, &mut ring, &mut upstream, fault);
            assert_eq!(upstream.starts.len(), 2);
        }
        assert_eq!(&cache.metrics().values()[6..14], &[1, 0, 1, 1, 0, 1, 1, 1]);
        let mut upstream = scoped_fake();
        upstream.peer = true;
        upstream.replies.extend([Reply::RetryPeer, Reply::Good]);
        let producer = cache
            .metadata::<Fake>("/scoped-retry", deadline())
            .unwrap()
            .0;
        let consumer = cache
            .metadata::<Fake>("/scoped-retry", deadline())
            .unwrap()
            .0;
        let (producer, _) = pending_fault(&mut cache, &mut ring, &mut upstream, producer);
        let (consumer, _) = pending_fault(&mut cache, &mut ring, &mut upstream, consumer);
        let _ = resolve_checked(&mut cache, &mut ring, &mut upstream, producer);
        let _ = resolve_checked(&mut cache, &mut ring, &mut upstream, consumer);
        assert_eq!(upstream.starts.len(), 1);
        assert_eq!(upstream.resumes.len(), 1);
        assert_eq!(&cache.metrics().values()[6..14], &[1, 0, 2, 2, 0, 1, 1, 1]);
        assert!(
            upstream.held.is_none(),
            "metadata retry owns no DMA destination"
        );
        drop(upstream);
        cache.shutdown(&mut ring).unwrap();
    }

    fn resolve_checked(
        cache: &mut Cache,
        ring: &mut Ring,
        upstream: &mut Fake,
        fault: Fault<Fake>,
    ) -> (TestValue, bool) {
        resolve_observed(cache, ring, upstream, fault, |_, _| {})
    }
    fn resolve_observed(
        cache: &mut Cache,
        ring: &mut Ring,
        upstream: &mut Fake,
        mut fault: Fault<Fake>,
        mut observe: impl FnMut(&Fault<Fake>, &Fake),
    ) -> (TestValue, bool) {
        fault.buffered = true;
        let mut disk = false;
        loop {
            observe(&fault, upstream);
            disk |= matches!(fault.state, Loading::Materializing(..));
            ring.progress().unwrap();
            cache.poll(ring, 16).unwrap();
            match cache.poll_value(fault, ring, upstream).unwrap() {
                Progress::Ready(value) => return (TestValue::from_value(value), disk),
                Progress::Pending { fault: next, .. } => fault = next,
            }
            thread::yield_now();
        }
    }
    include!(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/tests/storage/cache_persistence.rs"
    ));
}
