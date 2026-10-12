//! Validate origin response framing, status, metadata, and whole-page boundaries.

use crate::model::{
    ContentType, ExpiresAt, ObjectId, ObjectMetadata, ObjectVersion, PAGE_BYTES, PageId, StrongEtag,
};
use crate::{Error, Result};
use http1::{MessageHead, StartLine};

/// Encode one origin request, borrowing sensitive fields only for this head.
pub fn request(
    key: &crate::CacheKey,
    method: &str,
    metadata: Option<&[u8]>,
    authorization: Option<&[u8]>,
) -> Result<MessageHead> {
    let target = format!("/v1/objects/{}", key.to_hex());
    let mut headers = vec![
        http1::Header::new("Host", b"racer"),
        http1::Header::new("Content-Length", b"0"),
    ];
    for (name, value) in [
        ("Racer-Metadata", metadata),
        ("Authorization", authorization),
    ] {
        if let Some(bytes) = value {
            opaque(bytes)?;
            headers.push(http1::Header::new(name, bytes));
        }
    }
    Ok(MessageHead {
        start: StartLine::Request {
            method: method.to_owned(),
            target,
        },
        headers,
    })
}

/// Validate opaque origin context without interpreting credentials or normalizing bytes.
pub fn opaque(bytes: &[u8]) -> Result<()> {
    if bytes.is_empty()
        || bytes.len() > 8192
        || bytes.first() == Some(&b' ')
        || bytes.last() == Some(&b' ')
        || bytes.iter().any(|b| *b < 0x20 || *b == 0x7f)
    {
        return Err(Error::InvalidRequest);
    }
    Ok(())
}

/// The initial GET selects metadata and page zero atomically at the adapter.
pub fn validate_bootstrap(head: &MessageHead, object: &ObjectId) -> Result<(ObjectMetadata, u64)> {
    let (status, length) = validate_response(head, false)?;
    if required(head, "Content-Type")? != b"application/octet-stream" {
        return Err(Error::BadGateway);
    }
    let total = if status == 200 {
        if length != 0 {
            return Err(Error::BadGateway);
        }
        absent(head, &["Content-Range"])?;
        0
    } else {
        let (first, last, total) = content_range(head)?;
        if first != 0 || length != last + 1 || length != total.min(PAGE_BYTES) {
            return Err(Error::BadGateway);
        }
        total
    };
    Ok((metadata(head, object, total)?, length))
}

/// Validate an origin HEAD response without allocating an object body.
pub fn validate_metadata(head: &MessageHead, object: &ObjectId) -> Result<ObjectMetadata> {
    let (status, length) = validate_response(head, false)?;
    if status != 200 {
        return Err(Error::BadGateway);
    }
    absent(head, &["Content-Range"])?;
    metadata(head, object, length)
}

/// Require exact If-Match, Content-Range, whole-page length, and final-page bounds.
/// Reject multipart, short/overlong bodies, and unexpected versions.
pub fn validate_page(
    head: &MessageHead,
    page: &PageId,
    received_bytes: usize,
) -> Result<ObjectMetadata> {
    let (metadata, length) = validate_page_head(head, page)?;
    if received_bytes as u64 != length {
        return Err(Error::BadGateway);
    }
    Ok(metadata)
}

/// Validate before allocating or receiving any payload.
pub fn validate_page_head(head: &MessageHead, page: &PageId) -> Result<(ObjectMetadata, u64)> {
    let (status, length) = validate_response(head, true)?;
    if status != 206 || required(head, "Content-Type")? != b"application/octet-stream" {
        return Err(Error::BadGateway);
    }
    let (first, last, total) = content_range(head)?;
    let expected_first = page
        .number
        .0
        .checked_mul(PAGE_BYTES)
        .ok_or(Error::InvalidRange)?;
    if first != expected_first
        || length != last - first + 1
        || length != (total - first).min(PAGE_BYTES)
    {
        return Err(Error::BadGateway);
    }
    let metadata = metadata(head, &page.version.object, total)?;
    if metadata.version != page.version {
        return Err(Error::BadGateway);
    }
    Ok((metadata, length))
}

