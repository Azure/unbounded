//! The writer: serves `PUT`, `host-write`, `persist`, and `DELETE` on a
//! client socket of a cache with writes enabled.

use super::RANKED_DEPTH;
use super::coordinator::MAX_EXPLICIT_HOLDER_BYTES;
use super::coordinator::MAX_RANKED_HOLDER_BYTES;
use super::coordinator::background_scope;
use super::coordinator::page_id;
use super::coordinator::rank_holders;
use super::coordinator::ranked;
use super::metrics::Outcome;
use super::send;
use crate::admission::ResourceClass;
use crate::error::Error;
use crate::error::Result;
use crate::http::ConnectionLease;
use crate::http::HttpIo;
use crate::memory::CiphertextCopy;
use crate::memory::PlaintextBuffer;
use crate::model::ExpiresAt;
use crate::model::ObjectId;
use crate::model::ObjectMetadata;
use crate::model::ObjectVersion;
use crate::model::PageSize;
use crate::peer::protocol::Operation as PeerOperation;
use crate::peer::protocol::WriteStore;
use crate::read::Coordinator;
use crate::runtime::Cancellation;
use crate::runtime::RequestScope;
use crate::security::Authorization;
use crate::security::OpaqueMetadata;
use crate::security::OriginContext;
use crate::topology::Membership;
use futures::StreamExt;
use futures::stream::FuturesUnordered;
use http1::MessageHead;
use racer_core::write::Durability;
use racer_core::write::Holders;
use racer_core::write::SessionId;
use racer_core::write::UploadMode;
use racer_core::write::UploadState;
use racer_core::write::WriteAction;
use racer_core::write::WriteReply;
use racer_core::write::WriteSpec;
use racer_wire::control::CacheId;
use racer_wire::control::NodeId;
use racer_wire::object::write::WriteKind;
use racer_wire::object::write::error_token;
use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;
use std::task::Poll;
use std::time::Duration;

/// Smallest request budget of a write; large objects stream for a while.
const WRITE_TIMEOUT: Duration = Duration::from_secs(3600);

/// Retries of a store refused by a busy target or link (peer admission or
/// connection limits, which shed rather than queue).
const BUSY_RETRIES: u32 = 30;

/// First wait between those retries; it doubles up to [`BUSY_WAIT_MAX`].
const BUSY_WAIT: Duration = Duration::from_millis(2);

/// Longest wait between those retries.
const BUSY_WAIT_MAX: Duration = Duration::from_millis(100);

/// Longest `status` long poll of an `origin`-durability write.
const STATUS_WAIT_MILLIS: u32 = 30_000;

/// A write that failed: its status, `Racer-Write-Error`, and `Retry-After`.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Reject {
    pub status: u16,
    pub token: Option<&'static str>,
    pub retry_after: Option<u32>,
}

impl Reject {
    pub const fn new(status: u16) -> Self {
        Self {
            status,
            token: None,
            retry_after: None,
        }
    }

    const fn token(status: u16, token: &'static str) -> Self {
        Self {
            status,
            token: Some(token),
            retry_after: None,
        }
    }

    const fn busy(token: &'static str) -> Self {
        Self {
            status: 503,
            token: Some(token),
            retry_after: Some(1),
        }
    }

    fn outcome(self) -> Outcome {
        match self.status {
            412 => Outcome::Exists,
            503 | 504 => Outcome::Unavailable,
            400..=499 => Outcome::Rejected,
            _ => Outcome::Failed,
        }
    }

    fn head(self) -> MessageHead {
        racer_wire::object::write::write_error_head(self.status, self.token, self.retry_after)
    }
}

/// The status of an internal error.
fn reject_for(error: Error) -> Reject {
    match error {
        Error::InvalidRequest | Error::InvalidRange => Reject::new(400),
        Error::HeaderTooLarge => Reject::new(431),
        Error::MethodNotAllowed => Reject::new(405),
        Error::DeadlineExceeded => Reject::new(504),
        Error::Overloaded | Error::FlightFull => Reject::busy(error_token::DIRTY_CAP),
        Error::Forbidden | Error::OriginForbidden => Reject::new(403),
        Error::Unauthorized | Error::OriginRejected => Reject::new(401),
        _ => Reject::busy(error_token::COORDINATOR),
    }
}

