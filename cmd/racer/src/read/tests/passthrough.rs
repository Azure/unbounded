//! Passthrough caches: nothing is retained in memory, on disk, or in the
//! metadata catalog, concurrent readers still share one origin fill, and the
//! local route calls this node's own origin without asking peers.

use super::fill::*;
use crate::read::flight::AcquisitionBudget;
use racer_control_wire::CacheMode;

const MODES: [CacheMode; 2] = [CacheMode::PassthroughPeers, CacheMode::PassthroughLocal];

fn acquire_plain(f: &mut Fixture) -> Result<PageResult> {
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
    acquire(f, &mut budget)
}

fn acquire_cipher(f: &mut Fixture) -> Result<crate::memory::CiphertextCopy> {
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
    drive(
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
}

/// Nothing for `f.page` is retained by memory, pending writes, or the disk index.
fn assert_retains_nothing(f: &Fixture) {
    let deps = &f.fill.dependencies;
    assert!(deps.memory.get(&f.page).unwrap().is_none());
    assert!(deps.memory.ciphertext(&f.page).unwrap().is_none());
    assert!(deps.memory.metadata(&f.page.version).unwrap().is_none());
    assert_eq!(deps.writer.pending_count(), 0);
    assert!(deps.writer.index().lookup(&f.page).unwrap().is_none());
    assert!(!f.fill.holds(&f.page));
}

/// A cached read is served from memory the second time; a passthrough read
/// returns the same bytes, retains nothing, and asks origin every time.
#[test]
fn passthrough_reads_refetch_from_origin_and_retain_nothing() {
    for mode in [CacheMode::Cached, MODES[0], MODES[1]] {
        let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
        let _queue = queue.enter();
        let mut f = fixture();
        f.set_mode(mode);
        for read in 1..=3 {
            let result = acquire_plain(&mut f).unwrap();
            assert_eq!(result.plaintext.bytes(), b"abc");
            let expected = if mode.passthrough() { read } else { 1 };
            assert_eq!(f.origin.calls.get(), expected, "{mode:?} read {read}");
            if mode.passthrough() {
                drop(result);
                assert_retains_nothing(&f);
            }
        }
        assert_eq!(
            f.fill.metrics.count(Event::PassthroughLocalOrigin),
            if mode == CacheMode::PassthroughLocal {
                3
            } else {
                0
            },
            "{mode:?}"
        );
    }
}

/// An owner answering peers (ciphertext acquisition) shares the fill but
/// keeps neither the ciphertext nor a disk copy.
#[test]
fn passthrough_owner_ciphertext_fill_retains_nothing() {
    for mode in MODES {
        let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
        let _queue = queue.enter();
        let mut f = fixture();
        f.set_mode(mode);
        for read in 1..=2 {
            let copy = acquire_cipher(&mut f).unwrap();
            assert_eq!(copy.metadata.version, f.page.version);
            drop(copy);
            assert_eq!(f.origin.calls.get(), read, "{mode:?}");
            assert_retains_nothing(&f);
        }
    }
}

/// Concurrent readers of one page join one flight: one origin call serves all
/// of them, and the page is gone once they finish.
#[test]
fn passthrough_concurrent_reads_coalesce_into_one_origin_call() {
    for mode in MODES {
        let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
        let _queue = queue.enter();
        let mut f = fixture();
        f.set_mode(mode);
        f.origin.blocked_pages.borrow_mut().insert(0);
        let mut budgets: Vec<_> = (0..3)
            .map(|_| AcquisitionBudget::new(f.scope.deadline.0, 4, 8))
            .collect();
        let [a, b, c] = &mut budgets[..] else {
            unreachable!()
        };
        let mut first = f.fill.acquire(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            a,
        );
        let mut second = f.fill.acquire(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            b,
        );
        let mut third = f.fill.acquire_ciphertext(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            c,
        );
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        for _ in 0..64 {
            assert!(first.as_mut().poll(&mut cx).is_pending());
            assert!(second.as_mut().poll(&mut cx).is_pending());
            assert!(third.as_mut().poll(&mut cx).is_pending());
            uring_runtime::drivers::poll(&mut cx, 64);
        }
        assert_eq!(f.origin.calls.get(), 1, "{mode:?}");
        f.origin.blocked_pages.borrow_mut().clear();
        let (a, b, c) = drive(
            async { futures::join!(first, second, third) },
            &mut f.engine,
            &f.crypto,
        );
        assert_eq!(a.unwrap().plaintext.bytes(), b"abc");
        assert_eq!(b.unwrap().plaintext.bytes(), b"abc");
        assert_eq!(c.unwrap().metadata.version, f.page.version);
        assert_eq!(f.origin.calls.get(), 1, "{mode:?}");
        assert_retains_nothing(&f);
        // The completed flight does not outlive its readers.
        acquire_plain(&mut f).unwrap();
        assert_eq!(f.origin.calls.get(), 2, "{mode:?}");
    }
}

/// The local route fills from this node's origin even where other nodes own
/// the page: the fixture's requester panics if any peer is contacted.
#[test]
fn passthrough_local_route_never_asks_page_owners() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _queue = queue.enter();
    let mut f = fixture();
    let local = f.fill.dependencies.candidates.node.clone();
    let mut members: Vec<_> = (0..8)
        .map(|i| Member {
            node: racer_control_wire::NodeId(format!("{i:08x}-0000-4000-8000-000000000000")),
            shares: std::num::NonZeroU32::new(64).unwrap(),
            peer_endpoint: format!("127.0.0.1:{}", 9100 + i),
            rails: vec![],
            site: String::new(),
        })
        .collect();
    members.push(Member {
        node: local.clone(),
        shares: std::num::NonZeroU32::new(1).unwrap(),
        peer_endpoint: "127.0.0.1:8000".into(),
        rails: vec![],
        site: String::new(),
    });
    f.membership =
        Arc::new(Membership::validate(racer_control_wire::MembershipVersion(1), members).unwrap());
    let ranked =
        Placement::new(16).rank(f.membership.clone(), &f.page.version.object, f.page.number);
    assert!(
        !ranked.ordered.contains(&local),
        "the fixture node must not own the page"
    );
    f.set_mode(CacheMode::PassthroughLocal);
    let result = acquire_plain(&mut f).unwrap();
    assert_eq!(result.plaintext.bytes(), b"abc");
    let copy = acquire_cipher(&mut f).unwrap();
    assert_eq!(copy.metadata.version, f.page.version);
    assert_eq!(f.origin.calls.get(), 2);
    assert_eq!(f.fill.metrics.count(Event::PassthroughLocalOrigin), 2);
    drop(result);
    assert_retains_nothing(&f);
}

