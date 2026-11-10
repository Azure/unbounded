//! In-process Azure Blob origin.
//!
//! Serves caches whose publication carries `CacheDefinition::origin` directly
//! from Azure Blob Storage. Request plan:
//!
//! - bootstrap: one `GET` with `x-ms-range: bytes=0-<page size - 1>`. A 206
//!   carries metadata, the size (from `Content-Range`), and page 0. An empty
//!   blob answers 416, so a `HEAD` follows to read its metadata.
//! - page: one `GET` with `x-ms-range` and `If-Match: <pinned ETag>`.
//! - metadata: `HEAD` (Get Blob Properties), with `If-Match` when pinned.
//!
//! I/O goes through two thin seams so the HTTP client and credential providers
//! can be supplied separately: [`AzureTransport`] sends one request head and
//! receives the body straight into the admitted [`PlaintextBuffer`], and
//! [`AzureTokenSource`] supplies bearer tokens.
//!
//! `max_concurrent_requests` is enforced node-wide by [`AzureLimits`], which all
//! workers share through an `Arc`. Each origin operation holds one permit for
//! its whole duration (a bootstrap of an empty blob holds one permit across its
//! GET and HEAD). Over the limit, operations fail fast with `Overloaded` rather
//! than queue. `request_timeout` narrows the caller's deadline for the whole
//! operation, including token acquisition.

pub mod crc64;
pub mod identity;
pub mod request;
pub mod response;
pub mod tokens;
pub mod transport;
pub mod upload;

use self::identity::AzureObject;
use self::request::{AzureMethod, ByteRange};
use self::response::{classify, content_range, status, transport_error};
use super::{MetadataReply, Origin, OriginPage};
use crate::admission::{AdmissionPolicy, ResourceClass};
use crate::control::Snapshot;
use crate::control::cell::PublicationCell;
use crate::error::{Error, Operation, Result};
use crate::memory::{BufferPool, PlaintextBuffer};
use crate::model::{
    ExpiresAt, MetadataSelector, ObjectId, ObjectMetadata, ObjectVersion, PageId, PageNumber,
    StrongEtag,
};
use crate::read::candidates::OriginAuthority;
use crate::runtime::RequestScope;
use crate::security::OriginContext;
use http1::MessageHead;
use racer_io::deadline::Deadline;
use racer_wire::control::{AzureContainerUrl, AzureCredential, CacheId, OriginDefinition};
use std::collections::HashMap;
use std::fmt;
use std::rc::Rc;
use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::{Arc, Mutex, Weak};
use std::time::{SystemTime, UNIX_EPOCH};
use zeroize::Zeroizing;

/// An OAuth access token for `https://storage.azure.com/`. Zeroized on drop
/// and redacted from `Debug`.
pub struct BearerToken(Zeroizing<Vec<u8>>);

impl BearerToken {
    /// Wrap raw token bytes. They must be a valid header value.
    pub fn new(token: Vec<u8>) -> Result<Self> {
        if token.is_empty() || token.iter().any(|b| !b.is_ascii_graphic()) {
            return Err(Error::InvalidConfiguration);
        }
        Ok(Self(Zeroizing::new(token)))
    }

    /// Borrow the raw token for one request head.
    pub fn as_bytes(&self) -> &[u8] {
        &self.0
    }
}

impl fmt::Debug for BearerToken {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("BearerToken([redacted])")
    }
}

/// Supplies bearer tokens for a cache's credential.
pub trait AzureTokenSource {
    /// `Ok(None)` for `AzureCredential::Anonymous`; otherwise a token valid for
    /// at least the rest of `scope`. Implementations cache and refresh tokens
    /// node-wide; this call is on the request path.
    fn bearer<'a>(
        &'a self,
        credential: &'a AzureCredential,
        scope: &'a RequestScope,
    ) -> Operation<'a, Option<BearerToken>>;

    /// Storage answered 401 to a request for `credential`. `challenge` is its
    /// `WWW-Authenticate` value and `sent` the bearer token the request
    /// carried, if any. Drop the rejected token (but not a newer one cached
    /// since) or learn a tenant from the challenge, and return whether one
    /// retry may succeed. The origin retries at most once.
    fn rejected(
        &self,
        credential: &AzureCredential,
        challenge: Option<&[u8]>,
        sent: Option<&[u8]>,
    ) -> bool;

    /// Drop state for credentials not used by any of `live`. Called when the
    /// worker observes a new snapshot; in-flight requests keep what they hold.
    fn retain(&self, _live: &[&OriginDefinition]) {}
}

