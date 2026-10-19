use super::*;
use crate::admission::AdmissionPolicy;
use crate::admission::ResourceClass;
use crate::error::Error;
use crate::error::Result;
use crate::memory::BufferPool;
use crate::memory::CiphertextCopy;
use crate::model::CacheKey;
use crate::model::ExpiresAt;
use crate::model::Nonce;
use crate::model::ObjectId;
use crate::model::ObjectMetadata;
use crate::model::ObjectVersion;
use crate::model::PageEnvelope;
use crate::model::PageId;
use crate::model::PageNumber;
use crate::model::RequestId;
use crate::model::StrongEtag;
use crate::model::WorkerId;
use crate::runtime::Reactor;
use crate::runtime::RequestScope;
use crate::store::Generation;
use page_alloc::Alignment;
use page_alloc::Extent;
use page_alloc::Slab;
use page_alloc::volume::SLOT_BYTES;
use page_alloc::volume::Tier;
use racer_control_wire::CacheId;
use std::future::Future;
use std::path::PathBuf;
use std::sync::atomic::AtomicU64;
use std::sync::atomic::Ordering;
use std::task::Context;
use std::task::Poll;
use std::time::Duration;
use std::time::Instant;

mod volume_pressure {
    use super::*;
    fn fixture(capacity: usize) -> Fixture {
        let f = Fixture::new();
        let _ = f.open().expect("volume pressure tests require io_uring");
        f.store.writer.index().set_page_capacity(capacity).unwrap();
        f
    }
    fn persist(f: &Fixture, number: u8) -> PageId {
        let copy = f.copy(number, 113);
        let id = copy.ciphertext.envelope().page.clone();
        f.enqueue(copy).unwrap();
        assert_eq!(
            drive(&f.reactor, f.store.writer.progress(1, &scope())).unwrap(),
            1
        );
        assert!(f.store.writer.index().lookup(&id).unwrap().is_some());
        id
    }
    #[test]
    fn small_records_share_a_slot_and_committed_pages_survive_restart() {
        let f = fixture(8);
        let ids: Vec<_> = (1..=4).map(|number| persist(&f, number)).collect();
        let index = f.store.writer.index();
        let values: Vec<_> = ids
            .iter()
            .map(|id| index.lookup(id).unwrap().unwrap())
            .collect();
        assert!(values.iter().all(|value| value.slot == values[0].slot));
        assert!(values.iter().all(|value| value.tier == Tier::Cache));
        assert_eq!(index.disk_pages().len(), 4);
        assert_eq!(f.store.writer.discarded_count(), 0);
        // Drain commits the cache-tier index so a clean restart keeps it.
        drive(&f.reactor, f.store.writer.drain(&scope())).unwrap();
        assert!(!index.is_dirty());
        let f = f.restart();
        let _ = f.open().unwrap();
        let index = f.store.writer.index();
        for (id, number) in ids.iter().zip(1..) {
            assert_eq!(index.lookup(id).unwrap(), Some(values[number as usize - 1]));
            let read = drive(&f.reactor, f.store.reader.read(id, &scope()))
                .unwrap()
                .unwrap();
            assert_eq!(read.ciphertext.bytes(), vec![number; 129]);
        }
        // Mount always leaves the generation skip to commit.
        assert!(index.is_dirty());
    }
    #[test]
    fn replacement_does_not_evict_neighbors_or_let_old_tokens_remove_new_mapping() {
        let f = fixture(2);
        let first = persist(&f, 1);
        let second = persist(&f, 2);
        let (_, token) = drive(&f.reactor, f.store.reader.read_with_token(&first, &scope()))
            .unwrap()
            .unwrap();
        let index = f.store.writer.index();
        let neighbor = index.lookup(&second).unwrap().unwrap();
        let old = index.lookup(&first).unwrap().unwrap();
        // A healthy resident is deduplicated. Only invalidation warrants a rewrite.
        assert_eq!(f.enqueue(f.copy(1, 113)).unwrap(), 0);
        f.store.reader.invalidate(&token).unwrap();
        persist(&f, 1);
        let replacement = index.lookup(&first).unwrap().unwrap();
        assert_ne!(old, replacement);
        assert!(replacement.generation > old.generation);
        f.store.reader.invalidate(&token).unwrap();
        assert_eq!(index.lookup(&first).unwrap(), Some(replacement));
        assert_eq!(index.lookup(&second).unwrap(), Some(neighbor));
        assert_eq!(index.disk_pages().len(), 2);
    }
    #[test]
    fn removal_during_read_cannot_resurrect_copy() {
        let f = fixture(1);
        let first = persist(&f, 1);
        let index = f.store.writer.index();
        let old = index.lookup(&first).unwrap().unwrap();
        let (_, token) = drive(&f.reactor, f.store.reader.read_with_token(&first, &scope()))
            .unwrap()
            .unwrap();
        let request = scope();
        let mut read = f.store.reader.read(&first, &request);
        assert!(
            read.as_mut()
                .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
                .is_pending()
        );
        index.remove_if_matches(&first, &old);
        assert!(drive(&f.reactor, read).unwrap().is_none());
        persist(&f, 1);
        let new = index.lookup(&first).unwrap().unwrap();
        assert_ne!(new.generation, old.generation);
        f.store.reader.invalidate(&token).unwrap();
        assert_eq!(index.lookup(&first).unwrap(), Some(new));
    }
    #[test]
    fn cache_removal_during_pressure_write_prevents_late_publication() {
        let f = fixture(1);
        let first = persist(&f, 1);
        let copy = f.copy(2, 113);
        let second = copy.ciphertext.envelope().page.clone();
        f.enqueue(copy).unwrap();
        let request = scope();
        let mut write = f.store.writer.progress(1, &request);
        assert!(
            write
                .as_mut()
                .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
                .is_pending()
        );
        f.store
            .writer
            .remove_cache(&CacheId(crate::test_support::security::CACHE.into()))
            .unwrap();
        assert!(f.store.writer.index().lookup(&first).unwrap().is_none());
        drive(&f.reactor, write).unwrap();
        assert!(f.store.writer.index().lookup(&second).unwrap().is_none());
        assert!(f.store.writer.is_idle());
        assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
        f.store.writer.reclaim_idle_buffer();
        assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
        assert!(f.enqueue(f.copy(3, 113)).is_ok());
    }
    #[test]
    fn busy_volume_evicts_cache_slots_then_discards_as_overloaded() {
        let f = fixture(8);
        let index = f.store.writer.index();
        let volume = index.volume().unwrap();
        // A whole-slot cache page, then leased reservations on every free slot.
        let resident = f.copy(9, 113);
        let resident_id = resident.ciphertext.envelope().page.clone();
        f.place(&resident, Tier::Cache, SLOT_BYTES);
        let retention = index.retention();
        assert_eq!(retention.snapshot().indexed_payload_bytes, 113);
        let free = volume.usage().free_slots;
        let reservations: Vec<_> = (0..free)
            .map(|_| volume.allocate(Tier::Cache, SLOT_BYTES).unwrap())
            .collect();
        assert_eq!(volume.usage().free_slots, 0);
        // A small record needs a new slot: the volume evicts the cache page.
        let first = persist(&f, 1);
        assert!(index.lookup(&resident_id).unwrap().is_none());
        assert_eq!(retention.snapshot().disk[0].slot_evicted_pages, 1);
        assert!(index.lookup(&first).unwrap().is_some());
        // A larger record needs another slot, but only open and leased slots
        // remain: the write is discarded without failing progress.
        let large = f.copy(2, 9000);
        let large_id = large.ciphertext.envelope().page.clone();
        f.enqueue(large).unwrap();
        assert_eq!(
            drive(&f.reactor, f.store.writer.progress(1, &scope())),
            Ok(1)
        );
        assert!(index.lookup(&large_id).unwrap().is_none());
        assert_eq!(f.store.writer.discarded_count(), 1);
        assert!(f.store.writer.is_idle());
        drop(reservations);
        persist(&f, 3);
    }
}

#[test]
fn hard_hold_persist_confirms_publication_and_protects_pending() {
    let simulation = uring_runtime::reactor::simulation::Simulation::new();
    let _environment = simulation.enter();
    let f = Fixture::new();
    let _ = f.open().unwrap();
    f.reactor.init().unwrap();
    let copy = f.copy(1, 113);
    let id = copy.ciphertext.envelope().page.clone();
    let until = uring_runtime::environment::wall_now() + Duration::from_secs(60);
    let index = f.store.writer.index();
    index.reserve_hold(&id.version, 1, until).unwrap();
    f.enqueue(copy.clone()).unwrap();
    assert_eq!(f.store.writer.discard_unsubmitted(), 0);
    assert_eq!(f.store.writer.reclaim_ciphertext(None, usize::MAX), 0);
    assert_eq!(f.store.writer.pending_count(), 1);
    assert!(index.lookup(&id).unwrap().is_none());
    drive(
        &f.reactor,
        f.store.writer.persist_held(copy.clone(), &scope()),
    )
    .unwrap();
    assert!(index.lookup(&id).unwrap().is_some());
    assert_eq!(f.store.writer.pending_count(), 0);
    let first = index.lookup(&id).unwrap().unwrap();
    assert_eq!(first.tier, Tier::Storage);
    // Storage-tier acknowledgment is durable: nothing is left to commit.
    assert!(!index.is_dirty());
    drive(&f.reactor, f.store.writer.persist_held(copy, &scope())).unwrap();
    assert_eq!(index.lookup(&id).unwrap().unwrap(), first);
    index.release_hold(&id.version).unwrap();
    assert_eq!(index.hold_expiry(&id.version), Some(until));
}

#[test]
fn hard_hold_page_survives_crash_but_uncommitted_cache_page_does_not() {
    let simulation = uring_runtime::reactor::simulation::Simulation::new();
    let _environment = simulation.enter();
    let f = Fixture::new();
    let _ = f.open().unwrap();
    let held = f.copy(1, 113);
    let held_id = held.ciphertext.envelope().page.clone();
    let until = uring_runtime::environment::wall_now() + Duration::from_secs(60);
    f.store
        .writer
        .index()
        .reserve_hold(&held_id.version, 1, until)
        .unwrap();
    drive(&f.reactor, f.store.writer.persist_held(held, &scope())).unwrap();
    // A cache-tier page published after the durable acknowledgment is only
    // committed lazily.
    let cached = f.copy(2, 113);
    let cached_id = cached.ciphertext.envelope().page.clone();
    f.enqueue(cached).unwrap();
    drive(&f.reactor, f.store.writer.progress(1, &scope())).unwrap();
    assert!(f.store.writer.index().lookup(&cached_id).unwrap().is_some());
    assert!(f.store.writer.index().is_dirty());
    let f = f.restart_with(|| simulation.disk().crash().unwrap());
    let _ = f.open().unwrap();
    let index = f.store.writer.index();
    assert!(index.is_held(&held_id));
    let deadline = index.held_page_expiry(&held_id).unwrap();
    assert!(deadline >= until && deadline < until + Duration::from_secs(1));
    assert_eq!(index.lookup(&held_id).unwrap().unwrap().tier, Tier::Storage);
    let read = drive(&f.reactor, f.store.reader.read(&held_id, &scope()))
        .unwrap()
        .unwrap();
    assert_eq!(read.ciphertext.bytes(), vec![1; 129]);
    assert!(index.lookup(&cached_id).unwrap().is_none());
}

#[test]
fn stale_generation_record_is_a_silent_miss_and_drops_the_entry() {
    let simulation = uring_runtime::reactor::simulation::Simulation::new();
    let _environment = simulation.enter();
    let f = Fixture::new();
    let _ = f.open().unwrap();
    let page = f.copy(1, 113);
    let id = page.ciphertext.envelope().page.clone();
    f.enqueue(page).unwrap();
    drive(&f.reactor, f.store.writer.progress(1, &scope())).unwrap();
    // Model a slot reused after a crash: the header names another write.
    drive(
        &f.reactor,
        f.store.writer.rewrite_record(&id, &scope(), |bytes| {
            let header_len = u32::from_le_bytes(bytes[12..16].try_into().unwrap()) as usize;
            let generation = u64::from_le_bytes(bytes[16..24].try_into().unwrap());
            bytes[16..24].copy_from_slice(&(generation + 1).to_le_bytes());
            let digest_end = header_len - 32;
            let digest = Sha256::digest(&bytes[..digest_end]);
            bytes[digest_end..header_len].copy_from_slice(&digest);
        }),
    )
    .unwrap();
    assert!(
        drive(&f.reactor, f.store.reader.read(&id, &scope()))
            .unwrap()
            .is_none()
    );
    assert_eq!(f.metrics.count(crate::telemetry::Event::CorruptMiss), 0);
    assert!(f.store.writer.index().lookup(&id).unwrap().is_none());
    // The page can be written again.
    f.enqueue(f.copy(1, 113)).unwrap();
    drive(&f.reactor, f.store.writer.progress(1, &scope())).unwrap();
    assert!(
        drive(&f.reactor, f.store.reader.read(&id, &scope()))
            .unwrap()
            .is_some()
    );
}

#[test]
fn hard_hold_write_failure_is_not_success_and_unused_capacity_releases() {
    use uring_runtime::reactor::simulation::{Fault, Simulation};
    let simulation = Simulation::new();
    let _environment = simulation.enter();
    let f = Fixture::new();
    let _ = f.open().unwrap();
    f.reactor.init().unwrap();
    let copy = f.copy(1, 113);
    let id = copy.ciphertext.envelope().page.clone();
    let index = f.store.writer.index();
    assert_eq!(
        drive(
            &f.reactor,
            f.store.writer.persist_held(copy.clone(), &scope())
        ),
        Err(Error::Unavailable)
    );
    index
        .reserve_hold(
            &id.version,
            1,
            uring_runtime::environment::wall_now() + Duration::from_secs(60),
        )
        .unwrap();
    simulation.inject("write", Fault::Errno(libc::EIO)).unwrap();
    assert_eq!(
        drive(&f.reactor, f.store.writer.persist_held(copy, &scope())),
        Err(Error::Io)
    );
    assert!(index.lookup(&id).unwrap().is_none());
    index.release_hold(&id.version).unwrap();
    assert_eq!(index.hold_expiry(&id.version), None);
}

#[test]
fn hard_hold_cancel_waits_for_stalled_background_writer_without_self_wakes() {
    use uring_runtime::reactor::simulation::{Fault, Simulation};
    struct Wakes(std::sync::atomic::AtomicUsize);
    impl futures::task::ArcWake for Wakes {
        fn wake_by_ref(this: &std::sync::Arc<Self>) {
            this.0.fetch_add(1, Ordering::Relaxed);
        }
    }
    let simulation = Simulation::new();
    let _environment = simulation.enter();
    let f = Fixture::new();
    let _ = f.open().unwrap();
    f.reactor.init().unwrap();
    let copy = f.copy(1, 113);
    let id = copy.ciphertext.envelope().page.clone();
    let index = f.store.writer.index();
    let until = uring_runtime::environment::wall_now() + Duration::from_secs(60);
    index.reserve_hold(&id.version, 1, until).unwrap();
    f.enqueue(copy.clone()).unwrap();
    assert_eq!(index.release_hold(&id.version), Err(Error::Overloaded));
    simulation.inject("write", Fault::Delay(6)).unwrap();
    let background_scope = scope();
    let mut background = f.store.writer.progress(1, &background_scope);
    assert!(
        background
            .as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
            .is_pending()
    );
    assert_eq!(f.store.writer.writes_in_flight(), 1);
    let request = scope();
    let mut held = f.store.writer.persist_held(copy.clone(), &request);
    let wakes = std::sync::Arc::new(Wakes(std::sync::atomic::AtomicUsize::new(0)));
    let waker = futures::task::waker(wakes.clone());
    let mut cx = Context::from_waker(&waker);
    assert!(held.as_mut().poll(&mut cx).is_pending());
    request.cancel().unwrap();
    for _ in 0..4 {
        assert!(held.as_mut().poll(&mut cx).is_pending());
    }
    assert_eq!(wakes.0.load(Ordering::Relaxed), 0);
    assert_eq!(index.release_hold(&id.version), Err(Error::Overloaded));
    assert_eq!(f.store.writer.waiters.borrow().len(), 1);

    // Dropped followers unregister, and excess followers are bounded.
    let follower_scope = scope();
    let mut follower = f.store.writer.persist_held(copy.clone(), &follower_scope);
    assert!(follower.as_mut().poll(&mut cx).is_pending());
    let excess_scope = scope();
    let mut excess = f.store.writer.persist_held(copy, &excess_scope);
    assert_eq!(
        excess.as_mut().poll(&mut cx),
        Poll::Ready(Err(Error::Overloaded))
    );
    drop(excess);
    drop(follower);
    assert_eq!(f.store.writer.waiters.borrow().len(), 1);

    drive(&f.reactor, background).unwrap();
    assert_eq!(wakes.0.load(Ordering::Relaxed), 1);
    assert_eq!(f.store.writer.writes_in_flight(), 0);
    assert_eq!(
        held.as_mut().poll(&mut cx),
        Poll::Ready(Err(Error::Cancelled))
    );
    assert!(f.store.writer.waiters.borrow().is_empty());
    index.release_hold(&id.version).unwrap();
    assert_eq!(index.held_page_expiry(&id), Some(until));
}

#[test]
fn hard_hold_cancel_removes_unsubmitted_ticket_after_background_fence() {
    let simulation = uring_runtime::reactor::simulation::Simulation::new();
    let _environment = simulation.enter();
    let f = Fixture::new();
    let _ = f.open().unwrap();
    f.reactor.init().unwrap();
    let copy = f.copy(1, 113);
    let id = copy.ciphertext.envelope().page.clone();
    let index = f.store.writer.index();
    index
        .reserve_hold(
            &id.version,
            1,
            uring_runtime::environment::wall_now() + Duration::from_secs(60),
        )
        .unwrap();
    f.store.writer.busy.set(true);
    let busy = Busy(&f.store.writer);
    let request = scope();
    let mut held = f.store.writer.persist_held(copy, &request);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(held.as_mut().poll(&mut cx).is_pending());
    request.cancel().unwrap();
    assert!(held.as_mut().poll(&mut cx).is_pending());
    assert_eq!(f.store.writer.discard_unsubmitted(), 0);
    drop(busy);
    assert_eq!(
        held.as_mut().poll(&mut cx),
        Poll::Ready(Err(Error::Cancelled))
    );
    assert_eq!(f.store.writer.pending_count(), 0);
    index.release_hold(&id.version).unwrap();
    assert_eq!(index.hold_expiry(&id.version), None);
}