/// Only a local-route passthrough cache can mint a local origin grant, and the
/// grant names this node, the exact object, and the exact page.
#[test]
fn local_origin_authority_requires_local_route() {
    let f = fixture();
    let policy = &f.fill.dependencies.candidates;
    for mode in [CacheMode::Cached, CacheMode::PassthroughPeers] {
        assert_eq!(
            policy
                .local_authority(
                    f.membership.clone(),
                    &f.page.version.object,
                    f.page.number,
                    mode
                )
                .err(),
            Some(Error::Unauthorized),
            "{mode:?}"
        );
    }
    let authority = policy
        .local_authority(
            f.membership.clone(),
            &f.page.version.object,
            f.page.number,
            CacheMode::PassthroughLocal,
        )
        .unwrap();
    assert_eq!(authority.rank(), 0);
    assert_eq!(authority.owners(), std::slice::from_ref(&policy.node));
    authority
        .validate(&f.page.version.object, f.page.number)
        .unwrap();
    assert_eq!(
        authority.validate(&f.page.version.object, PageNumber(1)),
        Err(Error::Unauthorized)
    );
    // A node outside the membership gets no grant.
    let outsider = Arc::new(
        Membership::validate(
            racer_control_wire::MembershipVersion(2),
            vec![Member {
                node: racer_control_wire::NodeId("99999999-9999-4999-8999-999999999999".into()),
                shares: std::num::NonZeroU32::new(1).unwrap(),
                peer_endpoint: "127.0.0.1:9000".into(),
                rails: vec![],
                site: String::new(),
            }],
        )
        .unwrap(),
    );
    assert_eq!(
        policy
            .local_authority(
                outsider,
                &f.page.version.object,
                f.page.number,
                CacheMode::PassthroughLocal
            )
            .err(),
        Some(Error::Unauthorized)
    );
}

