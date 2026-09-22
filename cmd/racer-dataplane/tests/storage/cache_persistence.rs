// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

mod persistence {
    use super::*;
    #[test]
    fn ready_peer_holds_validation_and_retries_corruption_with_fresh_destination() {
        let Some(mut ring) = crate::control::tests::ring() else {
            return;
        };
        let pool = crate::crypto::Pool::test_pool(ring.pool());
        let (worker, source) = pool.attach_local(ring.pool(), ring.wake_handle()).unwrap();
        let worker = Rc::new(std::cell::RefCell::new(worker));
        for (ready, case) in [false, true]
            .into_iter()
            .flat_map(|ready| (0..6).map(move |case| (ready, case)))
        {
            let mut cache = cache(1);
            cache.set_crypto(Some(worker.clone()));
            let target = format!("/plaintext-ready-{ready}-{case}");
            let mut fault = cache.metadata::<Fake>(&target, deadline()).unwrap();
            let key = *fault.key();
            let mut metadata = Record::from_backend(facts(3, "\"v1\"", 60));
            if case == 2 {
                metadata.expires = now().saturating_sub(1);
            }
            let mut plaintext = fill(ring.pool(), key);
            metadata.encode(&mut plaintext.as_mut_slice()[..META_SIZE]);
            let record =
                allocator::crc64(&plaintext.as_mut_slice()[..META_SIZE]) ^ u64::from(case == 4);
            let sealed = plaintext.publish_checked(META_SIZE, record).unwrap();
            let mut bytes = sealed.as_slice().to_vec();
            if case == 3 {
                bytes[0] ^= 1; // checksum identity corruption is detected by the page CRC
            }
            drop(sealed);
            metadata.expires = now() + 60;
            let mut plaintext = fill(ring.pool(), key);
            metadata.encode(&mut plaintext.as_mut_slice()[..META_SIZE]);
            let fresh_record = allocator::crc64(&plaintext.as_mut_slice()[..META_SIZE]);
            let fresh = plaintext.publish_checked(META_SIZE, fresh_record).unwrap();
            let fresh_bytes = fresh.as_slice().to_vec();
            drop(fresh);
            if case == 1 {
                bytes[16] ^= 1;
            }
            if case == 5 {
                bytes.pop();
            }
            let mut upstream = Fake::peer([
                Reply::Checked(record, bytes),
                if !ready || case == 2 {
                    Reply::Good
                } else {
                    Reply::Checked(fresh_record, fresh_bytes)
                },
            ]);
            upstream.scope = scoped_fake().scope;
            upstream.ready_peer = ready;
            let mut saw_open = false;
            loop {
                match cache
                    .poll_metadata(fault, &mut ring, &mut upstream)
                    .unwrap()
                {
                    Progress::Ready(meta) => {
                        assert_eq!(meta.checksum(), fixture_checksum("\"v1\""));
                        break;
                    }
                    Progress::Pending { fault: next, .. } => {
                        if next.0.validation.is_some()
                            && matches!(
                                next.0.state,
                                Loading::ChecksumPending(_) | Loading::Checksum(..)
                            )
                        {
                            assert!(upstream.validations.is_empty());
                            assert!(upstream.resumes.is_empty());
                            saw_open = true;
                        }
                        fault = next;
                    }
                }
                thread::yield_now();
            }
            assert!(
                !saw_open,
                "metadata validation never uses the payload crypto queue"
            );
            assert_eq!(
                upstream.validations,
                if ready {
                    vec![matches!(case, 0 | 2)]
                } else {
                    vec![]
                }
            );
            assert_eq!(
                upstream.resumes.len(),
                usize::from(ready && !matches!(case, 0 | 2))
            );
            let backend = case == 2 || (!ready && case != 0);
            assert_eq!(upstream.starts.len(), 1 + usize::from(backend));
            if backend {
                assert_eq!(upstream.starts[1].0, RequestKind::BackendMetadata);
            }
            cache.shutdown(&mut ring).unwrap();
        }
        drop((source, worker));
        pool.shutdown().unwrap();
    }
    #[test]
    fn tags_policy_storage_and_semantic_ranges() {
        for invalid in [
            "v1",
            "w/\"a\"",
            "\"a b\"",
            "\"a\"b\"",
            "\"a\u{7f}\"",
            "\"a\n\"",
        ] {
            assert!(Checksum::from_etag(invalid).is_err());
        }
        assert!(Checksum::from_etag("").is_err());
        assert!(Checksum::from_etag("W/\"v1\"").is_err());
        assert!(Checksum::from_etag("\"\"").is_err());
        let policy = CachePolicy {
            max_age: Some(100),
            shared_max_age: Some(20),
            age: 3,
            disabled: false,
        };
        assert_eq!(policy.effective_ttl(), 17);
        assert_eq!(
            CachePolicy {
                disabled: true,
                ..policy
            }
            .effective_ttl(),
            0
        );
        assert_eq!(
            CachePolicy {
                age: 1000,
                ..policy
            }
            .effective_ttl(),
            0
        );
        assert_eq!(CachePolicy::default().effective_ttl(), 0);
        let a = Record::from_backend(facts(123, "\"v1\"", 60));
        let b = Record::from_backend(facts(123, "\"v1\"", 0));
        assert_eq!(a.checksum, b.checksum);
        // Representation identity is exactly the checksum, independent of TTL/length.
        assert_eq!(
            a.checksum,
            Record::from_backend(facts(124, "\"v1\"", 60)).checksum
        );
        let mut bytes = [0; META_SIZE];
        a.encode(&mut bytes);
        assert_eq!(&bytes[..32], &a.checksum.0);
        let decoded = Record::decode(&bytes).unwrap();
        assert_eq!(
            (decoded.len, decoded.expires, decoded.checksum),
            (123, a.expires, a.checksum)
        );
        assert!(Record::decode(&bytes[..META_SIZE - 1]).is_err());
        // Corruption detection belongs to the independent stored page CRC.
        assert!(ContentRange::new(1, 0, 3).is_err());
        assert!(ContentRange::new(0, 3, 3).is_err());
        assert!(ContentRange::new(0, u64::MAX, u64::MAX).is_err());
        let cache = cache(1);
        let meta = metadata(&cache, "/range", BUFFER_SIZE as u64 + 3, 0);
        let page = meta.record.page(&meta.object, BUFFER_SIZE as u64).unwrap();
        assert_eq!(page.range().end(), meta.len() - 1);
        assert!(!page.is_full_object());
        assert!(
            page.validate_backend(&BackendPage {
                checksum: page.checksum(),
                range: None
            })
            .is_err()
        );
        assert!(
            page.validate_backend(&BackendPage {
                range: Some(page.range()),
                checksum: page.checksum(),
            })
            .is_ok()
        );
        let meta = metadata(&cache, "/range", 3, 0);
        let page = meta.record.page(&meta.object, 0).unwrap();
        assert!(
            page.validate_backend(&BackendPage {
                checksum: page.checksum(),
                range: None
            })
            .is_ok()
        );
    }

