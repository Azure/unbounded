use racer_control_wire::CacheId;
use racer_object_wire::client;
use racer_object_wire::{
    ByteRange, CacheKey, Error, ExpiresAt, MAX_WIRE_INTEGER, ObjectId, ObjectMetadata,
    ObjectVersion, PAGE_BYTES, StrongEtag, parse_decimal,
};

fn metadata(length: u64) -> ObjectMetadata {
    ObjectMetadata {
        content_type: None,
        version: ObjectVersion {
            object: ObjectId {
                cache: CacheId("cache".into()),
                key: CacheKey([0; 32]),
            },
            etag: StrongEtag::parse(b"\"v1\"").unwrap(),
        },
        length,
        expires_at: ExpiresAt::from_unix_millis(0).unwrap(),
    }
}

#[test]
fn subscription_body_length_respects_the_exact_wire_boundary() {
    let pages = (MAX_WIRE_INTEGER - 21).div_ceil(PAGE_BYTES + 21);
    let maximum_payload = MAX_WIRE_INTEGER - 21 * (pages + 1);
    for (payload, expected) in [
        (maximum_payload - 1, Ok(MAX_WIRE_INTEGER - 1)),
        (maximum_payload, Ok(MAX_WIRE_INTEGER)),
        (maximum_payload + 1, Err(Error::BadGateway)),
    ] {
        let metadata = metadata(payload);
        assert_eq!(metadata.validate(), Ok(()));
        let range = ByteRange::from(0).unwrap().resolve(payload).unwrap();
        assert_eq!(range.last_page().0 + 1, pages);
        let result = client::subscription_head(&metadata, Some(range))
            .map(|head| parse_decimal(head.unique("Content-Length").unwrap().unwrap()).unwrap());
        assert_eq!(result, expected, "payload {payload}");
    }
}

#[test]
fn maximum_object_length_is_valid_for_head_but_not_a_full_subscription() {
    let metadata = metadata(MAX_WIRE_INTEGER);
    assert_eq!(metadata.validate(), Ok(()));
    let head = client::success_head(&metadata, None).unwrap();
    assert_eq!(
        parse_decimal(head.unique("Content-Length").unwrap().unwrap()),
        Ok(MAX_WIRE_INTEGER)
    );
    let range = ByteRange::from(0)
        .unwrap()
        .resolve(metadata.length)
        .unwrap();
    assert!(matches!(
        client::subscription_head(&metadata, Some(range)),
        Err(Error::BadGateway)
    ));
}

#[test]
fn small_subscription_at_the_maximum_offset_keeps_its_body_length() {
    let metadata = metadata(MAX_WIRE_INTEGER);
    let range = ByteRange::from(MAX_WIRE_INTEGER - 1)
        .unwrap()
        .resolve(metadata.length)
        .unwrap();
    let head = client::subscription_head(&metadata, Some(range)).unwrap();
    assert_eq!(
        head.unique("Content-Length").unwrap(),
        Some(b"43".as_slice())
    );
    assert_eq!(
        parse_decimal(head.unique("Racer-Range-End").unwrap().unwrap()),
        Ok(MAX_WIRE_INTEGER)
    );
}

#[test]
fn empty_subscription_keeps_its_completion_frame_length() {
    let head = client::subscription_head(&metadata(0), None).unwrap();
    assert_eq!(
        head.unique("Content-Length").unwrap(),
        Some(b"21".as_slice())
    );
}
