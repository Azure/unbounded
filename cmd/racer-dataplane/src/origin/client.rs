//! Per-cache local adapter operations requiring validated candidate authority.
//! Connect to the adapter-owned /run/racer/<cache name>/origin/socket.
//!
//! Credentials are only origin-fetch context, never Racer authorization. Do not
//! persist headers or retain them in pooled connections after an operation ends.
use super::{
    metadata::{self, MetadataReply},
    page::{self, OriginPage},
    protocol,
};
use crate::{
    control::{
        caches::{CacheDefinition, canonical_socket_paths},
        snapshot::SnapshotStore,
    },
    error::{Error, Operation, Result},
    http::{
        codec::{Header, MessageHead, StartLine},
        io::HttpIo,
        pool::{ConnectionLease, Endpoint, HttpPool},
    },
    memory::pool::{BufferPool, PlaintextBuffer},
    model::{
        context::OriginContext,
        identity::{PageId, PageNumber},
        limits::ResourceClass,
        metadata::MetadataSelector,
        range::PAGE_BYTES,
    },
    read::candidates::OriginAuthority,
    runtime::{
        admission::{Admission, Reservation},
        deadline::RequestScope,
        reactor::{Completion, IoBuffer},
    },
};
use std::{
    path::{Path, PathBuf},
    rc::Rc,
};
pub trait Origin {
    /// Consume the fill's atomically admitted plaintext budget.
    fn page_reserved<'a>(
        &'a self,
        authority: &'a OriginAuthority,
        context: &'a OriginContext,
        page: &'a PageId,
        reservation: Reservation,
        scope: &'a RequestScope,
    ) -> Operation<'a, OriginPage> {
        drop(reservation);
        self.page(authority, context, page, scope)
    }
    /// Fresh page-zero acquisition. Implementations may return metadata only.
    fn bootstrap<'a>(
        &'a self,
        authority: &'a OriginAuthority,
        context: &'a OriginContext,
        scope: &'a RequestScope,
    ) -> Operation<'a, MetadataReply> {
        self.metadata(authority, context, MetadataSelector::Fresh, scope)
    }
    fn metadata<'a>(
        &'a self,
        authority: &'a OriginAuthority,
        context: &'a OriginContext,
        selector: MetadataSelector,
        scope: &'a RequestScope,
    ) -> Operation<'a, MetadataReply>;
    fn page<'a>(
        &'a self,
        authority: &'a OriginAuthority,
        context: &'a OriginContext,
        page: &'a PageId,
        scope: &'a RequestScope,
    ) -> Operation<'a, OriginPage>;
}
pub struct OriginClient {
    snapshots: Rc<SnapshotStore>,
    pool: Rc<HttpPool>,
    io: Rc<HttpIo>,
    buffers: Option<(Rc<Admission>, Rc<BufferPool>)>,
    socket_root: PathBuf,
}
impl OriginClient {
    pub fn new(snapshots: Rc<SnapshotStore>, pool: Rc<HttpPool>, io: Rc<HttpIo>) -> Self {
        Self {
            snapshots,
            pool,
            io,
            buffers: None,
            socket_root: PathBuf::from("/run/racer"),
        }
    }

    /// Install the worker's shared bounded plaintext allocation owners.
    pub fn with_buffers(mut self, admission: Rc<Admission>, buffers: Rc<BufferPool>) -> Self {
        self.buffers = Some((admission, buffers));
        self
    }

    /// Remap canonical published endpoints to `<root>/<cache name>/origin/socket`.
    /// The deployment root must be an absolute, lexically canonical directory path
    /// without NUL, empty, `.` or `..` components. This performs no filesystem I/O;
    /// the caller provisions and owns the directories (including any symlink policy).
    /// The complete resolved endpoint is checked against Linux's 107-byte UDS cap.
    pub fn with_socket_root(mut self, root: impl Into<PathBuf>) -> Result<Self> {
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
        self.socket_root = root;
        Ok(self)
    }

