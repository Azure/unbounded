//! Publication cell semantics: replay, capacity, grace retention, destructor
//! placement, and epoch-published worker copies. Ported from the former
//! `controlplane` crate tests.

use super::*;
use crate::app::rollout::Rollout;
use std::cell::Cell;
use std::sync::atomic::{AtomicBool, AtomicU64};

use std::sync::atomic::{AtomicUsize, Ordering};

/// Destructor observer proves allocation cleanup occurs after unlock.
struct Part {
    owner: Weak<PublicationCell<Value>>,

    drops: Arc<AtomicUsize>,
}

impl Drop for Part {
    fn drop(&mut self) {
        if let Some(owner) = self.owner.upgrade() {
            assert!(
                owner.state.try_lock().is_ok(),
                "part destroyed under publication lock"
            );
        }
        self.drops.fetch_add(1, Ordering::SeqCst);
    }
}

/// Publication with separately observable snapshot and part destructors.
struct Value {
    version: u64,

    part: Arc<Part>,

    rescue: Option<Arc<Mutex<Option<Arc<Part>>>>>,
}

impl Drop for Value {
    fn drop(&mut self) {
        if let Some(rescue) = &self.rescue {
            *rescue.lock().unwrap() = Arc::downgrade(&self.part).upgrade();
        }
        if let Some(owner) = self.part.owner.upgrade() {
            assert!(
                owner.state.try_lock().is_ok(),
                "snapshot destroyed under publication lock"
            );
        }
    }
}

impl Generation for Value {
    type Version = u64;

    type PartVersion = u64;

    type Part = Part;

    type Digest = u64;

    fn version(&self) -> u64 {
        self.version
    }

    fn part_version(&self) -> u64 {
        self.version
    }

    fn part(&self) -> &Arc<Part> {
        &self.part
    }

    fn digest(&self) -> &u64 {
        &self.version
    }

    fn part_digest(&self) -> &u64 {
        &self.version
    }

    fn part_bytes(_: &Part) -> usize {
        1
    }
}

/// Release final snapshot and part owners only after the publication lock.
#[test]
fn final_allocations_drop_outside_lock_on_success_error_and_expiration() {
    let cell = Arc::new(PublicationCell::new(Retention {
        old_parts: 1,
        grace_parts: 1,
        grace_bytes: 1,
        grace: Duration::from_secs(1),
    }));
    let drops = Arc::new(AtomicUsize::new(0));
    let next = |version| {
        Arc::new(Value {
            version,
            part: Arc::new(Part {
                owner: Arc::downgrade(&cell),
                drops: drops.clone(),
            }),
            rescue: None,
        })
    };
    let now = Instant::now();
    cell.publish(next(1), now, |_, _| Ok::<_, Error>(()), || ())
        .unwrap();
    cell.publish(next(2), now, |_, _| Ok::<_, Error>(()), || ())
        .unwrap();
    assert_eq!(drops.load(Ordering::SeqCst), 0);
    assert!(
        cell.publish(
            next(3),
            now,
            |_, _| Err(Error::IncompatibleMembership),
            || ()
        )
        .is_err()
    );
    assert_eq!(drops.load(Ordering::SeqCst), 1);
    assert!(
        cell.resolve(1, now + Duration::from_secs(2))
            .unwrap()
            .is_none()
    );
    assert_eq!(drops.load(Ordering::SeqCst), 2);
    cell.publish(next(4), now, |_, _| Ok::<_, Error>(()), || ())
        .unwrap();
    cell.publish(next(5), now, |_, _| Ok::<_, Error>(()), || ())
        .unwrap();
    assert_eq!(drops.load(Ordering::SeqCst), 3);
}

/// Snapshot destruction can upgrade its part after replacement unlocks.
#[test]
fn snapshot_destructor_upgrade_preserves_registry_until_actual_death() {
    let cell = Arc::new(PublicationCell::new(Retention {
        old_parts: 0,
        grace_parts: 0,
        grace_bytes: 0,
        grace: Duration::ZERO,
    }));
    let rescue = Arc::new(Mutex::new(None));
    let drops = Arc::new(AtomicUsize::new(0));
    for version in 1..=2 {
        let value = Arc::new(Value {
            version,
            part: Arc::new(Part {
                owner: Arc::downgrade(&cell),
                drops: drops.clone(),
            }),
            rescue: (version == 1).then(|| rescue.clone()),
        });
        cell.publish(value, Instant::now(), |_, _| Ok::<_, Error>(()), || ())
            .unwrap();
    }
    let rescued = rescue.lock().unwrap().take().unwrap();
    assert!(Arc::ptr_eq(
        &rescued,
        &cell.resolve(1, Instant::now()).unwrap().unwrap()
    ));
    assert_eq!(drops.load(Ordering::SeqCst), 0);
    drop(rescued);
    assert!(cell.resolve(1, Instant::now()).unwrap().is_none());
    assert_eq!(drops.load(Ordering::SeqCst), 1);
}

