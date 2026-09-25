//! Ranked acquisition and copy-only predecessor probes. Only a validated local
//! candidate can mint origin authority; request headers never change placement.
use super::flight::AcquisitionBudget;
use crate::{
    error::{Error, Operation, Result},
    model::{
        context::OriginContext,
        identity::{AttemptId, NodeId, ObjectId, PageNumber},
        metadata::MetadataSelector,
    },
    peer::{
        requester::PeerClient,
        wire::{
            FetchMode, Operation as PeerOperation, PeerRequest, PeerResponse, VerifiedResponse,
        },
    },
    runtime::deadline::{Deadline, RequestScope},
    security::credentials::CredentialCrypto,
    topology::{
        membership::MembershipLease,
        paths::RouteBudget,
        placement::{Candidates, Placement},
    },
};
use std::{cell::RefCell, rc::Rc, time::Instant};

pub struct OriginAuthority {
    membership: MembershipLease,
    node: NodeId,
    object: ObjectId,
    page: PageNumber,
    predecessor_evidence: Vec<ProbeOutcome>,
    rank: usize,
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ProbeOutcome {
    CopyMiss,
    Unreachable,
    Overloaded,
    VersionUnavailable,
}
pub struct CandidatePolicy {
    node: NodeId,
    placement: Rc<Placement>,
    peers: Rc<dyn PeerClient>,
    credentials: RefCell<Option<Rc<CredentialCrypto>>>,
}
impl CandidatePolicy {
    pub fn new(node: NodeId, placement: Rc<Placement>, peers: Rc<dyn PeerClient>) -> Self {
        Self {
            node,
            placement,
            peers,
            credentials: RefCell::new(None),
        }
    }

    /// Composition hook: shares the same admission and credential domain as Fill.
    pub fn set_credentials(&self, credentials: Rc<CredentialCrypto>) {
        *self.credentials.borrow_mut() = Some(credentials);
    }

    pub fn candidates(
        &self,
        membership: MembershipLease,
        object: &ObjectId,
        page: PageNumber,
    ) -> Result<Candidates> {
        self.placement.rank(membership, object, page)
    }