/// Token source for deployments without credential providers: anonymous
/// access works, every other credential is `Unavailable`.
#[derive(Clone, Copy, Debug, Default)]
pub struct AnonymousOnly;

impl AzureTokenSource for AnonymousOnly {
    fn bearer<'a>(
        &'a self,
        credential: &'a AzureCredential,
        _scope: &'a RequestScope,
    ) -> Operation<'a, Option<BearerToken>> {
        Box::pin(async move {
            match credential {
                AzureCredential::Anonymous => Ok(None),
                _ => Err(Error::Unavailable),
            }
        })
    }

    fn rejected(&self, _: &AzureCredential, _: Option<&[u8]>, _: Option<&[u8]>) -> bool {
        false
    }
}

/// Sends one request to the container's host and receives the response head.
pub trait AzureTransport {
    type Exchange: AzureExchange;

    /// Connect (or reuse a pooled connection) to `url`'s scheme, host, and port,
    /// send `head` with no body, and receive the response head. Headers must be
    /// bounded; the body is left unread. Errors use the crate's transport kinds
    /// (`Io`, `Os`, `Unavailable` for network loss; anything else for malformed
    /// responses), never text containing the URL or headers.
    fn exchange<'a>(
        &'a self,
        url: &'a AzureContainerUrl,
        head: MessageHead,
        scope: &'a RequestScope,
    ) -> Operation<'a, Self::Exchange>;

    /// Like [`AzureTransport::exchange`], but send `body` after `head`, which
    /// must carry `Content-Length: body.len()`. Used by uploads.
    fn exchange_body<'a>(
        &'a self,
        url: &'a AzureContainerUrl,
        head: MessageHead,
        body: &'a [u8],
        scope: &'a RequestScope,
    ) -> Operation<'a, Self::Exchange>;

    /// Drop per-endpoint state for endpoints not used by any of `live`.
    /// Called when the worker observes a new snapshot; in-flight exchanges
    /// keep what they hold.
    fn retain(&self, _live: &[&OriginDefinition]) {}
}

/// One in-flight response. Dropping it without [`AzureExchange::finish`] or
/// [`AzureExchange::read_body`] abandons the connection (it is not reused).
pub trait AzureExchange {
    /// The received response head.
    fn head(&self) -> &MessageHead;

    /// Complete a response with no body to read: a `HEAD` response, or a
    /// response with `Content-Length: 0`. Returns the connection for reuse.
    fn finish(self) -> Result<()>;

    /// Receive exactly `buffer.len()` body bytes (the validated
    /// `Content-Length`) directly into `buffer`, then complete the exchange.
    /// A short body is an error.
    fn read_body<'a>(
        self,
        buffer: PlaintextBuffer,
        scope: &'a RequestScope,
    ) -> Operation<'a, PlaintextBuffer>
    where
        Self: 'a;

    /// Give up on an error response. Implementations may read a small,
    /// length-framed body to keep the connection; otherwise it is abandoned.
    fn discard<'a>(self, scope: &'a RequestScope) -> Operation<'a, ()>
    where
        Self: 'a;
}

/// Node-wide in-flight request counts per cache, shared by every worker.
///
/// Only permits own a counter; the map holds weak references, so a cache with
/// nothing in flight (including a deleted cache) holds no live counter. Dead
/// entries are swept when the map doubles past its last swept size, keeping
/// the map within twice the number of caches with operations in flight.
#[derive(Debug, Default)]
pub struct AzureLimits {
    caches: Mutex<LimitMap>,
}

#[derive(Debug, Default)]
struct LimitMap {
    counters: HashMap<CacheId, Weak<AtomicU32>>,
    /// Size after the last sweep; the next sweep runs at twice this.
    swept: usize,
}

/// One admitted upstream operation; releases its slot on drop.
#[derive(Debug)]
pub struct AzurePermit(Arc<AtomicU32>);

impl Drop for AzurePermit {
    fn drop(&mut self) {
        self.0.fetch_sub(1, Ordering::AcqRel);
    }
}

