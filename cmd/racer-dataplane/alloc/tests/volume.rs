//! Extent volume behavior: mount, publish, commit, eviction, and repair.

mod common;

use common::*;
use page_alloc::Error;
use page_alloc::volume::{Limits, SLOT_BYTES, Tier};

#[cfg(feature = "simulation")]
mod simulation {
    use super::*;
    use page_alloc::volume::{Key, RETAIN_FOREVER, SlotKind, UNIT_BYTES, WHOLE_SLOT_CLASS};
    use std::ops::Bound;
    use std::rc::Rc;
    use uring_runtime::reactor::simulation::{Fault, Simulation, disk::DiskState};

    const SLOTS: u64 = 16;

    fn setup() -> (Simulation, uring_runtime::reactor::simulation::Environment) {
        let sim = Simulation::new();
        let environment = sim.enter();
        (sim, environment)
    }

    /// Drop the volume and slab, then lose power.
    fn crash(
        sim: &Simulation,
        volume: Rc<page_alloc::volume::Volume<()>>,
        slab: Rc<page_alloc::Slab<()>>,
    ) {
        drop(volume);
        drop(slab);
        sim.disk().crash().unwrap();
    }

    #[test]
    fn formats_commits_and_remounts() {
        let (sim, _env) = setup();
        let reactor = TestReactor::new(64, ());
        let (volume, slab, report) = mount(&reactor, SLOTS, Limits::UNLIMITED).unwrap();
        assert!(report.formatted);
        assert_eq!((report.seq, report.entries), (1, 0));
        assert_eq!(volume.slot_count(), SLOTS);
        assert!(!volume.is_dirty());
        let mut expected = Vec::new();
        for n in 0..300 {
            let tier = if n % 3 == 0 {
                Tier::Cache
            } else {
                Tier::Storage
            };
            let length = 100 + u64::from(n % 5) * 5000;
            let value = put(
                &reactor,
                &volume,
                &slab,
                tier,
                key(1, n),
                length,
                RETAIN_FOREVER,
            )
            .unwrap();
            expected.push((key(1, n), value));
        }
        assert!(volume.is_dirty());
        drive(&reactor, volume.commit(&reactor, &SCOPE)).unwrap();
        assert!(!volume.is_dirty());
        assert_eq!(volume.commit_stats().succeeded, 1);
        // A commit with nothing to do writes nothing.
        drive(&reactor, volume.commit(&reactor, &SCOPE)).unwrap();
        assert_eq!(volume.commit_stats().attempts, 1);
        // Uncommitted changes are lost on crash.
        put(&reactor, &volume, &slab, Tier::Storage, key(2, 0), 10, 0).unwrap();
        volume.remove(&key(1, 1));
        let generation = volume.next_generation();
        crash(&sim, volume, slab);

        let (volume, slab, report) = mount(&reactor, SLOTS, Limits::UNLIMITED).unwrap();
        assert!(!report.formatted);
        assert_eq!(report.seq, 2);
        assert_eq!(report.superblock_unit, 1);
        assert!(report.lost.is_empty() && report.dropped.is_empty() && !report.dirty);
        assert_eq!(report.entries, 300);
        assert_eq!(volume.get(&key(2, 0)), None);
        for (key, value) in &expected {
            assert_eq!(volume.get(key), Some(*value));
            assert!(check(&reactor, &volume, &slab, key, value).unwrap());
        }
        assert!(volume.next_generation() > generation);
        // Keep working after the remount.
        put(&reactor, &volume, &slab, Tier::Storage, key(2, 1), 10, 0).unwrap();
        drive(&reactor, volume.commit(&reactor, &SCOPE)).unwrap();
        assert_eq!(volume.commit_stats().durable_unit, 0);
        crash(&sim, volume, slab);
        let (volume, _slab, report) = mount(&reactor, SLOTS, Limits::UNLIMITED).unwrap();
        assert_eq!(report.seq, 3);
        assert_eq!(report.entries, 301);
        // Nodes only the older tree references stay reserved.
        assert!(report.pending_nodes > 0);
        assert!(volume.usage().pending_units >= report.pending_nodes);
        assert!(volume.is_dirty());
        assert!(volume.get(&key(2, 1)).is_some());
    }