#[test]
fn hard_hold_cancel_fences_kernel_write_after_background_future_is_dropped() {
    use uring_runtime::reactor::simulation::{Fault, Simulation};
    let simulation = Simulation::new();
    let _environment = simulation.enter();
    let f = Fixture::new();
    let _ = f.open().unwrap();
    f.reactor.init().unwrap();
    let copy = f.copy(1, 113);
    let id = copy.ciphertext.envelope().page.clone();
    let index = f.store.writer.index();
    index
        .reserve_hold(
            &id.version,
            1,
            uring_runtime::environment::wall_now() + Duration::from_secs(60),
        )
        .unwrap();
    f.enqueue(copy.clone()).unwrap();
    simulation.inject("write", Fault::Delay(6)).unwrap();
    let background_scope = scope();
    let mut background = f.store.writer.progress(1, &background_scope);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(background.as_mut().poll(&mut cx).is_pending());
    let request = scope();
    let mut held = f.store.writer.persist_held(copy, &request);
    assert!(held.as_mut().poll(&mut cx).is_pending());
    request.cancel().unwrap();
    drop(background);
    assert_eq!(f.store.writer.pending_count(), 0);
    assert_eq!(f.store.writer.writes_in_flight(), 1);
    assert!(held.as_mut().poll(&mut cx).is_pending());
    assert_eq!(index.release_hold(&id.version), Err(Error::Overloaded));
    assert_eq!(drive(&f.reactor, held), Err(Error::Cancelled));
    assert_eq!(f.store.writer.writes_in_flight(), 0);
    index.release_hold(&id.version).unwrap();
    assert_eq!(index.hold_expiry(&id.version), None);
    assert!(f.store.writer.waiters.borrow().is_empty());
}
#[test]
fn disk_observability_publication_read_and_pending_cleanup() {
    let f = Fixture::new();
    let _ = f.open().unwrap();
    f.reactor.init().unwrap();
    let retention = f.store.writer.retention();
    let copy = f.copy(1, 113);
    let id = copy.ciphertext.envelope().page.clone();
    f.enqueue(copy).unwrap();
    f.enqueue(f.copy(1, 113)).unwrap();
    assert_eq!(retention.snapshot().pending_payload_bytes, 113);
    assert_eq!(retention.snapshot().disk[0].published_pages, 0);
    // Ownership is sampled at publication, not captured at queue insertion.
    retention.set_ownership(Rc::new(|_| true));
    drive(&f.reactor, f.store.writer.progress(1, &scope())).unwrap();
    assert_eq!(retention.snapshot().pending_payload_bytes, 0);
    assert_eq!(retention.snapshot().indexed_payload_bytes, 113);
    assert_eq!(retention.snapshot().disk[1].published_pages, 1);
    assert_eq!(retention.snapshot().disk[1].published_payload_bytes, 113);
    assert_eq!(f.enqueue(f.copy(1, 113)), Ok(0));
    assert_eq!(retention.snapshot().disk[1].published_pages, 1);
    let (_, token) = drive(&f.reactor, f.store.reader.read_with_token(&id, &scope()))
        .unwrap()
        .unwrap();
    retention.set_ownership(Rc::new(|_| false));
    assert!(
        drive(&f.reactor, f.store.reader.read(&id, &scope()))
            .unwrap()
            .is_some()
    );
    assert_eq!(retention.snapshot().disk[1].read_payload_bytes, 113);
    assert_eq!(retention.snapshot().disk[0].read_payload_bytes, 113);
    let canceled = scope();
    canceled.cancel().unwrap();
    assert!(drive(&f.reactor, f.store.reader.read(&id, &canceled)).is_err());
    f.store.reader.invalidate(&token).unwrap();
    assert!(
        drive(&f.reactor, f.store.reader.read(&id, &scope()))
            .unwrap()
            .is_none()
    );
    assert_eq!(retention.snapshot().disk[0].read_payload_bytes, 113);
    assert_eq!(retention.snapshot().indexed_payload_bytes, 0);
    f.enqueue(f.copy(2, 3)).unwrap();
    f.enqueue(f.copy(3, 7)).unwrap();
    assert_eq!(retention.snapshot().pending_payload_bytes, 10);
    assert_eq!(f.store.writer.discard_unsubmitted(), 2);
    assert_eq!(retention.snapshot().pending_payload_bytes, 0);
    f.enqueue(f.copy(4, 9)).unwrap();
    f.store.writer.cancel_pending_writes().unwrap();
    assert_eq!(retention.snapshot().pending_payload_bytes, 0);
    assert_eq!(retention.snapshot().disk[0].published_pages, 0);
}
#[test]
fn disk_observability_failed_write_and_read_do_not_credit_bytes() {
    use uring_runtime::reactor::simulation::{Fault, Simulation};
    let simulation = Simulation::new();
    let _environment = simulation.enter();
    let f = Fixture::new();
    let _ = f.open().unwrap();
    let retention = f.store.writer.retention();
    f.enqueue(f.copy(1, 3)).unwrap();
    simulation.inject("write", Fault::Errno(libc::EIO)).unwrap();
    drive(&f.reactor, f.store.writer.progress(1, &scope())).unwrap();
    assert_eq!(retention.snapshot().pending_payload_bytes, 0);
    assert_eq!(retention.snapshot().indexed_payload_bytes, 0);
    assert_eq!(retention.snapshot().disk[0].published_pages, 0);
    let volume = f.store.writer.index().volume().unwrap();
    assert_eq!(volume.usage().records, 0, "a failed write frees its record");
    let copy = f.copy(2, 7);
    let id = copy.ciphertext.envelope().page.clone();
    f.enqueue(copy).unwrap();
    drive(&f.reactor, f.store.writer.progress(1, &scope())).unwrap();
    assert_eq!(retention.snapshot().disk[0].published_payload_bytes, 7);
    simulation.inject("read", Fault::Errno(libc::EIO)).unwrap();
    assert!(
        drive(&f.reactor, f.store.reader.read(&id, &scope()))
            .unwrap()
            .is_none()
    );
    assert_eq!(retention.snapshot().disk[0].read_payload_bytes, 0);
    assert_eq!(retention.snapshot().indexed_payload_bytes, 0);
    assert_eq!(retention.snapshot().disk[0].index_evicted_pages, 0);
    assert_eq!(retention.snapshot().disk[0].slot_evicted_pages, 0);
    assert_eq!(
        volume.usage().records,
        0,
        "invalidation releases the cache record"
    );
}
#[test]
fn read_io_error_on_held_storage_page_misses_without_removing_it() {
    use uring_runtime::reactor::simulation::{Fault, Simulation};
    let simulation = Simulation::new();
    let _environment = simulation.enter();
    let f = Fixture::new();
    let _ = f.open().unwrap();
    f.reactor.init().unwrap();
    let copy = f.copy(1, 113);
    let id = copy.ciphertext.envelope().page.clone();
    let index = f.store.writer.index();
    let until = uring_runtime::environment::wall_now() + Duration::from_secs(60);
    index.reserve_hold(&id.version, 1, until).unwrap();
    drive(&f.reactor, f.store.writer.persist_held(copy, &scope())).unwrap();
    let value = index.lookup(&id).unwrap().unwrap();
    assert_eq!(value.tier, Tier::Storage);
    for fault in [Fault::Errno(libc::EIO), Fault::Short(1)] {
        simulation.inject("read", fault).unwrap();
        assert!(
            drive(&f.reactor, f.store.reader.read(&id, &scope()))
                .unwrap()
                .is_none()
        );
        assert_eq!(index.lookup(&id).unwrap(), Some(value));
        assert_eq!(index.held_page_expiry(&id), Some(until));
    }
    assert!(
        drive(&f.reactor, f.store.reader.read(&id, &scope()))
            .unwrap()
            .is_some()
    );
}
#[test]
fn second_sight_queue_owned_bias_hot_nonowned_and_pressure_discard() {
    for hot_nonowned in [false, true] {
        let f = Fixture::new();
        let _ = f.open().unwrap();
        f.reactor.init().expect("queue tests require io_uring");
        let owned = f.copy(1, 113);
        let other = f.copy(2, 113);
        let owner = owned.ciphertext.envelope().page.clone();
        let id = other.ciphertext.envelope().page.clone();
        let classify = owner.clone();
        let retention = f.store.writer.retention();
        retention.set_ownership(Rc::new(move |page| page == &classify));
        f.enqueue(other).unwrap();
        f.enqueue(owned).unwrap();
        if hot_nonowned {
            for _ in 0..3 {
                retention.touch(&id);
            }
        }
        drive(&f.reactor, f.store.writer.progress(1, &scope())).unwrap();
        assert_eq!(
            f.store.writer.index().lookup(&owner).unwrap().is_some(),
            !hot_nonowned
        );
        assert_eq!(
            f.store.writer.index().lookup(&id).unwrap().is_some(),
            hot_nonowned
        );
        f.store.writer.discard_unsubmitted();
    }
    let f = Fixture::new();
    let _ = f.open().unwrap();
    let low = f.copy(1, 113);
    let high = f.copy(2, 113);
    let low_id = low.ciphertext.envelope().page.clone();
    let high_id = high.ciphertext.envelope().page.clone();
    let retention = f.store.writer.retention();
    let owner = high_id.clone();
    retention.set_ownership(Rc::new(move |page| page == &owner));
    f.enqueue(high).unwrap();
    f.enqueue(low).unwrap();
    assert!(f.store.writer.reclaim_ciphertext(None, 1) > 0);
    assert!(f.store.writer.copy_only(&low_id).unwrap().is_none());
    assert!(f.store.writer.copy_only(&high_id).unwrap().is_some());
}

#[test]
fn second_sight_full_queue_replaces_only_lower_value_unsubmitted_work() {
    let f = Fixture::new();
    let _ = f.open().unwrap();
    let first = f.copy(1, 113);
    let first_id = first.ciphertext.envelope().page.clone();
    let second = f.copy(2, 113);
    let second_id = second.ciphertext.envelope().page.clone();
    let preferred = f.copy(3, 113);
    let preferred_id = preferred.ciphertext.envelope().page.clone();
    let owner = preferred_id.clone();
    f.store
        .writer
        .retention()
        .set_ownership(Rc::new(move |page| page == &owner));
    f.enqueue(first).unwrap();
    f.enqueue(second).unwrap();
    f.enqueue(preferred).unwrap();
    assert!(f.store.writer.copy_only(&first_id).unwrap().is_none());
    assert!(f.store.writer.copy_only(&second_id).unwrap().is_some());
    assert!(f.store.writer.copy_only(&preferred_id).unwrap().is_some());
    assert_eq!(f.store.writer.pending_count(), 2);
    assert_eq!(f.store.writer.discarded_count(), 1);
    assert_eq!(f.enqueue(f.copy(4, 113)), Err(Error::Overloaded));
}

#[test]
fn second_sight_pressure_never_discards_submitted_owner() {
    let f = Fixture::new();
    let _ = f.open().unwrap();
    f.reactor
        .init()
        .expect("submitted fence tests require io_uring");
    let copy = f.copy(1, 113);
    let id = copy.ciphertext.envelope().page.clone();
    f.enqueue(copy).unwrap();
    let request = scope();
    let mut progress = f.store.writer.progress(1, &request);
    assert!(
        progress
            .as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
            .is_pending()
    );
    assert_eq!(f.store.writer.queued_count(), 0);
    assert_eq!(f.store.writer.reclaim_ciphertext(None, usize::MAX), 0);
    assert_eq!(f.store.writer.discard_unsubmitted(), 0);
    assert_eq!(f.store.writer.pending_count(), 1);
    drive(&f.reactor, progress).unwrap();
    assert!(f.store.writer.index().lookup(&id).unwrap().is_some());
    assert_eq!(f.store.writer.discarded_count(), 0);
}

#[test]
fn second_sight_reclaim_scans_only_a_fixed_queue_prefix_and_makes_progress() {
    let f = Fixture::new();
    let _ = f.open().unwrap();
    f.store.configure(f.admission.clone(), 128, 128).unwrap();
    for number in 0..64 {
        f.enqueue(f.copy(number, 3)).unwrap();
    }
    let metadata = f.copy(64, 3).metadata.immutable();
    let last = crate::memory::tests::bundle_for(&f.admission, metadata);
    f.enqueue(last.copy()).unwrap();
    // Arbitrary idle-page reclamation must not walk a capacity-sized suffix.
    assert_eq!(f.store.writer.discard_idle_copy(&last), 0);
    assert_eq!(f.store.writer.queued_count(), 65);
    let calls = Rc::new(Cell::new(0));
    let counted = calls.clone();
    f.store.writer.retention().set_ownership(Rc::new(move |_| {
        counted.set(counted.get() + 1);
        false
    }));
    // A missing cache still cannot scan beyond the inspection prefix.
    assert_eq!(
        f.store
            .writer
            .reclaim_ciphertext(Some(&CacheId("absent".into())), usize::MAX),
        0
    );
    assert_eq!(calls.get(), 0);
    assert!(f.store.writer.reclaim_ciphertext(None, usize::MAX) > 0);
    assert_eq!(calls.get(), QUEUE_RECLAIM_CANDIDATES);
    assert_eq!(f.store.writer.queued_count(), 1);
    assert_eq!(f.store.writer.discarded_count(), 64);
    // Later calls reach the untouched suffix; no queued page becomes pinned.
    assert!(f.store.writer.discard_idle_copy(&last) > 0);
    assert_eq!(f.store.writer.pending_count(), 0);
    assert_eq!(f.store.writer.discarded_count(), 65);
}

