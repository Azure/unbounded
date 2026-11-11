//! Shared client/peer coordinator. Fresh admission, explicit version pins, and
//! page-zero bootstrap use the same metadata owner and Fill flights.

use self::fill::Fill;
use self::flight::AcquisitionBudget;
use self::metadata::MetadataService;
use self::range_stream::RangeStream;
use self::range_stream::RangeStreams;
use crate::client::ClientRequest;
use crate::client::ReadKind;
use crate::control::Snapshot;
use crate::control::cell::PublicationCell;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::model::ByteRange;
use crate::model::MetadataSelector;
use crate::model::ObjectId;
use crate::model::ObjectMetadata;
use crate::model::PageSize;
use crate::model::ResolvedRange;
use crate::peer::forwarding::VerifiedRequest;
use crate::peer::protocol::FetchMode;
use crate::peer::protocol::Operation as PeerOperation;
use crate::peer::protocol::PeerResponse;
use crate::peer::server::LocalPageService;
use crate::runtime::RequestScope;
use crate::security::ChargedOriginContext;
use crate::security::CredentialCrypto;
use crate::security::OriginContext;
use crate::security::PeerOriginContext;
use std::rc::Rc;

pub mod candidates;
pub mod dispatch;
pub mod fill;
pub mod flight;
pub(crate) mod gpu_copies;
pub mod metadata;
pub mod range_stream;
pub mod sealed;
pub(crate) mod shared;
pub mod spill;

/// Metadata and its optional stream, with no independent range to disagree with it.
pub struct ReadResponse {
    pub metadata: ObjectMetadata,

    pub payload: ReadPayload,
}

/// A HEAD or empty-object response has no stream. A nonempty response carries
/// the stream's pinned range, so a range without a body cannot be constructed.
#[allow(clippy::large_enum_variant)] // Keep the reactor-local stream inline without another allocation.
pub enum ReadPayload {
    Empty,

    Stream(RangeStream),
}

impl ReadPayload {
    /// Return the admitted range, if this response carries object bytes.
    pub(crate) fn range(&self) -> Option<ResolvedRange> {
        match self {
            Self::Empty => None,
            Self::Stream(stream) => Some(stream.range()),
        }
    }
}

/// Coordinate client admission and peer acquisition on the current worker.
pub struct Coordinator {
    snapshots: std::sync::Arc<PublicationCell<Snapshot>>,

    pub(super) metadata: Rc<MetadataService>,

    pub(super) fill: Rc<Fill>,

    streams: Rc<RangeStreams>,

    pub(super) credentials: Rc<CredentialCrypto>,

    availability: Rc<crate::control::Availability>,
}

/// Metadata/bootstrap allowance. Normal pinned client ranges admit bounded page
/// acquisitions separately; this is not a ceiling on successful pages delivered.
pub(crate) fn default_budget(scope: &RequestScope) -> AcquisitionBudget {
    AcquisitionBudget::new(scope.deadline.0, 32, 96)
}

/// Inherit the peer's remaining route allowance without extending its deadline.
pub(crate) fn inherited_budget(
    route: &crate::topology::RouteBudget,
    scope: &RequestScope,
) -> Result<AcquisitionBudget> {
    scope.check()?;
    racer_core::read::budget::inherited_budget(
        scope.deadline.0,
        route.deadline.0,
        route.remaining_attempts,
        route.remaining_links,
        route.visited.len(),
    )
    .map_err(Error::from)
}

impl Coordinator {
    /// This worker's detached driver queue.
    pub(crate) fn drivers(&self) -> &Rc<crate::drivers::DriverQueue> {
        self.fill.dependencies.candidates.drivers()
    }

    /// Expose the fill's hedge owner for coordinator tests.
    #[cfg(test)]
    pub(crate) fn hedge_owner(&self) -> Option<&std::sync::Arc<candidates::Hedges>> {
        self.fill.hedge_owner()
    }

    /// In-flight GPU fetches and requests joined to them, for tests.
    #[cfg(test)]
    pub(crate) fn gpu_flights(&self) -> (usize, usize) {
        self.fill.gpu_copies.flights()
    }

