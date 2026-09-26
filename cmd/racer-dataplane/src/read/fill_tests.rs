use super::*;
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
    let mut config = crate::test_support::cluster::config(false);
    if let Some(limits) = limits {
        config.limits = limits;
    }
    let worker = WorkerId(0);
    let admission = Rc::new(Admission::new(config.limits.clone()));
    let buffers = Rc::new(BufferPool::new(admission.clone()));
    let memory = Rc::new(MemoryCache::new(buffers.clone()));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let index = Rc::new(Index::new(worker, 16));
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
    let writer = Rc::new(StoreWriter::new(index, segments, slabs));
    let keys = Rc::new(crate::security::keyring::tests::keys());
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
    let fill = Fill::new(FillDependencies {
        memory,
        buffers,
        disk,
        writer,
        peers,
        origin: origin.clone(),
        candidates,
        flights: Rc::new(Flights::new(admission.clone())),
        crypto: Rc::new(PageCrypto::new(keys, crypto.clone())),
        credentials,
        admission,
        metadata_owner,
    });
    Fixture {
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
fn consumed_peer_output_reacquires_for_origin_and_honors_cancellation() {
    struct FailedBody {
        cancel: bool,
        calls: Cell<usize>,
    }
    impl PeerClient for FailedBody {
        fn request<'a>(
            &'a self,
            _: crate::peer::wire::PeerRequest,
            _: &'a RequestScope,
        ) -> Operation<'a, crate::peer::wire::VerifiedResponse> {
            Box::pin(async { panic!("Fill must pass its output") })
        }
        fn request_reserved<'a>(
            &'a self,
            _: crate::peer::wire::PeerRequest,
            scope: &'a RequestScope,
            output: &'a mut Option<Reservation>,
        ) -> Operation<'a, crate::peer::wire::VerifiedResponse> {
            Box::pin(async move {
                self.calls.set(self.calls.get() + 1);
                drop(output.take().expect("admitted output"));
                if self.cancel {
                    scope.cancel()?;
                }
                Err(Error::Io)
            })
        }
    }
    for cancel in [false, true] {
        let mut f = fixture();
        let local = f.membership.members()[0].node.clone();
        let mut members = f.membership.members().to_vec();
        members.push(Member {
            node: crate::model::identity::NodeId("44444444-4444-4444-8444-444444444444".into()),
            shares: NonZeroU32::new(4).unwrap(),
            peer_endpoint: "127.0.0.1:8001".into(),
            rails: vec![],
            alignment_enabled: false,
        });
        f.membership = Arc::new(Membership::validate(f.membership.version, members).unwrap());
        let placement = Rc::new(Placement::new(256));
        let key = (0..256u16)
            .map(|n| CacheKey([n as u8; 32]))
            .find(|key| {
                let object = ObjectId {
                    cache: f.context.object.cache.clone(),
                    key: *key,
                };
                placement
                    .rank(f.membership.clone(), &object, PageNumber(0))
                    .unwrap()
                    .ordered[1]
                    == local
            })
            .unwrap();
        f.context.object.key = key;
        f.page.version.object.key = key;
        // The origin descriptor must name the same immutable object.
        let origin = Rc::new(TestOrigin {
            buffers: f.origin.buffers.clone(),
            calls: Cell::new(0),
            metadata: ObjectMetadata {
                version: f.page.version.clone(),
                ..f.origin.metadata.clone()
            },
            reject_once: Cell::new(false),
        });
        f.fill.dependencies.origin = origin.clone();
        let peers = Rc::new(FailedBody {
            cancel,
            calls: Cell::new(0),
        });
        let candidates = Rc::new(CandidatePolicy::new(local, placement, peers.clone()));
        candidates.set_credentials(f.fill.dependencies.credentials.clone());
        f.fill.dependencies.candidates = candidates;
        let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 16);
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
        );
        assert_eq!(peers.calls.get(), 1);
        if cancel {
            assert!(matches!(result, Err(Error::Cancelled)));
            assert_eq!(origin.calls.get(), 0);
        } else {
            assert_eq!(result.unwrap().plaintext.bytes(), b"abc");
            assert_eq!(origin.calls.get(), 1);
        }
        f.fill.dependencies.writer.discard_unsubmitted();
        f.fill.dependencies.memory.evict_idle(usize::MAX).unwrap();
        assert_eq!(
            f.fill
                .dependencies
                .admission
                .used(ResourceClass::Ciphertext),
            0
        );
    }
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
fn progress_pressure_waits_without_spinning_or_spending_and_honors_cancellation() {
    use crate::test_support::WakeCounter;
    use std::task::Waker;
    for cancel in [false, true] {
        let mut f = fixture();
        let admission = f.fill.dependencies.admission.clone();
        let held = admission
            .reserve(
                Some(&f.context.object.cache),
                ResourceClass::Ciphertext,
                admission.limit(ResourceClass::Ciphertext),
            )
            .unwrap();
        let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
        let mut future = Box::pin(f.fill.acquire_once(
            &f.page,
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut budget,
        ));
        let count = Arc::new(WakeCounter::default());
        let waker = Waker::from(count.clone());
        let mut cx = Context::from_waker(&waker);
        for _ in 0..4 {
            assert!(future.as_mut().poll(&mut cx).is_pending());
        }
        assert_eq!(count.count(), 0);
        assert_eq!(f.origin.calls.get(), 0);
        if cancel {
            f.scope.cancel().unwrap();
            assert!(count.count() > 0);
        }
        drop(held);
        let result = drive(future, &mut f.engine, &f.crypto);
        if cancel {
            assert!(matches!(result, Err(Error::Cancelled)));
            assert_eq!(budget.remaining_attempts(), 4);
        } else {
            assert_eq!(result.unwrap().plaintext.bytes(), b"abc");
            assert_eq!(f.origin.calls.get(), 1);
            assert_eq!(budget.remaining_attempts(), 3);
        }
    }
}