impl AzureLimits {
    /// Admit one operation for `cache`, or fail with `Overloaded` at `max`.
    pub fn acquire(&self, cache: &CacheId, max: u32) -> Result<AzurePermit> {
        let counter = {
            let mut caches = self.caches.lock().map_err(|_| Error::Internal)?;
            match caches.counters.get(cache).and_then(Weak::upgrade) {
                Some(counter) => counter,
                None => {
                    if caches.counters.len() >= (caches.swept * 2).max(16) {
                        caches
                            .counters
                            .retain(|_, counter| counter.strong_count() > 0);
                        caches.swept = caches.counters.len();
                    }
                    let counter = Arc::new(AtomicU32::new(0));
                    caches
                        .counters
                        .insert(cache.clone(), Arc::downgrade(&counter));
                    counter
                }
            }
        };
        counter
            .fetch_update(Ordering::AcqRel, Ordering::Acquire, |count| {
                (count < max).then_some(count + 1)
            })
            .map_err(|_| Error::Overloaded)?;
        Ok(AzurePermit(counter))
    }

    /// In-flight operations for `cache`.
    pub fn in_flight(&self, cache: &CacheId) -> u32 {
        self.caches
            .lock()
            .ok()
            .and_then(|caches| caches.counters.get(cache).and_then(Weak::upgrade))
            .map_or(0, |counter| counter.load(Ordering::Acquire))
    }

    /// Map entries, live or not yet swept (tests).
    #[cfg(test)]
    pub(crate) fn tracked(&self) -> usize {
        self.caches.lock().map_or(0, |caches| caches.counters.len())
    }
}

/// Process-wide Azure origin state shared by every worker: the concurrency
/// limits, the token caches, and the TLS trust roots loaded at assembly.
#[derive(Clone, Debug)]
pub struct AzureNode {
    pub limits: Arc<AzureLimits>,
    pub tokens: Arc<tokens::AzureTokenCaches>,
    pub trust: Arc<transport::TrustRoots>,
}

impl AzureNode {
    /// Token endpoints come from configuration, and the trust bundle is
    /// loaded now (see [`transport::TrustRoots`]): call this at node
    /// assembly, never on a worker reactor. Nothing touches the network until
    /// a cache needs it.
    pub fn new(settings: &crate::config::OriginSettings) -> Self {
        Self::with_trust(
            settings,
            transport::TrustRoots::load(settings.trust_bundle.clone()),
        )
    }

    /// Default settings with no trust roots and no file I/O; HTTPS origins
    /// fail with `Unavailable`.
    pub fn unconfigured() -> Self {
        let settings = crate::config::OriginSettings::default();
        let trust = transport::TrustRoots::unavailable(settings.trust_bundle.clone());
        Self::with_trust(&settings, trust)
    }

    fn with_trust(settings: &crate::config::OriginSettings, trust: transport::TrustRoots) -> Self {
        Self {
            limits: Arc::default(),
            tokens: Arc::new(tokens::AzureTokenCaches::new(
                crate::origin::azure_auth::AuthEndpoints::new(settings, trust.config().ok()),
            )),
            trust: Arc::new(trust),
        }
    }
}

/// Reads published Azure origins for one worker.
pub struct AzureOrigin<T, K> {
    transport: T,
    tokens: K,
    snapshots: Arc<PublicationCell<Snapshot>>,
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    buffers: BufferPool,
    limits: Arc<AzureLimits>,
    /// The last snapshot pruned against. Weak, so the generation is not kept
    /// alive, and its allocation cannot be reused by a later snapshot.
    observed: std::cell::RefCell<std::sync::Weak<Snapshot>>,
}

impl<T: AzureTransport, K: AzureTokenSource> AzureOrigin<T, K> {
    pub fn new(
        transport: T,
        tokens: K,
        snapshots: Arc<PublicationCell<Snapshot>>,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        buffers: BufferPool,
        limits: Arc<AzureLimits>,
    ) -> Self {
        Self {
            transport,
            tokens,
            snapshots,
            admission,
            buffers,
            limits,
            observed: std::cell::RefCell::default(),
        }
    }

    /// Prune worker-local endpoint and token state against `snapshot` the
    /// first time it is seen. One pointer comparison on the request path;
    /// the scan over the snapshot's caches runs once per generation.
    pub fn observe(&self, snapshot: &Arc<Snapshot>) {
        if std::ptr::eq(self.observed.borrow().as_ptr(), Arc::as_ptr(snapshot)) {
            return;
        }
        *self.observed.borrow_mut() = Arc::downgrade(snapshot);
        let live: Vec<&OriginDefinition> = snapshot
            .caches
            .iter()
            .filter_map(|cache| cache.origin.as_ref())
            .collect();
        self.transport.retain(&live);
        self.tokens.retain(&live);
    }

