//! Client write request semantics and response heads, independent of I/O.
//!
//! The write value types ([`UploadMode`], [`Durability`], [`UploadState`]) live
//! here because racer-core depends on racer-wire; `racer_core::write`
//! re-exports them.
//!
//! [`is_write`] routes a head here before [`super::client::RequestParser`],
//! which keeps rejecting write methods. [`parse_write`] validates a write head
//! with the same style as the read parser: duplicate singleton fields,
//! decoded-size limit, `Host: racer`, and opaque context validation.

use crate::http::{Header, MessageHead, StartLine, is_token, trim_ows};
use crate::object::client::{Destination, MAX_HEAD_BYTES};
use crate::object::model::{
    CacheKey, ContentType, MAX_FIELD_BYTES, MAX_WIRE_INTEGER, ObjectMetadata, PageSize, StrongEtag,
    parse_decimal,
};
use std::collections::HashSet;

/// When a written object is uploaded to the origin, from `Racer-Upload` or
/// the cache's `spec.writes.uploadMode` default. `control` re-exports it.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Hash)]
pub enum UploadMode {
    #[default]
    Auto,
    Manual,
    Never,
}

impl UploadMode {
    /// The header token.
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Auto => "auto",
            Self::Manual => "manual",
            Self::Never => "never",
        }
    }

    /// Parse the exact lowercase header token.
    pub fn parse(value: &[u8]) -> Option<Self> {
        match value {
            b"auto" => Some(Self::Auto),
            b"manual" => Some(Self::Manual),
            b"never" => Some(Self::Never),
            _ => None,
        }
    }

    /// Stable one-byte code used by binary encodings.
    pub fn code(self) -> u8 {
        match self {
            Self::Auto => 0,
            Self::Manual => 1,
            Self::Never => 2,
        }
    }

    /// Inverse of [`Self::code`].
    pub fn from_code(code: u8) -> Option<Self> {
        match code {
            0 => Some(Self::Auto),
            1 => Some(Self::Manual),
            2 => Some(Self::Never),
            _ => None,
        }
    }
}

/// Smallest and largest `replicas=N` value.
pub const MIN_REPLICAS: u8 = 1;
/// Matches `topology::placement::REPLICAS`.
pub const MAX_REPLICAS: u8 = 3;

/// When a write is acknowledged, from `Racer-Durability`.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Hash)]
pub enum Durability {
    /// After this many durable owner copies, `1..=MAX_REPLICAS`.
    Replicas(u8),
    /// After the origin commit.
    Origin,
}

impl Durability {
    /// Parse `replicas=1..=3` or `origin`.
    pub fn parse(value: &[u8]) -> Option<Self> {
        match value {
            b"origin" => Some(Self::Origin),
            b"replicas=1" => Some(Self::Replicas(1)),
            b"replicas=2" => Some(Self::Replicas(2)),
            b"replicas=3" => Some(Self::Replicas(3)),
            _ => None,
        }
    }

    /// The header value.
    pub fn to_header(self) -> String {
        match self {
            Self::Replicas(n) => format!("replicas={n}"),
            Self::Origin => "origin".into(),
        }
    }
}

/// Upload progress of a written object, reported as `Racer-Upload-State`.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Hash)]
pub enum UploadState {
    Pending,
    Manual,
    Uploading,
    Committed,
    Failed,
    Never,
}

impl UploadState {
    /// The header token.
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Pending => "pending",
            Self::Manual => "manual",
            Self::Uploading => "uploading",
            Self::Committed => "committed",
            Self::Failed => "failed",
            Self::Never => "never",
        }
    }

    /// Parse the exact header token.
    pub fn parse(value: &[u8]) -> Option<Self> {
        Some(match value {
            b"pending" => Self::Pending,
            b"manual" => Self::Manual,
            b"uploading" => Self::Uploading,
            b"committed" => Self::Committed,
            b"failed" => Self::Failed,
            b"never" => Self::Never,
            _ => return None,
        })
    }

    /// Stable one-byte code used by binary encodings.
    pub fn code(self) -> u8 {
        match self {
            Self::Pending => 0,
            Self::Manual => 1,
            Self::Uploading => 2,
            Self::Committed => 3,
            Self::Failed => 4,
            Self::Never => 5,
        }
    }

    /// Inverse of [`Self::code`].
    pub fn from_code(code: u8) -> Option<Self> {
        Some(match code {
            0 => Self::Pending,
            1 => Self::Manual,
            2 => Self::Uploading,
            3 => Self::Committed,
            4 => Self::Failed,
            5 => Self::Never,
            _ => return None,
        })
    }

    /// The state a freshly committed object starts in.
    pub fn initial(upload: UploadMode) -> Self {
        match upload {
            UploadMode::Auto => Self::Pending,
            UploadMode::Manual => Self::Manual,
            UploadMode::Never => Self::Never,
        }
    }
}