#[test]
fn second_sight_pending_residency_transfers_to_index_and_releases_on_cancel_or_drop() {
    let f = Fixture::new();
    let _ = f.open().unwrap();
    f.reactor.init().expect("residency tests require io_uring");
    let retention = f.store.writer.retention();
    let copy = f.copy(1, 3);
    let id = copy.ciphertext.envelope().page.clone();
    f.enqueue(copy).unwrap();
    assert_eq!(retention.snapshot().heat_entries, 1);
    retention.touch(&id);
    drive(&f.reactor, f.store.writer.progress(1, &scope())).unwrap();
    assert_eq!(retention.snapshot().heat_entries, 1);
    assert_eq!(retention.score(&id), 1);
    f.enqueue(f.copy(1, 3)).unwrap();
    assert_eq!(retention.snapshot().heat_entries, 1);
    f.enqueue(f.copy(2, 3)).unwrap();
    assert_eq!(retention.snapshot().heat_entries, 2);
    f.store.writer.cancel_pending_writes().unwrap();
    assert_eq!(retention.snapshot().heat_entries, 1);
    drop(f);
    assert_eq!(retention.snapshot().heat_entries, 0);
}
#[test]
fn optional_headroom_reclaim_preserves_owned_submitted_and_index_residency() {
    let f = Fixture::new();
    let _ = f.open().unwrap();
    f.store.configure(f.admission.clone(), 8, 16).unwrap();
    f.reactor.init().unwrap();
    let incoming = f.copy(9, 3).ciphertext.envelope().page.clone();
    let owner = incoming.clone();
    f.store
        .writer
        .retention()
        .set_ownership(Rc::new(move |id| id == &owner));
    let active = f.copy(1, 3);
    let active_id = active.ciphertext.envelope().page.clone();
    f.enqueue(active).unwrap();
    let request = scope();
    let mut progress = f.store.writer.progress(1, &request);
    assert!(
        progress
            .as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
            .is_pending()
    );
    f.enqueue(f.copy(9, 3)).unwrap();
    let optional = f.copy(2, 3);
    let optional_id = optional.ciphertext.envelope().page.clone();
    f.enqueue(optional).unwrap();
    assert_eq!(
        f.store
            .writer
            .reclaim_optional_for_owned(&active_id, None, usize::MAX, usize::MAX),
        0
    );
    assert_eq!(
        f.store
            .writer
            .reclaim_optional_for_owned(&incoming, Some(&CacheId("other".into())), 1, 1),
        0
    );
    assert_eq!(
        f.store
            .writer
            .reclaim_optional_for_owned(&incoming, None, 0, 0),
        0
    );
    assert_eq!(
        f.store
            .writer
            .reclaim_optional_for_owned(&incoming, None, 1, 1),
        1
    );
    assert!(f.store.writer.copy_only(&optional_id).unwrap().is_none());
    assert!(f.store.writer.copy_only(&incoming).unwrap().is_some());
    assert!(f.store.writer.copy_only(&active_id).unwrap().is_some());
    assert_eq!(
        f.store
            .writer
            .reclaim_optional_for_owned(&incoming, None, usize::MAX, usize::MAX),
        0
    );
    drive(&f.reactor, progress).unwrap();
    assert!(f.store.writer.index().lookup(&active_id).unwrap().is_some());
}
#[test]
fn optional_headroom_reclaim_bounds_prefix_and_stops_only_after_both_deficits() {
    let f = Fixture::new();
    let _ = f.open().unwrap();
    f.store.configure(f.admission.clone(), 128, 128).unwrap();
    let incoming = f.copy(99, 3).ciphertext.envelope().page.clone();
    let owner = incoming.clone();
    f.store
        .writer
        .retention()
        .set_ownership(Rc::new(move |page| page == &owner));
    for number in 0..65 {
        f.enqueue(f.copy(number, 3)).unwrap();
    }
    assert_eq!(
        f.store
            .writer
            .reclaim_optional_for_owned(&incoming, None, 20, 1),
        2
    );
    assert_eq!(f.store.writer.pending_count(), 63);
    for number in 65..68 {
        f.enqueue(f.copy(number, 3)).unwrap();
    }
    assert_eq!(f.store.writer.pending_count(), 66);
    assert_eq!(
        f.store
            .writer
            .reclaim_optional_for_owned(&incoming, None, usize::MAX, usize::MAX),
        64
    );
    assert_eq!(f.store.writer.pending_count(), 2);
    // Classification is current, not an immutable optional bit at enqueue.
    f.store.writer.retention().set_ownership(Rc::new(|_| true));
    assert_eq!(
        f.store
            .writer
            .reclaim_optional_for_owned(&incoming, None, usize::MAX, usize::MAX),
        0
    );
    assert_eq!(f.store.writer.pending_count(), 2);
}
static NEXT: AtomicU64 = AtomicU64::new(0);
pub(super) struct Directory(pub(super) PathBuf);
impl Directory {
    pub(super) fn new() -> Self {
        let p = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("target")
            .join(format!(
                "store-test-{}-{}",
                std::process::id(),
                NEXT.fetch_add(1, Ordering::Relaxed)
            ));
        std::fs::create_dir_all(&p).unwrap();
        Self(p)
    }
}
impl Drop for Directory {
    fn drop(&mut self) {
        std::fs::remove_dir_all(&self.0).unwrap();
    }
}
struct Fixture {
    metrics: crate::telemetry::Metrics,
    store: Store,
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    reactor: Rc<Reactor>,
    pool: BufferPool,
    _directory: Directory,
}
/// Slots of the fixture slab. Small test records share a slot.
const FIXTURE_SLOTS: u64 = 8;
impl Fixture {
    fn new() -> Self {
        Self::in_directory(Directory::new())
    }
    fn in_directory(directory: Directory) -> Self {
        Self::assemble(directory, crate::test_support::availability())
    }
    fn assemble(directory: Directory, availability: Rc<crate::control::Availability>) -> Self {
        Self::assemble_with(directory, availability, FIXTURE_SLOTS)
    }
    fn assemble_with(
        directory: Directory,
        availability: Rc<crate::control::Availability>,
        slots: u64,
    ) -> Self {
        let metrics = crate::telemetry::Metrics::default();
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let pool = BufferPool::new(admission.clone());
        let index = Rc::new(catalog::Index::new(WorkerId(0), 16, availability.clone()));
        let slabs = Rc::new(Slab::new(
            directory.0.join("worker-0-slab-0.dat"),
            slots * SLOT_BYTES,
            SLOT_BYTES,
            crate::store::MAX_RECORD_BYTES,
        ));
        let reader = Rc::new(
            StoreReader::new(
                index.clone(),
                slabs.clone(),
                admission.clone(),
                reactor.clone(),
                pool.clone(),
            )
            .with_metrics(metrics.clone()),
        );
        let writer = Rc::new(
            StoreWriter::new(
                index.clone(),
                slabs,
                admission.clone(),
                reactor.clone(),
                availability,
            )
            .with_metrics(metrics.clone()),
        );
        let store = Store {
            reader,
            writer,
            checkpoint: Rc::new(super::checkpoint::Checkpointer::new(
                directory.0.clone(),
                index.clone(),
            )),
            recovery: super::checkpoint::Recovery::new(index),
        };
        store.configure(admission.clone(), 2, 16).unwrap();
        let foreign = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            admission.policy().limits().clone(),
        )));
        assert_eq!(
            store.configure(foreign, 2, 16),
            Err(Error::InvalidConfiguration)
        );
        Self {
            metrics,
            store,
            admission,
            reactor,
            pool,
            _directory: directory,
        }
    }
    /// Publish `copy`'s page as a placeholder record reserving `reserve` bytes
    /// in `tier`, without its real record bytes, to shape slot usage.
    fn place(&self, copy: &CiphertextCopy, tier: Tier, reserve: u64) {
        let index = self.store.writer.index();
        let page = copy.ciphertext.envelope().page.clone();
        let reservation = index.allocate(tier, reserve).unwrap();
        let generation = index.next_generation().unwrap();
        let charge = self
            .admission
            .reserve(None, ResourceClass::Ciphertext, 4096)
            .unwrap();
        let buffer = self.store.writer.slabs().allocate(4096, charge).unwrap();
        let volume = index.volume().unwrap();
        let request = scope();
        let _ = drive(
            &self.reactor,
            volume.write(&*self.reactor, &reservation, buffer, &request),
        )
        .unwrap();
        assert!(
            index
                .publish_page(
                    &page,
                    reservation,
                    4096,
                    generation,
                    &copy.metadata.immutable(),
                    copy.ciphertext.envelope().key_id,
                )
                .unwrap()
        );
    }
    /// Initialize the reactor and mount the slab's volume.
    fn open(&self) -> Result<Alignment> {
        self.reactor.init()?;
        drive(&self.reactor, self.store.open(&scope()))
    }
    fn copy(&self, number: u8, length: usize) -> CiphertextCopy {
        let version = ObjectVersion {
            object: ObjectId {
                cache: CacheId(crate::test_support::security::CACHE.into()),
                key: CacheKey([number; 32]),
            },
            etag: StrongEtag::parse(b"\"v1\"").unwrap(),
        };
        let envelope = PageEnvelope {
            cipher: crate::model::PageCipher::Xaes256Gcm,
            page: PageId {
                version: version.clone(),
                number: PageNumber(0),
            },
            key_id: crate::model::key_id_from_generation(1, 1).unwrap(),
            nonce: Nonce([2; 24]),
            plaintext_length: length as u32,
            ciphertext_length: length as u32 + 16,
        };
        let reservation = self
            .admission
            .reserve(
                Some(&version.object.cache),
                ResourceClass::Ciphertext,
                length + 16,
            )
            .unwrap();
        CiphertextCopy {
            metadata: ObjectMetadata {
                content_type: None,
                version,
                length: length as u64,
                expires_at: ExpiresAt::from_system_time(std::time::UNIX_EPOCH).unwrap(),
            },
            ciphertext: self
                .pool
                .ciphertext(reservation, envelope, vec![number; length + 16])
                .unwrap(),
        }
    }
    fn enqueue(&self, copy: CiphertextCopy) -> Result<u64> {
        let dirty = self.admission.reserve(
            Some(&copy.metadata.version.object.cache),
            ResourceClass::DirtyCiphertext,
            copy.ciphertext.bytes().len(),
        )?;
        self.store.writer.enqueue(copy, dirty)
    }

    fn restart(self) -> Self {
        self.restart_with(|| {})
    }

    /// Drop every component, run `between` (for example a simulated power
    /// loss), then assemble the store again on the same directory.
    fn restart_with(self, between: impl FnOnce()) -> Self {
        let Self {
            metrics,
            store,
            admission,
            reactor,
            pool,
            _directory,
        } = self;
        assert!(store.writer.is_idle());
        assert_eq!(reactor.in_flight(), 0);
        drop((store, reactor, pool, admission, metrics));
        between();
        Self::in_directory(_directory)
    }
}
fn scope() -> RequestScope {
    RequestScope::new(RequestId([0; 16]), Instant::now() + Duration::from_secs(10)).unwrap()
}

#[test]
fn storage_requires_published_cache_and_live_keys_including_restore() {
    use crate::control::Availability;
    use crate::control::Snapshot;
    use crate::control::for_caches;
    use crate::memory::MemoryCache;
    use crate::memory::tests::bundle_for;
    use crate::test_support::security::keys;
    use crate::test_support::security::rotation_bundle;
    use controlplane::Published;
    use std::sync::Arc;

    for mode in ["unpublished", "absent", "keyless", "available"] {
        let keys = Rc::new(keys());
        let cache = CacheId(crate::test_support::security::CACHE.into());
        let availability = match mode {
            "unpublished" => Rc::new(Availability::new(
                Arc::new(Published::new(Snapshot::retention(2))),
                keys.clone(),
            )),
            "absent" => for_caches(keys.clone(), vec![]),
            _ => for_caches(keys.clone(), vec![cache.clone()]),
        };
        let revoke = || {
            let mut next = rotation_bundle(2, (*keys.peer_trust_roots().unwrap()).clone());
            next.cache_keys.clear();
            keys.install(next).unwrap();
        };
        if mode == "keyless" {
            revoke();
        }
        let f = Fixture::assemble(Directory::new(), availability.clone());
        let memory = MemoryCache::new(f.pool.clone(), availability);
        let copy = f.copy(1, 3);
        let descriptor = copy.metadata.immutable();
        let page = bundle_for(&f.admission, descriptor.clone());
        let id = page.plaintext.page().clone();
        let index = f.store.writer.index();
        index.publish_version(descriptor.clone()).unwrap();
        if mode != "available" {
            assert!(index.version(&id.version).unwrap().is_none());
            assert_eq!(
                memory.publish(page.clone()),
                Err(if mode == "keyless" {
                    Error::MissingKey
                } else {
                    Error::Unavailable
                })
            );
            assert_eq!(
                memory.publish_ciphertext(crate::memory::UnverifiedPage {
                    copy: page.copy(),
                    disk_token: None
                }),
                Err(Error::MissingKey)
            );
            assert!(matches!(f.enqueue(copy), Err(Error::MissingKey)));
            assert_eq!(f.store.writer.pending_count(), 0);
            assert!(memory.get(&id).unwrap().is_none());
            continue;
        }
        let _ = f.open().unwrap();
        f.reactor.init().unwrap();
        f.enqueue(copy).unwrap();
        drive(&f.reactor, f.store.writer.progress(1, &scope())).unwrap();
        memory.publish(page.clone()).unwrap();
        assert!(memory.get(&id).unwrap().is_some());
        assert!(index.lookup(&id).unwrap().is_some());
        assert_eq!(index.version(&id.version).unwrap(), Some(descriptor));
        let snapshot = index.snapshot().unwrap();
        revoke();
        assert!(memory.get(&id).unwrap().is_none());
        assert!(index.lookup(&id).unwrap().is_none());
        assert_eq!(memory.publish(page.clone()), Err(Error::MissingKey));
        assert!(matches!(f.enqueue(page.copy()), Err(Error::MissingKey)));
        index.restore(snapshot).unwrap();
        assert!(index.lookup(&id).unwrap().is_none());
        assert!(index.snapshot().unwrap().metadata.is_empty());
        assert_eq!(page.plaintext.bytes(), &[1; 3]);
    }
}
fn drive<T>(reactor: &Reactor, future: impl Future<Output = Result<T>>) -> Result<T> {
    let mut future = std::pin::pin!(future);
    let waker = futures::task::noop_waker();
    let mut cx = Context::from_waker(&waker);
    let deadline = Instant::now() + Duration::from_secs(10);
    loop {
        if let Poll::Ready(result) = future.as_mut().poll(&mut cx) {
            return result;
        }
        reactor.poll_budgeted(32)?;
        assert!(Instant::now() < deadline, "storage I/O did not complete");
        std::thread::yield_now();
    }
}
#[test]
fn concurrent_writes_reserve_distinct_extents_and_capacity_before_completion() {
    let f = Fixture::new();
    let _ = f.open().unwrap();
    let a = f.copy(21, 4096);
    let b = f.copy(22, 4096);
    let aid = a.ciphertext.envelope().page.clone();
    let bid = b.ciphertext.envelope().page.clone();
    f.enqueue(a).unwrap();
    f.enqueue(b).unwrap();
    let scope = scope();
    let mut writes = f.store.writer.progress(8, &scope);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(writes.as_mut().poll(&mut cx).is_pending());
    assert_eq!(f.store.writer.writes_in_flight(), 2);
    assert!(f.store.writer.index().lookup(&aid).unwrap().is_none());
    assert!(f.store.writer.index().lookup(&bid).unwrap().is_none());
    assert_eq!(drive(&f.reactor, writes).unwrap(), 2);
    let a = f.store.writer.index().lookup(&aid).unwrap().unwrap();
    let b = f.store.writer.index().lookup(&bid).unwrap().unwrap();
    assert_ne!(a.offset(), b.offset());
    assert!(f.store.writer.is_idle());
    for (id, expected) in [(&aid, 21), (&bid, 22)] {
        let copy = drive(&f.reactor, f.store.reader.read(id, &scope))
            .unwrap()
            .unwrap();
        assert_eq!(copy.ciphertext.bytes(), vec![expected; 4112]);
    }
}

#[test]
fn pipeline_out_of_order_failure_and_short_cqes_preserve_other_mapping() {
    use uring_runtime::reactor::simulation::Fault;
    use uring_runtime::reactor::simulation::Simulation;
    for fault in [Fault::Delay(6), Fault::Errno(libc::EIO), Fault::Short(512)] {
        let simulation = Simulation::new();
        let _environment = simulation.enter();
        let f = Fixture::new();
        let _ = f.open().unwrap();
        let a = f.copy(21, 4096);
        let b = f.copy(22, 4096);
        let aid = a.ciphertext.envelope().page.clone();
        let bid = b.ciphertext.envelope().page.clone();
        f.enqueue(a).unwrap();
        f.enqueue(b).unwrap();
        simulation.inject("write", fault).unwrap();
        let scope = scope();
        let mut writes = f.store.writer.progress(8, &scope);
        let mut cx = Context::from_waker(std::task::Waker::noop());
        assert!(writes.as_mut().poll(&mut cx).is_pending());
        assert_eq!(f.store.writer.writes_in_flight(), 2);
        let _ = drive(&f.reactor, writes);
        assert!(f.store.writer.is_idle());
        assert!(f.store.writer.index().lookup(&bid).unwrap().is_some());
        if f.store.writer.index().lookup(&aid).unwrap().is_some() {
            let copy = drive(&f.reactor, f.store.reader.read(&aid, &scope))
                .unwrap()
                .unwrap();
            assert_eq!(copy.ciphertext.bytes(), vec![21; 4112]);
        }
        assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
    }
}
#[test]
fn delayed_write_keeps_its_leased_slot_through_eviction() {
    use uring_runtime::reactor::simulation::{Fault, Simulation};
    let simulation = Simulation::new();
    let _environment = simulation.enter();
    let f = Fixture::new();
    let _ = f.open().unwrap();
    let first = f.copy(1, 113);
    let first_id = first.ciphertext.envelope().page.clone();
    f.enqueue(first).unwrap();
    drive(&f.reactor, f.store.writer.progress(1, &scope())).unwrap();
    f.store.writer.retention().touch(&first_id);
    let delayed = f.copy(2, 9000);
    let id = delayed.ciphertext.envelope().page.clone();
    f.enqueue(delayed).unwrap();
    simulation.inject("write", Fault::Delay(6)).unwrap();
    let request = scope();
    let mut progress = f.store.writer.progress(1, &request);
    assert!(
        progress
            .as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
            .is_pending()
    );
    assert!(f.store.writer.index().lookup(&id).unwrap().is_none());
    // Evict everything evictable while the write is in flight.
    let volume = f.store.writer.index().volume().unwrap();
    while volume.evict_one(u64::MAX).is_some() {}
    drive(&f.reactor, progress).unwrap();
    assert!(f.store.writer.index().lookup(&id).unwrap().is_some());
    assert_eq!(f.store.writer.discarded_count(), 0);
}

#[test]
fn checkpoint_infeasible_budget_does_not_disturb_an_inflight_writer() {
    use uring_runtime::reactor::simulation::{Fault, Simulation};
    let simulation = Simulation::new();
    let _environment = simulation.enter();
    let f = Fixture::new();
    let _ = f.open().unwrap();
    f.reactor.init().unwrap();
    let first = f.copy(1, 113);
    let metadata = first.metadata.immutable();
    f.enqueue(first).unwrap();
    drive(&f.reactor, f.store.writer.progress(1, &scope())).unwrap();
    let index = f.store.writer.index();
    index.publish_version(metadata.clone()).unwrap();
    index
        .reserve_hold(
            &metadata.version,
            1,
            uring_runtime::environment::wall_now() + Duration::from_secs(60),
        )
        .unwrap();
    let mut version = super::versions::VersionEntry::new(metadata.clone());
    version.add_key_id(crate::model::key_id_from_generation(1, 1).unwrap());
    let base = super::checkpoint::SHARD_BYTES;
    let versions = base + super::checkpoint::snapshot_version_bytes(&version);
    let descriptors = versions + super::checkpoint::snapshot_metadata_bytes(&metadata);

    let copy = f.copy(2, 113);
    let id = copy.ciphertext.envelope().page.clone();
    f.enqueue(copy).unwrap();
    simulation.inject("write", Fault::Delay(6)).unwrap();
    let request = scope();
    let mut progress = f.store.writer.progress(1, &request);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(progress.as_mut().poll(&mut cx).is_pending());
    assert_eq!(f.store.writer.writes_in_flight(), 1);
    for budget in [0, base - 1, versions - 1, descriptors - 1] {
        let mut snapshot = f.store.checkpoint.snapshot_incremental(budget);
        assert!(matches!(
            snapshot.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Overloaded))
        ));
        assert!(f.store.writer.waiters.borrow().is_empty());
        assert_eq!(f.store.writer.writes_in_flight(), 1);
        assert!(!f.store.checkpoint.is_snapshot_taken());
    }
    drive(&f.reactor, progress).unwrap();
    assert!(index.lookup(&id).unwrap().is_some());
    f.enqueue(f.copy(3, 113)).unwrap();
    assert_eq!(
        drive(&f.reactor, f.store.writer.progress(1, &scope())),
        Ok(1)
    );
    assert_eq!(f.store.writer.discarded_count(), 0);
}

#[test]
fn checkpoint_exact_budget_matches_full_snapshot() {
    use super::checkpoint::{CHECKPOINT_VERSION, CheckpointImage, encode};
    let simulation = uring_runtime::reactor::simulation::Simulation::new();
    let _environment = simulation.enter();
    let f = Fixture::new();
    let _ = f.open().unwrap();
    f.enqueue(f.copy(1, 113)).unwrap();
    drive(&f.reactor, f.store.writer.progress(1, &scope())).unwrap();
    let index = f.store.writer.index();
    let version = index.snapshot().unwrap().versions[0].clone();
    let metadata = version.metadata.clone();
    index.publish_version(metadata.clone()).unwrap();
    let budget = super::checkpoint::SHARD_BYTES
        + super::checkpoint::snapshot_version_bytes(&version)
        + super::checkpoint::snapshot_metadata_bytes(&metadata);
    assert!(matches!(
        drive(
            &f.reactor,
            f.store.checkpoint.snapshot_incremental(budget - 1)
        ),
        Err(Error::Overloaded)
    ));
    let full = drive(&f.reactor, f.store.checkpoint.snapshot_shard()).unwrap();
    f.store.checkpoint.finish_snapshot();
    let incremental = drive(&f.reactor, f.store.checkpoint.snapshot_incremental(budget)).unwrap();
    let bytes = |shard| {
        encode(&CheckpointImage {
            version: CHECKPOINT_VERSION,
            sequence: 1,
            shards: vec![shard],
        })
        .unwrap()
    };
    let full = bytes(full);
    assert_eq!(bytes(incremental), full);
    assert_eq!(full.len(), budget + 32 + 4 + 32);
    f.store.checkpoint.finish_snapshot();
}