    #[test]
    fn plaintext_first_sight_publishes_files_and_recovers() {
        let Some(mut ring) = crate::control::tests::ring() else {
            return;
        };
        let crypto = crate::crypto::Pool::test_pool(ring.pool());
        let (worker, source) = crypto
            .attach_local(ring.pool(), ring.wake_handle())
            .unwrap();
        let worker = Rc::new(std::cell::RefCell::new(worker));
        let path = std::env::temp_dir().join(format!(
            "racer-plaintext-admission-{}-{}",
            std::process::id(),
            VERSION.fetch_add(1, Ordering::Relaxed)
        ));
        let mut slab = allocator::Slab::create(&path, 32 * 1024 * 1024, 1).unwrap();
        let mut cache = cache_from_slab(&mut slab, 1, allocator::Config::default());
        cache.set_crypto(Some(worker.clone()));
        let mut upstream = Fake::default();
        let target = "/plaintext-admission";
        let meta = resolve_metadata(&mut cache, &mut ring, &mut upstream, target);
        let metadata_key = Object::new(&cache.namespace, target)
            .unwrap()
            .metadata_key()
            .0;
        let fault = cache.page(&meta, 0, deadline()).unwrap();
        let page_key = *fault.key();
        let (page, disk) = resolve_checked(&mut cache, &mut ring, &mut upstream, fault);
        assert!(disk);
        assert_eq!(page.as_slice(), b"xxx");
        let keys = [metadata_key, page_key];
        let values = vec![
            (
                allocator::crc64(&meta.record.to_bytes()),
                meta.record.to_bytes().to_vec(),
            ),
            (page.checksum().unwrap(), page.as_slice().to_vec()),
        ];
        drop(page);
        for key in keys {
            assert!(
                if key == metadata_key {
                    cache.shards[0]
                        .allocator
                        .lookup_metadata(&key, now())
                        .is_some()
                } else {
                    cache.shards[0].allocator.lookup(&key, now()).is_some()
                },
                "first sight must admit to the slab"
            );
        }
        assert_eq!(upstream.starts.len(), 2);
        let meta = resolve_metadata(&mut cache, &mut ring, &mut upstream, target);
        let fault = cache.page(&meta, 0, deadline()).unwrap();
        let (page, disk) = resolve_checked(&mut cache, &mut ring, &mut upstream, fault);
        assert!(disk);
        assert_eq!(page.as_slice(), b"xxx");
        drop(page);
        assert_eq!(upstream.starts.len(), 2);
        for (key, (_, bytes)) in keys.iter().zip(&values) {
            if *key == metadata_key {
                assert_eq!(
                    cache.shards[0]
                        .allocator
                        .lookup_metadata(key, now())
                        .unwrap()
                        .to_bytes()
                        .as_slice(),
                    bytes
                );
                continue;
            }
            let lease = cache.shards[0]
                .allocator
                .lookup(key, now())
                .expect("first sight must admit to disk");
            assert_eq!(lease.info().crc64, allocator::crc64(bytes));
        }
        assert_eq!(&cache.metrics().values()[6..14], &[1, 0, 1, 0, 0, 1, 1, 0]);
        cache.shutdown(&mut ring).unwrap();
        drop((cache, slab));
        // Fork/exec may transiently inherit the O_CLOEXEC slab flock.
        let end = Instant::now() + Duration::from_secs(2);
        let mut slab = loop {
            match allocator::Slab::open(&path, 1) {
                Ok(slab) => break slab,
                Err(e) if e.kind() == io::ErrorKind::WouldBlock && Instant::now() < end => {
                    thread::sleep(Duration::from_millis(1))
                }
                Err(e) => panic!("slab recovery open failed: {e}"),
            }
        };
        let mut recovered = cache_from_slab(&mut slab, 1, allocator::Config::default());
        recovered.set_crypto(Some(worker.clone()));
        for (key, (record, bytes)) in keys.iter().zip(&values) {
            if *key == metadata_key {
                assert_eq!(
                    recovered.shards[0]
                        .allocator
                        .lookup_metadata(key, now())
                        .unwrap()
                        .to_bytes()
                        .as_slice(),
                    bytes
                );
            } else {
                let lease = recovered.shards[0].allocator.lookup(key, now()).unwrap();
                assert_eq!(lease.info().crc64, allocator::crc64(bytes));
                assert_eq!(lease.info().crc64, *record);
            }
        }
        let meta = metadata(&recovered, target, 3, now() + 60);
        for (index, (record, bytes)) in values.iter().enumerate() {
            let fault = if index == 0 {
                recovered.metadata(target, deadline()).unwrap().0
            } else {
                recovered.page(&meta, 0, deadline()).unwrap()
            };
            let (plaintext, disk) =
                resolve_checked(&mut recovered, &mut ring, &mut upstream, fault);
            assert_eq!(disk, index != 0, "only payloads require disk reads");
            if index == 0 {
                assert_eq!(Record::decode(plaintext.as_slice()).unwrap().len, 3);
            } else {
                assert_eq!(plaintext.as_slice(), b"xxx");
            }
            assert_eq!(plaintext.checksum(), Some(*record));
            assert_eq!(plaintext.as_slice(), bytes);
        }
        assert_eq!(upstream.starts.len(), 2, "recovery must not reach upstream");
        assert_eq!(
            &recovered.metrics().values()[6..14],
            &[1, 0, 0, 0, 0, 1, 0, 0]
        );
        recovered.shutdown(&mut ring).unwrap();
        drop((recovered, slab));
        std::fs::remove_file(path).unwrap();
        let mut cache = super::cache(1);
        cache.set_crypto(Some(worker.clone()));
        let meta = metadata(&cache, target, 3, now() + 60);
        let mut peer = Fake::peer([]);
        for sight in 0..2 {
            for (index, (record, bytes)) in values.iter().enumerate() {
                if sight == 0 {
                    peer.replies
                        .push_back(Reply::Checked(*record, bytes.clone()));
                }
                peer.ready_peer = true;
                let fault = if index == 0 {
                    cache.metadata(target, deadline()).unwrap().0
                } else {
                    cache.page(&meta, 0, deadline()).unwrap()
                };
                let (plaintext, disk) = resolve_checked(&mut cache, &mut ring, &mut peer, fault);
                assert!(index == 0 || disk);
                if index == 0 {
                    assert_eq!(Record::decode(plaintext.as_slice()).unwrap().len, 3);
                } else {
                    assert_eq!(plaintext.as_slice(), bytes);
                }
                if index == 0 {
                    assert_eq!(
                        cache.shards[0]
                            .allocator
                            .lookup_metadata(&keys[index], now())
                            .unwrap()
                            .to_bytes()
                            .as_slice(),
                        bytes
                    );
                } else {
                    let lease = cache.shards[0]
                        .allocator
                        .lookup(&keys[index], now())
                        .unwrap();
                    assert_eq!(lease.info().crc64, allocator::crc64(bytes));
                }
            }
        }
        assert_eq!(
            peer.starts.iter().map(|s| s.0).collect::<Vec<_>>(),
            [RequestKind::PeerMetadata, RequestKind::PeerPage]
        );
        assert_eq!(peer.validations, [true; 2]);
        cache.shutdown(&mut ring).unwrap();
        for index in 0..2 {
            let fault = if index == 0 {
                cache.metadata(target, deadline()).unwrap().0
            } else {
                cache.page(&meta, 0, deadline()).unwrap()
            };
            assert_eq!(
                resolve_checked(&mut cache, &mut ring, &mut peer, fault).1,
                index != 0
            );
        }
        assert_eq!(peer.starts.len(), 2);
        cache.shutdown(&mut ring).unwrap();
        drop((cache, source, worker));
        crypto.shutdown().unwrap();
    }

    #[test]
    fn backend_checksum_is_computed_and_stored_with_or_without_worker() {
        for compute_worker in [false, true] {
            let world = crate::simulation::World::new(612);
            let _scope = world.enter();
            let size = 32 * 1024 * 1024;
            let mut slab =
                allocator::Slab::simulated(crate::simulation::Disk::new(size), size, 1, true)
                    .unwrap();
            let mut cache = cache_from_slab(&mut slab, 1, allocator::Config::default());
            let mut ring = crate::control::tests::ring().unwrap();
            let crypto = crate::crypto::Pool::test_pool(ring.pool());
            let (worker, source) = crypto
                .attach_local(ring.pool(), ring.wake_handle())
                .unwrap();
            let worker = Rc::new(std::cell::RefCell::new(worker));
            if compute_worker {
                cache.set_crypto(Some(worker.clone()));
            }
            let meta = metadata(&cache, "/origin-checksum", 3, now() + 60);
            let fault = cache.page(&meta, 0, deadline()).unwrap();
            let key = *fault.key();
            let expected = allocator::crc64(b"abc");
            let mut upstream = Fake {
                replies: [Reply::Checked(expected ^ 1, b"abc".to_vec())].into(),
                ..Fake::default()
            };
            let (bytes, disk) = resolve_checked(&mut cache, &mut ring, &mut upstream, fault);
            assert!(disk);
            assert_eq!(bytes.as_slice(), b"abc");
            assert_eq!(bytes.checksum(), Some(expected));
            cache.shutdown(&mut ring).unwrap();
            let lease = cache.shards[0].allocator.lookup(&key, now()).unwrap();
            assert!(
                lease.buffer().is_none(),
                "checksum must survive disk persistence"
            );
            assert_eq!(lease.info().crc64, expected);
            assert_eq!(upstream.starts.len(), 1);
            drop((cache, source, worker));
            crypto.shutdown().unwrap();
        }
    }

