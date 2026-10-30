use http1::StartLine;
use racer_object_wire::{CacheKey, Error, origin};

#[test]
fn head_and_get_preserve_request_fields() {
    let key = CacheKey([0xab; 32]);
    for method in ["HEAD", "GET"] {
        for (metadata, authorization) in [
            (None, None),
            (Some(b"a,\\b\x80".as_slice()), None),
            (None, Some(b"Bearer \xff".as_slice())),
            (
                Some(b"a,\\b\x80".as_slice()),
                Some(b"Bearer \xff".as_slice()),
            ),
        ] {
            let head = origin::request(&key, method, metadata, authorization).unwrap();
            assert!(matches!(
                &head.start,
                StartLine::Request { method: actual, target }
                    if actual == method && target == &format!("/v1/objects/{}", key.to_hex())
            ));
            assert_eq!(head.unique("Host").unwrap(), Some(b"racer".as_slice()));
            assert_eq!(
                head.unique("Content-Length").unwrap(),
                Some(b"0".as_slice())
            );
            assert_eq!(head.unique("Racer-Metadata").unwrap(), metadata);
            assert_eq!(head.unique("Authorization").unwrap(), authorization);
            assert_eq!(
                head.headers.len(),
                2 + usize::from(metadata.is_some()) + usize::from(authorization.is_some())
            );
        }
    }
}

#[test]
fn unsupported_and_malformed_methods_are_rejected() {
    let key = CacheKey([0xab; 32]);
    for method in [
        "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE", "CUSTOM", "head", "get",
        "Head", "Get", "", " GET", "GET ", "HEAD\t", "GET\r\n", "GÉT",
    ] {
        assert!(
            matches!(
                origin::request(&key, method, None, None),
                Err(Error::MethodNotAllowed)
            ),
            "method: {method:?}"
        );
    }
}

#[test]
fn allowed_methods_still_validate_opaque_context() {
    let key = CacheKey([0xab; 32]);
    for method in ["HEAD", "GET"] {
        for value in [
            b"".as_slice(),
            b" padded",
            b"padded ",
            b"tab\tvalue",
            b"line\r\nbreak",
            b"nul\0value",
            b"del\x7fvalue",
        ] {
            assert_eq!(origin::opaque(value), Err(Error::InvalidRequest));
            for (metadata, authorization) in [(Some(value), None), (None, Some(value))] {
                assert!(matches!(
                    origin::request(&key, method, metadata, authorization),
                    Err(Error::InvalidRequest)
                ));
            }
        }
        let boundary = vec![b'x'; 8192];
        assert_eq!(origin::opaque(&boundary), Ok(()));
        let head = origin::request(&key, method, Some(&boundary), Some(&boundary)).unwrap();
        assert_eq!(
            head.unique("Racer-Metadata").unwrap(),
            Some(boundary.as_slice())
        );
        assert_eq!(
            head.unique("Authorization").unwrap(),
            Some(boundary.as_slice())
        );
    }
}

#[test]
fn oversized_opaque_context_is_header_too_large() {
    let key = CacheKey([0xab; 32]);
    for method in ["HEAD", "GET"] {
        for byte in [b'x', b' ', b'\t', b'\r', b'\n', 0, 0x7f] {
            let mut value = vec![b'x'; 8193];
            value[0] = byte;
            assert_eq!(origin::opaque(&value), Err(Error::HeaderTooLarge));
            for (metadata, authorization) in [
                (Some(value.as_slice()), None),
                (None, Some(value.as_slice())),
                (Some(value.as_slice()), Some(value.as_slice())),
            ] {
                assert!(matches!(
                    origin::request(&key, method, metadata, authorization),
                    Err(Error::HeaderTooLarge)
                ));
            }
        }
    }
}

#[test]
fn malformed_opaque_context_at_limit_is_invalid_request() {
    let key = CacheKey([0xab; 32]);
    for byte in (0..=0x20).chain([0x7f]) {
        for position in [0, 8191] {
            let mut value = vec![b'x'; 8192];
            value[position] = byte;
            assert_eq!(origin::opaque(&value), Err(Error::InvalidRequest));
            for method in ["HEAD", "GET"] {
                for (metadata, authorization) in [
                    (Some(value.as_slice()), None),
                    (None, Some(value.as_slice())),
                ] {
                    assert!(matches!(
                        origin::request(&key, method, metadata, authorization),
                        Err(Error::InvalidRequest)
                    ));
                }
            }
        }
    }
}
