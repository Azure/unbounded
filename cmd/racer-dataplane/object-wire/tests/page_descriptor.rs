use racer_control_wire::CacheId;
use racer_object_wire::{
    CacheKey, Error, MAX_WIRE_INTEGER, ObjectId, ObjectVersion, PAGE_BYTES, PageDescriptor, PageId,
    PageNumber, Result, StrongEtag, VersionMetadata,
};

struct StoredPage {
    structure: Result<()>,
    page: PageId,
    plaintext_length: u32,
}

impl PageDescriptor for StoredPage {
    fn validate_structure(&self) -> Result<()> {
        self.structure
    }

    fn page(&self) -> &PageId {
        assert!(self.structure.is_ok());
        &self.page
    }

    fn plaintext_length(&self) -> u32 {
        assert!(self.structure.is_ok());
        self.plaintext_length
    }
}

fn metadata(length: u64) -> VersionMetadata {
    VersionMetadata {
        content_type: None,
        version: ObjectVersion {
            object: ObjectId {
                cache: CacheId("cache".into()),
                key: CacheKey([0; 32]),
            },
            etag: StrongEtag::parse(b"\"v1\"").unwrap(),
        },
        length,
    }
}

fn stored_page(metadata: &VersionMetadata, number: u64, plaintext_length: u32) -> StoredPage {
    StoredPage {
        structure: Ok(()),
        page: PageId {
            version: metadata.version.clone(),
            number: PageNumber(number),
        },
        plaintext_length,
    }
}

#[test]
fn structure_errors_are_preserved_before_reading_page_fields() {
    let metadata = metadata(1);
    for error in [Error::CorruptRecord, Error::Internal] {
        for number in [0, u64::MAX] {
            let mut page = stored_page(&metadata, number, 1);
            page.structure = Err(error);
            assert_eq!(metadata.validate_page(&page), Err(error));
        }
    }
}

#[test]
fn accepts_exact_lengths_for_full_and_partial_pages_at_numeric_boundaries() {
    for (length, number, plaintext_length) in [
        (1, 0, 1),
        (PAGE_BYTES - 1, 0, PAGE_BYTES as u32 - 1),
        (PAGE_BYTES, 0, PAGE_BYTES as u32),
        (2 * PAGE_BYTES, 1, PAGE_BYTES as u32),
        (2 * PAGE_BYTES + 1, 0, PAGE_BYTES as u32),
        (2 * PAGE_BYTES + 1, 1, PAGE_BYTES as u32),
        (2 * PAGE_BYTES + 1, 2, 1),
        (MAX_WIRE_INTEGER, 0, PAGE_BYTES as u32),
        (
            MAX_WIRE_INTEGER,
            MAX_WIRE_INTEGER / PAGE_BYTES - 1,
            PAGE_BYTES as u32,
        ),
        (
            MAX_WIRE_INTEGER,
            MAX_WIRE_INTEGER / PAGE_BYTES,
            PAGE_BYTES as u32 - 1,
        ),
    ] {
        let metadata = metadata(length);
        let page = stored_page(&metadata, number, plaintext_length);
        assert_eq!(
            metadata.page_length(&page.page),
            Ok(plaintext_length),
            "length={length}, page={number}"
        );
        assert_eq!(
            metadata.validate_page(&page),
            Ok(()),
            "length={length}, page={number}"
        );
    }
}

#[test]
fn rejects_each_mismatched_version_identity_component() {
    let metadata = metadata(1);
    for change in 0..3 {
        let mut page = stored_page(&metadata, 0, 1);
        match change {
            0 => page.page.version.object.cache = CacheId("other".into()),
            1 => page.page.version.object.key = CacheKey([1; 32]),
            _ => page.page.version.etag = StrongEtag::parse(b"\"v2\"").unwrap(),
        }
        assert_eq!(metadata.page_length(&page.page), Err(Error::CorruptRecord));
        assert_eq!(metadata.validate_page(&page), Err(Error::CorruptRecord));
    }
}

#[test]
fn rejects_absent_pages_including_empty_objects_and_overflowing_offsets() {
    for (length, number) in [
        (0, 0),
        (1, 1),
        (PAGE_BYTES, 1),
        (PAGE_BYTES + 1, 2),
        (MAX_WIRE_INTEGER, MAX_WIRE_INTEGER / PAGE_BYTES + 1),
        (MAX_WIRE_INTEGER, u64::MAX / PAGE_BYTES),
        (MAX_WIRE_INTEGER, u64::MAX / PAGE_BYTES + 1),
        (MAX_WIRE_INTEGER, u64::MAX),
    ] {
        let metadata = metadata(length);
        for plaintext_length in [0, 1, PAGE_BYTES as u32] {
            let page = stored_page(&metadata, number, plaintext_length);
            assert_eq!(
                metadata.page_length(&page.page),
                Err(Error::CorruptRecord),
                "length={length}, page={number}"
            );
            assert_eq!(
                metadata.validate_page(&page),
                Err(Error::CorruptRecord),
                "length={length}, page={number}, plaintext_length={plaintext_length}"
            );
        }
    }
}

#[test]
fn rejects_object_lengths_above_the_wire_limit() {
    for length in [MAX_WIRE_INTEGER + 1, u64::MAX] {
        let metadata = metadata(length);
        let page = stored_page(&metadata, 0, PAGE_BYTES as u32);
        assert_eq!(metadata.page_length(&page.page), Err(Error::CorruptRecord));
        assert_eq!(metadata.validate_page(&page), Err(Error::CorruptRecord));
    }
}

#[test]
fn rejects_plaintext_lengths_that_do_not_match_the_exact_page_length() {
    for (length, number, expected) in [
        (1, 0, 1),
        (2 * PAGE_BYTES, 0, PAGE_BYTES as u32),
        (2 * PAGE_BYTES, 1, PAGE_BYTES as u32),
        (PAGE_BYTES + 3, 1, 3),
    ] {
        let metadata = metadata(length);
        for plaintext_length in [0, expected - 1, expected + 1, u32::MAX] {
            let page = stored_page(&metadata, number, plaintext_length);
            assert_eq!(metadata.page_length(&page.page), Ok(expected));
            assert_eq!(
                metadata.validate_page(&page),
                Err(Error::CorruptRecord),
                "length={length}, page={number}, plaintext_length={plaintext_length}"
            );
        }
    }
}