#[test]
fn incremental_checkpoint_budget_thaws_and_async_publication_roundtrips() {
    let f = Fixture::new();
    let _ = f.open().unwrap();
    f.enqueue(f.copy(1, 4096)).unwrap();
    drive(&f.reactor, f.store.writer.progress(8, &scope())).unwrap();
    assert!(matches!(
        drive(&f.reactor, f.store.checkpoint.snapshot_incremental(1)),
        Err(Error::Overloaded)
    ));
    let shard = drive(
        &f.reactor,
        f.store.checkpoint.snapshot_incremental(1024 * 1024),
    )
    .unwrap();
    assert_eq!(shard.index.versions.len(), 1);
    let task = f
        .store
        .checkpoint
        .publish_async(vec![shard], f.reactor.clone(), scope(), 7, 1, 1024 * 1024)
        .unwrap();
    drive(&f.reactor, task).unwrap();
    f.store.checkpoint.finish_snapshot();
    let bytes = std::fs::read(f._directory.0.join("checkpoint.1")).unwrap();
    let image = super::checkpoint::decode(&bytes).unwrap();
    assert_eq!(image.sequence, 7);
    assert_eq!(image.shards[0].index.versions.len(), 1);
}

#[test]
fn checkpoint_preflights_metadata_and_version_clones() {
    let f = Fixture::new();
    let _ = f.open().unwrap();
    f.enqueue(f.copy(1, 4096)).unwrap();
    drive(&f.reactor, f.store.writer.progress(8, &scope())).unwrap();
    let index = f.store.writer.index();
    let version = index.snapshot().unwrap().versions[0].clone();
    let metadata = version.metadata.clone();
    index.publish_version(metadata.clone()).unwrap();
    let item = super::checkpoint::snapshot_version_bytes(&version);
    let descriptor = super::checkpoint::snapshot_metadata_bytes(&metadata);
    let base = super::checkpoint::SHARD_BYTES;
    for budget in [
        base - 1,
        base + descriptor - 1,
        base + descriptor + item - 1,
    ] {
        assert!(matches!(
            drive(&f.reactor, f.store.checkpoint.snapshot_incremental(budget)),
            Err(Error::Overloaded)
        ));
        // Failure must release the cut before a subsequent one.
        assert!(!f.store.checkpoint.is_snapshot_taken());
    }
    let mut budget = descriptor - 1;
    assert!(matches!(
        index.snapshot_metadata_budgeted(&mut budget),
        Err(Error::Overloaded)
    ));
    let mut budget = item - 1;
    assert!(matches!(
        index.snapshot_versions_budgeted(None, 128, &mut budget),
        Err(Error::Overloaded)
    ));
    let image = drive(
        &f.reactor,
        f.store
            .checkpoint
            .snapshot_incremental(base + descriptor + item),
    )
    .unwrap();
    assert_eq!(image.index.versions.len(), 1);
    assert_eq!(image.index.metadata.len(), 1);
    f.store.checkpoint.finish_snapshot();
}

#[test]
fn checkpoint_directory_startup_is_bounded_and_simulation_compatible() {
    use uring_runtime::reactor::simulation::Simulation;
    let sim = Simulation::new();
    let _os = sim.enter();
    let f = Fixture::new();
    let directory = PathBuf::from("/checkpoint-new/nested");
    let checkpoint =
        super::checkpoint::Checkpointer::new(directory.clone(), f.store.writer.index().clone());
    assert!(sim.metadata(&directory).is_err());
    drive(
        &f.reactor,
        Box::pin(checkpoint.prepare_directory(&f.reactor, &scope())),
    )
    .unwrap();
    assert!(sim.metadata(&directory).is_ok());
    drive(
        &f.reactor,
        Box::pin(checkpoint.prepare_directory(&f.reactor, &scope())),
    )
    .unwrap();
    let bad = super::checkpoint::Checkpointer::new(
        PathBuf::from("/checkpoint-new/../bad"),
        f.store.writer.index().clone(),
    );
    assert!(
        drive(
            &f.reactor,
            Box::pin(bad.prepare_directory(&f.reactor, &scope()))
        )
        .is_err()
    );
    let canceled = scope();
    canceled.cancel().unwrap();
    assert_eq!(
        drive(
            &f.reactor,
            Box::pin(checkpoint.prepare_directory(&f.reactor, &canceled))
        ),
        Err(Error::Cancelled)
    );
}

#[test]
fn record_round_trip_preserves_ciphertext_zeroes_padding_and_rejects_torn_header() {
    let f = Fixture::new();
    let a = Alignment::new(512, 512, 512).unwrap();
    for length in [3, crate::model::PAGE_BYTES as usize] {
        let page = f.copy(9, length);
        let disk = a
            .extent(0, crate::store::logical_length(&page).unwrap())
            .unwrap();
        let reserve = f
            .admission
            .reserve(None, ResourceClass::Ciphertext, disk.length())
            .unwrap();
        let buffer = a.allocate(disk.length(), reserve).unwrap();
        assert_eq!(buffer.bytes().unwrap().as_ptr() as usize % a.memory(), 0);
        let mut encoded = crate::store::encode(&page, Generation(7), a, buffer).unwrap();
        let parsed = crate::store::parse(&encoded.buffer, disk).unwrap();
        assert_eq!(
            &encoded.buffer.bytes().unwrap()[parsed.ciphertext.clone()],
            page.ciphertext.bytes()
        );
        assert_eq!(parsed.header.metadata, page.metadata.immutable());
        assert!(
            encoded.buffer.bytes().unwrap()[parsed.ciphertext.end..]
                .iter()
                .all(|b| *b == 0)
        );
        assert_eq!(
            crate::store::decode(&encoded.buffer, &encoded.header).unwrap(),
            *page.ciphertext.envelope()
        );
        encoded.header.generation = Generation(8);
        assert!(crate::store::decode(&encoded.buffer, &encoded.header).is_err());
        encoded.buffer.bytes_mut().unwrap()[24] ^= 1;
        assert!(crate::store::parse(&encoded.buffer, disk).is_err());
    }
}
#[test]
fn dirty_queue_is_bounded_and_cache_removal_discards_without_io() {
    let f = Fixture::new();
    let _ = f.open().unwrap();
    let first = f.copy(1, 3);
    let id = first.ciphertext.envelope().page.clone();
    let ticket = f.enqueue(first.clone()).unwrap();
    assert_eq!(f.enqueue(first).unwrap(), ticket);
    f.enqueue(f.copy(2, 3)).unwrap();
    assert!(matches!(f.enqueue(f.copy(3, 3)), Err(Error::Overloaded)));
    assert_eq!(f.metrics.count(crate::telemetry::Event::DirtyDiscard), 0);
    assert!(f.store.writer.copy_only(&id).unwrap().is_some());
    f.store
        .writer
        .remove_cache(&CacheId(crate::test_support::security::CACHE.into()))
        .unwrap();
    assert_eq!(f.store.writer.pending_count(), 0);
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
    // Eviction alone has no permanent tombstone; production admission is the
    // current positive cache/key set, independently tested through the app.
    assert!(f.enqueue(f.copy(4, 3)).is_ok());
    assert_eq!(f.metrics.count(crate::telemetry::Event::DirtyDiscard), 2);
}

#[test]
fn allocator_integration_rejects_foreign_and_mismatched_writer_charges() {
    let f = Fixture::new();
    let _ = f.open().unwrap();
    let foreign =
        flow_control::Quotas::new(AdmissionPolicy::new(f.admission.policy().limits().clone()));
    let page = f.copy(1, 64);
    let cache = &page.metadata.version.object.cache;
    let other_cache = CacheId("other".into());
    for (owner, class, cache, amount) in [
        (&foreign, ResourceClass::DirtyCiphertext, cache, 80),
        (&*f.admission, ResourceClass::Ciphertext, cache, 80),
        (
            &*f.admission,
            ResourceClass::DirtyCiphertext,
            &other_cache,
            80,
        ),
        (&*f.admission, ResourceClass::DirtyCiphertext, cache, 79),
    ] {
        let dirty = owner.reserve(Some(cache), class, amount).unwrap();
        assert_eq!(
            f.store.writer.enqueue(page.clone(), dirty),
            Err(Error::InvalidConfiguration)
        );
        assert_eq!(f.store.writer.pending_count(), 0);
    }
    for variant in 0..4 {
        let dirty = f
            .admission
            .reserve(Some(cache), ResourceClass::DirtyCiphertext, 80)
            .unwrap();
        let result = f
            .store
            .writer
            .enqueue_reclaiming(page.clone(), dirty, |length| {
                let owner = if variant == 0 {
                    &foreign
                } else {
                    &*f.admission
                };
                let class = if variant == 1 {
                    ResourceClass::DirtyCiphertext
                } else {
                    ResourceClass::Ciphertext
                };
                let cache = if variant == 2 { &other_cache } else { cache };
                owner
                    .reserve(Some(cache), class, length - usize::from(variant == 3))
                    .map_err(Into::into)
            });
        assert_eq!(result, Err(Error::InvalidConfiguration));
        assert_eq!(f.store.writer.pending_count(), 0);
    }
    assert_eq!(foreign.used(ResourceClass::Ciphertext), 0);
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
    assert_eq!(f.reactor.in_flight(), 0);
    f.enqueue(page).unwrap();
    assert_eq!(f.store.writer.pending_count(), 1);
}

#[test]
fn allocator_integration_validates_reader_charges_before_submission() {
    let f = Fixture::new();
    let _ = f.open().unwrap();
    let page = f.copy(1, 64);
    let id = page.ciphertext.envelope().page.clone();
    let cache = &id.version.object.cache;
    let foreign =
        flow_control::Quotas::new(AdmissionPolicy::new(f.admission.policy().limits().clone()));
    let other_cache = CacheId("other".into());
    f.place(&page, Tier::Cache, 4096);
    for variant in 0..4 {
        let request = scope();
        let result = futures::executor::block_on(f.store.reader.read_with_token_reclaim(
            &id,
            &request,
            |amount| {
                let owner = if variant == 0 {
                    &foreign
                } else {
                    &*f.admission
                };
                let class = if variant == 1 {
                    ResourceClass::DirtyCiphertext
                } else {
                    ResourceClass::Ciphertext
                };
                let cache = if variant == 2 { &other_cache } else { cache };
                owner
                    .reserve(Some(cache), class, amount - usize::from(variant == 3))
                    .map_err(Into::into)
            },
        ));
        assert!(matches!(result, Err(Error::InvalidConfiguration)));
        assert_eq!(f.reactor.in_flight(), 0);
        assert!(f.store.writer.index().lookup(&id).unwrap().is_some());
    }
    assert_eq!(foreign.used(ResourceClass::Ciphertext), 0);
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
}

#[test]
fn allocator_integration_reclaims_idle_charge_before_writer_admission_retry() {
    let f = Fixture::new();
    let _ = f.open().unwrap();
    let page = f.copy(1, 64);
    let cache = &page.metadata.version.object.cache;
    let slab = f.store.writer.slabs();
    let length = slab
        .alignment()
        .unwrap()
        .extent(0, crate::store::logical_length(&page).unwrap())
        .unwrap()
        .length();
    let charge = f
        .admission
        .reserve(Some(cache), ResourceClass::Ciphertext, length)
        .unwrap();
    drop(slab.allocate(length, charge).unwrap());
    assert_eq!(slab.idle_bytes(), length);
    let pressure = f
        .admission
        .reserve(
            None,
            ResourceClass::Ciphertext,
            f.admission.limit(ResourceClass::Ciphertext)
                - f.admission.used(ResourceClass::Ciphertext),
        )
        .unwrap();
    f.enqueue(page).unwrap();
    assert_eq!(slab.idle_bytes(), 0);
    assert_eq!(f.store.writer.pending_count(), 1);
    assert_eq!(f.store.writer.discard_unsubmitted(), 1);
    drop(pressure);
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
}

#[test]
fn store_open_rejects_too_small_slab_and_reopens_idempotently() {
    let small = Fixture::assemble_with(Directory::new(), crate::test_support::availability(), 3);
    assert_eq!(small.open(), Err(Error::InvalidConfiguration));
    assert!(!small.store.writer.index().is_mounted());
    let f = Fixture::new();
    let alignment = f.open().unwrap();
    assert!(f.store.writer.index().is_mounted());
    assert_eq!(f.open(), Ok(alignment));
    assert_eq!(
        f.store.writer.index().volume().unwrap().slot_count(),
        FIXTURE_SLOTS
    );
}

#[test]
fn enqueue_before_mount_is_unavailable_and_releases_charges_without_io() {
    let f = Fixture::new();
    assert!(matches!(f.enqueue(f.copy(1, 64)), Err(Error::Unavailable)));
    assert_eq!(f.store.writer.pending_count(), 0);
    assert_eq!(f.store.writer.queued_count(), 0);
    assert_eq!(f.reactor.in_flight(), 0);
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
    // Reads of an unmounted disk miss.
    let id = f.copy(2, 64).ciphertext.envelope().page.clone();
    assert!(
        futures::executor::block_on(f.store.reader.read(&id, &scope()))
            .unwrap()
            .is_none()
    );
}

#[test]
fn real_writer_serializes_progress_and_replaces_invalidated_pages() {
    let f = Fixture::new();
    let _ = f.open().expect("storage test requires io_uring");
    let index = f.store.writer.index();
    let first = f.copy(1, 64);
    let first_id = first.ciphertext.envelope().page.clone();
    let second = f.copy(2, 64);
    let second_id = second.ciphertext.envelope().page.clone();
    f.enqueue(first).unwrap();
    f.enqueue(second).unwrap();
    assert_eq!(
        f.metrics.gauge(crate::telemetry::Gauge::PendingDiskWrites),
        2
    );
    assert_eq!(f.metrics.count(crate::telemetry::Event::DiskPublication), 0);
    let request = scope();
    let mut progress = f.store.writer.progress(1, &request);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(progress.as_mut().poll(&mut cx).is_pending());
    assert_eq!(f.store.writer.writes_in_flight(), 1);
    assert_eq!(
        futures::executor::block_on(f.store.writer.progress(1, &request)),
        Err(Error::Overloaded)
    );
    assert_eq!(drive(&f.reactor, progress).unwrap(), 1);
    let original = index.lookup(&first_id).unwrap().unwrap();
    assert_eq!(f.metrics.count(crate::telemetry::Event::DiskPublication), 1);
    assert_eq!(
        f.metrics.gauge(crate::telemetry::Gauge::PendingDiskWrites),
        1
    );
    let mut progress = f.store.writer.progress(1, &request);
    assert!(progress.as_mut().poll(&mut cx).is_pending());
    assert_eq!(drive(&f.reactor, progress).unwrap(), 1);
    assert!(index.lookup(&second_id).unwrap().is_some());
    assert!(index.lookup(&first_id).unwrap().is_some());
    assert_eq!(f.metrics.count(crate::telemetry::Event::DiskPublication), 2);
    assert_eq!(
        f.metrics.gauge(crate::telemetry::Gauge::PendingDiskWrites),
        0
    );
    assert_eq!(f.store.writer.discarded_count(), 0);
    assert!(f.store.writer.is_idle());
    assert_eq!(f.reactor.in_flight(), 0);
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
    f.store.writer.reclaim_idle_buffer();
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
    // A resident duplicate consumes neither a write nor a publication.
    let records = index.volume().unwrap().usage().records;
    assert_eq!(f.enqueue(f.copy(1, 64)), Ok(0));
    assert_eq!(f.store.writer.pending_count(), 0);
    assert_eq!(
        drive(&f.reactor, f.store.writer.progress(1, &request)).unwrap(),
        0
    );
    assert_eq!(index.lookup(&first_id).unwrap(), Some(original));
    assert_eq!(index.volume().unwrap().usage().records, records);
    // Once invalidated, an actual same-key replacement remains admissible.
    index.remove_if_matches(&first_id, &original);
    f.enqueue(f.copy(1, 64)).unwrap();
    drive(&f.reactor, f.store.writer.progress(1, &request)).unwrap();
    let latest = index.lookup(&first_id).unwrap().unwrap();
    assert!(latest.generation > original.generation);
    index.remove_if_matches(&first_id, &original);
    assert_eq!(index.lookup(&first_id).unwrap(), Some(latest));
    let read = drive(&f.reactor, f.store.reader.read(&first_id, &request))
        .unwrap()
        .unwrap();
    assert_eq!(read.ciphertext.bytes(), &[1; 80]);
    drop(read);
    assert!(f.store.writer.is_idle());
    assert_eq!(f.reactor.in_flight(), 0);
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
    f.store.writer.reclaim_idle_buffer();
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
}

#[test]
fn persistence_and_restart_preserve_committed_pages_and_metadata() {
    for length in [3, 4096, 65536] {
        let f = Fixture::new();
        let _ = f.open().unwrap();
        let request = scope();
        let mut expected = Vec::new();
        for number in 1..=6 {
            let page = f.copy(number, length);
            expected.push(page.ciphertext.envelope().page.clone());
            f.enqueue(page).unwrap();
            drive(&f.reactor, f.store.writer.progress(8, &request)).unwrap();
            let copy = drive(
                &f.reactor,
                f.store.reader.read(expected.last().unwrap(), &request),
            )
            .unwrap()
            .unwrap();
            assert_eq!(copy.ciphertext.bytes(), vec![number; length + 16]);
        }
        let image = drive(&f.reactor, f.store.checkpoint.snapshot_shard()).unwrap();
        assert_eq!(image.index.versions.len(), 6);
        drive(&f.reactor, f.store.checkpoint.publish(vec![image])).unwrap();
        f.store.checkpoint.finish_snapshot();
        drive(&f.reactor, f.store.writer.drain(&request)).unwrap();
        assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);

        // Drop every old component, then reopen the same actual slab/checkpoint files.
        let f = f.restart();
        // Ring teardown may release the kernel's final file reference after the
        // userspace owners drop. Bound the wait for the exclusive slab lock;
        // an actual leaked owner must still fail the restart scenario.
        let unlock_deadline = Instant::now() + Duration::from_secs(2);
        loop {
            match f.open() {
                Ok(_) => break,
                Err(Error::Unavailable) if Instant::now() < unlock_deadline => {
                    std::thread::sleep(Duration::from_millis(1));
                }
                result => panic!("slab did not reopen after owner teardown: {result:?}"),
            }
        }
        let image = drive(&f.reactor, f.store.recovery.load(&f._directory.0))
            .unwrap()
            .unwrap();
        drive(
            &f.reactor,
            f.store
                .recovery
                .install_shard(image.shards.into_iter().next()),
        )
        .unwrap();
        let index = f.store.writer.index();
        for (number, id) in (1..).zip(&expected) {
            assert!(index.version(&id.version).unwrap().is_some());
            let copy = drive(&f.reactor, f.store.reader.read(id, &request))
                .unwrap()
                .unwrap();
            assert_eq!(copy.ciphertext.bytes(), vec![number; length + 16]);
            assert_eq!(copy.ciphertext.envelope().page, *id);
        }
    }
}