    #[test]
    fn mount_without_format_rejects_empty_disk() {
        let (_sim, _env) = setup();
        let reactor = TestReactor::new(64, ());
        let mut opts = options(Limits::UNLIMITED);
        opts.format = false;
        let result = drive(
            &reactor,
            page_alloc::volume::Volume::mount(&reactor, slab(SLOTS), opts, &SCOPE),
        );
        assert_eq!(result.err(), Some(TestError::Alloc(Error::Corrupt)));
        // Wrong segment size.
        let wrong = Rc::new(page_alloc::Slab::<()>::new(
            "/volume/other".into(),
            8 * 8192,
            8192,
            8192,
        ));
        let result = drive(
            &reactor,
            page_alloc::volume::Volume::mount(&reactor, wrong, options(Limits::UNLIMITED), &SCOPE),
        );
        assert_eq!(
            result.err(),
            Some(TestError::Alloc(Error::InvalidConfiguration))
        );
        // Too few slots.
        let result = drive(
            &reactor,
            page_alloc::volume::Volume::mount(
                &reactor,
                slab(3),
                options(Limits::UNLIMITED),
                &SCOPE,
            ),
        );
        assert_eq!(
            result.err(),
            Some(TestError::Alloc(Error::InvalidConfiguration))
        );
    }