#[test]
fn resolve_recent_returns_only_recently_superseded_grace_parts() {
    let cell = Arc::new(PublicationCell::new(Retention {
        old_parts: 4,
        grace_parts: 4,
        grace_bytes: 16,
        grace: Duration::from_secs(30),
    }));
    let start = Instant::now();
    let drops = Arc::new(AtomicUsize::new(0));
    let publish = |version, at| {
        cell.publish(
            Arc::new(Value {
                version,
                part: Arc::new(Part {
                    owner: Arc::downgrade(&cell),
                    drops: drops.clone(),
                }),
                rescue: None,
            }),
            at,
            |_, _| Ok::<_, Error>(()),
            || (),
        )
        .unwrap()
    };
    let current = publish(1, start);
    let within = Duration::from_secs(10);
    // The current part was never superseded.
    assert!(cell.resolve_recent(1, start, within).unwrap().is_none());
    drop(current);
    let superseded = start + Duration::from_secs(5);
    let _current = publish(2, superseded);
    for (offset, recent) in [(0, true), (10, true), (11, false), (31, false)] {
        let now = superseded + Duration::from_secs(offset);
        assert_eq!(
            cell.resolve_recent(1, now, within).unwrap().is_some(),
            recent,
            "{offset} s after"
        );
    }
    // A window longer than grace is never trusted.
    let mut cell = PublicationCell::<Value>::new(Retention {
        old_parts: 4,
        grace_parts: 4,
        grace_bytes: 16,
        grace: Duration::from_secs(5),
    });
    assert!(cell.resolve_recent(1, start, within).unwrap().is_none());
    // Without grace retention nothing is recent, even when a lease keeps
    // the old part resolvable.
    cell = PublicationCell::new(Retention {
        old_parts: 1,
        grace_parts: 0,
        grace_bytes: 0,
        grace: Duration::ZERO,
    });
    let cell = Arc::new(cell);
    let lease = |version| {
        Arc::new(Value {
            version,
            part: Arc::new(Part {
                owner: Arc::downgrade(&cell),
                drops: drops.clone(),
            }),
            rescue: None,
        })
    };
    let old = lease(1);
    let pinned = old.part.clone();
    cell.publish(old, start, |_, _| Ok::<_, Error>(()), || ())
        .unwrap();
    cell.publish(lease(2), start, |_, _| Ok::<_, Error>(()), || ())
        .unwrap();
    assert!(cell.resolve(1, start).unwrap().is_some());
    assert!(
        cell.resolve_recent(1, start, Duration::ZERO)
            .unwrap()
            .is_none()
    );
    drop(pinned);
}

#[test]
fn resolve_expired_weak_entry_is_pruned_on_next_pass() {
    let cell = Arc::new(PublicationCell::new(Retention {
        old_parts: 1,
        grace_parts: 1,
        grace_bytes: 1,
        grace: Duration::from_secs(1),
    }));
    let now = Instant::now();
    let drops = Arc::new(AtomicUsize::new(0));
    for version in 1..=2 {
        cell.publish(
            Arc::new(Value {
                version,
                part: Arc::new(Part {
                    owner: Arc::downgrade(&cell),
                    drops: drops.clone(),
                }),
                rescue: None,
            }),
            now,
            |_, _| Ok::<_, Error>(()),
            || (),
        )
        .unwrap();
    }
    assert!(cell.resolve(1, now).unwrap().is_some());
    assert!(cell.resolve(99, now).unwrap().is_none());
    assert!(
        cell.resolve(1, now + Duration::from_secs(1))
            .unwrap()
            .is_none()
    );
    assert_eq!(drops.load(Ordering::SeqCst), 1);
    assert!(
        cell.resolve(2, now + Duration::from_secs(1))
            .unwrap()
            .is_some()
    );
    assert!(!cell.state.lock().unwrap().parts.contains_key(&1));
}