/// Whether `head` is a write the cache must serve: a write route on a cache
/// with writes enabled. Writes on other caches keep the read parser's `405`.
pub fn routes(local: &Coordinator, cache: &CacheId, head: &MessageHead) -> bool {
    racer_wire::object::write::is_write(head)
        && local.availability.writes(cache).is_some()
        && !local.availability.passthrough(cache)
}

/// Serve one write request and close the connection.
#[allow(clippy::too_many_arguments)]
pub async fn serve(
    connection: ConnectionLease,
    head: MessageHead,
    descriptors: Vec<std::os::fd::OwnedFd>,
    cache: &CacheId,
    header_limit: usize,
    local: &Coordinator,
    io: &HttpIo,
    timeout: Duration,
    cancellation: Cancellation,
) -> Result<()> {
    let scope = crate::client::listener::new_scope(
        io.reactor().ctx(),
        timeout.max(WRITE_TIMEOUT),
        cancellation,
    )?;
    let metrics = local.fill.metrics.writes().clone();
    let (connection, result) = handle(
        connection,
        &head,
        descriptors,
        cache,
        header_limit,
        local,
        io,
        &scope,
    )
    .await;
    let Some(mut connection) = connection else {
        metrics.request(&cache.0, Outcome::Failed);
        return Ok(());
    };
    let (head, outcome) = match result {
        Ok((head, outcome)) => (head, outcome),
        Err(reject) => (reject.head(), reject.outcome()),
    };
    metrics.request(&cache.0, outcome);
    if scope.check().is_err() {
        // The client went away or the request expired; nothing to answer.
        return Ok(());
    }
    let mut head = head;
    head.headers.push(http1::Header::new("Connection", "close"));
    connection.poison();
    let _ = io.send_head(connection, head, &scope).await?;
    Ok(())
}

/// The response head and outcome, and the connection when it can still
/// carry a response.
#[allow(clippy::too_many_arguments)]
async fn handle(
    connection: ConnectionLease,
    head: &MessageHead,
    descriptors: Vec<std::os::fd::OwnedFd>,
    cache: &CacheId,
    header_limit: usize,
    local: &Coordinator,
    io: &HttpIo,
    scope: &RequestScope,
) -> (
    Option<ConnectionLease>,
    std::result::Result<(MessageHead, Outcome), Reject>,
) {
    let page_size = match local.page_size() {
        Ok(size) => size,
        Err(error) => return (Some(connection), Err(reject_for(error))),
    };
    let request = match racer_wire::object::write::parse_write(head, header_limit, page_size) {
        Ok(request) => request,
        Err(error) => {
            let reject = match error.status() {
                Some(status) => Reject::new(status),
                None => match error {
                    racer_wire::object::write::WriteError::Object(error) => {
                        reject_for(Error::from(error))
                    }
                    _ => Reject::new(400),
                },
            };
            return (Some(connection), Err(reject));
        }
    };
    let object = ObjectId {
        cache: cache.clone(),
        key: request.key,
    };
    let context = match origin_context(&object, request.metadata, request.authorization) {
        Ok(context) => context,
        Err(_) => return (Some(connection), Err(Reject::new(400))),
    };
    match request.kind {
        WriteKind::Delete => (
            Some(connection),
            delete(local, &object, &context, scope).await,
        ),
        WriteKind::Persist => (
            Some(connection),
            persist(local, &object, &context, scope).await,
        ),
        WriteKind::Put { content_length } => {
            let options = Options {
                durability: request.durability,
                upload: request.upload,
                hold_seconds: request.hold_seconds,
                content_type: request.content_type.clone(),
                length: content_length,
            };
            if !descriptors.is_empty() {
                return (Some(connection), Err(Reject::new(400)));
            }
            let source = Source::Body {
                connection: Some(connection),
                io,
            };
            create(local, &object, context, options, source, page_size, scope).await
        }
        WriteKind::HostWrite { source } => {
            let options = Options {
                durability: request.durability,
                upload: request.upload,
                hold_seconds: request.hold_seconds,
                content_type: request.content_type.clone(),
                length: source.length,
            };
            let file = match host_source(descriptors, source.offset, source.length) {
                Ok(file) => file,
                Err(_) => return (Some(connection), Err(Reject::new(400))),
            };
            let source = Source::File {
                file,
                offset: source.offset,
            };
            let (_, result) =
                create(local, &object, context, options, source, page_size, scope).await;
            (Some(connection), result)
        }
    }
}

