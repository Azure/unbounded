use super::*;
use crate::{
    error::Error,
    model::{CacheId, CacheKey, StrongEtag},
};

#[test]
fn content_type_is_immutable_including_absence() {
    let index = Index::new(WorkerId(0), 8, crate::test_support::availability());
    let legacy = descriptor("v1", 3);
    index.publish_version(legacy.clone()).unwrap();
    let mut typed = legacy.clone();
    typed.content_type = Some(crate::model::ContentType::parse(b"text/plain").unwrap());
    assert_eq!(
        index.publish_version(typed.clone()),
        Err(Error::CorruptRecord)
    );
    index.publish_version(legacy).unwrap();
    let index = Index::new(WorkerId(0), 8, crate::test_support::availability());
    index.publish_version(typed.clone()).unwrap();
    assert_eq!(
        index.publish_version(descriptor("v1", 3)),
        Err(Error::CorruptRecord)
    );
    assert_eq!(index.version(&typed.version).unwrap(), Some(typed.clone()));
    assert_eq!(typed.for_pin().content_type, typed.content_type);
    let mut conflict = typed.clone();
    conflict.content_type = Some(crate::model::ContentType::parse(b"text/html").unwrap());
    assert_eq!(index.publish_version(conflict), Err(Error::CorruptRecord));
}

fn descriptor(etag: &str, length: u64) -> VersionMetadata {
    VersionMetadata {
        content_type: None,
        version: ObjectVersion {
            object: ObjectId {
                cache: CacheId(crate::security::identity::tests::CACHE.into()),
                key: CacheKey([0; 32]),
            },
            etag: StrongEtag::test_value(etag),
        },
        length,
    }
}

#[test]
fn metadata_only_checkpoint_preserves_empty_and_old_versions_without_freshness() {
    let old = descriptor("old", 17);
    let empty = descriptor("new", 0);
    let snapshot = IndexSnapshot {
        entries: vec![],
        metadata: vec![old.clone(), empty.clone()],
    };
    assert_eq!(snapshot.validate_metadata(), Ok(()));
    assert_eq!(snapshot.metadata[0].for_pin().length, 17);
    assert_eq!(snapshot.metadata[1].for_pin().length, 0);
    assert_eq!(
        snapshot.metadata[0].for_pin().expires_at.as_system_time(),
        std::time::UNIX_EPOCH
    );
}

#[test]
fn checkpoint_rejects_two_lengths_for_one_version() {
    let first = descriptor("v1", 17);
    let conflicting = VersionMetadata {
        length: 18,
        ..first.clone()
    };
    let snapshot = IndexSnapshot {
        entries: vec![],
        metadata: vec![first, conflicting],
    };
    assert_eq!(snapshot.validate_metadata(), Err(Error::CorruptRecord));
}

fn indexed(metadata: VersionMetadata, segment: u64) -> (PageId, IndexedPage) {
    let page = PageId {
        version: metadata.version.clone(),
        number: crate::model::PageNumber(0),
    };
    (
        page,
        IndexedPage {
            metadata,
            key_id: KeyId::from_generation(1, 1).unwrap(),
            location: RecordLocation {
                segment: SegmentId(segment),
                generation: Generation(1),
                location: SlabLocation {
                    slab: SlabId(0),
                    extent: crate::store::disk::DirectExtent::checked(segment * 1024, 512).unwrap(),
                },
            },
        },
    )
}

#[test]
fn catalog_eviction_preserves_pages_and_conditional_removal_preserves_replacement() {
    let index = Index::new(WorkerId(0), 1, crate::test_support::availability());
    index.set_page_capacity(1).unwrap();
    let (page, old) = indexed(descriptor("old", 17), 0);
    index.publish_version(old.metadata.clone()).unwrap();
    index.publish(page.clone(), old.clone()).unwrap();
    index.publish_version(descriptor("new", 0)).unwrap();
    assert_eq!(index.version(&page.version).unwrap().unwrap().length, 17);
    let (_, replacement) = indexed(old.metadata.clone(), 1);
    index.publish(page.clone(), replacement.clone()).unwrap();
    assert!(index.segment_entries(SegmentId(0)).is_empty());
    index.remove_if_matches(&page, &old.location).unwrap();
    assert_eq!(
        index.lookup(&page).unwrap().unwrap().location,
        replacement.location
    );
    let (other, entry) = indexed(descriptor("other", 1), 2);
    assert_eq!(index.publish(other, entry), Err(Error::Overloaded));
    index
        .remove_if_matches(&page, &replacement.location)
        .unwrap();
    assert!(index.version(&page.version).unwrap().is_none());
}

#[test]
fn capacity_preflight_allows_replacement_and_reopens_only_after_removal() {
    let index = Index::new(WorkerId(0), 1, crate::test_support::availability());
    index.set_page_capacity(1).unwrap();
    let (page, entry) = indexed(descriptor("first", 17), 0);
    let (other, other_entry) = indexed(descriptor("other", 17), 1);
    assert_eq!(index.preflight_capacity(&page), Ok(()));
    assert_eq!(index.preflight_capacity(&other), Ok(()));
    assert!(index.snapshot().unwrap().entries.is_empty());
    index.publish(page.clone(), entry.clone()).unwrap();
    assert_eq!(index.preflight_capacity(&page), Ok(()));
    assert_eq!(index.preflight_capacity(&other), Err(Error::Overloaded));
    // Preflight did not reserve a slot or bypass final publish validation.
    assert_eq!(
        index.publish(other.clone(), other_entry.clone()),
        Err(Error::Overloaded)
    );
    let (_, replacement) = indexed(entry.metadata, 2);
    index.publish(page.clone(), replacement.clone()).unwrap();
    assert_eq!(
        index.lookup(&page).unwrap().unwrap().location,
        replacement.location
    );
    assert_eq!(index.preflight_capacity(&other), Err(Error::Overloaded));
    index
        .remove_if_matches(&page, &replacement.location)
        .unwrap();
    assert_eq!(index.preflight_capacity(&other), Ok(()));
    index.publish(other, other_entry).unwrap();
}

#[test]
fn restore_is_atomic_and_drops_freshness() {
    let index = Index::new(WorkerId(0), 2, crate::test_support::availability());
    let m = descriptor("v1", 17);
    let mut current = m.for_pin();
    current.expires_at = crate::model::ExpiresAt::from_unix_millis(
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_millis() as u64
            + 60_000,
    )
    .unwrap();
    index.publish_current(current).unwrap();
    assert!(index.current(&m.version.object).unwrap().is_some());
    let bad = IndexSnapshot {
        entries: vec![],
        metadata: vec![
            m.clone(),
            VersionMetadata {
                content_type: None,
                length: 99,
                ..m.clone()
            },
        ],
    };
    assert_eq!(index.restore(bad), Err(Error::CorruptRecord));
    assert!(index.current(&m.version.object).unwrap().is_some());
    let snapshot = index.snapshot().unwrap();
    index.restore(snapshot).unwrap();
    assert!(index.current(&m.version.object).unwrap().is_none());
    assert_eq!(index.version(&m.version).unwrap(), Some(m));
}
