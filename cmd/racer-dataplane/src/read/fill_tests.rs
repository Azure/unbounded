use super::*;
#[path = "fill_peer_tests.rs"]
mod peer_copies;
use crate::{
    model::{
        identity::{
            CacheId, CacheKey, ObjectId, ObjectVersion, PageNumber, RequestId, StrongEtag, WorkerId,
        },
        limits::ResourceClass,
        metadata::ExpiresAt,
    },
    origin::{metadata::MetadataReply, page::OriginPage},
    read::dispatch::WorkerDirectory,
    runtime::{
        crypto::{self, CryptoClient},
        reactor::{IoBuffer, Reactor},
        worker::{CryptoRuntime, CryptoService, WorkerMap},
    },
    security::aead::PageCryptoEngine,
    store::{eviction::SegmentClock, index::Index, segment::Segments, slab::Slabs},
    topology::{
        membership::{Member, Membership},
        placement::Placement,
    },
};
use std::{
    cell::{Cell, RefCell},
    future::Future,
    num::NonZeroU32,
    pin::Pin,
    task::{Context, Poll},
    time::{Duration, Instant},
};

struct NoPeer;
impl PeerClient for NoPeer {
    fn request<'a>(
        &'a self,
        _: crate::peer::wire::PeerRequest,
        _: crate::topology::membership::MembershipLease,
        _: &'a RequestScope,
    ) -> Operation<'a, crate::peer::wire::VerifiedResponse> {
        Box::pin(async { panic!("single-node fill must never contact a peer") })
    }
}
struct TestOrigin {
    buffers: Rc<BufferPool>,
    calls: Cell<usize>,
    metadata: ObjectMetadata,
    reject_once: Cell<bool>,
    version_unavailable: Cell<bool>,
}
impl Origin for TestOrigin {
    fn metadata<'a>(
        &'a self,
        _: &'a super::super::candidates::OriginAuthority,
        _: &'a OriginContext,
        _: crate::model::metadata::MetadataSelector,
        _: &'a RequestScope,
    ) -> Operation<'a, MetadataReply> {
        Box::pin(async { panic!("pinned page fill must not refresh metadata") })
    }
    fn page<'a>(
        &'a self,
        _: &'a super::super::candidates::OriginAuthority,
        _: &'a OriginContext,
        _: &'a PageId,
        _: &'a RequestScope,
    ) -> Operation<'a, OriginPage> {
        Box::pin(async { panic!("fill must transfer its existing plaintext reservation") })
    }
    fn page_reserved<'a>(
        &'a self,
        authority: &'a super::super::candidates::OriginAuthority,
        context: &'a OriginContext,
        page: &'a PageId,
        reservation: Reservation,
        scope: &'a RequestScope,
    ) -> Operation<'a, OriginPage> {
        Box::pin(async move {
            scope.check()?;
            authority.validate(&context.object, page.number)?;
            self.calls.set(self.calls.get() + 1);
            let mut yielded = false;
            std::future::poll_fn(|cx| {
                if yielded {
                    Poll::Ready(())
                } else {
                    yielded = true;
                    cx.waker().wake_by_ref();
                    Poll::Pending
                }
            })
            .await;
            if self.reject_once.replace(false) {
                return Err(Error::OriginForbidden);
            }
            if self.version_unavailable.get() {
                return Err(Error::VersionUnavailable);
            }
            let length = self.metadata.immutable().page_length(page)? as usize;
            let mut plaintext = self.buffers.plaintext(reservation, length)?;
            if length == 3 {
                plaintext.bytes_mut()?.copy_from_slice(b"abc");
            } else {
                plaintext.bytes_mut()?.fill(page.number.0 as u8);
            }
            Ok(OriginPage {
                metadata: self.metadata.clone(),
                plaintext,
            })
        })
    }
}

struct Fixture {
    fill: Fill,
    keys: Rc<crate::security::keyring::Keyring>,
    origin: Rc<TestOrigin>,
    crypto: Rc<CryptoClient>,
    engine: PageCryptoEngine,
    context: OriginContext,
    membership: MembershipLease,
    page: PageId,
    scope: RequestScope,
    directory: std::path::PathBuf,
}
impl Drop for Fixture {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.directory);
    }
}
fn fixture() -> Fixture {
    fixture_with(3, None)
}
fn fixture_with(length: u64, limits: Option<crate::model::limits::Limits>) -> Fixture {
    fixture_with_availability(length, limits, false)
}
fn fixture_with_availability(
    length: u64,
    limits: Option<crate::model::limits::Limits>,
    check_availability: bool,
) -> Fixture {
    let mut config = crate::test_support::cluster::config(false);
    if let Some(limits) = limits {
        config.limits = limits;
    }
    let worker = WorkerId(0);
    let admission = Rc::new(Admission::new(config.limits.clone()));
    let keys = Rc::new(crate::security::keyring::tests::keys());
    let availability = crate::control::availability::for_caches(
        keys.clone(),
        vec![CacheId(crate::security::identity::tests::CACHE.into())],
    );
    let buffers = Rc::new(BufferPool::new(admission.clone()));
    let memory = MemoryCache::new(buffers.clone());
    let memory = Rc::new(if check_availability {
        memory.with_availability(availability.clone())
    } else {
        memory
    });
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let index = Index::new(worker, 16);
    let index = Rc::new(if check_availability {
        index.with_availability(availability.clone())
    } else {
        index
    });
    let segments = Rc::new(Segments::new(worker, 64 * 1024 * 1024));
    static NEXT: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);
    let directory = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("target")
        .join(format!(
            "read-fill-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, std::sync::atomic::Ordering::Relaxed)
        ));
    let slabs = Rc::new(Slabs::new(
        worker,
        directory.clone(),
        reactor,
        1024 * 1024 * 1024,
        64 * 1024 * 1024,
    ));
    slabs.set_admission(admission.clone());
    slabs
        .open_now()
        .expect("read fixture filesystem supports direct slab alignment");
    let clock = Rc::new(SegmentClock::new(index.clone(), segments.clone(), 1));
    let disk = Rc::new(StoreReader::new(
        clock,
        index.clone(),
        segments.clone(),
        slabs.clone(),
        buffers.clone(),
    ));
    let writer = StoreWriter::new(index, segments, slabs);
    let writer = Rc::new(if check_availability {
        writer.with_availability(availability.clone())
    } else {
        writer
    });
    let context = OriginContext {
        object: ObjectId {
            cache: CacheId("33333333-3333-4333-8333-333333333333".into()),
            key: CacheKey([0; 32]),
        },
        metadata: None,
        authorization: None,
    };
    let node = crate::model::identity::NodeId("22222222-2222-4222-8222-222222222222".into());
    let membership = Arc::new(
        Membership::validate(
            crate::model::identity::MembershipVersion(1),
            vec![Member {
                node: node.clone(),
                shares: NonZeroU32::new(4).unwrap(),
                peer_endpoint: "127.0.0.1:8000".into(),
                rails: vec![],
                alignment_enabled: false,
            }],
        )
        .unwrap(),
    );
    let metadata = ObjectMetadata {
        version: ObjectVersion {
            object: context.object.clone(),
            etag: StrongEtag::parse(b"\"v1\"").unwrap(),
        },
        length,
        expires_at: ExpiresAt(std::time::UNIX_EPOCH),
    };
    let page = PageId {
        version: metadata.version.clone(),
        number: PageNumber(0),
    };
    let origin = Rc::new(TestOrigin {
        buffers: buffers.clone(),
        calls: Cell::new(0),
        metadata,
        reject_once: Cell::new(false),
        version_unavailable: Cell::new(false),
    });
    let (port, engine) = crypto::pair(worker, 0, config.limits.queue_entries);
    let crypto = Rc::new(CryptoClient::new(port));
    let credentials = Rc::new(CredentialCrypto::new(keys.clone(), admission.clone()));
    let peers = Rc::new(NoPeer);
    let candidates = Rc::new(CandidatePolicy::new(
        node,
        Rc::new(Placement::new(16)),
        peers.clone(),
    ));
    // Deliberately uninstalled catalog owner: publication must retain page-local
    // metadata and still complete when the optional catalog is unavailable.
    let metadata_owner = Arc::new(
        WorkerDirectory::new(
            Arc::new(WorkerMap::new(vec![worker]).unwrap()),
            vec![worker],
            16,
        )
        .unwrap(),
    );
    let flights = Flights::new(admission.clone());
    let flights = Rc::new(if check_availability {
        flights.with_availability(availability)
    } else {
        flights
    });
    let fill = Fill::new(FillDependencies {
        memory,
        buffers,
        disk,
        writer,
        peers,
        origin: origin.clone(),
        candidates,
        flights,
        crypto: Rc::new(PageCrypto::new(keys.clone(), crypto.clone())),
        credentials,
        admission,
        metadata_owner,
    });
    Fixture {
        keys,
        directory,
        fill,
        origin,
        crypto,
        engine: PageCryptoEngine::new(CryptoRuntime { port: engine }),
        context,
        membership,
        page,
        scope: RequestScope::new(
            RequestId([1; 16]),
            Instant::now() + Duration::from_secs(600),
        )
        .unwrap(),
    }
}
fn drive<T>(
    future: impl Future<Output = T>,
    engine: &mut PageCryptoEngine,
    crypto: &CryptoClient,
) -> T {
    let mut future = Box::pin(future);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    for _ in 0..256 {
        super::super::drivers::poll(&mut cx, 64);
        engine.poll_budgeted(64).unwrap();
        crypto.poll_budgeted(64).unwrap();
        if let Poll::Ready(value) = future.as_mut().poll(&mut cx) {
            return value;
        }
    }
    panic!("bounded test executor did not complete");
}