    #[test]
    fn concurrent_commits_share_attempts() {
        let (sim, _env) = setup();
        let reactor = TestReactor::new(64, ());
        let (volume, slab, _) = mount(&reactor, SLOTS, Limits::UNLIMITED).unwrap();
        for n in 0..50 {
            put(&reactor, &volume, &slab, Tier::Storage, key(1, n), 4096, 0).unwrap();
        }
        let commits: Vec<Task<'_, Result<(), TestError>>> = (0..20)
            .map(|_| volume.commit(&reactor, &SCOPE) as Task<'_, _>)
            .collect();
        for result in drive_all(&reactor, commits) {
            result.unwrap();
        }
        assert_eq!(volume.commit_stats().attempts, 1);

        // Changes made while a commit runs get a follow-up attempt.
        put(&reactor, &volume, &slab, Tier::Storage, key(2, 0), 4096, 0).unwrap();
        let first = volume.commit(&reactor, &SCOPE);
        let mut first: Task<'_, _> = first;
        assert!(
            first
                .as_mut()
                .poll(&mut std::task::Context::from_waker(std::task::Waker::noop()))
                .is_pending()
        );
        put(&reactor, &volume, &slab, Tier::Storage, key(2, 1), 4096, 0).unwrap();
        let mut tasks: Vec<Task<'_, _>> = vec![first];
        for _ in 0..10 {
            tasks.push(volume.commit(&reactor, &SCOPE));
        }
        for result in drive_all(&reactor, tasks) {
            result.unwrap();
        }
        assert_eq!(volume.commit_stats().attempts, 3);
        crash(&sim, volume, slab);
        let (volume, _slab, report) = mount(&reactor, SLOTS, Limits::UNLIMITED).unwrap();
        assert_eq!(report.entries, 52);
        assert!(volume.get(&key(2, 1)).is_some());
    }

    #[test]
    fn failed_commit_reports_to_waiters_and_retries() {
        let (sim, _env) = setup();
        let reactor = TestReactor::new(64, ());
        let (volume, slab, _) = mount(&reactor, SLOTS, Limits::UNLIMITED).unwrap();
        for n in 0..100 {
            put(&reactor, &volume, &slab, Tier::Storage, key(1, n), 4096, 0).unwrap();
        }
        drive(&reactor, volume.commit(&reactor, &SCOPE)).unwrap();
        for n in 100..200 {
            put(&reactor, &volume, &slab, Tier::Storage, key(1, n), 4096, 0).unwrap();
        }
        volume.remove(&key(1, 0));
        let before = volume.usage();
        // Node write failure.
        sim.inject("write", Fault::Errno(libc::EIO)).unwrap();
        let tasks: Vec<Task<'_, _>> = (0..3).map(|_| volume.commit(&reactor, &SCOPE)).collect();
        for result in drive_all(&reactor, tasks) {
            assert!(matches!(result, Err(TestError::Runtime(_))), "{result:?}");
        }
        // Fsync failures, before and after the superblock write.
        for skip in 0..2 {
            for _ in 0..skip {
                sim.inject("fsync", Fault::Delay(0)).unwrap();
            }
            sim.inject("fsync", Fault::Errno(libc::EIO)).unwrap();
            assert!(drive(&reactor, volume.commit(&reactor, &SCOPE)).is_err());
        }
        let stats = volume.commit_stats();
        assert_eq!((stats.attempts, stats.failed), (4, 3));
        assert!(volume.is_dirty());
        // Failed attempts leak nothing beyond pending nodes.
        assert_eq!(volume.usage().records, before.records);
        drive(&reactor, volume.commit(&reactor, &SCOPE)).unwrap();
        // Freed space drains with two more commits.
        assert!(volume.is_dirty());
        for _ in 0..2 {
            drive(&reactor, volume.commit(&reactor, &SCOPE)).unwrap();
        }
        assert!(!volume.is_dirty());
        crash(&sim, volume, slab);
        let (volume, slab, report) = mount(&reactor, SLOTS, Limits::UNLIMITED).unwrap();
        assert_eq!(report.entries, 199);
        assert!(report.lost.is_empty());
        for n in 1..200 {
            let value = volume.get(&key(1, n)).unwrap();
            assert!(check(&reactor, &volume, &slab, &key(1, n), &value).unwrap());
        }
    }

    #[test]
    fn canceled_commit_fails_and_next_succeeds() {
        let (sim, _env) = setup();
        let reactor = TestReactor::new(64, ());
        let (volume, slab, _) = mount(&reactor, SLOTS, Limits::UNLIMITED).unwrap();
        for n in 0..100 {
            put(&reactor, &volume, &slab, Tier::Storage, key(1, n), 4096, 0).unwrap();
        }
        let mut first: Task<'_, _> = volume.commit(&reactor, &SCOPE);
        let mut waiter: Task<'_, _> = volume.commit(&reactor, &SCOPE);
        let cx = &mut std::task::Context::from_waker(std::task::Waker::noop());
        assert!(first.as_mut().poll(cx).is_pending());
        assert!(waiter.as_mut().poll(cx).is_pending());
        drop(first);
        assert_eq!(
            drive(&reactor, waiter),
            Err(TestError::Runtime(uring_runtime::Error::Cancelled))
        );
        drive(&reactor, volume.commit(&reactor, &SCOPE)).unwrap();
        crash(&sim, volume, slab);
        let (_volume, _slab, report) = mount(&reactor, SLOTS, Limits::UNLIMITED).unwrap();
        assert_eq!(report.entries, 100);
    }

    #[test]
    fn eviction_frees_cache_and_expired_storage() {
        let (_sim, _env) = setup();
        let reactor = TestReactor::new(64, ());
        // Meta slot plus three data slots.
        let (volume, slab, _) = mount(&reactor, 4, Limits::UNLIMITED).unwrap();
        let whole = SLOT_BYTES - 100;
        let a = put(&reactor, &volume, &slab, Tier::Cache, key(1, 0), whole, 0).unwrap();
        assert_eq!(a.class, WHOLE_SLOT_CLASS);
        put(
            &reactor,
            &volume,
            &slab,
            Tier::Storage,
            key(1, 1),
            whole,
            100,
        )
        .unwrap();
        put(&reactor, &volume, &slab, Tier::Cache, key(1, 2), whole, 0).unwrap();
        assert_eq!(volume.allocate(Tier::Cache, whole).err(), Some(Error::Busy));
        // A leased slot is skipped.
        let lease = volume.lease(a.slot).unwrap();
        let evicted = volume.evict_one(50).unwrap();
        assert_eq!(evicted.len(), 1);
        assert_eq!(evicted[0].0, key(1, 2));
        drop(lease);
        assert_eq!(volume.get(&key(1, 2)), None);
        let (reservation, evicted) = volume.allocate_evicting(Tier::Cache, whole, 50, 4);
        let reservation = reservation.unwrap();
        assert!(evicted.is_empty());
        drop(reservation);
        // Storage is protected until retain_until passes.
        put(&reactor, &volume, &slab, Tier::Cache, key(1, 3), whole, 0).unwrap();
        let (reservation, evicted) = volume.allocate_evicting(Tier::Cache, whole, 50, 8);
        let mut held = vec![reservation.unwrap()];
        assert_eq!(evicted.len(), 1);
        assert_eq!(evicted[0].1.tier, Tier::Cache);
        while let Some(evicted) = volume.evict_one(50) {
            assert!(evicted.iter().all(|(_, v)| v.tier == Tier::Cache));
        }
        assert!(volume.get(&key(1, 1)).is_some());
        while let Ok(reservation) = volume.allocate(Tier::Cache, whole) {
            held.push(reservation);
        }
        assert_eq!(held.len(), 2);
        assert_eq!(volume.evict_one(50), None);
        let evicted = volume.evict_one(101).unwrap();
        assert_eq!(
            evicted.iter().map(|(k, _)| *k).collect::<Vec<_>>(),
            [key(1, 1)]
        );
        assert_eq!(volume.get(&key(1, 1)), None);
    }

    #[test]
    fn open_cache_slots_do_not_block_a_new_class() {
        let (_sim, _env) = setup();
        let reactor = TestReactor::new(64, ());
        // Minimum volume: meta slot plus three data slots.
        let (volume, slab, _) = mount(&reactor, 4, Limits::UNLIMITED).unwrap();
        let page = UNIT_BYTES as u64;
        // Three small records in different classes keep all data slots open.
        let mut slots = Vec::new();
        for (n, length) in [page, 2 * page, 4 * page].into_iter().enumerate() {
            let value = put(
                &reactor,
                &volume,
                &slab,
                Tier::Cache,
                key(1, n as u32),
                length,
                0,
            )
            .unwrap();
            assert!(volume.slot_state(value.slot).unwrap().open);
            slots.push(value.slot);
        }
        assert_eq!(
            volume.allocate(Tier::Cache, page * 8).err(),
            Some(Error::Busy)
        );
        // A reservation in flight pins its open slot; the others are taken.
        let pinned = volume.allocate(Tier::Cache, page).unwrap();
        assert_eq!(pinned.slot(), slots[0]);
        let (reservation, evicted) = volume.allocate_evicting(Tier::Cache, page * 8, 0, 1);
        let reservation = reservation.unwrap();
        assert_eq!(evicted.len(), 1);
        assert_ne!(evicted[0].1.slot, slots[0]);
        assert!(volume.get(&key(1, 0)).is_some());
        drop(reservation);
        drop(pinned);
        // With no leases or holds, a full page fits after one eviction.
        put(
            &reactor,
            &volume,
            &slab,
            Tier::Cache,
            key(2, 0),
            page * 8,
            0,
        )
        .unwrap();
        let (reservation, evicted) = volume.allocate_evicting(Tier::Cache, SLOT_BYTES, 0, 4);
        assert!(reservation.is_ok());
        assert!(!evicted.is_empty());
    }

    #[test]
    fn storage_quota_evicts_only_expired_storage() {
        let (_sim, _env) = setup();
        let reactor = TestReactor::new(64, ());
        let limits = Limits {
            storage_slots: 1,
            small_bytes: u64::MAX,
        };
        // Meta slot plus three data slots: one storage, two cache.
        let (volume, slab, _) = mount(&reactor, 4, limits).unwrap();
        let whole = SLOT_BYTES - 100;
        put(
            &reactor,
            &volume,
            &slab,
            Tier::Storage,
            key(1, 0),
            whole,
            1000,
        )
        .unwrap();
        for n in 1..3 {
            put(&reactor, &volume, &slab, Tier::Cache, key(1, n), whole, 0).unwrap();
        }
        // Quota reached and the storage slot is retained: no cache slot is
        // evicted for a storage allocation.
        let (reservation, evicted) = volume.allocate_evicting(Tier::Storage, whole, 50, 64);
        assert_eq!(reservation.err(), Some(Error::Busy));
        assert!(evicted.is_empty());
        for n in 0..3 {
            assert!(volume.get(&key(1, n)).is_some());
        }
        // Once the storage slot expires, it is evicted and reused.
        let (reservation, evicted) = volume.allocate_evicting(Tier::Storage, whole, 2000, 64);
        assert!(reservation.is_ok());
        assert_eq!(
            evicted.iter().map(|(k, _)| *k).collect::<Vec<_>>(),
            [key(1, 0)]
        );
        for n in 1..3 {
            assert!(volume.get(&key(1, n)).is_some());
        }
    }

    #[test]
    fn quotas_return_busy() {
        let (_sim, _env) = setup();
        let reactor = TestReactor::new(64, ());
        let limits = Limits {
            storage_slots: 1,
            small_bytes: 3 * UNIT_BYTES as u64,
        };
        let (volume, slab, _) = mount(&reactor, SLOTS, limits).unwrap();
        let whole = SLOT_BYTES;
        put(&reactor, &volume, &slab, Tier::Storage, key(1, 0), whole, 0).unwrap();
        assert_eq!(
            volume.allocate(Tier::Storage, whole).err(),
            Some(Error::Busy)
        );
        let a = volume.allocate(Tier::Cache, 4096).unwrap();
        let b = volume.allocate(Tier::Cache, 2 * 4096).unwrap();
        assert_eq!(volume.allocate(Tier::Cache, 1).err(), Some(Error::Busy));
        drop(b);
        assert!(volume.allocate(Tier::Cache, 1).is_ok());
        drop(a);
        assert_eq!(
            volume.allocate(Tier::Cache, 0).err(),
            Some(Error::InvalidConfiguration)
        );
        assert_eq!(
            volume.allocate(Tier::Cache, SLOT_BYTES + 1).err(),
            Some(Error::InvalidConfiguration)
        );
        volume.set_limits(Limits::UNLIMITED);
        assert!(volume.allocate(Tier::Storage, whole).is_ok());
    }

    #[test]
    fn publish_requires_completed_write() {
        let (_sim, _env) = setup();
        let reactor = TestReactor::new(64, ());
        let (volume, slab, _) = mount(&reactor, SLOTS, Limits::UNLIMITED).unwrap();
        let reservation = volume.allocate(Tier::Storage, 100).unwrap();
        let slot = reservation.slot();
        assert_eq!(volume.slot_state(slot).unwrap().used, 1);
        assert_eq!(
            volume.publish(key(1, 0), reservation, 100, 7, 0),
            Err(Error::InvalidConfiguration)
        );
        // The dropped reservation was freed.
        assert_eq!(volume.slot_state(slot).unwrap().kind, SlotKind::Free);
        let reservation = volume.allocate(Tier::Storage, 100).unwrap();
        // Too large for the class.
        let big = slab.allocate(2 * UNIT_BYTES, ()).unwrap();
        assert_eq!(
            drive(&reactor, volume.write(&reactor, &reservation, big, &SCOPE)).err(),
            Some(TestError::Alloc(Error::InvalidConfiguration))
        );
        let generation = volume.next_generation();
        write(
            &reactor,
            &volume,
            &slab,
            &reservation,
            &key(1, 0),
            generation,
            100,
        )
        .unwrap();
        assert!(reservation.is_written());
        assert_eq!(reservation.capacity(), UNIT_BYTES as u64);
        assert_eq!(
            volume.publish(key(1, 0), reservation, 100, generation, 0),
            Ok(None)
        );
        let first = volume.get(&key(1, 0)).unwrap();
        assert_eq!(first.length, 100);
        // Replacing returns the previous value; the cache record is freed.
        let second = put(&reactor, &volume, &slab, Tier::Cache, key(1, 0), 5000, 0).unwrap();
        assert_ne!(first, second);
        assert!(volume.next_generation() > second.generation);
        assert_eq!(volume.usage().records, 1);
        // An in-flight write blocks publish.
        let reservation = volume.allocate(Tier::Cache, 100).unwrap();
        let buffer = record(&slab, &key(1, 1), 9, 100);
        let mut op = volume.write(&reactor, &reservation, buffer, &SCOPE);
        assert!(
            op.as_mut()
                .poll(&mut std::task::Context::from_waker(std::task::Waker::noop()))
                .is_pending()
        );
        drop(op);
        assert_eq!(
            volume.publish(key(1, 1), reservation, 100, 9, 0),
            Err(Error::Busy)
        );
        while reactor.in_flight() != 0 {
            reactor.poll_budgeted(64).unwrap();
        }
        assert_eq!(volume.usage().records, 1);
    }

    #[test]
    fn remove_variants_and_retain() {
        let (sim, _env) = setup();
        let reactor = TestReactor::new(64, ());
        let (volume, slab, _) = mount(&reactor, SLOTS, Limits::UNLIMITED).unwrap();
        for cache in 1..=3 {
            for n in 0..20 {
                put(
                    &reactor,
                    &volume,
                    &slab,
                    Tier::Storage,
                    key(cache, n),
                    4096,
                    10,
                )
                .unwrap();
            }
        }
        let low = Key::new([2; 8], [0; 12], 0);
        let high = Key::new([3; 8], [0; 12], 0);
        let removed = volume.remove_range(Bound::Included(low), Bound::Excluded(high));
        assert_eq!(removed.len(), 20);
        assert!(removed.iter().all(|(k, _)| k.cache() == [2; 8]));
        assert_eq!(volume.len(), 40);
        let value = volume.get(&key(1, 0)).unwrap();
        let mut other = value;
        other.generation += 1;
        assert!(!volume.remove_if(&key(1, 0), &other));
        assert!(volume.remove_if(&key(1, 0), &value));
        assert!(!volume.remove_if(&key(1, 0), &value));
        assert_eq!(volume.remove(&key(1, 0)), None);
        // All storage records of cache 1 and 3 share one slot.
        let slot = volume.get(&key(1, 1)).unwrap().slot;
        assert_eq!(volume.slot_state(slot).unwrap().retain_until, 10);
        assert!(volume.set_retain(&key(1, 1), 50));
        assert_eq!(volume.slot_state(slot).unwrap().retain_until, 50);
        assert!(volume.set_retain(&key(1, 1), 0));
        assert_eq!(volume.slot_state(slot).unwrap().retain_until, 10);
        for (key, _) in volume.range(Bound::Unbounded, Bound::Unbounded, usize::MAX) {
            assert!(volume.set_retain(&key, 0));
        }
        assert_eq!(volume.slot_state(slot).unwrap().retain_until, 0);
        assert!(!volume.set_retain(&key(9, 9), 0));
        assert_eq!(volume.next_after(None, 5).len(), 5);
        let page = volume.next_after(Some(&key(1, 19)), 100);
        assert_eq!(page.len(), 20);
        drive(&reactor, volume.commit(&reactor, &SCOPE)).unwrap();
        crash(&sim, volume, slab);
        let (volume, _slab, _) = mount(&reactor, SLOTS, Limits::UNLIMITED).unwrap();
        assert_eq!(volume.len(), 39);
        assert_eq!(volume.get(&key(1, 1)).unwrap().retain_until, 0);
        assert!(volume.get(&key(2, 1)).is_none());
    }

    /// Storage records freed by a remove are not reused until two commits
    /// succeed, so the older superblock never points at reused space.
    #[test]
    fn storage_reuse_waits_for_two_commits() {
        let (_sim, _env) = setup();
        let reactor = TestReactor::new(64, ());
        let (volume, slab, _) = mount(&reactor, SLOTS, Limits::UNLIMITED).unwrap();
        let first = put(&reactor, &volume, &slab, Tier::Storage, key(1, 0), 4096, 0).unwrap();
        drive(&reactor, volume.commit(&reactor, &SCOPE)).unwrap();
        volume.remove(&key(1, 0));
        let next = put(&reactor, &volume, &slab, Tier::Storage, key(1, 1), 4096, 0).unwrap();
        assert_ne!((next.slot, next.unit), (first.slot, first.unit));
        drive(&reactor, volume.commit(&reactor, &SCOPE)).unwrap();
        let next = put(&reactor, &volume, &slab, Tier::Storage, key(1, 2), 4096, 0).unwrap();
        assert_ne!((next.slot, next.unit), (first.slot, first.unit));
        drive(&reactor, volume.commit(&reactor, &SCOPE)).unwrap();
        let next = put(&reactor, &volume, &slab, Tier::Storage, key(1, 3), 4096, 0).unwrap();
        assert_eq!((next.slot, next.unit), (first.slot, first.unit));
    }

    /// Pending storage space at the quota is released by commits with no new
    /// changes, and the volume reports commit work until it drains.
    #[test]
    fn pending_space_drains_without_new_changes() {
        let (_sim, _env) = setup();
        let reactor = TestReactor::new(64, ());
        let limits = Limits {
            storage_slots: 1,
            small_bytes: u64::MAX,
        };
        let (volume, slab, _) = mount(&reactor, SLOTS, limits).unwrap();
        let whole = SLOT_BYTES;
        put(&reactor, &volume, &slab, Tier::Storage, key(1, 0), whole, 0).unwrap();
        drive(&reactor, volume.commit(&reactor, &SCOPE)).unwrap();
        volume.remove(&key(1, 0));
        drive(&reactor, volume.commit(&reactor, &SCOPE)).unwrap();
        // The removed record is still charged.
        assert_eq!(
            volume.allocate(Tier::Storage, whole).err(),
            Some(Error::Busy)
        );
        assert!(volume.is_dirty());
        let mut commits = 0;
        while volume.is_dirty() {
            assert!(commits < 4, "pending space never drained");
            drive(&reactor, volume.commit(&reactor, &SCOPE)).unwrap();
            commits += 1;
        }
        assert!(commits >= 1);
        assert!(volume.allocate(Tier::Storage, whole).is_ok());
        // Once drained, a commit has nothing to do.
        let attempts = volume.commit_stats().attempts;
        drive(&reactor, volume.commit(&reactor, &SCOPE)).unwrap();
        assert_eq!(volume.commit_stats().attempts, attempts);
        assert!(!volume.is_dirty());
    }

    /// A corrupt leaf loses only its range, which is salvaged from the older
    /// tree; a corrupt superblock falls back to the other one.
    #[test]
    fn corruption_is_repaired_from_the_older_tree() {
        let (sim, _env) = setup();
        let reactor = TestReactor::new(64, ());
        let (volume, slab, _) = mount(&reactor, SLOTS, Limits::UNLIMITED).unwrap();
        for n in 0..500 {
            put(&reactor, &volume, &slab, Tier::Storage, key(1, n), 4096, 0).unwrap();
        }
        drive(&reactor, volume.commit(&reactor, &SCOPE)).unwrap();
        // Touch one leaf so the newest tree differs.
        put(&reactor, &volume, &slab, Tier::Storage, key(2, 0), 4096, 0).unwrap();
        drive(&reactor, volume.commit(&reactor, &SCOPE)).unwrap();
        crash(&sim, volume, slab);

        // Find a leaf of the newest tree that the older tree shares: corrupt
        // the first node unit that decodes as a leaf with key(1, 0).
        let path = std::path::Path::new(PATH);
        let mut corrupted = 0;
        for unit in 2..4104u64 {
            let bytes = sim
                .disk()
                .read(
                    path,
                    unit * UNIT_BYTES as u64,
                    UNIT_BYTES,
                    DiskState::Durable,
                )
                .unwrap();
            // A leaf written by the last commit (seq 3); the older tree has
            // its previous version elsewhere.
            if bytes[..8] == *b"RCRVNODE" && bytes[11] == 0 && bytes[32..40] == 3u64.to_le_bytes() {
                sim.disk()
                    .corrupt(
                        path,
                        unit * UNIT_BYTES as u64 + 100,
                        &[!bytes[100]],
                        DiskState::Durable,
                    )
                    .unwrap();
                corrupted += 1;
                break;
            }
        }
        assert_eq!(corrupted, 1);
        // Durable-only corruption shows after power loss.
        sim.disk().crash().unwrap();
        let (volume, slab, report) = mount(&reactor, SLOTS, Limits::UNLIMITED).unwrap();
        assert_eq!(report.lost.len(), 1, "{report:?}");
        assert!(report.salvaged > 0);
        assert!(report.dirty);
        // Everything but the key added by the last commit is salvaged.
        assert_eq!(report.entries, 500);
        assert!(volume.get(&key(2, 0)).is_none());
        for n in 0..500 {
            let value = volume.get(&key(1, n)).unwrap();
            assert!(check(&reactor, &volume, &slab, &key(1, n), &value).unwrap());
        }
        drive(&reactor, volume.commit(&reactor, &SCOPE)).unwrap();
        crash(&sim, volume, slab);

        // Corrupt the newest superblock: the other one is mounted.
        let (_, _, report) = mount(&reactor, SLOTS, Limits::UNLIMITED).unwrap();
        let unit = report.superblock_unit;
        sim.disk().crash().unwrap();
        sim.disk()
            .corrupt(
                path,
                u64::from(unit) * UNIT_BYTES as u64 + 40,
                &[0xff],
                DiskState::Durable,
            )
            .unwrap();
        sim.disk().crash().unwrap();
        let (volume, _slab, report) = mount(&reactor, SLOTS, Limits::UNLIMITED).unwrap();
        assert_ne!(report.superblock_unit, unit);
        assert_eq!(report.invalid_superblocks, 1);
        // The older superblock still has the corrupt leaf, and there is no
        // valid tree to salvage from.
        assert_eq!(report.lost.len(), 1);
        assert_eq!(report.salvaged, 0);
        assert!(volume.len() < 500 && volume.len() >= 500 - 72);
    }
}

/// Real kernel: format, publish, commit, remount, and read back.
#[test]
fn io_uring_volume_round_trip() {
    use page_alloc::volume::Volume;
    if !kernel_available() {
        return;
    }
    let directory = Directory::new();
    let path = directory.0.join("slab");
    let slots = 4;
    let reactor = TestReactor::new(64, ());
    reactor.init().unwrap();
    let make = || {
        std::rc::Rc::new(page_alloc::Slab::<()>::new(
            path.clone(),
            slots * SLOT_BYTES,
            SLOT_BYTES,
            SLOT_BYTES as usize,
        ))
    };
    let slab = make();
    let (volume, report) = match drive(
        &reactor,
        Volume::mount(&reactor, slab.clone(), options(Limits::UNLIMITED), &SCOPE),
    ) {
        Ok(mounted) => mounted,
        Err(TestError::Alloc(Error::Unsupported)) => {
            capability_skip("direct I/O alignment does not divide 4096");
            return;
        }
        Err(error) => panic!("mount: {error:?}"),
    };
    assert!(report.formatted);
    let mut values = Vec::new();
    for n in 0..20 {
        let tier = if n % 2 == 0 {
            Tier::Storage
        } else {
            Tier::Cache
        };
        values.push(put(&reactor, &volume, &slab, tier, key(1, n), 6000, 0).unwrap());
    }
    drive(&reactor, volume.commit(&reactor, &SCOPE)).unwrap();
    drop(volume);
    drop(slab);
    let slab = make();
    let (volume, report) = drive(
        &reactor,
        Volume::mount(&reactor, slab.clone(), options(Limits::UNLIMITED), &SCOPE),
    )
    .unwrap();
    assert!(!report.formatted);
    assert_eq!(report.entries, 20);
    for (n, value) in values.iter().enumerate() {
        let key = key(1, n as u32);
        assert_eq!(volume.get(&key), Some(*value));
        assert!(check(&reactor, &volume, &slab, &key, value).unwrap());
    }
    assert_eq!(reactor.in_flight(), 0);
}

/// Owned test directory under the crate's target directory.
struct Directory(std::path::PathBuf);

impl Directory {
    /// Create a unique directory.
    fn new() -> Self {
        static NEXT: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);
        let id = NEXT.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
        let path = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("target")
            .join(format!(
                "page-alloc-volume-store-{}-{id}",
                std::process::id()
            ));
        std::fs::create_dir_all(&path).unwrap();
        Self(path)
    }
}