    /// The published definition for the context's cache. The cache UID is the
    /// lookup key, so a recreated ClusterCache never reaches a stale origin.
    fn definition(&self, object: &ObjectId) -> Result<(Arc<Snapshot>, usize)> {
        let snapshot = self.snapshots.current()?.ok_or(Error::Unavailable)?;
        self.observe(&snapshot);
        let index = snapshot
            .caches
            .position(&object.cache)
            .filter(|i| snapshot.caches[*i].origin.is_some())
            .ok_or(Error::Unavailable)?;
        Ok((snapshot, index))
    }

    /// The published cluster page size; the default before any publication.
    pub(crate) fn published_page_size(&self) -> Result<racer_wire::object::PageSize> {
        Ok(self
            .snapshots
            .current()?
            .map_or(racer_wire::object::PageSize::DEFAULT, |snapshot| {
                snapshot.page_size
            }))
    }

    /// Check the supplied full-page plaintext charge without taking ownership.
    fn validate_reservation(
        &self,
        reservation: &flow_control::Charge<AdmissionPolicy>,
        object: &ObjectId,
    ) -> Result<()> {
        reservation.validate(
            ResourceClass::Plaintext,
            self.published_page_size()?.get() as usize,
        )?;
        if !self.admission.owns(reservation) || reservation.key() != Some(&object.cache) {
            return Err(Error::InvalidConfiguration);
        }
        Ok(())
    }

    /// Decode the request identity, admit, narrow the deadline, and fetch a token.
    async fn begin(
        &self,
        definition: &OriginDefinition,
        context: &OriginContext,
        scope: &RequestScope,
    ) -> Result<Begun> {
        scope.check()?;
        let raw = context.metadata.as_ref().ok_or(Error::InvalidRequest)?;
        let object = identity::decode(
            raw.as_header(),
            &context.object.key,
            definition.azure.container_url.container(),
        )?;
        let permit = self
            .limits
            .acquire(&context.object.cache, definition.max_concurrent_requests)?;
        let mut scope = scope.clone();
        let limit = scope
            .clock()
            .now()
            .checked_add(definition.request_timeout)
            .unwrap_or(scope.deadline.0);
        scope.deadline = Deadline(scope.deadline.0.min(limit));
        scope.check()?;
        Ok(Begun {
            object,
            scope,
            _permit: permit,
        })
    }

    async fn send(
        &self,
        definition: &OriginDefinition,
        begun: &Begun,
        method: AzureMethod,
        range: Option<ByteRange>,
        if_match: Option<&StrongEtag>,
    ) -> Result<T::Exchange> {
        let url = &definition.azure.container_url;
        self.send_with(
            definition,
            begun,
            |bearer, now| request::head(url, &begun.object, method, range, if_match, bearer, now),
            None,
        )
        .await
    }

    /// Send one request built by `build(bearer, now)`, with `body` when set.
    /// The head is rebuilt per attempt so a retry carries a fresh date and
    /// token. A 401 earns one retry when the token source says it may help.
    async fn send_with(
        &self,
        definition: &OriginDefinition,
        begun: &Begun,
        build: impl Fn(Option<&[u8]>, SystemTime) -> Result<MessageHead>,
        body: Option<&[u8]>,
    ) -> Result<T::Exchange> {
        let url = &definition.azure.container_url;
        let credential = &definition.azure.credential;
        let mut retried = false;
        loop {
            let token = self.tokens.bearer(credential, &begun.scope).await?;
            let head = build(
                token.as_ref().map(BearerToken::as_bytes),
                begun.scope.clock().wall_now(),
            )?;
            let exchange = match body {
                None => self.transport.exchange(url, head, &begun.scope).await,
                Some(body) => {
                    self.transport
                        .exchange_body(url, head, body, &begun.scope)
                        .await
                }
            }
            .map_err(transport_error)?;
            begun.scope.check()?;
            if retried || status(exchange.head())? != 401 {
                return Ok(exchange);
            }
            // Expired or revoked tokens, and workload identity before its
            // tenant is known, earn one retry. Anything else is final.
            let challenge = response::field(exchange.head(), "WWW-Authenticate")
                .ok()
                .flatten();
            let sent = token.as_ref().map(BearerToken::as_bytes);
            if !self.tokens.rejected(credential, challenge, sent) {
                return Ok(exchange);
            }
            drop(token);
            let _ = exchange.discard(&begun.scope).await;
            retried = true;
        }
    }