#[test]
fn prefetched_corrupt_ciphertext_falls_back_without_exposing_plaintext() {
    let mut f = fixture();
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
    let result = drive(
        f.fill.acquire(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut budget,
        ),
        &mut f.engine,
        &f.crypto,
    )
    .unwrap();
    let mut bytes = result.ciphertext.bytes().to_vec();
    bytes[0] ^= 1;
    let copy = crate::memory::page::CiphertextCopy {
        metadata: result.metadata.clone(),
        ciphertext: f
            .fill
            .dependencies
            .buffers
            .ciphertext(
                f.fill
                    .dependencies
                    .admission
                    .reserve(
                        Some(&f.context.object.cache),
                        ResourceClass::Ciphertext,
                        bytes.len(),
                    )
                    .unwrap(),
                result.ciphertext.envelope().clone(),
                bytes,
            )
            .unwrap(),
    };
    f.fill
        .dependencies
        .memory
        .remove_cache(&f.context.object.cache)
        .unwrap();
    drop(result);
    let metrics = Metrics::default();
    f.fill.metrics = metrics.clone();
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
    let result = drive(
        f.fill.accept_ciphertext(
            copy,
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut budget,
        ),
        &mut f.engine,
        &f.crypto,
    )
    .unwrap();
    assert_eq!(result.plaintext.bytes(), b"abc");
    assert_eq!(
        metrics.count(Event::PageDecrypt),
        2,
        "bad prefetch then retained original"
    );
    assert_eq!(f.origin.calls.get(), 1);
}

#[test]
fn ciphertext_ready_promotes_once_for_concurrent_plaintext_readers() {
    let mut f = fixture();
    let metrics = Metrics::default();
    f.fill.metrics = metrics.clone();
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
    let original = drive(
        f.fill.acquire(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut budget,
        ),
        &mut f.engine,
        &f.crypto,
    )
    .unwrap();
    assert_eq!(f.origin.calls.get(), 1);
    let copy = original.copy();
    drop(original);
    // Pending original ciphertext is retained by the writer, without plaintext.
    f.fill
        .dependencies
        .memory
        .remove_cache(&f.context.object.cache)
        .unwrap();
    let flights = f.fill.dependencies.flights.clone();
    let mut holder_budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
    let JoinedFlight::Waiter(holder) = flights
        .join_for(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut holder_budget,
            false,
        )
        .unwrap()
    else {
        panic!("registration")
    };
    let mut cipher_budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
    let result = drive(
        f.fill.acquire_ciphertext(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut cipher_budget,
        ),
        &mut f.engine,
        &f.crypto,
    )
    .unwrap();
    assert_eq!(result.ciphertext.bytes(), copy.ciphertext.bytes());
    assert_eq!(metrics.count(Event::PageDecrypt), 0);
    let mut first_budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
    let mut second_budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
    let (a, b) = drive(
        async {
            futures::join!(
                f.fill.acquire(
                    f.page.clone(),
                    f.membership.clone(),
                    &f.context,
                    &f.scope,
                    &mut first_budget
                ),
                f.fill.acquire(
                    f.page.clone(),
                    f.membership.clone(),
                    &f.context,
                    &f.scope,
                    &mut second_budget
                )
            )
        },
        &mut f.engine,
        &f.crypto,
    );
    let (a, b) = (a.unwrap(), b.unwrap());
    assert_eq!(a.plaintext.bytes(), b"abc");
    assert!(Arc::ptr_eq(&a.plaintext.inner, &b.plaintext.inner));
    assert_eq!(metrics.count(Event::PageDecrypt), 1);
    assert_eq!(f.origin.calls.get(), 1);
    drop(holder);
}

