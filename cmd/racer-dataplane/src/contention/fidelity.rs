//! Allocation-backed oracles for the simulator's Bundle and page reclamation.
//! Dirty Fill and crypto checks below cover production contracts against explicit
//! Bundle owner traces, not the simulator's event scheduling or completion timing.
use super::*;
use crate::{
    error::{Error, Operation},
    memory::{
        cache::MemoryCache,
        page::PageResult,
        pool::{BufferPool, VerifiedBytes, VerifiedPage},
    },
    model::{
        context::OriginContext,
        envelope::{KeyId, Nonce, PageEnvelope},
        identity::{
            CacheKey, ObjectId, ObjectVersion, PageId, PageNumber, RequestId, StrongEtag, WorkerId,
        },
        metadata::{MetadataSelector, VersionMetadata},
    },
    origin::{client::Origin, metadata::MetadataReply, page::OriginPage},
    peer::{
        requester::PeerClient,
        wire::{PeerRequest, VerifiedResponse},
    },
    read::{
        candidates::{CandidatePolicy, OriginAuthority},
        dispatch::WorkerDirectory,
        fill::{Fill, FillDependencies},
        flight::{AcquisitionBudget, Flights},
    },
    runtime::{
        crypto::{self, CryptoClient},
        deadline::RequestScope,
        reactor::{IoBuffer, Reactor},
        worker::{CryptoRuntime, CryptoService, WorkerMap},
    },
    security::{
        aead::{PageCrypto, PageCryptoEngine},
        credentials::CredentialCrypto,
    },
    store::{
        eviction::SegmentClock, index::Index, reader::StoreReader, segment::Segments, slab::Slabs,
        writer::StoreWriter,
    },
    topology::{
        membership::{Member, Membership},
        placement::Placement,
    },
};
use std::{
    num::{NonZeroU32, NonZeroUsize},
    rc::Rc,
    task::Context,
    time::{Duration, Instant},
};

// The abstract side owns no payload. Production pages put the same non-cloneable
// charges inside Arc<VerifiedBytes>/Arc<CiphertextBytes> (memory/pool.rs:41-58).
struct MetadataOwners {
    bundle: super::Bundle,
}

impl MetadataOwners {
    fn reserve(admission: &Admission, cache: &CacheId) -> Self {
        let reserved = admission.reserve_fill(cache, false).unwrap();
        Self {
            bundle: super::Bundle {
                plain: Arc::new(reserved.plaintext),
                cipher: Arc::new(reserved.ciphertext),
            },
        }
    }
}

impl Clone for MetadataOwners {
    fn clone(&self) -> Self {
        Self {
            bundle: super::Bundle {
                plain: self.bundle.plain.clone(),
                cipher: self.bundle.cipher.clone(),
            },
        }
    }
}

fn admission(pages: usize) -> Rc<Admission> {
    let mut limits = crate::test_support::cluster::config(false).limits;
    limits.plaintext_bytes = NonZeroUsize::new(pages * PLAIN).unwrap();
    limits.ciphertext_bytes = NonZeroUsize::new(pages * CIPHER).unwrap();
    limits.dirty_bytes = NonZeroUsize::new(CIPHER).unwrap();
    limits.metadata_entries = NonZeroUsize::new(16).unwrap();
    Rc::new(Admission::new(limits))
}

fn descriptor(cache: &CacheId, version: &str, length: usize) -> VersionMetadata {
    VersionMetadata {
        version: ObjectVersion {
            object: ObjectId {
                cache: cache.clone(),
                key: CacheKey([0; 32]),
            },
            etag: StrongEtag::test_value(version),
        },
        length: length as u64,
    }
}

fn page_id(metadata: &VersionMetadata) -> PageId {
    PageId {
        version: metadata.version.clone(),
        number: PageNumber(0),
    }
}

