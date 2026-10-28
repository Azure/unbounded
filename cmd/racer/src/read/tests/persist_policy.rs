//! Real `Fill` persistence against the core `persist` rule the simulator uses
//! (`sim/src/node/world.rs`, `publish_page`).

use super::fill::Fixture;
use super::fill::drive;
use super::fill::fixture_with;
use super::*;
use crate::origin::OriginPage;
use crate::read::flight::AcquisitionBudget;
use racer_core::read::Persist;
use racer_core::read::PersistFacts;

const CIPHER: usize = crate::model::PAGE_BYTES as usize + 16;

/// A fixture whose dirty ciphertext limit is `dirty` bytes, with `held`
/// bytes of it already reserved, and ownership fixed to `owned`.
fn persisting(owned: bool, dirty: usize) -> Fixture {
    let mut limits = crate::test_support::cluster::config(false).limits;
    limits.dirty_bytes = std::num::NonZeroUsize::new(dirty).unwrap();
    let f = fixture_with(3, Some(limits));
    f.fill
        .dependencies
        .writer
        .retention()
        .set_ownership(Rc::new(move |_| owned));
    f
}

/// Seen-before (second sight) by `observations` earlier interests, then one
/// more: production `Fill::observe_verified` against `persist` for the facts
/// it saw. Owned pages bypass optional headroom but still need a dirty
/// reservation, which `persist` leaves to the caller (the simulator models
/// a failed one as `disk_write_rejected`).
#[test]
fn fill_persistence_matches_core_policy() {
    let mut cases = std::collections::BTreeSet::new();
    for (owned, seen) in [(true, false), (false, false), (false, true)] {
        for (limit, held) in [
            (4 * CIPHER, 0),
            (4 * CIPHER, 2 * CIPHER - 19),
            (4 * CIPHER, 2 * CIPHER - 18),
            (4 * CIPHER, 3 * CIPHER),
            (CIPHER, CIPHER),
            (CIPHER, CIPHER - 8),
        ] {
            let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
            let _queue = queue.enter();
            let f = persisting(owned, limit);
            let deps = &f.fill.dependencies;
            let admission = &deps.admission;
            let page = crate::memory::tests::bundle_for(admission, f.origin.metadata.immutable());
            if seen {
                // A non-owned first sight is never persisted.
                f.fill.observe_verified(&page, &f.scope);
                assert_eq!(deps.writer.pending_count(), 0);
            }
            let cache = &f.context.object.cache;
            let reserved = (held > 0).then(|| {
                admission
                    .reserve(Some(cache), ResourceClass::DirtyCiphertext, held)
                    .unwrap()
            });
            let facts = PersistFacts {
                eligible: racer_core::read::retention_eligible(owned, true, seen),
                metadata_valid: true,
                resident: false,
                owned,
                dirty_limit: admission.limit(ResourceClass::DirtyCiphertext),
                dirty_used: admission.used(ResourceClass::DirtyCiphertext),
                amount: page.ciphertext.bytes().len(),
                one_page: CIPHER,
            };
            f.fill.observe_verified(&page, &f.scope);
            let decision = racer_core::read::persist(facts);
            let reservable = facts.dirty_used + facts.amount <= facts.dirty_limit;
            let pending = deps.writer.pending_count();
            assert_eq!(
                pending,
                usize::from(decision == Persist::Write && reservable),
                "owned={owned} seen={seen} limit={limit} held={held} {decision:?}"
            );
            assert_eq!(deps.writer.discarded_count(), 0);
            cases.insert(format!("{decision:?}/{pending}"));
            drop(reserved);
        }
    }
    // Enqueued, Write but the hard quota rejects, no optional headroom, and
    // first-sight ineligible.
    for case in ["Write/1", "Write/0", "NoHeadroom/0", "Ineligible/0"] {
        assert!(cases.contains(case), "{case}: {cases:?}");
    }
}

/// A real bootstrap read of an owned page succeeds whether or not dirty
/// ciphertext is exhausted; persistence is skipped exactly when it is.
#[test]
fn dirty_pressure_skips_persistence_while_bootstrap_read_succeeds() {
    for full in [false, true] {
        let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
        let _queue = queue.enter();
        let mut f = persisting(true, CIPHER);
        let cache = f.context.object.cache.clone();
        let admission = f.fill.dependencies.admission.clone();
        let held = full.then(|| {
            admission
                .reserve(Some(&cache), ResourceClass::DirtyCiphertext, CIPHER)
                .unwrap()
        });
        let mut plaintext = f
            .fill
            .dependencies
            .buffers
            .plaintext(f.fill.reserve_bootstrap(&cache).unwrap(), 3)
            .unwrap();
        uring_runtime::reactor::IoBuffer::bytes_mut(&mut plaintext)
            .unwrap()
            .copy_from_slice(b"abc");
        let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 4, 8);
        let result = drive(
            f.fill.publish_bootstrap_with_context(
                OriginPage {
                    metadata: f.origin.metadata.clone(),
                    plaintext,
                },
                f.membership.clone(),
                &f.context,
                &f.scope,
                &mut budget,
            ),
            &mut f.engine,
            &f.crypto,
        )
        .expect("dirty pressure must not fail the bootstrap read");
        assert_eq!(result.plaintext.bytes(), b"abc");
        let writer = &f.fill.dependencies.writer;
        assert_eq!(writer.pending_count(), usize::from(!full), "full={full}");
        assert_eq!(writer.discarded_count(), 0);
        drop(held);
    }
}

/// The optional headroom boundary of `persist`: a non-owned eligible page is
/// persisted only within the optional ceiling. Production
/// `Fill::persist_verified` (`src/read/fill.rs`) re-exports and calls the same
/// `optional_headroom`; this pins the boundary both use.
#[test]
fn non_owned_persistence_uses_optional_headroom() {
    let limit = 4 * CIPHER;
    for (used, expected) in [
        (0, Persist::Write),
        (CIPHER, Persist::Write),
        (2 * CIPHER - 19, Persist::Write),
        (2 * CIPHER - 18, Persist::NoHeadroom),
        (3 * CIPHER, Persist::NoHeadroom),
    ] {
        let facts = PersistFacts {
            eligible: true,
            metadata_valid: true,
            resident: false,
            owned: false,
            dirty_limit: limit,
            dirty_used: used,
            amount: 19,
            one_page: CIPHER,
        };
        assert_eq!(racer_core::read::persist(facts), expected, "{used}");
        assert_eq!(
            crate::read::fill::optional_headroom(limit, used, 19, CIPHER),
            expected == Persist::Write
        );
        assert_eq!(
            racer_core::read::persist(PersistFacts {
                owned: true,
                ..facts
            }),
            Persist::Write
        );
    }
}