#[test]
fn retired_completed_flight_misses_new_callers_but_admitted_waiters_finish() {
    use crate::security::keyring::{KeyPurpose, tests::rotation_bundle};
    let mut f = fixture_with_availability(3, None, true);
    let flights = f.fill.dependencies.flights.clone();
    let mut held_budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
    let JoinedFlight::Waiter(mut held) = flights
        .join(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut held_budget,
        )
        .unwrap()
    else {
        panic!("expected registration")
    };
    let JoinedCopy::Waiter(mut old_copy) = flights.join_copy(&f.page, &f.scope).unwrap() else {
        panic!("expected copy registration")
    };
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
    let original = drive(
        f.fill.acquire(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut budget,
        ),
        &mut f.engine,
        &f.crypto,
    )
    .unwrap();
    let old_key = original.ciphertext.envelope().key_id;
    // Exercise the completed fast paths before retirement, with registrations
    // deliberately retaining the real encrypted fill after its driver finishes.
    assert!(matches!(
        flights.join_copy(&f.page, &f.scope).unwrap(),
        JoinedCopy::Complete(_)
    ));
    assert!(matches!(
        flights
            .join(
                f.page.clone(),
                f.membership.clone(),
                &f.context,
                &f.scope,
                &mut budget
            )
            .unwrap(),
        JoinedFlight::Complete(_)
    ));
    let roots = (*f.keys.peer_trust_roots().unwrap()).clone();
    f.keys.install(rotation_bundle(2, roots)).unwrap();
    let current_key = f
        .keys
        .active(&f.context.object.cache, KeyPurpose::Page)
        .unwrap()
        .id();
    assert_ne!(current_key, old_key);
    assert!(
        f.keys
            .lease(Some(&f.context.object.cache), old_key, KeyPurpose::Page)
            .is_err()
    );
    assert!(f.fill.dependencies.memory.get(&f.page).unwrap().is_none());
    assert!(
        f.fill
            .dependencies
            .writer
            .copy_only(&f.page)
            .unwrap()
            .is_none()
    );
    assert!(matches!(
        flights.join(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut budget
        ),
        Err(Error::MissingKey)
    ));
    assert!(matches!(
        flights.join_copy(&f.page, &f.scope).unwrap(),
        JoinedCopy::Miss
    ));
    assert!(matches!(
        drive(
            f.fill.acquire(
                f.page.clone(),
                f.membership.clone(),
                &f.context,
                &f.scope,
                &mut budget
            ),
            &mut f.engine,
            &f.crypto,
        ),
        Err(Error::MissingKey)
    ));
    assert!(
        drive(
            f.fill.copy_only(&f.page, &f.scope),
            &mut f.engine,
            &f.crypto
        )
        .unwrap()
        .is_none()
    );
    assert_eq!(f.origin.calls.get(), 1);
    // The public pinned coordinator may still know the immutable descriptor.
    // Its range body must nevertheless miss the retired completed page flight.
    {
        use crate::{
            client::request::{ClientRequest, ReadKind},
            control::{
                caches::CacheDefinition,
                snapshot::{PublishedState, SnapshotStore},
                wire::*,
            },
            memory::{delivery::Delivery, pipe::PipePool},
            model::range::ByteRange,
            read::{
                metadata::{MetadataDependencies, MetadataService},
                range_stream::RangeStreams,
                serve::{Coordinator, ReadService},
            },
        };
        let snapshots = Rc::new(SnapshotStore::new(
            f.keys.cluster().clone(),
            Arc::new(PublishedState::default()),
            2,
        ));
        let (client_socket, origin_socket) =
            crate::control::caches::canonical_socket_paths("rotation").unwrap();
        snapshots
            .publish(Publication {
                schema_version: SCHEMA_VERSION,
                cluster: f.keys.cluster().clone(),
                sequence: PublicationSequence(1),
                membership_version: f.membership.version,
                members: f.membership.members().to_vec(),
                caches: vec![CacheDefinition {
                    id: f.context.object.cache.clone(),
                    name: "rotation".into(),
                    client_socket,
                    origin_socket,
                }],
            })
            .unwrap();
        let fill = Rc::new(Fill::new(f.fill.dependencies.clone()));
        let owners = fill.dependencies.metadata_owner.clone();
        let metadata = Rc::new(MetadataService::new(
            fill.dependencies.candidates.clone(),
            f.origin.clone(),
            fill.dependencies.peers.clone(),
            fill.dependencies.credentials.clone(),
            16,
            MetadataDependencies {
                index: Rc::new(Index::new(WorkerId(0), 16)),
                owners: owners.clone(),
                fill: fill.clone(),
            },
        ));
        metadata
            .publish_version(original.metadata.immutable())
            .unwrap();
        let admission = fill.dependencies.admission.clone();
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let delivery = Rc::new(Delivery::new(
            Rc::new(PipePool::new(admission, reactor)),
            Duration::from_secs(10),
        ));
        let streams = Rc::new(RangeStreams::new(fill.clone(), owners.clone(), delivery, 1));
        let coordinator = Rc::new(Coordinator::new(
            snapshots,
            metadata,
            fill.clone(),
            streams,
            fill.dependencies.credentials.clone(),
        ));
        let mut endpoint = owners.install(WorkerId(0), coordinator.clone()).unwrap();
        let mut response = drive(
            coordinator.read(
                ClientRequest {
                    kind: ReadKind::Pinned {
                        etag: f.page.version.etag.clone(),
                        range: ByteRange::Closed { first: 0, last: 2 },
                    },
                    origin: OriginContext {
                        object: f.context.object.clone(),
                        metadata: None,
                        authorization: None,
                    },
                },
                &f.scope,
            ),
            &mut f.engine,
            &f.crypto,
        )
        .unwrap();
        let mut slice = response.body.as_mut().unwrap().next_slice();
        assert!(matches!(
            drive(
                std::future::poll_fn(|cx| {
                    endpoint.poll(cx, 64).unwrap();
                    slice.as_mut().poll(cx)
                }),
                &mut f.engine,
                &f.crypto
            ),
            Err(Error::MissingKey)
        ));
    }
    let AcquisitionEvent::Complete(admitted) = futures::executor::block_on(held.wait()).unwrap()
    else {
        panic!("original waiter must finish")
    };
    let admitted_copy = futures::executor::block_on(old_copy.wait()).unwrap();
    assert_eq!(admitted.plaintext.bytes(), b"abc");
    assert_eq!(
        admitted_copy.copy().ciphertext.bytes(),
        original.ciphertext.bytes()
    );
    assert_eq!(admitted.ciphertext.envelope().key_id, old_key);
    drop((held, old_copy));
    let replacement = drive(
        f.fill.acquire(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut budget,
        ),
        &mut f.engine,
        &f.crypto,
    )
    .unwrap();
    assert_eq!(replacement.ciphertext.envelope().key_id, current_key);
    assert_eq!(replacement.plaintext.bytes(), b"abc");
    assert_eq!(f.origin.calls.get(), 2);
    assert_eq!(original.plaintext.bytes(), b"abc");

    // Publication can also retain a completed bundle behind an outstanding
    // completion fence. New joins must not sneak into that draining cohort.
    let JoinedFlight::Waiter(mut draining) = flights
        .join(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut held_budget,
        )
        .unwrap()
    else {
        panic!("expected new flight")
    };
    let AcquisitionEvent::Lead(leader) = futures::executor::block_on(draining.wait()).unwrap()
    else {
        panic!("expected election")
    };
    let operation = flights.retain_operation(&leader, ()).unwrap();
    flights.publish(leader, replacement.clone()).unwrap();
    let roots = (*f.keys.peer_trust_roots().unwrap()).clone();
    f.keys.install(rotation_bundle(3, roots)).unwrap();
    assert!(matches!(
        flights.join(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut budget
        ),
        Err(Error::MissingKey)
    ));
    assert!(matches!(
        flights.join_copy(&f.page, &f.scope).unwrap(),
        JoinedCopy::Miss
    ));
    operation.complete().unwrap();
    let AcquisitionEvent::Complete(result) = futures::executor::block_on(draining.wait()).unwrap()
    else {
        panic!("admitted draining waiter must finish")
    };
    assert_eq!(result.ciphertext.envelope().key_id, current_key);
}