fn allocated_page(
    admission: &Rc<Admission>,
    cache: &CacheId,
    version: &str,
    length: usize,
) -> PageResult {
    let metadata = descriptor(cache, version, length);
    let id = page_id(&metadata);
    let reserved = admission.reserve_fill(cache, false).unwrap();
    let pool = BufferPool::new(admission.clone());
    let mut plaintext = pool.plaintext(reserved.plaintext, length).unwrap();
    plaintext.bytes_mut().unwrap().fill(7);
    let (bytes, reservation) = plaintext.into_parts();
    // Structural fixture only: use the production allocation/owner layout, not
    // an authentication oracle (security/aead.rs:267-280 performs this transfer).
    let plaintext = VerifiedPage {
        inner: Arc::new(VerifiedBytes {
            page: id.clone(),
            bytes: bytes.into_vec(),
            reservation,
        }),
    };
    let ciphertext = pool
        .ciphertext(
            reserved.ciphertext,
            PageEnvelope {
                page: id,
                key_id: KeyId([1; 16]),
                nonce: Nonce([2; 24]),
                plaintext_length: length as u32,
                ciphertext_length: (length + 16) as u32,
            },
            vec![9; length + 16],
        )
        .unwrap();
    PageResult {
        metadata: metadata.for_pin(),
        plaintext,
        ciphertext,
    }
}

fn occupancy(admission: &Admission) -> [usize; 3] {
    [
        ResourceClass::Plaintext,
        ResourceClass::Ciphertext,
        ResourceClass::DirtyCiphertext,
    ]
    .map(|class| admission.used(class))
}

fn checkpoint(label: &str, model: &Admission, real: &Admission, expected: [usize; 3]) {
    assert_eq!(occupancy(model), expected, "abstract: {label}");
    assert_eq!(occupancy(real), expected, "production: {label}");
}

#[test]
fn duplicate_owners_match_full_page_occupancy_until_each_last_owner() {
    assert_eq!((PLAIN, CIPHER), (16 * 1024 * 1024, 16 * 1024 * 1024 + 16));
    let model = admission(2);
    let real = admission(2);
    let cache = CacheId("owners".into());
    let memory = MemoryCache::new(Rc::new(BufferPool::new(real.clone())));
    let retained = MetadataOwners::reserve(&model, &cache);
    let page = allocated_page(&real, &cache, "v1", PLAIN);
    let id = page.plaintext.page().clone();
    memory.publish(page).unwrap();
    assert!(retained.bundle.idle(), "published bundle has no readers");
    checkpoint("publish", &model, &real, [PLAIN, CIPHER, 0]);

    let duplicate = retained.clone();
    assert!(!retained.bundle.idle());
    assert!(!duplicate.bundle.idle());
    let read = memory.get(&id).unwrap().unwrap();
    let again = memory.get(&id).unwrap().unwrap();
    assert!(Arc::ptr_eq(&read.plaintext.inner, &again.plaintext.inner));
    assert!(Arc::ptr_eq(&read.ciphertext.inner, &again.ciphertext.inner));
    drop(again);
    checkpoint(
        "duplicate reader charges once",
        &model,
        &real,
        [PLAIN, CIPHER, 0],
    );
    assert!(!retained.bundle.idle());
    assert_eq!(memory.evict_idle(usize::MAX), Ok(0));

    // Either allocation protects the whole bundle, memory/cache.rs:184-186.
    drop(duplicate.bundle.plain);
    drop(read.plaintext);
    assert!(!retained.bundle.idle());
    assert_eq!(memory.evict_idle(usize::MAX), Ok(0));
    checkpoint("ciphertext-only reader", &model, &real, [PLAIN, CIPHER, 0]);
    drop(duplicate.bundle.cipher);
    drop(read.ciphertext);
    assert!(retained.bundle.idle(), "last ciphertext reader released");
    checkpoint("readers released", &model, &real, [PLAIN, CIPHER, 0]);
    let plain_owner = retained.bundle.plain.clone();
    let plain = memory.get(&id).unwrap().unwrap().plaintext;
    assert!(!retained.bundle.idle());
    assert_eq!(memory.evict_idle(usize::MAX), Ok(0));
    checkpoint("plaintext-only reader", &model, &real, [PLAIN, CIPHER, 0]);

    // Lookup removal is not a completion fence, memory/cache.rs:148-180.
    let last_plain = plain_owner.clone();
    let last_real_plain = plain.clone();
    assert!(
        !retained.bundle.idle(),
        "live readers before lookup removal"
    );
    drop(retained);
    memory.remove_cache(&cache).unwrap();
    assert!(memory.get(&id).unwrap().is_none());
    checkpoint(
        "cache removed while readers live",
        &model,
        &real,
        [PLAIN, 0, 0],
    );
    drop((plain_owner, plain));
    checkpoint("one reader remains", &model, &real, [PLAIN, 0, 0]);
    assert_eq!(last_real_plain.bytes().len(), PLAIN);
    assert_eq!(last_real_plain.bytes()[PLAIN - 1], 7);
    drop((last_plain, last_real_plain));
    checkpoint("last owner released", &model, &real, [0, 0, 0]);
}