    async fn bootstrap_at(
        &self,
        endpoint: &Endpoint,
        context: &OriginContext,
        scope: &RequestScope,
    ) -> Result<MetadataReply> {
        scope.check()?;
        let (admission, buffers) = self.buffers.as_ref().ok_or(Error::InvalidConfiguration)?;
        let reservation = admission.reserve(
            Some(&context.object.cache),
            ResourceClass::Plaintext,
            PAGE_BYTES as usize,
        )?;
        let mut head = request(context, "GET")?;
        head.headers.push(header("Range", b"bytes=0-16777215"));
        let connection = self
            .pool
            .checkout(endpoint, scope)
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
        let (metadata, length) = metadata::validate_bootstrap(&response.value, &context.object)?;
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
        page::validate(&response.value, &page, body.bytes)?;
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
        Ok(Endpoint::Unix(resolve_socket(&self.socket_root, cache)?))
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
        let connection = self
            .pool
            .checkout(endpoint, scope)
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
        let metadata = metadata::validate(&response.value, &context.object)?;
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

    async fn page_at(
        &self,
        endpoint: &Endpoint,
        context: &OriginContext,
        page: &PageId,
        scope: &RequestScope,
    ) -> Result<OriginPage> {
        let (admission, _) = self.buffers.as_ref().ok_or(Error::InvalidConfiguration)?;
        let reservation = admission.reserve(
            Some(&context.object.cache),
            ResourceClass::Plaintext,
            PAGE_BYTES as usize,
        )?;
        self.page_reserved_at(endpoint, context, page, reservation, scope)
            .await
    }

    async fn page_reserved_at(
        &self,
        endpoint: &Endpoint,
        context: &OriginContext,
        page: &PageId,
        reservation: Reservation,
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
        let (admission, buffers) = self.buffers.as_ref().ok_or(Error::InvalidConfiguration)?;
        reservation.validate(ResourceClass::Plaintext, PAGE_BYTES as usize)?;
        if !admission.owns(&reservation) || reservation.cache() != Some(&context.object.cache) {
            return Err(Error::InvalidConfiguration);
        }
        let connection = self
            .pool
            .checkout(endpoint, scope)
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
        let (_, length) = page::validate_head(&response.value, page)?;
        let buffer = buffers.plaintext(reservation, length as usize)?;
        let mut body = self.read_page(response.connection, buffer, scope).await?;
        let metadata = page::validate(&response.value, page, body.bytes)?;
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
    fn page_reserved<'a>(
        &'a self,
        authority: &'a OriginAuthority,
        context: &'a OriginContext,
        page: &'a PageId,
        reservation: Reservation,
        scope: &'a RequestScope,
    ) -> Operation<'a, OriginPage> {
        Box::pin(async move {
            scope.check()?;
            if context.object != page.version.object {
                return Err(Error::Unauthorized);
            }
            authority.validate(&page.version.object, page.number)?;
            let endpoint = self.endpoint(context)?;
            self.page_reserved_at(&endpoint, context, page, reservation, scope)
                .await
        })
    }
    fn bootstrap<'a>(
        &'a self,
        authority: &'a OriginAuthority,
        context: &'a OriginContext,
        scope: &'a RequestScope,
    ) -> Operation<'a, MetadataReply> {
        Box::pin(async move {
            scope.check()?;
            authority.validate(&context.object, PageNumber(0))?;
            let endpoint = self.endpoint(context)?;
            self.bootstrap_at(&endpoint, context, scope).await
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
            self.metadata_at(&endpoint, context, selector, scope).await
        })
    }
    fn page<'a>(
        &'a self,
        authority: &'a OriginAuthority,
        context: &'a OriginContext,
        page: &'a PageId,
        scope: &'a RequestScope,
    ) -> Operation<'a, OriginPage> {
        Box::pin(async move {
            scope.check()?;
            if context.object != page.version.object {
                return Err(Error::Unauthorized);
            }
            authority.validate(&page.version.object, page.number)?;
            let endpoint = self.endpoint(context)?;
            self.page_at(&endpoint, context, page, scope).await
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
        let bytes = metadata.as_header()?;
        opaque(bytes)?;
        headers.push(header("Racer-Metadata", bytes));
    }
    if let Some(authorization) = &context.authorization {
        let bytes = authorization.expose_for_origin()?;
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
mod tests {
    include!("tests.rs");
}