/// A tiny immutable snapshot with an independently versioned retained graph.
#[derive(Debug)]
struct Gen {
    sequence: u64,

    version: u64,

    graph: Arc<Vec<u8>>,

    digest: u64,

    graph_digest: u64,
}

impl Generation for Gen {
    type Version = u64;

    type PartVersion = u64;

    type Part = Vec<u8>;

    type Digest = u64;

    // The wire cursor and retained-part version intentionally differ.
    #[allow(clippy::misnamed_getters)]
    fn version(&self) -> u64 {
        self.sequence
    }

    fn part_version(&self) -> u64 {
        self.version
    }

    fn part(&self) -> &Arc<Vec<u8>> {
        &self.graph
    }

    fn digest(&self) -> &u64 {
        &self.digest
    }

    fn part_digest(&self) -> &u64 {
        &self.graph_digest
    }

    fn part_bytes(part: &Vec<u8>) -> usize {
        part.len()
    }
}

/// Build a publication with independently selected cursor and graph version.
fn snapshot(sequence: u64, version: u64) -> Arc<Gen> {
    Arc::new(Gen {
        sequence,
        version,
        graph: Arc::new(vec![version as u8; 32]),
        digest: sequence,
        graph_digest: version,
    })
}

/// Configure an old-part budget without grace retention.
fn retention(old_parts: usize) -> Retention {
    Retention {
        old_parts,
        grace_parts: 0,
        grace_bytes: 0,
        grace: Duration::ZERO,
    }
}

/// Publish fixture state without domain validation or resource side effects.
fn publish(cell: &PublicationCell<Gen>, value: Arc<Gen>) -> Result<Arc<Gen>> {
    cell.publish(value, Instant::now(), |_, _| Ok(()), || ())
}

/// Both snapshot and direct part leases retain capacity without advancing acceptance.
#[test]
fn old_snapshot_and_part_leases_block_without_advancing_cursor() {
    let cell = PublicationCell::new(retention(1));
    let old = publish(&cell, snapshot(1, 1)).unwrap();
    let current = publish(&cell, snapshot(2, 2)).unwrap();
    assert_eq!(
        publish(&cell, snapshot(3, 3)).unwrap_err(),
        Error::Overloaded
    );
    assert_eq!(cell.current().unwrap().unwrap().sequence, 2);
    let part = cell.resolve(1, Instant::now()).unwrap().unwrap();
    drop(old);
    assert_eq!(
        publish(&cell, snapshot(3, 3)).unwrap_err(),
        Error::Overloaded
    );
    drop(part);
    publish(&cell, snapshot(3, 3)).unwrap();
    assert!(cell.resolve(1, Instant::now()).unwrap().is_none());
    assert_eq!(current.version, 2);
}

/// Cache-only updates share graph capacity and reject conflicting replay atomically.
#[test]
fn cache_only_generations_share_one_slot_and_conflicts_are_atomic() {
    let cell = PublicationCell::new(retention(0));
    let first = publish(&cell, snapshot(1, 1)).unwrap();
    let mut history = vec![first.clone()];
    for sequence in 2..100 {
        let next = Arc::new(Gen {
            sequence,
            version: 1,
            graph: first.graph.clone(),
            digest: sequence,
            graph_digest: 1,
        });
        history.push(publish(&cell, next).unwrap());
    }
    assert_eq!(
        publish(&cell, snapshot(100, 2)).unwrap_err(),
        Error::Overloaded
    );
    assert_eq!(
        publish(&cell, snapshot(100, 1)).unwrap_err(),
        Error::IncompatibleMembership
    );
    assert_eq!(publish(&cell, snapshot(98, 1)).unwrap_err(), Error::Replay);
    let latest = cell.current().unwrap().unwrap();
    let replay = Arc::new(Gen {
        sequence: 99,
        version: 1,
        graph: first.graph.clone(),
        digest: 99,
        graph_digest: 1,
    });
    let committed = Cell::new(false);
    let repeated = cell
        .publish(
            replay,
            Instant::now(),
            |_, _| Ok::<_, Error>(()),
            || committed.set(true),
        )
        .unwrap();
    assert!(Arc::ptr_eq(&latest, &repeated));
    assert!(!committed.get());
    drop(repeated);
    drop(latest);
    drop(history);
    drop(first);
    for version in 2..100 {
        publish(&cell, snapshot(version + 100, version)).unwrap();
        assert!(cell.resolve(version - 1, Instant::now()).unwrap().is_none());
    }
}