/// Pages cached before a switch to passthrough are never served afterwards,
/// and switching back to cached retains pages again.
#[test]
fn residual_pages_are_not_served_after_switching_to_passthrough() {
    for mode in MODES {
        let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
        let _queue = queue.enter();
        let mut f = fixture();
        let cached = acquire_plain(&mut f).unwrap();
        assert!(
            f.fill
                .dependencies
                .memory
                .ciphertext(&f.page)
                .unwrap()
                .is_some()
        );
        assert!(f.fill.holds(&f.page));
        f.set_mode(mode);
        assert!(f.fill.dependencies.memory.get(&f.page).unwrap().is_none());
        assert!(
            f.fill
                .dependencies
                .memory
                .ciphertext(&f.page)
                .unwrap()
                .is_none()
        );
        assert!(!f.fill.holds(&f.page));
        drop(cached);
        acquire_plain(&mut f).unwrap();
        assert_eq!(f.origin.calls.get(), 2, "{mode:?}");
        // The worker purges the cache on the transition (app refresh_snapshot).
        f.fill
            .dependencies
            .memory
            .remove_cache(&f.context.object.cache)
            .unwrap();
        f.fill
            .dependencies
            .writer
            .remove_cache(&f.context.object.cache)
            .unwrap();
        f.set_mode(CacheMode::Cached);
        acquire_plain(&mut f).unwrap();
        acquire_plain(&mut f).unwrap();
        assert_eq!(f.origin.calls.get(), 3, "{mode:?}");
    }
}

/// A disk copy written before a switch to passthrough is not read back.
#[test]
fn residual_disk_copy_is_not_served_after_switching_to_passthrough() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _queue = queue.enter();
    let mut f = fixture();
    let result = acquire_plain(&mut f).unwrap();
    let deps = f.fill.dependencies.clone();
    let dirty = deps
        .admission
        .reserve(
            Some(&f.context.object.cache),
            ResourceClass::DirtyCiphertext,
            result.ciphertext.bytes().len(),
        )
        .unwrap();
    deps.writer.enqueue(result.copy(), dirty).ok();
    drop(result);
    drive_disk(&f, deps.writer.progress(8, &f.scope)).unwrap();
    assert!(deps.writer.index().lookup(&f.page).unwrap().is_some());
    deps.memory.remove_cache(&f.context.object.cache).unwrap();
    f.set_mode(CacheMode::PassthroughPeers);
    acquire_plain(&mut f).unwrap();
    assert_eq!(f.origin.calls.get(), 2);
    assert_eq!(f.fill.metrics.count(Event::DiskHit), 0);
}

