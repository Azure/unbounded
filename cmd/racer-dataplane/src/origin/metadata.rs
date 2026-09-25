//! HEAD/initial-GET validation; empty objects produce metadata without a page.
use super::{page::OriginPage, protocol};
use crate::{
    error::{Error, Result},
    http::codec::MessageHead,
    model::{identity::ObjectId, metadata::ObjectMetadata, range::PAGE_BYTES},
};
pub struct MetadataReply {
    pub metadata: ObjectMetadata,
    pub page_zero: Option<OriginPage>,
}

/// The initial GET selects metadata and page zero atomically at the adapter.
pub fn validate_bootstrap(head: &MessageHead, object: &ObjectId) -> Result<(ObjectMetadata, u64)> {
    let (status, length) = protocol::response(head, false)?;
    if protocol::required(head, "Content-Type")? != b"application/octet-stream" {
        return Err(Error::BadGateway);
    }
    let total = if status == 200 {
        if length != 0 {
            return Err(Error::BadGateway);
        }
        protocol::absent(head, &["Content-Range"])?;
        0
    } else {
        let (first, last, total) = protocol::content_range(head)?;
        if first != 0 || length != last + 1 || length != total.min(PAGE_BYTES) {
            return Err(Error::BadGateway);
        }
        total
    };
    Ok((protocol::metadata(head, object, total)?, length))
}
pub fn validate(head: &MessageHead, object: &ObjectId) -> Result<ObjectMetadata> {
    let (status, length) = protocol::response(head, false)?;
    if status != 200 {
        return Err(Error::BadGateway);
    }
    protocol::absent(head, &["Content-Range"])?;
    protocol::metadata(head, object, length)
}
#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        http::codec::{Header, StartLine},
        model::identity::{CacheId, CacheKey},
    };
    use std::time::{Duration, UNIX_EPOCH};

    pub(super) fn object() -> ObjectId {
        ObjectId {
            cache: CacheId("cache-uid".into()),
            key: CacheKey([0xab; 32]),
        }
    }

    fn head(length: &[u8], expiry: &[u8], etag: &[u8]) -> MessageHead {
        MessageHead {
            start: StartLine::Response { status: 200 },
            headers: vec![
                Header {
                    name: "Content-Length".into(),
                    value: length.to_vec(),
                },
                Header {
                    name: "Racer-Expires-At".into(),
                    value: expiry.to_vec(),
                },
                Header {
                    name: "ETag".into(),
                    value: etag.to_vec(),
                },
            ],
        }
    }

    #[test]
    fn head_retains_quoted_validator_and_exact_millisecond_expiry() {
        let metadata = validate(&head(b"0", b"1234", b"\"v,\\1\""), &object()).unwrap();
        assert_eq!(metadata.length, 0);
        assert_eq!(metadata.version.etag.as_bytes(), b"\"v,\\1\"");
        assert_eq!(
            metadata.expires_at.0,
            UNIX_EPOCH + Duration::from_millis(1234)
        );
        assert_eq!(
            validate(
                &head(b"9223372036854775807", b"9223372036854775807", b"\"\""),
                &object()
            )
            .unwrap()
            .length,
            i64::MAX as u64
        );
    }

    #[test]
    fn metadata_rejects_ambiguous_and_out_of_domain_fields() {
        for invalid in [
            b"".as_slice(),
            b"01",
            b"-1",
            b"+1",
            b" 1",
            b"1 ",
            b"\t1",
            b"1\t",
            b"1.0",
            b"9223372036854775808",
        ] {
            assert_eq!(protocol::decimal(invalid), Err(Error::BadGateway));
            assert_eq!(
                validate(&head(b"1", invalid, b"\"v\""), &object()),
                Err(Error::BadGateway)
            );
            assert_eq!(
                validate(&head(invalid, b"0", b"\"v\""), &object()),
                Err(Error::BadGateway)
            );
        }
        for etag in [b"v".as_slice(), b"W/\"v\"", b"*", b"\"v\", \"w\""] {
            assert_eq!(
                validate(&head(b"1", b"0", etag), &object()),
                Err(Error::BadGateway)
            );
        }
        for name in [
            "ETag",
            "Content-Length",
            "Racer-Expires-At",
            "Content-Range",
            "Transfer-Encoding",
            "Content-Encoding",
        ] {
            let mut response = head(b"1", b"0", b"\"v\"");
            response.headers.push(Header {
                name: name.into(),
                value: b"1".to_vec(),
            });
            assert_eq!(validate(&response, &object()), Err(Error::BadGateway));
        }
    }

    #[test]
    fn bootstrap_empty_and_short_page_are_distinct_from_head() {
        let mut response = head(b"0", b"0", b"\"v\"");
        response.headers.push(Header {
            name: "Content-Type".into(),
            value: b"application/octet-stream".to_vec(),
        });
        assert_eq!(validate_bootstrap(&response, &object()).unwrap().1, 0);
        response.headers[0].value = b"3".to_vec();
        assert_eq!(
            validate_bootstrap(&response, &object()),
            Err(Error::BadGateway)
        );
        response.start = StartLine::Response { status: 206 };
        response.headers.push(Header {
            name: "Content-Range".into(),
            value: b"bytes 0-2/3".to_vec(),
        });
        assert_eq!(
            validate_bootstrap(&response, &object()).unwrap().0.length,
            3
        );
        response.headers.last_mut().unwrap().value = b"bytes 0-2/4".to_vec();
        assert_eq!(
            validate_bootstrap(&response, &object()),
            Err(Error::BadGateway)
        );
    }

    #[test]
    fn raw_expiry_whitespace_is_rejected_before_metadata_publication() {
        use crate::http::codec::Codec;
        for expiry in ["0", " 0", "0 ", "\t0", "0\t", "0 \t"] {
            let raw = format!(
                "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nETag: \"v\"\r\nRacer-Expires-At: {expiry}\r\n\r\n"
            );
            let (head, _) = Codec::new(32768, 0)
                .decode_head(raw.as_bytes())
                .unwrap()
                .unwrap();
            let result = validate(&head, &object());
            if expiry == "0" {
                assert_eq!(result.unwrap().expires_at.0, UNIX_EPOCH);
            } else {
                assert_eq!(result, Err(Error::BadGateway), "expiry={expiry:?}");
            }
        }
    }
}