    /// Assemble worker-local services without transferring their reactor ownership.
    pub fn new(
        snapshots: std::sync::Arc<PublicationCell<Snapshot>>,
        metadata: Rc<MetadataService>,
        fill: Rc<Fill>,
        streams: Rc<RangeStreams>,
        credentials: Rc<CredentialCrypto>,
        availability: Rc<crate::control::Availability>,
    ) -> Self {
        streams.directory().prefetch.attach_metrics(&fill.metrics);
        Self {
            snapshots,
            metadata,
            fill,
            streams,
            credentials,
            availability,
        }
    }

    /// The page's ranked owners under `membership`, or `None` when ranking
    /// fails (successor hints are best effort).
    async fn page_owners(
        &self,
        membership: std::sync::Arc<crate::topology::Membership>,
        page: &crate::model::PageId,
        scope: &RequestScope,
    ) -> Option<Vec<racer_wire::control::NodeId>> {
        let mut owners = self
            .fill
            .dependencies
            .candidates
            .candidates_scoped(membership, &page.version.object, page.number, scope)
            .await
            .ok()?
            .ordered;
        owners.truncate(3);
        Some(owners)
    }

    /// This node answered `requester`'s page probe without a copy: remember
    /// that `requester` is likely to fetch the page itself. A copy-only miss
    /// is noted without ranking (deferral checks ownership); a failed
    /// acquisition is noted when the ranking this node's own walk cached
    /// shows both as owners. Neither path ranks a page.
    fn note_successor(
        &self,
        page: &crate::model::PageId,
        requester: &racer_wire::control::NodeId,
        membership: &crate::topology::Membership,
        copy_only: bool,
    ) {
        // Successor hints assume the successor keeps its copy.
        if self.fill.passthrough(&page.version.object.cache) {
            return;
        }
        let candidates = &self.fill.dependencies.candidates;
        let local = &candidates.node;
        let owners = || {
            candidates.owns_cached(membership, page, local)
                && candidates.owns_cached(membership, page, requester)
        };
        if local == requester || !(copy_only || owners()) {
            return;
        }
        self.fill
            .dependencies
            .candidates
            .successors
            .borrow_mut()
            .note(
                page.clone(),
                requester.clone(),
                self.fill.dependencies.candidates.ctx().now(),
            );
        self.fill
            .metrics
            .record(crate::telemetry::Event::SuccessorHint, 1);
    }

    /// Whether to answer a non-owner's page `Acquire` with `Miss` because
    /// another owner fetched the page after this node answered it without a
    /// copy. Free when no hint names the page.
    async fn defer_to_successor(
        &self,
        page: &crate::model::PageId,
        requester: &racer_wire::control::NodeId,
        membership: std::sync::Arc<crate::topology::Membership>,
        scope: &RequestScope,
    ) -> bool {
        use racer_core::read::hints::{MAX_DEFERRALS, defer_allowed, successor_deferral};
        if self.fill.passthrough(&page.version.object.cache) {
            return false;
        }
        if !defer_allowed(self.fill.dependencies.flights.acquiring(page)) {
            return false;
        }
        let hints = self
            .fill
            .dependencies
            .candidates
            .successors
            .borrow()
            .peek_for(
                page,
                requester,
                self.fill.dependencies.candidates.ctx().now(),
            );
        if hints.is_empty() {
            return false;
        }
        let Some(owners) = self.page_owners(membership, page, scope).await else {
            return false;
        };
        let local = &self.fill.dependencies.candidates.node;
        let Some(to) = successor_deferral(hints, local, requester, &owners) else {
            return false;
        };
        self.fill
            .dependencies
            .candidates
            .successors
            .borrow_mut()
            .charge(
                page,
                requester.clone(),
                &to,
                MAX_DEFERRALS,
                self.fill.dependencies.candidates.ctx().now(),
            );
        self.fill
            .metrics
            .record(crate::telemetry::Event::SuccessorDeferral, 1);
        true
    }

    /// Whether a copy-only page request from `requester` may wait on this
    /// node's running acquisition of the page
    /// (`racer_core::read::copy_join_allowed`). Ranks
    /// the page only while an acquisition runs.
    async fn copy_join_allowed(
        &self,
        page: &crate::model::PageId,
        requester: &racer_wire::control::NodeId,
        membership: std::sync::Arc<crate::topology::Membership>,
        scope: &RequestScope,
    ) -> bool {
        if !self.fill.dependencies.flights.acquiring(page) {
            return true;
        }
        let Some(owners) = self.page_owners(membership, page, scope).await else {
            return true;
        };
        let local = &self.fill.dependencies.candidates.node;
        let allowed = racer_core::read::copy_join_allowed(&owners, local, requester);
        if !allowed {
            self.fill
                .metrics
                .record(crate::telemetry::Event::CopyJoinDeclined, 1);
        }
        allowed
    }