fn origin_context(
    object: &ObjectId,
    metadata: Option<&[u8]>,
    authorization: Option<&[u8]>,
) -> Result<OriginContext> {
    Ok(OriginContext {
        object: object.clone(),
        metadata: metadata.map(OpaqueMetadata::from_header).transpose()?,
        authorization: authorization.map(Authorization::from_header).transpose()?,
    })
}

/// Validate a `host-write` descriptor: exactly one regular file (or memfd)
/// at least `offset + length` bytes long.
pub(crate) fn host_source(
    mut descriptors: Vec<std::os::fd::OwnedFd>,
    offset: u64,
    length: u64,
) -> Result<std::fs::File> {
    let (Some(descriptor), true) = (descriptors.pop(), descriptors.is_empty()) else {
        return Err(Error::InvalidRequest);
    };
    let file = std::fs::File::from(descriptor);
    let metadata = file.metadata().map_err(|_| Error::InvalidRequest)?;
    let end = offset.checked_add(length).ok_or(Error::InvalidRequest)?;
    if !metadata.file_type().is_file() || metadata.len() < end {
        return Err(Error::InvalidRequest);
    }
    Ok(file)
}

/// The resolved coordinator of `object`: the first ranked member of page 0.
async fn coordinator_node(
    local: &Coordinator,
    membership: &Arc<Membership>,
    object: &ObjectId,
    scope: &RequestScope,
) -> std::result::Result<NodeId, Reject> {
    ranked(local, membership, object, 0, scope)
        .await
        .ok()
        .and_then(|ranked| ranked.into_iter().next())
        .ok_or(Reject::busy(error_token::COORDINATOR))
}

fn membership(
    local: &Coordinator,
    cache: &CacheId,
) -> std::result::Result<Arc<Membership>, Reject> {
    local
        .cache_membership(cache)
        .map_err(|_| Reject::busy(error_token::OWNERS))
}

async fn session_action(
    local: &Coordinator,
    object: &ObjectId,
    etag: Option<crate::model::StrongEtag>,
    action: WriteAction,
    context: &OriginContext,
    scope: &RequestScope,
) -> std::result::Result<WriteReply, Reject> {
    let membership = membership(local, &object.cache)?;
    let coordinator = coordinator_node(local, &membership, object, scope).await?;
    let operation = PeerOperation::WriteSession {
        object: object.clone(),
        etag,
        action: Box::new(action),
    };
    send(local, &membership, &coordinator, operation, context, scope)
        .await
        .map_err(|error| match error {
            Error::DeadlineExceeded => Reject::new(504),
            _ => Reject::busy(error_token::COORDINATOR),
        })
}

async fn delete(
    local: &Coordinator,
    object: &ObjectId,
    context: &OriginContext,
    scope: &RequestScope,
) -> std::result::Result<(MessageHead, Outcome), Reject> {
    match session_action(local, object, None, WriteAction::Delete, context, scope).await? {
        WriteReply::Released => Ok((
            racer_wire::object::write::write_no_content_head(None),
            Outcome::Deleted,
        )),
        WriteReply::Status { .. } => Err(Reject::token(409, error_token::UPLOADED)),
        WriteReply::Unknown => Err(Reject::new(404)),
        _ => Err(Reject::busy(error_token::COORDINATOR)),
    }
}

async fn persist(
    local: &Coordinator,
    object: &ObjectId,
    context: &OriginContext,
    scope: &RequestScope,
) -> std::result::Result<(MessageHead, Outcome), Reject> {
    if context.metadata.is_none() {
        return Err(Reject::new(400));
    }
    match session_action(local, object, None, WriteAction::Persist, context, scope).await? {
        WriteReply::Committed { state } => Ok((
            racer_wire::object::write::write_no_content_head(Some(state)),
            Outcome::Persisted,
        )),
        WriteReply::Status { .. } => Err(Reject::token(409, error_token::NOT_MANUAL)),
        WriteReply::Unknown => Err(Reject::new(404)),
        _ => Err(Reject::busy(error_token::COORDINATOR)),
    }
}