struct GatedMetadataOrigin {
    receive: RefCell<Option<futures::channel::oneshot::Receiver<MetadataReply>>>,
    calls: Cell<usize>,
}
impl Origin for GatedMetadataOrigin {
    fn metadata<'a>(
        &'a self,
        _: &'a super::super::candidates::OriginAuthority,
        _: &'a OriginContext,
        _: crate::model::metadata::MetadataSelector,
        _: &'a RequestScope,
    ) -> Operation<'a, MetadataReply> {
        self.calls.set(self.calls.get() + 1);
        let receive = self.receive.borrow_mut().take().unwrap();
        Box::pin(async move { receive.await.map_err(|_| Error::Unavailable) })
    }
    fn page<'a>(
        &'a self,
        _: &'a super::super::candidates::OriginAuthority,
        _: &'a OriginContext,
        _: &'a PageId,
        _: &'a RequestScope,
    ) -> Operation<'a, OriginPage> {
        Box::pin(async { panic!("metadata-only refresh") })
    }
}

#[test]
fn blocked_metadata_leader_and_follower_notify_without_spinning() {
    use crate::{
        model::metadata::MetadataSelector,
        read::metadata::{MetadataDependencies, MetadataService},
        test_support::WakeCounter,
    };
    use std::task::Waker;
    for cancel_leader in [false, true] {
        let f = fixture();
        let (send, receive) = futures::channel::oneshot::channel();
        let service = MetadataService::new(
            f.fill.dependencies.candidates.clone(),
            Rc::new(GatedMetadataOrigin {
                receive: RefCell::new(Some(receive)),
                calls: Cell::new(0),
            }),
            f.fill.dependencies.peers.clone(),
            f.fill.dependencies.credentials.clone(),
            4,
            MetadataDependencies {
                index: Rc::new(Index::new(WorkerId(0), 4)),
                owners: f.fill.dependencies.metadata_owner.clone(),
                fill: Rc::new(Fill::new(f.fill.dependencies.clone())),
            },
        );
        let follower_scope = RequestScope::new(RequestId([2; 16]), f.scope.deadline.0).unwrap();
        let mut leader = service.resolve(
            MetadataSelector::Fresh,
            f.membership.clone(),
            &f.context,
            &f.scope,
        );
        let mut follower = service.resolve(
            MetadataSelector::Fresh,
            f.membership.clone(),
            &f.context,
            &follower_scope,
        );
        let count = Arc::new(WakeCounter::default());
        let waker = Waker::from(count.clone());
        let mut cx = Context::from_waker(&waker);
        assert!(leader.as_mut().poll(&mut cx).is_pending());
        assert!(follower.as_mut().poll(&mut cx).is_pending());
        let settled = count.count();
        for _ in 0..4 {
            assert!(leader.as_mut().poll(&mut cx).is_pending());
            assert!(follower.as_mut().poll(&mut cx).is_pending());
        }
        assert_eq!(
            count.count(),
            settled,
            "external metadata waits must not self-wake"
        );
        let canceled = if cancel_leader {
            &f.scope
        } else {
            &follower_scope
        };
        canceled.cancel().unwrap();
        assert!(
            count.count() > settled,
            "cancellation registration survives polling"
        );
        if cancel_leader {
            assert!(matches!(
                leader.as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::Cancelled))
            ));
        } else {
            assert!(matches!(
                follower.as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::Cancelled))
            ));
        }
        let before = count.count();
        send.send(MetadataReply {
            metadata: f.origin.metadata.clone(),
            page_zero: None,
        })
        .ok()
        .unwrap();
        assert!(
            count.count() > before,
            "origin completion reaches registered driver"
        );
        if cancel_leader {
            // Finish the retained driver after caller cancellation without electing
            // a replacement supplier in this test.
            drop(follower);
            super::super::drivers::poll(&mut cx, 64);
        } else {
            assert!(matches!(leader.as_mut().poll(&mut cx), Poll::Ready(Ok(_))));
        }
        assert_eq!(super::super::drivers::pending(), 0);
    }
}