    #[test]
    fn plaintext_forwarding_retains_crc_and_hits_need_no_checksum_work() {
        let Some(mut ring) = crate::control::tests::ring() else {
            return;
        };
        let pool = crate::crypto::Pool::test_pool(ring.pool());
        let (worker, source) = pool.attach_local(ring.pool(), ring.wake_handle()).unwrap();
        let worker = Rc::new(std::cell::RefCell::new(worker));
        for corrupt in [false, true] {
            let mut cache = cache(1);
            cache.set_crypto(Some(worker.clone()));
            let meta = metadata(&cache, "/payload-relay", 17, now() + 60);
            let fault = cache.page::<Fake>(&meta, 0, deadline()).unwrap();
            let key = *fault.key();
            let mut plain = fill(ring.pool(), key);
            plain.as_mut_slice()[..17].fill(42);
            let record = allocator::crc64(&plain.as_mut_slice()[..17]);
            let sealed = plain.publish_checked(17, record).unwrap();
            let bytes = sealed.as_slice().to_vec();
            drop(sealed);
            let mut received = bytes.clone();
            if corrupt {
                received[16] ^= 1;
            }
            let mut peer = Fake::peer([
                Reply::Checked(record, received),
                Reply::Checked(record, bytes.clone()),
            ]);
            peer.ready_peer = true;
            let mut saw_verify = false;
            let (forwarded, _) =
                resolve_observed(&mut cache, &mut ring, &mut peer, fault, |fault, peer| {
                    if matches!(fault.state, Loading::Checksum(..)) {
                        saw_verify = true;
                        assert!(peer.validations.is_empty() || peer.validations == [false]);
                    }
                });
            assert!(saw_verify);
            assert_eq!(forwarded.as_slice(), bytes);
            assert_eq!(forwarded.checksum(), Some(record));
            assert_eq!(peer.validations, [!corrupt]);
            assert_eq!(peer.resumes.len(), usize::from(corrupt));
            assert_eq!(
                peer.starts.len(),
                1,
                "corruption must retry HTTP at same peer"
            );
            let mut pinned = Vec::new();
            while let Ok(scratch) = ring.pool().private_fill() {
                pinned.push(scratch);
            }
            let mut fault = cache.page::<Fake>(&meta, 0, deadline()).unwrap();
            let forwarded_again = loop {
                assert!(!matches!(
                    fault.state,
                    Loading::Checksum(..) | Loading::ChecksumPending(..)
                ));
                match cache.poll_value(fault, &mut ring, &mut peer).unwrap() {
                    Progress::Ready(value) => break value,
                    Progress::Pending { fault: next, .. } => fault = next,
                }
                thread::yield_now();
            };
            assert!(matches!(forwarded_again, CachedValue::File(_)));
            assert_eq!(forwarded_again.checksum(), Some(record));
            drop(pinned);
            let fault = cache.page::<Fake>(&meta, 0, deadline()).unwrap();
            let (client, _) = resolve_checked(&mut cache, &mut ring, &mut peer, fault);
            assert_eq!(client.as_slice(), &[42; 17]);
            assert_eq!(client.as_slice(), forwarded.as_slice());
            assert_eq!(forwarded.as_slice(), bytes);
            let lease = cache.shards[0].allocator.lookup(&key, now()).unwrap();
            assert_eq!(lease.info().crc64, allocator::crc64(&bytes));
            cache.shutdown(&mut ring).unwrap();
        }
        drop((source, worker));
        pool.shutdown().unwrap();
    }

    #[test]
    fn first_sight_admission_and_metadata_only_ttl() {
        let mut cache = cache(1);
        let pool = buffers::io_test_pool(3);
        let meta = metadata(&cache, "/object", 3, 0);
        let first = cache.page::<Fake>(&meta, 0, deadline()).unwrap();
        let bytes = buffer(&pool, first.key, b"abc");
        cache.admit(&first, &bytes, PageCrc::Compute).unwrap();
        assert!(
            cache.shards[0]
                .allocator
                .lookup(&first.key, now())
                .is_some()
        );
        let second = cache.page::<Fake>(&meta, 0, deadline()).unwrap();
        cache
            .admit(&second, &bytes, PageCrc::Supplied(123))
            .unwrap();
        let lease = cache.shards[0]
            .allocator
            .lookup(&first.key, u64::MAX)
            .unwrap();
        assert_eq!(lease.info().expires, 0);
        assert_eq!(lease.info().crc64, allocator::crc64(b"abc"));
        cache.admit(&second, &bytes, PageCrc::Compute).unwrap();
        assert_eq!(
            cache.shards[0]
                .allocator
                .lookup(&first.key, now())
                .unwrap()
                .info()
                .crc64,
            allocator::crc64(b"abc")
        );
        drop(cache.metadata::<Fake>("/expired", deadline()).unwrap());
        let expired = cache.metadata::<Fake>("/expired", deadline()).unwrap();
        assert!(
            !cache.shards[0]
                .allocator
                .insert_metadata(expired.0.key, *meta.record, now())
                .unwrap()
        );
        assert!(
            cache.shards[0]
                .allocator
                .lookup(&expired.0.key, now())
                .is_none()
        );
    }

    #[test]
    fn admission_pressure_is_explicit_with_retained_crc() {
        let path = std::env::temp_dir().join(format!(
            "racer-admission-pressure-{}-{}",
            std::process::id(),
            VERSION.fetch_add(1, Ordering::Relaxed)
        ));
        let mut slab = allocator::Slab::create(&path, 32 * 1024 * 1024, 1).unwrap();
        std::fs::remove_file(path).unwrap();
        let mut cache = cache_from_slab(
            &mut slab,
            1,
            allocator::Config {
                max_pending_values: 1,
                ..allocator::Config::default()
            },
        );
        let pool = buffers::io_test_pool(3);
        for index in 0..2 {
            let meta = metadata(&cache, &format!("/pressure-{index}"), 3, 0);
            drop(cache.page::<Fake>(&meta, 0, deadline()).unwrap());
            let fault = cache.page::<Fake>(&meta, 0, deadline()).unwrap();
            let mut plaintext = fill(&pool, fault.key);
            plaintext.as_mut_slice()[..3].copy_from_slice(b"abc");
            let crc = allocator::crc64(&plaintext.as_mut_slice()[..3]);
            let buffer = plaintext.publish_checked(3, crc).unwrap();
            let result = cache.admit(&fault, &buffer, PageCrc::Supplied(crc));
            if index == 0 {
                result.unwrap();
            } else {
                assert!(
                    matches!(result, Err(Error::Admission(e)) if e.kind() == io::ErrorKind::WouldBlock)
                );
            }
            assert_eq!(
                cache.shards[0]
                    .allocator
                    .lookup(&fault.key, now())
                    .is_some(),
                index == 0
            );
            assert!(cache.sweep_work.runnable);
        }
    }

    #[test]
    fn publication_authority_rejects_foreign_kind_length_and_stale_bytes() {
        let mut cache = cache(1);
        let pool = buffers::io_test_pool(3);
        let meta = metadata(&cache, "/object", 3, 0);
        for bad in 0..5 {
            let mut fault = cache.page::<Fake>(&meta, 0, deadline()).unwrap();
            fault.route = Route::Backend;
            let (authority, mut destination) = fill(&pool, fault.key).split_destination();
            destination.as_mut_slice()[..3].copy_from_slice(b"abc");
            if bad == 0 {
                let (foreign, other) = fill(&pool, [239; 32]).split_destination();
                drop(foreign);
                destination = other;
            }
            let received = Received {
                destination,
                len: if bad == 1 { usize::MAX } else { 3 },
                checksum: None,
            };
            let result = if bad == 2 {
                UpstreamResult::PeerPage(received)
            } else {
                UpstreamResult::BackendPage {
                    received,
                    facts: BackendPage {
                        range: if bad == 3 {
                            Some(ContentRange::new(0, 0, 3).unwrap())
                        } else {
                            None
                        },
                        checksum: if bad == 4 {
                            fixture_checksum("changed")
                        } else {
                            meta.checksum()
                        },
                    },
                }
            };
            assert!(cache.receive(&fault, authority, result).is_err());
            drop(fill(&pool, fault.key));
            assert!(
                cache.shards[0]
                    .allocator
                    .lookup(&fault.key, now())
                    .is_none()
            );
        }
        for truncated in [false, true] {
            let mut fault = cache.metadata::<Fake>("/stale", deadline()).unwrap().0;
            fault.route = Route::Peer;
            let (authority, mut destination) = fill(&pool, fault.key).split_destination();
            let mut record = Record::from_backend(facts(3, "\"v1\"", 60));
            record.expires = now().saturating_sub(1);
            record.encode(&mut destination.as_mut_slice()[..META_SIZE]);
            assert!(
                cache
                    .receive(
                        &fault,
                        authority,
                        UpstreamResult::PeerPage(Received {
                            destination,
                            len: META_SIZE - usize::from(truncated),
                            checksum: None
                        })
                    )
                    .is_err()
            );
            drop(fill(&pool, fault.key));
        }
        let mut fault = cache.metadata::<Fake>("/head", deadline()).unwrap().0;
        fault.route = Route::Backend;
        let (authority, mut destination) = fill(&pool, fault.key).split_destination();
        destination.as_mut_slice()[..META_SIZE].fill(255);
        assert!(
            cache
                .receive(
                    &fault,
                    authority,
                    UpstreamResult::Metadata(Record::from_backend(facts(7, "", 0))),
                )
                .is_err(),
            "metadata cannot be encoded into a payload buffer"
        );
    }

    pub(super) fn maintenance(ring: &mut Ring) {
        let mut cache = cache(3);
        for (budget, runnable) in [(0, true), (1, true), (1, true), (1, false), (1, true)] {
            assert_eq!(cache.poll(ring, budget).unwrap().runnable, runnable);
        }
        Waker::from(ring.wake_handle()).wake_by_ref();
        ring.progress().unwrap();
        for _ in 0..4 {
            assert!(cache.poll(ring, 1).unwrap().runnable);
        }
        assert!(!cache.poll(ring, 1).unwrap().runnable);
        cache.shutdown(ring).unwrap();
    }