/// Creation options from the request, before cache defaults.
struct Options {
    durability: Option<Durability>,
    upload: Option<UploadMode>,
    hold_seconds: Option<u64>,
    content_type: Option<crate::model::ContentType>,
    length: u64,
}

/// Where a creating write's bytes come from.
#[allow(clippy::large_enum_variant)] // One per request, on its stack.
enum Source<'a> {
    Body {
        connection: Option<ConnectionLease>,
        io: &'a HttpIo,
    },
    File {
        file: std::fs::File,
        offset: u64,
    },
}

/// Whether `metadata` names an object version (`versionId`), which a write
/// cannot create.
fn names_version(metadata: &[u8]) -> bool {
    match serde_json::from_slice::<serde_json::Value>(metadata) {
        Ok(serde_json::Value::Object(fields)) => fields.contains_key("versionId"),
        _ => false,
    }
}

/// Resolve request options against the cache's write settings.
fn spec(
    local: &Coordinator,
    object: &ObjectId,
    options: &Options,
    context: &OriginContext,
    page_size: PageSize,
) -> std::result::Result<WriteSpec, Reject> {
    let definition = local
        .availability
        .writes(&object.cache)
        .ok_or(Reject::new(405))?;
    let upload = options.upload.unwrap_or(definition.upload_mode);
    let durability = options
        .durability
        .unwrap_or(Durability::Replicas(definition.replicas));
    let replicas = match durability {
        Durability::Replicas(n) => n,
        Durability::Origin => definition.replicas,
    };
    if options.length == 0
        || (durability == Durability::Origin && upload != UploadMode::Auto)
        || (upload != UploadMode::Never && !super::native_origin(local, &object.cache))
    {
        return Err(Reject::new(400));
    }
    if upload != UploadMode::Never {
        match &context.metadata {
            None => return Err(Reject::new(400)),
            Some(metadata) if names_version(metadata.as_header()) => {
                return Err(Reject::new(400));
            }
            Some(_) => {}
        }
    }
    let hold = options
        .hold_seconds
        .map(Duration::from_secs)
        .unwrap_or(definition.hold_ttl)
        .min(Duration::from_secs(
            racer_wire::object::write::MAX_HOLD_SECONDS,
        ));
    let now = super::wall_now(local);
    let spec = WriteSpec {
        length: options.length,
        page_size: page_size.get(),
        content_type: options.content_type.as_ref().map(|c| c.as_str().to_owned()),
        replicas,
        durability,
        upload,
        hold_until_millis: crate::store::writes::unix_millis(now + hold),
    };
    spec.validate().map_err(|_| Reject::new(400))?;
    Ok(spec)
}

/// Everything one creating write shares across its pages.
struct Write<'a> {
    local: &'a Coordinator,
    membership: Arc<Membership>,
    version: ObjectVersion,
    spec: WriteSpec,
    session: SessionId,
    metadata: ObjectMetadata,
    store: WriteStore,
}

/// A page's placement: the targets that stored it, and why it could not be
/// placed on enough of them.
type Placed = (u64, Vec<NodeId>, Option<Reject>);

