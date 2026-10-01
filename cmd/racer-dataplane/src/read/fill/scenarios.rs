use super::*;
use crate::peer::PeerClient;
mod hot_reads;
mod metadata;
mod peer_copies;
mod pressure;
use crate::{
    model::{
        CacheId, CacheKey, ExpiresAt, ObjectId, ObjectVersion, PageNumber, RequestId,
        ResourceClass, StrongEtag, WorkerId,
    },
    origin::{metadata::MetadataReply, page::OriginPage},
    read::dispatch::WorkerDirectory,
    runtime::{
        crypto::{self, CryptoClient},
        reactor::{IoBuffer, Reactor},
        worker::{CryptoRuntime, CryptoService, WorkerMap},
    },
    security::aead::PageCryptoEngine,
    store::{
        catalog::{Index, SegmentClock, Segments},
        disk::Slabs,
    },
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

use crate::test_support::NoPeers as NoPeer;
struct TestOrigin {
    buffers: BufferPool,
    calls: Cell<usize>,
    metadata: ObjectMetadata,
    reject_once: Cell<bool>,
    version_unavailable: Cell<bool>,
    blocked_pages: RefCell<std::collections::BTreeSet<u64>>,
    started_pages: RefCell<Vec<u64>>,
}
impl Origin for TestOrigin {
    fn bootstrap_reserved<'a>(
        &'a self,
        _: &'a super::super::candidates::OriginAuthority,
        _: &'a OriginContext,
        _: Reservation,
        _: &'a RequestScope,
    ) -> Operation<'a, MetadataReply> {
        Box::pin(async { panic!("pinned page fill must not bootstrap metadata") })
    }
    fn metadata<'a>(
        &'a self,
        _: &'a super::super::candidates::OriginAuthority,
        _: &'a OriginContext,
        _: crate::model::MetadataSelector,
        _: &'a RequestScope,
    ) -> Operation<'a, MetadataReply> {
        Box::pin(async { panic!("pinned page fill must not refresh metadata") })
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
            self.started_pages.borrow_mut().push(page.number.0);
            std::future::poll_fn(|cx| {
                if self.blocked_pages.borrow().contains(&page.number.0) {
                    cx.waker().wake_by_ref();
                    Poll::Pending
                } else {
                    Poll::Ready(())
                }
            })
            .await;
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
    reactor: Rc<Reactor>,
    keys: Rc<crate::security::identity::Keyring>,
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
fn fixture_with(length: u64, limits: Option<crate::model::Limits>) -> Fixture {
    fixture_with_availability(length, limits, false)
}
fn fixture_with_availability(
    length: u64,
    limits: Option<crate::model::Limits>,
    check_availability: bool,
) -> Fixture {
    let mut config = crate::test_support::cluster::config(false);
    if let Some(limits) = limits {
        config.limits = limits;
    }
    let worker = WorkerId(0);
    let admission = Rc::new(Admission::new(config.limits.clone()));
    let keys = Rc::new(crate::security::identity::keyring_tests::keys());
    let availability = crate::control::state::for_caches(
        keys.clone(),
        vec![CacheId(crate::security::identity::tests::CACHE.into())],
    );
    let buffers = BufferPool::new(admission.clone());
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
        reactor.clone(),
        admission.clone(),
        1024 * 1024 * 1024,
        64 * 1024 * 1024,
    ));
    slabs
        .open_now()
        .expect("read fixture filesystem supports direct slab alignment");
    let clock = Rc::new(SegmentClock::new(index.clone(), segments.clone(), 1));
    let metrics = Metrics::default();
    let disk = Rc::new(
        StoreReader::new(
            clock,
            index.clone(),
            segments.clone(),
            slabs.clone(),
            buffers.clone(),
        )
        .with_metrics(metrics.clone()),
    );
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
    let node = crate::model::NodeId("22222222-2222-4222-8222-222222222222".into());
    let membership = Arc::new(
        Membership::validate(
            crate::model::MembershipVersion(1),
            vec![Member {
                node: node.clone(),
                shares: NonZeroU32::new(4).unwrap(),
                peer_endpoint: "127.0.0.1:8000".into(),
                rails: vec![],
                alignment_enabled: false,
                site: String::new(),
            }],
        )
        .unwrap(),
    );
    let metadata = ObjectMetadata {
        content_type: None,
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
        blocked_pages: RefCell::new(Default::default()),
        started_pages: RefCell::new(Vec::new()),
    });
    let (port, engine) = crypto::pair(worker, 0, config.limits.queue_entries);
    let crypto = Rc::new(CryptoClient::new(port));
    let credentials = Rc::new(CredentialCrypto::new(keys.clone(), admission.clone()));
    let peers = Rc::new(NoPeer);
    let candidates = Rc::new(CandidatePolicy::new(
        node,
        Rc::new(Placement::new(16)),
        peers.clone(),
        credentials.clone(),
        Arc::new(Default::default()),
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
    let flights = Rc::new(Flights::new(
        admission.clone(),
        if check_availability {
            availability
        } else {
            crate::control::state::Availability::permissive_for_tests()
        },
    ));
    let fill = Fill::new(FillDependencies {
        memory,
        buffers,
        disk,
        writer,
        origin: origin.clone(),
        candidates,
        flights,
        crypto: Rc::new(PageCrypto::new(keys.clone(), crypto.clone())),
        credentials,
        admission,
        metadata_owner,
    })
    .with_metrics(metrics);
    Fixture {
        keys,
        reactor,
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

fn adapter_client(
    f: &Fixture,
    adapter: &crate::test_support::origin::AdapterOrigin,
) -> Rc<crate::origin::OriginClient> {
    use crate::control::{
        state::{PublishedState, SnapshotStore},
        wire::{Publication, PublicationSequence, SCHEMA_VERSION},
    };
    let snapshots = Rc::new(SnapshotStore::new(
        f.keys.cluster().clone(),
        Arc::new(PublishedState::default()),
        2,
    ));
    let (client_socket, origin_socket) =
        crate::control::state::canonical_socket_paths("fixture").unwrap();
    snapshots
        .publish(Publication {
            schema_version: SCHEMA_VERSION,
            cluster: f.keys.cluster().clone(),
            sequence: PublicationSequence(1),
            membership_version: f.membership.version,
            members: f.membership.members().to_vec(),
            caches: vec![crate::control::state::CacheDefinition {
                id: f.context.object.cache.clone(),
                name: "fixture".into(),
                client_socket,
                origin_socket,
            }],
        })
        .unwrap();
    adapter.client(
        snapshots,
        f.fill.dependencies.admission.clone(),
        f.reactor.clone(),
        f.fill.dependencies.buffers.clone(),
    )
}

fn drive_io<T>(
    future: impl Future<Output = T>,
    reactor: &Reactor,
    engine: &mut PageCryptoEngine,
    crypto: &CryptoClient,
) -> T {
    let mut future = std::pin::pin!(future);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let deadline = Instant::now() + Duration::from_secs(10);
    loop {
        super::super::drivers::poll(&mut cx, 64);
        engine.poll_budgeted(64).unwrap();
        crypto.poll_budgeted(64).unwrap();
        if let Poll::Ready(value) = future.as_mut().poll(&mut cx) {
            return value;
        }
        assert!(Instant::now() < deadline, "read fixture made no progress");
        reactor.poll_budgeted(128).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
}

#[test]
fn abandoned_acquisition_does_not_cancel_shared_peer_scope() {
    abandoned_acquisition_preserves_peer_scope(false);
}

#[test]
fn abandoned_metadata_does_not_cancel_shared_peer_scope() {
    abandoned_acquisition_preserves_peer_scope(true);
}

fn abandoned_acquisition_preserves_peer_scope(metadata: bool) {
    use crate::{
        model::MetadataSelector,
        read::metadata::{MetadataDependencies, MetadataService},
    };
    {
        let mut f = fixture();
        let queue = Rc::new(crate::read::drivers::DriverQueue::default());
        let _queue = queue.enter();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 16);
        if metadata {
            let (send, receive) = futures::channel::oneshot::channel();
            let service = MetadataService::new(
                f.fill.dependencies.candidates.clone(),
                Rc::new(metadata::GatedMetadataOrigin {
                    buffers: f.origin.buffers.clone(),
                    receive: RefCell::new(Some(receive)),
                    calls: Cell::new(0),
                }),
                f.fill.dependencies.credentials.clone(),
                4,
                MetadataDependencies {
                    index: Rc::new(Index::new(WorkerId(0), 4)),
                    owners: f.fill.dependencies.metadata_owner.clone(),
                    fill: Rc::new(Fill::new(f.fill.dependencies.clone())),
                },
            );
            let mut read = service.resolve(
                MetadataSelector::Fresh,
                f.membership.clone(),
                &f.context,
                &f.scope,
            );
            assert!(read.as_mut().poll(&mut cx).is_pending());
            drop(read);
            send.send(MetadataReply {
                metadata: f.origin.metadata.clone(),
                page_zero: None,
            })
            .ok()
            .unwrap();
            queue.poll(&mut cx, 64);
        } else {
            let mut read = f.fill.acquire(
                f.page.clone(),
                f.membership.clone(),
                &f.context,
                &f.scope,
                &mut budget,
            );
            assert!(read.as_mut().poll(&mut cx).is_pending());
            drop(read);
            queue.poll(&mut cx, 64);
        }
        assert_eq!(
            f.scope.check(),
            Ok(()),
            "abandoning metadata={metadata} must not poison worker peer ingress"
        );
        assert_eq!(queue.pending(), 0);
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
        )
        .unwrap();
        assert_eq!(result.plaintext.bytes(), b"abc");
    }
}

#[test]
fn completed_fill_waits_release_shared_cancellation_capacity() {
    use futures::{StreamExt, stream::FuturesUnordered};
    let mut f = fixture();
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _queue = queue.enter();
    // Force a terminal fill failure without any accepted I/O. Every cohort must
    // release its waiter and driver before the same long-lived scope is reused.
    let admission = &f.fill.dependencies.admission;
    let pressure = admission
        .reserve(
            None,
            ResourceClass::Plaintext,
            admission.limit(ResourceClass::Plaintext),
        )
        .unwrap();
    for round in 0..1100 {
        let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
        let mut pending = FuturesUnordered::new();
        pending.push(f.fill.acquire(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            &mut budget,
        ));
        assert!(
            matches!(
                drive(pending.next(), &mut f.engine, &f.crypto),
                Some(Err(Error::Overloaded))
            ),
            "pressure round {round}"
        );
        drop(pending);
        assert_eq!(queue.pending(), 0);
        assert_eq!(admission.used(ResourceClass::Flight), 0);
        assert_eq!(admission.used(ResourceClass::Waiter), 0);
    }
    drop(pressure);
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
    let mut pending = FuturesUnordered::new();
    pending.push(f.fill.acquire(
        f.page.clone(),
        f.membership.clone(),
        &f.context,
        &f.scope,
        &mut budget,
    ));
    let result = drive(pending.next(), &mut f.engine, &f.crypto).unwrap();
    assert!(
        result.is_ok(),
        "released pressure must recover without replacing scope: {:?}",
        result.as_ref().err()
    );
    assert_eq!(result.unwrap().plaintext.bytes(), b"abc");
}

#[test]
fn selected_owner_reclaims_foreign_receive_charges_across_full_pages() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    use crate::model::PAGE_BYTES;
    use std::num::NonZeroUsize;
    let mut limits = crate::test_support::cluster::config(false).limits;
    limits.plaintext_bytes = NonZeroUsize::new(2 * PAGE_BYTES as usize).unwrap();
    limits.ciphertext_bytes = NonZeroUsize::new(3 * (PAGE_BYTES as usize + 16)).unwrap();
    let mut source = fixture_with(8 * PAGE_BYTES, Some(limits.clone()));
    let mut target = fixture_with(8 * PAGE_BYTES, Some(limits));
    for number in 0..8 {
        let page = PageId {
            version: source.page.version.clone(),
            number: PageNumber(number),
        };
        let mut budget = AcquisitionBudget::new(source.scope.deadline.0, 8, 16);
        let received = drive(
            source.fill.acquire(
                page.clone(),
                source.membership.clone(),
                &source.context,
                &source.scope,
                &mut budget,
            ),
            &mut source.engine,
            &source.crypto,
        )
        .unwrap();
        // Hold an alias to model a transport completion owner. Rehoming must not
        // revoke its bytes/charge, even when a copy is necessary.
        let canceled = RequestScope::new(target.scope.request, target.scope.deadline.0).unwrap();
        canceled.cancel().unwrap();
        assert!(matches!(
            drive(
                target.fill.accept_selected(received.copy(), &canceled),
                &mut target.engine,
                &target.crypto
            ),
            Err(Error::Cancelled)
        ));
        assert_eq!(target.fill.metrics.count(Event::PeerHit), number);
        let copy = received.copy();
        let result = drive(
            target.fill.accept_selected(copy, &target.scope),
            &mut target.engine,
            &target.crypto,
        )
        .unwrap();
        assert!(
            target
                .fill
                .dependencies
                .admission
                .owns(&result.plaintext.inner.reservation)
        );
        assert!(
            target
                .fill
                .dependencies
                .admission
                .owns(&result.ciphertext.inner.reservation)
        );
        assert_eq!(result.ciphertext.bytes(), received.ciphertext.bytes());
        assert_eq!(result.plaintext.bytes()[0], number as u8);
        assert_eq!(target.fill.metrics.count(Event::PeerHit), number + 1);
        assert_eq!(target.fill.metrics.count(Event::OriginFill), 0);
        drop((received, result));
        source.fill.dependencies.flights.poll_budgeted(128).unwrap();
        source.fill.dependencies.writer.discard_unsubmitted();
        source
            .fill
            .dependencies
            .memory
            .remove_cache(&source.context.object.cache)
            .unwrap();
        source.fill.dependencies.admission.reclaim_buffers();
        assert_eq!(
            source
                .fill
                .dependencies
                .admission
                .used(ResourceClass::Ciphertext),
            0
        );
        // Previously this source reservation failed while the remote owner held
        // an idle page. No target eviction should be required to free the source.
        assert!(
            source
                .fill
                .reserve_bootstrap(&source.context.object.cache)
                .is_ok()
        );
        assert!(
            target
                .fill
                .dependencies
                .memory
                .get(&page)
                .unwrap()
                .is_some()
        );
    }
    assert_eq!(source.origin.calls.get(), 8);
    assert_eq!(target.origin.calls.get(), 0);
}

#[test]
fn ordinary_publication_rejects_foreign_worker_charges() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    let mut source = fixture();
    let target = fixture();
    let mut budget = AcquisitionBudget::new(source.scope.deadline.0, 8, 16);
    let page = drive(
        source.fill.acquire(
            source.page.clone(),
            source.membership.clone(),
            &source.context,
            &source.scope,
            &mut budget,
        ),
        &mut source.engine,
        &source.crypto,
    )
    .unwrap();
    assert!(matches!(
        target.fill.dependencies.memory.publish(page.clone()),
        Err(Error::InvalidConfiguration)
    ));
    assert!(
        target
            .fill
            .dependencies
            .memory
            .get(&source.page)
            .unwrap()
            .is_none()
    );
    assert_eq!(
        target
            .fill
            .dependencies
            .admission
            .used(ResourceClass::Plaintext),
        0
    );
    assert!(
        source
            .fill
            .dependencies
            .admission
            .used(ResourceClass::Plaintext)
            >= 3
    );
}