/// Passthrough metadata is resolved with origin on every read; a cached
/// cache reuses the fresh observation.
#[test]
fn passthrough_metadata_is_revalidated_on_every_read() {
    use crate::model::MetadataSelector;
    use crate::test_support::origin::AdapterOrigin;
    use crate::test_support::origin::RequestKind;
    for mode in [CacheMode::Cached, MODES[0], MODES[1]] {
        let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
        let _queue = queue.enter();
        let mut f = fixture();
        f.set_mode(mode);
        let mut metadata = f.origin.metadata.clone();
        metadata.expires_at = crate::model::ExpiresAt::test_time(
            std::time::SystemTime::now() + Duration::from_secs(3600),
        );
        let adapter = AdapterOrigin::new("fixture", metadata.clone());
        let index = f.metadata_index(8);
        let service = f.metadata_service_with_index(adapter_client(&f, &adapter), 1, index.clone());
        for read in 1..=3 {
            let resolved = drive_io(
                service.resolve(
                    MetadataSelector::Fresh,
                    f.membership.clone(),
                    &f.context,
                    &f.scope,
                ),
                &f.reactor,
                &mut f.engine,
                &f.crypto,
            )
            .unwrap_or_else(|error| panic!("{mode:?} read {read}: {error:?}"));
            assert_eq!(resolved, metadata);
            let expected = if mode.passthrough() { read } else { 1 };
            assert_eq!(adapter.count(RequestKind::Head), expected, "{mode:?}");
        }
        assert_eq!(
            index.current(&f.context.object).unwrap().is_some(),
            !mode.passthrough(),
            "{mode:?}"
        );
        // A pinned read still resolves, and is not answered from a catalog.
        let pinned = drive_io(
            service.resolve(
                MetadataSelector::Pinned(metadata.version.etag.clone()),
                f.membership.clone(),
                &f.context,
                &f.scope,
            ),
            &f.reactor,
            &mut f.engine,
            &f.crypto,
        )
        .unwrap();
        assert_eq!(pinned.version, metadata.version);
    }
}

/// Holds and warm fills retain pages, so a passthrough cache refuses them on
/// the owner worker while a cached cache accepts the same hold.
#[test]
fn passthrough_refuses_warm_holds() {
    use crate::test_support::security::network;
    for mode in [CacheMode::Cached, MODES[0], MODES[1]] {
        let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
        let _queue = queue.enter();
        let signers = network(1);
        let mut f = fixture_with(PAGE_BYTES + 7, None);
        f.reactor.init().unwrap();
        f.set_mode(mode);
        let membership = f.membership.clone();
        let (_local, mut endpoint, _publication) =
            super::hot_reads::coordinator(&f, &signers[0], &membership, NoPeer::requester());
        let directory = f.fill.dependencies.metadata_owner.clone();
        let version = f.page.version.clone();
        let worker = directory.page_owner(&f.page).unwrap();
        let held = std::collections::BTreeMap::from([(
            worker.0,
            [PageNumber(0)]
                .into_iter()
                .collect::<crate::store::holds::HoldPages>(),
        )]);
        let until = uring_runtime::environment::wall_now() + Duration::from_secs(60);
        let mut fresh = Vec::new();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut future =
            Box::pin(directory.reserve_warm(&version, &held, until, &f.scope, &mut fresh));
        let mut output = None;
        for _ in 0..4096 {
            if let Poll::Ready(result) = future.as_mut().poll(&mut cx) {
                output = Some(result);
                break;
            }
            pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
        }
        let result = output.expect("hold did not complete");
        drop(future);
        let index = f.fill.dependencies.writer.index();
        if mode.passthrough() {
            assert_eq!(result.err(), Some(Error::Unavailable), "{mode:?}");
            assert!(!index.is_held(&f.page), "{mode:?}");
        } else {
            result.unwrap();
            assert!(index.is_held(&f.page));
        }
        endpoint.uninstall().unwrap();
    }
}