    /// Send this node's due copy offers; call once per
    /// worker service turn.
    pub(crate) fn poll_offers(&self, now: std::time::Instant) {
        let fill = &self.fill;
        fill.dependencies
            .candidates
            .poll_offers(now, |page| fill.holds(page), &fill.metrics);
        // Silence pings share the turn.
        fill.dependencies.candidates.poll_pings(now, &fill.metrics);
    }

    /// Answer a copy offer: `requester` fetched `page` from
    /// origin as an owner ranked after this node. Note it as a successor
    /// hint for this node's own next acquisition (it defers no readers,
    /// `SuccessorHints::note_offer`), unless this node holds the page or is
    /// fetching it. Never opens credentials, acquires, or sends a
    /// body.
    fn serve_offer(
        &self,
        page: &crate::model::PageId,
        requester: &racer_wire::control::NodeId,
    ) -> Result<PeerResponse> {
        let holds = self.fill.holds(page);
        let acquiring = self.fill.dependencies.flights.acquiring(page);
        if &self.fill.dependencies.candidates.node != requester
            && self
                .fill
                .dependencies
                .candidates
                .successors
                .borrow_mut()
                .note_offer(
                    page.clone(),
                    requester.clone(),
                    holds,
                    acquiring,
                    self.fill.dependencies.candidates.ctx().now(),
                )
        {
            self.fill
                .metrics
                .record(crate::telemetry::Event::SuccessorHint, 1);
            self.fill
                .metrics
                .record(crate::telemetry::Event::OrphanOfferNoted, 1);
        }
        Ok(PeerResponse::Miss)
    }

    /// Serve a page copy-only: a local copy or `Miss`, never acquisition.
    /// `join` lets it wait on a running acquisition of the page.
    async fn serve_page_copy(
        &self,
        page: &crate::model::PageId,
        scope: &RequestScope,
        join: bool,
    ) -> Result<PeerResponse> {
        match self.fill.copy_only_joining(page, scope, join).await? {
            Some((metadata, ciphertext)) => {
                metadata.immutable().validate_page(ciphertext.envelope())?;
                if &ciphertext.envelope().page != page {
                    return Err(Error::CorruptRecord);
                }
                Ok(PeerResponse::Page {
                    metadata,
                    ciphertext,
                })
            }
            None => Ok(PeerResponse::Miss),
        }
    }

    /// Authenticate and charge the exact peer request and attempt context.
    pub(crate) fn open_context(&self, envelope: PeerOriginContext) -> Result<ChargedOriginContext> {
        let request = envelope.request;
        let attempt = envelope.attempt;
        self.credentials.open_charged(envelope, request, attempt)
    }