#[cfg(test)]
mod value_tests {
    use super::*;

    #[test]
    fn tokens_round_trip() {
        for mode in [UploadMode::Auto, UploadMode::Manual, UploadMode::Never] {
            assert_eq!(UploadMode::parse(mode.as_str().as_bytes()), Some(mode));
            assert_eq!(UploadMode::from_code(mode.code()), Some(mode));
        }
        assert_eq!(UploadMode::parse(b"Auto"), None);
        assert_eq!(UploadMode::from_code(3), None);
        for state in [
            UploadState::Pending,
            UploadState::Manual,
            UploadState::Uploading,
            UploadState::Committed,
            UploadState::Failed,
            UploadState::Never,
        ] {
            assert_eq!(UploadState::parse(state.as_str().as_bytes()), Some(state));
            assert_eq!(UploadState::from_code(state.code()), Some(state));
        }
        assert_eq!(UploadState::from_code(6), None);
        for d in [
            Durability::Replicas(1),
            Durability::Replicas(2),
            Durability::Replicas(3),
            Durability::Origin,
        ] {
            assert_eq!(Durability::parse(d.to_header().as_bytes()), Some(d));
        }
        for bad in [
            &b"replicas=0"[..],
            b"replicas=4",
            b"replicas=02",
            b"",
            b"Origin",
        ] {
            assert_eq!(Durability::parse(bad), None);
        }
    }
}

/// Largest `Racer-Hold-Seconds`, the 30-day hard-hold limit.
pub const MAX_HOLD_SECONDS: u64 = 2_592_000;
/// Azure Blob block limit; writes use one block per page.
pub const MAX_WRITE_PAGES: u64 = 50_000;

/// Largest object a cache with this page size accepts.
pub fn max_object_bytes(page_size: PageSize) -> u64 {
    MAX_WRITE_PAGES * page_size.get()
}

/// `Racer-Write-Error` diagnostic tokens.
pub mod error_token {
    pub const DIRTY_CAP: &str = "dirty-cap";
    pub const OWNERS: &str = "owners";
    pub const COORDINATOR: &str = "coordinator";
    pub const LEASE: &str = "lease";
    pub const UPLOAD: &str = "upload";
    pub const UPLOADED: &str = "uploaded";
    pub const NOT_MANUAL: &str = "not-manual";
    pub const BUCKET: &str = "bucket";
}

/// A write operation.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum WriteKind {
    /// `PUT /v1/objects/<key>` with a body of exactly `content_length` bytes.
    Put { content_length: u64 },
    /// `POST /v1/objects/<key>/host-write`; the object is the descriptor
    /// window `source`, and `source.length` is the object length.
    HostWrite { source: Destination },
    /// `POST /v1/objects/<key>/persist`.
    Persist,
    /// `DELETE /v1/objects/<key>`.
    Delete,
}

impl WriteKind {
    /// Whether this request creates an object.
    pub fn creates(&self) -> bool {
        matches!(self, Self::Put { .. } | Self::HostWrite { .. })
    }

    /// Object length for creating requests.
    pub fn object_length(&self) -> Option<u64> {
        match self {
            Self::Put { content_length } => Some(*content_length),
            Self::HostWrite { source } => Some(source.length),
            _ => None,
        }
    }
}

