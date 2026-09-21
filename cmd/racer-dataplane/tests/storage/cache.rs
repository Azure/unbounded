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
        fn typed_cross_worker_admission_and_superseded_completion() {
            let world = crate::simulation::World::new(614);
            let _scope = world.enter();
            let pool = buffers::io_test_pool(4);
            let mut rings = [
                Ring::http_test_ring(pool.clone(), uring::Config::default()).unwrap(),
                Ring::http_test_ring(pool.test_other_worker(), uring::Config::default()).unwrap(),
            ];
            let mut caches = std::array::from_fn::<_, 2, _>(|_| {
                let mut slab = allocator::Slab::simulated(
                    crate::simulation::Disk::new(32 * 1024 * 1024),
                    32 * 1024 * 1024,
                    1,
                    true,
                )
                .unwrap();
                cache_from_slab(&mut slab, 1, allocator::Config::default())
            });
            let pinned: Vec<_> = (0..4).map(|_| pool.private_fill().unwrap()).collect();
            let mut upstream = [scoped_fake(), scoped_fake()];
            let a = caches[0].metadata("/cross-worker", deadline()).unwrap();
            let b = caches[1].metadata("/cross-worker", deadline()).unwrap();
            let key = *a.key();
            let (a, _) = pending(
                caches[0]
                    .poll_metadata(a, &mut rings[0], &mut upstream[0])
                    .unwrap(),
            );
            let (b, _) = pending(
                caches[1]
                    .poll_metadata(b, &mut rings[1], &mut upstream[1])
                    .unwrap(),
            );
            let Progress::Ready(a) = caches[0]
                .poll_metadata(a, &mut rings[0], &mut upstream[0])
                .unwrap()
            else {
                panic!()
            };
            let Progress::Ready(b) = caches[1]
                .poll_metadata(b, &mut rings[1], &mut upstream[1])
                .unwrap()
            else {
                panic!()
            };
            assert_eq!(*a.record, *b.record);
            assert!(upstream[1].starts.is_empty());
            for cache in &mut caches {
                assert_eq!(
                    cache.shards[0].allocator.lookup_metadata(&key, now()),
                    Some(*a.record)
                );
            }

            // Different candidates can resolve concurrently. A late old completion may
            // satisfy its snapshot, but cannot resurrect after the newer entry expires.
            let cache = &mut caches[0];
            let ring = &mut rings[0];
            let mut old = scoped_fake();
            let mut new = scoped_fake();
            new.scope.as_mut().unwrap().destination += 1;
            let record = Record {
                checksum: fixture_checksum("new"),
                len: 17,
                expires: now() + 1,
            };
            new.replies.push_back(Reply::Checked(
                allocator::crc64(&record.to_bytes()),
                record.to_bytes().to_vec(),
            ));
            let a = cache.metadata("/version-race", deadline()).unwrap();
            let b = cache.metadata("/version-race", deadline()).unwrap();
            let key = *a.key();
            let (a, _) = pending(cache.poll_metadata(a, ring, &mut old).unwrap());
            let (b, _) = pending(cache.poll_metadata(b, ring, &mut new).unwrap());
            let Progress::Ready(snapshot) = cache.poll_metadata(b, ring, &mut new).unwrap() else {
                panic!()
            };
            assert_eq!(*snapshot.record, record);
            world.advance(Duration::from_secs(2));
            assert!(
                cache.shards[0]
                    .allocator
                    .lookup_metadata(&key, now())
                    .is_none()
            );
            assert!(matches!(
                cache.poll_metadata(a, ring, &mut old).unwrap(),
                Progress::Ready(_)
            ));
            assert!(
                cache.shards[0]
                    .allocator
                    .lookup_metadata(&key, now())
                    .is_none(),
                "superseded completion resurrected old version"
            );
            assert_eq!(snapshot.checksum(), record.checksum);
            drop(pinned);
            for (cache, ring) in caches.iter_mut().zip(&mut rings) {
                cache.shutdown(ring).unwrap();
                ring.shutdown().unwrap();
            }
            drop((caches, rings, pool, upstream));
            world.run_tasks();
            world.assert_clean();
        }

        #[test]
        fn resident_metadata_hit_needs_no_pool_slot_or_upstream() {
            let world = crate::simulation::World::new(610);
            let _scope = world.enter();
            let mut slab = allocator::Slab::simulated(
                crate::simulation::Disk::new(32 * 1024 * 1024),
                32 * 1024 * 1024,
                1,
                true,
            )
            .unwrap();
            let mut cache = cache_from_slab(&mut slab, 1, allocator::Config::default());
            let mut ring = crate::control::tests::ring().unwrap();
            let object = Object::new(&cache.namespace, "/resident").unwrap();
            let record = Record::from_backend(facts(3, "v1", 60));
            cache.shards[0]
                .allocator
                .insert_metadata(object.metadata_key().0, record, now())
                .unwrap();
            cache.shutdown(&mut ring).unwrap();
            let mut held = Vec::new();
            while let Ok(fill) = ring.pool().private_fill() {
                held.push(fill);
            }
            let mut upstream = Fake::default();
            let fault = cache.metadata::<Fake>("/resident", deadline()).unwrap();
            let Progress::Ready(meta) = cache
                .poll_metadata(fault, &mut ring, &mut upstream)
                .unwrap()
            else {
                panic!("resident metadata waited for a pool slot");
            };
            assert_eq!(*meta.record, record);
            assert!(upstream.starts.is_empty());
            drop((meta, held, cache, ring, slab));
            world.assert_clean();
        }

        fn drive(
            cache: &mut Cache,
            ring: &mut Ring,
            upstream: &mut Fake,
            mut fault: Fault<Fake>,
        ) -> Result<TestValue> {
            fault.buffered = true;
            for _ in 0..2000 {
                ring.progress().unwrap();
                cache.poll(ring, 16).unwrap();
                match cache.poll_value(fault, ring, upstream)? {
                    Progress::Ready(value) => return Ok(TestValue::from_value(value)),
                    Progress::Pending { fault: next, .. } => fault = next,
                }
            }
            panic!("bounded fault did not finish")
        }

        fn seed(cache: &mut Cache, ring: &mut Ring, target: &str, tag: &str) -> Metadata {
            let object = Object::new(&cache.namespace, target).unwrap();
            let key = object.metadata_key().0;
            let record = Record::from_backend(facts(3, tag, 86400));
            let _ = ring;
            let shard = local_replica(&key, cache.shards.len()).0;
            cache.shards[shard]
                .allocator
                .insert_metadata(key, record, now())
                .unwrap();
            Metadata {
                object,
                record: Rc::new(record),
                owner: cache.owner.clone(),
            }
        }

        #[test]
        fn metadata_rejection_disk_memory_flights_and_newer_race() {
            for disk in [false, true] {
                for changed_tag in [false, true] {
                    let world = crate::simulation::World::new(611);
                    let _scope = world.enter();
                    let mut slab = allocator::Slab::simulated(
                        crate::simulation::Disk::new(96 * 1024 * 1024),
                        96 * 1024 * 1024,
                        3,
                        true,
                    )
                    .unwrap();
                    let mut cache = cache_from_slab(&mut slab, 3, allocator::Config::default());
                    let mut ring = crate::control::tests::ring().unwrap();
                    let old = seed(&mut cache, &mut ring, "/reject", "\"v1\"");
                    let key = old.object.metadata_key().0;
                    let shard = local_replica(&key, cache.shards.len()).0;
                    if disk {
                        cache.shutdown(&mut ring).unwrap();
                        assert!(
                            cache.shards[shard]
                                .allocator
                                .lookup_metadata(&key, now())
                                .is_some()
                        );
                    }
                    let mut upstream = scoped_fake();
                    upstream.replies.push_back(if changed_tag {
                        Reply::ChangedTag
                    } else {
                        Reply::Error(Error::Precondition)
                    });
                    let first = cache.page(&old, 0, deadline()).unwrap();
                    let second = cache.page(&old, 0, deadline()).unwrap();
                    let (first, _) = pending_fault(&mut cache, &mut ring, &mut upstream, first);
                    let (second, _) = pending_fault(&mut cache, &mut ring, &mut upstream, second);
                    // A metadata consumer already exists when the page version fails.
                    let mut late = cache.metadata::<Fake>("/reject", deadline()).unwrap().0;
                    late.route = Route::Backend;
                    late.state = Loading::MetadataExchange(
                        upstream
                            .start_metadata(
                                UpstreamRequest::BackendMetadata(MetadataRequest {
                                    object: late.spec.object().clone(),
                                }),
                                late.deadline(),
                                &mut ring,
                            )
                            .unwrap(),
                    );
                    assert!(matches!(
                        drive(&mut cache, &mut ring, &mut upstream, first)
                            .err()
                            .unwrap()
                            .root(),
                        Error::Precondition
                    ));
                    assert!(
                        cache.shards[shard]
                            .allocator
                            .lookup_metadata(&key, now())
                            .is_none()
                    );
                    // The late flight receives valid, long-TTL bytes for the rejected
                    // version. It must not publish or re-admit them.
                    assert!(matches!(
                        drive(&mut cache, &mut ring, &mut upstream, late)
                            .err()
                            .unwrap()
                            .root(),
                        Error::Precondition
                    ));
                    let new = seed(&mut cache, &mut ring, "/reject", "\"v2\"");
                    // The unpolled shared page failure arrives after replacement.
                    assert!(matches!(
                        drive(&mut cache, &mut ring, &mut upstream, second)
                            .err()
                            .unwrap()
                            .root(),
                        Error::Precondition
                    ));
                    let next = cache.metadata::<Fake>("/reject", deadline()).unwrap().0;
                    let bytes = drive(&mut cache, &mut ring, &mut upstream, next).unwrap();
                    assert_eq!(
                        Record::decode(bytes.as_slice()).unwrap().checksum.0,
                        *new.version()
                    );
                    assert_eq!(
                        upstream.starts.len(),
                        2,
                        "one page attempt and one late metadata flight"
                    );
                    assert!(
                        cache.shards[shard]
                            .allocator
                            .lookup_metadata(&key, now())
                            .is_some()
                    );
                    drop(bytes);
                    cache.shutdown(&mut ring).unwrap();
                    drop((cache, ring, upstream, slab));
                    world.run_tasks();
                    world.assert_clean();
                }
            }
        }

        #[test]
        fn metadata_rejection_inline_preserves_racing_replacement() {
            let world = crate::simulation::World::new(612);
            let _scope = world.enter();
            let mut slab = allocator::Slab::simulated(
                crate::simulation::Disk::new(32 * 1024 * 1024),
                32 * 1024 * 1024,
                1,
                true,
            )
            .unwrap();
            let mut cache = cache_from_slab(&mut slab, 1, allocator::Config::default());
            let mut ring = crate::control::tests::ring().unwrap();
            let old = seed(&mut cache, &mut ring, "/disk-race", "\"v1\"");
            cache.shutdown(&mut ring).unwrap();
            let mut upstream = scoped_fake();
            upstream
                .replies
                .push_back(Reply::Error(Error::Precondition));
            let page = cache.page(&old, 0, deadline()).unwrap();
            let (page, _) = pending_fault(&mut cache, &mut ring, &mut upstream, page);
            let new = seed(&mut cache, &mut ring, "/disk-race", "\"v2\"");
            assert!(matches!(
                drive(&mut cache, &mut ring, &mut upstream, page)
                    .err()
                    .unwrap()
                    .root(),
                Error::Precondition
            ));
            let fault = cache.metadata::<Fake>("/disk-race", deadline()).unwrap().0;
            let bytes = drive(&mut cache, &mut ring, &mut upstream, fault).unwrap();
            assert_eq!(
                Record::decode(bytes.as_slice()).unwrap().checksum.0,
                *new.version()
            );
            cache.shutdown(&mut ring).unwrap();
            drop(bytes);
            // The disk replacement survives too, after all pending writes/checkpoints.
            let fault = cache.metadata::<Fake>("/disk-race", deadline()).unwrap().0;
            let bytes = drive(&mut cache, &mut ring, &mut upstream, fault).unwrap();
            assert_eq!(
                Record::decode(bytes.as_slice()).unwrap().checksum.0,
                *new.version()
            );
            assert_eq!(upstream.starts.len(), 1);
            drop(bytes);
            cache.shutdown(&mut ring).unwrap();
            drop((cache, ring, upstream, slab));
            world.run_tasks();
            world.assert_clean();
        }

        #[test]
        fn metadata_rejection_completed_network_joiner_cannot_readmit() {
            let world = crate::simulation::World::new(613);
            let _scope = world.enter();
            let mut slab = allocator::Slab::simulated(
                crate::simulation::Disk::new(32 * 1024 * 1024),
                32 * 1024 * 1024,
                1,
                true,
            )
            .unwrap();
            let mut cache = cache_from_slab(&mut slab, 1, allocator::Config::default());
            let mut ring = crate::control::tests::ring().unwrap();
            let mut upstream = scoped_fake();
            let producer = cache.metadata::<Fake>("/joined", deadline()).unwrap().0;
            let joiner = cache.metadata::<Fake>("/joined", deadline()).unwrap().0;
            let (producer, _) = pending_fault(&mut cache, &mut ring, &mut upstream, producer);
            let (joiner, _) = pending_fault(&mut cache, &mut ring, &mut upstream, joiner);
            let bytes = drive(&mut cache, &mut ring, &mut upstream, producer).unwrap();
            let meta = Metadata {
                object: Object::new(&cache.namespace, "/joined").unwrap(),
                record: Rc::new(Record::decode(bytes.as_slice()).unwrap()),
                owner: cache.owner.clone(),
            };
            upstream
                .replies
                .push_back(Reply::Error(Error::Precondition));
            let page = cache.page(&meta, 0, deadline()).unwrap();
            assert!(matches!(
                drive(&mut cache, &mut ring, &mut upstream, page)
                    .err()
                    .unwrap()
                    .root(),
                Error::Precondition
            ));
            assert!(matches!(
                drive(&mut cache, &mut ring, &mut upstream, joiner)
                    .err()
                    .unwrap()
                    .root(),
                Error::Precondition
            ));
            let key = meta.object.metadata_key().0;
            assert!(
                cache.shards[0]
                    .allocator
                    .lookup_metadata(&key, now())
                    .is_none()
            );
            assert_eq!(upstream.starts.len(), 2);
            drop(bytes);
            cache.shutdown(&mut ring).unwrap();
            drop((cache, ring, upstream, slab));
            world.run_tasks();
            world.assert_clean();
        }

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
            for node in 1..=3 {
                let Some(ring) =
                    crate::conformance::kernel_ring(8, crate::uring::Config::default())
                else {
                    panic!("kernel ring required");
                };
                let backend = Backend::new(&url, "test-origin").unwrap();
                let mut handler = Handler::new(
                    crate::cache::adapter_fixture::cache(backend.namespace(), 3),
                    backend,
                );
                handler.test_authentication(node, &[1, 2, 3], (node > 1).then_some(node - 1));
                if let Some(url) = &peer {
                    handler.set_peer(Peer::new(url, None).unwrap());
                }
                let listener = http::Listener::bind(
                    "127.0.0.1:0".parse().unwrap(),
                    NonZeroU32::new(16).unwrap(),
                )
                .unwrap();
                let address = listener.local_addr().unwrap();
                peer = Some(address.to_string());
                servers.push((
                    ring,
                    http::Server::new(listener, handler, http::Config::default()),
                    address,
                ));
            }
            let addresses: Vec<_> = servers.iter().map(|(_, _, a)| *a).collect();
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
                for (ring, server, _) in &mut servers {
                    ring.progress().unwrap();
                    server.handler_mut().poll_background(ring, 32).unwrap();
                    server.poll(ring, 32).unwrap();
                }
                thread::yield_now();
            }
            client.join().unwrap();
            stop.store(true, Ordering::Release);
            backend.join().unwrap();
            for (ring, server, _) in &mut servers {
                server.shutdown(ring).unwrap();
                server.handler_mut().shutdown(ring).unwrap();
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
    pub(crate) fn durable_ready(cache: &mut Cache, target: &str) -> bool {
        let object = Object::new(&cache.namespace, target).unwrap();
        let metadata = Spec::Metadata(object.clone());
        let mut bytes = vec![0; crate::simulation::corpus::length(target)];
        crate::simulation::corpus::origin_bytes(target, 0, &mut bytes);
        let record = Rc::new(Record {
            checksum: crate::metadata::Checksum::from_etag(&crate::conformance::etag(&bytes))
                .unwrap(),
            len: bytes.len() as u64,
            expires: now() + 60,
        });
        let page = Spec::Page(record.page(&object, 0).unwrap());
        [metadata.key(), page.key()].iter().all(|key| {
            let shard = local_replica(key, cache.shards.len()).0;
            cache.shards[shard].allocator.is_idle()
                && if *key == metadata.key() {
                    cache.shards[shard]
                        .allocator
                        .lookup_metadata(key, now())
                        .is_some()
                } else {
                    cache.shards[shard].allocator.lookup(key, now()).is_some()
                }
        })
    }
    pub(crate) fn idle(cache: &Cache) -> bool {
        cache.shards.iter().all(|shard| shard.allocator.is_idle())
    }
    pub(crate) fn pending_admission(cache: &mut Cache, pool: &buffers::WorkerPool) {
        let meta = metadata(cache, "/shutdown-admission", 3, 0);
        let fault = cache.page::<Fake>(&meta, 0, deadline()).unwrap();
        let bytes = buffer(pool, fault.key, b"abc");
        cache.admit(&fault, &bytes, PageCrc::Compute).unwrap();
        assert!(!idle(cache));
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
    fn oversized_peer_input_never_selects_backend() {
        let world = crate::simulation::World::new(12);
        let _scope = world.enter();
        let mut slab = allocator::Slab::simulated(
            crate::simulation::Disk::new(32 * 1024 * 1024),
            32 * 1024 * 1024,
            1,
            true,
        )
        .unwrap();
        let mut cache = cache_from_slab(&mut slab, 1, allocator::Config::default());
        let mut ring = crate::control::tests::ring().unwrap();
        let target = format!("/{}", "x".repeat(MAX_PEER_INPUT));
        let meta = metadata(&cache, &target, 3, u64::MAX);
        let mut upstream = Fake::peer([Reply::Hold, Reply::Hold]);
        let metadata_fault = cache
            .metadata::<Fake>(&target, world.now() + Duration::from_secs(10))
            .unwrap()
            .0;
        let page_fault = cache
            .page::<Fake>(&meta, 0, world.now() + Duration::from_secs(10))
            .unwrap();
        let (metadata_fault, _) =
            pending_fault(&mut cache, &mut ring, &mut upstream, metadata_fault);
        let (page_fault, _) = pending_fault(&mut cache, &mut ring, &mut upstream, page_fault);
        assert_eq!(
            upstream.starts.iter().map(|s| s.0).collect::<Vec<_>>(),
            [RequestKind::PeerMetadata, RequestKind::PeerPage]
        );
        assert_eq!(upstream.advances, 0);
        drop((metadata_fault, page_fault));
        cache.shutdown(&mut ring).unwrap();
        ring.shutdown().unwrap();
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
        }
    }
    fn deadline() -> Instant {
        Instant::now() + Duration::from_secs(10)
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
    fn generated_consumer_lifecycle() {
        for seed in 0..24 {
            let world = crate::simulation::World::new(seed);
            let _scope = world.enter();
            let mut random = crate::simulation::corpus::Random(seed);
            let mut slab = allocator::Slab::simulated(
                crate::simulation::Disk::new(32 * 1024 * 1024),
                32 * 1024 * 1024,
                1,
                true,
            )
            .unwrap();
            let mut cache = cache_from_slab(&mut slab, 1, allocator::Config::default());
            let mut ring = crate::control::tests::ring().unwrap();
            let mut upstream = scoped_fake();
            let failed = seed % 3 == 0;
            let page = seed % 2 == 0;
            upstream.replies.push_back(if failed {
                Reply::Error(Error::Precondition)
            } else {
                Reply::Hold
            });
            let short = world.now() + Duration::from_millis(100);
            let long = world.now() + Duration::from_secs(10);
            let count = 2 + random.index(5);
            let retiring = random.index(count);
            let meta = metadata(&cache, "/handoff", 3, now() + 60);
            let mut consumers = Vec::new();
            for i in 0..count {
                let end = if i == retiring { short } else { long };
                let fault = if page {
                    cache.page::<Fake>(&meta, 0, end).unwrap()
                } else {
                    cache.metadata::<Fake>("/handoff", end).unwrap().0
                };
                let (fault, work) = pending_fault(&mut cache, &mut ring, &mut upstream, fault);
                if i > 0 {
                    assert!(!work.runnable);
                    assert_eq!(work.deadline, Some(end));
                }
                consumers.push(fault);
            }
            assert_eq!(upstream.starts.len(), 1);
            if !failed {
                let retired = consumers.remove(retiring);
                if seed % 3 == 1 {
                    world.advance(short.saturating_duration_since(world.now()));
                    assert!(matches!(
                        cache.poll_fault(retired, &mut ring, &mut upstream),
                        Err(Error::Timeout)
                    ));
                } else {
                    drop(retired);
                }
            }
            // Poll consumers in a generated order. One survivor may take ownership;
            // shared terminal failures must never start another upstream attempt.
            upstream.release = true;
            if failed {
                let producer = consumers.remove(0);
                let error = cache
                    .poll_fault(producer, &mut ring, &mut upstream)
                    .err()
                    .unwrap();
                assert!(matches!(error.root(), Error::Precondition));
            } else if retiring != 0 {
                let producer = consumers.remove(0);
                let _ = resolve_checked(&mut cache, &mut ring, &mut upstream, producer);
            }
            while !consumers.is_empty() {
                let fault = consumers.remove(random.index(consumers.len()));
                if failed {
                    let error = cache
                        .poll_fault(fault, &mut ring, &mut upstream)
                        .err()
                        .unwrap();
                    assert!(matches!(error.root(), Error::Precondition));
                } else {
                    let scope = fault.scope.clone();
                    assert_eq!(fault.deadline(), long);
                    let _ =
                        resolve_observed(&mut cache, &mut ring, &mut upstream, fault, |next, _| {
                            assert_eq!(next.scope, scope);
                            assert_eq!(next.deadline(), long);
                        });
                }
            }
            assert_eq!(
                upstream.starts.len(),
                1 + usize::from(!failed && retiring == 0)
            );
            if !failed && retiring == 0 {
                assert_eq!(upstream.starts.last().unwrap().4, long);
            }
            cache.shutdown(&mut ring).unwrap();
            drop((cache, ring, upstream, slab));
            world.run_tasks();
            world.assert_clean();
        }
    }

    #[test]
    fn dst_candidate_budget_includes_fallback_and_queue_without_reset() {
        let world = crate::simulation::World::new(101);
        let _scope = world.enter();
        let mut slab = allocator::Slab::simulated(
            crate::simulation::Disk::new(32 * 1024 * 1024),
            32 * 1024 * 1024,
            1,
            true,
        )
        .unwrap();
        let mut cache = cache_from_slab(&mut slab, 1, allocator::Config::default());
        let mut ring = crate::control::tests::ring().unwrap();
        let mut upstream = scoped_fake();
        upstream.peer = true;
        upstream.candidate_cap = Some(Duration::from_secs(2));
        upstream.replies.extend([Reply::RetryPeer, Reply::Hold]);
        let caller = world.now() + Duration::from_secs(30);
        let fault = cache
            .metadata::<Fake>("/combined-budget", caller)
            .unwrap()
            .0;
        let (fault, _) = pending_fault(&mut cache, &mut ring, &mut upstream, fault);
        let candidate = fault.deadline();
        assert!(candidate < caller);
        world.advance(Duration::from_millis(750));
        let (fault, _) = pending_fault(&mut cache, &mut ring, &mut upstream, fault);
        assert!(matches!(fault.state, Loading::MetadataRetry(_)));
        let (fault, _) = pending_fault(&mut cache, &mut ring, &mut upstream, fault);
        assert_eq!(upstream.resumes, vec![candidate]);
        world.advance(candidate.saturating_duration_since(world.now()));
        assert!(matches!(
            cache.poll_fault(fault, &mut ring, &mut upstream),
            Err(Error::Timeout)
        ));
        assert_eq!(upstream.starts.len(), 1);
        // Exhaustion before the first acquisition never starts an owner request.
        upstream.candidate_cap = Some(Duration::ZERO);
        let fault = cache.metadata::<Fake>("/queue-expired", caller).unwrap().0;
        assert!(matches!(
            cache.poll_fault(fault, &mut ring, &mut upstream),
            Err(Error::Timeout)
        ));
        assert_eq!(upstream.starts.len(), 1);
        // A failure already proven by the initiated service attempt must reach the
        // ingress coordinator even when its poll lands exactly on candidate expiry.
        upstream.candidate_cap = Some(Duration::from_secs(1));
        upstream.proven = true;
        upstream.replies.push_back(Reply::Error(Error::Unavailable));
        let fault = cache
            .metadata::<Fake>("/proven-at-boundary", caller)
            .unwrap()
            .0;
        let (fault, _) = pending_fault(&mut cache, &mut ring, &mut upstream, fault);
        world.advance(fault.deadline().saturating_duration_since(world.now()));
        let (fault, _) = pending_fault(&mut cache, &mut ring, &mut upstream, fault);
        assert_eq!(upstream.advances, 1);
        drop(fault);
        cache.shutdown(&mut ring).unwrap();
        drop((cache, ring, upstream, slab));
        world.run_tasks();
        world.assert_clean();
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