/// Grace bounds expire structural owners without revoking external leases.
#[test]
fn grace_count_time_and_byte_budgets_preserve_external_leases() {
    let now = Instant::now();
    let cell = PublicationCell::new(Retention {
        old_parts: 2,
        grace_parts: 2,
        grace_bytes: 64,
        grace: Duration::from_secs(30),
    });
    for version in 1..10 {
        cell.publish(
            snapshot(version, version),
            now,
            |_, _| Ok::<_, Error>(()),
            || (),
        )
        .unwrap();
        for retained in version.saturating_sub(2).max(1)..=version {
            assert!(cell.resolve(retained, now).unwrap().is_some());
        }
        if version > 3 {
            assert!(cell.resolve(version - 3, now).unwrap().is_none());
        }
    }
    let leased = cell.resolve(8, now).unwrap().unwrap();
    assert!(
        cell.resolve(7, now + Duration::from_secs(31))
            .unwrap()
            .is_none()
    );
    assert!(
        cell.resolve(8, now + Duration::from_secs(31))
            .unwrap()
            .is_some()
    );
    drop(leased);
    assert!(
        cell.resolve(8, now + Duration::from_secs(31))
            .unwrap()
            .is_none()
    );
    let cell = PublicationCell::new(Retention {
        old_parts: 10,
        grace_parts: 10,
        grace_bytes: 32,
        grace: Duration::from_secs(30),
    });
    for version in 1..5 {
        publish(&cell, snapshot(version, version)).unwrap();
    }
    assert!(cell.resolve(2, now).unwrap().is_none());
    assert!(cell.resolve(3, now).unwrap().is_some());
}

/// Pinned parts consume capacity before optional grace owners are retained.
#[test]
fn grace_cannot_overcommit_slots_already_consumed_by_external_pins() {
    let cell = PublicationCell::new(Retention {
        old_parts: 1,
        grace_parts: 1,
        grace_bytes: 32,
        grace: Duration::from_secs(30),
    });
    let first = publish(&cell, snapshot(1, 1)).unwrap();
    publish(&cell, snapshot(2, 2)).unwrap();
    publish(&cell, snapshot(3, 3)).unwrap();
    assert!(cell.resolve(1, Instant::now()).unwrap().is_some());
    assert!(cell.resolve(2, Instant::now()).unwrap().is_none());
    let current = cell.current().unwrap().unwrap();
    assert_eq!(
        publish(&cell, snapshot(4, 4)).unwrap_err(),
        Error::Overloaded
    );
    drop(first);
    publish(&cell, snapshot(4, 4)).unwrap();
    assert_eq!(current.version, 3);
}

/// Failed validation rolls back a cut; successful commits expose coherent resources.
#[test]
fn validation_failure_rolls_back_barrier_and_commit_visibility_is_coherent() {
    let cell = Arc::new(PublicationCell::new(retention(0)));
    let rollout = Rollout::new([0]);
    let generation = rollout.propose(Arc::new(1)).unwrap();
    rollout.ack(0, generation).unwrap();
    let staged = rollout.stage(generation).unwrap();
    let failed = cell.publish(
        snapshot(1, 1),
        Instant::now(),
        |_, _| Err(Error::Unauthorized),
        || staged.commit(),
    );
    assert_eq!(failed.unwrap_err(), Error::Unauthorized);
    assert!(cell.current().unwrap().is_none());
    assert!(rollout.pending(&0).unwrap().is_some());
    let resources = Arc::new(AtomicU64::new(0));
    let done = Arc::new(AtomicBool::new(false));
    let reader = {
        let cell = cell.clone();
        let resources = resources.clone();
        let done = done.clone();
        std::thread::spawn(move || {
            while !done.load(Ordering::Acquire) {
                if let Some(snapshot) = cell.current().unwrap() {
                    assert!(resources.load(Ordering::Acquire) >= snapshot.sequence);
                    assert_eq!(snapshot.graph[0], 1);
                }
            }
        })
    };
    let graph = Arc::new(vec![1; 32]);
    for sequence in 1..1000 {
        cell.publish(
            Arc::new(Gen {
                sequence,
                version: 1,
                graph: graph.clone(),
                digest: sequence,
                graph_digest: 1,
            }),
            Instant::now(),
            |_, _| Ok::<_, Error>(()),
            || resources.store(sequence, Ordering::Release),
        )
        .unwrap();
    }
    done.store(true, Ordering::Release);
    reader.join().unwrap();
}