/// Validated write request borrowing opaque fields. Deliberately neither
/// Clone nor Debug, to avoid duplicating or logging secrets.
pub struct WriteRequest<'a> {
    pub kind: WriteKind,

    pub key: CacheKey,

    pub metadata: Option<&'a [u8]>,

    pub authorization: Option<&'a [u8]>,

    pub content_type: Option<ContentType>,

    /// `None` selects the cache default.
    pub durability: Option<Durability>,

    /// `None` selects the cache default.
    pub upload: Option<UploadMode>,

    pub hold_seconds: Option<u64>,
}

/// Write parse failure. The read-path [`super::Error`] has no variants for
/// `411`, `413`, or `428`, so writes carry their own.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum WriteError {
    /// A shared object protocol failure (`400`, `405`, `431`, ...).
    Object(super::Error),
    /// `PUT` without `Content-Length`: `411`.
    LengthRequired,
    /// Object above [`max_object_bytes`]: `413`.
    PayloadTooLarge,
    /// Creating request without `If-None-Match: *`: `428`.
    PreconditionRequired,
}

impl WriteError {
    /// HTTP status for the variants specific to writes; `None` for
    /// [`WriteError::Object`], which callers map like read errors.
    pub fn status(&self) -> Option<u16> {
        match self {
            Self::Object(_) => None,
            Self::LengthRequired => Some(411),
            Self::PayloadTooLarge => Some(413),
            Self::PreconditionRequired => Some(428),
        }
    }
}

impl From<super::Error> for WriteError {
    fn from(error: super::Error) -> Self {
        Self::Object(error)
    }
}

impl std::fmt::Display for WriteError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{self:?}")
    }
}

impl std::error::Error for WriteError {}

/// Result of parsing a write head.
pub type WriteResult<T> = std::result::Result<T, WriteError>;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Route {
    Put,
    Delete,
    HostWrite,
    Persist,
}

/// Classify the start line without reading fields.
fn route(head: &MessageHead) -> Option<(Route, &str)> {
    let StartLine::Request { method, target } = &head.start else {
        return None;
    };
    let path = target.strip_prefix("/v1/objects/")?;
    let (key, route) = match (method.as_str(), path.split_once('/')) {
        ("PUT", None) => (path, Route::Put),
        ("DELETE", None) => (path, Route::Delete),
        ("POST", Some((key, "host-write"))) => (key, Route::HostWrite),
        ("POST", Some((key, "persist"))) => (key, Route::Persist),
        _ => return None,
    };
    (key.len() == 64 && key.bytes().all(|b| b.is_ascii_hexdigit())).then_some((route, key))
}

/// Whether this head is a write: `PUT` or `DELETE` on `/v1/objects/<64 hex>`,
/// or `POST` on `/v1/objects/<64 hex>/host-write` or `/persist`.
pub fn is_write(head: &MessageHead) -> bool {
    route(head).is_some()
}

/// Whether this head targets a write-only subresource,
/// `/v1/objects/<64 hex>/host-write` or `/persist`, with any method. On a
/// cache with writes, a method other than `POST` there is `405`, not the
/// read parser's `400` for an unknown path.
pub fn is_write_subresource(head: &MessageHead) -> bool {
    let StartLine::Request { target, .. } = &head.start else {
        return false;
    };
    let Some((key, suffix)) = target
        .strip_prefix("/v1/objects/")
        .and_then(|path| path.split_once('/'))
    else {
        return false;
    };
    matches!(suffix, "host-write" | "persist")
        && key.len() == 64
        && key.bytes().all(|b| b.is_ascii_hexdigit())
}