#[test]
fn cached_corrupt_ciphertext_falls_back_without_exposing_plaintext() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
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
    f.fill
        .dependencies
        .memory
        .publish_ciphertext(UnverifiedPage {
            copy,
            disk_token: None,
        })
        .unwrap();
    let metrics = Metrics::default();
    f.fill.metrics = metrics.clone();
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
    assert_eq!(result.plaintext.bytes(), b"abc");
    assert_eq!(
        metrics.count(Event::PageDecrypt),
        2,
        "bad cached copy then retained original"
    );
    assert_eq!(f.origin.calls.get(), 1);
}

#[test]
fn ciphertext_ready_promotes_once_for_concurrent_plaintext_readers() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
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
    // The elected promotion returns its transferred budget intact. Neither a
    // ciphertext-only reader nor a coalesced plaintext reader starts a new route.
    for budget in [&cipher_budget, &first_budget, &second_budget] {
        assert_eq!(budget.remaining_attempts(), 8);
        assert_eq!(budget.remaining_links(), 8);
        assert_eq!(budget.deadline(), f.scope.deadline.0);
    }
    assert_eq!(metrics.gauge(Gauge::ActiveFills), 0);
    assert_eq!(super::super::drivers::pending(), 0);
    drop(holder);
}

#[test]
fn retired_completed_flight_misses_new_callers_but_admitted_waiters_finish() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    use crate::security::identity::{KeyPurpose, keyring_tests::rotation_bundle};
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
            client::{ClientRequest, ReadKind},
            control::{
                state::CacheDefinition,
                state::{PublishedState, SnapshotStore},
                wire::*,
            },
            memory::{delivery::Delivery, pipe::PipePool},
            model::ByteRange,
            read::{
                Coordinator, ReadService,
                metadata::{MetadataDependencies, MetadataService},
                range_stream::RangeStreams,
            },
        };
        let snapshots = Rc::new(SnapshotStore::new(
            f.keys.cluster().clone(),
            Arc::new(PublishedState::default()),
            2,
        ));
        let (client_socket, origin_socket) =
            crate::control::state::canonical_socket_paths("rotation").unwrap();
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
        let streams = Rc::new(RangeStreams::new(owners.clone(), delivery, 1));
        let coordinator = Rc::new(Coordinator::new(
            snapshots,
            metadata,
            fill.clone(),
            streams,
            fill.dependencies.credentials.clone(),
            crate::control::state::Availability::permissive_for_tests(),
        ));
        let mut endpoint = owners.install(WorkerId(0), coordinator.clone()).unwrap();
        for ordered in [false, true] {
            let kind = ReadKind::Subscription {
                pin: Some(f.page.version.etag.clone()),
                range: Some(ByteRange::Closed { first: 0, last: 2 }),
                page_credits: 1,
                byte_credits: crate::model::PAGE_BYTES,
                ordered,
            };
            let mut response = drive(
                coordinator.read(
                    ClientRequest {
                        kind,
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

#[test]
fn concurrent_readers_share_origin_encryption_and_pending_original_ciphertext() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
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
fn ciphertext_origin_fill_retains_verified_publication_without_a_plaintext_waiter() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    let mut f = fixture();
    f.context.authorization =
        Some(crate::model::Authorization::from_header(b"test-supplier-credential").unwrap());
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
        panic!("ciphertext registration")
    };
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 8);
    let copy = drive(
        f.fill.acquire_ciphertext(
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
    let retained = f.fill.dependencies.memory.get(&f.page).unwrap().unwrap();
    assert_eq!(retained.plaintext.bytes(), b"abc");
    assert!(Arc::ptr_eq(
        &retained.ciphertext.inner,
        &copy.ciphertext.inner
    ));
    assert!(
        f.fill
            .dependencies
            .memory
            .unverified(&f.page)
            .unwrap()
            .is_none()
    );
    assert!(f.fill.local_copies.borrow().is_empty());
    assert!(matches!(
        flights.join_copy(&f.page, &f.scope).unwrap(),
        JoinedCopy::Complete(_)
    ));
    drop(holder);
    // A different credential context can consume verified bytes, never the
    // supplier's authorization. No additional origin call or decrypt is needed.
    let context = OriginContext {
        object: f.context.object.clone(),
        metadata: None,
        authorization: None,
    };
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 0, 0);
    let result = drive(
        f.fill.acquire(
            f.page.clone(),
            f.membership.clone(),
            &context,
            &f.scope,
            &mut budget,
        ),
        &mut f.engine,
        &f.crypto,
    )
    .unwrap();
    assert!(Arc::ptr_eq(
        &retained.plaintext.inner,
        &result.plaintext.inner
    ));
    assert_eq!(f.origin.calls.get(), 1);
    assert_eq!(f.fill.metrics.count(Event::PageDecrypt), 0);
    assert_eq!(
        f.fill
            .dependencies
            .admission
            .used(ResourceClass::RequestContext),
        0
    );
}

fn drive_disk<T>(f: &Fixture, future: impl Future<Output = T>) -> T {
    let mut future = Box::pin(future);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    loop {
        f.scope.check().unwrap();
        super::super::drivers::poll(&mut cx, 64);
        if let Poll::Ready(result) = future.as_mut().poll(&mut cx) {
            return result;
        }
        f.reactor.poll_budgeted(64).unwrap();
        f.reactor.wait(Duration::from_millis(1)).unwrap();
    }
}

fn cold_disk_fixture() -> Fixture {
    let mut f = fixture();
    f.scope.deadline.0 = Instant::now() + Duration::from_secs(30);
    f.reactor.init().unwrap();
    futures::executor::block_on(f.fill.dependencies.writer.open()).unwrap();
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
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
    assert_eq!(
        drive_disk(&f, f.fill.dependencies.writer.progress(1, &f.scope)).unwrap(),
        1
    );
    drop(original);
    f.fill.dependencies.memory.evict_idle(usize::MAX).unwrap();
    assert!(
        f.fill
            .dependencies
            .memory
            .ciphertext(&f.page)
            .unwrap()
            .is_none()
    );
    assert!(
        f.fill
            .dependencies
            .writer
            .copy_only(&f.page)
            .unwrap()
            .is_none()
    );
    f.origin.calls.set(0);
    f
}

#[test]
fn concurrent_cold_disk_copy_only_shares_io_and_retains_original_ciphertext() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    let mut f = cold_disk_fixture();
    let disk_hits = f.fill.metrics.count(Event::DiskIndexLookupHit);
    let cipher_misses = f.fill.metrics.count(Event::CiphertextLookupMiss);
    let pending_misses = f.fill.metrics.count(Event::PendingLookupMiss);
    let context_baseline = f
        .fill
        .dependencies
        .admission
        .used(ResourceClass::RequestContext);
    let mut first = f.fill.copy_only(&f.page, &f.scope);
    let mut second = f.fill.copy_only(&f.page, &f.scope);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(first.as_mut().poll(&mut cx).is_pending());
    assert!(second.as_mut().poll(&mut cx).is_pending());
    assert_eq!(f.reactor.in_flight(), 1, "one cold disk submission");
    assert_eq!(f.fill.local_copies.borrow().len(), 1);
    let (first, second) = drive_disk(&f, futures::future::join(first, second));
    let (first, second) = (first.unwrap().unwrap(), second.unwrap().unwrap());
    assert!(Arc::ptr_eq(&first.1.inner, &second.1.inner));
    let retained = f
        .fill
        .dependencies
        .memory
        .unverified(&f.page)
        .unwrap()
        .unwrap();
    assert!(retained.disk_token.is_some());
    assert!(Arc::ptr_eq(&first.1.inner, &retained.copy.ciphertext.inner));
    assert!(f.fill.dependencies.memory.get(&f.page).unwrap().is_none());
    let hot = drive_disk(&f, f.fill.copy_only(&f.page, &f.scope))
        .unwrap()
        .unwrap();
    assert!(Arc::ptr_eq(&first.1.inner, &hot.1.inner));
    assert_eq!(f.fill.metrics.count(Event::DiskHit), 1);
    assert_eq!(
        f.fill.metrics.count(Event::DiskIndexLookupHit),
        disk_hits + 1
    );
    assert_eq!(
        f.fill.metrics.count(Event::CiphertextLookupMiss),
        cipher_misses + 2
    );
    assert_eq!(
        f.fill.metrics.count(Event::PendingLookupMiss),
        pending_misses + 2
    );
    assert_eq!(f.fill.metrics.count(Event::CiphertextLookupHit), 1);
    assert_eq!(f.fill.metrics.count(Event::PageDecrypt), 0);
    assert_eq!(f.origin.calls.get(), 0);
    assert_eq!(f.reactor.in_flight(), 0);
    assert!(f.fill.local_copies.borrow().is_empty());
    assert_eq!(f.fill.dependencies.admission.used(ResourceClass::Flight), 0);
    assert_eq!(f.fill.dependencies.admission.used(ResourceClass::Waiter), 0);
    assert_eq!(
        f.fill
            .dependencies
            .admission
            .used(ResourceClass::RequestContext),
        context_baseline
    );
    // The retained disk token and original ciphertext also support a single
    // verified promotion, rather than another disk read or origin acquisition.
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
    let promoted = drive(
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
    assert_eq!(promoted.plaintext.bytes(), b"abc");
    assert!(Arc::ptr_eq(&first.1.inner, &promoted.ciphertext.inner));
    assert_eq!(f.fill.metrics.count(Event::DiskHit), 1);
    assert_eq!(f.fill.metrics.count(Event::PageDecrypt), 1);
    assert_eq!(f.origin.calls.get(), 0);
}

#[test]
fn detached_copy_only_keeps_disk_fence_and_independent_waiters() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    let f = cold_disk_fixture();
    let caller = RequestScope::new(RequestId([9; 16]), f.scope.deadline.0).unwrap();
    let mut first = f.fill.copy_only(&f.page, &caller);
    let mut second = f.fill.copy_only(&f.page, &f.scope);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(first.as_mut().poll(&mut cx).is_pending());
    assert!(second.as_mut().poll(&mut cx).is_pending());
    caller.cancel().unwrap();
    assert!(matches!(
        first.as_mut().poll(&mut cx),
        Poll::Ready(Err(Error::Cancelled))
    ));
    drop((first, second));
    assert_eq!(f.fill.dependencies.admission.used(ResourceClass::Waiter), 0);
    assert_eq!(f.fill.dependencies.admission.used(ResourceClass::Flight), 1);
    assert_eq!(f.fill.local_copies.borrow().len(), 1);
    assert_eq!(f.reactor.in_flight(), 1);
    // A replacement joins the still-owned read, not a second disk submission.
    let mut replacement = f.fill.copy_only(&f.page, &f.scope);
    assert!(replacement.as_mut().poll(&mut cx).is_pending());
    assert_eq!(f.reactor.in_flight(), 1);
    assert!(drive_disk(&f, replacement).unwrap().is_some());
    assert_eq!(f.fill.dependencies.admission.used(ResourceClass::Flight), 0);
    assert_eq!(f.fill.dependencies.admission.used(ResourceClass::Waiter), 0);
    assert!(f.fill.local_copies.borrow().is_empty());
    assert_eq!(f.reactor.in_flight(), 0);
    assert_eq!(f.origin.calls.get(), 0);
}

#[test]
fn copy_only_miss_releases_shared_scope_subscriptions_across_cohorts() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    use futures::{StreamExt, stream::FuturesUnordered};
    let mut f = fixture();
    for _ in 0..1100 {
        let mut requests = FuturesUnordered::new();
        requests.push(f.fill.copy_only(&f.page, &f.scope));
        assert!(
            drive(requests.next(), &mut f.engine, &f.crypto)
                .unwrap()
                .unwrap()
                .is_none()
        );
        drop(requests);
        assert!(f.fill.local_copies.borrow().is_empty());
        assert_eq!(f.fill.dependencies.admission.used(ResourceClass::Flight), 0);
        assert_eq!(f.fill.dependencies.admission.used(ResourceClass::Waiter), 0);
        assert_eq!(super::super::drivers::pending(), 0);
    }
    assert_eq!(f.origin.calls.get(), 0);
    assert_eq!(f.fill.metrics.count(Event::CiphertextLookupMiss), 1100);
    assert_eq!(f.fill.metrics.count(Event::PendingLookupMiss), 1100);
    assert_eq!(f.fill.metrics.count(Event::DiskIndexLookupMiss), 1100);
    assert_eq!(f.fill.metrics.count(Event::DiskIndexLookupHit), 0);
}