#[test]
fn real_direct_slab_roundtrip_checkpoint_and_corruption_miss() {
    let f = Fixture::new();
    let _alignment = f
        .open()
        .expect("test filesystem must support O_DIRECT and STATX_DIOALIGN");
    f.reactor.init().expect("storage test requires io_uring");
    let page = f.copy(3, 23);
    let id = page.ciphertext.envelope().page.clone();
    f.enqueue(page.clone()).unwrap();
    assert!(f.store.writer.index().lookup(&id).unwrap().is_none());
    let request = scope();
    assert_eq!(
        drive(&f.reactor, f.store.writer.progress(1, &request)).unwrap(),
        1
    );
    let (read, token) = drive(&f.reactor, f.store.reader.read_with_token(&id, &request))
        .unwrap()
        .unwrap();
    assert_eq!(read.ciphertext.bytes(), page.ciphertext.bytes());
    assert_eq!(read.ciphertext.envelope(), page.ciphertext.envelope());
    let image = futures::executor::block_on(f.store.checkpoint.snapshot_shard()).unwrap();
    futures::executor::block_on(f.store.checkpoint.publish(vec![image])).unwrap();
    f.store.checkpoint.finish_snapshot();
    let loaded = futures::executor::block_on(f.store.recovery.load(&f._directory.0))
        .unwrap()
        .unwrap();
    futures::executor::block_on(
        f.store
            .recovery
            .install_shard(loaded.shards.into_iter().next()),
    )
    .unwrap();
    assert!(
        drive(&f.reactor, f.store.reader.read(&id, &request))
            .unwrap()
            .is_some()
    );
    drive(
        &f.reactor,
        f.store
            .writer
            .rewrite_record(&id, &request, |bytes| bytes.fill(0)),
    )
    .unwrap();
    assert!(
        drive(&f.reactor, f.store.reader.read(&id, &request))
            .unwrap()
            .is_none()
    );
    assert!(f.store.writer.index().lookup(&id).unwrap().is_none());
    f.store.reader.invalidate(&token).unwrap();
    assert!(
        drive(&f.reactor, f.store.reader.read(&id, &request))
            .unwrap()
            .is_none()
    );
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
    assert_eq!(f.metrics.count(crate::telemetry::Event::CorruptMiss), 1);
}

#[test]
fn other_format_version_and_unknown_cipher_records_are_cache_misses() {
    let f = Fixture::new();
    let _ = f.open().unwrap();
    f.reactor.init().unwrap();
    let page = f.copy(3, 23);
    let id = page.ciphertext.envelope().page.clone();
    let request = scope();
    for other_version in [true, false] {
        f.enqueue(page.clone()).unwrap();
        drive(&f.reactor, f.store.writer.progress(1, &request)).unwrap();
        drive(
            &f.reactor,
            f.store.writer.rewrite_record(&id, &request, |bytes| {
                let header_len = u32::from_le_bytes(bytes[12..16].try_into().unwrap()) as usize;
                let cipher_offset = header_len - 36;
                if other_version {
                    // A well-formed record from another format version.
                    bytes[8..12].copy_from_slice(&(FORMAT_VERSION + 1).to_le_bytes());
                } else {
                    bytes[cipher_offset..cipher_offset + 4]
                        .copy_from_slice(&u32::MAX.to_le_bytes());
                }
                let digest_end = header_len - 32;
                let digest = Sha256::digest(&bytes[..digest_end]);
                bytes[digest_end..digest_end + 32].copy_from_slice(&digest);
                let extent = Extent::new(0, bytes.len()).unwrap();
                assert!(matches!(
                    super::parse_bytes(bytes, extent),
                    Err(Error::CorruptRecord)
                ));
            }),
        )
        .unwrap();
        assert!(
            drive(&f.reactor, f.store.reader.read(&id, &request))
                .unwrap()
                .is_none()
        );
        assert!(f.store.writer.index().lookup(&id).unwrap().is_none());
        assert_eq!(
            f.metrics.count(crate::telemetry::Event::CorruptMiss),
            u64::from(!other_version)
        );
    }
}

#[test]
fn stored_payload_and_tag_corruption_fail_mandatory_checksum() {
    let f = Fixture::new();
    let _ = f.open().unwrap();
    f.reactor.init().unwrap();
    let page = f.copy(3, 23);
    let id = page.ciphertext.envelope().page.clone();
    let request = scope();
    for corrupt_tag in [false, true] {
        f.enqueue(page.clone()).unwrap();
        drive(&f.reactor, f.store.writer.progress(1, &request)).unwrap();
        let checksum = page.ciphertext.checksum();
        drive(
            &f.reactor,
            f.store.writer.rewrite_record(&id, &request, |bytes| {
                let extent = Extent::new(0, bytes.len()).unwrap();
                let parsed = super::parse_bytes(bytes, extent).unwrap();
                assert_eq!(parsed.header.format_version, super::FORMAT_VERSION);
                assert_eq!(parsed.checksum, checksum);
                let offset = if corrupt_tag {
                    parsed.ciphertext.end - 1
                } else {
                    parsed.ciphertext.start
                };
                bytes[offset] ^= 1;
            }),
        )
        .unwrap();
        let (read, token) = drive(&f.reactor, f.store.reader.read_with_token(&id, &request))
            .unwrap()
            .unwrap();
        // The I/O worker carries the stored expectation; crypto-worker validation
        // rejects corruption before AEAD can publish plaintext.
        assert_eq!(read.ciphertext.checksum(), page.ciphertext.checksum());
        assert_eq!(read.ciphertext.verify_checksum(), Err(Error::CorruptRecord));
        f.store.reader.invalidate(&token).unwrap();
        assert!(f.store.writer.index().lookup(&id).unwrap().is_none());
    }
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
}

#[test]
fn abandoned_write_retains_kernel_lease_and_cannot_publish() {
    let f = Fixture::new();
    let _ = f.open().unwrap();
    f.reactor.init().unwrap();
    let page = f.copy(1, 64);
    let id = page.ciphertext.envelope().page.clone();
    f.enqueue(page).unwrap();
    let request = scope();
    let mut operation = f.store.writer.progress(1, &request);
    let waker = futures::task::noop_waker();
    let mut cx = Context::from_waker(&waker);
    assert!(operation.as_mut().poll(&mut cx).is_pending());
    assert!(f.reactor.in_flight() > 0);
    drop(operation);
    assert!(f.store.writer.index().lookup(&id).unwrap().is_none());
    assert_eq!(f.metrics.count(crate::telemetry::Event::DirtyDiscard), 1);
    assert_eq!(f.store.writer.pending_count(), 0);
    assert_eq!(
        f.metrics.gauge(crate::telemetry::Gauge::PendingDiskWrites),
        0
    );
    assert_eq!(f.metrics.count(crate::telemetry::Event::DiskPublication), 0);
    assert!(f.admission.used(ResourceClass::Ciphertext) > 0);
    assert!(!f.store.writer.is_idle());
    assert_eq!(f.store.writer.writes_in_flight(), 1);
    let deadline = Instant::now() + Duration::from_secs(10);
    while f.reactor.in_flight() != 0 {
        f.reactor.poll_budgeted(32).unwrap();
        assert!(Instant::now() < deadline);
    }
    f.store.writer.reclaim_idle_buffer();
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
    assert!(f.store.writer.is_idle());
    assert!(f.store.writer.index().lookup(&id).unwrap().is_none());
}

#[test]
fn cache_removal_during_write_fences_late_publication() {
    let f = Fixture::new();
    let _ = f.open().unwrap();
    f.reactor.init().unwrap();
    let page = f.copy(1, 64);
    let id = page.ciphertext.envelope().page.clone();
    f.enqueue(page).unwrap();
    let request = scope();
    let mut operation = f.store.writer.progress(1, &request);
    let waker = futures::task::noop_waker();
    let mut cx = Context::from_waker(&waker);
    assert!(operation.as_mut().poll(&mut cx).is_pending());
    f.store
        .writer
        .remove_cache(&CacheId(crate::test_support::security::CACHE.into()))
        .unwrap();
    // Reinsert the same immutable page while the original submitted write still
    // owns its segment/buffer. The old completion cannot publish or remove it.
    let replacement = f.enqueue(f.copy(1, 64)).unwrap();
    assert!(replacement > 1);
    drive(&f.reactor, operation).unwrap();
    assert!(f.store.writer.index().lookup(&id).unwrap().is_none());
    assert_eq!(f.store.writer.pending_count(), 1);
    assert_eq!(
        f.metrics.gauge(crate::telemetry::Gauge::PendingDiskWrites),
        1
    );
    assert_eq!(f.metrics.count(crate::telemetry::Event::DiskPublication), 0);
    drive(&f.reactor, f.store.writer.progress(1, &request)).unwrap();
    assert_eq!(
        f.metrics.gauge(crate::telemetry::Gauge::PendingDiskWrites),
        0
    );
    assert_eq!(f.metrics.count(crate::telemetry::Event::DiskPublication), 1);
    assert!(f.store.writer.index().lookup(&id).unwrap().is_some());
    assert_eq!(f.store.writer.pending_count(), 0);
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
}

#[test]
fn sustained_rotation_reclaims_history_and_fences_held_pages_and_write_completions() {
    use crate::control::for_caches;
    use crate::memory::MemoryCache;
    use crate::memory::tests::bundle_for;
    use crate::test_support::security::keys;
    use crate::test_support::security::rotation_bundle;
    use racer_crypto::identity::KeyPurpose;
    use std::sync::Arc;
    let keys = Rc::new(keys());
    let roots = (*keys.peer_trust_roots().unwrap()).clone();
    let cache = CacheId(crate::test_support::security::CACHE.into());
    let availability = for_caches(keys.clone(), vec![cache.clone()]);
    let f = Fixture::assemble(Directory::new(), availability.clone());
    let memory = MemoryCache::new(f.pool.clone(), availability);
    let _ = f.open().unwrap();
    f.reactor.init().unwrap();
    let mut descriptor = f.copy(1, 3).metadata.immutable();
    descriptor.version.object.cache = cache.clone();
    let late = bundle_for(&f.admission, descriptor.clone());
    memory.publish(late.clone()).unwrap();
    let request = RequestScope::new(
        RequestId([0; 16]),
        Instant::now() + Duration::from_secs(300),
    )
    .unwrap();
    // More than the former writer limit, and twice as many keyring retirements.
    // Lookups and writer progress consult current availability without eager eviction.
    for generation in 2..=65539 {
        let lease = keys.active(&cache, KeyPurpose::Page).unwrap();
        let old = lease.reference().clone();
        let mut page = bundle_for(&f.admission, descriptor.clone());
        Arc::get_mut(&mut page.ciphertext.inner)
            .unwrap()
            .envelope
            .key_id = old.id;
        memory.publish(page.clone()).unwrap();
        f.enqueue(page.copy()).unwrap();
        let mut write = if generation == 2 {
            let mut operation = f.store.writer.progress(1, &request);
            assert!(
                operation
                    .as_mut()
                    .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
                    .is_pending()
            );
            Some(operation)
        } else {
            None
        };
        keys.install(rotation_bundle(generation, roots.clone()))
            .unwrap();
        assert!(memory.get(page.plaintext.page()).unwrap().is_none());
        assert!(
            f.store
                .writer
                .copy_only(page.plaintext.page())
                .unwrap()
                .is_none()
        );
        assert_eq!(memory.publish(page.clone()), Err(Error::MissingKey));
        assert!(matches!(f.enqueue(page.copy()), Err(Error::MissingKey)));
        assert!(keys.lease(Some(&cache), old.id, KeyPurpose::Page).is_err());
        assert_eq!(lease.id(), old.id);
        let mut sealed = [0; 19];
        lease
            .seal_page(&cache, &[1; 24], b"retained", b"abc", &mut sealed)
            .unwrap();
        let mut opened = [0; 3];
        lease
            .open_page(&cache, old.id, &[1; 24], b"retained", &sealed, &mut opened)
            .unwrap();
        assert_eq!(&opened, b"abc");
        assert_eq!(page.plaintext.bytes(), &[1; 3]);
        if let Some(operation) = write.take() {
            assert!(f.store.writer.writes_in_flight() > 0);
            assert!(f.admission.used(ResourceClass::DirtyCiphertext) > 0);
            drive(&f.reactor, operation).unwrap();
            assert!(
                f.store
                    .writer
                    .index()
                    .lookup(page.plaintext.page())
                    .unwrap()
                    .is_none()
            );
        } else {
            // Unsubmitted stale copies are discarded without issuing a write.
            assert_eq!(
                drive(&f.reactor, f.store.writer.progress(1, &request)),
                Ok(0)
            );
        }
        assert_eq!(f.store.writer.pending_count(), 0);
        assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
        drop(lease);
    }
    assert_eq!(memory.publish(late.clone()), Err(Error::MissingKey));
    assert!(matches!(f.enqueue(late.copy()), Err(Error::MissingKey)));
    // Current keys still publish after all old limits have been exceeded.
    let mut page = bundle_for(&f.admission, descriptor);
    Arc::get_mut(&mut page.ciphertext.inner)
        .unwrap()
        .envelope
        .key_id = keys.active(&cache, KeyPurpose::Page).unwrap().id();
    memory.publish(page.clone()).unwrap();
    f.enqueue(page.copy()).unwrap();
    drive(&f.reactor, f.store.writer.progress(1, &request)).unwrap();
    assert!(
        f.store
            .writer
            .index()
            .lookup(page.plaintext.page())
            .unwrap()
            .is_some()
    );
}

#[test]
fn truncated_payload_is_a_miss_and_write_failure_releases_dirty_accounting() {
    let f = Fixture::new();
    let _ = f.open().unwrap();
    f.reactor.init().unwrap();
    let request = scope();
    let page = f.copy(7, 64);
    let id = page.ciphertext.envelope().page.clone();
    f.enqueue(page).unwrap();
    drive(&f.reactor, f.store.writer.progress(1, &request)).unwrap();
    let file = std::fs::OpenOptions::new()
        .write(true)
        .open(f._directory.0.join("worker-0-slab-0.dat"))
        .unwrap();
    file.set_len(0).unwrap();
    assert!(
        drive(&f.reactor, f.store.reader.read(&id, &request))
            .unwrap()
            .is_none()
    );
    assert!(f.store.writer.index().lookup(&id).unwrap().is_none());
    let page = f.copy(8, 64);
    f.enqueue(page).unwrap();
    request.cancel().unwrap();
    assert_eq!(
        drive(&f.reactor, f.store.writer.progress(1, &request)).unwrap(),
        0
    );
    assert_eq!(f.store.writer.pending_count(), 0);
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
}

#[test]
fn stopped_admission_drains_two_accepted_copies_with_no_spare_ciphertext_quota() {
    let f = Fixture::new();
    let _ = f.open().unwrap();
    f.reactor.init().unwrap();
    let first = f.copy(1, 64);
    let first_id = first.ciphertext.envelope().page.clone();
    let second = f.copy(2, 64);
    let second_id = second.ciphertext.envelope().page.clone();
    f.enqueue(first).unwrap();
    f.enqueue(second).unwrap();
    let used = f.admission.used(ResourceClass::Ciphertext);
    assert!(
        used > 160,
        "both accepted writes must already own padded staging quota"
    );
    let pressure = f
        .admission
        .reserve(
            None,
            ResourceClass::Ciphertext,
            f.admission.limit(ResourceClass::Ciphertext) - used,
        )
        .unwrap();
    f.admission.stop();
    assert!(
        f.admission
            .reserve(None, ResourceClass::Ciphertext, 512)
            .is_err()
    );
    let request = scope();
    drive(&f.reactor, f.store.writer.drain(&request)).unwrap();
    assert!(f.store.writer.index().lookup(&first_id).unwrap().is_some());
    assert!(f.store.writer.index().lookup(&second_id).unwrap().is_some());
    assert!(f.store.writer.is_idle());
    assert_eq!(f.store.writer.writes_in_flight(), 0);
    assert_eq!(f.reactor.in_flight(), 0);
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
    drop(pressure);
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
}

#[test]
fn staging_pressure_rejects_before_queue_acceptance_and_preserves_accepted_work() {
    let f = Fixture::new();
    let _ = f.open().unwrap();
    f.reactor.init().unwrap();
    let first = f.copy(1, 64);
    let second = f.copy(2, 64);
    f.enqueue(first).unwrap();
    let pressure = f
        .admission
        .reserve(
            None,
            ResourceClass::Ciphertext,
            f.admission.limit(ResourceClass::Ciphertext)
                - f.admission.used(ResourceClass::Ciphertext),
        )
        .unwrap();
    assert!(matches!(f.enqueue(second), Err(Error::Overloaded)));
    assert_eq!(f.store.writer.pending_count(), 1);
    f.admission.stop();
    drive(&f.reactor, f.store.writer.drain(&scope())).unwrap();
    assert!(f.store.writer.is_idle());
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
    drop(pressure);
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
}

#[test]
fn shutdown_deadline_discards_second_copy_and_fences_submitted_first_copy() {
    let f = Fixture::new();
    let _ = f.open().unwrap();
    f.reactor.init().unwrap();
    f.enqueue(f.copy(1, 64)).unwrap();
    f.enqueue(f.copy(2, 64)).unwrap();
    f.admission.stop();
    let request = scope();
    let mut write = f.store.writer.progress(1, &request);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(write.as_mut().poll(&mut cx).is_pending());
    assert_eq!(f.store.writer.queued_count(), 1);
    assert_eq!(f.store.writer.writes_in_flight(), 1);
    let expired =
        RequestScope::new(RequestId([9; 16]), Instant::now() - Duration::from_secs(1)).unwrap();
    let mut drain = f.store.writer.drain(&expired);
    assert!(drain.as_mut().poll(&mut cx).is_pending());
    assert_eq!(f.store.writer.queued_count(), 0);
    assert_eq!(f.store.writer.pending_count(), 1);
    assert!(!f.store.writer.is_idle());
    assert!(f.admission.used(ResourceClass::DirtyCiphertext) > 0);
    // The submitted write keeps its slot leased even though queued work is gone.
    let volume = f.store.writer.index().volume().unwrap();
    while volume.evict_one(u64::MAX).is_some() {}
    assert_eq!(volume.usage().records, 0);
    assert!(volume.usage().free_slots < FIXTURE_SLOTS - 1);
    drive(&f.reactor, write).unwrap();
    drive(&f.reactor, drain).unwrap();
    assert!(f.store.writer.is_idle());
    assert_eq!(f.reactor.in_flight(), 0);
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
    assert!(f.store.writer.index().disk_pages().is_empty());
}