/// Concurrent snapshot release never hides a part still held by a strong lease.
#[test]
fn pinned_release_during_admission_never_loses_a_live_strong_lease() {
    let cell = Arc::new(PublicationCell::new(retention(1)));
    for version in 1..100 {
        let old = publish(&cell, snapshot(version, version)).unwrap();
        let part = old.graph.clone();
        let reader = {
            let cell = cell.clone();
            std::thread::spawn(move || {
                drop(old);
                assert!(Arc::ptr_eq(
                    &cell.resolve(version, Instant::now()).unwrap().unwrap(),
                    &part
                ));
                part
            })
        };
        publish(&cell, snapshot(version + 1, version + 1)).unwrap();
        let part = reader.join().unwrap();
        assert!(Arc::ptr_eq(
            &cell.resolve(version, Instant::now()).unwrap().unwrap(),
            &part
        ));
        drop(part);
    }
}

/// Capacity rejection releases a reserved cut so workers can acknowledge it again.
#[test]
fn capacity_failure_rolls_back_reserved_barrier_before_restaging() {
    let cell = PublicationCell::new(retention(0));
    let pin = publish(&cell, snapshot(1, 1)).unwrap();
    let rollout = Rollout::new([0]);
    let generation = rollout.propose(Arc::new(2)).unwrap();
    rollout.ack(0, generation).unwrap();
    let guard = rollout.stage(generation).unwrap();
    assert_eq!(
        cell.publish(
            snapshot(2, 2),
            Instant::now(),
            |_, _| Ok::<_, Error>(()),
            || guard.commit()
        )
        .unwrap_err(),
        Error::Overloaded
    );
    assert!(!rollout.committed(generation).unwrap());
    assert!(rollout.pending(&0).unwrap().is_some());
    assert_eq!(cell.current().unwrap().unwrap().sequence, 1);
    drop(pin);
    rollout.ack(0, generation).unwrap();
    let guard = rollout.stage(generation).unwrap();
    cell.publish(
        snapshot(2, 2),
        Instant::now(),
        |_, _| Ok::<_, Error>(()),
        || guard.commit(),
    )
    .unwrap();
    assert!(rollout.committed(generation).unwrap());
}

/// A grace eviction must not erase lookup while its deferred owner is upgradeable.
#[test]
fn weak_upgrade_during_grace_retirement_preserves_live_resolution() {
    let cell = PublicationCell::new(Retention {
        old_parts: 1,
        grace_parts: 1,
        grace_bytes: 32,
        grace: Duration::from_secs(30),
    });
    let first = publish(&cell, snapshot(1, 1)).unwrap();
    let weak = Arc::downgrade(&first.graph);
    drop(first);
    publish(&cell, snapshot(2, 2)).unwrap();
    let mut upgraded = None;
    cell.publish(
        snapshot(3, 3),
        Instant::now(),
        |_, _| Ok::<_, Error>(()),
        || {
            upgraded = weak.upgrade();
        },
    )
    .unwrap();
    let upgraded = upgraded.expect("retired structural owner is still alive during commit");
    assert!(Arc::ptr_eq(
        &upgraded,
        &cell.resolve(1, Instant::now()).unwrap().unwrap()
    ));
    drop(upgraded);
    assert!(cell.resolve(1, Instant::now()).unwrap().is_none());
    assert!(weak.upgrade().is_none());
}