#[test]
fn metadata_deadline_wakes_parked_follower_without_polling_gated_leader() {
    use crate::{
        model::metadata::MetadataSelector,
        read::metadata::{MetadataDependencies, MetadataService, tests::assert_ingress_counts},
        test_support::WakeCounter,
    };
    use futures::{Stream, stream::FuturesUnordered};
    use std::task::Waker;

    for (budget_earlier, expire_leader) in [(false, false), (true, false), (false, true)] {
        let f = fixture();
        let admission = &f.fill.dependencies.admission;
        let baseline = admission.used(ResourceClass::RequestContext);
        let (send, receive) = futures::channel::oneshot::channel();
        let origin = Rc::new(GatedMetadataOrigin {
            receive: RefCell::new(Some(receive)),
            calls: Cell::new(0),
        });
        let service = MetadataService::new(
            f.fill.dependencies.candidates.clone(),
            origin.clone(),
            f.fill.dependencies.peers.clone(),
            f.fill.dependencies.credentials.clone(),
            1,
            MetadataDependencies {
                index: Rc::new(Index::new(WorkerId(0), 4)),
                owners: f.fill.dependencies.metadata_owner.clone(),
                fill: Rc::new(Fill::new(f.fill.dependencies.clone())),
            },
        );
        let due = Instant::now() + Duration::from_secs(60);
        let follower_scope = RequestScope::new(
            RequestId([2; 16]),
            if budget_earlier {
                f.scope.deadline.0
            } else {
                due
            },
        )
        .unwrap();
        let mut budget = AcquisitionBudget::new(
            if budget_earlier {
                due
            } else {
                f.scope.deadline.0
            },
            4,
            8,
        );
        let leader_polls = Cell::new(0);
        let follower_polls = Cell::new(0);
        let mut leader = service.resolve(
            MetadataSelector::Fresh,
            f.membership.clone(),
            &f.context,
            &f.scope,
        );
        let mut follower = service.resolve_with_budget(
            MetadataSelector::Fresh,
            f.membership.clone(),
            &f.context,
            &follower_scope,
            &mut budget,
        );
        let mut stream = FuturesUnordered::<
            futures::future::LocalBoxFuture<'_, (bool, Result<ObjectMetadata>)>,
        >::new();
        stream.push(Box::pin(std::future::poll_fn(|cx| {
            leader_polls.set(leader_polls.get() + 1);
            leader.as_mut().poll(cx).map(|result| (true, result))
        })));
        let count = Arc::new(WakeCounter::default());
        let waker = Waker::from(count.clone());
        let mut cx = Context::from_waker(&waker);
        assert!(Pin::new(&mut stream).poll_next(&mut cx).is_pending());
        stream.push(Box::pin(std::future::poll_fn(|cx| {
            follower_polls.set(follower_polls.get() + 1);
            follower.as_mut().poll(cx).map(|result| (false, result))
        })));
        // Drain initial scheduling notifications, then only poll the parent stream.
        for _ in 0..4 {
            assert!(Pin::new(&mut stream).poll_next(&mut cx).is_pending());
        }
        assert_eq!(origin.calls.get(), 1);
        assert_ingress_counts(&service, 2, 2);
        let retained = admission.used(ResourceClass::RequestContext);
        assert!(retained > baseline);
        let parked = (leader_polls.get(), follower_polls.get(), count.count());
        for _ in 0..4 {
            assert_eq!(service.poll_deadlines(due - Duration::from_nanos(1), 64), 0);
            assert!(Pin::new(&mut stream).poll_next(&mut cx).is_pending());
        }
        assert_eq!(
            (leader_polls.get(), follower_polls.get(), count.count()),
            parked
        );
        assert_eq!(service.poll_deadlines(due, 0), 0);
        assert!(Pin::new(&mut stream).poll_next(&mut cx).is_pending());
        assert_eq!(service.poll_deadlines(due, 1), 1);
        assert!(matches!(
            Pin::new(&mut stream).poll_next(&mut cx),
            Poll::Ready(Some((false, Err(Error::DeadlineExceeded))))
        ));
        assert_eq!(leader_polls.get(), parked.0);
        assert_eq!(follower_polls.get(), parked.1 + 1);
        assert_eq!(origin.calls.get(), 1);
        assert_eq!(super::super::drivers::pending(), 1);
        assert_ingress_counts(&service, 1, 1);
        assert_eq!(admission.used(ResourceClass::RequestContext), retained);
        let after_expiry = count.count();
        assert_eq!(service.poll_deadlines(due, 64), 0);
        assert!(Pin::new(&mut stream).poll_next(&mut cx).is_pending());
        assert_eq!(count.count(), after_expiry);
        if expire_leader {
            assert_eq!(service.poll_deadlines(f.scope.deadline.0, 1), 1);
            assert!(matches!(
                Pin::new(&mut stream).poll_next(&mut cx),
                Poll::Ready(Some((true, Err(Error::DeadlineExceeded))))
            ));
            // Ingress timeout detaches only its guard. The driver still owns the
            // refresh registration, original budget, and charged origin context.
            assert_ingress_counts(&service, 0, 1);
            assert_eq!(super::super::drivers::pending(), 1);
            assert_eq!(admission.used(ResourceClass::RequestContext), retained);
            assert_eq!(origin.calls.get(), 1);
        }
        send.send(MetadataReply {
            metadata: f.origin.metadata.clone(),
            page_zero: None,
        })
        .ok()
        .unwrap();
        super::super::drivers::poll(&mut cx, 64);
        if !expire_leader {
            assert!(matches!(Pin::new(&mut stream).poll_next(&mut cx),
                Poll::Ready(Some((true, Ok(metadata)))) if metadata == f.origin.metadata));
        }
        assert!(matches!(
            Pin::new(&mut stream).poll_next(&mut cx),
            Poll::Ready(None)
        ));
        assert_eq!(service.poll_deadlines(f.scope.deadline.0, 64), 0);
        assert_eq!(super::super::drivers::pending(), 0);
        assert_ingress_counts(&service, 0, 0);
        assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
        assert_eq!(origin.calls.get(), 1);
    }
}

#[test]
fn concurrent_readers_share_origin_encryption_and_pending_original_ciphertext() {
    let mut f = fixture();
    let mut a = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
    let mut b = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
    let reads = futures::future::join(
        f.fill.acquire(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut a,
        ),
        f.fill.acquire(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut b,
        ),
    );
    let (a, b) = drive(reads, &mut f.engine, &f.crypto);
    let a = a.unwrap();
    let b = b.unwrap();
    assert_eq!(f.origin.calls.get(), 1);
    assert!(Arc::ptr_eq(&a.plaintext.inner, &b.plaintext.inner));
    assert_eq!(f.fill.metrics.count(Event::OriginFill), 1);
    assert_eq!(f.fill.metrics.count(Event::MemoryHit), 0);
    assert_eq!(f.fill.metrics.gauge(Gauge::ActiveFills), 0);
    assert!(Arc::ptr_eq(&a.ciphertext.inner, &b.ciphertext.inner));
    assert_eq!(a.plaintext.bytes(), b"abc");
    let copy = drive(
        f.fill.copy_only(&f.page, &f.scope),
        &mut f.engine,
        &f.crypto,
    )
    .unwrap()
    .unwrap();
    assert_eq!(copy.1.bytes(), a.ciphertext.bytes());
    assert_eq!(copy.1.envelope().nonce, a.ciphertext.envelope().nonce);
    assert_eq!(f.origin.calls.get(), 1);
    assert!(
        f.fill
            .dependencies
            .writer
            .copy_only(&f.page)
            .unwrap()
            .is_some()
    );
    assert!(
        f.fill
            .dependencies
            .admission
            .used(ResourceClass::DirtyCiphertext)
            > 0
    );
}

#[test]
fn copy_only_miss_has_no_origin_side_effect_and_wrong_context_never_joins() {
    let mut f = fixture();
    assert!(
        drive(
            f.fill.copy_only(&f.page, &f.scope),
            &mut f.engine,
            &f.crypto
        )
        .unwrap()
        .is_none()
    );
    assert_eq!(f.origin.calls.get(), 0);
    f.context.object.key = CacheKey([1; 32]);
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
    assert!(matches!(
        drive(
            f.fill.acquire(
                f.page.clone(),
                f.membership.clone(),
                &f.context,
                &f.scope,
                &mut budget
            ),
            &mut f.engine,
            &f.crypto
        ),
        Err(Error::InvalidRequest)
    ));
    assert_eq!(budget.remaining_attempts(), 4);
    assert_eq!(f.origin.calls.get(), 0);
}