/// Validate a write head. `page_size` is the cluster page size and bounds the
/// object length at [`max_object_bytes`]. Upload-mode defaults are not known
/// here: callers resolve them, then enforce that `Racer-Metadata` is present
/// for modes other than `never` and that the cache accepts the mode.
pub fn parse_write<'a>(
    head: &'a MessageHead,
    header_limit: usize,
    page_size: PageSize,
) -> WriteResult<WriteRequest<'a>> {
    let header_limit = header_limit.min(MAX_HEAD_BYTES);
    let Some((route, key_text)) = route(head) else {
        return Err(super::Error::MethodNotAllowed.into());
    };
    let StartLine::Request { method, target } = &head.start else {
        unreachable!("route accepted a request line");
    };
    let creates = matches!(route, Route::Put | Route::HostWrite);
    let invalid = || WriteError::Object(super::Error::InvalidRequest);
    let mut decoded_bytes = method.len().saturating_add(target.len());
    let mut seen = HashSet::new();
    let mut host = None;
    let mut content_length = None;
    let mut if_none_match = None;
    let mut metadata = None;
    let mut authorization = None;
    let mut content_type = None;
    let mut durability = None;
    let mut upload = None;
    let mut hold_seconds = None;
    let mut source_offset = None;
    let mut source_length = None;
    for header in &head.headers {
        decoded_bytes = decoded_bytes
            .saturating_add(header.name.len())
            .saturating_add(header.value.len());
        if decoded_bytes > header_limit {
            return Err(super::Error::HeaderTooLarge.into());
        }
        if header.name.is_empty()
            || !header.name.bytes().all(is_token)
            || header
                .value
                .iter()
                .any(|&b| b == 0x7f || (b < 0x20 && b != b'\t'))
        {
            return Err(invalid());
        }
        let name = header.name.to_ascii_lowercase();
        let value = header.value.as_slice();
        if matches!(
            name.as_str(),
            "host"
                | "content-length"
                | "content-type"
                | "if-none-match"
                | "racer-metadata"
                | "authorization"
                | "racer-content-type"
                | "racer-durability"
                | "racer-upload"
                | "racer-hold-seconds"
                | "racer-source-offset"
                | "racer-source-length"
        ) && !seen.insert(name.clone())
        {
            return Err(invalid());
        }
        match name.as_str() {
            "host" => host = Some(trim_ows(value) == b"racer"),
            "content-length" => content_length = Some(parse_decimal(value)?),
            "transfer-encoding"
            | "content-encoding"
            | "content-range"
            | "trailer"
            | "upgrade"
            | "expect"
            | "etag"
            | "if-match"
            | "if-modified-since"
            | "if-unmodified-since"
            | "if-range"
            | "range"
            | "racer-expires-at" => return Err(invalid()),
            "connection"
                if value
                    .split(|&b| b == b',')
                    .any(|token| trim_ows(token).eq_ignore_ascii_case(b"upgrade")) =>
            {
                return Err(invalid());
            }
            "racer-metadata" => {
                validate_opaque(value)?;
                metadata = Some(value);
            }
            "authorization" => {
                validate_opaque(value)?;
                authorization = Some(value);
            }
            "if-none-match" if creates => if_none_match = Some(trim_ows(value) == b"*"),
            "racer-content-type" if creates => {
                content_type = Some(ContentType::parse(trim_ows(value))?);
            }
            "racer-durability" if creates => {
                durability = Some(Durability::parse(trim_ows(value)).ok_or_else(invalid)?);
            }
            "racer-upload" if creates => {
                upload = Some(UploadMode::parse(trim_ows(value)).ok_or_else(invalid)?);
            }
            "racer-hold-seconds" if creates => {
                let seconds = parse_decimal(value)?;
                if !(1..=MAX_HOLD_SECONDS).contains(&seconds) {
                    return Err(invalid());
                }
                hold_seconds = Some(seconds);
            }
            "racer-source-offset" if route == Route::HostWrite => {
                source_offset = Some(parse_decimal(value)?);
            }
            "racer-source-length" if route == Route::HostWrite => {
                source_length = Some(parse_decimal(value)?);
            }
            // Creation fields on a non-creating request are a client bug.
            "if-none-match"
            | "racer-content-type"
            | "racer-durability"
            | "racer-upload"
            | "racer-hold-seconds"
            | "racer-source-offset"
            | "racer-source-length" => {
                return Err(invalid());
            }
            _ => {}
        }
    }
    if host != Some(true) {
        return Err(invalid());
    }
    let key = CacheKey::parse_hex(key_text.as_bytes())?;
    let limit = max_object_bytes(page_size);
    let kind = match route {
        Route::Put => {
            let length = content_length.ok_or(WriteError::LengthRequired)?;
            if length == 0 {
                return Err(invalid());
            }
            if length > limit {
                return Err(WriteError::PayloadTooLarge);
            }
            WriteKind::Put {
                content_length: length,
            }
        }
        _ if content_length.is_some_and(|length| length != 0) => return Err(invalid()),
        Route::HostWrite => {
            let source = Destination {
                offset: source_offset.ok_or_else(invalid)?,
                length: source_length
                    .filter(|&length| length != 0)
                    .ok_or_else(invalid)?,
            };
            source
                .offset
                .checked_add(source.length)
                .filter(|&end| end <= MAX_WIRE_INTEGER)
                .ok_or_else(invalid)?;
            if source.length > limit {
                return Err(WriteError::PayloadTooLarge);
            }
            WriteKind::HostWrite { source }
        }
        Route::Persist => WriteKind::Persist,
        Route::Delete => WriteKind::Delete,
    };
    if creates && if_none_match != Some(true) {
        return Err(WriteError::PreconditionRequired);
    }
    if durability == Some(Durability::Origin)
        && matches!(upload, Some(UploadMode::Never | UploadMode::Manual))
    {
        return Err(invalid());
    }
    Ok(WriteRequest {
        kind,
        key,
        metadata,
        authorization,
        content_type,
        durability,
        upload,
        hold_seconds,
    })
}

