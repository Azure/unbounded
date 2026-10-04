//! Per-cache local adapter operations requiring validated candidate authority.
//! Connect to the adapter-owned /run/racer/<cache name>/origin/socket.
//!
//! Credentials are only origin-fetch context, never Racer authorization. Do not
//! persist headers or retain them in pooled connections after an operation ends.
use crate::admission::AdmissionPolicy;
use crate::control::SnapshotStore;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::http::ConnectionLease;
use crate::http::Endpoint;
use crate::http::HttpIo;
use crate::http::HttpPool;
use crate::memory::BufferPool;
use crate::memory::PlaintextBuffer;
use crate::model::ExpiresAt;
use crate::model::MetadataSelector;
use crate::model::ObjectId;
use crate::model::ObjectMetadata;
use crate::model::ObjectVersion;
use crate::security::OriginContext;
use crate::model::PAGE_BYTES;
use crate::model::PageId;
use crate::model::PageNumber;
use crate::admission::ResourceClass;
use crate::model::StrongEtag;
use crate::read::candidates::OriginAuthority;
use crate::runtime::RequestScope;
use http1::Header;
use http1::MessageHead;
use http1::StartLine;
use racer_control_wire::CacheDefinition;
use racer_control_wire::canonical_socket_paths;
use std::path::Path;
use std::path::PathBuf;
use std::rc::Rc;
use uring_runtime::reactor::Completion;
use uring_runtime::reactor::IoBuffer;
/// OriginClient is the shipping adapter implementation. Scripted implementations
/// remain for poll-exact cancellation, wake ordering, and reservation-fence tests;
/// ordinary adapter scenarios should use the shared test_support UDS fixture.
pub trait Origin {
    /// Consume the read owner's reclaimed and admitted bootstrap plaintext budget.
    fn bootstrap_reserved<'a>(
        &'a self,
        authority: &'a OriginAuthority,
        context: &'a OriginContext,
        reservation: flow_control::Charge<AdmissionPolicy>,
        scope: &'a RequestScope,
    ) -> Operation<'a, MetadataReply>;
    /// Consume the fill's atomically admitted plaintext budget.
    fn page_reserved<'a>(
        &'a self,
        authority: &'a OriginAuthority,
        context: &'a OriginContext,
        page: &'a PageId,
        reservation: flow_control::Charge<AdmissionPolicy>,
        scope: &'a RequestScope,
    ) -> Operation<'a, OriginPage>;
    fn metadata<'a>(
        &'a self,
        authority: &'a OriginAuthority,
        context: &'a OriginContext,
        selector: MetadataSelector,
        scope: &'a RequestScope,
    ) -> Operation<'a, MetadataReply>;
}
pub struct OriginClient {
    health: crate::topology::LinkHealth,
    snapshots: Rc<SnapshotStore>,
    pool: Rc<HttpPool>,
    io: Rc<HttpIo>,
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    buffers: BufferPool,
    socket_root: PathBuf,
}
impl OriginClient {
    /// Assemble the adapter with its shared plaintext owners and socket root.
    /// Canonical published endpoints map to `<root>/<cache name>/origin/socket`.
    /// The deployment root must be an absolute, lexically canonical directory path
    /// without NUL, empty, `.` or `..` components. This performs no filesystem I/O;
    /// the caller provisions and owns the directories (including any symlink policy).
    /// The complete resolved endpoint is checked against Linux's 107-byte UDS cap.
    pub fn new(
        snapshots: Rc<SnapshotStore>,
        pool: Rc<HttpPool>,
        io: Rc<HttpIo>,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        buffers: BufferPool,
        root: impl Into<PathBuf>,
    ) -> Result<Self> {
        let root = root.into();
        let bytes = root.as_os_str().as_encoded_bytes();
        if !root.is_absolute()
            || bytes.contains(&0)
            || (bytes != b"/"
                && bytes[1..]
                    .split(|b| *b == b'/')
                    .any(|part| part.is_empty() || part == b"." || part == b".."))
            || root.join("a/origin/socket").as_os_str().len() > 107
        {
            return Err(Error::InvalidConfiguration);
        }
        Ok(Self {
            health: crate::topology::LinkHealth::new(64),
            snapshots,
            pool,
            io,
            admission,
            buffers,
            socket_root: root,
        })
    }