#[allow(clippy::too_many_arguments)]
async fn create(
    local: &Coordinator,
    object: &ObjectId,
    context: OriginContext,
    options: Options,
    mut source: Source<'_>,
    page_size: PageSize,
    scope: &RequestScope,
) -> (
    Option<ConnectionLease>,
    std::result::Result<(MessageHead, Outcome), Reject>,
) {
    let take = |source: &mut Source<'_>| match source {
        Source::Body { connection, .. } => connection.take(),
        Source::File { .. } => None,
    };
    let spec = match spec(local, object, &options, &context, page_size) {
        Ok(spec) => spec,
        Err(reject) => return (take(&mut source), Err(reject)),
    };
    let membership = match membership(local, &object.cache) {
        Ok(membership) => membership,
        Err(reject) => return (take(&mut source), Err(reject)),
    };
    let mut random = [0u8; 32];
    if local
        .fill
        .dependencies
        .candidates
        .ctx()
        .rng
        .fill(&mut random)
        .is_err()
    {
        return (take(&mut source), Err(Reject::new(500)));
    }
    let etag = match super::mint_etag(random[..16].try_into().expect("16 bytes")) {
        Ok(etag) => etag,
        Err(_) => return (take(&mut source), Err(Reject::new(500))),
    };
    let session = SessionId(random[16..].try_into().expect("16 bytes"));
    let version = ObjectVersion {
        object: object.clone(),
        etag: etag.clone(),
    };
    let coordinator = match coordinator_node(local, &membership, object, scope).await {
        Ok(node) => node,
        Err(reject) => return (take(&mut source), Err(reject)),
    };
    let open = PeerOperation::WriteSession {
        object: object.clone(),
        etag: Some(etag.clone()),
        action: Box::new(WriteAction::Open {
            session,
            spec: spec.clone(),
        }),
    };
    match send(local, &membership, &coordinator, open, &context, scope).await {
        Ok(WriteReply::Opened { .. }) => {}
        Ok(WriteReply::Exists) => return (take(&mut source), Err(Reject::new(412))),
        Ok(_) | Err(_) => {
            return (
                take(&mut source),
                Err(Reject::busy(error_token::COORDINATOR)),
            );
        }
    }
    let Ok(expires_at) = ExpiresAt::from_system_time(crate::store::writes::from_unix_millis(
        spec.hold_until_millis,
    )) else {
        return (take(&mut source), Err(Reject::new(500)));
    };
    let content_type = options.content_type.clone();
    let write = Write {
        local,
        membership: membership.clone(),
        version: version.clone(),
        session,
        metadata: ObjectMetadata {
            content_type: content_type.clone(),
            version: version.clone(),
            length: spec.length,
            expires_at,
        },
        store: WriteStore {
            session,
            pages: vec![0],
            length: spec.length,
            page_size,
            content_type,
            source: local.local_node(),
            hold_until_millis: spec.hold_until_millis,
        },
        spec,
    };
    let mut placed: Vec<Vec<NodeId>> = Vec::new();
    let result = write
        .stream(&mut source, &coordinator, &context, &mut placed, scope)
        .await;
    let result = match result {
        Ok(()) => write.commit(&coordinator, &context, &placed, scope).await,
        Err(reject) => Err(reject),
    };
    if result.is_err() {
        write.abort(&coordinator, &placed).await;
    } else {
        local
            .fill
            .metrics
            .writes()
            .bytes(&object.cache.0, write.spec.length);
    }
    let result = result.map(|state| {
        (
            racer_wire::object::write::write_created_head(&etag, state),
            Outcome::Created,
        )
    });
    (take(&mut source), result)
}