#[test]
fn incremental_ciphertext_reclamation_preserves_submitted_fence_and_remaining_queue() {
    let f = Fixture::new();
    f.store.configure(f.admission.clone(), 3, 16).unwrap();
    let _ = f.open().unwrap();
    f.reactor.init().unwrap();
    f.enqueue(f.copy(1, 64)).unwrap();
    f.enqueue(f.copy(2, 64)).unwrap();
    f.enqueue(f.copy(3, 64)).unwrap();
    let request = scope();
    let mut write = f.store.writer.progress(1, &request);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(write.as_mut().poll(&mut cx).is_pending());
    assert_eq!(f.store.writer.writes_in_flight(), 1);
    let before = f.admission.used(ResourceClass::Ciphertext);
    let released = f.store.writer.reclaim_ciphertext(
        Some(&CacheId(crate::test_support::security::CACHE.into())),
        1,
    );
    assert!(released > 0);
    assert_eq!(
        before - f.admission.used(ResourceClass::Ciphertext),
        released
    );
    assert_eq!(f.store.writer.discarded_count(), 1);
    assert_eq!(f.store.writer.queued_count(), 1);
    assert_eq!(f.store.writer.pending_count(), 2);
    assert_eq!(f.store.writer.writes_in_flight(), 1);
    assert!(!f.store.writer.is_idle());
    drive(&f.reactor, write).unwrap();
    drive(&f.reactor, f.store.writer.drain(&request)).unwrap();
    assert_eq!(f.store.writer.index().disk_pages().len(), 2);
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
}

#[test]
fn disk_read_reclaims_exact_staging_and_decode_charges_without_flushing_queue() {
    let f = Fixture::new();
    let _ = f.open().unwrap();
    f.reactor.init().unwrap();
    let copy = f.copy(1, 64);
    let id = copy.ciphertext.envelope().page.clone();
    f.enqueue(copy).unwrap();
    let request = scope();
    drive(&f.reactor, f.store.writer.progress(1, &request)).unwrap();
    let disk_bytes = f.store.writer.index().lookup(&id).unwrap().unwrap().length as usize;
    f.enqueue(f.copy(2, 64)).unwrap();
    let last = f.copy(3, 64);
    let last_id = last.ciphertext.envelope().page.clone();
    f.enqueue(last).unwrap();
    let _pressure = f
        .admission
        .reserve(
            None,
            ResourceClass::Ciphertext,
            f.admission.limit(ResourceClass::Ciphertext)
                - f.admission.used(ResourceClass::Ciphertext),
        )
        .unwrap();
    let allocations = std::cell::RefCell::new(Vec::new());
    let (read, _) = drive(
        &f.reactor,
        f.store
            .reader
            .read_with_token_reclaim(&id, &request, |amount| {
                allocations.borrow_mut().push(amount);
                match f
                    .admission
                    .reserve(
                        Some(&id.version.object.cache),
                        ResourceClass::Ciphertext,
                        amount,
                    )
                    .map_err(Error::from)
                {
                    Err(Error::Overloaded) => {
                        let (cache, bytes) = f
                            .admission
                            .reclamation(
                                &id.version.object.cache,
                                ResourceClass::Ciphertext,
                                amount,
                            )
                            .unwrap();
                        f.store.writer.reclaim_ciphertext(cache.as_ref(), bytes);
                        f.admission
                            .reserve(
                                Some(&id.version.object.cache),
                                ResourceClass::Ciphertext,
                                amount,
                            )
                            .map_err(Into::into)
                    }
                    result => result,
                }
            }),
    )
    .unwrap()
    .unwrap();
    assert_eq!(*allocations.borrow(), [disk_bytes]);
    assert_eq!(read.ciphertext.bytes(), &[1; 80]);
    assert_eq!(f.store.writer.discarded_count(), 0);
    assert_eq!(f.store.writer.queued_count(), 2);
    assert!(f.store.writer.copy_only(&last_id).unwrap().is_some());
}

#[test]
fn unreclaimable_volume_pressure_discards_copies_without_fatal_progress_error() {
    let f = Fixture::new();
    let _ = f.open().unwrap();
    f.enqueue(f.copy(1, 64)).unwrap();
    f.enqueue(f.copy(2, 64)).unwrap();
    // Lease every free slot; nothing else is evictable.
    let volume = f.store.writer.index().volume().unwrap();
    let free = volume.usage().free_slots;
    let reservations: Vec<_> = (0..free)
        .map(|_| volume.allocate(Tier::Cache, SLOT_BYTES).unwrap())
        .collect();
    f.admission.stop();
    assert_eq!(
        drive(&f.reactor, f.store.writer.progress(2, &scope())).unwrap(),
        2
    );
    assert_eq!(f.store.writer.discarded_count(), 2);
    assert!(f.store.writer.is_idle());
    assert!(f.store.writer.index().disk_pages().is_empty());
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
    f.store.writer.reclaim_idle_buffer();
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
    drop(reservations);
    assert_eq!(volume.usage().free_slots, free);
}

#[test]
fn actual_enospc_completion_discards_both_writes_and_drains() {
    let f = Fixture::new();
    let _ = f.open().unwrap();
    f.reactor.init().unwrap();
    f.enqueue(f.copy(1, 64)).unwrap();
    f.enqueue(f.copy(2, 64)).unwrap();
    // Real kernel ENOSPC from /dev/full, substituted only as the test fault target.
    // Production slab open remains exclusively O_DIRECT with no fallback.
    f.store
        .writer
        .slabs()
        .replace_file_for_test(
            std::fs::OpenOptions::new()
                .write(true)
                .open("/dev/full")
                .unwrap(),
        )
        .unwrap();
    f.admission.stop();
    drive(&f.reactor, f.store.writer.drain(&scope())).unwrap();
    assert_eq!(f.store.writer.discarded_count(), 2);
    assert!(f.store.writer.is_idle());
    assert!(f.store.writer.index().disk_pages().is_empty());
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
    assert_eq!(f.reactor.in_flight(), 0);
}

#[test]
fn fill_accepted_before_stop_can_transfer_dirty_ownership_during_drain() {
    let f = Fixture::new();
    let _ = f.open().unwrap();
    f.reactor.init().unwrap();
    let page = f.copy(1, 64);
    let id = page.ciphertext.envelope().page.clone();
    let dirty = f
        .admission
        .reserve(
            Some(&page.metadata.version.object.cache),
            ResourceClass::DirtyCiphertext,
            page.ciphertext.bytes().len(),
        )
        .unwrap();
    f.admission.stop();
    f.store.writer.enqueue(page, dirty).unwrap();
    drive(&f.reactor, f.store.writer.drain(&scope())).unwrap();
    assert!(f.store.writer.index().lookup(&id).unwrap().is_some());
    assert!(f.store.writer.is_idle());
}

mod records {
    use super::*;
    use crate::admission::ResourceClass;
    use crate::memory::CiphertextBytes;
    use crate::memory::CiphertextPage;
    use crate::model::ExpiresAt;
    use crate::model::PAGE_BYTES;
    use std::sync::Arc;
    use std::time::UNIX_EPOCH;

    fn admission() -> flow_control::Quotas<AdmissionPolicy> {
        flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        ))
    }

    fn page(
        admission: &flow_control::Quotas<AdmissionPolicy>,
        length: usize,
        number: u64,
        cache: &str,
        etag: &str,
    ) -> CiphertextCopy {
        let metadata = VersionMetadata {
            content_type: None,
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId(cache.into()),
                    key: CacheKey([3; 32]),
                },
                etag: StrongEtag::test_value(etag),
            },
            length: number * PAGE_BYTES + length as u64,
        };
        CiphertextCopy {
            ciphertext: CiphertextPage {
                provenance: None,
                inner: Arc::new(CiphertextBytes {
                    checksum: std::sync::OnceLock::new(),
                    envelope: PageEnvelope {
                        cipher: crate::model::PageCipher::Xaes256Gcm,
                        page: PageId {
                            version: metadata.version.clone(),
                            number: PageNumber(number),
                        },
                        key_id: KeyId([1; 16]),
                        nonce: Nonce([2; 24]),
                        plaintext_length: length as u32,
                        ciphertext_length: length as u32 + 16,
                    },
                    storage: flow_control::ChargedBytes {
                        bytes: vec![2; length + 16],
                        reservation: admission
                            .reserve(None, ResourceClass::Ciphertext, length + 16)
                            .unwrap(),
                    }
                    .into(),
                }),
            },
            metadata: metadata.for_pin(),
        }
    }

    fn buffer(
        admission: &flow_control::Quotas<AdmissionPolicy>,
        alignment: Alignment,
        length: usize,
    ) -> AlignedBuffer<flow_control::Charge<AdmissionPolicy>> {
        alignment
            .allocate(
                length,
                admission
                    .reserve(None, ResourceClass::Ciphertext, length)
                    .unwrap(),
            )
            .unwrap()
    }

    fn unhex(hex: &str) -> Vec<u8> {
        hex.as_bytes()
            .chunks_exact(2)
            .map(|pair| u8::from_str_radix(std::str::from_utf8(pair).unwrap(), 16).unwrap())
            .collect()
    }

    // Little-endian record header for `page(3, 0, "cache", "v1")` at
    // generation 7, including the header digest and ciphertext. Bytes past the
    // logical record are zero padding.
    const GOLDEN_LOGICAL: &str = concat!(
        "524352504147453101000000b900000007000000000000000300000000000000",
        "0000000000000000030000001300000001010101010101010101010101010101",
        "0202020202020202020202020202020202020202020202020303030303030303",
        "0303030303030303030303030303030303030303030303030500000004000000",
        "6361636865227631227853ecbd5e15ca0c0000000001000000c49c9e3d747d13",
        "105b9fa5b7cf40f7b7d36e745029bbc001738e28cfbaa6edaa02020202020202",
        "020202020202020202020202",
    );

    #[test]
    fn current_wire_bytes_and_reused_padding_across_page_and_alignment_boundaries() {
        let admission = admission();
        // Normal header is 185 bytes, plus a 16-byte tag. Straddle both units.
        for (length, number, maximum, geometry, offset) in [
            (1, 0, false, (512, 512, 512), 0),
            (310, 0, false, (512, 512, 512), 512),
            (311, 0, false, (512, 512, 512), 1024),
            (312, 0, false, (512, 512, 512), 512),
            (314, 0, false, (512, 512, 512), 512),
            (315, 0, false, (512, 512, 512), 1024),
            (316, 0, false, (512, 512, 512), 512),
            (3894, 0, false, (4096, 4096, 4096), 4096),
            (3895, 0, false, (4096, 4096, 4096), 8192),
            (3896, 0, false, (4096, 4096, 4096), 4096),
            (3898, 0, false, (4096, 4096, 4096), 4096),
            (3899, 0, false, (4096, 4096, 4096), 8192),
            (3900, 0, false, (4096, 4096, 4096), 4096),
            (PAGE_BYTES as usize, 0, false, (4096, 512, 4096), 512),
            (PAGE_BYTES as usize, 1, true, (512, 4096, 512), 4096),
            (3, 2, true, (4096, 512, 4096), 1536),
        ] {
            // UTF-8 cache IDs are bounded in bytes; strong ETags are ASCII-only.
            let cache = if maximum {
                "é".repeat(MAX_ID_BYTES / 2)
            } else {
                "cache".into()
            };
            let etag = if maximum {
                "x".repeat(MAX_ETAG_BYTES - 2)
            } else {
                "v1".into()
            };
            let mut page = page(&admission, length, number, &cache, &etag);
            let alignment = Alignment::new(geometry.0, geometry.1, geometry.2).unwrap();
            let logical = logical_length(&page).unwrap();
            assert_eq!(
                logical,
                128 + 16 + cache.len() + etag.len() + 2 + 32 + length + 16
            );
            let extent = alignment.extent(offset, logical).unwrap();
            let mut staging = buffer(&admission, alignment, extent.length());
            staging.bytes_mut().unwrap().fill(0xa5);
            let mut encoded =
                encode_at(&page, Generation(u64::MAX), alignment, offset, staging).unwrap();
            assert_eq!(
                parse(&encoded.buffer, encoded.header.extent)
                    .unwrap()
                    .header
                    .envelope,
                *page.ciphertext.envelope()
            );
            assert_eq!(encoded.header.extent, extent);
            assert_eq!(
                decode(&encoded.buffer, &encoded.header).unwrap(),
                *page.ciphertext.envelope()
            );

            // Reuse an actual record, shortening ciphertext into the old payload
            // when possible. This catches stale bytes at the new padding boundary.
            let inner = Arc::get_mut(&mut page.ciphertext.inner).unwrap();
            inner.checksum.take();
            if length > 1 {
                inner.envelope.plaintext_length -= 1;
                inner.envelope.ciphertext_length -= 1;
                inner.bytes.pop();
                page.metadata.length -= 1;
            }
            inner.bytes.fill(0x6b);
            let shortened_logical = logical - usize::from(length > 1);
            // At a rounding boundary, use a same-length replacement instead.
            if alignment.extent(offset, shortened_logical).unwrap() != extent {
                inner.bytes.push(0x6b);
                inner.envelope.plaintext_length += 1;
                inner.envelope.ciphertext_length += 1;
                page.metadata.length += 1;
            }
            encoded = encode_at(&page, Generation(9), alignment, offset, encoded.buffer).unwrap();
            let bytes = encoded.buffer.bytes().unwrap();
            let decoded = parse(&encoded.buffer, extent).unwrap();
            assert_eq!(&bytes[decoded.ciphertext.clone()], page.ciphertext.bytes());
            assert!(bytes[decoded.ciphertext.end..].iter().all(|&b| b == 0));
            assert_eq!(decoded.header.generation, Generation(9));
            assert_eq!(decoded.header.metadata, page.metadata.immutable());
        }
        admission.reclaim_buffers();
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
    }

    #[test]
    fn malformed_inputs_reject_record_errors_before_geometry_checks() {
        let admission = admission();
        let alignment = Alignment::new(512, 512, 512).unwrap();
        let mutations: &[fn(&mut CiphertextCopy)] = &[
            |p| p.metadata.length = 0,
            |p| p.metadata.length = 4,
            |p| p.metadata.length = crate::model::MAX_WIRE_INTEGER + 1,
            |p| p.metadata.version.object.key = CacheKey([4; 32]),
            |p| {
                Arc::get_mut(&mut p.ciphertext.inner)
                    .unwrap()
                    .envelope
                    .page
                    .number = PageNumber(u64::MAX)
            },
            |p| {
                Arc::get_mut(&mut p.ciphertext.inner)
                    .unwrap()
                    .envelope
                    .page
                    .number = PageNumber(1)
            },
            |p| {
                Arc::get_mut(&mut p.ciphertext.inner)
                    .unwrap()
                    .envelope
                    .plaintext_length = 0
            },
            |p| {
                Arc::get_mut(&mut p.ciphertext.inner)
                    .unwrap()
                    .envelope
                    .plaintext_length = PAGE_BYTES as u32 + 1
            },
            |p| {
                Arc::get_mut(&mut p.ciphertext.inner)
                    .unwrap()
                    .envelope
                    .plaintext_length = u32::MAX
            },
            |p| {
                Arc::get_mut(&mut p.ciphertext.inner)
                    .unwrap()
                    .envelope
                    .ciphertext_length = 18
            },
            |p| {
                Arc::get_mut(&mut p.ciphertext.inner).unwrap().bytes.pop();
            },
            |p| Arc::get_mut(&mut p.ciphertext.inner).unwrap().bytes.push(0),
            |p| {
                p.metadata.version.object.cache.0.clear();
                Arc::get_mut(&mut p.ciphertext.inner)
                    .unwrap()
                    .envelope
                    .page
                    .version = p.metadata.version.clone();
            },
            |p| {
                p.metadata.version.object.cache.0 = "é".repeat(MAX_ID_BYTES / 2 + 1);
                Arc::get_mut(&mut p.ciphertext.inner)
                    .unwrap()
                    .envelope
                    .page
                    .version = p.metadata.version.clone();
            },
        ];
        for mutate in mutations {
            let mut page = page(&admission, 3, 0, "cache", "v1");
            mutate(&mut page);
            assert_eq!(logical_length(&page), Err(Error::CorruptRecord));
            let baseline = admission.used(ResourceClass::Ciphertext);
            for generation in [Generation(0), Generation(1)] {
                // Bad offset/size must not mask the record error.
                let actual = encode_at(
                    &page,
                    generation,
                    alignment,
                    1,
                    buffer(&admission, alignment, 1024),
                )
                .err();
                assert_eq!(actual, Some(Error::CorruptRecord));
                assert_eq!(admission.used(ResourceClass::Ciphertext), baseline);
            }
        }
    }

    #[test]
    fn sizing_uses_generation_one_and_ignores_historical_freshness() {
        let admission = admission();
        let mut page = page(&admission, 3, 0, "cache", "v1");
        let alignment = Alignment::new(512, 512, 512).unwrap();
        for expiry in [
            UNIX_EPOCH,
            UNIX_EPOCH + std::time::Duration::from_secs(1),
            UNIX_EPOCH + std::time::Duration::from_millis(1),
        ] {
            page.metadata.expires_at = ExpiresAt::from_system_time(expiry).unwrap();
            assert_eq!(logical_length(&page), Ok(204));
            let encoded = encode(
                &page,
                Generation(7),
                alignment,
                buffer(&admission, alignment, 512),
            )
            .unwrap();
            assert_eq!(
                parse(&encoded.buffer, encoded.header.extent)
                    .unwrap()
                    .header
                    .metadata,
                page.metadata.immutable()
            );
            assert_eq!(
                parse(&encoded.buffer, encoded.header.extent)
                    .unwrap()
                    .checksum,
                page.ciphertext.checksum()
            );
            assert_eq!(
                encode_at(
                    &page,
                    Generation(0),
                    alignment,
                    1,
                    buffer(&admission, alignment, 512)
                )
                .err(),
                Some(Error::CorruptRecord)
            );
        }
    }

    #[test]
    fn geometry_failures_release_owned_and_retained_charges() {
        let admission = admission();
        let page = page(&admission, 3, 0, "cache", "v1");
        let baseline = admission.used(ResourceClass::Ciphertext);
        let normal = Alignment::new(512, 512, 512).unwrap();
        for (alignment, offset, length, expected) in [
            (normal, 1, 512, Error::InvalidConfiguration),
            (normal, 0, 1024, Error::InvalidConfiguration),
            (normal, u64::MAX - 511, 512, Error::CorruptRecord),
            (
                Alignment::new(512, 512, usize::MAX).unwrap(),
                0,
                512,
                Error::InvalidConfiguration,
            ),
            (
                Alignment::new(512, 1, usize::MAX).unwrap(),
                0,
                512,
                Error::InvalidConfiguration,
            ),
        ] {
            let mut staging = buffer(&admission, normal, length);
            staging.bytes_mut().unwrap().fill(0xa5);
            staging.retain(std::rc::Rc::new(
                admission
                    .reserve(None, ResourceClass::Ciphertext, 17)
                    .unwrap(),
            ));
            assert_eq!(
                encode_at(&page, Generation(1), alignment, offset, staging).err(),
                Some(expected)
            );
            assert_eq!(admission.used(ResourceClass::Ciphertext), baseline);
        }
    }

    /// CPU/memory microbenchmark only: no slabs, files, reactor, or storage I/O.
    /// Run with cargo test --release --lib memory_only_codec_benchmark -- --ignored --nocapture.
    #[test]
    #[ignore = "bounded release-only memory benchmark"]
    fn memory_only_codec_benchmark() {
        use std::hint::black_box;
        use std::time::Instant;
        // This ignored benchmark must fail at runtime, not prevent debug builds.
        #[allow(clippy::assertions_on_constants)]
        {
            assert!(!cfg!(debug_assertions), "run with --release");
        }
        let admission = admission();
        let alignment = Alignment::new(4096, 4096, 4096).unwrap();
        println!(
            "memory-only; 5 samples; median [min,max] ns/record; GiB/s uses logical record bytes (sizing does not touch payload)"
        );
        for (size, length) in [("tiny", 3), ("full", PAGE_BYTES as usize)] {
            for (ids, cache, etag) in [
                ("normal", "cache".into(), "v1".into()),
                (
                    "max",
                    "c".repeat(MAX_ID_BYTES),
                    "x".repeat(MAX_ETAG_BYTES - 2),
                ),
            ] {
                let page = page(&admission, length, 0, &cache, &etag);
                let logical = logical_length(&page).unwrap();
                let extent = alignment.extent(4096, logical).unwrap();
                let mut staging = Some(buffer(&admission, alignment, extent.length()));
                staging.as_mut().unwrap().bytes_mut().unwrap().fill(0xa5);
                for mode in ["sizing", "encode-reuse", "two-sizing+encode"] {
                    let iterations = if mode == "sizing" || size == "tiny" {
                        2000
                    } else {
                        32
                    };
                    let mut run = |count: usize| {
                        let start = Instant::now();
                        for _ in 0..count {
                            let page = black_box(&page);
                            let sizing_count = match mode {
                                "sizing" => 1,
                                "two-sizing+encode" => 2,
                                _ => 0,
                            };
                            for _ in 0..sizing_count {
                                black_box(logical_length(black_box(page)).unwrap());
                            }
                            if mode != "sizing" {
                                let buffer = black_box(staging.take().unwrap());
                                let encoded = encode_at(
                                    page,
                                    black_box(Generation(7)),
                                    black_box(alignment),
                                    black_box(4096),
                                    buffer,
                                )
                                .unwrap();
                                black_box(encoded.buffer.bytes().unwrap());
                                black_box(&encoded.header);
                                staging = Some(encoded.buffer);
                            }
                        }
                        start.elapsed().as_nanos() as f64 / count as f64
                    };
                    run(iterations / 4);
                    let mut values: Vec<_> = (0..5).map(|_| run(iterations)).collect();
                    values.sort_by(f64::total_cmp);
                    let ns = values[2];
                    let gib = logical as f64 / (1u64 << 30) as f64 / (ns / 1e9);
                    println!(
                        "{size}/{ids} {mode} n={iterations}: {ns:.1} [{:.1},{:.1}] ns/record, {gib:.3} logical GiB/s",
                        values[0], values[4]
                    );
                }
            }
        }
    }

    #[test]
    fn current_record_matches_golden_vector() {
        let admission = admission();
        let page = page(&admission, 3, 0, "cache", "v1");
        let alignment = Alignment::new(512, 512, 512).unwrap();
        let encoded = encode(
            &page,
            Generation(7),
            alignment,
            buffer(&admission, alignment, 512),
        )
        .unwrap();
        let bytes = encoded.buffer.bytes().unwrap();
        let logical = logical_length(&page).unwrap();
        assert_eq!(bytes[..logical], unhex(GOLDEN_LOGICAL));
        assert!(bytes[logical..].iter().all(|&b| b == 0));
        let parsed = parse_bytes(bytes, Extent::new(0, 512).unwrap()).unwrap();
        assert_eq!(parsed.header.format_version, FORMAT_VERSION);
        assert_eq!(parsed.checksum, page.ciphertext.checksum());
    }

    #[test]
    fn current_records_preserve_bounded_content_type_and_reject_malformed_values() {
        let admission = admission();
        let mut page = page(&admission, 3, 0, "cache", "v1");
        page.metadata.content_type = Some(crate::model::ContentType::parse(b"text/plain").unwrap());
        let alignment = Alignment::new(512, 512, 512).unwrap();
        let encoded = encode(
            &page,
            Generation(7),
            alignment,
            buffer(&admission, alignment, 512),
        )
        .unwrap();
        let parsed = parse(&encoded.buffer, encoded.header.extent).unwrap();
        assert_eq!(parsed.header.format_version, FORMAT_VERSION);
        assert_eq!(parsed.header.metadata, page.metadata.immutable());
        assert_eq!(parsed.checksum, page.ciphertext.checksum());

        let mut corrupted = encoded.buffer.bytes().unwrap().to_vec();
        let start = corrupted
            .windows(10)
            .position(|w| w == b"text/plain")
            .unwrap();
        corrupted[start] = b'\r';
        let end = u32::from_le_bytes(corrupted[12..16].try_into().unwrap()) as usize;
        let digest = Sha256::digest(&corrupted[..end - 32]);
        corrupted[end - 32..end].copy_from_slice(&digest);
        assert!(parse_bytes(&corrupted, encoded.header.extent).is_err());
    }

    #[test]
    fn only_current_version_is_accepted_even_with_valid_header_digest() {
        let admission = admission();
        let page = page(&admission, 3, 0, "cache", "v1");
        let alignment = Alignment::new(512, 512, 512).unwrap();
        let encoded = encode(
            &page,
            Generation(7),
            alignment,
            buffer(&admission, alignment, 512),
        )
        .unwrap();
        let original = encoded.buffer.bytes().unwrap();
        assert_eq!(&original[8..12], &FORMAT_VERSION.to_le_bytes());
        let header_len = u32::from_le_bytes(original[12..16].try_into().unwrap()) as usize;
        let crc_offset = 128 + "cache".len() + "\"v1\"".len();
        assert_eq!(
            &original[crc_offset..crc_offset + 8],
            &page.ciphertext.checksum().to_le_bytes()
        );
        for version in [0, FORMAT_VERSION, FORMAT_VERSION + 1, u32::MAX] {
            let mut bytes = original.to_vec();
            bytes[8..12].copy_from_slice(&version.to_le_bytes());
            let digest = Sha256::digest(&bytes[..header_len - 32]);
            bytes[header_len - 32..header_len].copy_from_slice(&digest);
            let parsed = parse_bytes(&bytes, encoded.header.extent);
            if version == FORMAT_VERSION {
                assert_eq!(parsed.unwrap().checksum, page.ciphertext.checksum());
            } else {
                assert!(
                    matches!(parsed, Err(Error::CorruptRecord)),
                    "version {version}"
                );
            }
        }

        // A current-version header cannot omit the mandatory checksum even if
        // the framing hash and lengths have been recomputed by the producer.
        let mut missing_crc = original.to_vec();
        missing_crc.drain(crc_offset..crc_offset + 8);
        missing_crc.resize(original.len(), 0);
        let shorter_header = header_len - 8;
        missing_crc[12..16].copy_from_slice(&(shorter_header as u32).to_le_bytes());
        let digest = Sha256::digest(&missing_crc[..shorter_header - 32]);
        missing_crc[shorter_header - 32..shorter_header].copy_from_slice(&digest);
        assert!(matches!(
            parse_bytes(&missing_crc, encoded.header.extent),
            Err(Error::CorruptRecord)
        ));

        let mut damaged_crc = original.to_vec();
        damaged_crc[crc_offset] ^= 1;
        assert!(matches!(
            parse_bytes(&damaged_crc, encoded.header.extent),
            Err(Error::CorruptRecord)
        ));
    }

    #[test]
    fn malformed_frames_are_rejected_without_unbounded_allocations() {
        for len in [1, 16, 512] {
            let bytes = vec![0; len];
            assert!(parse_bytes(&bytes, Extent::new(0, len).unwrap()).is_err());
        }
        let mut bytes = vec![0; 512];
        bytes[..8].copy_from_slice(MAGIC);
        bytes[8..12].copy_from_slice(&FORMAT_VERSION.to_le_bytes());
        bytes[12..16].copy_from_slice(&u32::MAX.to_le_bytes());
        assert!(parse_bytes(&bytes, Extent::new(0, 512).unwrap()).is_err());
    }
}

