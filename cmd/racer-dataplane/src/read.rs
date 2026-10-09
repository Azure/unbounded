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
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::model::ByteRange;
use crate::model::MetadataSelector;
use crate::model::ObjectId;
use crate::model::ObjectMetadata;
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
use controlplane::Published;
use std::rc::Rc;

pub mod candidates;
pub mod dispatch;
pub mod fill;
pub mod flight;
pub mod metadata;
pub mod range_stream;

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
    snapshots: std::sync::Arc<Published<Snapshot>>,

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
    if route.remaining_links > 8 || route.remaining_links == 0 || route.visited.is_empty() {
        return Err(Error::HopBudgetExhausted);
    }
    let mut budget = AcquisitionBudget::new(
        scope.deadline.0.min(route.deadline.0),
        route.remaining_attempts,
        route
            .remaining_links
            .checked_sub(1)
            .ok_or(Error::HopBudgetExhausted)?,
    );
    if route.remaining_links > 4 {
        budget.note_route_failure();
    }
    Ok(budget)
}

impl Coordinator {
    /// Expose the fill's hedge owner for coordinator tests.
    #[cfg(test)]
    pub(crate) fn hedge_owner(&self) -> Option<&std::sync::Arc<candidates::Hedges>> {
        self.fill.hedge_owner()
    }

    /// Assemble worker-local services without transferring their reactor ownership.
    pub fn new(
        snapshots: std::sync::Arc<Published<Snapshot>>,
        metadata: Rc<MetadataService>,
        fill: Rc<Fill>,
        streams: Rc<RangeStreams>,
        credentials: Rc<CredentialCrypto>,
        availability: Rc<crate::control::Availability>,
    ) -> Self {
        Self {
            snapshots,
            metadata,
            fill,
            streams,
            credentials,
            availability,
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
            if !snapshot
                .caches
                .iter()
                .any(|c| c.id == request.origin.object.cache)
                || !self.availability.metadata(&request.origin.object.cache)
            {
                return Err(Error::Unavailable);
            }
            let membership = snapshot.membership.clone();
            let directory = self.streams.directory();
            let ClientRequest { kind, origin } = request;
            let selector = match &kind {
                ReadKind::Subscription { pin, .. } => pin
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
            match kind {
                ReadKind::Subscription {
                    range,
                    page_credits,
                    byte_credits,
                    ordered,
                    ..
                } => {
                    if metadata.length == 0 && range.is_none() {
                        return Ok(ReadResponse {
                            metadata,
                            payload: ReadPayload::Empty,
                        });
                    }
                    let range =
                        resolve_range(range.unwrap_or(ByteRange::From(0)), metadata.length)?;
                    let mut body = self.streams.open(
                        metadata.clone(),
                        range,
                        origin,
                        membership,
                        scope.clone(),
                    )?;
                    body.configure_subscription(page_credits, byte_credits, ordered)?;
                    Ok(ReadResponse {
                        metadata,
                        payload: ReadPayload::Stream(body),
                    })
                }
                ReadKind::Head | ReadKind::HeadPinned { .. } => Ok(ReadResponse {
                    metadata,
                    payload: ReadPayload::Empty,
                }),
            }
        })
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
            let request = verified.into_signed().request;
            crate::peer::check_membership(&request, &membership)?;
            if request.route.request != scope.request
                || request.origin.request != request.route.request
                || request.origin.attempt != request.route.attempt
            {
                return Err(Error::Unauthorized);
            }
            let object = match &request.operation {
                PeerOperation::Subscribe { .. } => return Err(Error::InvalidRequest),
                PeerOperation::Bootstrap { object, .. } => object,
                PeerOperation::Page { page, .. } => &page.version.object,
                PeerOperation::Metadata { object, .. } => object,
            };
            if object != &request.origin.object {
                return Err(Error::InvalidRequest);
            }
            if !self
                .snapshots
                .current()?
                .ok_or(Error::Unavailable)?
                .caches
                .iter()
                .any(|c| c.id == object.cache)
                || !self.availability.metadata(&object.cache)
            {
                return Ok(PeerResponse::Miss);
            }
            let mut effective = scope.clone();
            effective.deadline.0 = effective.deadline.0.min(request.route.deadline.0);
            effective.check()?;
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
                } => self
                    .fill
                    .copy_only(&page, &effective)
                    .await
                    .and_then(|copy| match copy {
                        Some((metadata, ciphertext)) => {
                            metadata.immutable().validate_page(ciphertext.envelope())?;
                            if ciphertext.envelope().page != page {
                                return Err(Error::CorruptRecord);
                            }
                            Ok(PeerResponse::Page {
                                metadata,
                                ciphertext,
                            })
                        }
                        None => Ok(PeerResponse::Miss),
                    }),
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
                        } => self
                            .fill
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
                            }),
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
                        _ => Err(Error::InvalidRequest),
                    }
                }
            };
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
fn resolve_range(range: ByteRange, length: u64) -> Result<ResolvedRange> {
    range
        .resolve(length)
        .map_err(|error| match Error::from(error) {
            Error::UnsatisfiableRange => Error::UnsatisfiableRangeWithLength(length),
            other => other,
        })
}

/// Translate expected acquisition failures without masking internal peer errors.
fn peer_error(error: Error) -> Result<PeerResponse> {
    match error {
        Error::NotFound => Ok(PeerResponse::NotFound),
        Error::VersionUnavailable => Ok(PeerResponse::VersionUnavailable),
        Error::Overloaded => Ok(PeerResponse::Overloaded),
        Error::OriginRejected => Ok(PeerResponse::OriginRejected),
        Error::OriginForbidden => Ok(PeerResponse::OriginForbidden),
        Error::Unavailable | Error::HopBudgetExhausted | Error::DeadlineExceeded => {
            Ok(PeerResponse::Unavailable)
        }
        error => Err(error),
    }
}

#[cfg(test)]
pub(crate) mod tests;
