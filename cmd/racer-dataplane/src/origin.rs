//! Per-cache local adapter operations requiring validated candidate authority.
//! Connect to the adapter-owned /run/racer/<cache name>/origin/socket.
//!
//! Credentials are only origin-fetch context, never Racer authorization. Do not
//! persist headers or retain them in pooled connections after an operation ends.
use crate::admission::AdmissionPolicy;
use crate::admission::ResourceClass;
use crate::control::Snapshot;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::http::ConnectionLease;
use crate::http::Endpoint;
use crate::http::HttpIo;
use crate::http::HttpPool;
use crate::memory::BufferPool;
use crate::memory::PlaintextBuffer;
use crate::model::MetadataSelector;
use crate::model::ObjectId;
use crate::model::ObjectMetadata;
#[cfg(test)]
use crate::model::ObjectVersion;
use crate::model::PAGE_BYTES;
use crate::model::PageId;
use crate::model::PageNumber;
use crate::read::candidates::OriginAuthority;
use crate::runtime::RequestScope;
use crate::security::OriginContext;
use controlplane::Published;
use http1::Header;
use http1::MessageHead;
#[cfg(test)]
use http1::StartLine;
use racer_control_wire::CacheDefinition;
use racer_control_wire::canonical_socket_paths;
use std::path::Path;
use std::path::PathBuf;
use std::rc::Rc;
use uring_runtime::reactor::Completion;
#[cfg(test)]
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
    snapshots: std::sync::Arc<Published<Snapshot>>,
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
        snapshots: std::sync::Arc<Published<Snapshot>>,
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
        let snapshot = self.snapshots.current()?.ok_or(Error::Unavailable)?;
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
        connection: ConnectionLease,
        buffer: PlaintextBuffer,
        scope: &RequestScope,
    ) -> Result<Completion<PlaintextBuffer, ConnectionLease>> {
        self.io
            .read_body_exact(connection, buffer, scope)
            .await
            .map_err(response_error)
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
    Header::new(name, value)
}

fn response_error(error: Error) -> Error {
    match error {
        Error::InvalidRequest
        | Error::HeaderTooLarge
        | Error::CorruptRecord
        | Error::Io
        | Error::Os(_) => Error::BadGateway,
        other => other,
    }
}

/// Context is copied only into this operation's outbound HTTP head, never the pool.
fn request(context: &OriginContext, method: &str) -> Result<MessageHead> {
    racer_object_wire::origin::request(
        &context.object.key,
        method,
        context.metadata.as_ref().map(|value| value.as_header()),
        context
            .authorization
            .as_ref()
            .map(|value| value.expose_for_origin()),
    )
    .map_err(Into::into)
}

#[cfg(test)]
fn opaque(bytes: &[u8]) -> Result<()> {
    racer_object_wire::origin::opaque(bytes).map_err(Into::into)
}

/// HEAD/initial-GET validation; empty objects produce metadata without a page.
pub struct MetadataReply {
    pub metadata: ObjectMetadata,
    pub page_zero: Option<OriginPage>,
}

/// The initial GET selects metadata and page zero atomically at the adapter.
pub fn validate_bootstrap(head: &MessageHead, object: &ObjectId) -> Result<(ObjectMetadata, u64)> {
    racer_object_wire::origin::validate_bootstrap(head, object).map_err(Into::into)
}
pub fn validate_metadata(head: &MessageHead, object: &ObjectId) -> Result<ObjectMetadata> {
    racer_object_wire::origin::validate_metadata(head, object).map_err(Into::into)
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
    racer_object_wire::origin::validate_page(head, page, received_bytes).map_err(Into::into)
}

/// Validate before allocating or receiving any payload.
fn validate_page_head(head: &MessageHead, page: &PageId) -> Result<(ObjectMetadata, u64)> {
    racer_object_wire::origin::validate_page_head(head, page).map_err(Into::into)
}

/// Validate framing before interpreting status; malformed errors are never miss proof.
fn validate_response(head: &MessageHead, pinned: bool) -> Result<(u16, u64)> {
    racer_object_wire::origin::validate_response(head, pinned).map_err(Into::into)
}

#[cfg(test)]
mod tests;

#[cfg(test)]
fn decimal(bytes: &[u8]) -> Result<u64> {
    racer_object_wire::parse_decimal(bytes).map_err(|_| Error::BadGateway)
}

#[cfg(test)]
fn metadata(head: &MessageHead, object: &ObjectId, length: u64) -> Result<ObjectMetadata> {
    racer_object_wire::origin::metadata(head, object, length).map_err(Into::into)
}