#[test]
fn sequential_full_pages_reclaim_idle_bytes_and_preserve_busy_reader_leases() {
    use crate::model::range::PAGE_BYTES;
    let mut limits = crate::test_support::cluster::config(false).limits;
    limits.plaintext_bytes = std::num::NonZeroUsize::new(2 * PAGE_BYTES as usize).unwrap();
    limits.ciphertext_bytes = std::num::NonZeroUsize::new(4 * (PAGE_BYTES as usize + 16)).unwrap();
    limits.dirty_bytes = std::num::NonZeroUsize::new(2 * (PAGE_BYTES as usize + 16)).unwrap();
    let mut f = fixture_with(8 * PAGE_BYTES, Some(limits.clone()));
    let mut pinned = None;
    for number in 0..8 {
        let page = PageId {
            version: f.page.version.clone(),
            number: PageNumber(number),
        };
        let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
        let result = drive(
            f.fill.acquire(
                page,
                f.membership.clone(),
                &f.context,
                &f.scope,
                &mut budget,
            ),
            &mut f.engine,
            &f.crypto,
        )
        .unwrap();
        assert_eq!(result.plaintext.bytes().len(), PAGE_BYTES as usize);
        assert_eq!(result.plaintext.bytes()[0], number as u8);
        if number == 0 {
            pinned = Some(result.plaintext.clone());
        }
        assert_eq!(pinned.as_ref().unwrap().bytes()[0], 0);
        drop(result);
        assert!(
            f.fill.dependencies.admission.used(ResourceClass::Plaintext)
                <= limits.plaintext_bytes.get()
        );
        assert!(
            f.fill
                .dependencies
                .admission
                .used(ResourceClass::Ciphertext)
                <= limits.ciphertext_bytes.get()
        );
        assert!(
            f.fill
                .dependencies
                .admission
                .used(ResourceClass::DirtyCiphertext)
                <= limits.dirty_bytes.get()
        );
    }
    assert_eq!(
        f.origin.calls.get(),
        8,
        "byte capacity must not permanently block idle-cache misses"
    );
    assert!(
        f.fill.dependencies.memory.get(&f.page).unwrap().is_some(),
        "independent reader protected its cached page"
    );
    drop(pinned);
    f.fill.dependencies.writer.discard_unsubmitted();
    f.fill.dependencies.memory.evict_idle(usize::MAX).unwrap();
    assert_eq!(
        f.fill.dependencies.admission.used(ResourceClass::Plaintext),
        0
    );
    assert_eq!(
        f.fill
            .dependencies
            .admission
            .used(ResourceClass::Ciphertext),
        0
    );
    assert_eq!(
        f.fill
            .dependencies
            .admission
            .used(ResourceClass::DirtyCiphertext),
        0
    );
}

#[test]
fn bootstrap_admission_discards_queued_copy_before_evicting_idle_bundle() {
    let mut limits = crate::test_support::cluster::config(false).limits;
    limits.plaintext_bytes =
        std::num::NonZeroUsize::new(crate::model::range::PAGE_BYTES as usize).unwrap();
    let mut f = fixture_with(3, Some(limits));
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
    let result = drive(
        f.fill.acquire(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut budget,
        ),
        &mut f.engine,
        &f.crypto,
    )
    .unwrap();
    drop(result);
    let dependencies = &f.fill.dependencies;
    assert_eq!(dependencies.writer.queued_count(), 1);
    assert_eq!(dependencies.memory.evict_idle(usize::MAX), Ok(0));
    let reservation = f.fill.reserve_bootstrap(&f.context.object.cache).unwrap();
    assert_eq!(dependencies.writer.discarded_count(), 1);
    assert_eq!(dependencies.writer.pending_count(), 0);
    assert!(dependencies.memory.get(&f.page).unwrap().is_none());
    assert_eq!(dependencies.admission.used(ResourceClass::Ciphertext), 0);
    assert_eq!(
        dependencies.admission.used(ResourceClass::DirtyCiphertext),
        0
    );
    assert_eq!(
        dependencies.admission.used(ResourceClass::Plaintext),
        reservation.amount()
    );
    drop(reservation);
    assert_eq!(dependencies.admission.used(ResourceClass::Plaintext), 0);
}

fn retained_page(
    f: &Fixture,
    cache: &CacheId,
    version: &str,
    class: ResourceClass,
    amount: usize,
    queued: bool,
) -> PageId {
    let admission = &f.fill.dependencies.admission;
    let mut descriptor = f.origin.metadata.immutable();
    descriptor.version.object.cache = cache.clone();
    descriptor.version.etag = StrongEtag::test_value(version);
    let id = PageId {
        version: descriptor.version.clone(),
        number: PageNumber(0),
    };
    let mut page = PageResult {
        metadata: descriptor.for_pin(),
        plaintext: crate::memory::pool::VerifiedPage {
            inner: Arc::new(crate::memory::pool::VerifiedBytes {
                page: id.clone(),
                bytes: vec![1; 3],
                reservation: admission
                    .reserve(Some(cache), ResourceClass::Plaintext, 3)
                    .unwrap(),
            }),
        },
        ciphertext: f
            .fill
            .dependencies
            .buffers
            .ciphertext(
                admission
                    .reserve(Some(cache), ResourceClass::Ciphertext, 19)
                    .unwrap(),
                crate::model::envelope::PageEnvelope {
                    page: id,
                    key_id: crate::model::envelope::KeyId([1; 16]),
                    nonce: crate::model::envelope::Nonce([2; 24]),
                    plaintext_length: 3,
                    ciphertext_length: 19,
                },
                vec![2; 19],
            )
            .unwrap(),
    };
    // Small final pages may retain full-page admission slack.
    let reservation = admission.reserve(Some(cache), class, amount).unwrap();
    match class {
        ResourceClass::Plaintext => {
            Arc::get_mut(&mut page.plaintext.inner).unwrap().reservation = reservation
        }
        ResourceClass::Ciphertext => {
            Arc::get_mut(&mut page.ciphertext.inner)
                .unwrap()
                .reservation = reservation
        }
        _ => panic!("page class required"),
    }
    let id = page.plaintext.page().clone();
    if queued {
        let dirty = admission
            .reserve(Some(cache), ResourceClass::DirtyCiphertext, 19)
            .unwrap();
        f.fill
            .dependencies
            .writer
            .enqueue(page.copy(), dirty)
            .unwrap();
    }
    f.fill.dependencies.memory.publish(page).unwrap();
    id
}