/// A warm job that completed before the cache switched to passthrough no
/// longer holds a copy, so reads skip it and fetch from origin, and the
/// transition forgets the job and its leases.
#[test]
fn reads_after_switching_a_warmed_cache_to_passthrough_use_origin() {
    use crate::test_support::security::network;
    // The local route reaches origin without peers; the guard is mode-agnostic.
    for mode in [CacheMode::PassthroughLocal] {
        let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
        let _queue = queue.enter();
        let signers = network(1);
        let mut f = fixture();
        f.reactor.init().unwrap();
        // The local route only grants origin authority to a member node.
        let membership = Arc::new(
            Membership::validate(
                racer_control_wire::MembershipVersion(1),
                vec![Member {
                    node: signers[0].node().clone(),
                    shares: std::num::NonZeroU32::new(1).unwrap(),
                    peer_endpoint: "127.0.0.1:8000".into(),
                    rails: vec![],
                    site: String::new(),
                }],
            )
            .unwrap(),
        );
        let (_local, mut endpoint, _publication) =
            super::hot_reads::coordinator(&f, &signers[0], &membership, NoPeer::requester());
        let directory = f.fill.dependencies.metadata_owner.clone();
        let version = f.page.version.clone();
        // The warm job completed page 0 on this node, then the cache switched.
        let (job, _) = directory.warm.begin(&version).unwrap();
        crate::read::warm::WarmRegistry::publish_through(&job, 1);
        let until = uring_runtime::environment::wall_now() + Duration::from_secs(60);
        crate::read::warm::WarmRegistry::finish(&job, until);
        let key = (
            racer_control_wire::NodeId("22222222-2222-4222-8222-000000000007".into()),
            [1; 16],
        );
        directory.warm.grant_lease(&version, &key, until).unwrap();
        f.set_mode(mode);

        let budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
        let mut work = directory
            .start_page(
                f.page.clone(),
                membership.clone(),
                &f.context,
                &f.scope,
                budget,
            )
            .unwrap();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut output = None;
        for _ in 0..4096 {
            if let Poll::Ready(result) = work.as_mut().poll(&mut cx) {
                output = Some(result);
                break;
            }
            pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
        }
        drop(work);
        let result = output.expect("read did not complete").unwrap().0.unwrap();
        assert_eq!(result.plaintext.bytes(), b"abc", "{mode:?}");
        assert_eq!(f.origin.calls.get(), 1, "{mode:?}");

        assert!(directory.warm.contains(&version));
        directory.warm.forget_cache(&version.object.cache);
        assert!(!directory.warm.contains(&version), "{mode:?}");
        assert_eq!(directory.warm.lease_count(&version), 0, "{mode:?}");
    }
}

/// A publication queued by a cached fill can reach the metadata owner after
/// the cache switched to passthrough. The owner drops it rather than
/// repopulating the catalog after the one-time purge.
#[test]
fn queued_publication_is_dropped_after_switching_to_passthrough() {
    use crate::test_support::security::network;
    for mode in [CacheMode::Cached, MODES[0], MODES[1]] {
        let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
        let _queue = queue.enter();
        let signers = network(1);
        let mut f = fixture_with(PAGE_BYTES + 7, None);
        f.reactor.init().unwrap();
        let membership = f.membership.clone();
        let (local, mut endpoint, _publication) =
            super::hot_reads::coordinator(&f, &signers[0], &membership, NoPeer::requester());
        let directory = f.fill.dependencies.metadata_owner.clone();
        let mut metadata = f.origin.metadata.immutable();
        metadata.version.etag = StrongEtag::test_value("queued-publish");
        let version = metadata.version.clone();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        // The cached fill queues its publication before the mode switch.
        let mut future = directory.queue_publication(metadata, &f.scope).unwrap();
        f.set_mode(mode);
        let mut output = None;
        for _ in 0..4096 {
            if let Poll::Ready(result) = future.as_mut().poll(&mut cx) {
                output = Some(result);
                break;
            }
            pump_worker(&mut endpoint, &mut f.engine, &f.crypto, &f.reactor);
        }
        output.expect("publication did not complete").unwrap();
        drop(future);
        let published = local.metadata.catalog().version(&version).unwrap();
        assert_eq!(published.is_some(), !mode.passthrough(), "{mode:?}");
        endpoint.uninstall().unwrap();
    }
}