    /// Resolve admission metadata before opening a stream with independent page budgets.
    fn read_budgeted<'a>(
        &'a self,
        request: ClientRequest,
        scope: &'a RequestScope,
        mut budget: AcquisitionBudget,
    ) -> Operation<'a, ReadResponse> {
        Box::pin(async move {
            scope.check()?;
            let snapshot = self.snapshots.current()?.ok_or(Error::Unavailable)?;
            if !snapshot.caches.contains(&request.origin.object.cache)
                || !self.availability.metadata(&request.origin.object.cache)
            {
                return Err(Error::Unavailable);
            }
            let membership = snapshot.membership.clone();
            let directory = self.streams.directory();
            let ClientRequest { kind, origin } = request;
            let selector = match &kind {
                ReadKind::Subscription { pin, .. }
                | ReadKind::HostRead { pin, .. }
                | ReadKind::GpuRead { pin, .. } => pin
                    .clone()
                    .map_or(MetadataSelector::Fresh, MetadataSelector::Pinned),
                ReadKind::Head => MetadataSelector::Fresh,
                ReadKind::HeadPinned { etag } => MetadataSelector::Pinned(etag.clone()),
            };
            let metadata = directory
                .resolve_with_budget(
                    selector.clone(),
                    membership.clone(),
                    &origin,
                    scope,
                    &mut budget,
                )
                .await?;
            validate_metadata(&metadata, &origin.object, &selector)?;
            // Direct reads have no client credits. The dataplane paces them
            // with its own window, at least `range_window_pages`, and releases
            // each page after writing it.
            let direct_window = crate::client::DIRECT_WINDOW_PAGES.max(self.streams.window_pages());
            let kind = match kind {
                ReadKind::HostRead { range, .. } | ReadKind::GpuRead { range, .. } => {
                    ReadKind::Subscription {
                        pin: None,
                        range,
                        page_credits: direct_window,
                        byte_credits: direct_byte_credits(direct_window, snapshot.page_size),
                    }
                }
                kind => kind,
            };
            match kind {
                ReadKind::Subscription {
                    range,
                    page_credits,
                    byte_credits,
                    ..
                } => {
                    if metadata.length == 0 && range.is_none() {
                        return Ok(ReadResponse {
                            metadata,
                            payload: ReadPayload::Empty,
                        });
                    }
                    let range = resolve_range(
                        range.unwrap_or(ByteRange::From(0)),
                        metadata.length,
                        snapshot.page_size,
                    )?;
                    let passthrough = self.fill.passthrough(&origin.object.cache);
                    let mut body = self.streams.open(
                        metadata.clone(),
                        range,
                        origin,
                        membership,
                        scope.clone(),
                    )?;
                    if passthrough {
                        body.set_passthrough();
                    }
                    body.configure_subscription(page_credits, byte_credits)?;
                    Ok(ReadResponse {
                        metadata,
                        payload: ReadPayload::Stream(body),
                    })
                }
                ReadKind::Head | ReadKind::HeadPinned { .. } => Ok(ReadResponse {
                    metadata,
                    payload: ReadPayload::Empty,
                }),
                // Mapped to Subscription above.
                ReadKind::HostRead { .. } | ReadKind::GpuRead { .. } => Err(Error::Internal),
            }
        })
    }

    /// The node's worker directory, which owns the prefetch registry.
    pub(crate) fn prefetch_directory(&self) -> &std::rc::Rc<dispatch::WorkerDirectory> {
        self.streams.directory()
    }

    /// The current membership, if a snapshot is published.
    pub(crate) fn current_membership(&self) -> Option<std::sync::Arc<crate::topology::Membership>> {
        self.snapshots
            .current()
            .ok()
            .flatten()
            .map(|snapshot| snapshot.membership.clone())
    }

    /// The membership for work on `cache`, or `Unavailable` when the cache is
    /// not published or not ready.
    pub(crate) fn cache_membership(
        &self,
        cache: &racer_wire::control::CacheId,
    ) -> Result<std::sync::Arc<crate::topology::Membership>> {
        let snapshot = self.snapshots.current()?.ok_or(Error::Unavailable)?;
        // Only the cache's pool members place, fill and track its pages.
        if !snapshot.caches.contains(cache)
            || !self.availability.metadata(cache)
            || !snapshot.membership.serves(cache, &self.local_node())
        {
            return Err(Error::Unavailable);
        }
        Ok(snapshot.membership.clone())
    }

    /// The page size the cluster publication fixed; unavailable before one is adopted.
    pub(crate) fn page_size(&self) -> Result<PageSize> {
        Ok(self
            .snapshots
            .current()?
            .ok_or(Error::Unavailable)?
            .page_size)
    }

    pub(crate) fn cache_passthrough(&self, cache: &racer_wire::control::CacheId) -> bool {
        self.fill.passthrough(cache)
    }

    pub(crate) fn local_node(&self) -> racer_wire::control::NodeId {
        self.fill.dependencies.candidates.node.clone()
    }

    /// The page's rank-0 owner within its cache's pool, the same ranking reads use.
    pub(crate) async fn rank_owner(
        &self,
        membership: std::sync::Arc<crate::topology::Membership>,
        object: &ObjectId,
        page: u64,
        scope: &RequestScope,
    ) -> Result<Option<racer_wire::control::NodeId>> {
        Ok(self
            .fill
            .dependencies
            .candidates
            .candidates_scoped(membership, object, crate::model::PageNumber(page), scope)
            .await?
            .ordered
            .first()
            .cloned())
    }

    /// Send one `prefetch` operation directly to `owner`.
    pub(crate) async fn peer_prefetch(
        &self,
        membership: &std::sync::Arc<crate::topology::Membership>,
        owner: &racer_wire::control::NodeId,
        context: &OriginContext,
        operation: &PeerOperation,
        scope: &RequestScope,
    ) -> Result<PeerResponse> {
        let mut budget = default_budget(scope);
        let response = self
            .fill
            .dependencies
            .candidates
            .request(
                membership,
                owner,
                context,
                operation,
                FetchMode::Acquire,
                scope,
                &mut budget,
                1,
            )
            .await?;
        Ok(match response.response() {
            PeerResponse::Prefetch(results) => PeerResponse::Prefetch(results.clone()),
            PeerResponse::Overloaded => PeerResponse::Overloaded,
            PeerResponse::FlightFull => PeerResponse::FlightFull,
            PeerResponse::Unavailable => PeerResponse::Unavailable,
            PeerResponse::Miss => PeerResponse::Miss,
            PeerResponse::StaleMembership => PeerResponse::StaleMembership,
            PeerResponse::VersionUnavailable => PeerResponse::VersionUnavailable,
            PeerResponse::NotFound => PeerResponse::NotFound,
            PeerResponse::OriginRejected => PeerResponse::OriginRejected,
            PeerResponse::OriginForbidden => PeerResponse::OriginForbidden,
            _ => return Err(Error::CorruptRecord),
        })
    }

    /// Resolve a version through the normal metadata path.
    pub(crate) async fn resolve_version(
        &self,
        selector: MetadataSelector,
        membership: std::sync::Arc<crate::topology::Membership>,
        context: &OriginContext,
        scope: &RequestScope,
    ) -> Result<crate::model::VersionMetadata> {
        let mut budget = default_budget(scope);
        let metadata = self
            .streams
            .directory()
            .resolve_with_budget(selector.clone(), membership, context, scope, &mut budget)
            .await?;
        validate_metadata(&metadata, &context.object, &selector)?;
        Ok(metadata.immutable())
    }

    /// Admit a client read using the default metadata/bootstrap allowance.
    pub fn read<'a>(
        &'a self,
        request: ClientRequest,
        scope: &'a RequestScope,
    ) -> Operation<'a, ReadResponse> {
        self.read_budgeted(request, scope, default_budget(scope))
    }
}

