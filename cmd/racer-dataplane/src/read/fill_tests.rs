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
    cell::Cell,
    future::Future,
    num::NonZeroU32,
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
            let mut plaintext = self.buffers.plaintext(reservation, 3)?;
            plaintext.bytes_mut()?.copy_from_slice(b"abc");
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
}
fn fixture() -> Fixture {
    let config = crate::test_support::cluster::config(false);
    let worker = WorkerId(0);
    let admission = Rc::new(Admission::new(config.limits.clone()));
    let buffers = Rc::new(BufferPool::new(admission.clone()));
    let memory = Rc::new(MemoryCache::new(buffers.clone()));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let index = Rc::new(Index::new(worker, 16));
    let segments = Rc::new(Segments::new(worker, 64 * 1024 * 1024));
    let slabs = Rc::new(Slabs::new(
        worker,
        "unused-fill-test".into(),
        reactor,
        1024 * 1024 * 1024,
        64 * 1024 * 1024,
    ));
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
        length: 3,
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
    let directory = Arc::new(
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
        metadata_owner: directory,
    });
    Fixture {
        fill,
        origin,
        crypto,
        engine: PageCryptoEngine::new(CryptoRuntime { port: engine }),
        context,
        membership,
        page,
        scope: RequestScope::new(RequestId([1; 16]), Instant::now() + Duration::from_secs(30))
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
    let result = drive(second, &mut f.engine, &f.crypto).unwrap();
    assert_eq!(result.plaintext.bytes(), b"abc");
    assert_eq!(f.origin.calls.get(), 2);
}