impl Drop for Directory {
    /// Remove the directory.
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

/// Report a capability skip, or fail when real I/O is required.
fn capability_skip(reason: &str) {
    assert_ne!(
        std::env::var("PAGE_ALLOC_REQUIRE_REAL_IO").as_deref(),
        Ok("1"),
        "PAGE_ALLOC_REQUIRE_REAL_IO=1 forbids capability skips: {reason}"
    );
    eprintln!("SKIP real volume test: {reason}");
}

/// Whether io_uring is usable on this host.
fn kernel_available() -> bool {
    use std::os::fd::FromRawFd;
    let mut params = [0u64; 15];
    // SAFETY: initialized, aligned 120-byte UAPI output; success owns a new descriptor.
    let fd = unsafe { libc::syscall(libc::SYS_io_uring_setup, 2u32, params.as_mut_ptr()) };
    if fd >= 0 {
        drop(unsafe { std::fs::File::from_raw_fd(fd as i32) });
        return true;
    }
    let error = std::io::Error::last_os_error();
    match error.raw_os_error() {
        Some(libc::ENOSYS | libc::EOPNOTSUPP | libc::EPERM | libc::EACCES) => {
            capability_skip(&format!("io_uring unavailable: {error}"));
            false
        }
        _ => panic!("unexpected io_uring setup failure: {error}"),
    }
}