#[test]
fn fair_reclamation_matches_metadata_trace_and_keeps_other_cache() {
    // Short final pages keep full-page charges; execute class cases sequentially.
    for class in [ResourceClass::Plaintext, ResourceClass::Ciphertext] {
        let mut model = Simulator::new(Config {
            nodes: 1,
            workers: 1,
            pages_per_worker: 4,
            dirty_pages: 1,
            queue_entries: 16,
            ..Config::default()
        });
        let real = admission(4);
        let memory = MemoryCache::new(Rc::new(BufferPool::new(real.clone())));
        // reserve_page uses numeric cache IDs, including for fair-share scope.
        let a = CacheId("0".into());
        let b = CacheId("1".into());
        let keys = [(1, 0, 0), (0, 1, 0), (0, 2, 0)];
        let mut ids = Vec::new();
        for (key, (cache, version)) in
            keys.into_iter()
                .zip([(&b, "other"), (&a, "old"), (&a, "new")])
        {
            let bundle = Bundle {
                plain: Arc::new(
                    model
                        .reserve_page(0, key.0, ResourceClass::Plaintext, PLAIN)
                        .unwrap(),
                ),
                cipher: Arc::new(
                    model
                        .reserve_page(0, key.0, ResourceClass::Ciphertext, CIPHER)
                        .unwrap(),
                ),
            };
            assert!(bundle.idle());
            // Seed retained pages without scheduling network/disk service. All
            // reservation, idle selection, and eviction run the shared model code.
            model.workers[0].cache.insert(key, bundle);
            model.workers[0].lru.push_back(key);
            model.directory.entry(key).or_default().insert(0);
            let page = allocated_page(&real, cache, version, 3);
            ids.push(page.plaintext.page().clone());
            memory.publish(page).unwrap();
        }
        checkpoint(
            "three retained final pages",
            &model.workers[0].admission,
            &real,
            [3 * PLAIN, 3 * CIPHER, 0],
        );
        let amount = if matches!(class, ResourceClass::Plaintext) {
            PLAIN
        } else {
            CIPHER
        };
        assert_eq!(model.report.evictions, 0);
        assert!(model.workers[0].cache.values().all(Bundle::idle));
        for admission in [&model.workers[0].admission, real.as_ref()] {
            assert!(matches!(
                admission.reserve(Some(&a), class, amount),
                Err(Error::Overloaded)
            ));
            // Local deficit takes precedence over global spare room,
            // runtime/admission.rs:149-156. Evicting B cannot remedy A's share.
            assert_eq!(
                admission.reclamation(&a, class, amount),
                Some((Some(a.clone()), amount))
            );
        }
        checkpoint(
            "failed reservation is unchanged",
            &model.workers[0].admission,
            &real,
            [3 * PLAIN, 3 * CIPHER, 0],
        );
        // Either kind of external owner must prevent reserve_page from evicting
        // A's pages, even though B is idle and global capacity remains available.
        let pins: Vec<_> = keys[1..]
            .iter()
            .map(|key| {
                let bundle = &model.workers[0].cache[key];
                if matches!(class, ResourceClass::Plaintext) {
                    bundle.plain.clone()
                } else {
                    bundle.cipher.clone()
                }
            })
            .collect();
        let reads: Vec<_> = ids[1..]
            .iter()
            .map(|id| {
                let page = memory.get(id).unwrap().unwrap();
                if matches!(class, ResourceClass::Plaintext) {
                    (Some(page.plaintext), None)
                } else {
                    (None, Some(page.ciphertext))
                }
            })
            .collect();
        assert!(model.workers[0].cache[&keys[0]].idle());
        assert!(
            keys[1..]
                .iter()
                .all(|key| !model.workers[0].cache[key].idle())
        );
        assert!(model.reserve_page(0, 0, class, amount).is_none());
        assert_eq!(memory.reclaim_idle(class, Some(&a), amount, |_| 0), 0);
        assert_eq!(model.report.evictions, 0);
        assert_eq!(
            model.workers[0].lru.iter().copied().collect::<Vec<_>>(),
            keys
        );
        assert_eq!(model.workers[0].cache.len(), 3);
        assert_eq!(model.directory.len(), 3);
        checkpoint(
            "busy requesting-cache pages survive",
            &model.workers[0].admission,
            &real,
            [3 * PLAIN, 3 * CIPHER, 0],
        );
        drop((pins, reads));
        assert!(model.workers[0].cache.values().all(Bundle::idle));

        let model_next = model
            .reserve_page(0, 0, class, amount)
            .expect("reclaim and retry must admit one page");
        assert_eq!(memory.reclaim_idle(class, Some(&a), amount, |_| 0), amount);
        assert_eq!(occupancy(&real), [2 * PLAIN, 2 * CIPHER, 0]);
        assert!(memory.get(&ids[1]).unwrap().is_none());
        assert!(memory.get(&ids[0]).unwrap().is_some());
        assert!(memory.get(&ids[2]).unwrap().is_some());
        assert_eq!(model.report.evictions, 1);
        assert!(!model.workers[0].cache.contains_key(&keys[1]));
        assert_eq!(model.workers[0].cache.len(), 2);
        assert_eq!(
            model.workers[0].lru.iter().copied().collect::<Vec<_>>(),
            [keys[0], keys[2]]
        );
        assert!(!model.directory.contains_key(&keys[1]));
        assert_eq!(model.directory.len(), 2);
        for key in [keys[0], keys[2]] {
            assert!(model.workers[0].cache[&key].idle());
            assert_eq!(
                model.directory[&key].iter().copied().collect::<Vec<_>>(),
                [0]
            );
        }
        let real_next = real.reserve(Some(&a), class, amount).unwrap();
        let expected = if matches!(class, ResourceClass::Plaintext) {
            [3 * PLAIN, 2 * CIPHER, 0]
        } else {
            [2 * PLAIN, 3 * CIPHER, 0]
        };
        checkpoint(
            "retry admitted",
            &model.workers[0].admission,
            &real,
            expected,
        );
        drop((model_next, real_next));
        assert!(model.workers[0].cache.values().all(Bundle::idle));
        checkpoint(
            "only oldest requesting-cache bundle reclaimed",
            &model.workers[0].admission,
            &real,
            [2 * PLAIN, 2 * CIPHER, 0],
        );
        for key in [keys[0], keys[2]] {
            model.evict(0, key);
        }
        assert!(model.workers[0].cache.is_empty());
        assert!(model.workers[0].lru.is_empty());
        assert!(model.directory.is_empty());
        assert_eq!(model.report.evictions, 3);
        assert_eq!(memory.evict_idle(usize::MAX), Ok(2 * (PLAIN + CIPHER)));
        checkpoint("drained", &model.workers[0].admission, &real, [0, 0, 0]);
    }
}