impl LocalPageService for Coordinator {
    fn serve_peer<'a>(
        &'a self,
        verified: VerifiedRequest,
        membership: std::sync::Arc<crate::topology::Membership>,
        scope: &'a RequestScope,
    ) -> Operation<'a, PeerResponse> {
        Box::pin(async move {
            scope.check()?;
            let requester = verified.origin().node().clone();
            let request = verified.into_signed().request;
            crate::peer::check_membership(&request, &membership)?;
            if request.route.request != scope.request
                || request.origin.request != request.route.request
                || request.origin.attempt != request.route.attempt
            {
                return Err(Error::Unauthorized);
            }
            if let PeerOperation::Page {
                page,
                mode: FetchMode::Offer,
            } = &request.operation
            {
                if page.version.object != request.origin.object {
                    return Err(Error::InvalidRequest);
                }
                return self.serve_offer(page, &requester);
            }
            if let PeerOperation::Page {
                page,
                mode: FetchMode::Ping,
            } = &request.operation
            {
                // A silence ping: answered at once,
                // never waiting on the cache or a flight.
                if page.version.object != request.origin.object {
                    return Err(Error::InvalidRequest);
                }
                self.fill
                    .metrics
                    .record(crate::telemetry::Event::SilencePingAnswered, 1);
                return Ok(PeerResponse::Miss);
            }
            let object = match &request.operation {
                PeerOperation::Subscribe { .. } => None,
                PeerOperation::Bootstrap { object, .. } => Some(object),
                PeerOperation::Page { page, .. } => Some(&page.version.object),
                PeerOperation::Metadata { object, .. } => Some(object),
                PeerOperation::Prefetch { version, .. } => Some(&version.object),
            };
            let object_matches = object.is_some_and(|object| object == &request.origin.object);
            use racer_core::read::{ServeAction, ServeFacts, serve_action};
            let facts = |cache_available| ServeFacts {
                operation: candidates::operation_kind(&request.operation),
                copy_only: matches!(
                    request.operation,
                    PeerOperation::Bootstrap {
                        mode: FetchMode::CopyOnly,
                        ..
                    } | PeerOperation::Page {
                        mode: FetchMode::CopyOnly,
                        ..
                    } | PeerOperation::Metadata {
                        mode: FetchMode::CopyOnly,
                        ..
                    } | PeerOperation::Subscribe {
                        mode: FetchMode::CopyOnly,
                        ..
                    }
                ),
                object_matches,
                cache_available,
            };
            // racer-core dispatches. Malformed requests are rejected before
            // consulting cache availability.
            if let ServeAction::Reject(error) = serve_action(facts(true)) {
                return Err(error.into());
            }
            let object = object.ok_or(Error::InvalidRequest)?;
            // `metadata` checks cache admission against the current publication.
            let cache_available = self.availability.metadata(&object.cache);
            let action = serve_action(facts(cache_available));
            if action == ServeAction::Miss {
                return Ok(PeerResponse::Miss);
            }
            let mut effective = scope.clone();
            effective.deadline.0 =
                racer_core::read::serve_deadline(effective.deadline.0, request.route.deadline.0);
            effective.check()?;
            // A non-owner's Acquire for a page another owner fetched after
            // this node answered it without a copy is served copy-only: the
            // reader asks that owner next instead of this node fetching the
            // page again.
            if let PeerOperation::Page {
                page,
                mode: FetchMode::Acquire,
            } = &request.operation
                && self
                    .defer_to_successor(page, &requester, membership.clone(), &effective)
                    .await
            {
                return self.serve_page_copy(page, &effective, true).await;
            }
            // Answers to a page probe without a copy are noted below.
            let noted = match &request.operation {
                PeerOperation::Page { page, mode } => {
                    Some((page.clone(), matches!(mode, FetchMode::CopyOnly)))
                }
                _ => None,
            };
            let noted_membership = membership.clone();
            let noted_requester = requester.clone();
            // Copy-only never opens credentials and never invokes acquisition.
            // The ingress lease is still retained while serving those bytes.
            let result = match request.operation {
                PeerOperation::Bootstrap {
                    object,
                    mode: FetchMode::CopyOnly,
                } => {
                    let context = OriginContext {
                        object,
                        metadata: None,
                        authorization: None,
                    };
                    self.metadata.bootstrap_copy(&context, &effective).await
                }
                PeerOperation::Page {
                    page,
                    mode: FetchMode::CopyOnly,
                } => {
                    let join = self
                        .copy_join_allowed(&page, &requester, membership.clone(), &effective)
                        .await;
                    self.serve_page_copy(&page, &effective, join).await
                }
                PeerOperation::Metadata {
                    object,
                    selector,
                    mode: FetchMode::CopyOnly,
                } => {
                    let context = OriginContext {
                        object: object.clone(),
                        metadata: None,
                        authorization: None,
                    };
                    self.metadata
                        .copy_only(selector.clone(), &context, &effective)
                        .await
                        .and_then(|value| match value {
                            Some(metadata) => {
                                validate_metadata(&metadata, &object, &selector)?;
                                Ok(PeerResponse::Metadata(metadata))
                            }
                            None => Ok(PeerResponse::Miss),
                        })
                }
                operation => {
                    debug_assert_eq!(action, ServeAction::Acquire);
                    let mut budget = inherited_budget(&request.route, &effective)?;
                    let context = self.open_context(request.origin)?;
                    match operation {
                        PeerOperation::Bootstrap {
                            mode: FetchMode::Acquire,
                            ..
                        } => {
                            self.fill
                                .metrics
                                .record(crate::telemetry::Event::PeerBootstrap, 1);
                            self.metadata
                                .bootstrap_peer(membership, &context, &effective, &mut budget)
                                .await
                        }
                        PeerOperation::Page {
                            page,
                            mode: FetchMode::Acquire,
                        } => {
                            // A remote demand read joining a prefetch flight
                            // promotes it.
                            self.streams.directory().prefetch.promote(&page);
                            self.fill
                                .acquire_ciphertext(
                                    page.clone(),
                                    membership,
                                    &context,
                                    &effective,
                                    &mut budget,
                                )
                                .await
                                .and_then(|page_result| {
                                    page_result.validate_metadata()?;
                                    Ok(PeerResponse::Page {
                                        metadata: page_result.metadata,
                                        ciphertext: page_result.ciphertext,
                                    })
                                })
                        }
                        PeerOperation::Metadata {
                            object,
                            selector,
                            mode: FetchMode::Acquire,
                        } => self
                            .metadata
                            .resolve_with_budget(
                                selector.clone(),
                                membership,
                                &context,
                                &effective,
                                &mut budget,
                            )
                            .await
                            .and_then(|metadata| {
                                validate_metadata(&metadata, &object, &selector)?;
                                Ok(PeerResponse::Metadata(metadata))
                            }),
                        PeerOperation::Prefetch { version, action } => {
                            crate::prefetch::service::serve(
                                self, version, action, membership, &context, &effective,
                            )
                            .await
                        }
                        _ => Err(Error::InvalidRequest),
                    }
                }
            };
            if let Some((page, copy_only)) = noted {
                let missed = match &result {
                    Ok(PeerResponse::Miss) => copy_only,
                    Err(_) => !copy_only,
                    Ok(_) => false,
                };
                if missed {
                    self.note_successor(&page, &noted_requester, &noted_membership, copy_only);
                }
            }
            match result {
                Ok(response) => Ok(response),
                Err(error) => {
                    self.fill
                        .observe_peer_error(&effective, request.route.attempt, error);
                    peer_error(error)
                }
            }
        })
    }
}