mod checkpoint {
    //! Metadata-only (version 2) checkpoint codec, selection, and installation.

    use crate::error::Error;
    use crate::model::CacheKey;
    use crate::model::ObjectId;
    use crate::model::ObjectVersion;
    use crate::model::StrongEtag;
    use crate::model::VersionMetadata;
    use crate::model::WorkerId;
    use crate::store::catalog::Index;
    use crate::store::catalog::IndexSnapshot;
    use crate::store::checkpoint as checkpoint_format;
    use crate::store::checkpoint::*;
    use crate::store::tests::Directory;
    use crate::store::versions::VersionEntry;
    use futures::executor::block_on;
    use racer_control_wire::CacheId;
    use racer_control_wire::KeyId;
    use sha2::Digest;
    use sha2::Sha256;
    use std::fs;
    use std::rc::Rc;

    #[test]
    fn canceled_async_publication_preserves_existing_slots_without_submission() {
        use crate::admission::AdmissionPolicy;
        use crate::runtime::Reactor;
        use crate::runtime::RequestScope;
        use std::time::Duration;
        use std::time::Instant;
        let directory = Directory::new();
        let checkpoint = Checkpointer::new(directory.0.clone(), index(8));
        checkpoint.configure_layout().unwrap();
        let reactor = Rc::new(Reactor::new(Rc::new(flow_control::Quotas::new(
            AdmissionPolicy::new(crate::test_support::cluster::config(false).limits),
        ))));
        let scope = RequestScope::new(
            crate::model::RequestId([202; 16]),
            Instant::now() + Duration::from_secs(10),
        )
        .unwrap();
        for slot in ["checkpoint.0", "checkpoint.1"] {
            fs::write(directory.0.join(slot), b"retained cut").unwrap();
        }
        scope.cancel().unwrap();
        assert_eq!(
            block_on(
                checkpoint
                    .publish_async(vec![shard()], reactor.clone(), scope, 3, 0, 1024 * 1024)
                    .unwrap()
            ),
            Err(Error::Cancelled)
        );
        assert_eq!(reactor.in_flight(), 0);
        for slot in ["checkpoint.0", "checkpoint.1"] {
            assert_eq!(fs::read(directory.0.join(slot)).unwrap(), b"retained cut");
        }
    }