    async fn bootstrap_reserved_at(
        &self,
        endpoint: &Endpoint,
        context: &OriginContext,
        reservation: flow_control::Charge<AdmissionPolicy>,
        scope: &RequestScope,
    ) -> Result<MetadataReply> {
        scope.check()?;
        let (admission, buffers) = (&self.admission, &self.buffers);
        reservation.validate(ResourceClass::Plaintext, PAGE_BYTES as usize)?;
        if !admission.owns(&reservation) || reservation.key() != Some(&context.object.cache) {
            return Err(Error::InvalidConfiguration);
        }
        let mut head = request(context, "GET")?;
        head.headers.push(header("Range", b"bytes=0-16777215"));
        let connection = self
            .pool
            .checkout_wait(endpoint, scope)
            .await
            .map_err(response_error)?;
        let sent = self
            .io
            .send_head(connection, head, scope)
            .await
            .map_err(response_error)?;
        let mut response = self
            .io
            .receive_head_limited(sent.connection, scope, 32 * 1024)
            .await
            .map_err(response_error)?;
        let (metadata, length) = validate_bootstrap(&response.value, &context.object)?;
        if length == 0 {
            scope.check()?;
            response
                .connection
                .finish_exchange()
                .map_err(|_| Error::BadGateway)?;
            return Ok(MetadataReply {
                metadata,
                page_zero: None,
            });
        }
        let buffer = buffers.plaintext(reservation, length as usize)?;
        let mut body = self.read_page(response.connection, buffer, scope).await?;
        let page = PageId {
            version: metadata.version.clone(),
            number: PageNumber(0),
        };
        validate_page(&response.value, &page, body.bytes)?;
        scope.check()?;
        body.lease
            .finish_exchange()
            .map_err(|_| Error::BadGateway)?;
        Ok(MetadataReply {
            page_zero: Some(OriginPage {
                metadata: metadata.clone(),
                plaintext: body.buffer,
            }),
            metadata,
        })
    }

    fn endpoint(&self, context: &OriginContext) -> Result<Endpoint> {
        let snapshot = self.snapshots.current()?;
        let cache = snapshot
            .caches
            .iter()
            .find(|cache| cache.id == context.object.cache)
            .ok_or(Error::Unavailable)?;
        Ok(Endpoint::Origin {
            cache: cache.id.clone(),
            path: resolve_socket(&self.socket_root, cache)?,
        })
    }

    async fn metadata_at(
        &self,
        endpoint: &Endpoint,
        context: &OriginContext,
        selector: MetadataSelector,
        scope: &RequestScope,
    ) -> Result<MetadataReply> {
        scope.check()?;
        let mut head = request(context, "HEAD")?;
        if let MetadataSelector::Pinned(etag) = &selector {
            head.headers.push(header("If-Match", etag.as_bytes()));
        }
        // HEAD has no plaintext reservation and uses metadata connection admission.
        // Where the endpoint cap permits, GETs leave a slot available for HEAD.
        let connection = self
            .pool
            .checkout_metadata(endpoint, scope)
            .await
            .map_err(response_error)?;
        let sent = self
            .io
            .send_head(connection, head, scope)
            .await
            .map_err(response_error)?;
        let mut response = self
            .io
            .receive_head_limited(sent.connection, scope, 32 * 1024)
            .await
            .map_err(response_error)?;
        // A pinned 404 is a broken origin contract, not permission to refresh.
        validate_response(
            &response.value,
            matches!(selector, MetadataSelector::Pinned(_)),
        )?;
        let metadata = validate_metadata(&response.value, &context.object)?;
        if let MetadataSelector::Pinned(etag) = selector {
            if metadata.version.etag != etag {
                return Err(Error::BadGateway);
            }
        }
        scope.check()?;
        response
            .connection
            .finish_exchange()
            .map_err(|_| Error::BadGateway)?;
        Ok(MetadataReply {
            metadata,
            page_zero: None,
        })
    }

