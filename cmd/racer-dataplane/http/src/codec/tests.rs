use super::*;
const MAX_HEAD_BYTES: usize = 32 * 1024;
struct Fields;
impl Opaque for Fields {
    const NAMES: &'static [&'static str] = &["authorization", "x-metadata"];
}
type TestCodec = Codec<Fields>;
#[test]
fn maximum_wire_heads_and_maximum_field_count_have_checked_decoded_bounds() {
    for limit in [MAX_HEAD_BYTES, 18 * 64 * 1024] {
        let codec = TestCodec::new(limit);
        let mut bytes = b"GET / HTTP/1.1\r\nX: ".to_vec();
        bytes.resize(limit - 4, b'a');
        bytes.extend_from_slice(b"\r\n\r\n");
        let (head, used) = codec.decode_head(&bytes).unwrap().unwrap();
        assert_eq!(used, limit);
        assert_eq!(codec.encode_head(&head).unwrap(), bytes);
        let allocation = codec.decoded_allocation(&bytes).unwrap();
        assert!(allocation >= head.headers[0].value.len() + std::mem::size_of::<Header>());
        let mut fields = b"GET / HTTP/1.1\r\n".to_vec();
        while fields.len() + 6 <= limit {
            fields.extend_from_slice(b"X:\r\n");
        }
        fields.extend_from_slice(b"\r\n");
        let count = (fields.len() - 18) / 4;
        let allocation = codec.decoded_allocation(&fields).unwrap();
        assert!(allocation >= count * std::mem::size_of::<Header>());
        assert_eq!(
            codec.decode_head(&fields).unwrap().unwrap().0.headers.len(),
            count
        );
        assert!(matches!(
            TestCodec::new(limit - 1).decoded_allocation(&bytes),
            Err(Error::HeadTooLarge)
        ));
    }
}
#[test]
fn fragmented_head_preserves_opaque_values_and_duplicates() {
    let codec = TestCodec::new(1024);
    let bytes = b"GET /a HTTP/1.1\r\nX-Opaque:  \xff\t \r\nx-opaque: second\r\nContent-Length: 3\r\n\r\nabc";
    let end = bytes.len() - 3;
    for length in 0..end {
        assert!(codec.decode_head(&bytes[..length]).unwrap().is_none());
    }
    let (head, used) = codec.decode_head(bytes).unwrap().unwrap();
    assert_eq!(used, end);
    assert_eq!(head.headers[0].value, b" \xff\t ");
    assert_eq!(head.values("x-OPAQUE").count(), 2);
    assert_eq!(head.unique("x-opaque"), Err(Error::Malformed));
    assert_eq!(codec.encode_head(&head).unwrap(), bytes[..end]);
}
#[test]
fn rejects_smuggling_and_malformed_syntax() {
    let codec = TestCodec::new(1024);
    for bytes in [
        &b"GET / HTTP/1.1\n\n"[..],
        &b"GET / HTTP/1.0\r\n\r\n"[..],
        &b"GET / HTTP/1.1\r\nContent-Length: 1\r\ncontent-length: 1\r\n\r\n"[..],
        &b"GET / HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n"[..],
        &b"GET / HTTP/1.1\r\nContent-Length: +1\r\n\r\n"[..],
        &b"GET / HTTP/1.1\r\nContent-Length: 18446744073709551616\r\n\r\n"[..],
        &b"GET / HTTP/1.1\r\nX: first\r\n second\r\n\r\n"[..],
        &b"GET / HTTP/1.1\r\nConnection: content-length\r\n\r\n"[..],
        &b"GET / HTTP/1.1\r\nBad : x\r\n\r\n"[..],
    ] {
        assert!(codec.decode_head(bytes).is_err(), "accepted malformed head");
    }
}
#[test]
fn bounds_heads_without_counting_read_ahead_or_head_representation_length() {
    let bytes = b"HTTP/1.1 200 OK\r\nContent-Length: 99999999\r\n\r\n";
    let codec = TestCodec::new(bytes.len());
    assert_eq!(codec.decode_head(bytes).unwrap().unwrap().1, bytes.len());
    assert!(TestCodec::new(bytes.len() - 1).decode_head(bytes).is_err());
    assert!(TestCodec::new(8).decode_head(b"GET /thi").is_err());
    let head = MessageHead {
        start: StartLine::Response { status: 200 },
        headers: vec![Header {
            name: "x".into(),
            value: b"a\r\ninjected: true".to_vec(),
        }],
    };
    assert!(codec.encode_head(&head).is_err());
}
#[test]
fn opaque_field_raw_separators_are_validated_before_decoding() {
    let codec = TestCodec::new(MAX_HEAD_BYTES);
    for field in ["Authorization", "X-Metadata"] {
        for suffix in [
            &b"x"[..],
            &b"  x"[..],
            &b"\tx"[..],
            &b" \tx"[..],
            &b" x "[..],
            &b" x\t"[..],
        ] {
            let mut bytes = format!("GET / HTTP/1.1\r\n{field}:").into_bytes();
            bytes.extend_from_slice(suffix);
            bytes.extend_from_slice(b"\r\n\r\n");
            assert!(codec.decode_head(&bytes).is_err());
        }
        let mut bytes = format!("GET / HTTP/1.1\r\n{field}: ").into_bytes();
        bytes.extend_from_slice(b"\xffopaque\x80\r\n\r\n");
        let (head, _) = codec.decode_head(&bytes).unwrap().unwrap();
        assert_eq!(head.unique(field).unwrap().unwrap(), b"\xffopaque\x80");
        assert_eq!(codec.encode_head(&head).unwrap(), bytes);
    }
    assert!(matches!(
        codec.decode_head(&vec![b'x'; MAX_HEAD_BYTES]),
        Err(Error::HeadTooLarge)
    ));
}
#[test]
fn empty_and_custom_opaque_lists_are_case_insensitive() {
    let bytes = b"GET / HTTP/1.1\r\nAuThOrIzAtIoN:  padded \r\n\r\n";
    let plain = Codec::<()>::new(1024);
    let head = plain.decode_head(bytes).unwrap().unwrap().0;
    assert_eq!(plain.encode_head(&head).unwrap(), bytes);
    assert!(TestCodec::new(1024).decode_head(bytes).is_err());
    assert!(TestCodec::new(1024).encode_head(&head).is_err());
}