#[test]
fn canceled_before_lookup_has_no_outcome() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    let f = fixture();
    f.scope.cancel().unwrap();
    assert!(matches!(
        f.fill.cached_page(&f.page, &f.scope),
        Err(Error::Cancelled)
    ));
    assert!(matches!(
        futures::executor::block_on(f.fill.copy_only(&f.page, &f.scope)),
        Err(Error::Cancelled)
    ));
    for event in [
        Event::PlaintextLookupHit,
        Event::PlaintextLookupMiss,
        Event::PlaintextLookupError,
        Event::CiphertextLookupHit,
        Event::CiphertextLookupMiss,
        Event::CiphertextLookupError,
        Event::PendingLookupMiss,
        Event::DiskIndexLookupMiss,
    ] {
        assert_eq!(f.fill.metrics.count(event), 0);
    }
}

#[test]
fn lookup_plaintext_and_pending_hits_do_not_probe_disk() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    let mut f = fixture();
    assert!(f.fill.cached_page(&f.page, &f.scope).unwrap().is_none());
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
    let page = drive(
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
    assert!(f.fill.cached_page(&f.page, &f.scope).unwrap().is_some());
    assert_eq!(f.fill.metrics.count(Event::PlaintextLookupHit), 1);
    assert_eq!(f.fill.metrics.count(Event::PlaintextLookupMiss), 2);
    drop(page);
    f.fill.dependencies.memory.evict_idle(usize::MAX).unwrap();
    // The pending writer protects its shared ciphertext from idle eviction.
    // Remove only memory lookup references to exercise the pending boundary.
    f.fill
        .dependencies
        .memory
        .remove_cache(&f.context.object.cache)
        .unwrap();
    assert!(
        f.fill
            .dependencies
            .memory
            .ciphertext(&f.page)
            .unwrap()
            .is_none()
    );
    assert!(
        f.fill
            .dependencies
            .writer
            .copy_only(&f.page)
            .unwrap()
            .is_some()
    );
    let disk_misses = f.fill.metrics.count(Event::DiskIndexLookupMiss);
    assert!(
        drive(
            f.fill.copy_only(&f.page, &f.scope),
            &mut f.engine,
            &f.crypto
        )
        .unwrap()
        .is_some()
    );
    assert_eq!(f.fill.metrics.count(Event::PendingLookupHit), 1);
    assert_eq!(
        f.fill.metrics.count(Event::DiskIndexLookupMiss),
        disk_misses
    );
    assert_eq!(f.fill.metrics.count(Event::DiskIndexLookupHit), 0);
}