#[test]
fn fair_share_one_page_deficit_preserves_other_caches_and_remaining_working_set() {
    for class in [ResourceClass::Plaintext, ResourceClass::Ciphertext] {
        let amount = PAGE_BYTES as usize + 16;
        let mut limits = crate::test_support::cluster::config(false).limits;
        limits.plaintext_bytes = std::num::NonZeroUsize::new(6 * amount + 64).unwrap();
        limits.ciphertext_bytes = std::num::NonZeroUsize::new(6 * amount + 64).unwrap();
        let f = fixture_with(3, Some(limits));
        let cache = &f.context.object.cache;
        let queued = matches!(class, ResourceClass::Plaintext);
        let other = retained_page(&f, &CacheId("other".into()), "other", class, amount, queued);
        let first = retained_page(&f, cache, "first", class, amount, queued);
        let second = retained_page(&f, cache, "second", class, amount, queued);
        let third = retained_page(&f, cache, "third", class, amount, queued);
        let reservation = f
            .fill
            .reserve_with_reclamation(cache, class, amount)
            .unwrap();
        let deps = &f.fill.dependencies;
        assert!(deps.memory.get(&first).unwrap().is_none());
        for page in [&other, &second, &third] {
            assert!(deps.memory.get(page).unwrap().is_some());
            if queued {
                assert!(deps.writer.copy_only(page).unwrap().is_some());
            }
        }
        assert_eq!(deps.writer.discarded_count(), u64::from(queued));
        drop(reservation);
    }
}

#[test]
fn global_plaintext_deficit_counts_plaintext_not_combined_bundle_bytes() {
    let f = fixture();
    let cache = &f.context.object.cache;
    let first = retained_page(&f, cache, "first", ResourceClass::Plaintext, 3, false);
    let second = retained_page(&f, cache, "second", ResourceClass::Plaintext, 3, false);
    let third = retained_page(&f, cache, "third", ResourceClass::Plaintext, 3, false);
    let admission = &f.fill.dependencies.admission;
    let _pressure = admission
        .reserve(
            None,
            ResourceClass::Plaintext,
            admission.limit(ResourceClass::Plaintext) - 9,
        )
        .unwrap();
    let _reservation = f
        .fill
        .reserve_with_reclamation(cache, ResourceClass::Plaintext, 6)
        .unwrap();
    assert!(f.fill.dependencies.memory.get(&first).unwrap().is_none());
    assert!(f.fill.dependencies.memory.get(&second).unwrap().is_none());
    assert!(f.fill.dependencies.memory.get(&third).unwrap().is_some());
    // A full-page deficit cannot be remedied by the remaining short idle page.
    // Bounded failure does not spin on live charges.
    assert!(matches!(
        f.fill.reserve_bootstrap(cache),
        Err(Error::Overloaded)
    ));
    for page in [&first, &second, &third] {
        assert!(f.fill.dependencies.memory.get(page).unwrap().is_none());
    }
}

#[test]
fn dirty_only_pressure_skips_persistence_without_flushing_memory_or_queue() {
    let f = fixture();
    let cache = &f.context.object.cache;
    let page = retained_page(&f, cache, "queued", ResourceClass::Plaintext, 3, true);
    let deps = &f.fill.dependencies;
    let _pressure = deps
        .admission
        .reserve(
            None,
            ResourceClass::DirtyCiphertext,
            deps.admission.limit(ResourceClass::DirtyCiphertext) - 19,
        )
        .unwrap();
    let reservation = f.fill.reserve_progress(cache, true).unwrap();
    assert!(reservation.dirty.is_none());
    assert!(deps.memory.get(&page).unwrap().is_some());
    assert!(deps.writer.copy_only(&page).unwrap().is_some());
    assert_eq!(deps.writer.discarded_count(), 0);
}

#[test]
fn busy_leases_and_impossible_allocations_do_not_discard_queued_work() {
    let f = fixture();
    let cache = &f.context.object.cache;
    let first = retained_page(&f, cache, "first", ResourceClass::Plaintext, 3, true);
    let second = retained_page(&f, cache, "second", ResourceClass::Plaintext, 3, true);
    let deps = &f.fill.dependencies;
    let plaintext = deps.memory.get(&first).unwrap().unwrap().plaintext;
    let ciphertext = deps.memory.ciphertext(&second).unwrap().unwrap();
    let _pressure = deps
        .admission
        .reserve(
            None,
            ResourceClass::Plaintext,
            deps.admission.limit(ResourceClass::Plaintext) - 6,
        )
        .unwrap();
    assert!(matches!(
        f.fill.reserve_bootstrap(cache),
        Err(Error::Overloaded)
    ));
    assert_eq!(deps.writer.discarded_count(), 0);
    assert_eq!(deps.writer.pending_count(), 2);
    assert_eq!(plaintext.bytes(), &[1; 3]);
    assert_eq!(ciphertext.ciphertext.bytes(), &[2; 19]);
    drop((plaintext, ciphertext));
    assert!(matches!(
        f.fill
            .reserve_with_reclamation(cache, ResourceClass::Plaintext, usize::MAX),
        Err(Error::Overloaded)
    ));
    assert!(deps.memory.get(&first).unwrap().is_some());
    assert!(deps.memory.get(&second).unwrap().is_some());
    assert_eq!(deps.writer.discarded_count(), 0);
}

#[test]
fn ciphertext_staging_release_stops_before_evicting_unpinned_memory_bundle() {
    let f = fixture();
    let cache = &f.context.object.cache;
    let first = retained_page(&f, cache, "first", ResourceClass::Plaintext, 3, true);
    let second = retained_page(&f, cache, "second", ResourceClass::Plaintext, 3, true);
    let deps = &f.fill.dependencies;
    let _pressure = deps
        .admission
        .reserve(
            None,
            ResourceClass::Ciphertext,
            deps.admission.limit(ResourceClass::Ciphertext)
                - deps.admission.used(ResourceClass::Ciphertext),
        )
        .unwrap();
    let _reservation = f
        .fill
        .reserve_with_reclamation(cache, ResourceClass::Ciphertext, 1)
        .unwrap();
    assert_eq!(deps.writer.discarded_count(), 1);
    assert!(deps.writer.copy_only(&first).unwrap().is_none());
    assert!(deps.writer.copy_only(&second).unwrap().is_some());
    assert!(deps.memory.get(&first).unwrap().is_some());
    assert!(deps.memory.get(&second).unwrap().is_some());
}

#[test]
fn rejected_origin_supplier_does_not_fail_an_independent_coalesced_reader() {
    let mut f = fixture();
    f.origin.reject_once.set(true);
    let mut a = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
    let mut b = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
    let reads = futures::future::join(
        f.fill.acquire(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut a,
        ),
        f.fill.acquire(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut b,
        ),
    );
    let (first, second) = drive(reads, &mut f.engine, &f.crypto);
    assert!(matches!(first, Err(Error::OriginForbidden)));
    assert_eq!(second.unwrap().plaintext.bytes(), b"abc");
    assert_eq!(f.origin.calls.get(), 2);
    assert_eq!(a.remaining_attempts(), 3);
    assert_eq!(b.remaining_attempts(), 3);
}