impl Write<'_> {
    /// Read, encrypt, stage, and place every page with at most the window in
    /// flight. `placed` collects each page's holders in page order.
    async fn stream(
        &self,
        source: &mut Source<'_>,
        coordinator: &NodeId,
        context: &OriginContext,
        placed: &mut Vec<Vec<NodeId>>,
        scope: &RequestScope,
    ) -> std::result::Result<(), Reject> {
        let mut slots: Vec<Vec<NodeId>> = vec![Vec::new(); self.spec.pages() as usize];
        let result = self
            .stream_into(source, coordinator, context, &mut slots, scope)
            .await;
        // Every started placement finished; its targets are known even when
        // the write failed, so an abort releases them.
        placed.extend(slots);
        result
    }

    async fn stream_into(
        &self,
        source: &mut Source<'_>,
        coordinator: &NodeId,
        context: &OriginContext,
        slots: &mut [Vec<NodeId>],
        scope: &RequestScope,
    ) -> std::result::Result<(), Reject> {
        let pages = self.spec.pages();
        let window = self.local.writes.settings().window_pages.max(1) as usize;
        let lease = self.local.writes.settings().lease;
        let clock = scope.clock().clone();
        let mut renewed = clock.now();
        let mut inflight: FuturesUnordered<Pin<Box<dyn Future<Output = Placed> + '_>>> =
            FuturesUnordered::new();
        let mut failure = None;
        for page in 0..pages {
            while inflight.len() >= window {
                if let Some(done) = inflight.next().await {
                    settle(done, &mut failure, slots);
                }
            }
            if failure.is_some() {
                break;
            }
            let length = self.page_length(page);
            let plaintext = match self.plaintext(length) {
                Ok(plaintext) => plaintext,
                Err(reject) => {
                    failure.get_or_insert(reject);
                    break;
                }
            };
            // Read the page while placed pages keep moving.
            let read = read_page(source, plaintext, page, self.spec.page_size, scope);
            let mut read = std::pin::pin!(read);
            let plaintext = std::future::poll_fn(|cx| {
                while let Poll::Ready(Some(done)) = inflight.poll_next_unpin(cx) {
                    settle(done, &mut failure, slots);
                }
                read.as_mut().poll(cx)
            })
            .await;
            let plaintext = match plaintext {
                Ok(plaintext) => plaintext,
                Err(_) => {
                    failure.get_or_insert(Reject::new(400));
                    break;
                }
            };
            inflight.push(Box::pin(self.place(page, plaintext, scope)));
            if clock.now().saturating_duration_since(renewed) >= lease / 3 {
                renewed = clock.now();
                if let Err(reject) = self.renew(coordinator, context, scope).await {
                    failure.get_or_insert(reject);
                    break;
                }
            }
        }
        while let Some(done) = inflight.next().await {
            settle(done, &mut failure, slots);
        }
        match failure {
            Some(reject) => Err(reject),
            None => Ok(()),
        }
    }

    fn page_length(&self, page: u64) -> usize {
        let start = page * self.spec.page_size;
        (self.spec.length - start).min(self.spec.page_size) as usize
    }

    fn plaintext(&self, length: usize) -> std::result::Result<PlaintextBuffer, Reject> {
        let deps = &self.local.fill.dependencies;
        let charge = deps
            .admission
            .reserve(
                Some(&self.version.object.cache),
                ResourceClass::Plaintext,
                length,
            )
            .map_err(|_| Reject::busy(error_token::DIRTY_CAP))?;
        deps.buffers
            .plaintext(charge, length)
            .map_err(|_| Reject::busy(error_token::DIRTY_CAP))
    }

    async fn renew(
        &self,
        coordinator: &NodeId,
        context: &OriginContext,
        scope: &RequestScope,
    ) -> std::result::Result<(), Reject> {
        let operation = PeerOperation::WriteSession {
            object: self.version.object.clone(),
            etag: Some(self.version.etag.clone()),
            action: Box::new(WriteAction::Renew {
                session: self.session,
            }),
        };
        match send(
            self.local,
            &self.membership,
            coordinator,
            operation,
            context,
            scope,
        )
        .await
        {
            Ok(WriteReply::Opened { .. }) => Ok(()),
            Ok(WriteReply::LeaseExpired) => Err(Reject::busy(error_token::LEASE)),
            _ => Err(Reject::busy(error_token::COORDINATOR)),
        }
    }

    /// Encrypt and stage one page, then place it on `replicas` ranked
    /// targets, trying deeper ranks when one cannot store it.
    async fn place(&self, page: u64, plaintext: PlaintextBuffer, scope: &RequestScope) -> Placed {
        let mut holders = Vec::new();
        let failure = self
            .place_inner(page, plaintext, &mut holders, scope)
            .await
            .err();
        (page, holders, failure)
    }

    async fn place_inner(
        &self,
        page: u64,
        plaintext: PlaintextBuffer,
        holders: &mut Vec<NodeId>,
        scope: &RequestScope,
    ) -> std::result::Result<(), Reject> {
        let deps = &self.local.fill.dependencies;
        let id = page_id(&self.version, page);
        let length = racer_io::ring::IoBuffer::bytes(&plaintext)
            .map_err(|_| Reject::new(500))?
            .len();
        let charge = deps
            .admission
            .reserve(
                Some(&self.version.object.cache),
                ResourceClass::Ciphertext,
                length + racer_crypto::TAG_LEN,
            )
            .map_err(|_| Reject::busy(error_token::DIRTY_CAP))?;
        let page_size = PageSize::from_wire(self.spec.page_size).map_err(|_| Reject::new(500))?;
        let (_verified, ciphertext) = deps
            .crypto
            .encrypt(id.clone(), page_size, plaintext, charge, scope)
            .await
            .map_err(reject_for)?;
        let staging = &self.local.prefetch_directory().write_staging;
        let _staged = super::staging::Staged::new(
            staging,
            CiphertextCopy {
                metadata: self.metadata.clone(),
                ciphertext,
            },
        );
        let ranked = ranked(
            self.local,
            &self.membership,
            &self.version.object,
            page,
            scope,
        )
        .await
        .map_err(|_| Reject::busy(error_token::OWNERS))?;
        let health = deps.candidates.link_health();
        let candidates: Vec<NodeId> = ranked
            .into_iter()
            .filter(|node| *node == self.local.local_node() || !health.silent(node))
            .take(RANKED_DEPTH)
            .collect();
        let replicas = usize::from(self.spec.replicas);
        let mut full = false;
        let first = candidates.len().min(replicas);
        let outcomes = futures::future::join_all(
            candidates[..first]
                .iter()
                .map(|node| self.store(node, page, scope)),
        )
        .await;
        for (node, outcome) in candidates[..first].iter().zip(outcomes) {
            match outcome {
                Ok(true) => holders.push(node.clone()),
                Ok(false) => full = true,
                Err(()) => {}
            }
        }
        for node in &candidates[first..] {
            if holders.len() >= replicas {
                break;
            }
            match self.store(node, page, scope).await {
                Ok(true) => holders.push(node.clone()),
                Ok(false) => full = true,
                Err(()) => {}
            }
        }
        scope.check().map_err(reject_for)?;
        if holders.len() < replicas {
            return Err(Reject::busy(if full {
                error_token::DIRTY_CAP
            } else {
                error_token::OWNERS
            }));
        }
        Ok(())
    }

    /// Ask `node` to store `page`: `Ok(true)` stored, `Ok(false)` full.
    async fn store(
        &self,
        node: &NodeId,
        page: u64,
        scope: &RequestScope,
    ) -> std::result::Result<bool, ()> {
        let operation = PeerOperation::WriteStore {
            version: self.version.clone(),
            store: Box::new(WriteStore {
                pages: vec![page],
                ..self.store.clone()
            }),
        };
        let context = super::bare_context(&self.version.object);
        let clock = scope.clock().clone();
        let mut attempt = 0;
        loop {
            let reply = send(
                self.local,
                &self.membership,
                node,
                Self::copy_store(&operation),
                &context,
                scope,
            )
            .await;
            match reply {
                Ok(WriteReply::Stored { pages }) if pages.contains(&page) => return Ok(true),
                Ok(WriteReply::Full) => return Ok(false),
                // A busy target or link (peer admission, connection limits):
                // back off briefly and ask again.
                Err(Error::Overloaded | Error::FlightFull) if attempt < BUSY_RETRIES => {
                    attempt += 1;
                    let Some(reactor) = self.local.writes.reactor() else {
                        return Err(());
                    };
                    let until =
                        clock.now() + (BUSY_WAIT * (1 << attempt.min(6))).min(BUSY_WAIT_MAX);
                    if reactor.sleep_until(until, scope).await.is_err() {
                        return Err(());
                    }
                }
                _ => return Err(()),
            }
        }
    }

    fn copy_store(operation: &PeerOperation) -> PeerOperation {
        match operation {
            PeerOperation::WriteStore { version, store } => PeerOperation::WriteStore {
                version: version.clone(),
                store: store.clone(),
            },
            _ => unreachable!("only write-store is retried"),
        }
    }

    /// Commit with every page's holders, then wait for the upload under
    /// `origin` durability. Returns the upload state for the `201`.
    async fn commit(
        &self,
        coordinator: &NodeId,
        context: &OriginContext,
        placed: &[Vec<NodeId>],
        scope: &RequestScope,
    ) -> std::result::Result<UploadState, Reject> {
        let holders = Holders(
            placed
                .iter()
                .map(|nodes| nodes.iter().map(|n| n.0.clone().into_bytes()).collect())
                .collect(),
        );
        let explicit = holders.to_bytes().map_err(|_| Reject::new(500))?.len();
        let action = if explicit <= MAX_EXPLICIT_HOLDER_BYTES {
            WriteAction::Commit {
                session: self.session,
                holders,
            }
        } else {
            let ranks = rank_holders(
                self.local,
                &self.membership,
                &self.version.object,
                &holders,
                scope,
            )
            .await
            .ok()
            .flatten()
            .filter(|ranks| ranks.to_bytes().len() <= MAX_RANKED_HOLDER_BYTES)
            .ok_or(Reject::busy(error_token::OWNERS))?;
            WriteAction::CommitRanked {
                session: self.session,
                ranks,
            }
        };
        let operation = PeerOperation::WriteSession {
            object: self.version.object.clone(),
            etag: Some(self.version.etag.clone()),
            action: Box::new(action),
        };
        let state = match send(
            self.local,
            &self.membership,
            coordinator,
            operation,
            context,
            scope,
        )
        .await
        {
            Ok(WriteReply::Committed { state }) => state,
            Ok(WriteReply::LeaseExpired) => return Err(Reject::busy(error_token::LEASE)),
            _ => return Err(Reject::busy(error_token::COORDINATOR)),
        };
        if self.spec.durability != Durability::Origin {
            return Ok(state);
        }
        // `origin` durability acknowledges only after Put Block List.
        loop {
            let operation = PeerOperation::WriteSession {
                object: self.version.object.clone(),
                etag: None,
                action: Box::new(WriteAction::Status {
                    wait_millis: STATUS_WAIT_MILLIS,
                }),
            };
            match send(
                self.local,
                &self.membership,
                coordinator,
                operation,
                context,
                scope,
            )
            .await
            {
                Ok(WriteReply::Status {
                    state: UploadState::Committed,
                    ..
                }) => return Ok(UploadState::Committed),
                Ok(WriteReply::Status {
                    state: UploadState::Failed,
                    ..
                }) => return Err(Reject::token(502, error_token::UPLOAD)),
                Ok(WriteReply::Status { .. }) => scope.check().map_err(reject_for)?,
                Err(Error::DeadlineExceeded) => return Err(Reject::new(504)),
                _ => return Err(Reject::busy(error_token::COORDINATOR)),
            }
        }
    }

    /// Release the pages this writer placed and abort the session, under a
    /// fresh scope: the request's may be canceled.
    async fn abort(&self, coordinator: &NodeId, placed: &[Vec<NodeId>]) {
        let Ok(scope) = background_scope(self.local) else {
            return;
        };
        let mut plan: std::collections::BTreeMap<&NodeId, Vec<u64>> = Default::default();
        for (page, nodes) in placed.iter().enumerate() {
            for node in nodes {
                plan.entry(node).or_default().push(page as u64);
            }
        }
        let context = super::bare_context(&self.version.object);
        for (node, pages) in plan {
            for chunk in pages.chunks(super::OPERATION_PAGES) {
                let operation = PeerOperation::WriteRelease {
                    version: self.version.clone(),
                    session: self.session,
                    pages: chunk.to_vec(),
                    keep: false,
                };
                let _ = send(
                    self.local,
                    &self.membership,
                    node,
                    operation,
                    &context,
                    &scope,
                )
                .await;
            }
        }
        let operation = PeerOperation::WriteSession {
            object: self.version.object.clone(),
            etag: Some(self.version.etag.clone()),
            action: Box::new(WriteAction::Abort {
                session: self.session,
            }),
        };
        let _ = send(
            self.local,
            &self.membership,
            coordinator,
            operation,
            &context,
            &scope,
        )
        .await;
    }
}

