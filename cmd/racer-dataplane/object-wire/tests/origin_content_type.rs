use http1::{Codec, Header, MessageHead, StartLine};
use racer_control_wire::CacheId;
use racer_object_wire::{
    CacheKey, ContentType, Error, ObjectId, ObjectVersion, PageId, PageNumber, StrongEtag,
    VersionMetadata, origin,
};

fn object() -> ObjectId {
    ObjectId {
        cache: CacheId("cache".into()),
        key: CacheKey([0; 32]),
    }
}

fn response(content_type: Option<&[u8]>) -> MessageHead {
    let mut head = MessageHead {
        start: StartLine::Response { status: 200 },
        headers: vec![
            Header::new("Content-Length", b"1"),
            Header::new("Content-Type", b"application/octet-stream"),
            Header::new("ETag", b"\"v1\""),
            Header::new("Racer-Expires-At", b"1234"),
        ],
    };
    if let Some(value) = content_type {
        head.headers.push(Header::new("Racer-Content-Type", value));
    }
    head
}

fn assert_content_type(mut head: MessageHead, expected: Option<&[u8]>) {
    let object = object();
    let metadata = origin::validate_metadata(&head, &object, None).unwrap();
    assert_eq!(
        metadata.content_type.as_ref().map(|value| value.as_bytes()),
        expected
    );
    head.start = StartLine::Response { status: 206 };
    head.headers
        .push(Header::new("Content-Range", b"bytes 0-0/1"));
    assert_eq!(
        origin::validate_bootstrap(&head, &object),
        Ok((metadata.clone(), 1))
    );
    let page = PageId {
        version: metadata.version.clone(),
        number: PageNumber(0),
    };
    assert_eq!(
        origin::validate_page(&head, &page, &metadata.immutable(), 1),
        Ok(metadata)
    );
}

#[test]
fn ordinary_content_type_trims_wire_ows_and_preserves_mime_spelling() {
    let mime = "Text/Plain; Charset = \"utf-8\"; x=\" a;b \\\"c \"";
    for leading in ["", " ", "  ", "\t", " \t "] {
        for trailing in ["", " ", "\t", " \t "] {
            let wire = format!(
                "HTTP/1.1 200 OK\r\nContent-Length: 1\r\nContent-Type: application/octet-stream\r\nETag: \"v1\"\r\nRacer-Expires-At: 1234\r\nrAcEr-CoNtEnT-tYpE:{leading}{mime}{trailing}\r\n\r\n"
            );
            let (head, consumed) = Codec::<()>::new(8192)
                .decode_head(wire.as_bytes())
                .unwrap()
                .unwrap();
            assert_eq!(consumed, wire.len());
            assert_content_type(head, Some(mime.as_bytes()));
        }
    }
}

#[test]
fn absent_content_type_remains_absent() {
    assert_content_type(response(None), None);
}

#[test]
fn normalized_content_type_retains_its_size_limit() {
    let boundary = format!("a/{}", "b".repeat(254));
    let padded = format!(" \t{boundary}\t ");
    assert_content_type(response(Some(padded.as_bytes())), Some(boundary.as_bytes()));
    let oversized = format!(" \ta/{}\t ", "b".repeat(255));
    assert_eq!(
        origin::validate_metadata(&response(Some(oversized.as_bytes())), &object(), None),
        Err(Error::BadGateway)
    );
}

#[test]
fn normalization_does_not_accept_empty_malformed_or_duplicate_content_types() {
    for value in [
        b"".as_slice(),
        b" \t ",
        b" text/plain, application/json ",
        b" text/plain; x=a; X=b ",
        b" text/ plain ",
        b" text/plain; charset=\tutf-8 ",
        b" text/plain; x=\"unterminated ",
        b"\x0btext/plain\x0b",
        b"\x80text/plain\x80",
    ] {
        let head = response(Some(value));
        assert_eq!(
            origin::validate_response(&head, false),
            Err(Error::BadGateway)
        );
        assert_eq!(
            origin::validate_metadata(&head, &object(), None),
            Err(Error::BadGateway)
        );
    }
    let mut head = response(Some(b" text/plain\t"));
    head.headers
        .push(Header::new("racer-content-type", b"text/plain"));
    assert_eq!(
        origin::validate_metadata(&head, &object(), None),
        Err(Error::BadGateway)
    );
}

#[test]
fn opaque_numeric_and_range_fields_still_reject_padding() {
    for (name, value) in [
        ("Authorization", "Bearer token"),
        ("Racer-Metadata", "opaque value"),
        ("Content-Length", "1"),
        ("Racer-Expires-At", "1234"),
        ("Content-Range", "bytes 0-0/1"),
    ] {
        for (leading, trailing) in [(" ", ""), ("", " "), ("\t", ""), ("", "\t")] {
            let mut head = response(Some(b" \ttext/plain\t "));
            head.start = StartLine::Response { status: 206 };
            head.headers
                .push(Header::new("Content-Range", b"bytes 0-0/1"));
            head.headers
                .retain(|header| !header.name.eq_ignore_ascii_case(name));
            head.headers
                .push(Header::new(name, format!("{leading}{value}{trailing}")));
            assert_eq!(
                origin::validate_bootstrap(&head, &object()),
                Err(Error::BadGateway),
                "{name}: {leading:?}{value}{trailing:?}"
            );
            let page = PageId {
                version: ObjectVersion {
                    object: object(),
                    etag: StrongEtag::parse(b"\"v1\"").unwrap(),
                },
                number: PageNumber(0),
            };
            let expected = VersionMetadata {
                version: page.version.clone(),
                length: 1,
                content_type: Some(ContentType::parse(b"text/plain").unwrap()),
            };
            assert_eq!(
                origin::validate_page(&head, &page, &expected, 1),
                Err(Error::BadGateway)
            );
        }
    }
}