    pub fn is_candidate(&self, candidates: &Candidates) -> bool {
        candidates
            .ordered
            .iter()
            .take(3)
            .any(|node| node == &self.node)
    }
    pub fn candidates_async<'a>(
        &'a self,
        membership: MembershipLease,
        object: &ObjectId,
        page: PageNumber,
    ) -> Operation<'a, Candidates> {
        self.placement.rank_async(membership, object, page)
    }

    pub fn resolve<'a>(
        &'a self,
        candidates: Candidates,
        context: &'a OriginContext,
        operation: PeerOperation,
        scope: &'a RequestScope,
    ) -> Operation<'a, CandidateResolution> {
        Box::pin(async move {
            let mut budget = super::serve::default_budget(scope);
            self.resolve_with_budget(candidates, context, operation, scope, &mut budget)
                .await
        })
    }

    pub fn resolve_with_budget<'a>(
        &'a self,
        candidates: Candidates,
        context: &'a OriginContext,
        operation: PeerOperation,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
    ) -> Operation<'a, CandidateResolution> {
        Box::pin(async move {
            scope.check()?;
            let (object, page) = operation_identity(&operation);
            if object != &context.object {
                return Err(Error::InvalidRequest);
            }
            // Public Candidates values are not authority. Recompute before origin.
            let expected = self
                .candidates_async(candidates.membership.clone(), object, page)
                .await?;
            if expected.ordered != candidates.ordered
                || candidates.ordered.is_empty()
                || candidates.ordered.len() > 3
            {
                return Err(Error::IncompatibleMembership);
            }
            let rank = candidates
                .ordered
                .iter()
                .position(|node| node == &self.node);
            let mut evidence = Vec::new();
            let mut saw_transient = false;
            let mut saw_version = false;
            let count = rank.unwrap_or(candidates.ordered.len());
            for (index, destination) in candidates.ordered[..count].iter().enumerate() {
                let mode = if rank.is_some() {
                    FetchMode::CopyOnly
                } else {
                    FetchMode::Acquire
                };
                match self
                    .request(
                        &candidates.membership,
                        destination,
                        context,
                        &operation,
                        mode,
                        scope,
                        budget,
                        (count - index) as u32,
                    )
                    .await
                {
                    Ok(response) => match classify(response.response(), &operation)? {
                        None => return Ok(CandidateResolution::Copy(response)),
                        Some(outcome) => {
                            saw_version |= outcome == ProbeOutcome::VersionUnavailable;
                            saw_transient |= matches!(
                                outcome,
                                ProbeOutcome::Unreachable | ProbeOutcome::Overloaded
                            );
                            evidence.push(outcome);
                            if saw_transient {
                                budget.note_route_failure();
                            }
                        }
                    },
                    Err(Error::Unavailable | Error::Io) => {
                        evidence.push(ProbeOutcome::Unreachable);
                        saw_transient = true;
                        budget.note_route_failure();
                    }
                    Err(Error::Overloaded) => {
                        evidence.push(ProbeOutcome::Overloaded);
                        saw_transient = true;
                        budget.note_route_failure();
                    }
                    Err(error) => return Err(error),
                }
            }
            if rank.is_some() {
                scope.check()?;
                Ok(CandidateResolution::Origin(OriginAuthority {
                    membership: candidates.membership,
                    node: self.node.clone(),
                    object: object.clone(),
                    page,
                    predecessor_evidence: evidence,
                    rank: rank.expect("candidate checked"),
                }))
            } else if saw_version && !saw_transient {
                Err(Error::VersionUnavailable)
            } else {
                Err(Error::Unavailable)
            }
        })
    }

    /// After an origin version miss, old copies on later candidates remain useful.
    /// This path never asks another node to start acquisition.
    pub fn remaining_copy<'a>(
        &'a self,
        candidates: &'a Candidates,
        context: &'a OriginContext,
        operation: &'a PeerOperation,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
    ) -> Operation<'a, Option<VerifiedResponse>> {
        Box::pin(async move {
            let rank = candidates
                .ordered
                .iter()
                .position(|node| node == &self.node)
                .ok_or(Error::Unauthorized)?;
            let mut transient = false;
            for destination in candidates.ordered.iter().skip(rank + 1) {
                match self
                    .request(
                        &candidates.membership,
                        destination,
                        context,
                        operation,
                        FetchMode::CopyOnly,
                        scope,
                        budget,
                        1,
                    )
                    .await
                {
                    Ok(response) => match classify(response.response(), operation)? {
                        None => return Ok(Some(response)),
                        Some(ProbeOutcome::Unreachable | ProbeOutcome::Overloaded) => {
                            transient = true
                        }
                        Some(_) => {}
                    },
                    Err(Error::Unavailable | Error::Overloaded | Error::Io) => {
                        transient = true;
                        budget.note_route_failure();
                    }
                    Err(error) => return Err(error),
                }
            }
            if transient {
                Err(Error::Unavailable)
            } else {
                Ok(None)
            }
        })
    }

    async fn request(
        &self,
        membership: &MembershipLease,
        destination: &NodeId,
        context: &OriginContext,
        operation: &PeerOperation,
        mode: FetchMode,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
        remaining_candidates: u32,
    ) -> Result<VerifiedResponse> {
        scope.check()?;
        // Reserve the complete permitted route before sending. Lost responses cannot
        // refund an unknown number of forwarded links. No retry gets fresh credits.
        let links = budget.route_links();
        if links == 0 {
            return Err(Error::HopBudgetExhausted);
        }
        let deadline = budget.begin_peer_attempt(Instant::now(), scope.deadline.0, links)?;
        let attempts = if matches!(mode, FetchMode::Acquire) {
            // Reserve remote acquisition credits from the same original call.
            // Without a signed response receipt unused remote credits stay spent.
            let credits = budget
                .remaining_attempts()
                .div_ceil(remaining_candidates.max(1));
            budget.partition(credits, 0)?.remaining_attempts()
        } else {
            0
        };
        let mut bytes = [0; 16];
        getrandom::getrandom(&mut bytes).map_err(|_| Error::Unavailable)?;
        let attempt = AttemptId(bytes);
        let credentials = self.credentials.borrow().clone().ok_or(Error::MissingKey)?;
        let origin = credentials.seal(context, attempt, scope)?;
        let request = PeerRequest {
            operation: copy_operation(operation, mode),
            origin,
            route: RouteBudget {
                membership: membership.version,
                request: scope.request,
                attempt,
                destination: destination.clone(),
                visited: vec![self.node.clone()],
                remaining_links: links,
                remaining_attempts: attempts,
                deadline: Deadline(deadline),
            },
        };
        let response = self.peers.request(request, scope).await?;
        scope.check()?;
        Ok(response)
    }

    pub fn origin_miss_error(&self, authority: &OriginAuthority) -> Error {
        if authority.predecessor_evidence.iter().any(|outcome| {
            matches!(
                outcome,
                ProbeOutcome::Unreachable | ProbeOutcome::Overloaded
            )
        }) {
            Error::Unavailable
        } else {
            Error::VersionUnavailable
        }
    }
}