#[test]
fn copy_only_local_state_admission_failure_releases_all_reservations() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    let mut f = fixture();
    let admission = &f.fill.dependencies.admission;
    let pressure = admission
        .reserve(
            None,
            ResourceClass::Flight,
            admission.limit(ResourceClass::Flight),
        )
        .unwrap();
    assert!(matches!(
        drive(
            f.fill.copy_only(&f.page, &f.scope),
            &mut f.engine,
            &f.crypto
        ),
        Err(Error::Overloaded)
    ));
    assert!(f.fill.local_copies.borrow().is_empty());
    assert_eq!(admission.used(ResourceClass::Waiter), 0);
    assert_eq!(super::super::drivers::pending(), 0);
    drop(pressure);
    assert!(
        drive(
            f.fill.copy_only(&f.page, &f.scope),
            &mut f.engine,
            &f.crypto
        )
        .unwrap()
        .is_none()
    );
    assert_eq!(admission.used(ResourceClass::Flight), 0);
    assert_eq!(admission.used(ResourceClass::Waiter), 0);
    assert_eq!(f.origin.calls.get(), 0);
}

#[test]
fn copy_only_disk_read_reclaims_idle_ciphertext_without_revoking_live_copies() {
    disk_copy_reclaims_idle_ciphertext(false);
}

