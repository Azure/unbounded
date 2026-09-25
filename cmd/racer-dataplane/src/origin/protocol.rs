//! SDK wire rules shared by HEAD and whole-page responses.
use crate::{
    error::{Error, Result},
    http::codec::{MessageHead, StartLine},
    model::{
        identity::{ObjectId, ObjectVersion, StrongEtag},
        metadata::{ExpiresAt, ObjectMetadata},
    },
};

pub(super) fn field<'a>(head: &'a MessageHead, name: &str) -> Result<Option<&'a [u8]>> {
    let mut values = head
        .headers
        .iter()
        .filter(|h| h.name.eq_ignore_ascii_case(name));
    let value = values.next().map(|h| h.value.as_slice());
    if values.next().is_some() {
        return Err(Error::BadGateway);
    }
    // Preserve canonical numeric fields before decimal/range validation, just as
    // opaque context preserves exact bytes. The codec removed only separator SP;
    // trimming here would accept forbidden padding in expiry, lengths, or ranges.
    Ok(value.map(|value| {
        if name.eq_ignore_ascii_case("Authorization")
            || name.eq_ignore_ascii_case("Racer-Metadata")
            || name.eq_ignore_ascii_case("Racer-Expires-At")
            || name.eq_ignore_ascii_case("Content-Length")
            || name.eq_ignore_ascii_case("Content-Range")
        {
            value
        } else {
            trim_ows(value)
        }
    }))
}

fn trim_ows(mut value: &[u8]) -> &[u8] {
    while matches!(value.first(), Some(b' ' | b'\t')) {
        value = &value[1..];
    }
    while matches!(value.last(), Some(b' ' | b'\t')) {
        value = &value[..value.len() - 1];
    }
    value
}

pub(super) fn required<'a>(head: &'a MessageHead, name: &str) -> Result<&'a [u8]> {
    field(head, name)?.ok_or(Error::BadGateway)
}

/// Canonical decimal, bounded by the SDK's nonnegative signed-64-bit domain.
pub(super) fn decimal(bytes: &[u8]) -> Result<u64> {
    crate::model::parse_decimal(bytes).map_err(|_| Error::BadGateway)
}

pub(super) fn absent(head: &MessageHead, names: &[&str]) -> Result<()> {
    for name in names {
        if field(head, name)?.is_some() {
            return Err(Error::BadGateway);
        }
    }
    Ok(())
}

/// Validate framing before interpreting status; malformed errors are never miss proof.
pub(super) fn response(head: &MessageHead, pinned: bool) -> Result<(u16, u64)> {
    for name in [
        "Host",
        "Content-Length",
        "Content-Type",
        "Content-Range",
        "ETag",
        "If-Match",
        "Range",
        "Racer-Expires-At",
        "Racer-Metadata",
        "Authorization",
    ] {
        field(head, name)?;
    }
    for name in ["Racer-Metadata", "Authorization"] {
        if let Some(value) = field(head, name)? {
            if value.is_empty()
                || value.len() > 8192
                || value.first() == Some(&b' ')
                || value.last() == Some(&b' ')
                || value.iter().any(|byte| *byte < 32 || *byte == 127)
            {
                return Err(Error::BadGateway);
            }
        }
    }
    absent(
        head,
        &[
            "Transfer-Encoding",
            "Content-Encoding",
            "Trailer",
            "Upgrade",
            "Expect",
            "If-None-Match",
            "If-Modified-Since",
            "If-Unmodified-Since",
            "If-Range",
        ],
    )?;
    for connection in head.values("Connection") {
        if connection
            .split(|b| *b == b',')
            .any(|token| token.trim_ascii().eq_ignore_ascii_case(b"upgrade"))
        {
            return Err(Error::BadGateway);
        }
    }
    let status = match head.start {
        StartLine::Response { status } => status,
        _ => return Err(Error::BadGateway),
    };
    let length = decimal(required(head, "Content-Length")?)?;
    if status == 200 || status == 206 {
        return Ok((status, length));
    }
    if length != 0 {
        return Err(Error::BadGateway);
    }
    absent(head, &["ETag", "Racer-Expires-At"])?;
    let unsatisfied_length = if status == 416 {
        let value = required(head, "Content-Range")?;
        Some(decimal(
            value.strip_prefix(b"bytes */").ok_or(Error::BadGateway)?,
        )?)
    } else {
        absent(head, &["Content-Range"])?;
        None
    };
    if status == 405 && required(head, "Allow")? != b"HEAD, GET" {
        return Err(Error::BadGateway);
    }
    Err(match status {
        400 => Error::InvalidRequest,
        405 => Error::MethodNotAllowed,
        431 => Error::HeaderTooLarge,
        401 => Error::OriginRejected,
        403 => Error::OriginForbidden,
        404 if pinned => Error::BadGateway,
        404 => Error::NotFound,
        500 => Error::Internal,
        503 => Error::Unavailable,
        412 => Error::VersionUnavailable,
        416 => Error::UnsatisfiableRangeWithLength(unsatisfied_length.ok_or(Error::BadGateway)?),
        _ => Error::BadGateway,
    })
}

