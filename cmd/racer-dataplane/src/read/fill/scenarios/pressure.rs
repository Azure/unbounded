//! Reclamation scenarios share the acquisition fixture and real crypto pipeline.
use super::*;

#[test]
fn sequential_full_pages_reclaim_idle_bytes_and_preserve_busy_reader_leases() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    for (pages, ciphertext_pages, dirty_pages, hold_bundle) in
        [(8, 4, 2, false), (8, 3, 1, false), (12, 3, 2, true)]
    {
        let mut limits = crate::test_support::cluster::config(false).limits;
        limits.plaintext_bytes = std::num::NonZeroUsize::new(2 * PAGE_BYTES as usize).unwrap();
        limits.ciphertext_bytes =
            std::num::NonZeroUsize::new(ciphertext_pages * (PAGE_BYTES as usize + 16)).unwrap();
        limits.dirty_bytes =
            std::num::NonZeroUsize::new(dirty_pages * (PAGE_BYTES as usize + 16)).unwrap();
        // Byte pressure must occur before entry-count eviction in every case.
        limits.metadata_entries = std::num::NonZeroUsize::new(128).unwrap();
        let mut f = fixture_with(pages * PAGE_BYTES, Some(limits.clone()));
        let mut pinned = None;
        let mut bundle = None;
        for number in 0..pages {
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
            assert!(
                result
                    .plaintext
                    .bytes()
                    .iter()
                    .all(|byte| *byte == number as u8)
            );
            if number == 0 {
                pinned = Some(result.plaintext.clone());
                if hold_bundle {
                    bundle = Some(result.clone());
                }
            }
            assert_eq!(pinned.as_ref().unwrap().bytes()[0], 0);
            drop(result);
            f.fill.dependencies.flights.poll_budgeted(64).unwrap();
            let admission = &f.fill.dependencies.admission;
            for class in [
                ResourceClass::Plaintext,
                ResourceClass::Ciphertext,
                ResourceClass::DirtyCiphertext,
            ] {
                assert!(admission.used(class) <= admission.limit(class));
            }
        }
        assert_eq!(
            f.origin.calls.get(),
            pages as usize,
            "byte capacity must not permanently block idle-cache misses"
        );
        assert!(
            f.fill.dependencies.memory.get(&f.page).unwrap().is_some(),
            "independent reader protected its cached page"
        );
        drop((pinned, bundle));
        f.fill.dependencies.writer.discard_unsubmitted();
        f.fill.dependencies.memory.evict_idle(usize::MAX).unwrap();
        for class in [
            ResourceClass::Plaintext,
            ResourceClass::Ciphertext,
            ResourceClass::DirtyCiphertext,
        ] {
            assert_eq!(f.fill.dependencies.admission.used(class), 0);
        }
    }
}

#[test]
fn bootstrap_admission_discards_queued_copy_before_evicting_idle_bundle() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    let mut limits = crate::test_support::cluster::config(false).limits;
    limits.plaintext_bytes = std::num::NonZeroUsize::new(PAGE_BYTES as usize).unwrap();
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
                crate::model::PageEnvelope {
                    page: id,
                    key_id: crate::model::KeyId([1; 16]),
                    nonce: crate::model::Nonce([2; 24]),
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
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    let mut f = fixture();
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
    assert_eq!(result.plaintext.bytes(), b"abc");
    assert_eq!(f.origin.calls.get(), 1);
    assert!(deps.writer.copy_only(&f.page).unwrap().is_none());
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
