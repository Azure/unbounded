//! Shared client/peer coordinator. Fresh admission, explicit version pins, and
//! page-zero bootstrap use the same metadata owner and Fill flights.
use super::{
    fill::Fill,
    flight::AcquisitionBudget,
    metadata::{BootstrapResult, MetadataService},
    range_stream::{RangeStream, RangeStreams},
};
use crate::{
    client::request::{ClientRequest, ReadKind},
    control::snapshot::SnapshotStore,
    error::{Error, Operation, Result},
    model::{
        context::{OriginContext, PeerOriginContext},
        identity::{AttemptId, ObjectId, ObjectVersion, PageId, PageNumber},
        metadata::{MetadataSelector, ObjectMetadata, VersionMetadata},
        range::{ByteRange, PAGE_BYTES, ResolvedRange},
    },
    peer::{
        server::LocalPageService,
        wire::{FetchMode, Operation as PeerOperation, PeerResponse, VerifiedRequest},
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
    metadata: Rc<MetadataService>,
    fill: Rc<Fill>,
    streams: Rc<RangeStreams>,
    credentials: Rc<CredentialCrypto>,
}
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
    pub fn new(
        snapshots: Rc<SnapshotStore>,
        metadata: Rc<MetadataService>,
        fill: Rc<Fill>,
        streams: Rc<RangeStreams>,
        credentials: Rc<CredentialCrypto>,
    ) -> Self {
        Self {
            snapshots,
            metadata,
            fill,
            streams,
            credentials,
        }
    }
    pub(crate) fn resolve_metadata<'a>(
        &'a self,
        selector: MetadataSelector,
        membership: MembershipLease,
        context: &'a OriginContext,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
    ) -> Operation<'a, ObjectMetadata> {
        self.metadata
            .resolve_with_budget(selector, membership, context, scope, budget)
    }
    pub(crate) fn bootstrap<'a>(
        &'a self,
        selector: MetadataSelector,
        membership: MembershipLease,
        context: &'a OriginContext,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
    ) -> Operation<'a, BootstrapResult> {
        self.metadata
            .bootstrap_with_budget(selector, membership, context, scope, budget)
    }
    pub(crate) fn acquire<'a>(
        &'a self,
        page: PageId,
        membership: MembershipLease,
        context: &'a OriginContext,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
    ) -> Operation<'a, super::fill::PageResult> {
        self.fill.acquire(page, membership, context, scope, budget)
    }
    pub(crate) fn retained_metadata<'a>(
        &'a self,
        version: &'a ObjectVersion,
        scope: &'a RequestScope,
    ) -> Operation<'a, Option<VersionMetadata>> {
        self.fill.retained_metadata(version, scope)
    }
    pub(crate) fn publish_metadata(&self, metadata: VersionMetadata) -> Result<()> {
        self.metadata.publish_version(metadata)
    }
    pub(crate) fn seal_context(
        &self,
        context: &OriginContext,
        attempt: AttemptId,
        scope: &RequestScope,
    ) -> Result<PeerOriginContext> {
        self.credentials.seal(context, attempt, scope)
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
        mut budget: AcquisitionBudget,
    ) -> Operation<'a, ReadResponse> {
        Box::pin(async move {
            scope.check()?;
            let membership = self.snapshots.current()?.membership.clone();
            let directory = self.streams.directory();
            let ClientRequest { kind, origin } = request;
            match kind {
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
                ReadKind::Bootstrap => {
                    let bootstrap = directory
                        .bootstrap_with_budget(
                            MetadataSelector::Fresh,
                            membership.clone(),
                            &origin,
                            scope,
                            &mut budget,
                        )
                        .await?;
                    match bootstrap {
                        BootstrapResult::Empty(metadata) => {
                            validate_metadata(&metadata, &origin.object, &MetadataSelector::Fresh)?;
                            if metadata.length != 0 {
                                return Err(Error::CorruptRecord);
                            }
                            Ok(ReadResponse {
                                metadata,
                                range: None,
                                body: None,
                            })
                        }
                        BootstrapResult::Page(page) => {
                            let metadata = page.metadata.clone();
                            validate_metadata(&metadata, &origin.object, &MetadataSelector::Fresh)?;
                            page.validate_for(&PageId {
                                version: metadata.version.clone(),
                                number: PageNumber(0),
                            })?;
                            let range = resolve_range(
                                ByteRange::Closed {
                                    first: 0,
                                    last: PAGE_BYTES - 1,
                                },
                                metadata.length,
                            )?;
                            let body = self.streams.open_with_budget(
                                metadata.clone(),
                                range,
                                origin,
                                membership,
                                scope.clone(),
                                budget,
                                Some(page),
                            )?;
                            Ok(ReadResponse {
                                metadata,
                                range: Some(range),
                                body: Some(body),
                            })
                        }
                    }
                }
                ReadKind::Pinned { etag, range } => {
                    let selector = MetadataSelector::Pinned(etag);
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
                    let range = resolve_range(range, metadata.length)?;
                    let body = self.streams.open_with_budget(
                        metadata.clone(),
                        range,
                        origin,
                        membership,
                        scope.clone(),
                        budget,
                        None,
                    )?;
                    Ok(ReadResponse {
                        metadata,
                        range: Some(range),
                        body: Some(body),
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
        self.read_with_budget(request, scope, default_budget(scope))
    }
}
impl LocalPageService for Coordinator {
    fn serve_peer<'a>(
        &'a self,
        verified: VerifiedRequest,
        scope: &'a RequestScope,
    ) -> Operation<'a, PeerResponse> {
        Box::pin(async move {
            scope.check()?;
            let request = verified.into_signed().request;
            if request.route.request != scope.request
                || request.origin.request != request.route.request
                || request.origin.attempt != request.route.attempt
            {
                return Err(Error::Unauthorized);
            }
            let object = match &request.operation {
                PeerOperation::Page { page, .. } => &page.version.object,
                PeerOperation::Metadata { object, .. } => object,
            };
            if object != &request.origin.object {
                return Err(Error::InvalidRequest);
            }
            let mut effective = scope.clone();
            effective.deadline.0 = effective.deadline.0.min(request.route.deadline.0);
            effective.check()?;
            // Copy-only never opens credentials and never invokes acquisition.
            // It does not need a live membership snapshot to serve retained bytes.
            let result = match request.operation {
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
                    let membership = self.snapshots.current()?.membership.clone();
                    if membership.version != request.route.membership {
                        return Err(Error::IncompatibleMembership);
                    }
                    let mut budget = inherited_budget(&request.route, &effective)?;
                    let context = self.open_context(request.origin)?;
                    match operation {
                        PeerOperation::Page {
                            page,
                            mode: FetchMode::Acquire,
                        } => self
                            .fill
                            .acquire(page.clone(), membership, &context, &effective, &mut budget)
                            .await
                            .and_then(|page_result| {
                                page_result.validate_for(&page)?;
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
                Err(error) => peer_error(error),
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
    #[test]
    fn remote_budget_charges_final_incoming_link_and_never_restores_attempts() {
        let now = std::time::Instant::now();
        let scope = RequestScope::new(
            crate::model::identity::RequestId([1; 16]),
            now + std::time::Duration::from_secs(60),
        )
        .unwrap();
        let mut route = crate::topology::paths::RouteBudget {
            membership: crate::model::identity::MembershipVersion(1),
            request: scope.request,
            attempt: AttemptId([2; 16]),
            destination: crate::model::identity::NodeId("destination".into()),
            visited: vec![crate::model::identity::NodeId("sender".into())],
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
    use crate::model::{
        identity::{CacheId, CacheKey, ObjectVersion, StrongEtag},
        metadata::ExpiresAt,
    };
    #[test]
    fn pinned_head_validation_never_accepts_current_version_substitution() {
        let object = ObjectId {
            cache: CacheId("cache".into()),
            key: CacheKey([0; 32]),
        };
        let metadata = ObjectMetadata {
            version: ObjectVersion {
                object: object.clone(),
                etag: StrongEtag::test_value("new"),
            },
            length: 0,
            expires_at: ExpiresAt(std::time::UNIX_EPOCH),
        };
        assert_eq!(
            validate_metadata(&metadata, &object, &MetadataSelector::Fresh),
            Ok(())
        );
        assert_eq!(
            validate_metadata(
                &metadata,
                &object,
                &MetadataSelector::Pinned(StrongEtag::test_value("old"))
            ),
            Err(Error::VersionUnavailable)
        );
    }
    #[test]
    fn unsatisfiable_range_reports_the_selected_versions_length() {
        assert!(matches!(
            resolve_range(ByteRange::From(4), 4),
            Err(Error::UnsatisfiableRangeWithLength(4))
        ));
        assert!(matches!(
            resolve_range(ByteRange::From(0), 0),
            Err(Error::UnsatisfiableRangeWithLength(0))
        ));
    }
    #[test]
    fn peer_failures_are_not_copy_misses() {
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