    #[test]
    fn abandoned_async_publication_fences_io_and_preserves_existing_slots() {
        use crate::admission::AdmissionPolicy;
        use crate::runtime::Reactor;
        use crate::runtime::RequestScope;
        use std::task::Context;
        use std::task::Poll;
        use std::time::Duration;
        use std::time::Instant;
        let directory = Directory::new();
        let checkpoint = Checkpointer::new(directory.0.clone(), index(8));
        let reactor = Rc::new(Reactor::new(Rc::new(flow_control::Quotas::new(
            AdmissionPolicy::new(crate::test_support::cluster::config(false).limits),
        ))));
        reactor.init().unwrap();
        let scope = || {
            RequestScope::new(
                crate::model::RequestId([201; 16]),
                Instant::now() + Duration::from_secs(10),
            )
            .unwrap()
        };
        let drive = |mut operation: crate::error::Operation<'static, ()>| {
            let end = Instant::now() + Duration::from_secs(10);
            loop {
                reactor.poll_budgeted(16).unwrap();
                if let Poll::Ready(result) = operation
                    .as_mut()
                    .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
                {
                    break result;
                }
                assert!(Instant::now() < end);
                reactor.wait(Duration::from_millis(1)).unwrap();
            }
        };
        for slot in ["checkpoint.0", "checkpoint.1"] {
            fs::write(directory.0.join(slot), b"recoverable cut").unwrap();
        }
        let mut abandoned = checkpoint
            .publish_async(vec![shard()], reactor.clone(), scope(), 3, 0, 1024 * 1024)
            .unwrap();
        // Encoding yields before the first file submission. Stop at the submission
        // without reaping its completion, so abandonment exercises the actual fence.
        for _ in 0..16 {
            assert!(
                abandoned
                    .as_mut()
                    .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
                    .is_pending()
            );
            if reactor.in_flight() > 0 {
                break;
            }
        }
        assert!(reactor.in_flight() > 0);
        drop(abandoned);
        let fence_reactor = reactor.clone();
        drive(Box::pin(async move {
            fence_reactor
                .file_fence(crate::model::RequestId([201; 16]))
                .await
        }))
        .unwrap();
        assert_eq!(reactor.in_flight(), 0);
        for slot in ["checkpoint.0", "checkpoint.1"] {
            assert_eq!(
                fs::read(directory.0.join(slot)).unwrap(),
                b"recoverable cut"
            );
        }
    }
    fn layout() -> CheckpointLayout {
        CheckpointLayout {
            layout_digest: FILE_LAYOUT_DIGEST,
        }
    }

    fn descriptor(etag: &str, length: u64) -> VersionMetadata {
        VersionMetadata {
            content_type: None,
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId(crate::test_support::security::CACHE.into()),
                    key: CacheKey([7; 32]),
                },
                etag: StrongEtag::parse(format!("\"{etag}\"").as_bytes()).unwrap(),
            },
            length,
        }
    }

    fn key_id() -> KeyId {
        crate::model::key_id_from_generation(1, 1).unwrap()
    }

    fn version(etag: &str, length: u64) -> VersionEntry {
        let mut entry = VersionEntry::new(descriptor(etag, length));
        entry.add_key_id(key_id());
        entry
    }

    fn index(capacity: usize) -> Rc<Index> {
        Rc::new(Index::new(
            WorkerId(0),
            capacity,
            crate::test_support::availability(),
        ))
    }

    fn shard() -> ShardImage {
        ShardImage {
            worker: WorkerId(0),
            layout: layout(),
            index: IndexSnapshot {
                metadata: vec![descriptor("empty", 0)],
                versions: vec![version("v1", 17)],
            },
        }
    }

    fn image(sequence: u64) -> CheckpointImage {
        CheckpointImage {
            version: CHECKPOINT_VERSION,
            sequence,
            shards: vec![shard()],
        }
    }

    fn resign(bytes: &mut [u8]) {
        let end = bytes.len() - DIGEST_BYTES;
        let digest = Sha256::digest(&bytes[..end]);
        bytes[end..].copy_from_slice(&digest);
    }

    #[test]
    fn recovery_budget_accounts_for_decoded_strings_vectors_and_validation_before_decode() {
        let bytes = encode(&image(1)).unwrap();
        let required = recovery_memory(&bytes, usize::MAX).unwrap();
        assert!(required > bytes.len());
        assert!(matches!(
            decode_with_budget(&bytes, bytes.len()),
            Err(Error::Overloaded)
        ));
        assert!(matches!(
            decode_with_budget(&bytes, required - 1),
            Err(Error::Overloaded)
        ));
        assert_eq!(decode_with_budget(&bytes, required).unwrap().sequence, 1);
        // A checksum-valid count may not claim more items than the bytes hold.
        let mut malformed = bytes.clone();
        // Header 32, shard count 4, worker 2, digest 32: the version count.
        malformed[70..74].copy_from_slice(&1_000_000u32.to_le_bytes());
        resign(&mut malformed);
        assert!(recovery_memory(&malformed, usize::MAX).is_err());
        for length in 0..68 {
            assert!(decode_with_budget(&bytes[..length], 1).is_err());
        }
    }

    #[test]
    fn recovery_downsize_falls_back_or_starts_cold_without_retaining_two_images() {
        use crate::store::checkpoint::candidates;
        let directory = Directory::new();
        let older = encode(&image(1)).unwrap();
        let mut newer = image(2);
        for n in 0..20 {
            newer.shards[0]
                .index
                .metadata
                .push(descriptor(&format!("extra-{n}"), 0));
        }
        let newer = encode(&newer).unwrap();
        fs::write(directory.0.join("checkpoint.0"), &older).unwrap();
        fs::write(directory.0.join("checkpoint.1"), &newer).unwrap();
        let budget = recovery_memory(&older, usize::MAX).unwrap();
        // Both encoded files fit; only the older decoded image fits.
        assert!(newer.len() < budget);
        assert_eq!(
            candidates(&directory.0, budget)
                .unwrap()
                .next()
                .unwrap()
                .1
                .sequence,
            1
        );
        for tiny in [0, 1, older.len(), budget - 1] {
            assert!(candidates(&directory.0, tiny).unwrap().next().is_none());
        }
        assert_eq!(
            candidates(&directory.0, MAX_CHECKPOINT_BYTES)
                .unwrap()
                .next()
                .unwrap()
                .1
                .sequence,
            2
        );
    }

    #[test]
    fn unreadable_slot_is_disposable_but_storage_directory_failure_is_fatal() {
        use crate::store::checkpoint::candidates;
        use std::os::fd::AsRawFd;
        let directory = Directory::new();
        fs::write(directory.0.join("checkpoint.1"), encode(&image(1)).unwrap()).unwrap();
        // Opening a Unix socket as a file fails even when tests run as root.
        let dir = fs::File::open(&directory.0).unwrap();
        let _socket = std::os::unix::net::UnixListener::bind(format!(
            "/proc/self/fd/{}/checkpoint.0",
            dir.as_raw_fd()
        ))
        .unwrap();
        assert_eq!(
            candidates(&directory.0, MAX_CHECKPOINT_BYTES)
                .unwrap()
                .next()
                .unwrap()
                .1
                .sequence,
            1
        );
        assert!(matches!(
            candidates(&directory.0.join("checkpoint.1"), MAX_CHECKPOINT_BYTES),
            Err(Error::Io)
        ));
    }

    #[test]
    fn binary_round_trip_retains_versions_keys_metadata_and_is_send() {
        fn assert_send<T: Send>() {}
        assert_send::<ShardImage>();
        let encoded = checkpoint_format::encode(&image(7)).unwrap();
        let decoded = checkpoint_format::decode(&encoded).unwrap();
        assert_eq!(decoded.sequence, 7);
        assert_eq!(&encoded[..8], b"RACERCP\0");
        assert_eq!(&encoded[8..12], &CHECKPOINT_VERSION.to_le_bytes());
        assert_eq!(CHECKPOINT_VERSION, 2);
        let shard = &decoded.shards[0];
        assert_eq!(shard.layout, layout());
        assert_eq!(shard.index.versions, vec![version("v1", 17)]);
        assert_eq!(shard.index.metadata, vec![descriptor("empty", 0)]);
        assert_eq!(checkpoint_format::encode(&decoded).unwrap(), encoded);
    }

    #[test]
    fn layout_digest_round_trips_and_rejects_changed_mapping_before_recovery() {
        let mut cut = image(7);
        cut.shards[0].layout.layout_digest = [17; 32];
        let encoded = checkpoint_format::encode(&cut).unwrap();
        let mut decoded = checkpoint_format::decode(&encoded).unwrap();
        assert_eq!(decoded.shards[0].layout.layout_digest, [17; 32]);
        let index = index(8);
        let recovery = Recovery::new(index.clone());
        recovery.configure_layout(layout());
        assert_eq!(
            block_on(recovery.install_shard(Some(decoded.shards.remove(0)))),
            Err(Error::CorruptRecord)
        );
        assert!(index.snapshot().unwrap().metadata.is_empty());
        recovery.configure_layout(CheckpointLayout {
            layout_digest: [17; 32],
        });
        let mut decoded = checkpoint_format::decode(&encoded).unwrap();
        block_on(recovery.install_shard(Some(decoded.shards.remove(0)))).unwrap();
        assert_eq!(index.snapshot().unwrap().metadata.len(), 1);
    }

    #[test]
    fn current_metadata_checkpoints_round_trip_without_data_loss() {
        let encoded = checkpoint_format::encode(&image(9)).unwrap();
        let mut recovered = checkpoint_format::decode(&encoded).unwrap();
        assert_eq!(recovered.version, CHECKPOINT_VERSION);
        assert!(
            recovered.shards[0]
                .index
                .metadata
                .iter()
                .all(|m| m.content_type.is_none())
        );
        assert_eq!(checkpoint_format::encode(&recovered).unwrap(), encoded);
        let value = crate::model::ContentType::parse(b"application/vnd.oci.image.manifest.v1+json")
            .unwrap();
        for m in recovered.shards[0].index.metadata.iter_mut() {
            m.content_type = Some(value.clone());
        }
        for entry in recovered.shards[0].index.versions.iter_mut() {
            entry.metadata.content_type = Some(value.clone());
        }
        let extended = checkpoint_format::encode(&recovered).unwrap();
        let decoded = checkpoint_format::decode(&extended).unwrap();
        assert_eq!(
            decoded.shards[0].index.versions[0].metadata.content_type,
            Some(value.clone())
        );
        assert!(
            decoded.shards[0]
                .index
                .metadata
                .iter()
                .all(|m| m.content_type == Some(value.clone()))
        );
        assert_eq!(checkpoint_format::encode(&decoded).unwrap(), extended);
    }

    #[test]
    fn empty_cut_has_stable_current_binary_vector() {
        let image = CheckpointImage {
            version: CHECKPOINT_VERSION,
            sequence: 1,
            shards: vec![ShardImage {
                worker: WorkerId(0),
                layout: layout(),
                index: index(8).snapshot().unwrap(),
            }],
        };
        let bytes = checkpoint_format::encode(&image).unwrap();
        // Header 32 + shard count 4 + worker 2 + layout digest 32 + two
        // counts 8 = 78 body bytes, then the digest.
        assert_eq!(bytes.len(), 110);
        let digest: String = bytes[78..]
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect();
        assert_eq!(
            digest,
            "40bd0e03eb7695248f268dd7caf668000ae6a2e904dd3d0e358d5c47b5927e13"
        );
    }

    #[test]
    fn populated_cut_has_stable_current_binary_vector() {
        let bytes = checkpoint_format::encode(&image(7)).unwrap();
        // image(7) layout: header 32 + shard count 4 + worker 2 + layout
        // digest 32 + version count 4 + version descriptor 92 (36-byte cache,
        // 32-byte key, 4-byte quoted ETag, length 8, three string lengths 12)
        // + key ID count 4 + key ID 16 + descriptor count 4 + the standalone
        // "empty" descriptor 95 = 285 body bytes, then the digest.
        assert_eq!(bytes.len(), 317);
        let digest: String = bytes[285..]
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect();
        assert_eq!(
            digest,
            "6d26608cda92cfd2e723510bec76b29a84ab293f35847175ac5cee97c26f6f18"
        );
        let decoded = checkpoint_format::decode(&bytes).unwrap();
        assert_eq!(decoded.shards[0].index.versions.len(), 1);
        assert_eq!(
            decoded.shards[0].index.versions[0].metadata,
            descriptor("v1", 17)
        );
        assert_eq!(checkpoint_format::encode(&decoded).unwrap(), bytes);
    }

    #[test]
    fn other_checkpoint_versions_are_rejected_and_recover_as_cold_cache() {
        let directory = Directory::new();
        for version in [0, 1, CHECKPOINT_VERSION + 1, u32::MAX] {
            let mut cut = image(9);
            cut.version = version;
            assert!(checkpoint_format::encode(&cut).is_err());
            let mut bytes = checkpoint_format::encode(&image(9)).unwrap();
            bytes[8..12].copy_from_slice(&version.to_le_bytes());
            resign(&mut bytes);
            assert!(checkpoint_format::decode(&bytes).is_err());
            assert!(sequence_hint(&bytes).is_err());
            fs::write(directory.0.join("checkpoint.0"), bytes).unwrap();
            let index = index(8);
            index.publish_version(descriptor("stale", 0)).unwrap();
            let recovery = Recovery::new(index.clone());
            recovery.configure_layout(layout());
            assert!(block_on(recovery.load(&directory.0)).unwrap().is_none());
            block_on(recovery.install_shard(None)).unwrap();
            assert!(index.snapshot().unwrap().metadata.is_empty());
        }
    }

    #[test]
    fn metadata_order_does_not_affect_encoding_and_cross_shard_conflicts_fail() {
        let mut image = image(1);
        image.shards[0]
            .index
            .metadata
            .push(descriptor("another", 1));
        image.shards[0].index.versions.push(version("v2", 3));
        let first = checkpoint_format::encode(&image).unwrap();
        image.shards[0].index.metadata.reverse();
        image.shards[0].index.versions.reverse();
        image.shards[0].index.versions[0].add_key_id(KeyId([3; 16]));
        image.shards[0].index.versions[0].key_ids.reverse();
        let reordered = checkpoint_format::encode(&image).unwrap();
        image.shards[0].index.versions[0].key_ids.reverse();
        assert_eq!(checkpoint_format::encode(&image).unwrap(), reordered);
        assert_ne!(reordered, first);
        let mut other = shard();
        other.worker = WorkerId(1);
        other.index.versions.clear();
        other.index.metadata = vec![descriptor("v1", 18)];
        image.shards.push(other);
        assert!(checkpoint_format::encode(&image).is_err());
    }

    #[test]
    fn newer_incompatible_layout_or_catalog_falls_back_and_keys_filter_on_load() {
        let directory = Directory::new();
        fs::write(
            directory.0.join("checkpoint.0"),
            checkpoint_format::encode(&image(1)).unwrap(),
        )
        .unwrap();
        let mut newer = image(2);
        newer.shards[0].layout.layout_digest = [9; 32];
        fs::write(
            directory.0.join("checkpoint.1"),
            checkpoint_format::encode(&newer).unwrap(),
        )
        .unwrap();
        let recovery = Recovery::new(index(1));
        recovery.configure_layout(layout());
        let recovered = block_on(recovery.load_filtered(&directory.0, |_, _| false))
            .unwrap()
            .unwrap();
        assert_eq!(recovered.sequence, 1);
        assert!(recovered.shards[0].index.versions.is_empty());
        assert_eq!(recovered.shards[0].index.metadata.len(), 1);
        newer.shards[0].layout = layout();
        newer.shards[0].index.metadata.push(descriptor("extra", 0));
        fs::write(
            directory.0.join("checkpoint.1"),
            checkpoint_format::encode(&newer).unwrap(),
        )
        .unwrap();
        assert_eq!(
            block_on(recovery.load(&directory.0))
                .unwrap()
                .unwrap()
                .sequence,
            1
        );
    }

    #[test]
    fn oversized_sparse_files_and_symlinks_are_rejected_without_payload_reads() {
        use std::os::unix::fs::symlink;
        let directory = Directory::new();
        let file = fs::File::create(directory.0.join("checkpoint.0")).unwrap();
        file.set_len(MAX_CHECKPOINT_BYTES as u64 + 1).unwrap();
        fs::write(
            directory.0.join("payload"),
            checkpoint_format::encode(&image(3)).unwrap(),
        )
        .unwrap();
        symlink(
            directory.0.join("payload"),
            directory.0.join("checkpoint.1"),
        )
        .unwrap();
        let recovery = Recovery::new(index(8));
        assert!(block_on(recovery.load(&directory.0)).unwrap().is_none());
    }

    #[test]
    fn malformed_hash_version_length_counts_and_trailing_bytes_are_rejected() {
        let encoded = checkpoint_format::encode(&image(1)).unwrap();
        for cut in [0, 7, 31, encoded.len() - 1] {
            assert!(checkpoint_format::decode(&encoded[..cut]).is_err());
        }
        let mut corrupt = encoded.clone();
        corrupt[20] ^= 1;
        assert!(checkpoint_format::decode(&corrupt).is_err());
        for (offset, value) in [(8, 5u32), (12, 1), (32, u32::MAX)] {
            let mut corrupt = encoded.clone();
            corrupt[offset..offset + 4].copy_from_slice(&value.to_le_bytes());
            resign(&mut corrupt);
            assert!(checkpoint_format::decode(&corrupt).is_err());
        }
        let mut corrupt = encoded.clone();
        corrupt[24..32].copy_from_slice(&1u64.to_le_bytes());
        resign(&mut corrupt);
        assert!(checkpoint_format::decode(&corrupt).is_err());
        let mut corrupt = encoded;
        corrupt.push(0);
        assert!(checkpoint_format::decode(&corrupt).is_err());
    }

    #[test]
    fn duplicates_key_lists_and_descriptor_conflicts_are_rejected() {
        let mut bad = image(1);
        let duplicate = bad.shards[0].index.versions[0].clone();
        bad.shards[0].index.versions.push(duplicate);
        assert!(checkpoint_format::encode(&bad).is_err());
        let mut bad = image(1);
        let duplicate = bad.shards[0].index.metadata[0].clone();
        bad.shards[0].index.metadata.push(duplicate);
        assert!(checkpoint_format::encode(&bad).is_err());
        let mut bad = image(1);
        bad.shards[0].index.metadata.push(descriptor("v1", 18));
        assert!(checkpoint_format::encode(&bad).is_err());
        let mut bad = image(1);
        let key = bad.shards[0].index.versions[0].key_ids[0];
        bad.shards[0].index.versions[0].key_ids.push(key);
        assert!(checkpoint_format::encode(&bad).is_err());
        let mut bad = image(1);
        bad.shards[0].index.versions[0].version.etag = StrongEtag::test_value("other");
        assert!(checkpoint_format::encode(&bad).is_err());
    }

    #[test]
    fn alternating_publication_falls_back_to_valid_older_cut_and_ignores_temp_and_payload() {
        let directory = Directory::new();
        let index = index(8);
        let checkpointer = Checkpointer::new(directory.0.clone(), index.clone());
        let recovery = Recovery::new(index);
        recovery.configure_layout(layout());
        assert!(block_on(recovery.load(&directory.0)).unwrap().is_none());
        block_on(checkpointer.publish(vec![shard()])).unwrap();
        block_on(checkpointer.publish(vec![shard()])).unwrap();
        let load = || block_on(recovery.load(&directory.0)).unwrap().unwrap();
        assert_eq!(load().sequence, 2);
        fs::write(directory.0.join("checkpoint.1"), b"torn").unwrap();
        fs::write(
            directory.0.join(".checkpoint.tmp"),
            checkpoint_format::encode(&image(99)).unwrap(),
        )
        .unwrap();
        fs::write(directory.0.join("slab.0"), b"payload must not be scanned").unwrap();
        assert_eq!(load().sequence, 1);
        block_on(checkpointer.publish(vec![shard()])).unwrap();
        assert_eq!(load().sequence, 2);
        assert_eq!(
            fs::read(directory.0.join("slab.0")).unwrap(),
            b"payload must not be scanned"
        );
        fs::write(directory.0.join("checkpoint.0"), b"bad").unwrap();
        fs::write(directory.0.join("checkpoint.1"), b"bad").unwrap();
        assert!(block_on(recovery.load(&directory.0)).unwrap().is_none());
    }

    #[test]
    fn recovery_validates_before_mutation_and_filters_missing_keys() {
        let index = index(8);
        let keep = descriptor("keep", 0);
        index
            .publish_current(crate::model::ObjectMetadata {
                content_type: None,
                version: keep.version.clone(),
                length: keep.length,
                expires_at: crate::model::ExpiresAt::test_time(
                    std::time::SystemTime::now() + std::time::Duration::from_secs(300),
                ),
            })
            .unwrap();
        assert!(index.current(&keep.version.object).unwrap().is_some());
        let recovery = Recovery::new(index.clone());
        recovery.configure_layout(layout());
        let mut bad = shard();
        bad.index.metadata.push(descriptor("v1", 18));
        assert!(block_on(recovery.install_shard(Some(bad))).is_err());
        assert!(index.version(&keep.version).unwrap().is_some());
        assert!(index.current(&keep.version.object).unwrap().is_some());
        let mut recovered = image(1);
        Recovery::filter_available(&mut recovered, |_| true, |_, _| false);
        assert!(recovered.shards[0].index.versions.is_empty());
        block_on(recovery.install_shard(recovered.shards.pop())).unwrap();
        assert!(
            index
                .version(&descriptor("empty", 0).version)
                .unwrap()
                .is_some()
        );
        assert!(index.version(&keep.version).unwrap().is_none());
        assert!(index.current(&keep.version.object).unwrap().is_none());
        block_on(recovery.install_shard(None)).unwrap();
        assert!(
            index
                .version(&descriptor("empty", 0).version)
                .unwrap()
                .is_none()
        );
    }

    #[test]
    fn catalog_capacity_failure_preserves_all_live_state() {
        let index = index(1);
        index.publish_version(descriptor("keep", 0)).unwrap();
        let recovery = Recovery::new(index.clone());
        recovery.configure_layout(layout());
        let mut recovered = shard();
        recovered.index.metadata = vec![descriptor("one", 0), descriptor("two", 0)];
        assert!(block_on(recovery.install_shard(Some(recovered))).is_err());
        assert!(
            index
                .version(&descriptor("keep", 0).version)
                .unwrap()
                .is_some()
        );
    }

    #[test]
    fn snapshot_requires_explicit_owner_release() {
        let directory = Directory::new();
        let checkpointer = Checkpointer::new(directory.0.join("not-created"), index(8));
        assert!(block_on(checkpointer.snapshot_shard()).is_err());
        assert!(!directory.0.join("not-created").exists());
        checkpointer.configure_layout().unwrap();
        let snapshot = block_on(checkpointer.snapshot_shard()).unwrap();
        assert_eq!(snapshot.layout, layout());
        assert!(block_on(checkpointer.snapshot_shard()).is_err());
        assert_eq!(checkpointer.configure_layout(), Err(Error::Overloaded));
        drop(snapshot);
        assert!(checkpointer.is_snapshot_taken());
        checkpointer.finish_snapshot();
        checkpointer.finish_snapshot();
        assert!(!checkpointer.is_snapshot_taken());
        assert!(block_on(checkpointer.snapshot_shard()).is_ok());
    }

    #[test]
    fn failed_publication_preserves_prior_cut() {
        let directory = Directory::new();
        let index = index(8);
        let checkpointer = Checkpointer::new(directory.0.clone(), index.clone());
        block_on(checkpointer.publish(vec![shard()])).unwrap();
        // Force rename to fail while leaving the previously published slot readable.
        fs::create_dir(directory.0.join("checkpoint.1")).unwrap();
        assert!(block_on(checkpointer.publish(vec![shard()])).is_err());
        let recovery = Recovery::new(index);
        let recovered = block_on(recovery.load(&directory.0)).unwrap().unwrap();
        assert_eq!(recovered.sequence, 1);
        assert!(fs::read_dir(&directory.0).unwrap().all(|entry| {
            !entry
                .unwrap()
                .file_name()
                .to_string_lossy()
                .ends_with(".tmp")
        }));
    }

    #[test]
    fn wrong_worker_cannot_install() {
        let index = index(8);
        index.publish_version(descriptor("keep", 0)).unwrap();
        let recovery = Recovery::new(index.clone());
        assert_eq!(
            block_on(recovery.install_shard(Some(shard()))),
            Err(Error::InvalidConfiguration)
        );
        recovery.configure_layout(layout());
        let mut wrong = shard();
        wrong.worker = WorkerId(1);
        assert!(block_on(recovery.install_shard(Some(wrong))).is_err());
        assert!(
            index
                .version(&descriptor("keep", 0).version)
                .unwrap()
                .is_some()
        );
    }
}