/// Combine exact immutable descriptors without replacing a valid value on failure.
fn merge_metadata(
    found: &mut Option<crate::model::VersionMetadata>,
    descriptor: crate::model::VersionMetadata,
    version: &crate::model::ObjectVersion,
) -> Result<()> {
    if &descriptor.version != version
        || found
            .as_ref()
            .is_some_and(|old| !old.compatible(&descriptor))
    {
        return Err(Error::CorruptRecord);
    }
    *found = Some(descriptor);
    Ok(())
}

/// Reject a different object or an unavailable explicit version pin.
fn validate_metadata(
    metadata: &ObjectMetadata,
    object: &ObjectId,
    selector: &MetadataSelector,
) -> Result<()> {
    if &metadata.version.object != object {
        return Err(Error::CorruptRecord);
    }
    if let MetadataSelector::Pinned(etag) = selector
        && &metadata.version.etag != etag
    {
        return Err(Error::VersionUnavailable);
    }
    Ok(())
}

/// Preserve the object's length when translating an unsatisfiable client range.
/// Byte credits for a direct read's `window` pages at the published page
/// size, so a small-page cluster does not grant 16 MiB per page.
fn direct_byte_credits(window: usize, page: PageSize) -> u64 {
    (window as u64).saturating_mul(page.get())
}