/// Read one unique field, preserving canonical numeric and opaque bytes.
fn field<'a>(head: &'a MessageHead, name: &str) -> Result<Option<&'a [u8]>> {
    let value = head.unique(name).map_err(|_| Error::BadGateway)?;
    // The codec removed only separator SP. Trimming numeric fields would accept padding.
    Ok(value.map(|value| {
        if name.eq_ignore_ascii_case("Authorization")
            || name.eq_ignore_ascii_case("Racer-Metadata")
            || name.eq_ignore_ascii_case("Racer-Expires-At")
            || name.eq_ignore_ascii_case("Racer-Content-Type")
            || name.eq_ignore_ascii_case("Content-Length")
            || name.eq_ignore_ascii_case("Content-Range")
        {
            value
        } else {
            http1::trim_ows(value)
        }
    }))
}

/// Read a required, unique response field.
fn required<'a>(head: &'a MessageHead, name: &str) -> Result<&'a [u8]> {
    field(head, name)?.ok_or(Error::BadGateway)
}

/// Canonical decimal, bounded by the SDK's nonnegative signed-64-bit domain.
fn decimal(bytes: &[u8]) -> Result<u64> {
    crate::model::parse_decimal(bytes).map_err(|_| Error::BadGateway)
}

/// Reject fields forbidden for this response.
fn absent(head: &MessageHead, names: &[&str]) -> Result<()> {
    for name in names {
        if field(head, name)?.is_some() {
            return Err(Error::BadGateway);
        }
    }
    Ok(())
}

/// Validate framing before interpreting status; malformed errors are never miss proof.
pub fn validate_response(head: &MessageHead, pinned: bool) -> Result<(u16, u64)> {
    for name in [
        "Host",
        "Content-Length",
        "Content-Type",
        "Content-Range",
        "ETag",
        "If-Match",
        "Range",
        "Racer-Expires-At",
        "Racer-Content-Type",
        "Racer-Metadata",
        "Authorization",
    ] {
        field(head, name)?;
    }
    for name in ["Racer-Metadata", "Authorization"] {
        if let Some(value) = field(head, name)?
            && (value.is_empty()
                || value.len() > 8192
                || value.first() == Some(&b' ')
                || value.last() == Some(&b' ')
                || value.iter().any(|byte| *byte < 32 || *byte == 127))
        {
            return Err(Error::BadGateway);
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
    if let Some(value) = field(head, "Racer-Content-Type")? {
        ContentType::parse(value).map_err(|_| Error::BadGateway)?;
    }
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
    absent(head, &["ETag", "Racer-Expires-At", "Racer-Content-Type"])?;
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

/// Decode immutable identity and expiry after framing has been checked.
pub fn metadata(head: &MessageHead, object: &ObjectId, length: u64) -> Result<ObjectMetadata> {
    let etag = StrongEtag::parse(required(head, "ETag")?).map_err(|_| Error::BadGateway)?;
    let expires_at =
        ExpiresAt::parse(required(head, "Racer-Expires-At")?).map_err(|_| Error::BadGateway)?;
    Ok(ObjectMetadata {
        content_type: field(head, "Racer-Content-Type")?
            .map(ContentType::parse)
            .transpose()
            .map_err(|_| Error::BadGateway)?,
        version: ObjectVersion {
            object: object.clone(),
            etag,
        },
        length,
        expires_at,
    })
}

/// Decode one canonical satisfied byte range.
fn content_range(head: &MessageHead) -> Result<(u64, u64, u64)> {
    let range = http1::range::ContentRange::parse_with(required(head, "Content-Range")?, decimal)
        .map_err(|_| Error::BadGateway)?;
    Ok((range.first, range.last, range.total))
}