pub enum CandidateResolution {
    Copy(VerifiedResponse),
    Origin(OriginAuthority),
}
impl OriginAuthority {
    pub fn validate(&self, object: &ObjectId, page: PageNumber) -> Result<()> {
        if object != &self.object || page != self.page {
            return Err(Error::Unauthorized);
        }
        // Private construction follows a recomputed ranking under this immutable
        // membership. Validation need not rehash the entire cluster on each write.
        if self.rank >= 3
            || self.predecessor_evidence.len() != self.rank
            || self.membership.member(&self.node).is_err()
        {
            return Err(Error::Unauthorized);
        }
        Ok(())
    }
}

fn operation_identity(operation: &PeerOperation) -> (&ObjectId, PageNumber) {
    match operation {
        PeerOperation::Page { page, .. } => (&page.version.object, page.number),
        PeerOperation::Metadata { object, .. } => (object, PageNumber(0)),
    }
}
fn copy_operation(operation: &PeerOperation, mode: FetchMode) -> PeerOperation {
    match operation {
        PeerOperation::Page { page, .. } => PeerOperation::Page {
            page: page.clone(),
            mode,
        },
        PeerOperation::Metadata {
            object, selector, ..
        } => PeerOperation::Metadata {
            object: object.clone(),
            selector: selector.clone(),
            mode,
        },
    }
}
fn classify(response: &PeerResponse, operation: &PeerOperation) -> Result<Option<ProbeOutcome>> {
    match response {
        PeerResponse::Miss => Ok(Some(ProbeOutcome::CopyMiss)),
        PeerResponse::VersionUnavailable => Ok(Some(ProbeOutcome::VersionUnavailable)),
        PeerResponse::Unavailable => Ok(Some(ProbeOutcome::Unreachable)),
        PeerResponse::Overloaded => Ok(Some(ProbeOutcome::Overloaded)),
        PeerResponse::OriginRejected => Err(Error::OriginRejected),
        PeerResponse::OriginForbidden => Err(Error::OriginForbidden),
        PeerResponse::Page {
            metadata,
            ciphertext,
        } => match operation {
            PeerOperation::Page { page, .. }
                if &ciphertext.envelope().page == page && metadata.version == page.version =>
            {
                metadata.immutable().validate_page(ciphertext.envelope())?;
                Ok(None)
            }
            _ => Err(Error::CorruptRecord),
        },
        PeerResponse::Metadata(metadata) => match operation {
            PeerOperation::Metadata {
                object, selector, ..
            } if &metadata.version.object == object => {
                if let MetadataSelector::Pinned(etag) = selector {
                    if &metadata.version.etag != etag {
                        return Err(Error::CorruptRecord);
                    }
                }
                Ok(None)
            }
            _ => Err(Error::CorruptRecord),
        },
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn candidate_failure_routes_spend_initial_allowance_with_four_then_eight_link_ceiling() {
        struct Routes(RefCell<Vec<(u8, u32)>>);
        impl PeerClient for Routes {
            fn request<'a>(
                &'a self,
                request: PeerRequest,
                _: &'a RequestScope,
            ) -> Operation<'a, VerifiedResponse> {
                self.0.borrow_mut().push((
                    request.route.remaining_links,
                    request.route.remaining_attempts,
                ));
                Box::pin(async { Err(Error::Unavailable) })
            }
        }
        let (membership, placement, context, scope, credentials) = fixture();
        let peers = Rc::new(Routes(RefCell::new(Vec::new())));
        let policy = CandidatePolicy::new(NodeId("outside".into()), placement, peers.clone());
        policy.set_credentials(credentials);
        let mut budget = AcquisitionBudget::new(scope.deadline.0, 16, 24);
        let result = futures::executor::block_on(
            policy.resolve_with_budget(
                policy
                    .candidates(membership, &context.object, PageNumber(0))
                    .unwrap(),
                &context,
                PeerOperation::Metadata {
                    object: context.object.clone(),
                    selector: MetadataSelector::Fresh,
                    mode: FetchMode::Acquire,
                },
                &scope,
                &mut budget,
            ),
        );
        assert!(matches!(result, Err(Error::Unavailable)));
        let routes = peers.0.borrow();
        assert_eq!(
            routes.iter().map(|(links, _)| *links).collect::<Vec<_>>(),
            vec![4, 8, 8]
        );
        assert_eq!(
            routes.iter().map(|(_, credits)| *credits).sum::<u32>()
                + routes.len() as u32
                + budget.remaining_attempts(),
            16
        );
        assert_eq!(budget.remaining_links(), 4);
    }
    use crate::model::{
        identity::{CacheId, CacheKey, ObjectVersion, StrongEtag},
        metadata::{ExpiresAt, ObjectMetadata},
    };
    fn object() -> ObjectId {
        ObjectId {
            cache: CacheId("33333333-3333-4333-8333-333333333333".into()),
            key: CacheKey([0; 32]),
        }
    }
    struct ProbePeer {
        calls: RefCell<Vec<(NodeId, bool)>>,
        error: Error,
    }
    impl PeerClient for ProbePeer {
        fn request<'a>(
            &'a self,
            request: PeerRequest,
            _: &'a RequestScope,
        ) -> Operation<'a, VerifiedResponse> {
            let copy = matches!(
                request.operation,
                PeerOperation::Metadata {
                    mode: FetchMode::CopyOnly,
                    ..
                } | PeerOperation::Page {
                    mode: FetchMode::CopyOnly,
                    ..
                }
            );
            self.calls
                .borrow_mut()
                .push((request.route.destination, copy));
            Box::pin(async move { Err(self.error) })
        }
    }
    fn fixture() -> (
        MembershipLease,
        Rc<Placement>,
        OriginContext,
        RequestScope,
        Rc<CredentialCrypto>,
    ) {
        use crate::{
            model::identity::ClusterId,
            model::identity::{MembershipVersion, RequestId},
            runtime::admission::Admission,
            security::keyring::{KeyEpochs, Keyring},
            topology::membership::{Member, Membership},
        };
        let membership = std::sync::Arc::new(
            Membership::validate(
                MembershipVersion(1),
                (0..4)
                    .map(|n| Member {
                        node: NodeId(format!("22222222-2222-4222-8222-{n:012}")),
                        shares: std::num::NonZeroU32::new(4).unwrap(),
                        peer_endpoint: format!("127.0.0.1:{}", 8000 + n),
                        rails: vec![],
                        alignment_enabled: true,
                    })
                    .collect(),
            )
            .unwrap(),
        );
        let context = OriginContext {
            object: object(),
            metadata: None,
            authorization: None,
        };
        let scope = RequestScope::new(
            RequestId([0; 16]),
            Instant::now() + std::time::Duration::from_secs(60),
        )
        .unwrap();
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let keys = Rc::new(Keyring::new(
            ClusterId("cluster".into()),
            NodeId("local".into()),
            std::sync::Arc::new(KeyEpochs::default()),
        ));
        (
            membership,
            Rc::new(Placement::new(16)),
            context,
            scope,
            Rc::new(CredentialCrypto::new(keys, admission)),
        )
    }
    #[test]
    fn only_candidates_mint_authority_after_ordered_copy_probes() {
        let (membership, placement, context, scope, credentials) = fixture();
        let ordered = placement
            .rank(membership.clone(), &context.object, PageNumber(0))
            .unwrap()
            .ordered;
        let peer = Rc::new(ProbePeer {
            calls: RefCell::new(vec![]),
            error: Error::Unavailable,
        });
        let policy = CandidatePolicy::new(ordered[2].clone(), placement, peer.clone());
        policy.set_credentials(credentials);
        let candidates = policy
            .candidates(membership, &context.object, PageNumber(0))
            .unwrap();
        let mut budget = AcquisitionBudget::new(scope.deadline.0, 4, 8);
        let result = futures::executor::block_on(policy.resolve_with_budget(
            candidates,
            &context,
            PeerOperation::Metadata {
                object: context.object.clone(),
                selector: MetadataSelector::Fresh,
                mode: FetchMode::Acquire,
            },
            &scope,
            &mut budget,
        ))
        .unwrap();
        let CandidateResolution::Origin(authority) = result else {
            panic!("must grant third candidate after probes")
        };
        authority.validate(&context.object, PageNumber(0)).unwrap();
        assert_eq!(
            *peer.calls.borrow(),
            vec![(ordered[0].clone(), true), (ordered[1].clone(), true)]
        );
        assert_eq!(budget.remaining_attempts(), 2);
        assert_eq!(budget.remaining_links(), 0);
        assert_eq!(
            authority.validate(&context.object, PageNumber(1)),
            Err(Error::Unauthorized)
        );
    }
    #[test]
    fn noncandidate_never_gets_origin_and_auth_failure_is_not_miss_evidence() {
        let (membership, placement, context, scope, credentials) = fixture();
        let peer = Rc::new(ProbePeer {
            calls: RefCell::new(vec![]),
            error: Error::Unauthorized,
        });
        let ordered = placement
            .rank(membership.clone(), &context.object, PageNumber(0))
            .unwrap()
            .ordered;
        let policy = CandidatePolicy::new(ordered[1].clone(), placement.clone(), peer.clone());
        policy.set_credentials(credentials.clone());
        let mut budget = AcquisitionBudget::new(scope.deadline.0, 4, 8);
        let result = futures::executor::block_on(
            policy.resolve_with_budget(
                policy
                    .candidates(membership.clone(), &context.object, PageNumber(0))
                    .unwrap(),
                &context,
                PeerOperation::Metadata {
                    object: context.object.clone(),
                    selector: MetadataSelector::Fresh,
                    mode: FetchMode::Acquire,
                },
                &scope,
                &mut budget,
            ),
        );
        assert!(matches!(result, Err(Error::Unauthorized)));
        let peer = Rc::new(ProbePeer {
            calls: RefCell::new(vec![]),
            error: Error::Unavailable,
        });
        let policy = CandidatePolicy::new(NodeId("outside".into()), placement, peer.clone());
        policy.set_credentials(credentials);
        let mut budget = AcquisitionBudget::new(scope.deadline.0, 16, 24);
        let result = futures::executor::block_on(
            policy.resolve_with_budget(
                policy
                    .candidates(membership, &context.object, PageNumber(0))
                    .unwrap(),
                &context,
                PeerOperation::Metadata {
                    object: context.object.clone(),
                    selector: MetadataSelector::Fresh,
                    mode: FetchMode::Acquire,
                },
                &scope,
                &mut budget,
            ),
        );
        assert!(matches!(result, Err(Error::Unavailable)));
        assert_eq!(peer.calls.borrow().len(), 3);
        assert!(peer.calls.borrow().iter().all(|(_, copy)| !copy));
    }
    #[test]
    fn signed_failure_classification_preserves_auth_and_version_distinctions() {
        let operation = PeerOperation::Metadata {
            object: object(),
            selector: MetadataSelector::Fresh,
            mode: FetchMode::CopyOnly,
        };
        assert_eq!(
            classify(&PeerResponse::OriginRejected, &operation),
            Err(Error::OriginRejected)
        );
        assert_eq!(
            classify(&PeerResponse::VersionUnavailable, &operation),
            Ok(Some(ProbeOutcome::VersionUnavailable))
        );
        assert_eq!(
            classify(&PeerResponse::Overloaded, &operation),
            Ok(Some(ProbeOutcome::Overloaded))
        );
    }
    #[test]
    fn metadata_copy_must_match_object_and_explicit_pin() {
        let operation = PeerOperation::Metadata {
            object: object(),
            selector: MetadataSelector::Pinned(StrongEtag::test_value("v1")),
            mode: FetchMode::CopyOnly,
        };
        let mut metadata = ObjectMetadata {
            version: ObjectVersion {
                object: object(),
                etag: StrongEtag::test_value("v1"),
            },
            length: 0,
            expires_at: ExpiresAt(std::time::UNIX_EPOCH),
        };
        assert_eq!(
            classify(&PeerResponse::Metadata(metadata.clone()), &operation),
            Ok(None)
        );
        metadata.version.etag = StrongEtag::test_value("v2");
        assert_eq!(
            classify(&PeerResponse::Metadata(metadata.clone()), &operation),
            Err(Error::CorruptRecord)
        );
        metadata.version.etag = StrongEtag::test_value("v1");
        metadata.version.object.key = CacheKey([1; 32]);
        assert_eq!(
            classify(&PeerResponse::Metadata(metadata), &operation),
            Err(Error::CorruptRecord)
        );
    }

    struct RecordedPeer {
        calls: RefCell<Vec<(NodeId, bool)>>,
        error: Error,
    }
    impl PeerClient for RecordedPeer {
        fn request<'a>(
            &'a self,
            request: PeerRequest,
            _scope: &'a RequestScope,
        ) -> Operation<'a, VerifiedResponse> {
            Box::pin(async move {
                let copy = match request.operation {
                    PeerOperation::Page { mode, .. } | PeerOperation::Metadata { mode, .. } => {
                        matches!(mode, FetchMode::CopyOnly)
                    }
                };
                self.calls
                    .borrow_mut()
                    .push((request.route.destination, copy));
                Err(self.error)
            })
        }
    }
    fn membership() -> MembershipLease {
        use crate::{
            model::identity::MembershipVersion,
            topology::membership::{Member, Membership},
        };
        std::sync::Arc::new(
            Membership::validate(
                MembershipVersion(1),
                (0..4)
                    .map(|i| Member {
                        node: NodeId(format!("22222222-2222-4222-8222-{i:012}")),
                        shares: std::num::NonZeroU32::new(4).unwrap(),
                        peer_endpoint: format!("127.0.0.1:{}", 8000 + i),
                        rails: vec![],
                        alignment_enabled: false,
                    })
                    .collect(),
            )
            .unwrap(),
        )
    }
    fn policy(node: NodeId, peers: Rc<RecordedPeer>) -> CandidatePolicy {
        use crate::security::keyring::{KeyEpochs, Keyring};
        let config = crate::test_support::cluster::config(false);
        let admission = Rc::new(crate::runtime::admission::Admission::new(config.limits));
        let keys = Rc::new(Keyring::new(
            config.cluster,
            node.clone(),
            std::sync::Arc::new(KeyEpochs::default()),
        ));
        let policy = CandidatePolicy::new(node, Rc::new(Placement::new(8)), peers);
        policy.set_credentials(Rc::new(CredentialCrypto::new(keys, admission)));
        policy
    }
    fn scope() -> RequestScope {
        RequestScope::new(
            crate::model::identity::RequestId([7; 16]),
            Instant::now() + std::time::Duration::from_secs(60),
        )
        .unwrap()
    }
    #[test]
    fn backup_probes_only_predecessors_in_order_before_scoped_origin_authority() {
        futures::executor::block_on(async {
            let membership = membership();
            let candidates = Placement::new(8)
                .rank(membership, &object(), PageNumber(0))
                .unwrap();
            let expected = candidates.ordered[..2].to_vec();
            let peers = Rc::new(RecordedPeer {
                calls: RefCell::new(vec![]),
                error: Error::Unavailable,
            });
            let policy = policy(candidates.ordered[2].clone(), peers.clone());
            let context = OriginContext {
                object: object(),
                metadata: None,
                authorization: None,
            };
            let scope = scope();
            let mut budget = AcquisitionBudget::new(scope.deadline.0, 3, 8);
            let operation = PeerOperation::Metadata {
                object: object(),
                selector: MetadataSelector::Fresh,
                mode: FetchMode::Acquire,
            };
            let CandidateResolution::Origin(authority) = policy
                .resolve_with_budget(candidates, &context, operation, &scope, &mut budget)
                .await
                .unwrap()
            else {
                panic!("expected authority")
            };
            assert_eq!(
                *peers.calls.borrow(),
                expected
                    .into_iter()
                    .map(|node| (node, true))
                    .collect::<Vec<_>>()
            );
            assert_eq!(authority.validate(&object(), PageNumber(0)), Ok(()));
            assert_eq!(
                authority.validate(&object(), PageNumber(1)),
                Err(Error::Unauthorized)
            );
            assert_eq!(budget.remaining_attempts(), 1);
            assert_eq!(budget.remaining_links(), 0);
        });
    }
    #[test]
    fn noncandidate_acquires_from_candidates_and_never_mints_origin_authority() {
        futures::executor::block_on(async {
            let membership = membership();
            let candidates = Placement::new(8)
                .rank(membership.clone(), &object(), PageNumber(0))
                .unwrap();
            let local = membership
                .members()
                .iter()
                .find(|member| !candidates.ordered.contains(&member.node))
                .unwrap()
                .node
                .clone();
            let peers = Rc::new(RecordedPeer {
                calls: RefCell::new(vec![]),
                error: Error::Unavailable,
            });
            let policy = policy(local, peers.clone());
            let context = OriginContext {
                object: object(),
                metadata: None,
                authorization: None,
            };
            let scope = scope();
            let mut budget = AcquisitionBudget::new(scope.deadline.0, 16, 24);
            let operation = PeerOperation::Metadata {
                object: object(),
                selector: MetadataSelector::Fresh,
                mode: FetchMode::Acquire,
            };
            assert!(matches!(
                policy
                    .resolve_with_budget(candidates, &context, operation, &scope, &mut budget)
                    .await,
                Err(Error::Unavailable)
            ));
            assert_eq!(peers.calls.borrow().len(), 3);
            assert!(peers.calls.borrow().iter().all(|(_, copy)| !copy));
        });
    }
    #[test]
    fn peer_auth_failure_is_not_predecessor_miss_evidence() {
        futures::executor::block_on(async {
            let candidates = Placement::new(8)
                .rank(membership(), &object(), PageNumber(0))
                .unwrap();
            let peers = Rc::new(RecordedPeer {
                calls: RefCell::new(vec![]),
                error: Error::Unauthorized,
            });
            let policy = policy(candidates.ordered[1].clone(), peers.clone());
            let context = OriginContext {
                object: object(),
                metadata: None,
                authorization: None,
            };
            let scope = scope();
            let mut budget = AcquisitionBudget::new(scope.deadline.0, 3, 8);
            let operation = PeerOperation::Metadata {
                object: object(),
                selector: MetadataSelector::Fresh,
                mode: FetchMode::Acquire,
            };
            assert!(matches!(
                policy
                    .resolve_with_budget(candidates, &context, operation, &scope, &mut budget)
                    .await,
                Err(Error::Unauthorized)
            ));
            assert_eq!(peers.calls.borrow().len(), 1);
        });
    }
    #[test]
    fn origin_version_miss_cannot_claim_absence_when_predecessor_was_unreachable() {
        let membership = membership();
        let ranked = Placement::new(8)
            .rank(membership.clone(), &object(), PageNumber(0))
            .unwrap();
        let peers = Rc::new(RecordedPeer {
            calls: RefCell::new(vec![]),
            error: Error::Unavailable,
        });
        let policy = policy(ranked.ordered[1].clone(), peers);
        let mut authority = OriginAuthority {
            membership,
            node: ranked.ordered[1].clone(),
            object: object(),
            page: PageNumber(0),
            predecessor_evidence: vec![ProbeOutcome::Unreachable],
            rank: 1,
        };
        assert_eq!(policy.origin_miss_error(&authority), Error::Unavailable);
        authority.predecessor_evidence = vec![ProbeOutcome::CopyMiss];
        assert_eq!(
            policy.origin_miss_error(&authority),
            Error::VersionUnavailable
        );
    }
}
