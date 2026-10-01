//! Shared client/peer coordinator. Fresh admission, explicit version pins, and
//! page-zero bootstrap use the same metadata owner and Fill flights.
pub mod candidates;
pub mod dispatch;
pub mod drivers;
pub mod fill;
pub mod flight;
pub mod hedge;
pub mod metadata;
pub mod range_stream;
#[cfg(test)]
mod remote_tests;
pub mod subscription;

use self::{
    fill::Fill,
    flight::AcquisitionBudget,
    metadata::MetadataService,
    range_stream::{RangeStream, RangeStreams},
};
use crate::{
    client::{ClientRequest, ReadKind},
    control::snapshot::SnapshotStore,
    error::{Error, Operation, Result},
    model::{
        ByteRange, MetadataSelector, ObjectId, ObjectMetadata, OriginContext, PeerOriginContext,
        ResolvedRange,
    },
    peer::{
        protocol::{FetchMode, Operation as PeerOperation, PeerResponse, VerifiedRequest},
        server::LocalPageService,
    },
    runtime::deadline::RequestScope,
    security::credentials::{ChargedOriginContext, CredentialCrypto},
    topology::membership::MembershipLease,
};
use std::rc::Rc;

pub struct ReadResponse {
    pub metadata: ObjectMetadata,
    pub range: Option<ResolvedRange>,
    pub body: Option<RangeStream>,
}
pub trait ReadService {
    fn read<'a>(
        &'a self,
        request: ClientRequest,
        scope: &'a RequestScope,
    ) -> Operation<'a, ReadResponse>;
}
pub struct Coordinator {
    snapshots: Rc<SnapshotStore>,
    pub(super) metadata: Rc<MetadataService>,
    pub(super) fill: Rc<Fill>,
    streams: Rc<RangeStreams>,
    pub(super) credentials: Rc<CredentialCrypto>,
    availability: Rc<crate::control::availability::Availability>,
}
// Metadata/bootstrap allowance. Normal pinned client ranges admit bounded page
// acquisitions separately; this is not a ceiling on successful pages delivered.
pub(crate) fn default_budget(scope: &RequestScope) -> AcquisitionBudget {
    AcquisitionBudget::new(scope.deadline.0, 32, 96)
}
pub(crate) fn inherited_budget(
    route: &crate::topology::paths::RouteBudget,
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
    #[cfg(test)]
    pub(crate) fn hedge_owner(&self) -> Option<&std::sync::Arc<hedge::Hedges>> {
        self.fill.hedge_owner()
    }
    pub fn new(
        snapshots: Rc<SnapshotStore>,
        metadata: Rc<MetadataService>,
        fill: Rc<Fill>,
        streams: Rc<RangeStreams>,
        credentials: Rc<CredentialCrypto>,
        availability: Rc<crate::control::availability::Availability>,
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
    pub(crate) fn open_context(&self, envelope: PeerOriginContext) -> Result<ChargedOriginContext> {
        let request = envelope.request;
        let attempt = envelope.attempt;
        self.credentials.open_charged(envelope, request, attempt)
    }
    /// Explicit original-budget entry point for callers with tighter admission
    /// policies. Ownership passes to a returned body without creating new credits.
    pub fn read_with_budget<'a>(
        &'a self,
        request: ClientRequest,
        scope: &'a RequestScope,
        budget: AcquisitionBudget,
    ) -> Operation<'a, ReadResponse> {
        self.read_budgeted(request, scope, budget, false)
    }
    fn read_budgeted<'a>(
        &'a self,
        request: ClientRequest,
        scope: &'a RequestScope,
        mut budget: AcquisitionBudget,
        client_pages: bool,
    ) -> Operation<'a, ReadResponse> {
        Box::pin(async move {
            scope.check()?;
            let snapshot = self.snapshots.current()?;
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
            match kind {
                ReadKind::Subscription {
                    pin,
                    range,
                    page_credits,
                    byte_credits,
                    ordered,
                } => {
                    let selector = pin.map_or(MetadataSelector::Fresh, MetadataSelector::Pinned);
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
                    if metadata.length == 0 && range.is_none() {
                        return Ok(ReadResponse {
                            metadata,
                            range: None,
                            body: None,
                        });
                    }
                    let range =
                        resolve_range(range.unwrap_or(ByteRange::From(0)), metadata.length)?;
                    let mut body = if client_pages {
                        self.streams.open(
                            metadata.clone(),
                            range,
                            origin,
                            membership,
                            scope.clone(),
                        )?
                    } else {
                        self.streams.open_with_budget(
                            metadata.clone(),
                            range,
                            origin,
                            membership,
                            scope.clone(),
                            budget,
                        )?
                    };
                    body.configure_subscription(page_credits, byte_credits, ordered)?;
                    Ok(ReadResponse {
                        metadata,
                        range: Some(range),
                        body: Some(body),
                    })
                }
                ReadKind::Head | ReadKind::HeadPinned { .. } => {
                    let selector = match kind {
                        ReadKind::HeadPinned { etag } => MetadataSelector::Pinned(etag),
                        _ => MetadataSelector::Fresh,
                    };
                    let metadata = directory
                        .resolve_with_budget(
                            selector.clone(),
                            membership,
                            &origin,
                            scope,
                            &mut budget,
                        )
                        .await?;
                    validate_metadata(&metadata, &origin.object, &selector)?;
                    Ok(ReadResponse {
                        metadata,
                        range: None,
                        body: None,
                    })
                }
            }
        })
    }
}
impl ReadService for Coordinator {
    fn read<'a>(
        &'a self,
        request: ClientRequest,
        scope: &'a RequestScope,
    ) -> Operation<'a, ReadResponse> {
        self.read_budgeted(request, scope, default_budget(scope), true)
    }
}
impl LocalPageService for Coordinator {
    fn serve_peer<'a>(
        &'a self,
        verified: VerifiedRequest,
        membership: MembershipLease,
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
                            self.fill.record_peer_bootstrap()?;
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
fn validate_metadata(
    metadata: &ObjectMetadata,
    object: &ObjectId,
    selector: &MetadataSelector,
) -> Result<()> {
    if &metadata.version.object != object {
        return Err(Error::CorruptRecord);
    }
    if let MetadataSelector::Pinned(etag) = selector {
        if &metadata.version.etag != etag {
            return Err(Error::VersionUnavailable);
        }
    }
    Ok(())
}
fn resolve_range(range: ByteRange, length: u64) -> Result<ResolvedRange> {
    range.resolve(length).map_err(|error| match error {
        Error::UnsatisfiableRange => Error::UnsatisfiableRangeWithLength(length),
        other => other,
    })
}
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
mod tests {
    use super::*;
    use crate::model::AttemptId;
    #[test]
    fn remote_budget_charges_final_incoming_link_and_never_restores_attempts() {
        let now = crate::runtime::environment::now();
        let scope = RequestScope::new(
            crate::model::RequestId([1; 16]),
            now + std::time::Duration::from_secs(60),
        )
        .unwrap();
        let mut route = crate::topology::paths::RouteBudget {
            membership: crate::model::MembershipVersion(1),
            request: scope.request,
            attempt: AttemptId([2; 16]),
            destination: crate::model::NodeId("destination".into()),
            visited: vec![crate::model::NodeId("sender".into())],
            remaining_links: 1,
            remaining_attempts: 0,
            deadline: scope.deadline,
        };
        let mut budget = inherited_budget(&route, &scope).unwrap();
        assert_eq!(budget.remaining_links(), 0);
        assert_eq!(
            budget.begin_attempt(now, scope.deadline.0),
            Err(Error::Unavailable)
        );
        route.remaining_links = 8;
        route.remaining_attempts = 3;
        route.deadline.0 = now + std::time::Duration::from_secs(5);
        let mut budget = inherited_budget(&route, &scope).unwrap();
        assert_eq!(budget.route_links(), 7);
        assert_eq!(
            budget.begin_attempt(now, scope.deadline.0),
            Ok(route.deadline.0)
        );
        assert_eq!(budget.remaining_attempts(), 2);
        route.remaining_links = 0;
        assert!(matches!(
            inherited_budget(&route, &scope),
            Err(Error::HopBudgetExhausted)
        ));
    }
    #[test]
    fn peer_failures_are_not_copy_misses() {
        assert!(matches!(
            peer_error(Error::NotFound),
            Ok(PeerResponse::NotFound)
        ));
        assert!(matches!(
            peer_error(Error::VersionUnavailable),
            Ok(PeerResponse::VersionUnavailable)
        ));
        assert!(matches!(
            peer_error(Error::OriginRejected),
            Ok(PeerResponse::OriginRejected)
        ));
        assert!(matches!(
            peer_error(Error::Unauthorized),
            Err(Error::Unauthorized)
        ));
        assert!(matches!(
            peer_error(Error::OriginForbidden),
            Ok(PeerResponse::OriginForbidden)
        ));
        assert!(matches!(
            peer_error(Error::Overloaded),
            Ok(PeerResponse::Overloaded)
        ));
        assert!(matches!(
            peer_error(Error::CorruptRecord),
            Err(Error::CorruptRecord)
        ));
    }
}