fn resolve_range(range: ByteRange, length: u64, page: PageSize) -> Result<ResolvedRange> {
    range
        .resolve_paged(length, page)
        .map_err(|error| match Error::from(error) {
            Error::UnsatisfiableRange => Error::UnsatisfiableRangeWithLength(length),
            other => other,
        })
}

/// Translate expected acquisition failures without masking internal peer errors.
fn peer_error(error: Error) -> Result<PeerResponse> {
    use racer_core::read::ResponseKind;
    let kind = racer_core::read::Error::try_from(error)
        .ok()
        .and_then(racer_core::read::peer_error);
    Ok(match kind {
        Some(ResponseKind::NotFound) => PeerResponse::NotFound,
        Some(ResponseKind::VersionUnavailable) => PeerResponse::VersionUnavailable,
        Some(ResponseKind::Overloaded) => PeerResponse::Overloaded,
        Some(ResponseKind::FlightFull) => PeerResponse::FlightFull,
        Some(ResponseKind::OriginRejected) => PeerResponse::OriginRejected,
        Some(ResponseKind::OriginForbidden) => PeerResponse::OriginForbidden,
        Some(ResponseKind::Unavailable) => PeerResponse::Unavailable,
        _ => return Err(error),
    })
}

#[cfg(test)]
pub(crate) mod tests;