#[test]
fn peer_bootstrap_disk_copy_reclaims_idle_ciphertext_before_fresh_acquisition() {
    disk_copy_reclaims_idle_ciphertext(true);
}

fn disk_copy_reclaims_idle_ciphertext(bootstrap: bool) {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    let mut limits = crate::test_support::cluster::config(false).limits;
    limits.plaintext_bytes = std::num::NonZeroUsize::new(512 * 1024 * 1024).unwrap();
    limits.ciphertext_bytes = limits.plaintext_bytes;
    limits.metadata_entries = std::num::NonZeroUsize::new(64).unwrap();
    let mut f = fixture_with(PAGE_BYTES, Some(limits));
    f.scope.deadline.0 = Instant::now() + Duration::from_secs(30);
    f.reactor.init().unwrap();
    futures::executor::block_on(f.fill.dependencies.writer.open()).unwrap();
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
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
    let expected = original.ciphertext.bytes().to_vec();
    let envelope = original.ciphertext.envelope().clone();
    let metadata = original.metadata.clone();
    let run_io = |mut work: crate::error::Operation<'_, usize>| {
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        loop {
            f.scope.check().unwrap();
            if let Poll::Ready(result) = work.as_mut().poll(&mut cx) {
                return result.unwrap();
            }
            f.reactor.poll_budgeted(64).unwrap();
            f.reactor.wait(Duration::from_millis(1)).unwrap();
        }
    };
    assert_eq!(run_io(f.fill.dependencies.writer.progress(1, &f.scope)), 1);
    assert!(
        f.fill
            .dependencies
            .writer
            .index()
            .lookup(&f.page)
            .unwrap()
            .is_some()
    );
    drop(original);
    f.fill.dependencies.memory.evict_idle(usize::MAX).unwrap();
    let deps = &f.fill.dependencies;
    assert!(
        deps.memory.ciphertext(&f.page).unwrap().is_none(),
        "target must be disk-only"
    );
    let failures = crate::telemetry::failures::Failures::default();
    deps.admission.set_observer(failures.observer(WorkerId(0)));
    use crate::read::metadata::{MetadataDependencies, MetadataService};
    let index = Rc::new(Index::new(WorkerId(0), 64));
    let mut fresh = metadata.clone();
    fresh.expires_at = ExpiresAt::from_unix_millis(
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_millis() as u64
            + 60_000,
    )
    .unwrap();
    index.publish_current(fresh).unwrap();
    let service = MetadataService::new(
        deps.candidates.clone(),
        deps.origin.clone(),
        deps.credentials.clone(),
        64,
        MetadataDependencies {
            index,
            owners: deps.metadata_owner.clone(),
            fill: Rc::new(Fill::new(deps.clone())),
        },
    );
    let read_copy = || -> crate::error::Operation<'_, (ObjectMetadata, CiphertextPage)> {
        Box::pin(async {
            if bootstrap {
                let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
                let response = service
                    .bootstrap_peer(f.membership.clone(), &f.context, &f.scope, &mut budget)
                    .await?;
                let PeerResponse::Bootstrap {
                    metadata,
                    page_zero: Some(ciphertext),
                } = response
                else {
                    panic!("expected retained bootstrap page")
                };
                assert_eq!(
                    budget.remaining_attempts(),
                    4,
                    "copy needs no acquisition attempt"
                );
                Ok((metadata, ciphertext))
            } else {
                f.fill
                    .copy_only(&f.page, &f.scope)
                    .await
                    .map(|copy| copy.expect("persisted page"))
            }
        })
    };
    let mut retained = Vec::new();
    for number in 1..=30 {
        let mut metadata = metadata.clone();
        metadata.version.object.key = CacheKey([number; 32]);
        let mut envelope = envelope.clone();
        envelope.page.version = metadata.version.clone();
        let ciphertext = deps
            .buffers
            .ciphertext(
                deps.admission
                    .reserve(
                        Some(&f.context.object.cache),
                        ResourceClass::Ciphertext,
                        expected.len(),
                    )
                    .unwrap(),
                envelope,
                expected.clone(),
            )
            .unwrap();
        let page = crate::memory::page::UnverifiedPage {
            copy: crate::memory::page::CiphertextCopy {
                metadata,
                ciphertext,
            },
            disk_token: None,
        };
        deps.memory.publish_ciphertext(page.clone()).unwrap();
        retained.push(page);
    }
    // All copies are live: fail without revoking a reader or starting origin work.
    let mut blocked = read_copy();
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    match blocked.as_mut().poll(&mut cx) {
        Poll::Ready(Err(Error::Overloaded)) => {}
        Poll::Ready(Err(error)) => panic!("unexpected blocked error {error:?}"),
        Poll::Ready(Ok(_)) => panic!("unexpected blocked success"),
        Poll::Pending => panic!("unexpected blocked pending"),
    }
    drop(blocked);
    assert_eq!(f.origin.calls.get(), 1);
    // Keep one copy pinned, but make the remaining working set reclaimable.
    let busy = retained.pop().unwrap();
    drop(retained);
    let mut read = read_copy();
    let copy = loop {
        f.scope.check().unwrap();
        if let Poll::Ready(result) = read.as_mut().poll(&mut cx) {
            break result.expect("idle cached ciphertext must not reject a disk copy");
        }
        f.reactor.poll_budgeted(64).unwrap();
        f.reactor.wait(Duration::from_millis(1)).unwrap();
    };
    assert_eq!(copy.1.bytes(), expected);
    assert_eq!(copy.1.envelope(), &envelope);
    assert_eq!(busy.copy.ciphertext.bytes(), expected);
    assert!(
        deps.memory
            .ciphertext(&busy.copy.ciphertext.envelope().page)
            .unwrap()
            .is_some()
    );
    assert_eq!(f.origin.calls.get(), 1);
    assert!(
        deps.admission.used(ResourceClass::Ciphertext)
            <= deps.admission.limit(ResourceClass::Ciphertext)
    );
    let mut diagnostics = String::new();
    failures.write(&mut diagnostics).unwrap();
    assert!(diagnostics.contains("requested: 33554960"), "{diagnostics}");
    drop((read, copy, busy));
    deps.memory.evict_idle(usize::MAX).unwrap();
    deps.writer.slabs().reclaim_buffer();
    deps.admission.reclaim_buffers();
    assert_eq!(deps.admission.used(ResourceClass::Ciphertext), 0);
    assert_eq!(f.reactor.in_flight(), 0);
}

#[test]
fn copy_only_miss_has_no_origin_side_effect_and_wrong_context_never_joins() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
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
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
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
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
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
