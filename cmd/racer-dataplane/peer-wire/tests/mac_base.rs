use http1::{Header, MessageHead, StartLine};
use peer_wire::{Error, MAX_ENVELOPE_HEAD, mac_base};

const EXPECTED: &[u8] = b"racer/request-mac/message/v1\0\
    \x00\x00\x00\x09POST /mac\
    \x00\x00\x00\x05alpha\x00\x00\x00\x03a b\
    \x00\x00\x00\x0dauthorization\x00\x00\x00\x02A\xff\
    \x00\x00\x00\x04host\x00\x00\x00\x00\
    \x00\x00\x00\x0dracer-mac-key\x00\x00\x00\x03key\
    \x00\x00\x00\x04zeta\x00\x00\x00\x01Z";

fn request() -> MessageHead {
    MessageHead {
        start: StartLine::Request {
            method: "POST".into(),
            target: "/mac".into(),
        },
        headers: vec![
            Header::new("Zeta", b"Z"),
            Header::new("Racer-Mac-Key", b"key"),
            Header::new("Host", b""),
            Header::new("Authorization", b"A\xff"),
            Header::new("Alpha", b"a b"),
        ],
    }
}

fn minimal_request() -> MessageHead {
    MessageHead {
        start: StartLine::Request {
            method: "GET".into(),
            target: "/".into(),
        },
        headers: vec![Header::new("host", b"")],
    }
}

#[test]
fn domain_separated_byte_vector_preserves_exact_values() {
    assert_eq!(mac_base(&request()).unwrap(), EXPECTED);
    assert_eq!(
        mac_base(&minimal_request()).unwrap(),
        b"racer/request-mac/message/v1\0\
          \x00\x00\x00\x05GET /\
          \x00\x00\x00\x04host\x00\x00\x00\x00"
    );
}

#[test]
fn ordering_and_header_case_do_not_change_the_vector() {
    let mut head = request();
    for _ in 0..head.headers.len() {
        head.headers.rotate_left(1);
        for field in &mut head.headers {
            field.name = field.name.to_ascii_uppercase();
        }
        assert_eq!(mac_base(&head).unwrap(), EXPECTED);
    }
    head.headers.reverse();
    assert_eq!(mac_base(&head).unwrap(), EXPECTED);
}

#[test]
fn mac_and_signature_fields_are_excluded_but_mac_key_is_bound() {
    let mut head = request();
    for name in ["RaCeR-ReQuEsT-MaC", "Signature", "SIGNATURE-INPUT"] {
        head.headers.push(Header::new(name, b"first"));
        assert_eq!(mac_base(&head).unwrap(), EXPECTED, "{name}");
        head.headers.last_mut().unwrap().value = b"different".to_vec();
        assert_eq!(mac_base(&head).unwrap(), EXPECTED, "{name}");
    }
    head.headers
        .iter_mut()
        .find(|field| field.name == "Racer-Mac-Key")
        .unwrap()
        .value = b"other-key".to_vec();
    assert_ne!(mac_base(&head).unwrap(), EXPECTED);
}

#[test]
fn exclusion_does_not_match_a_mac_field_prefix() {
    let mut head = minimal_request();
    head.headers
        .push(Header::new("racer-request-mac-extra", b"bound"));
    assert_eq!(
        mac_base(&head).unwrap(),
        b"racer/request-mac/message/v1\0\
          \x00\x00\x00\x05GET /\
          \x00\x00\x00\x04host\x00\x00\x00\x00\
          \x00\x00\x00\x17racer-request-mac-extra\x00\x00\x00\x05bound"
    );
}

#[test]
fn length_framing_distinguishes_name_value_boundaries() {
    let mut first = minimal_request();
    first.headers.push(Header::new("a", b"bc"));
    let mut second = minimal_request();
    second.headers.push(Header::new("ab", b"c"));
    assert_eq!(
        mac_base(&first).unwrap(),
        b"racer/request-mac/message/v1\0\
          \x00\x00\x00\x05GET /\
          \x00\x00\x00\x01a\x00\x00\x00\x02bc\
          \x00\x00\x00\x04host\x00\x00\x00\x00"
    );
    assert_eq!(
        mac_base(&second).unwrap(),
        b"racer/request-mac/message/v1\0\
          \x00\x00\x00\x05GET /\
          \x00\x00\x00\x02ab\x00\x00\x00\x01c\
          \x00\x00\x00\x04host\x00\x00\x00\x00"
    );
    assert_ne!(mac_base(&first).unwrap(), mac_base(&second).unwrap());
}

