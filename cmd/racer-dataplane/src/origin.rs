//! Per-cache local adapter operations requiring validated candidate authority.
//! Connect to the adapter-owned /run/racer/<cache name>/origin/socket.
//!
//! Credentials are only origin-fetch context, never Racer authorization. Do not
//! persist headers or retain them in pooled connections after an operation ends.
use crate::{
    control::state::{CacheDefinition, SnapshotStore, canonical_socket_paths},
    error::{Error, Operation, Result},
    http::{
        Header, MessageHead, StartLine,
        connection::HttpIo,
        connection::{ConnectionLease, Endpoint, HttpPool},
    },
    memory::{BufferPool, PlaintextBuffer},
    model::{
        MetadataSelector, ObjectId, ObjectMetadata, OriginContext, PAGE_BYTES, PageId, PageNumber,
        ResourceClass,
    },
    read::candidates::OriginAuthority,
    runtime::{admission::AdmissionPolicy, deadline::RequestScope},
};
use std::{
    path::{Path, PathBuf},
    rc::Rc,
};
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
    health: crate::topology::health::LinkHealth,
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
            health: crate::topology::health::LinkHealth::new(64),
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
        protocol::response(
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
                    &crate::model::NodeId(format!("{endpoint:?}")),
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
                    &crate::model::NodeId(format!("{endpoint:?}")),
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
                    &crate::model::NodeId(format!("{endpoint:?}")),
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
#[cfg(test)]
mod tests;

/// HEAD/initial-GET validation; empty objects produce metadata without a page.
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
pub fn validate_metadata(head: &MessageHead, object: &ObjectId) -> Result<ObjectMetadata> {
    let (status, length) = protocol::response(head, false)?;
    if status != 200 {
        return Err(Error::BadGateway);
    }
    protocol::absent(head, &["Content-Range"])?;
    protocol::metadata(head, object, length)
}
#[cfg(test)]
mod metadata_tests {
    use super::*;
    use crate::{
        http::{Header, StartLine},
        model::{CacheId, CacheKey},
    };
    use std::time::{Duration, UNIX_EPOCH};

    pub(super) fn object() -> ObjectId {
        ObjectId {
            cache: CacheId("cache-uid".into()),
            key: CacheKey([0xab; 32]),
        }
    }

    fn head(length: &[u8], expiry: &[u8], etag: &[u8]) -> MessageHead {
        crate::origin::tests::response_head(
            200,
            &[
                ("Content-Length", length),
                ("Racer-Expires-At", expiry),
                ("ETag", etag),
            ],
        )
    }

    #[test]
    fn head_retains_quoted_validator_and_exact_millisecond_expiry() {
        let metadata = validate_metadata(&head(b"0", b"1234", b"\"v,\\1\""), &object()).unwrap();
        assert_eq!(metadata.length, 0);
        assert_eq!(metadata.version.etag.as_bytes(), b"\"v,\\1\"");
        assert_eq!(
            metadata.expires_at.as_system_time(),
            UNIX_EPOCH + Duration::from_millis(1234)
        );
        assert_eq!(
            validate_metadata(
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
                validate_metadata(&head(b"1", invalid, b"\"v\""), &object()),
                Err(Error::BadGateway)
            );
            assert_eq!(
                validate_metadata(&head(invalid, b"0", b"\"v\""), &object()),
                Err(Error::BadGateway)
            );
        }
        for etag in [b"v".as_slice(), b"W/\"v\"", b"*", b"\"v\", \"w\""] {
            assert_eq!(
                validate_metadata(&head(b"1", b"0", etag), &object()),
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
            assert_eq!(
                validate_metadata(&response, &object()),
                Err(Error::BadGateway)
            );
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
        use crate::http::Codec;
        for expiry in ["0", " 0", "0 ", "\t0", "0\t", "0 \t"] {
            let raw = format!(
                "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nETag: \"v\"\r\nRacer-Expires-At: {expiry}\r\n\r\n"
            );
            let (head, _) = Codec::new(32768)
                .decode_head(raw.as_bytes())
                .unwrap()
                .unwrap();
            let result = validate_metadata(&head, &object());
            if expiry == "0" {
                assert_eq!(result.unwrap().expires_at.as_system_time(), UNIX_EPOCH);
            } else {
                assert_eq!(result, Err(Error::BadGateway), "expiry={expiry:?}");
            }
        }
    }
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
    let (status, length) = protocol::response(head, true)?;
    if status != 206 || protocol::required(head, "Content-Type")? != b"application/octet-stream" {
        return Err(Error::BadGateway);
    }
    let (first, last, total) = protocol::content_range(head)?;
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
    let metadata = protocol::metadata(head, &page.version.object, total)?;
    if metadata.version != page.version {
        return Err(Error::BadGateway);
    }
    Ok((metadata, length))
}
#[cfg(test)]
mod page_tests {
    use super::*;
    use crate::{
        http::StartLine,
        model::{CacheId, CacheKey, ObjectId, ObjectVersion, PageNumber, StrongEtag},
    };

    fn page() -> PageId {
        PageId {
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId("cache".into()),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::parse(b"\"v\"").unwrap(),
            },
            number: PageNumber(1),
        }
    }
    fn head() -> MessageHead {
        crate::origin::tests::response_head(
            206,
            &[
                ("Content-Length", b"3"),
                ("Content-Type", b"application/octet-stream"),
                ("Content-Range", b"bytes 16777216-16777218/16777219"),
                ("ETag", b"\"v\""),
                ("Racer-Expires-At", b"0"),
            ],
        )
    }

    #[test]
    fn pinned_final_page_accepts_only_exact_body_range_and_version() {
        let page = page();
        assert_eq!(
            validate_page(&head(), &page, 3).unwrap().length,
            PAGE_BYTES + 3
        );
        for count in [0, 2, 4, PAGE_BYTES as usize] {
            assert_eq!(validate_page(&head(), &page, count), Err(Error::BadGateway));
        }
        for range in [
            "bytes 0-2/3",
            "bytes 16777216-16777218/16777220",
            "bytes */16777219",
            "bytes 16777216-16777219/16777219",
            "bytes 016777216-16777218/16777219",
        ] {
            let mut response = head();
            response.headers[2].value = range.as_bytes().to_vec();
            assert_eq!(validate_page(&response, &page, 3), Err(Error::BadGateway));
        }
        let mut response = head();
        response.headers[3].value = b"\"other\"".to_vec();
        assert_eq!(validate_page(&response, &page, 3), Err(Error::BadGateway));
        response = head();
        response.start = StartLine::Response { status: 200 };
        assert_eq!(validate_page(&response, &page, 3), Err(Error::BadGateway));
        response = head();
        response.headers[1].value = b"multipart/byteranges".to_vec();
        assert_eq!(validate_page(&response, &page, 3), Err(Error::BadGateway));
    }
}

/// SDK wire rules shared by HEAD and whole-page responses.
mod protocol {
    use crate::{
        error::{Error, Result},
        http::{MessageHead, StartLine},
        model::{ExpiresAt, ObjectId, ObjectMetadata, ObjectVersion, StrongEtag},
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
                || name.eq_ignore_ascii_case("Racer-Content-Type")
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
            416 => {
                Error::UnsatisfiableRangeWithLength(unsatisfied_length.ok_or(Error::BadGateway)?)
            }
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
        use crate::http::Header;

        #[test]
        fn optional_content_type_is_validated_without_transport_substitution() {
            let object = ObjectId {
                cache: crate::model::CacheId("cache".into()),
                key: crate::model::CacheKey([0; 32]),
            };
            let base = crate::origin::tests::response_head(
                200,
                &[
                    ("ETag", b"\"v1\""),
                    ("Content-Length", b"3"),
                    ("Racer-Expires-At", b"0"),
                    ("Content-Type", b"application/octet-stream"),
                ],
            );
            assert!(metadata(&base, &object, 3).unwrap().content_type.is_none());
            for value in [
                b"application/vnd.oci.image.manifest.v1+json".as_slice(),
                b"",
                b"text",
                b"text/plain\t",
                b"text/plain, text/html",
                b"text/\xff",
                b"text/plain\r\nx:y",
            ] {
                let mut head = MessageHead {
                    start: StartLine::Response { status: 200 },
                    headers: base
                        .headers
                        .iter()
                        .map(|h| Header {
                            name: h.name.clone(),
                            value: h.value.clone(),
                        })
                        .collect(),
                };
                head.headers.push(Header {
                    name: "Racer-Content-Type".into(),
                    value: value.to_vec(),
                });
                let valid = value.starts_with(b"application/");
                assert_eq!(response(&head, false).is_ok(), valid, "{value:?}");
                assert_eq!(metadata(&head, &object, 3).is_ok(), valid);
                if valid {
                    assert_eq!(
                        metadata(&head, &object, 3)
                            .unwrap()
                            .content_type
                            .unwrap()
                            .as_bytes(),
                        value
                    );
                }
                head.headers.push(Header {
                    name: "racer-content-type".into(),
                    value: value.to_vec(),
                });
                assert_eq!(response(&head, false), Err(Error::BadGateway));
                assert!(metadata(&head, &object, 3).is_err());
            }
        }

        #[test]
        fn numeric_headers_reject_padding_before_any_normalization() {
            use crate::{
                http::Codec,
                model::{CacheId, CacheKey},
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
                    let result = Codec::new(32768)
                        .decode_head(raw.as_bytes())
                        .map_err(|_| Error::BadGateway)
                        .and_then(|head| {
                            super::super::validate_bootstrap(&head.unwrap().0, &object)
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
                let mut head =
                    crate::origin::tests::response_head(200, &[("Content-Length", b"0")]);
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
                let mut head =
                    crate::origin::tests::response_head(status, &[("Content-Length", b"0")]);
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
}
use uring_runtime::reactor::{Completion, IoBuffer};