    pub(super) fn disk_singleflight_and_scrub(ring: &mut Ring) {
        let mut cache = cache(1);
        let meta = metadata(&cache, "/corrupt", 3, 0);
        drop(cache.page::<Fake>(&meta, 0, deadline()).unwrap());
        let mut fault = cache.page(&meta, 0, deadline()).unwrap();
        let key = fault.key;
        let bytes = buffer(ring.pool(), key, b"abc");
        cache.admit(&fault, &bytes, PageCrc::Supplied(0)).unwrap();
        drop(bytes);
        cache.shutdown(ring).unwrap();
        let mut upstream = Fake::default();
        let mut waiter = cache.page(&meta, 0, deadline()).unwrap();
        loop {
            ring.progress().unwrap();
            let mut work = cache.poll(ring, 1).unwrap();
            match cache.poll_fault(fault, ring, &mut upstream).unwrap() {
                Progress::Ready(bytes) => {
                    assert_eq!(bytes.as_slice(), b"abc");
                    let Progress::Ready(shared) =
                        cache.poll_fault(waiter, ring, &mut upstream).unwrap()
                    else {
                        panic!()
                    };
                    assert_eq!(shared.as_slice(), bytes.as_slice());
                    break;
                }
                Progress::Pending {
                    fault: next,
                    work: w,
                } => {
                    fault = next;
                    work.merge(w);
                }
            }
            let (next, w) = pending(cache.poll_fault(waiter, ring, &mut upstream).unwrap());
            waiter = next;
            work.merge(w);
            if !work.runnable {
                ring.wait(work.deadline).unwrap();
            }
        }
        assert!(upstream.starts.is_empty());
        cache.scrub_at = Instant::now();
        let end = deadline();
        while cache.shards[0].allocator.lookup(&key, now()).is_some() {
            assert!(Instant::now() < end);
            ring.progress().unwrap();
            let work = cache.poll(ring, 1).unwrap();
            if !work.runnable {
                ring.wait(Some(work.deadline.unwrap_or(end).min(end)))
                    .unwrap();
            }
        }
        cache.shutdown(ring).unwrap();
    }
}
use persistence::{disk_singleflight_and_scrub, maintenance};