#[test]
fn length_prefix_is_big_endian_across_the_single_byte_boundary() {
    let mut head = minimal_request();
    head.headers.push(Header::new("x", [b'a'; 256]));
    let expected = [
        b"racer/request-mac/message/v1\0\
          \x00\x00\x00\x05GET /\
          \x00\x00\x00\x04host\x00\x00\x00\x00\
          \x00\x00\x00\x01x\x00\x00\x01\x00"
            .as_slice(),
        &[b'a'; 256],
    ]
    .concat();
    assert_eq!(mac_base(&head).unwrap(), expected);
}

#[test]
fn method_target_and_each_included_value_are_bound() {
    for (method, target) in [("HEAD", "/mac"), ("POST", "/other")] {
        let mut head = request();
        head.start = StartLine::Request {
            method: method.into(),
            target: target.into(),
        };
        assert_ne!(mac_base(&head).unwrap(), EXPECTED);
    }
    for index in 0..request().headers.len() {
        let mut head = request();
        head.headers[index].value = b"changed".to_vec();
        assert_ne!(mac_base(&head).unwrap(), EXPECTED, "field {index}");
    }
}

#[test]
fn responses_are_not_request_mac_messages() {
    let mut head = request();
    head.start = StartLine::Response { status: 200 };
    assert_eq!(mac_base(&head), Err(Error::Unauthorized));
}

#[test]
fn duplicate_fields_are_rejected_even_when_excluded() {
    for name in ["x", "racer-request-mac", "signature", "signature-input"] {
        let mut head = minimal_request();
        head.headers.push(Header::new(name, b"same"));
        head.headers
            .push(Header::new(name.to_ascii_uppercase(), b"same"));
        assert_eq!(mac_base(&head), Err(Error::Unauthorized), "{name}");
    }
}

#[test]
fn noncanonical_values_are_rejected_even_when_excluded() {
    for name in ["x", "racer-request-mac", "signature", "signature-input"] {
        for value in [b" leading".as_slice(), b"trailing ", b"in\tternal"] {
            let mut head = minimal_request();
            head.headers.push(Header::new(name, value));
            assert_eq!(
                mac_base(&head),
                Err(Error::Unauthorized),
                "{name}: {value:?}"
            );
        }
        for byte in (0..=31).chain(std::iter::once(127)).filter(|b| *b != b'\t') {
            let mut head = minimal_request();
            head.headers.push(Header::new(name, [b'a', byte, b'z']));
            assert_eq!(
                mac_base(&head),
                Err(Error::InvalidRequest),
                "{name}: {byte}"
            );
        }
    }
}

#[test]
fn malformed_http_heads_are_rejected() {
    for (method, target) in [
        ("", "/"),
        ("G ET", "/"),
        ("GET", "/bad target"),
        ("GET", "/%zz"),
    ] {
        let mut head = minimal_request();
        head.start = StartLine::Request {
            method: method.into(),
            target: target.into(),
        };
        assert_eq!(mac_base(&head), Err(Error::InvalidRequest));
    }
    for name in ["", "bad name", "@method"] {
        let mut head = minimal_request();
        head.headers.push(Header::new(name, b"value"));
        assert_eq!(mac_base(&head), Err(Error::InvalidRequest), "{name}");
    }
    let mut head = minimal_request();
    head.headers.clear();
    assert_eq!(mac_base(&head), Err(Error::InvalidRequest));
}

#[test]
fn oversized_heads_are_rejected_even_when_the_field_is_excluded() {
    for name in ["x", "racer-request-mac"] {
        let mut head = minimal_request();
        head.headers
            .push(Header::new(name, vec![b'a'; MAX_ENVELOPE_HEAD]));
        assert_eq!(mac_base(&head), Err(Error::HeaderTooLarge), "{name}");
    }
}