struct NoTransport;
impl PeerClient for NoTransport {
    fn request<'a>(
        &'a self,
        _: PeerRequest,
        _: &'a RequestScope,
    ) -> Operation<'a, VerifiedResponse> {
        Box::pin(async { panic!("bootstrap fixture must not use a peer") })
    }
}
impl Origin for NoTransport {
    fn metadata<'a>(
        &'a self,
        _: &'a OriginAuthority,
        _: &'a OriginContext,
        _: MetadataSelector,
        _: &'a RequestScope,
    ) -> Operation<'a, MetadataReply> {
        Box::pin(async { panic!("bootstrap fixture already owns origin metadata") })
    }
    fn page<'a>(
        &'a self,
        _: &'a OriginAuthority,
        _: &'a OriginContext,
        _: &'a PageId,
        _: &'a RequestScope,
    ) -> Operation<'a, OriginPage> {
        Box::pin(async { panic!("bootstrap fixture already owns origin bytes") })
    }
}

fn scope() -> RequestScope {
    RequestScope::new(
        RequestId([1; 16]),
        Instant::now() + Duration::from_secs(600),
    )
    .unwrap()
}

#[test]
fn dirty_pressure_matches_metadata_skip_while_real_bootstrap_read_succeeds() {
    // Standalone Fill contract: dirty overload skips persistence while preserving
    // the read and idle working set. No simulator dirty event is driven here.
    let model = admission(4);
    let real = admission(4);
    let cache = CacheId(crate::security::identity::tests::CACHE.into());
    let model_dirty = model
        .reserve(Some(&cache), ResourceClass::DirtyCiphertext, CIPHER)
        .unwrap();
    let real_dirty = real
        .reserve(Some(&cache), ResourceClass::DirtyCiphertext, CIPHER)
        .unwrap();
    let buffers = Rc::new(BufferPool::new(real.clone()));
    let memory = Rc::new(MemoryCache::new(buffers.clone()));
    let old_model = MetadataOwners::reserve(&model, &cache);
    let old = allocated_page(&real, &cache, "old", 3);
    let old_id = old.plaintext.page().clone();
    memory.publish(old).unwrap();
    assert!(old_model.bundle.idle());
    let next_model = MetadataOwners::reserve(&model, &cache);
    assert!(next_model.bundle.idle());
    assert!(matches!(
        model.reserve(Some(&cache), ResourceClass::DirtyCiphertext, CIPHER),
        Err(Error::Overloaded)
    ));
    assert!(old_model.bundle.idle());
    assert!(next_model.bundle.idle());

    let worker = WorkerId(0);
    let index = Rc::new(Index::new(worker, 16));
    let segments = Rc::new(Segments::new(worker, 64 * 1024 * 1024));
    // Never opened: dirty saturation must bypass writer enqueue and all disk I/O.
    let slabs = Rc::new(Slabs::new(
        worker,
        std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("target/contention-fidelity-unopened"),
        Rc::new(Reactor::new(real.clone())),
        128 * 1024 * 1024,
        64 * 1024 * 1024,
    ));
    slabs.set_admission(real.clone());
    let disk = Rc::new(StoreReader::new(
        Rc::new(SegmentClock::new(index.clone(), segments.clone(), 1)),
        index.clone(),
        segments.clone(),
        slabs.clone(),
        buffers.clone(),
    ));
    let writer = Rc::new(StoreWriter::new(index, segments, slabs));
    let keys = Rc::new(crate::security::keyring::tests::keys());
    let (port, engine_port) = crypto::pair(worker, 0, NonZeroUsize::new(4).unwrap());
    let client = Rc::new(CryptoClient::new(port));
    let mut engine = PageCryptoEngine::new(CryptoRuntime { port: engine_port });
    let peers = Rc::new(NoTransport);
    let node = crate::model::identity::NodeId(crate::security::identity::tests::NODE.into());
    let membership = Arc::new(
        Membership::validate(
            crate::model::identity::MembershipVersion(1),
            vec![Member {
                node: node.clone(),
                shares: NonZeroU32::new(1).unwrap(),
                peer_endpoint: "127.0.0.1:8000".into(),
                rails: vec![],
                alignment_enabled: false,
            }],
        )
        .unwrap(),
    );
    let fill = Fill::new(FillDependencies {
        memory: memory.clone(),
        buffers: buffers.clone(),
        disk,
        writer: writer.clone(),
        peers: peers.clone(),
        origin: Rc::new(NoTransport),
        candidates: Rc::new(CandidatePolicy::new(
            node,
            Rc::new(Placement::new(8)),
            peers,
        )),
        flights: Rc::new(Flights::new(real.clone())),
        crypto: Rc::new(PageCrypto::new(keys.clone(), client.clone())),
        credentials: Rc::new(CredentialCrypto::new(keys, real.clone())),
        admission: real.clone(),
        metadata_owner: Arc::new(
            WorkerDirectory::new(
                Arc::new(WorkerMap::new(vec![worker]).unwrap()),
                vec![worker],
                16,
            )
            .unwrap(),
        ),
    });
    let metadata = descriptor(&cache, "new", 3);
    let mut plaintext = buffers
        .plaintext(fill.reserve_bootstrap(&cache).unwrap(), 3)
        .unwrap();
    plaintext.bytes_mut().unwrap().copy_from_slice(b"abc");
    let context = OriginContext {
        object: metadata.version.object.clone(),
        metadata: None,
        authorization: None,
    };
    let scope = scope();
    let mut budget = AcquisitionBudget::new(scope.deadline.0, 4, 8);
    let mut read = fill.publish_bootstrap_with_context(
        OriginPage {
            metadata: metadata.for_pin(),
            plaintext,
        },
        membership,
        &context,
        &scope,
        &mut budget,
    );
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let mut result = None;
    for _ in 0..32 {
        crate::read::drivers::poll(&mut cx, 8);
        engine.poll_budgeted(8).unwrap();
        client.poll_budgeted(8).unwrap();
        if let std::task::Poll::Ready(value) = read.as_mut().poll(&mut cx) {
            result = Some(value.unwrap());
            break;
        }
    }
    let result = result.expect("dirty pressure must not fail or stall the bootstrap read");
    drop(read);
    let result_model = next_model.clone();
    assert!(old_model.bundle.idle());
    assert!(!next_model.bundle.idle());
    assert!(!result_model.bundle.idle());
    assert_eq!(result.plaintext.bytes(), b"abc");
    assert_eq!(result.ciphertext.bytes().len(), 19);
    // Actual Fill overload-to-None policy, read/fill.rs:400-408,720-726.
    assert_eq!(writer.pending_count(), 0);
    assert_eq!(writer.discarded_count(), 0);
    assert!(memory.get(&old_id).unwrap().is_some());
    checkpoint(
        "read succeeds with persistence skipped",
        &model,
        &real,
        [2 * PLAIN, 2 * CIPHER, CIPHER],
    );
    drop(result);
    drop(result_model);
    assert!(old_model.bundle.idle());
    assert!(next_model.bundle.idle());
    drop(fill);
    assert_eq!(crate::read::drivers::pending(), 0);
    drop((old_model, next_model, model_dirty, real_dirty));
    assert_eq!(memory.evict_idle(usize::MAX), Ok(2 * (PLAIN + CIPHER)));
    checkpoint("dirty and page owners drained", &model, &real, [0, 0, 0]);
    assert!(CLASSES.iter().all(|class| real.used(*class) == 0));
}