/// One replay check classifies every relation between accepted and proposed.
#[test]
fn accept_classifies_replay_conflict_and_progress() {
    let one = snapshot(1, 1);
    assert_eq!(accept(None, &*one), Ok(Accepted::NewPart));
    assert_eq!(accept(Some(&*one), &*one), Ok(Accepted::Exact));
    let same_part = Gen {
        sequence: 2,
        version: 1,
        graph: one.graph.clone(),
        digest: 2,
        graph_digest: 1,
    };
    assert_eq!(accept(Some(&*one), &same_part), Ok(Accepted::SamePart));
    // Same part version, equal digest, but a different allocation.
    assert_eq!(
        accept(Some(&*one), &*snapshot(2, 1)),
        Err(Error::IncompatibleMembership)
    );
    let mut changed = same_part;
    changed.graph_digest = 9;
    assert_eq!(
        accept(Some(&*one), &changed),
        Err(Error::IncompatibleMembership)
    );
    assert_eq!(accept(Some(&*one), &*snapshot(2, 2)), Ok(Accepted::NewPart));
    let two = snapshot(2, 2);
    assert_eq!(accept(Some(&*two), &*snapshot(1, 3)), Err(Error::Replay));
    assert_eq!(accept(Some(&*two), &*snapshot(3, 1)), Err(Error::Replay));
    let mut conflicting = Gen {
        sequence: 2,
        version: 2,
        graph: two.graph.clone(),
        digest: 99,
        graph_digest: 2,
    };
    assert_eq!(accept(Some(&*two), &conflicting), Err(Error::Replay));
    conflicting.digest = 2;
    conflicting.version = 3;
    assert_eq!(accept(Some(&*two), &conflicting), Err(Error::Replay));
}

/// The epoch advances exactly on visible replacement, never on replay or failure.
#[test]
fn epoch_advances_only_on_visible_replacement() {
    let cell = PublicationCell::new(retention(1));
    assert_eq!(cell.epoch(), 0);
    let one = publish(&cell, snapshot(1, 1)).unwrap();
    assert_eq!(cell.epoch(), 1);
    publish(&cell, one.clone()).unwrap();
    assert_eq!(cell.epoch(), 1, "exact replay is not a new generation");
    assert!(publish(&cell, snapshot(0, 0)).is_err());
    assert!(
        cell.publish(
            snapshot(2, 2),
            Instant::now(),
            |_, _| Err(Error::Unauthorized),
            || ()
        )
        .is_err()
    );
    assert_eq!(cell.epoch(), 1);
    drop(one);
    publish(&cell, snapshot(2, 2)).unwrap();
    let (epoch, current) = cell.current_with_epoch().unwrap();
    assert_eq!((epoch, current.unwrap().sequence), (2, 2));
}

/// Worker copies refresh only when the epoch moves and never pin the part.
#[test]
fn epoch_copy_follows_publications_without_pinning_parts() {
    let cell = PublicationCell::new(retention(0));
    let copy = EpochCopy::<u64>::default();
    let derived = Cell::new(0);
    let read = |copy: &EpochCopy<u64>| {
        copy.with(
            &cell,
            |g: &Gen| {
                derived.set(derived.get() + 1);
                g.sequence
            },
            |v| v.copied(),
        )
    };
    assert_eq!(read(&copy), None);
    publish(&cell, snapshot(1, 1)).unwrap();
    assert_eq!(read(&copy), Some(1));
    assert_eq!(read(&copy), Some(1));
    assert_eq!(derived.get(), 1, "steady state reuses the worker copy");
    // With zero old parts, a pinned part would block this replacement.
    publish(&cell, snapshot(2, 2)).unwrap();
    assert_eq!(read(&copy), Some(2));
    assert_eq!(derived.get(), 2);
}

/// Readers that observe a new epoch also observe what commit installed.
#[test]
fn epoch_copy_observes_committed_resources() {
    let cell = Arc::new(PublicationCell::new(retention(0)));
    let resources = Arc::new(AtomicU64::new(0));
    let done = Arc::new(AtomicBool::new(false));
    let reader = {
        let cell = cell.clone();
        let resources = resources.clone();
        let done = done.clone();
        std::thread::spawn(move || {
            let copy = EpochCopy::<u64>::default();
            while !done.load(Ordering::Acquire) {
                if let Some(sequence) = copy.with(&cell, |g: &Gen| g.sequence, |v| v.copied()) {
                    assert!(resources.load(Ordering::Acquire) >= sequence);
                }
            }
        })
    };
    let graph = Arc::new(vec![1; 32]);
    for sequence in 1..1000 {
        cell.publish(
            Arc::new(Gen {
                sequence,
                version: 1,
                graph: graph.clone(),
                digest: sequence,
                graph_digest: 1,
            }),
            Instant::now(),
            |_, _| Ok::<_, Error>(()),
            || resources.store(sequence, Ordering::Release),
        )
        .unwrap();
    }
    done.store(true, Ordering::Release);
    reader.join().unwrap();
}