pub(super) fn metadata(
    head: &MessageHead,
    object: &ObjectId,
    length: u64,
) -> Result<ObjectMetadata> {
    let etag = StrongEtag::parse(required(head, "ETag")?).map_err(|_| Error::BadGateway)?;
    let expires_at =
        ExpiresAt::parse(required(head, "Racer-Expires-At")?).map_err(|_| Error::BadGateway)?;
    Ok(ObjectMetadata {
        version: ObjectVersion {
            object: object.clone(),
            etag,
        },
        length,
        expires_at,
    })
}

pub(super) fn content_range(head: &MessageHead) -> Result<(u64, u64, u64)> {
    let value = required(head, "Content-Range")?
        .strip_prefix(b"bytes ")
        .ok_or(Error::BadGateway)?;
    let slash = value
        .iter()
        .position(|b| *b == b'/')
        .ok_or(Error::BadGateway)?;
    let bounds = &value[..slash];
    let dash = bounds
        .iter()
        .position(|b| *b == b'-')
        .ok_or(Error::BadGateway)?;
    let first = decimal(&bounds[..dash])?;
    let last = decimal(&bounds[dash + 1..])?;
    let total = decimal(&value[slash + 1..])?;
    if first > last || last >= total {
        return Err(Error::BadGateway);
    }
    Ok((first, last, total))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::http::codec::Header;

    #[test]
    fn numeric_headers_reject_padding_before_any_normalization() {
        use crate::{
            http::codec::Codec,
            model::identity::{CacheId, CacheKey},
        };
        let object = ObjectId {
            cache: CacheId("cache".into()),
            key: CacheKey([0; 32]),
        };
        for (name, canonical) in [
            ("Racer-Expires-At", "0"),
            ("Content-Length", "1"),
            ("Content-Range", "bytes 0-0/1"),
        ] {
            for (prefix, suffix) in [("", ""), (" ", ""), ("", " "), ("\t", ""), ("", "\t")] {
                let fields = [
                    ("Content-Length", "1"),
                    ("Content-Type", "application/octet-stream"),
                    ("Content-Range", "bytes 0-0/1"),
                    ("ETag", "\"v\""),
                    ("Racer-Expires-At", "0"),
                ];
                let mut raw = String::from("HTTP/1.1 206 Partial Content\r\n");
                for (field, value) in fields {
                    if field == name {
                        raw.push_str(&format!("{field}: {prefix}{canonical}{suffix}\r\n"));
                    } else {
                        raw.push_str(&format!("{field}: {value}\r\n"));
                    }
                }
                raw.push_str("\r\n");
                let result = Codec::new(32768, 1)
                    .decode_head(raw.as_bytes())
                    .map_err(|_| Error::BadGateway)
                    .and_then(|head| {
                        super::super::metadata::validate_bootstrap(&head.unwrap().0, &object)
                    });
                if prefix.is_empty() && suffix.is_empty() {
                    assert_eq!(result.unwrap().1, 1);
                } else {
                    assert_eq!(result, Err(Error::BadGateway), "{name}");
                }
            }
        }
    }

    #[test]
    fn all_sdk_singletons_reject_case_insensitive_duplicates() {
        for name in [
            "Host",
            "Content-Length",
            "Content-Type",
            "Content-Range",
            "ETag",
            "If-Match",
            "Range",
            "Racer-Expires-At",
            "Racer-Metadata",
            "Authorization",
        ] {
            let mut head = MessageHead {
                start: StartLine::Response { status: 200 },
                headers: vec![Header {
                    name: "Content-Length".into(),
                    value: b"0".to_vec(),
                }],
            };
            if name != "Content-Length" {
                head.headers.push(Header {
                    name: name.into(),
                    value: b"x".to_vec(),
                });
            }
            head.headers.push(Header {
                name: name.to_ascii_lowercase(),
                value: b"x".to_vec(),
            });
            assert_eq!(response(&head, false), Err(Error::BadGateway), "{name}");
        }
    }

    #[test]
    fn error_contracts_preserve_origin_credential_and_status_distinctions() {
        for (status, expected) in [
            (400, Error::InvalidRequest),
            (401, Error::OriginRejected),
            (403, Error::OriginForbidden),
            (404, Error::NotFound),
            (405, Error::MethodNotAllowed),
            (412, Error::VersionUnavailable),
            (416, Error::UnsatisfiableRangeWithLength(27)),
            (431, Error::HeaderTooLarge),
            (500, Error::Internal),
            (502, Error::BadGateway),
            (503, Error::Unavailable),
            (302, Error::BadGateway),
        ] {
            let mut head = MessageHead {
                start: StartLine::Response { status },
                headers: vec![Header {
                    name: "Content-Length".into(),
                    value: b"0".to_vec(),
                }],
            };
            if status == 416 {
                head.headers.push(Header {
                    name: "Content-Range".into(),
                    value: b"bytes */27".to_vec(),
                });
            }
            if status == 405 {
                head.headers.push(Header {
                    name: "Allow".into(),
                    value: b"HEAD, GET".to_vec(),
                });
            }
            assert_eq!(response(&head, false), Err(expected));
            if status == 404 {
                assert_eq!(response(&head, true), Err(Error::BadGateway));
            }
            head.headers[0].value = b"1".to_vec();
            assert_eq!(response(&head, false), Err(Error::BadGateway));
            head.headers[0].value = b"0".to_vec();
            head.headers.push(Header {
                name: "ETag".into(),
                value: b"\"v\"".to_vec(),
            });
            assert_eq!(response(&head, false), Err(Error::BadGateway));
        }
    }
}
