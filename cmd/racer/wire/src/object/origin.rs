//! Validate origin response framing, status, metadata, and whole-page boundaries.

use crate::http::{MessageHead, StartLine};
use crate::object::model::{
    ContentType, ExpiresAt, MAX_FIELD_BYTES, ObjectId, ObjectMetadata, ObjectVersion, PageId,
    PageSize, StrongEtag,
};
use crate::object::{Error, Result};

/// Encode one origin request, borrowing sensitive fields only for this head.
pub fn request(
    key: &crate::object::CacheKey,
    method: &str,
    metadata: Option<&[u8]>,
    authorization: Option<&[u8]>,
) -> Result<MessageHead> {
    if !matches!(method, "HEAD" | "GET") {
        return Err(Error::MethodNotAllowed);
    }
    let target = format!("/v1/objects/{}", key.to_hex());
    let mut headers = vec![
        crate::http::Header::new("Host", b"racer"),
        crate::http::Header::new("Content-Length", b"0"),
    ];
    for (name, value) in [
        ("Racer-Metadata", metadata),
        ("Authorization", authorization),
    ] {
        if let Some(bytes) = value {
            opaque(bytes)?;
            headers.push(crate::http::Header::new(name, bytes));
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

/// Request field that limits a pinned page GET to a copy the adapter already
/// holds locally. Its only value is [`SOURCE_LOCAL`]. Absent, the adapter may
/// contact its remote source as usual.
pub const SOURCE_HEADER: &str = "Racer-Source";

/// The [`SOURCE_HEADER`] value of a local-only request.
pub const SOURCE_LOCAL: &[u8] = b"local";

/// Encode one local-only pinned page GET: the adapter must serve `page` only
/// from a copy of the pinned version it already holds, never from its remote
/// source. The response shape is a pinned page GET's; a local miss is 404.
/// Opaque context is forwarded when available, but adapters must not require
/// it, since a node filling a page for a peer may have none.
pub fn local_page_request(
    page: &PageId,
    size: PageSize,
    metadata: Option<&[u8]>,
    authorization: Option<&[u8]>,
) -> Result<MessageHead> {
    let first = page
        .number
        .0
        .checked_mul(size.get())
        .ok_or(Error::InvalidRange)?;
    let last = first
        .checked_add(size.get() - 1)
        .filter(|last| *last <= i64::MAX as u64)
        .ok_or(Error::InvalidRange)?;
    let mut head = request(&page.version.object.key, "GET", metadata, authorization)?;
    head.headers.push(crate::http::Header::new(
        "Range",
        format!("bytes={first}-{last}").as_bytes(),
    ));
    head.headers.push(crate::http::Header::new(
        "If-Match",
        page.version.etag.as_bytes(),
    ));
    head.headers
        .push(crate::http::Header::new(SOURCE_HEADER, SOURCE_LOCAL));
    Ok(head)
}

/// Validate the head of a local-only page response before receiving payload.
/// A well-framed 404 (no local copy) or 412 (the local copy is another
/// version) is a local miss, `Ok(None)`, never evidence about the remote
/// origin. Any other non-success is an error; success must match the pinned
/// page exactly, as for [`validate_page_head`].
pub fn validate_local_page_head(
    head: &MessageHead,
    page: &PageId,
    size: PageSize,
) -> Result<Option<(ObjectMetadata, u64)>> {
    match validate_response(head, false) {
        Err(Error::NotFound | Error::VersionUnavailable) => Ok(None),
        Err(error) => Err(error),
        Ok(_) => validate_page_head(head, page, size).map(Some),
    }
}

/// Validate opaque origin context without interpreting credentials or normalizing bytes.
pub fn opaque(bytes: &[u8]) -> Result<()> {
    if bytes.len() > MAX_FIELD_BYTES {
        return Err(Error::HeaderTooLarge);
    }
    if bytes.is_empty()
        || bytes.first() == Some(&b' ')
        || bytes.last() == Some(&b' ')
        || bytes.iter().any(|b| *b < 0x20 || *b == 0x7f)
    {
        return Err(Error::InvalidRequest);
    }
    Ok(())
}

/// The initial GET selects metadata and page zero atomically at the adapter.
pub fn validate_bootstrap(
    head: &MessageHead,
    object: &ObjectId,
    size: PageSize,
) -> Result<(ObjectMetadata, u64)> {
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
        if first != 0 || length != last + 1 || length != total.min(size.get()) {
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
    size: PageSize,
    received_bytes: usize,
) -> Result<ObjectMetadata> {
    let (metadata, length) = validate_page_head(head, page, size)?;
    if received_bytes as u64 != length {
        return Err(Error::BadGateway);
    }
    Ok(metadata)
}

/// Validate before allocating or receiving any payload.
pub fn validate_page_head(
    head: &MessageHead,
    page: &PageId,
    size: PageSize,
) -> Result<(ObjectMetadata, u64)> {
    let (status, length) = validate_response(head, true)?;
    if status != 206 || required(head, "Content-Type")? != b"application/octet-stream" {
        return Err(Error::BadGateway);
    }
    let (first, last, total) = content_range(head)?;
    let expected_first = page
        .number
        .0
        .checked_mul(size.get())
        .ok_or(Error::InvalidRange)?;
    if first != expected_first
        || length != last - first + 1
        || length != (total - first).min(size.get())
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
            || name.eq_ignore_ascii_case("Content-Length")
            || name.eq_ignore_ascii_case("Content-Range")
        {
            value
        } else {
            crate::http::trim_ows(value)
        }
    }))
}

/// Read a required, unique response field.
fn required<'a>(head: &'a MessageHead, name: &str) -> Result<&'a [u8]> {
    field(head, name)?.ok_or(Error::BadGateway)
}

/// Canonical decimal, bounded by the SDK's nonnegative signed-64-bit domain.
fn decimal(bytes: &[u8]) -> Result<u64> {
    crate::object::model::parse_decimal(bytes).map_err(|_| Error::BadGateway)
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
    let range =
        crate::http::range::ContentRange::parse_with(required(head, "Content-Range")?, decimal)
            .map_err(|_| Error::BadGateway)?;
    Ok((range.first, range.last, range.total))
}