    /// Classify an error response, then release its connection.
    async fn reject(&self, exchange: T::Exchange, pinned: bool, scope: &RequestScope) -> Error {
        let error = classify(exchange.head(), pinned);
        let _ = exchange.discard(scope).await;
        error
    }

    async fn read(
        &self,
        exchange: T::Exchange,
        reservation: flow_control::Charge<AdmissionPolicy>,
        length: u64,
        scope: &RequestScope,
    ) -> Result<PlaintextBuffer> {
        let buffer = self.buffers.plaintext(reservation, length as usize)?;
        let buffer = exchange
            .read_body(buffer, scope)
            .await
            .map_err(transport_error)?;
        scope.check()?;
        Ok(buffer)
    }

    /// Bootstrap against an explicit definition (router entry point).
    pub async fn bootstrap_with(
        &self,
        definition: &OriginDefinition,
        context: &OriginContext,
        reservation: flow_control::Charge<AdmissionPolicy>,
        scope: &RequestScope,
    ) -> Result<MetadataReply> {
        self.validate_reservation(&reservation, &context.object)?;
        let page_size = self.published_page_size()?;
        let size = page_size.get();
        let begun = self.begin(definition, context, scope).await?;
        let range = ByteRange {
            first: 0,
            last: size - 1,
        };
        let exchange = self
            .send(definition, &begun, AzureMethod::Get, Some(range), None)
            .await?;
        let head = exchange.head();
        let (blob, total) = match status(head)? {
            206 => {
                let blob = response::blob(head, &begun.object)?;
                let (first, last, total) = content_range(head)?;
                if first != 0
                    || blob.content_length != last + 1
                    || blob.content_length != total.min(size)
                {
                    return Err(Error::BadGateway);
                }
                (blob, total)
            }
            // Defensive: a service that ignores the range must still fit page 0.
            200 => {
                let blob = response::blob(head, &begun.object)?;
                if response::field(head, "Content-Range")?.is_some() || blob.content_length > size {
                    return Err(Error::BadGateway);
                }
                let total = blob.content_length;
                (blob, total)
            }
            // The range is unsatisfiable only for an empty blob.
            416 => {
                let _ = exchange.discard(&begun.scope).await;
                let metadata = self.head(definition, &begun, &context.object, None).await?;
                if metadata.length != 0 {
                    // Replaced between the two requests; let the caller retry.
                    return Err(Error::Unavailable);
                }
                return Ok(MetadataReply {
                    metadata,
                    page_zero: None,
                });
            }
            _ => return Err(self.reject(exchange, false, &begun.scope).await),
        };
        let metadata = metadata(
            &context.object,
            blob.etag,
            blob.content_type,
            total,
            definition,
            begun.scope.clock().wall_now(),
        )?;
        if blob.content_length == 0 {
            exchange.finish().map_err(transport_error)?;
            return Ok(MetadataReply {
                metadata,
                page_zero: None,
            });
        }
        let plaintext = self
            .read(exchange, reservation, blob.content_length, &begun.scope)
            .await?;
        Ok(MetadataReply {
            page_zero: Some(OriginPage {
                metadata: metadata.clone(),
                plaintext,
                page_size,
            }),
            metadata,
        })
    }

    /// Get Blob Properties, optionally pinned.
    async fn head(
        &self,
        definition: &OriginDefinition,
        begun: &Begun,
        object: &ObjectId,
        pin: Option<&StrongEtag>,
    ) -> Result<ObjectMetadata> {
        let exchange = self
            .send(definition, begun, AzureMethod::Head, None, pin)
            .await?;
        let head = exchange.head();
        if status(head)? != 200 {
            return Err(self.reject(exchange, pin.is_some(), &begun.scope).await);
        }
        let blob = response::blob(head, &begun.object)?;
        // Recheck the pin even if the service ignored If-Match.
        if pin.is_some_and(|pin| *pin != blob.etag) {
            return Err(Error::VersionUnavailable);
        }
        exchange.finish().map_err(transport_error)?;
        metadata(
            object,
            blob.etag,
            blob.content_type,
            blob.content_length,
            definition,
            begun.scope.clock().wall_now(),
        )
    }