    async fn page_reserved_at(
        &self,
        endpoint: &Endpoint,
        context: &OriginContext,
        page: &PageId,
        reservation: flow_control::Charge<AdmissionPolicy>,
        scope: &RequestScope,
    ) -> Result<OriginPage> {
        scope.check()?;
        if page.version.object != context.object {
            return Err(Error::Unauthorized);
        }
        let first = page
            .number
            .0
            .checked_mul(PAGE_BYTES)
            .ok_or(Error::InvalidRange)?;
        let last = first
            .checked_add(PAGE_BYTES - 1)
            .filter(|last| *last <= i64::MAX as u64)
            .ok_or(Error::InvalidRange)?;
        let mut head = request(context, "GET")?;
        head.headers
            .push(header("Range", format!("bytes={first}-{last}").as_bytes()));
        head.headers
            .push(header("If-Match", page.version.etag.as_bytes()));
        let (admission, buffers) = (&self.admission, &self.buffers);
        reservation.validate(ResourceClass::Plaintext, PAGE_BYTES as usize)?;
        if !admission.owns(&reservation) || reservation.key() != Some(&context.object.cache) {
            return Err(Error::InvalidConfiguration);
        }
        let connection = self
            .pool
            .checkout_wait(endpoint, scope)
            .await
            .map_err(response_error)?;
        let sent = self
            .io
            .send_head(connection, head, scope)
            .await
            .map_err(response_error)?;
        let response = self
            .io
            .receive_head_limited(sent.connection, scope, 32 * 1024)
            .await
            .map_err(response_error)?;
        let (_, length) = validate_page_head(&response.value, page)?;
        let buffer = buffers.plaintext(reservation, length as usize)?;
        let mut body = self.read_page(response.connection, buffer, scope).await?;
        let metadata = validate_page(&response.value, page, body.bytes)?;
        scope.check()?;
        body.lease
            .finish_exchange()
            .map_err(|_| Error::BadGateway)?;
        Ok(OriginPage {
            metadata,
            plaintext: body.buffer,
        })
    }

    async fn read_page(
        &self,
        mut connection: ConnectionLease,
        mut buffer: PlaintextBuffer,
        scope: &RequestScope,
    ) -> Result<Completion<PlaintextBuffer, ConnectionLease>> {
        let length = buffer.bytes()?.len();
        let mut offset = 0;
        while offset < length {
            let completion = self
                .io
                .read_body_range(connection, buffer, offset..length, scope)
                .await
                .map_err(response_error)?;
            if completion.bytes == 0 || completion.bytes > length - offset {
                return Err(Error::BadGateway);
            }
            offset += completion.bytes;
            connection = completion.lease;
            buffer = completion.buffer;
        }
        Ok(Completion {
            buffer,
            bytes: offset,
            lease: connection,
        })
    }
}

fn resolve_socket(root: &Path, cache: &CacheDefinition) -> Result<PathBuf> {
    // Keep publications canonical even when the local deployment root differs.
    // Derive the suffix from the validated name, never an arbitrary supplied path.
    let (client, origin) = canonical_socket_paths(&cache.name)?;
    if cache.client_socket.as_os_str() != client.as_os_str()
        || cache.origin_socket.as_os_str() != origin.as_os_str()
    {
        return Err(Error::InvalidRequest);
    }
    let endpoint = root.join(&cache.name).join("origin/socket");
    if endpoint.as_os_str().len() > 107 {
        return Err(Error::InvalidConfiguration);
    }
    Ok(endpoint)
}
impl Origin for OriginClient {
    fn bootstrap_reserved<'a>(
        &'a self,
        authority: &'a OriginAuthority,
        context: &'a OriginContext,
        reservation: flow_control::Charge<AdmissionPolicy>,
        scope: &'a RequestScope,
    ) -> Operation<'a, MetadataReply> {
        Box::pin(async move {
            scope.check()?;
            authority.validate(&context.object, PageNumber(0))?;
            let endpoint = self.endpoint(context)?;
            self.health
                .run(
                    &racer_control_wire::NodeId(format!("{endpoint:?}")),
                    self.bootstrap_reserved_at(&endpoint, context, reservation, scope),
                )
                .await
        })
    }
    fn page_reserved<'a>(
        &'a self,
        authority: &'a OriginAuthority,
        context: &'a OriginContext,
        page: &'a PageId,
        reservation: flow_control::Charge<AdmissionPolicy>,
        scope: &'a RequestScope,
    ) -> Operation<'a, OriginPage> {
        Box::pin(async move {
            scope.check()?;
            if context.object != page.version.object {
                return Err(Error::Unauthorized);
            }
            authority.validate(&page.version.object, page.number)?;
            let endpoint = self.endpoint(context)?;
            self.health
                .run(
                    &racer_control_wire::NodeId(format!("{endpoint:?}")),
                    self.page_reserved_at(&endpoint, context, page, reservation, scope),
                )
                .await
        })
    }
    fn metadata<'a>(
        &'a self,
        authority: &'a OriginAuthority,
        context: &'a OriginContext,
        selector: MetadataSelector,
        scope: &'a RequestScope,
    ) -> Operation<'a, MetadataReply> {
        Box::pin(async move {
            scope.check()?;
            authority.validate(&context.object, PageNumber(0))?;
            let endpoint = self.endpoint(context)?;
            self.health
                .run(
                    &racer_control_wire::NodeId(format!("{endpoint:?}")),
                    self.metadata_at(&endpoint, context, selector, scope),
                )
                .await
        })
    }
}

