use http1::{Header, MessageHead, StartLine};
use racer_control_wire::CacheId;
use racer_object_wire::{
    CacheKey, Error, ObjectId, ObjectVersion, PAGE_BYTES, PageId, PageNumber, StrongEtag,
    VersionMetadata, origin,
};

fn object() -> ObjectId {
    ObjectId {
        cache: CacheId("cache".into()),
        key: CacheKey([0; 32]),
    }
}

fn response(length: u64, tag: &[u8]) -> MessageHead {
    MessageHead {
        start: StartLine::Response { status: 200 },
        headers: vec![
            Header::new("Content-Length", length.to_string()),
            Header::new("ETag", tag),
            Header::new("Racer-Expires-At", b"1234"),
        ],
    }
}

#[test]
fn head_accepts_exact_pins_and_unpinned_versions() {
    let object = object();
    for tag in [b"\"v1\"".as_slice(), b"\"\"", b"\"a,b\\c\""] {
        let pin = StrongEtag::parse(tag).unwrap();
        for length in [0, 1, PAGE_BYTES + 1, i64::MAX as u64] {
            for expires_at in ["0", "1234", "9223372036854775807"] {
                let mut head = response(length, tag);
                head.headers[2].value = expires_at.as_bytes().to_vec();
                for expected in [None, Some(&pin)] {
                    let metadata = origin::validate_metadata(&head, &object, expected).unwrap();
                    assert_eq!(metadata.version.object, object);
                    assert_eq!(metadata.version.etag, pin);
                    assert_eq!(metadata.length, length);
                    assert_eq!(
                        metadata.expires_at.to_unix_millis().unwrap().to_string(),
                        expires_at
                    );
                }
            }
        }
    }
}

#[test]
fn head_rejects_other_versions_including_empty_tags_and_objects() {
    for (expected, actual) in [
        (b"\"v1\"".as_slice(), b"\"v2\"".as_slice()),
        (b"\"v1\"", b"\"V1\""),
        (b"\"\"", b"\"v1\""),
        (b"\"v1\"", b"\"\""),
        (b"\"a,b\\c\"", b"\"a,b\\C\""),
    ] {
        let pin = StrongEtag::parse(expected).unwrap();
        for length in [0, 1] {
            let head = response(length, actual);
            assert_eq!(
                origin::validate_metadata(&head, &object(), Some(&pin)),
                Err(Error::BadGateway)
            );
            assert_eq!(
                origin::validate_metadata(&head, &object(), None)
                    .unwrap()
                    .version
                    .etag,
                StrongEtag::parse(actual).unwrap()
            );
        }
    }
}

#[test]
fn head_validates_tag_syntax_uniqueness_and_framing_before_pin_success() {
    let pin = StrongEtag::parse(b"\"v1\"").unwrap();
    for tag in [
        b"".as_slice(),
        b"W/\"v1\"",
        b"*",
        b"\"v1\", \"v2\"",
        b"\"\x80\"",
    ] {
        assert_eq!(
            origin::validate_metadata(&response(0, tag), &object(), Some(&pin)),
            Err(Error::BadGateway)
        );
    }
    let mut missing = response(0, b"\"v1\"");
    missing.headers.remove(1);
    let mut duplicate = response(0, b"\"v1\"");
    duplicate.headers.push(Header::new("etag", b"\"v1\""));
    let mut malformed = response(0, b"\"v1\"");
    malformed.headers[0].value = b"00".to_vec();
    for head in [missing, duplicate, malformed] {
        assert_eq!(
            origin::validate_metadata(&head, &object(), Some(&pin)),
            Err(Error::BadGateway)
        );
    }
    let padded = response(0, b" \t\"v1\"\t ");
    assert_eq!(
        origin::validate_metadata(&padded, &object(), Some(&pin))
            .unwrap()
            .version
            .etag,
        pin
    );
}

#[test]
fn head_pin_controls_missing_status_without_hiding_framing_errors() {
    let pin = StrongEtag::parse(b"\"\"").unwrap();
    for (status, unpinned, pinned) in [
        (404, Error::NotFound, Error::BadGateway),
        (412, Error::VersionUnavailable, Error::VersionUnavailable),
    ] {
        let mut head = MessageHead {
            start: StartLine::Response { status },
            headers: vec![Header::new("Content-Length", b"0")],
        };
        assert_eq!(
            origin::validate_metadata(&head, &object(), None),
            Err(unpinned)
        );
        assert_eq!(
            origin::validate_metadata(&head, &object(), Some(&pin)),
            Err(pinned)
        );
        head.headers[0].value = b"1".to_vec();
        for expected in [None, Some(&pin)] {
            assert_eq!(
                origin::validate_metadata(&head, &object(), expected),
                Err(Error::BadGateway)
            );
        }
    }
}

#[test]
fn page_validation_still_requires_the_requested_version() {
    let page = PageId {
        version: ObjectVersion {
            object: object(),
            etag: StrongEtag::parse(b"\"v1\"").unwrap(),
        },
        number: PageNumber(1),
    };
    let mut head = response(1, b"\"v1\"");
    head.start = StartLine::Response { status: 206 };
    head.headers.extend([
        Header::new("Content-Type", b"application/octet-stream"),
        Header::new(
            "Content-Range",
            format!("bytes {PAGE_BYTES}-{PAGE_BYTES}/{}", PAGE_BYTES + 1),
        ),
    ]);
    let expected = VersionMetadata {
        version: page.version.clone(),
        length: PAGE_BYTES + 1,
        content_type: None,
    };
    let (metadata, length) = origin::validate_page_head(&head, &page, &expected).unwrap();
    assert_eq!(metadata.version, page.version);
    assert_eq!(length, 1);
    assert_eq!(
        origin::validate_page(&head, &page, &expected, 1),
        Ok(metadata)
    );
    for received in [0, 2] {
        assert_eq!(
            origin::validate_page(&head, &page, &expected, received),
            Err(Error::BadGateway)
        );
    }
    head.headers[1].value = b"\"v2\"".to_vec();
    assert_eq!(
        origin::validate_page_head(&head, &page, &expected),
        Err(Error::BadGateway)
    );
    assert_eq!(
        origin::validate_page(&head, &page, &expected, 1),
        Err(Error::BadGateway)
    );
}