/// Record one placed page's targets, and the first failure.
fn settle(done: Placed, failure: &mut Option<Reject>, slots: &mut [Vec<NodeId>]) {
    let (page, holders, reject) = done;
    slots[page as usize] = holders;
    if let Some(reject) = reject {
        failure.get_or_insert(reject);
    }
}

/// Fill `plaintext` with page `page` from the request body or the file.
async fn read_page(
    source: &mut Source<'_>,
    mut plaintext: PlaintextBuffer,
    page: u64,
    page_size: u64,
    scope: &RequestScope,
) -> Result<PlaintextBuffer> {
    match source {
        Source::Body { connection, io } => {
            let lease = connection.take().ok_or(Error::Internal)?;
            let completed = io.read_body_exact(lease, plaintext, scope).await?;
            *connection = Some(completed.lease);
            Ok(completed.buffer)
        }
        Source::File { file, offset } => {
            use std::os::unix::fs::FileExt;
            let at = offset
                .checked_add(page * page_size)
                .ok_or(Error::InvalidRequest)?;
            let bytes = racer_io::ring::IoBuffer::bytes_mut(&mut plaintext)?;
            file.read_exact_at(bytes, at)
                .map_err(|_| Error::InvalidRequest)?;
            Ok(plaintext)
        }
    }
}