fn header(name: &str, value: &[u8]) -> Header {
    Header {
        name: name.to_owned(),
        value: value.to_vec(),
    }
}

fn response_error(error: Error) -> Error {
    match error {
        Error::InvalidRequest | Error::HeaderTooLarge | Error::CorruptRecord | Error::Io => {
            Error::BadGateway
        }
        other => other,
    }
}

/// Context is copied only into this operation's outbound HTTP head, never the pool.
fn request(context: &OriginContext, method: &str) -> Result<MessageHead> {
    let target = format!("/v1/objects/{}", context.object.key.to_hex());
    let mut headers = vec![header("Host", b"racer"), header("Content-Length", b"0")];
    if let Some(metadata) = &context.metadata {
        let bytes = metadata.as_header();
        opaque(bytes)?;
        headers.push(header("Racer-Metadata", bytes));
    }
    if let Some(authorization) = &context.authorization {
        let bytes = authorization.expose_for_origin();
        opaque(bytes)?;
        headers.push(header("Authorization", bytes));
    }
    Ok(MessageHead {
        start: StartLine::Request {
            method: method.to_owned(),
            target,
        },
        headers,
    })
}

fn opaque(bytes: &[u8]) -> Result<()> {
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

/// HEAD/initial-GET validation; empty objects produce metadata without a page.
pub struct MetadataReply {
    pub metadata: ObjectMetadata,
    pub page_zero: Option<OriginPage>,
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
pub fn validate_metadata(head: &MessageHead, object: &ObjectId) -> Result<ObjectMetadata> {
    let (status, length) = validate_response(head, false)?;
    if status != 200 {
        return Err(Error::BadGateway);
    }
    absent(head, &["Content-Range"])?;
    metadata(head, object, length)
}

/// Conditional full-page GET validation before authenticated publication.
pub struct OriginPage {
    pub metadata: ObjectMetadata,
    pub plaintext: PlaintextBuffer,
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
fn validate_page_head(head: &MessageHead, page: &PageId) -> Result<(ObjectMetadata, u64)> {
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

fn field<'a>(head: &'a MessageHead, name: &str) -> Result<Option<&'a [u8]>> {
    let value = head.unique(name).map_err(|_| Error::BadGateway)?;
    // Preserve canonical numeric fields before decimal/range validation, just as
    // opaque context preserves exact bytes. The codec removed only separator SP;
    // trimming here would accept forbidden padding in expiry, lengths, or ranges.
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

fn required<'a>(head: &'a MessageHead, name: &str) -> Result<&'a [u8]> {
    field(head, name)?.ok_or(Error::BadGateway)
}

/// Canonical decimal, bounded by the SDK's nonnegative signed-64-bit domain.
fn decimal(bytes: &[u8]) -> Result<u64> {
    crate::model::parse_decimal(bytes).map_err(|_| Error::BadGateway)
}

fn absent(head: &MessageHead, names: &[&str]) -> Result<()> {
    for name in names {
        if field(head, name)?.is_some() {
            return Err(Error::BadGateway);
        }
    }
    Ok(())
}

/// Validate framing before interpreting status; malformed errors are never miss proof.
fn validate_response(head: &MessageHead, pinned: bool) -> Result<(u16, u64)> {
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
    if let Some(value) = field(head, "Racer-Content-Type")? {
        crate::model::ContentType::parse(value).map_err(|_| Error::BadGateway)?;
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

fn metadata(head: &MessageHead, object: &ObjectId, length: u64) -> Result<ObjectMetadata> {
    let etag = StrongEtag::parse(required(head, "ETag")?).map_err(|_| Error::BadGateway)?;
    let expires_at =
        ExpiresAt::parse(required(head, "Racer-Expires-At")?).map_err(|_| Error::BadGateway)?;
    Ok(ObjectMetadata {
        content_type: field(head, "Racer-Content-Type")?
            .map(crate::model::ContentType::parse)
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

fn content_range(head: &MessageHead) -> Result<(u64, u64, u64)> {
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
mod tests;
