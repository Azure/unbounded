use http1::{Header, MessageHead, StartLine};
use racer_control_wire::CacheId;
use racer_object_wire::{
    CacheKey, ContentType, Error, ObjectId, ObjectVersion, PAGE_BYTES, PageId, PageNumber,
    StrongEtag, VersionMetadata, origin,
};

fn descriptor(length: u64, content_type: Option<&[u8]>) -> VersionMetadata {
    VersionMetadata {
        version: ObjectVersion {
            object: ObjectId {
                cache: CacheId("cache".into()),
                key: CacheKey([0; 32]),
            },
            etag: StrongEtag::parse(b"\"v1\"").unwrap(),
        },
        length,
        content_type: content_type.map(|value| ContentType::parse(value).unwrap()),
    }
}

fn response(metadata: &VersionMetadata, number: u64, expiry: u64) -> (MessageHead, PageId, usize) {
    let first = number * PAGE_BYTES;
    let length = (metadata.length - first).min(PAGE_BYTES);
    let mut headers = vec![
        Header::new("Content-Length", length.to_string()),
        Header::new("Content-Type", b"application/octet-stream"),
        Header::new("ETag", metadata.version.etag.as_bytes()),
        Header::new("Racer-Expires-At", expiry.to_string()),
        Header::new(
            "Content-Range",
            format!("bytes {first}-{}/{}", first + length - 1, metadata.length),
        ),
    ];
    if let Some(content_type) = &metadata.content_type {
        headers.push(Header::new("Racer-Content-Type", content_type.as_bytes()));
    }
    (
        MessageHead {
            start: StartLine::Response { status: 206 },
            headers,
        },
        PageId {
            version: metadata.version.clone(),
            number: PageNumber(number),
        },
        length as usize,
    )
}

#[test]
fn pages_accept_exact_descriptors_with_refreshed_expiry() {
    for (length, number) in [(1, 0), (PAGE_BYTES, 0), (PAGE_BYTES + 3, 1)] {
        for content_type in [None, Some(b"text/plain".as_slice())] {
            let expected = descriptor(length, content_type);
            for expiry in [0, 1234, i64::MAX as u64] {
                let (head, page, body_length) = response(&expected, number, expiry);
                let (metadata, received_length) =
                    origin::validate_page_head(&head, &page, &expected).unwrap();
                assert_eq!(metadata.immutable(), expected);
                assert_eq!(metadata.expires_at.to_unix_millis().unwrap(), expiry);
                assert_eq!(received_length, body_length as u64);
                assert_eq!(
                    origin::validate_page(&head, &page, &expected, body_length),
                    Ok(metadata)
                );
            }
        }
    }
}

#[test]
fn pages_reject_same_tag_length_changes_even_when_body_length_matches() {
    for (expected_length, actual_length, number) in [
        (PAGE_BYTES + 1, PAGE_BYTES + 2, 0),
        (PAGE_BYTES + 2, PAGE_BYTES + 1, 0),
        (PAGE_BYTES + 1, PAGE_BYTES + 2, 1),
        (PAGE_BYTES + 2, PAGE_BYTES + 1, 1),
        (0, 1, 0),
    ] {
        let expected = descriptor(expected_length, None);
        let actual = descriptor(actual_length, None);
        let (head, page, body_length) = response(&actual, number, 1234);
        assert_eq!(
            origin::validate_page_head(&head, &page, &expected),
            Err(Error::BadGateway)
        );
        assert_eq!(
            origin::validate_page(&head, &page, &expected, body_length),
            Err(Error::BadGateway)
        );
    }
}

#[test]
fn pages_require_exact_content_type_including_presence_and_spelling() {
    let types = [
        None,
        Some(b"text/plain".as_slice()),
        Some(b"Text/Plain".as_slice()),
        Some(b"application/octet-stream".as_slice()),
    ];
    for initial in types {
        for next in types {
            let expected = descriptor(1, initial);
            let actual = descriptor(1, next);
            let (head, page, body_length) = response(&actual, 0, 1234);
            let head_result = origin::validate_page_head(&head, &page, &expected);
            let body_result = origin::validate_page(&head, &page, &expected, body_length);
            if initial == next {
                assert_eq!(head_result.unwrap().0.immutable(), expected);
                assert_eq!(body_result.unwrap().immutable(), expected);
            } else {
                assert_eq!(head_result, Err(Error::BadGateway));
                assert_eq!(body_result, Err(Error::BadGateway));
            }
        }
    }
}

#[test]
fn pages_reject_descriptors_for_other_objects_or_versions() {
    let actual = descriptor(1, None);
    let (head, page, body_length) = response(&actual, 0, 1234);
    for change in 0..3 {
        let mut expected = actual.clone();
        match change {
            0 => expected.version.object.cache = CacheId("other".into()),
            1 => expected.version.object.key = CacheKey([1; 32]),
            _ => expected.version.etag = StrongEtag::parse(b"\"v2\"").unwrap(),
        }
        assert_eq!(
            origin::validate_page_head(&head, &page, &expected),
            Err(Error::BadGateway)
        );
        assert_eq!(
            origin::validate_page(&head, &page, &expected, body_length),
            Err(Error::BadGateway)
        );
    }
}