fn fill(pool: &buffers::WorkerPool, key: [u8; 32]) -> Fill {
    pool.stage(Key::new(key)).unwrap()
}
fn buffer(pool: &buffers::WorkerPool, key: [u8; 32], bytes: &[u8]) -> Buffer {
    let mut fill = fill(pool, key);
    fill.as_mut_slice()[..bytes.len()].copy_from_slice(bytes);
    fill.publish(bytes.len()).unwrap()
}
fn pending<F, T>(progress: Progress<F, T>) -> (F, Work) {
    match progress {
        Progress::Pending { fault, work } => (fault, work),
        Progress::Ready(_) => panic!("expected pending"),
    }
}
fn pending_metadata(
    cache: &mut Cache,
    ring: &mut Ring,
    upstream: &mut Fake,
    fault: MetadataFault<Fake>,
) -> (MetadataFault<Fake>, Work) {
    pending(cache.poll_metadata(fault, ring, upstream).unwrap())
}
fn pending_fault(
    cache: &mut Cache,
    ring: &mut Ring,
    upstream: &mut Fake,
    fault: Fault<Fake>,
) -> (Fault<Fake>, Work) {
    pending(cache.poll_fault(fault, ring, upstream).unwrap())
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum RequestKind {
    BackendMetadata,
    BackendPage,
    PeerMetadata,
    PeerPage,
}
fn kind(request: &UpstreamRequest) -> RequestKind {
    match request {
        UpstreamRequest::BackendMetadata(_) => RequestKind::BackendMetadata,
        UpstreamRequest::BackendPage(_) => RequestKind::BackendPage,
        UpstreamRequest::PeerMetadata(_) => RequestKind::PeerMetadata,
        UpstreamRequest::PeerPage(_) => RequestKind::PeerPage,
    }
}
enum Reply {
    Checked(u64, Vec<u8>),
    Good,
    Stale,
    Corrupt,
    WrongLength(usize),
    WrongRange,
    ChangedTag,
    WrongKind,
    Error(Error),
    StartError,
    Hold,
    RetryPeer,
    Foreign,
}
struct Exchange {
    request: UpstreamRequest,
    destination: Option<Destination>,
    reply: Reply,
}
#[derive(Default)]
struct Fake {
    release: bool,
    candidate_cap: Option<Duration>,
    proven: bool,
    advances: usize,
    scope: Option<crate::buffers::NetworkFlightKey>,
    peer: bool,
    replies: VecDeque<Reply>,
    starts: Vec<(RequestKind, String, [u8; 32], usize, Instant)>,
    held: Option<Destination>,
    resumes: Vec<Instant>,
    resume_error: bool,
    ready_peer: bool,
    validations: Vec<bool>,
}
impl Fake {
    fn peer(replies: impl IntoIterator<Item = Reply>) -> Self {
        Self {
            peer: true,
            replies: replies.into_iter().collect(),
            ..Self::default()
        }
    }
}
impl Upstream for Fake {
    type Exchange = Exchange;
    fn start_metadata(
        &mut self,
        request: UpstreamRequest,
        deadline: Instant,
        _: &mut Ring,
    ) -> Result<Exchange> {
        let (target, key, len) = match &request {
            UpstreamRequest::BackendMetadata(r) | UpstreamRequest::PeerMetadata(r) => {
                (r.target(), r.key(), r.len())
            }
            _ => unreachable!(),
        };
        self.starts
            .push((kind(&request), target.to_owned(), key, len, deadline));
        let reply = self.replies.pop_front().unwrap_or(Reply::Good);
        if matches!(reply, Reply::StartError) {
            return Err(Error::Unavailable);
        }
        Ok(Exchange {
            request,
            destination: None,
            reply,
        })
    }
    fn resume_metadata(
        &mut self,
        mut exchange: Exchange,
        deadline: Instant,
        _: &mut Ring,
    ) -> Result<Exchange> {
        self.resumes.push(deadline);
        if self.resume_error {
            return Err(Error::Unavailable);
        }
        exchange.reply = self.replies.pop_front().unwrap_or(Reply::Good);
        Ok(exchange)
    }
    fn candidate_deadline(&mut self, caller: Instant) -> Instant {
        self.candidate_cap
            .map_or(caller, |d| (crate::environment::now() + d).min(caller))
    }
    fn proven_failure(&self, error: &Error) -> bool {
        self.proven && matches!(error.root(), Error::Unavailable)
    }
    fn peer_failed(&mut self, error: Error) -> Result<bool> {
        // Match production attribution: local admission is not owner failure
        // and cannot authorize candidate advancement or backend fallback.
        if matches!(error.root(), Error::Admission(_)) {
            return Err(error);
        }
        if self.proven {
            self.advances += 1;
        }
        Ok(self.proven)
    }
    fn network_scope(&self, value: [u8; 32]) -> Option<crate::buffers::NetworkFlightKey> {
        self.scope.clone().map(|mut scope| {
            scope.value = value;
            scope
        })
    }
    fn peer_validated(&mut self, _: &mut Exchange, valid: bool) {
        self.validations.push(valid);
    }
    fn has_peer(&self) -> bool {
        self.peer
    }
    fn start(
        &mut self,
        request: UpstreamRequest,
        destination: Destination,
        deadline: Instant,
        _: &mut Ring,
    ) -> Result<Exchange> {
        let (target, key, len) = match &request {
            UpstreamRequest::BackendMetadata(r) | UpstreamRequest::PeerMetadata(r) => {
                (r.target(), r.key(), r.len())
            }
            UpstreamRequest::BackendPage(r) | UpstreamRequest::PeerPage(r) => {
                (r.target(), *r.key(), r.len())
            }
        };
        self.starts
            .push((kind(&request), target.to_owned(), key, len, deadline));
        let reply = self.replies.pop_front().unwrap_or(Reply::Good);
        if matches!(reply, Reply::StartError) {
            // Simulate a driver retaining canceled I/O past a failed start.
            self.held = Some(destination);
            return Err(Error::Unavailable);
        }
        Ok(Exchange {
            request,
            destination: Some(destination),
            reply,
        })
    }
    fn resume_peer(
        &mut self,
        mut exchange: Exchange,
        destination: Destination,
        deadline: Instant,
        _: &mut Ring,
    ) -> Result<Exchange> {
        assert!(exchange.destination.is_none());
        self.resumes.push(deadline);
        if self.resume_error {
            return Err(Error::Unavailable);
        }
        exchange.destination = Some(destination);
        exchange.reply = self.replies.pop_front().unwrap_or(Reply::Good);
        Ok(exchange)
    }
    fn poll(
        &mut self,
        mut exchange: Exchange,
        ring: &mut Ring,
    ) -> Result<ExchangeProgress<Exchange>> {
        if matches!(exchange.reply, Reply::Hold) && !self.release {
            return Ok(ExchangeProgress::Pending {
                exchange,
                work: Work::default(),
            });
        }
        if matches!(exchange.reply, Reply::RetryPeer) {
            self.held = exchange.destination.take();
            return Ok(ExchangeProgress::RetryPeer { exchange });
        }
        if let Reply::Error(error) = exchange.reply {
            return Err(error);
        }
        if exchange.destination.is_none() {
            let bad = matches!(
                exchange.reply,
                Reply::Corrupt | Reply::WrongLength(_) | Reply::Foreign | Reply::WrongKind
            ) || matches!(&exchange.reply, Reply::Checked(crc, bytes) if allocator::crc64(bytes) != *crc || Record::from_bytes(bytes).is_err());
            if bad {
                if std::mem::take(&mut self.ready_peer) {
                    self.validations.push(false);
                    return Ok(ExchangeProgress::RetryPeer { exchange });
                }
                return Err(invalid("bad metadata transport"));
            }
            let record = match &exchange.reply {
                Reply::Checked(crc, bytes) => {
                    if allocator::crc64(bytes) != *crc {
                        return Err(invalid("peer checksum mismatch"));
                    }
                    Record::decode(bytes)?
                }
                Reply::Corrupt | Reply::WrongLength(_) | Reply::Foreign | Reply::WrongKind => {
                    return Err(invalid("bad metadata transport"));
                }
                _ => {
                    let mut record = Record::from_backend(facts(3, "\"v1\"", 60));
                    if matches!(exchange.reply, Reply::Stale) {
                        record.expires = now().saturating_sub(1);
                    }
                    record
                }
            };
            return Ok(if std::mem::take(&mut self.ready_peer) {
                ExchangeProgress::ReadyPeer {
                    result: UpstreamResult::Metadata(record),
                    retry: exchange,
                }
            } else {
                ExchangeProgress::Ready(UpstreamResult::Metadata(record))
            });
        }
        let mut destination = exchange.destination.take().unwrap();
        if let Reply::Checked(crc, bytes) = &exchange.reply {
            destination.as_mut_slice()[..bytes.len()].copy_from_slice(&bytes);
            let received = Received {
                destination,
                len: bytes.len(),
                checksum: Some(*crc),
            };
            let result = match exchange.request {
                UpstreamRequest::PeerMetadata(_) => unreachable!("typed metadata transport"),
                UpstreamRequest::PeerPage(_) => UpstreamResult::PeerPage(received),
                UpstreamRequest::BackendPage(ref request) => UpstreamResult::BackendPage {
                    received,
                    facts: BackendPage {
                        range: Some(request.range()),
                        checksum: request.checksum(),
                    },
                },
                _ => panic!("checked response for backend metadata request"),
            };
            return Ok(if std::mem::take(&mut self.ready_peer) {
                ExchangeProgress::ReadyPeer {
                    result,
                    retry: exchange,
                }
            } else {
                ExchangeProgress::Ready(result)
            });
        }
        let mut len = match &exchange.request {
            UpstreamRequest::BackendMetadata(_) => 0,
            UpstreamRequest::PeerMetadata(_) => {
                let mut record = Record::from_backend(facts(3, "\"v1\"", 60));
                if matches!(exchange.reply, Reply::Stale) {
                    record.expires = now().saturating_sub(1);
                }
                record.encode(&mut destination.as_mut_slice()[..META_SIZE]);
                if matches!(exchange.reply, Reply::Corrupt) {
                    META_SIZE - 1
                } else {
                    META_SIZE
                }
            }
            UpstreamRequest::PeerPage(r) | UpstreamRequest::BackendPage(r) => {
                destination.as_mut_slice()[..r.len()].fill(b'x');
                r.len()
            }
        };
        if let Reply::WrongLength(n) = exchange.reply {
            len = n;
        }
        if matches!(exchange.reply, Reply::Foreign) {
            let (authority, foreign) = fill(ring.pool(), [239; 32]).split_destination();
            drop(authority);
            destination = foreign;
        }
        let bytes = destination.as_mut_slice();
        let checksum = Some(allocator::crc64(&bytes[..len.min(bytes.len())]));
        let received = Received {
            destination,
            len,
            checksum,
        };
        if matches!(exchange.reply, Reply::WrongKind) {
            return Ok(ExchangeProgress::Ready(UpstreamResult::PeerPage(received)));
        }
        let result = match &exchange.request {
            UpstreamRequest::BackendMetadata(_) | UpstreamRequest::PeerMetadata(_) => {
                unreachable!("typed metadata transport")
            }
            UpstreamRequest::PeerPage(_) => UpstreamResult::PeerPage(received),
            UpstreamRequest::BackendPage(request) => {
                let range = if matches!(exchange.reply, Reply::WrongRange) {
                    ContentRange::new(0, 0, request.object_len()).unwrap()
                } else {
                    request.range()
                };
                let checksum = if matches!(exchange.reply, Reply::ChangedTag) {
                    fixture_checksum("changed")
                } else {
                    request.checksum()
                };
                UpstreamResult::BackendPage {
                    received,
                    facts: BackendPage {
                        range: Some(range),
                        checksum,
                    },
                }
            }
        };
        if std::mem::take(&mut self.ready_peer) {
            Ok(ExchangeProgress::ReadyPeer {
                result,
                retry: exchange,
            })
        } else {
            Ok(ExchangeProgress::Ready(result))
        }
    }
}

#[test]
fn completed_payload_file_hits_need_no_slot_and_buffered_hits_are_private() {
    let world = crate::simulation::World::new(413);
    let _scope = world.enter();
    let pool = buffers::io_test_pool(2);
    let mut ring = Ring::http_test_ring(pool.clone(), uring::Config::default()).unwrap();
    let mut slab = allocator::Slab::simulated(
        crate::simulation::Disk::new(32 * 1024 * 1024),
        32 * 1024 * 1024,
        1,
        true,
    )
    .unwrap();
    let mut cache = cache_from_slab(&mut slab, 1, allocator::Config::default());
    let mut upstream = Fake::default();
    let meta = metadata(&cache, "/transient-payload", 3, 0);
    let fault = cache.page(&meta, 0, deadline()).unwrap();
    let (bytes, _) = resolve_checked(&mut cache, &mut ring, &mut upstream, fault);
    assert_eq!(bytes.as_slice(), b"xxx");
    drop(bytes);
    cache.shutdown(&mut ring).unwrap();
    pool.assert_recovered();
    let held: Vec<_> = (0..2).map(|_| pool.private_fill().unwrap()).collect();
    let fault = cache.page(&meta, 0, deadline()).unwrap();
    let Progress::Ready(CachedValue::File(file)) =
        cache.poll_value(fault, &mut ring, &mut upstream).unwrap()
    else {
        panic!("allocator file hit must not allocate a buffer")
    };
    assert_eq!(file.info().len, 3);
    assert_eq!(upstream.starts.len(), 1);
    let fault = cache.page(&meta, 0, deadline()).unwrap();
    let (fault, _) = pending(cache.poll_fault(fault, &mut ring, &mut upstream).unwrap());
    let (fault, work) = pending(cache.poll_fault(fault, &mut ring, &mut upstream).unwrap());
    assert!(
        !work.runnable,
        "materialization must apply pool backpressure"
    );
    drop(held);
    let (first, _) = resolve_checked(&mut cache, &mut ring, &mut upstream, fault);
    let fault = cache.page(&meta, 0, deadline()).unwrap();
    let (second, _) = resolve_checked(&mut cache, &mut ring, &mut upstream, fault);
    assert_eq!(first.as_slice(), b"xxx");
    assert_eq!(second.as_slice(), b"xxx");
    assert_ne!(first.as_slice().as_ptr(), second.as_slice().as_ptr());
    assert_eq!(upstream.starts.len(), 1);
    assert!(pool.private_fill().is_err());
    drop((first, second, file));
    cache.shutdown(&mut ring).unwrap();
    ring.shutdown().unwrap();
    pool.assert_recovered();
    drop((cache, slab, ring, pool));
    world.assert_clean();
}

#[test]
fn payload_takeover_keeps_cancelled_producer_destination_pinned() {
    let world = crate::simulation::World::new(414);
    let _scope = world.enter();
    let pool = buffers::io_test_pool(3);
    let mut ring = Ring::http_test_ring(pool.clone(), uring::Config::default()).unwrap();
    let mut slab = allocator::Slab::simulated(
        crate::simulation::Disk::new(32 * 1024 * 1024),
        32 * 1024 * 1024,
        1,
        true,
    )
    .unwrap();
    let mut cache = cache_from_slab(&mut slab, 1, allocator::Config::default());
    let mut upstream = scoped_fake();
    upstream.replies.push_back(Reply::Hold);
    let meta = metadata(&cache, "/payload-takeover", 3, 0);
    let producer = cache.page(&meta, 0, deadline()).unwrap();
    let consumer = cache.page(&meta, 0, deadline()).unwrap();
    let (mut producer, _) = pending_fault(&mut cache, &mut ring, &mut upstream, producer);
    let (consumer, _) = pending_fault(&mut cache, &mut ring, &mut upstream, consumer);
    let old_io = std::mem::replace(&mut producer.state, Loading::Done);
    assert!(matches!(old_io, Loading::Upstream { .. }));
    drop(producer);
    let (result, _) = resolve_checked(&mut cache, &mut ring, &mut upstream, consumer);
    assert_eq!(result.as_slice(), b"xxx");
    assert_eq!(upstream.starts.len(), 2);
    drop(result);
    cache.shutdown(&mut ring).unwrap();
    assert_eq!(pool.invariant_snapshot().loading, 1);
    let spare: Vec<_> = (0..2).map(|_| pool.private_fill().unwrap()).collect();
    assert!(pool.private_fill().is_err());
    drop(old_io);
    drop(pool.private_fill().unwrap());
    drop(spare);
    ring.shutdown().unwrap();
    pool.assert_recovered();
    drop((cache, slab, ring, pool));
    world.assert_clean();
}

#[test]
fn two_worker_metadata_takeover_with_old_exchange_retained() {
    fn scenario() -> [u8; 32] {
        let world = crate::simulation::World::new(137);
        let _scope = world.enter();
        let pool = buffers::io_test_pool_config(buffers::Config::new(
            std::num::NonZeroUsize::new(3).unwrap(),
        ));
        let other = pool.test_other_worker();
        let mut rings = [
            Ring::http_test_ring(pool.clone(), uring::Config::default()).unwrap(),
            Ring::http_test_ring(other, uring::Config::default()).unwrap(),
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
        for cache in &mut caches {
            cache
                .set_limits(Limits {
                    active_faults: 3,
                    internal_reserve: 1,
                    resource_retries: 2,
                })
                .unwrap();
        }
        for algorithm in [1, 2] {
            let mut providers = [scoped_fake(), scoped_fake()];
            for provider in &mut providers {
                if algorithm == 2 {
                    provider.scope.as_mut().unwrap().dependency =
                        crate::buffers::NetworkDependency::Canonical { slot: 1 };
                    provider.scope.as_mut().unwrap().version = 4;
                }
            }
            providers[0].replies.push_back(Reply::Hold);
            let target = format!("/worker-takeover-{algorithm}");
            let end = world.now() + Duration::from_secs(5);
            let a = caches[0].metadata::<Fake>(&target, end).unwrap().0;
            let b = caches[1].metadata::<Fake>(&target, end).unwrap().0;
            let c = caches[1].metadata::<Fake>(&target, end).unwrap().0;
            let (mut producer, _) = pending(
                caches[0]
                    .poll_fault(a, &mut rings[0], &mut providers[0])
                    .unwrap(),
            );
            let (b, _) = pending(
                caches[1]
                    .poll_fault(b, &mut rings[1], &mut providers[1])
                    .unwrap(),
            );
            let (c, _) = pending(
                caches[1]
                    .poll_fault(c, &mut rings[1], &mut providers[1])
                    .unwrap(),
            );
            assert_eq!(providers[0].starts.len(), 1);
            assert!(providers[1].starts.is_empty());
            // Retain a cancelled typed exchange while the second worker restarts.
            let old_io = std::mem::replace(&mut producer.state, Loading::Done);
            drop(producer);
            let (buffer, _) = resolve_checked(&mut caches[1], &mut rings[1], &mut providers[1], b);
            let (joined, _) = resolve_checked(&mut caches[1], &mut rings[1], &mut providers[1], c);
            assert_eq!(buffer.as_slice(), joined.as_slice());
            assert_eq!(providers[1].starts.len(), 1);
            let key = providers[1].starts[0].2;
            assert!(matches!(old_io, Loading::MetadataExchange(_)));
            assert_eq!(
                caches[1].shards[0]
                    .allocator
                    .lookup_metadata(&key, now())
                    .unwrap()
                    .to_bytes(),
                buffer.as_slice()
            );
            drop((old_io, buffer, joined));
        }
        for (cache, ring) in caches.iter_mut().zip(&mut rings) {
            assert_eq!(cache.active_faults.get(), 0);
            cache.shutdown(ring).unwrap();
            ring.shutdown().unwrap();
        }
        pool.assert_recovered();
        drop((caches, rings, pool));
        world.run_tasks();
        world.assert_clean();
        world.digest()
    }
    assert_eq!(scenario(), scenario());
}

#[test]
fn dst_step7_admission_reserve_and_bounded_pool_wait() {
    let world = crate::simulation::World::new(131);
    let _scope = world.enter();
    let pool = buffers::io_test_pool(1);
    let mut ring = Ring::http_test_ring(pool.clone(), uring::Config::default()).unwrap();
    let mut cache = cache(1);
    cache
        .set_limits(Limits {
            active_faults: 3,
            internal_reserve: 1,
            resource_retries: 2,
        })
        .unwrap();
    let a = cache
        .metadata::<Fake>("/a", world.now() + Duration::from_secs(10))
        .unwrap();
    let b = cache
        .metadata::<Fake>("/b", world.now() + Duration::from_secs(10))
        .unwrap();
    assert!(matches!(
        cache.metadata::<Fake>("/rejected", deadline()),
        Err(Error::Admission(_))
    ));
    let internal = cache
        .peer_fault::<Fake>(PeerDescriptor::metadata("/internal"), deadline())
        .unwrap();
    assert!(matches!(
        cache.peer_fault::<Fake>(PeerDescriptor::metadata("/overload"), deadline()),
        Err(Error::Admission(_))
    ));
    drop((b, internal));
    let held = pool.private_fill().unwrap();
    let mut upstream = Fake::default();
    let (a, _) = pending(cache.poll_metadata(a, &mut ring, &mut upstream).unwrap());
    assert!(matches!(
        cache.poll_metadata(a, &mut ring, &mut upstream).unwrap(),
        Progress::Ready(_)
    ));
    assert_eq!(
        upstream.starts.len(),
        1,
        "cold HEAD ignores payload pool pressure"
    );
    upstream.starts.clear();
    let meta = metadata(&cache, "/pool-pressure-page", 3, 0);
    let start = world.now();
    let mut fault = cache.page(&meta, 0, deadline()).unwrap();
    let mut attempts = 0;
    loop {
        match cache.poll_fault(fault, &mut ring, &mut upstream) {
            Ok(Progress::Pending { fault: next, work }) => {
                attempts += 1;
                assert!(attempts <= 2);
                assert!(!work.runnable);
                world.advance(work.deadline.unwrap().duration_since(world.now()));
                fault = next;
            }
            Err(error) => {
                assert!(matches!(error.root(), Error::Admission(_)));
                break;
            }
            _ => panic!("exhausted pool succeeded"),
        }
    }
    assert_eq!(world.now() - start, Duration::from_millis(20));
    assert!(upstream.starts.is_empty());
    assert_eq!(cache.active_faults.get(), 0);
    drop(held);
    drop(cache.metadata::<Fake>("/recovered", deadline()).unwrap());
    cache.shutdown(&mut ring).unwrap();
    ring.shutdown().unwrap();
    pool.assert_recovered();
    drop((cache, ring, pool));
    world.assert_clean();
}

#[test]
fn exact_identity_version_bounds_and_peer_validation() {
    let mut cache = cache(3);
    assert_eq!(
        cache.namespace,
        digest(b"racer-origin-v1", &[b"127.0.0.1:1"])
    );
    let meta = metadata(&cache, "//%2f?x=1&x=2", BUFFER_SIZE as u64 + 3, 0);
    let first = cache.page::<Fake>(&meta, 0, deadline()).unwrap();
    let last = cache
        .page::<Fake>(&meta, BUFFER_SIZE as u64, deadline())
        .unwrap();
    assert_eq!(first.len(), BUFFER_SIZE);
    assert_eq!(last.len(), 3);
    assert_ne!(first.key, last.key);
    let object_key = digest(
        b"object",
        &[
            &cache.namespace,
            blake3::hash(meta.target().as_bytes()).as_bytes(),
        ],
    );
    assert_eq!(
        first.key,
        digest(
            b"page",
            &[
                &object_key,
                meta.version(),
                &meta.len().to_le_bytes(),
                &0u64.to_le_bytes()
            ]
        )
    );
    assert_eq!(
        *cache
            .metadata::<Fake>(meta.target(), deadline())
            .unwrap()
            .key(),
        digest(b"metadata", &[&object_key])
    );
    assert!(cache.page::<Fake>(&meta, 1, deadline()).is_err());
    assert!(
        cache
            .page::<Fake>(&meta, 2 * BUFFER_SIZE as u64, deadline())
            .is_err()
    );
    for target in ["//%2F?x=1&x=2", "//%2f?x=2&x=1", "//%2f?x=1"] {
        let other = metadata(&cache, target, meta.len(), 0);
        assert_ne!(
            first.key,
            cache.page::<Fake>(&other, 0, deadline()).unwrap().key
        );
    }
    for target in ["", "*", "http://host/x", "/a b", "/a\r\nb", "/#f", "/é"] {
        assert!(cache.metadata::<Fake>(target, deadline()).is_err());
    }
    for identity in ["", "has space", "host\r\n", "é"] {
        assert!(Namespace::new(identity).is_err());
    }
    assert!(Namespace::new(&"x".repeat(1025)).is_err());
    let mut other = self::cache(1);
    assert!(other.page::<Fake>(&meta, 0, deadline()).is_err());
    for fault in [first, last] {
        let Spec::Page(p) = &fault.spec else { panic!() };
        let parsed = PeerDescriptor::page(
            p.target(),
            PeerPage::new(p.offset(), p.object_len(), p.checksum()),
        )
        .with_expected(*p.key(), p.len());
        let decoded = cache.peer_fault::<Fake>(parsed, deadline()).unwrap();
        assert_eq!(decoded.key, fault.key);
        assert_eq!(decoded.len(), fault.len());
        assert_eq!(decoded.target(), meta.target());
    }
    let original = cache.metadata::<Fake>(meta.target(), deadline()).unwrap();
    let descriptor =
        PeerDescriptor::metadata(meta.target()).with_expected(*original.key(), META_SIZE);
    assert_eq!(
        cache
            .peer_fault::<Fake>(descriptor, deadline())
            .unwrap()
            .key(),
        original.key()
    );
    for (key, len) in [
        ([0; 32], META_SIZE),
        (*original.key(), META_SIZE - 1),
        (*original.key(), 0),
    ] {
        assert!(
            cache
                .peer_fault::<Fake>(
                    PeerDescriptor::metadata(meta.target()).with_expected(key, len),
                    deadline()
                )
                .is_err()
        );
    }
    let descriptor = PeerDescriptor::page(
        meta.target(),
        PeerPage::new(0, meta.len(), Checksum([42; 32])),
    )
    .with_expected(
        *cache.page::<Fake>(&meta, 0, deadline()).unwrap().key(),
        BUFFER_SIZE,
    );
    assert!(cache.peer_fault::<Fake>(descriptor, deadline()).is_err());
    let descriptor = PeerDescriptor::page(meta.target(), PeerPage::new(0, 0, Checksum([42; 32])));
    assert!(cache.peer_fault::<Fake>(descriptor, deadline()).is_err());
    let long = format!("/{}", "a".repeat(MAX_PEER_INPUT));
    assert!(cache.metadata::<Fake>(&long, deadline()).is_ok());
    assert!(
        cache
            .peer_fault::<Fake>(PeerDescriptor::metadata(&long), deadline())
            .is_err()
    );
    let descriptor =
        PeerDescriptor::page(meta.target(), PeerPage::new(1, meta.len(), meta.checksum()));
    assert!(cache.peer_fault::<Fake>(descriptor, deadline()).is_err());
    // Different checksums must remain distinct page identities.
    let one = cache
        .peer_fault::<Fake>(
            PeerDescriptor::page("/opaque", PeerPage::new(0, 3, Checksum([1; 32]))),
            deadline(),
        )
        .unwrap();
    let two = cache
        .peer_fault::<Fake>(
            PeerDescriptor::page("/opaque", PeerPage::new(0, 3, Checksum([2; 32]))),
            deadline(),
        )
        .unwrap();
    assert_ne!(one.key(), two.key());
}

#[test]
fn kernel_integration() {
    let mut child = Command::new(std::env::current_exe().unwrap())
        .args([
            "--exact",
            "cache::tests::kernel_child",
            "--ignored",
            "--nocapture",
        ])
        .env("RACER_CACHE_CHILD", "1")
        .stdout(Stdio::inherit())
        .stderr(Stdio::inherit())
        .spawn()
        .unwrap();
    let end = Instant::now() + Duration::from_secs(45);
    loop {
        if let Some(status) = child.try_wait().unwrap() {
            assert!(status.success());
            break;
        }
        if Instant::now() >= end {
            child.kill().unwrap();
            child.wait().unwrap();
            panic!("cache kernel child timed out");
        }
        thread::sleep(Duration::from_millis(10));
    }
}
#[test]
#[ignore = "run via bounded kernel_integration subprocess"]
fn kernel_child() {
    if std::env::var_os("RACER_CACHE_CHILD").is_none() {
        return;
    }
    let mut ring = match Ring::http_test_ring(buffers::io_test_pool(2), uring::Config::default()) {
        Ok(ring) => ring,
        Err(error)
            if matches!(
                error.raw_os_error(),
                Some(libc::EPERM | libc::ENOSYS | libc::ENOMEM)
            ) || error.kind() == io::ErrorKind::Unsupported =>
        {
            assert!(
                std::env::var_os("RACER_REQUIRE_URING").is_none(),
                "io_uring required: {error}"
            );
            eprintln!("SKIP cache kernel tests: {error}");
            return;
        }
        Err(error) => panic!("ring: {error}"),
    };
    single_flight_and_deadlines(&mut ring);
    semantic_fallback(&mut ring);
    peer_retry_reacquisition();
    maintenance(&mut ring);
    disk_singleflight_and_scrub(&mut ring);
    ring.shutdown().unwrap();
}

fn single_flight_and_deadlines(ring: &mut Ring) {
    let mut cache = cache(1);
    let mut upstream = Fake::default();
    let meta = metadata(&cache, "/shared", 3, 0);
    let a = cache.page(&meta, 0, deadline()).unwrap();
    let b = cache.page(&meta, 0, deadline()).unwrap();
    let (a, _) = pending(cache.poll_fault(a, ring, &mut upstream).unwrap());
    assert!(a.can_prefetch());
    let (b, _) = pending(cache.poll_fault(b, ring, &mut upstream).unwrap());
    assert!(!b.can_prefetch());
    let (b, work) = pending(cache.poll_fault(b, ring, &mut upstream).unwrap());
    assert!(!work.runnable);
    assert_eq!(work.deadline, Some(b.deadline));
    let (bytes, _) = resolve_checked(&mut cache, ring, &mut upstream, a);
    let (shared, _) = resolve_checked(&mut cache, ring, &mut upstream, b);
    assert_eq!(bytes.as_slice(), shared.as_slice());
    assert_eq!(upstream.starts.len(), 1);
    drop((bytes, shared));

    let fault = cache.metadata("/timeout", Instant::now()).unwrap();
    assert!(matches!(
        cache.poll_metadata(fault, ring, &mut upstream),
        Err(Error::Timeout)
    ));
    let mut other = self::cache(1);
    let fault = cache.metadata("/foreign", deadline()).unwrap();
    assert!(matches!(
        other.poll_metadata(fault, ring, &mut upstream),
        Err(Error::InvalidData(_))
    ));
    let mut other_ring =
        Ring::http_test_ring(buffers::io_test_pool(1), uring::Config::default()).unwrap();
    let fault = cache.metadata("/foreign-ring", deadline()).unwrap();
    assert!(
        cache
            .poll_metadata(fault, &mut other_ring, &mut upstream)
            .is_err()
    );
    other_ring.shutdown().unwrap();

    // Zero TTL is retained by joined consumers only, even after a newer request starts.
    let record = Record::from_backend(facts(3, "", 0));
    upstream.replies.push_back(Reply::Checked(
        allocator::crc64(&record.to_bytes()),
        record.to_bytes().to_vec(),
    ));
    let producer = cache.metadata("/zero-ttl", deadline()).unwrap();
    let joiner = cache.metadata("/zero-ttl", deadline()).unwrap();
    let (producer, _) = pending(cache.poll_metadata(producer, ring, &mut upstream).unwrap());
    let (joiner, _) = pending(cache.poll_metadata(joiner, ring, &mut upstream).unwrap());
    let Progress::Ready(meta) = cache.poll_metadata(producer, ring, &mut upstream).unwrap() else {
        panic!()
    };
    assert_eq!(meta.expires(), 0);
    let next = cache.metadata("/zero-ttl", deadline()).unwrap();
    let (next, _) = pending(cache.poll_metadata(next, ring, &mut upstream).unwrap());
    assert!(matches!(next.0.state, Loading::MetadataExchange(_)));
    let Progress::Ready(joined) = cache.poll_metadata(joiner, ring, &mut upstream).unwrap() else {
        panic!()
    };
    assert_eq!(*joined.record, record);
    drop(next);
    cache.shutdown(ring).unwrap();
}

fn resolve_metadata(
    cache: &mut Cache,
    ring: &mut Ring,
    upstream: &mut Fake,
    target: &str,
) -> Metadata {
    let mut fault = cache.metadata(target, deadline()).unwrap();
    loop {
        ring.progress().unwrap();
        let mut work = cache.poll(ring, 1).unwrap();
        match cache.poll_metadata(fault, ring, upstream).unwrap() {
            Progress::Ready(metadata) => return metadata,
            Progress::Pending {
                fault: next,
                work: w,
            } => {
                fault = next;
                work.merge(w);
            }
        }
        if !work.runnable {
            ring.wait(work.deadline).unwrap();
        }
    }
}
fn semantic_fallback(ring: &mut Ring) {
    // Only opt-in peer completions retry their alternate transport on invalid
    // bytes. Stale valid records skip that continuation; ordinary Ready below
    // retains its original backend-fallback contract.
    for reply in [
        Reply::Good,
        Reply::Stale,
        Reply::Corrupt,
        Reply::WrongLength(0),
        Reply::WrongLength(usize::MAX),
        Reply::Foreign,
    ] {
        let retry_expected = !matches!(reply, Reply::Good | Reply::Stale);
        let backend_expected = matches!(reply, Reply::Stale);
        let mut cache = cache(1);
        let mut upstream = Fake::peer([reply, Reply::Good]);
        upstream.ready_peer = true;
        let meta = resolve_metadata(&mut cache, ring, &mut upstream, "/ready-peer");
        assert_eq!(meta.len(), 3);
        assert!(meta.expires() > now());
        assert_eq!(upstream.resumes.len(), usize::from(retry_expected));
        assert_eq!(upstream.starts.len(), 1 + usize::from(backend_expected));
        assert_eq!(&cache.metrics().values()[6..10], &[0, 0, 1, 0]);
        if backend_expected {
            assert_eq!(upstream.starts[1].0, RequestKind::BackendMetadata);
        }
        cache.shutdown(ring).unwrap();
    }
    for reply in [
        Reply::Good,
        Reply::WrongLength(0),
        Reply::Error(Error::Gone),
    ] {
        let expect_fallback = !matches!(reply, Reply::Good);
        let mut cache = cache(1);
        let meta = metadata(&cache, "/peer-page", 3, 0);
        let mut upstream = Fake::peer([reply, Reply::Good]);
        let fault = cache.page(&meta, 0, deadline()).unwrap();
        let (bytes, _) = resolve_checked(&mut cache, ring, &mut upstream, fault);
        assert_eq!(bytes.as_slice(), b"xxx");
        drop(bytes);
        assert_eq!(upstream.starts[0].0, RequestKind::PeerPage);
        assert_eq!(upstream.starts.len(), if expect_fallback { 2 } else { 1 });
        if expect_fallback {
            assert_eq!(upstream.starts[1].0, RequestKind::BackendPage);
        }
        cache.shutdown(ring).unwrap();
    }
    for reply in [
        Reply::Stale,
        Reply::Corrupt,
        Reply::WrongLength(usize::MAX),
        Reply::WrongKind,
        Reply::Error(Error::Unavailable),
        Reply::Error(Error::NotFound),
    ] {
        let mut cache = cache(1);
        let mut upstream = Fake::peer([reply, Reply::Good]);
        let meta = resolve_metadata(&mut cache, ring, &mut upstream, "//%2f?x=1&x=2");
        assert_eq!(meta.len(), 3);
        assert!(meta.expires() > now());
        assert_eq!(
            upstream.starts.iter().map(|s| s.0).collect::<Vec<_>>(),
            [RequestKind::PeerMetadata, RequestKind::BackendMetadata]
        );
        assert_eq!(upstream.starts[0].1, "//%2f?x=1&x=2");
        assert_eq!(upstream.starts[0].2, upstream.starts[1].2);
        assert_eq!(upstream.starts[0].3, META_SIZE);
        assert!(upstream.starts[0].4 <= upstream.starts[1].4);
        cache.shutdown(ring).unwrap();
    }
    // Backend semantic failures are terminal, with no publication or admission.
    for reply in [
        Reply::WrongLength(usize::MAX),
        Reply::WrongRange,
        Reply::ChangedTag,
        Reply::WrongKind,
        Reply::Foreign,
    ] {
        let mut cache = cache(1);
        let meta = metadata(&cache, "/invalid-backend", 3, 0);
        let mut upstream = Fake {
            replies: [reply].into(),
            ..Fake::default()
        };
        let fault = cache.page(&meta, 0, deadline()).unwrap();
        let key = fault.key;
        let (fault, _) = pending(cache.poll_fault(fault, ring, &mut upstream).unwrap());
        assert!(cache.poll_fault(fault, ring, &mut upstream).is_err());
        drop(fill(ring.pool(), key));
        cache.shutdown(ring).unwrap();
    }
    // Canceled driver ownership must delay reuse with a one-buffer pool.
    let mut one = Ring::http_test_ring(buffers::io_test_pool(1), uring::Config::default()).unwrap();
    let mut cache = cache(1);
    // Both cold and hot metadata remain independent of the sole pinned slot.
    let occupied = fill(one.pool(), [222; 32]);
    let mut backend = Fake::default();
    let head = cache.metadata("/reserved-head", deadline()).unwrap();
    let (head, _) = pending(cache.poll_metadata(head, &mut one, &mut backend).unwrap());
    assert_eq!(backend.starts[0].0, RequestKind::BackendMetadata);
    assert!(matches!(
        cache.poll_metadata(head, &mut one, &mut backend).unwrap(),
        Progress::Ready(_)
    ));
    let head = cache.metadata("/reserved-head", deadline()).unwrap();
    assert!(matches!(
        cache.poll_metadata(head, &mut one, &mut backend).unwrap(),
        Progress::Ready(_)
    ));
    assert!(one.pool().private_fill().is_err());
    drop(occupied);
    let mut upstream = Fake::peer([Reply::StartError, Reply::Good]);
    let fault = cache.metadata("/cancel", deadline()).unwrap();
    let (fault, _) = pending(cache.poll_metadata(fault, &mut one, &mut upstream).unwrap());
    assert!(matches!(fault.0.route, Route::Backend));
    let (fault, _) = pending(cache.poll_metadata(fault, &mut one, &mut upstream).unwrap());
    assert_eq!(upstream.starts.len(), 2);
    assert!(matches!(
        cache.poll_metadata(fault, &mut one, &mut upstream).unwrap(),
        Progress::Ready(_)
    ));
    cache.shutdown(&mut one).unwrap();
    one.shutdown().unwrap();

    // A cache is pinned to its first ring, even after a completed drain.
    assert!(cache.poll(ring, 1).is_err());
    let mut cache = self::cache(1);

    let mut upstream = Fake::peer([Reply::Hold, Reply::Good]);
    let fault = cache.metadata("/peer-timeout", deadline()).unwrap();
    let (mut fault, _) = pending(cache.poll_metadata(fault, ring, &mut upstream).unwrap());
    let (next, work) = pending(cache.poll_metadata(fault, ring, &mut upstream).unwrap());
    fault = next;
    assert!(!work.runnable);
    assert_eq!(work.deadline, Some(fault.deadline()));
    assert_eq!(upstream.starts[0].4, fault.deadline());
    assert!(matches!(fault.0.route, Route::Peer));
    drop(fault);
    let mut upstream = Fake {
        replies: [Reply::Hold].into(),
        ..Fake::default()
    };
    let fault = cache.metadata("/overall-timeout", deadline()).unwrap();
    let key = *fault.key();
    let (mut fault, _) = pending(cache.poll_metadata(fault, ring, &mut upstream).unwrap());
    fault.0.deadline = Instant::now();
    assert!(matches!(
        cache.poll_metadata(fault, ring, &mut upstream),
        Err(Error::Timeout)
    ));
    drop(fill(ring.pool(), key));
    cache.shutdown(ring).unwrap();
}

fn peer_retry_reacquisition() {
    let mut ring =
        Ring::http_test_ring(buffers::io_test_pool(1), uring::Config::default()).unwrap();
    // Typed retries preserve candidate budgets without acquiring payload storage.
    let held = ring.pool().private_fill().unwrap();
    for mode in 0..8 {
        let mut cache = cache(1);
        let mut upstream = Fake::peer([Reply::RetryPeer, Reply::Good]);
        let fault = cache.metadata("/retry", deadline()).unwrap();
        let end = fault.deadline();
        let (fault, _) = pending_metadata(&mut cache, &mut ring, &mut upstream, fault);
        let (mut fault, _) = pending_metadata(&mut cache, &mut ring, &mut upstream, fault);
        assert!(matches!(fault.0.state, Loading::MetadataRetry(_)));
        assert!(matches!(fault.0.route, Route::Peer));
        assert!(upstream.resumes.is_empty());
        assert!(upstream.held.is_none());
        if mode == 7 {
            fault.0.deadline = Instant::now();
            assert!(matches!(
                cache.poll_metadata(fault, &mut ring, &mut upstream),
                Err(Error::Timeout)
            ));
            cache.shutdown(&mut ring).unwrap();
            continue;
        }
        if mode == 5 {
            upstream.resume_error = true;
        }
        if mode == 6 {
            upstream.replies = [Reply::Corrupt, Reply::Good].into();
        }
        let (mut fault, _) = pending_metadata(&mut cache, &mut ring, &mut upstream, fault);
        assert_eq!(upstream.resumes, [end]);
        assert_eq!(upstream.starts[0].4, end);
        if mode == 5 {
            assert!(matches!(fault.0.route, Route::Backend));
        } else {
            assert!(matches!(fault.0.route, Route::Peer));
            assert!(fault.0.can_prefetch());
        }
        loop {
            match cache
                .poll_metadata(fault, &mut ring, &mut upstream)
                .unwrap()
            {
                Progress::Pending { fault: next, .. } => fault = next,
                Progress::Ready(meta) => {
                    assert_eq!(meta.len(), 3);
                    break;
                }
            }
        }
        assert_eq!(
            upstream.starts.len(),
            if mode == 5 || mode == 6 { 2 } else { 1 }
        );
        if mode == 5 || mode == 6 {
            assert_eq!(upstream.starts[1].0, RequestKind::BackendMetadata);
        }
        cache.shutdown(&mut ring).unwrap();
    }
    drop(held);
    let mut cache = cache(1);
    let mut upstream = Fake {
        replies: [Reply::RetryPeer].into(),
        ..Fake::default()
    };
    let fault = cache.metadata("/invalid-retry", deadline()).unwrap();
    let key = *fault.key();
    let (fault, _) = pending_metadata(&mut cache, &mut ring, &mut upstream, fault);
    assert!(matches!(
        cache
            .poll_metadata(fault, &mut ring, &mut upstream)
            .err()
            .unwrap()
            .root(),
        Error::InvalidData(_)
    ));
    drop(upstream.held.take());
    drop(fill(ring.pool(), key));
    cache.shutdown(&mut ring).unwrap();
    ring.shutdown().unwrap();
}
