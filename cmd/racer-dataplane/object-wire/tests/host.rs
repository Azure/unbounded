use http1::{Codec, Header, MessageHead, StartLine};
use racer_object_wire::client::{MAX_HEAD_BYTES, RequestParser};
use racer_object_wire::{CacheKey, Error};

fn request(method: &str, host: &str) -> MessageHead {
    MessageHead {
        start: StartLine::Request {
            method: method.into(),
            target: format!("/v1/objects/{}", CacheKey([0xab; 32]).to_hex()),
        },
        headers: vec![
            Header::new("Host", host),
            Header::new("Content-Length", "0"),
        ],
    }
}

#[test]
fn host_accepts_only_surrounding_http_ows_for_head_and_post() {
    let parser = RequestParser::new(MAX_HEAD_BYTES);
    for method in ["HEAD", "POST"] {
        for host in [
            "racer",
            " racer",
            "racer ",
            "\tracer",
            "racer\t",
            " \t racer\t  ",
        ] {
            let head = request(method, host);
            let parsed = parser.parse(&head).unwrap();
            assert_eq!(parsed.kind.is_head(), method == "HEAD");
            assert_eq!(head.headers[0].value, host.as_bytes());
        }
    }
}

#[test]
fn raw_http_host_ows_survives_decoding_and_parses() {
    let parser = RequestParser::new(MAX_HEAD_BYTES);
    for method in ["HEAD", "POST"] {
        for host in ["racer", " racer", "\tracer", "  racer \t", " \t racer\t  "] {
            let raw = format!(
                "{method} /v1/objects/{} HTTP/1.1\r\nHost:{host}\r\nContent-Length: 0\r\n\r\n",
                CacheKey([0xab; 32]).to_hex()
            );
            let (head, consumed) = Codec::<()>::new(MAX_HEAD_BYTES)
                .decode_head(raw.as_bytes())
                .unwrap()
                .unwrap();
            assert_eq!(consumed, raw.len());
            assert_eq!(
                head.unique("Host").unwrap().unwrap(),
                host.strip_prefix(' ').unwrap_or(host).as_bytes()
            );
            assert_eq!(
                parser.parse(&head).unwrap().kind.is_head(),
                method == "HEAD"
            );
        }
    }
}

#[test]
fn host_still_requires_one_matching_value() {
    let parser = RequestParser::new(MAX_HEAD_BYTES);
    for method in ["HEAD", "POST"] {
        for host in [
            "",
            " \t ",
            "other",
            " racer:80 ",
            "ra cer",
            "ra\tcer",
            "racer\u{a0}",
        ] {
            assert!(matches!(
                parser.parse(&request(method, host)),
                Err(Error::InvalidRequest)
            ));
        }
        let mut head = request(method, " \tracer\t ");
        head.headers.push(Header::new("hOsT", "racer"));
        assert!(matches!(parser.parse(&head), Err(Error::InvalidRequest)));
        head.headers
            .retain(|h| !h.name.eq_ignore_ascii_case("Host"));
        assert!(matches!(parser.parse(&head), Err(Error::InvalidRequest)));
    }
}

#[test]
fn host_normalization_does_not_normalize_numeric_or_opaque_fields() {
    let parser = RequestParser::new(MAX_HEAD_BYTES);
    for method in ["HEAD", "POST"] {
        for host in ["racer", " \tracer\t "] {
            for (name, value) in [
                ("Content-Length", "0"),
                ("Racer-Page-Credits", "2"),
                ("Racer-Byte-Credits", "33554432"),
                ("Authorization", "Bearer token"),
                ("Racer-Metadata", "a,\\b"),
            ] {
                let mut head = request(method, host);
                head.headers.retain(|h| h.name != name);
                head.headers.push(Header::new(name, value));
                let parsed = parser.parse(&head).unwrap();
                if name == "Authorization" {
                    assert_eq!(parsed.authorization, Some(value.as_bytes()));
                }
                if name == "Racer-Metadata" {
                    assert_eq!(parsed.metadata, Some(value.as_bytes()));
                }
                for padded in [
                    format!(" {value}"),
                    format!("{value} "),
                    format!("\t{value}"),
                    format!("{value}\t"),
                ] {
                    head.headers.last_mut().unwrap().value = padded.into_bytes();
                    assert!(
                        matches!(parser.parse(&head), Err(Error::InvalidRequest)),
                        "{method} {name}"
                    );
                }
            }
        }
    }
}