#[test]
fn progress_pressure_preserves_earlier_acquisition_deadline() {
    let f = fixture();
    let admission = &f.fill.dependencies.admission;
    let _held = admission
        .reserve(
            Some(&f.context.object.cache),
            ResourceClass::Ciphertext,
            admission.limit(ResourceClass::Ciphertext),
        )
        .unwrap();
    let due = Instant::now() + Duration::from_millis(20);
    let mut budget = AcquisitionBudget::new(due, 4, 8);
    let mut future = Box::pin(f.fill.acquire_once(
        &f.page,
        f.membership.clone(),
        &f.context,
        &f.scope,
        &mut budget,
    ));
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(future.as_mut().poll(&mut cx).is_pending());
    std::thread::sleep(due.saturating_duration_since(Instant::now()));
    assert!(matches!(
        future.as_mut().poll(&mut cx),
        Poll::Ready(Err(Error::DeadlineExceeded))
    ));
    drop(future);
    assert_eq!(budget.remaining_attempts(), 4);
    assert_eq!(budget.remaining_links(), 8);
    assert_eq!(f.origin.calls.get(), 0);
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
fn admitted_bootstrap_waits_for_ciphertext_and_preserves_scope() {
    for outcome in ["release", "cancel", "deadline"] {
        let mut f = fixture();
        let admission = f.fill.dependencies.admission.clone();
        let held = admission
            .reserve(
                None,
                ResourceClass::Ciphertext,
                admission.limit(ResourceClass::Ciphertext),
            )
            .unwrap();
        let reservation = admission
            .reserve(
                Some(&f.context.object.cache),
                ResourceClass::Plaintext,
                crate::model::range::PAGE_BYTES as usize,
            )
            .unwrap();
        let mut plaintext = f
            .fill
            .dependencies
            .buffers
            .plaintext(reservation, 3)
            .unwrap();
        plaintext.bytes_mut().unwrap().copy_from_slice(b"abc");
        let origin = OriginPage {
            metadata: f.origin.metadata.clone(),
            plaintext,
        };
        let mut scope = f.scope.clone();
        if outcome == "deadline" {
            scope.deadline.0 = Instant::now() + Duration::from_millis(20);
        }
        let deadline = scope.deadline;
        let mut future =
            Box::pin(
                f.fill
                    .admit_bootstrap(origin, &f.page, f.membership.clone(), &scope),
            );
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(future.as_mut().poll(&mut cx).is_pending(), "{outcome}");
        assert_eq!(f.crypto.outstanding(), 0);
        assert_eq!(admission.used(ResourceClass::Ciphertext), held.amount());
        if outcome == "release" {
            drop(held);
            let result = drive(future, &mut f.engine, &f.crypto).unwrap();
            assert_eq!(result.plaintext.bytes(), b"abc");
            assert_eq!(scope.deadline.0, deadline.0);
            drop(result);
        } else {
            if outcome == "cancel" {
                scope.cancel().unwrap();
            } else {
                std::thread::sleep(Duration::from_millis(25));
            }
            let result = drive(future, &mut f.engine, &f.crypto);
            assert!(
                matches!(result, Err(error) if error == if outcome == "cancel" { Error::Cancelled } else { Error::DeadlineExceeded })
            );
            drop(held);
        }
        f.fill.dependencies.writer.discard_unsubmitted();
        f.fill.dependencies.memory.evict_idle(usize::MAX).unwrap();
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
    }
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