#[test]
fn canceled_crypto_matches_metadata_owner_trace_through_completion_reap() {
    // Standalone crypto contract: a manually retained Bundle represents accepted
    // work through reap. This does not verify Simulator's cancellation scheduling.
    let model = admission(1);
    let real = admission(1);
    let cache = CacheId(crate::security::identity::tests::CACHE.into());
    let caller = MetadataOwners::reserve(&model, &cache);
    assert!(caller.bundle.idle());
    let completion_owner = caller.clone();
    assert!(!caller.bundle.idle());
    assert!(!completion_owner.bundle.idle());
    let reserved = real.reserve_fill(&cache, false).unwrap();
    let buffers = BufferPool::new(real.clone());
    let plaintext = buffers.plaintext(reserved.plaintext, 3).unwrap();
    let (port, engine_port) = crypto::pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
    let client = Rc::new(CryptoClient::new(port));
    let crypto = PageCrypto::new(
        Rc::new(crate::security::keyring::tests::keys()),
        client.clone(),
    );
    let mut engine = PageCryptoEngine::new(CryptoRuntime { port: engine_port });
    let scope = scope();
    let mut work = crypto.encrypt(
        page_id(&descriptor(&cache, "v1", 3)),
        plaintext,
        reserved.ciphertext,
        &scope,
    );
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(work.as_mut().poll(&mut cx).is_pending());
    assert_eq!(client.outstanding(), 1);
    assert!(!caller.bundle.idle());
    assert!(!completion_owner.bundle.idle());
    checkpoint("accepted crypto", &model, &real, [PLAIN, CIPHER, 0]);
    scope.cancel().unwrap();
    assert!(work.as_mut().poll(&mut cx).is_pending());
    assert!(!caller.bundle.idle());
    assert!(!completion_owner.bundle.idle());
    drop(work);
    drop(caller);
    model.stop();
    real.stop();
    client.poll_budgeted(1).unwrap();
    // Unique ownership is idle by Bundle's predicate, but this owner is held by
    // the completion trace, not a worker cache eligible for reclamation.
    assert!(completion_owner.bundle.idle());
    checkpoint(
        "canceled caller is not a fence",
        &model,
        &real,
        [PLAIN, CIPHER, 0],
    );
    assert_eq!(client.outstanding(), 1);
    // Failed inputs survive the engine as well, security/aead.rs:259-260;
    // abandoned completion release occurs in runtime/crypto.rs:497-518.
    engine.poll_budgeted(1).unwrap();
    assert!(completion_owner.bundle.idle());
    checkpoint(
        "completion published but not reaped",
        &model,
        &real,
        [PLAIN, CIPHER, 0],
    );
    assert_eq!(client.outstanding(), 1);
    client.poll_budgeted(1).unwrap();
    assert!(completion_owner.bundle.idle());
    drop(completion_owner);
    assert_eq!(client.outstanding(), 0);
    checkpoint(
        "completion reaped after admission stop",
        &model,
        &real,
        [0, 0, 0],
    );
    assert!(CLASSES.iter().all(|class| real.used(*class) == 0));
}