    /// Metadata against an explicit definition (router entry point).
    pub async fn metadata_with(
        &self,
        definition: &OriginDefinition,
        context: &OriginContext,
        selector: MetadataSelector,
        scope: &RequestScope,
    ) -> Result<MetadataReply> {
        let begun = self.begin(definition, context, scope).await?;
        let pin = match &selector {
            MetadataSelector::Fresh => None,
            MetadataSelector::Pinned(etag) => Some(etag),
        };
        let metadata = self.head(definition, &begun, &context.object, pin).await?;
        Ok(MetadataReply {
            metadata,
            page_zero: None,
        })
    }

    /// One pinned page against an explicit definition (router entry point).
    pub async fn page_with(
        &self,
        definition: &OriginDefinition,
        context: &OriginContext,
        page: &PageId,
        reservation: flow_control::Charge<AdmissionPolicy>,
        scope: &RequestScope,
    ) -> Result<OriginPage> {
        if page.version.object != context.object {
            return Err(Error::Unauthorized);
        }
        let page_size = self.published_page_size()?;
        let size = page_size.get();
        let first = page.number.0.checked_mul(size).ok_or(Error::InvalidRange)?;
        let last = first
            .checked_add(size - 1)
            .filter(|last| *last <= i64::MAX as u64)
            .ok_or(Error::InvalidRange)?;
        self.validate_reservation(&reservation, &context.object)?;
        let begun = self.begin(definition, context, scope).await?;
        let exchange = self
            .send(
                definition,
                &begun,
                AzureMethod::Get,
                Some(ByteRange { first, last }),
                Some(&page.version.etag),
            )
            .await?;
        let head = exchange.head();
        match status(head)? {
            206 => {}
            // The service ignored the page range.
            200 => return Err(Error::BadGateway),
            _ => return Err(self.reject(exchange, true, &begun.scope).await),
        }
        let blob = response::blob(head, &begun.object)?;
        if blob.etag != page.version.etag {
            return Err(Error::VersionUnavailable);
        }
        let (got_first, got_last, total) = content_range(head)?;
        if got_first != first
            || blob.content_length != got_last - got_first + 1
            || blob.content_length != (total - first).min(size)
        {
            return Err(Error::BadGateway);
        }
        let metadata = metadata(
            &context.object,
            blob.etag,
            blob.content_type,
            total,
            definition,
            begun.scope.clock().wall_now(),
        )?;
        let plaintext = self
            .read(exchange, reservation, blob.content_length, &begun.scope)
            .await?;
        Ok(OriginPage {
            metadata,
            plaintext,
            page_size,
        })
    }
}

/// Per-operation state after admission.
struct Begun {
    object: AzureObject,
    scope: RequestScope,
    _permit: AzurePermit,
}

/// `expires_at = now + metadata_ttl`, truncated to milliseconds as in Go.
fn metadata(
    object: &ObjectId,
    etag: StrongEtag,
    content_type: Option<crate::model::ContentType>,
    length: u64,
    definition: &OriginDefinition,
    now: SystemTime,
) -> Result<ObjectMetadata> {
    let expires = now
        .checked_add(definition.metadata_ttl)
        .and_then(|time| time.duration_since(UNIX_EPOCH).ok())
        .ok_or(Error::Internal)?;
    let millis = u64::try_from(expires.as_millis()).map_err(|_| Error::Internal)?;
    Ok(ObjectMetadata {
        content_type,
        version: ObjectVersion {
            object: object.clone(),
            etag,
        },
        length,
        expires_at: ExpiresAt::from_unix_millis(millis).map_err(|_| Error::Internal)?,
    })
}

impl<T: AzureTransport, K: AzureTokenSource> Origin for AzureOrigin<T, K> {
    fn page_size(&self) -> Result<racer_wire::object::PageSize> {
        self.published_page_size()
    }

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
            let (snapshot, index) = self.definition(&context.object)?;
            let definition = snapshot.caches[index]
                .origin
                .as_ref()
                .ok_or(Error::Unavailable)?;
            self.bootstrap_with(definition, context, reservation, scope)
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
            let (snapshot, index) = self.definition(&context.object)?;
            let definition = snapshot.caches[index]
                .origin
                .as_ref()
                .ok_or(Error::Unavailable)?;
            self.page_with(definition, context, page, reservation, scope)
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
            let (snapshot, index) = self.definition(&context.object)?;
            let definition = snapshot.caches[index]
                .origin
                .as_ref()
                .ok_or(Error::Unavailable)?;
            self.metadata_with(definition, context, selector, scope)
                .await
        })
    }
}

#[cfg(test)]
mod tests;