/// Same rules as the read parser's opaque context fields.
fn validate_opaque(value: &[u8]) -> super::Result<()> {
    if value.len() > MAX_FIELD_BYTES {
        return Err(super::Error::HeaderTooLarge);
    }
    if value.is_empty()
        || value.first() == Some(&b' ')
        || value.last() == Some(&b' ')
        || value.iter().any(|&b| b < 0x20 || b == 0x7f)
    {
        return Err(super::Error::InvalidRequest);
    }
    Ok(())
}

/// `201 Created` for a durable, visible write.
pub fn write_created_head(etag: &StrongEtag, state: UploadState) -> MessageHead {
    MessageHead {
        start: StartLine::Response { status: 201 },
        headers: vec![
            Header::new("ETag", etag.as_bytes()),
            Header::new("Racer-Upload-State", state.as_str()),
            Header::new("Content-Length", "0"),
        ],
    }
}

/// `204 No Content` for an accepted `DELETE` (no state) or `persist`.
pub fn write_no_content_head(state: Option<UploadState>) -> MessageHead {
    let mut headers = Vec::new();
    if let Some(state) = state {
        headers.push(Header::new("Racer-Upload-State", state.as_str()));
    }
    // 204 carries no Content-Length; the response has no body by definition.
    MessageHead {
        start: StartLine::Response { status: 204 },
        headers,
    }
}

/// Zero-body write error. A `405` carries `Allow: <allow>`, normally
/// [`super::client::allowed_methods`] for the request target with writes on.
/// `error` is a `Racer-Write-Error` token from [`error_token`].
pub fn write_error_head(
    status: u16,
    allow: &str,
    error: Option<&str>,
    retry_after: Option<u32>,
) -> MessageHead {
    let mut headers = vec![Header::new("Content-Length", "0")];
    if status == 405 {
        headers.push(Header::new("Allow", allow));
    }
    if let Some(error) = error {
        headers.push(Header::new("Racer-Write-Error", error));
    }
    if let Some(seconds) = retry_after {
        headers.push(Header::new("Retry-After", seconds.to_string()));
    }
    MessageHead {
        start: StartLine::Response { status },
        headers,
    }
}

/// HEAD success for a possibly written object: [`super::client::success_head`]
/// plus `Racer-Upload-State` before `Content-Length` when `state` is set.
pub fn success_head_with_upload(
    metadata: &ObjectMetadata,
    state: Option<UploadState>,
) -> super::Result<MessageHead> {
    let mut head = super::client::success_head(metadata, None)?;
    if let Some(state) = state {
        let at = head
            .headers
            .iter()
            .position(|h| h.name.eq_ignore_ascii_case("Content-Length"))
            .unwrap_or(head.headers.len());
        head.headers
            .insert(at, Header::new("Racer-Upload-State", state.as_str()));
    }
    Ok(head)
}

#[cfg(test)]
mod tests;