#[test]
fn canceled_supplier_retains_crypto_fence_before_replacement_origin_work() {
    let mut f = fixture();
    let mut a = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
    let second_scope = RequestScope::new(RequestId([2; 16]), f.scope.deadline.0).unwrap();
    let mut b = AcquisitionBudget::new(second_scope.deadline.0, 4, 8);
    let mut first = f.fill.acquire(
        f.page.clone(),
        f.membership.clone(),
        &f.context,
        &f.scope,
        &mut a,
    );
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(first.as_mut().poll(&mut cx).is_pending());
    assert_eq!(f.origin.calls.get(), 1);
    super::super::drivers::poll(&mut cx, 64);
    assert_eq!(
        f.crypto.outstanding(),
        1,
        "origin bytes accepted for crypto before cancellation"
    );
    drop(first);
    assert_eq!(f.fill.metrics.gauge(Gauge::ActiveFills), 1);
    let mut second = f.fill.acquire(
        f.page.clone(),
        f.membership.clone(),
        &f.context,
        &second_scope,
        &mut b,
    );
    assert!(second.as_mut().poll(&mut cx).is_pending());
    assert_eq!(
        f.origin.calls.get(),
        1,
        "retry cannot overlap retained crypto completion"
    );
    // Reap no crypto here: a cancellation notification must not stand in for its
    // accepted completion even when the detached read driver is polled again.
    super::super::drivers::poll(&mut cx, 64);
    assert!(second.as_mut().poll(&mut cx).is_pending());
    assert_eq!(
        f.origin.calls.get(),
        1,
        "accepted crypto must fence replacement election"
    );
    let result = drive(second, &mut f.engine, &f.crypto).unwrap();
    assert_eq!(result.plaintext.bytes(), b"abc");
    assert_eq!(f.origin.calls.get(), 2);
    assert_eq!(f.fill.metrics.gauge(Gauge::ActiveFills), 0);
    assert_eq!(f.fill.metrics.count(Event::OriginFill), 1);
}

#[test]
fn sequential_full_pages_reclaim_idle_bytes_but_preserve_independent_reader() {
    use crate::model::range::PAGE_BYTES;
    use std::num::NonZeroUsize;
    let mut limits = crate::test_support::cluster::config(false).limits;
    limits.plaintext_bytes = NonZeroUsize::new(2 * PAGE_BYTES as usize).unwrap();
    limits.ciphertext_bytes = NonZeroUsize::new(3 * (PAGE_BYTES as usize + 16)).unwrap();
    limits.dirty_bytes = NonZeroUsize::new(PAGE_BYTES as usize + 16).unwrap();
    let mut f = fixture_with(8 * PAGE_BYTES, Some(limits.clone()));
    let mut held = None;
    for number in 0..8 {
        let mut page = f.page.clone();
        page.number = PageNumber(number);
        let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
        let result = drive(
            f.fill.acquire(
                page,
                f.membership.clone(),
                &f.context,
                &f.scope,
                &mut budget,
            ),
            &mut f.engine,
            &f.crypto,
        )
        .unwrap();
        assert_eq!(result.plaintext.bytes().len(), PAGE_BYTES as usize);
        assert!(
            result
                .plaintext
                .bytes()
                .iter()
                .all(|byte| *byte == number as u8)
        );
        if number == 0 {
            held = Some(result.plaintext.clone());
        }
        assert_eq!(held.as_ref().unwrap().bytes()[0], 0);
        assert!(
            f.fill.dependencies.admission.used(ResourceClass::Plaintext)
                <= limits.plaintext_bytes.get()
        );
        assert!(
            f.fill
                .dependencies
                .admission
                .used(ResourceClass::Ciphertext)
                <= limits.ciphertext_bytes.get()
        );
        assert!(
            f.fill
                .dependencies
                .admission
                .used(ResourceClass::DirtyCiphertext)
                <= limits.dirty_bytes.get()
        );
        drop(result);
        f.fill.dependencies.flights.poll_budgeted(64).unwrap();
    }
    assert_eq!(f.origin.calls.get(), 8);
    drop(held);
    f.fill.dependencies.writer.discard_unsubmitted();
    f.fill.dependencies.memory.evict_idle(usize::MAX).unwrap();
    assert_eq!(
        f.fill.dependencies.admission.used(ResourceClass::Plaintext),
        0
    );
    assert_eq!(
        f.fill
            .dependencies
            .admission
            .used(ResourceClass::Ciphertext),
        0
    );
    assert_eq!(
        f.fill
            .dependencies
            .admission
            .used(ResourceClass::DirtyCiphertext),
        0
    );
}

#[test]
fn sequential_full_pages_reclaim_idle_bytes_and_preserve_a_busy_reader() {
    use crate::model::range::PAGE_BYTES;
    use std::num::NonZeroUsize;
    let mut limits = crate::test_support::cluster::config(false).limits;
    limits.plaintext_bytes = NonZeroUsize::new(2 * PAGE_BYTES as usize).unwrap();
    limits.ciphertext_bytes = NonZeroUsize::new(3 * (PAGE_BYTES as usize + 16)).unwrap();
    limits.dirty_bytes = NonZeroUsize::new(2 * (PAGE_BYTES as usize + 16)).unwrap();
    // Byte pressure must occur far earlier than the entry-count eviction policy.
    limits.metadata_entries = NonZeroUsize::new(128).unwrap();
    let mut f = fixture_with(12 * PAGE_BYTES, Some(limits));
    let mut held = None;
    for number in 0..12 {
        let page = PageId {
            version: f.page.version.clone(),
            number: PageNumber(number),
        };
        let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
        let result = drive(
            f.fill.acquire(
                page,
                f.membership.clone(),
                &f.context,
                &f.scope,
                &mut budget,
            ),
            &mut f.engine,
            &f.crypto,
        )
        .unwrap();
        assert_eq!(result.plaintext.bytes().len(), PAGE_BYTES as usize);
        assert_eq!(result.plaintext.bytes()[0], number as u8);
        if number == 0 {
            held = Some(result);
        } else {
            drop(result);
        }
        let admission = &f.fill.dependencies.admission;
        for class in [
            ResourceClass::Plaintext,
            ResourceClass::Ciphertext,
            ResourceClass::DirtyCiphertext,
        ] {
            assert!(admission.used(class) <= admission.limit(class));
        }
        assert_eq!(held.as_ref().unwrap().plaintext.bytes()[0], 0);
    }
    assert_eq!(f.origin.calls.get(), 12);
    drop(held);
    f.fill.dependencies.writer.discard_unsubmitted();
    f.fill.dependencies.flights.poll_budgeted(128).unwrap();
    f.fill.dependencies.memory.evict_idle(usize::MAX).unwrap();
    assert_eq!(
        f.fill.dependencies.admission.used(ResourceClass::Plaintext),
        0
    );
    assert_eq!(
        f.fill
            .dependencies
            .admission
            .used(ResourceClass::Ciphertext),
        0
    );
    assert_eq!(
        f.fill
            .dependencies
            .admission
            .used(ResourceClass::DirtyCiphertext),
        0
    );
}
