//! Ordered copy-only predecessor probes and origin-fetch authority.
//!
//! Authority pins membership and object/page scope. Joining a flight from a newer
//! snapshot cannot grant origin permission. Only ranked candidates persist fills;
//! noncandidate requesters use memory, relays use transit buffers only.
use crate::{
    error::{Operation, Result, deferred, pending},
    model::{
        context::OriginContext,
        identity::{NodeId, ObjectId, PageNumber},
    },
    peer::{requester::PeerClient, wire::Operation as PeerOperation},
    runtime::deadline::RequestScope,
    topology::{
        membership::MembershipLease,
        placement::{Candidates, Placement},
    },
};
use std::rc::Rc;

/// Private fields and no public constructor prevent transport/callers from minting
/// fill permission. Origin must check requested cache/key/page against this scope.
pub struct OriginAuthority {
    membership: MembershipLease,
    node: NodeId,
    object: ObjectId,
    page: PageNumber,
    predecessor_evidence: Vec<ProbeOutcome>,
}
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
}
impl CandidatePolicy {
    pub fn new(node: NodeId, placement: Rc<Placement>, peers: Rc<dyn PeerClient>) -> Self {
        Self {
            node,
            placement,
            peers,
        }
    }
    pub fn candidates(
        &self,
        _membership: MembershipLease,
        _object: &ObjectId,
        _page: PageNumber,
    ) -> Result<Candidates> {
        pending("candidates.rank")
    }
    /// Probe predecessors with copy-only semantics before issuing authority. An
    /// available copy is returned instead; auth/protocol errors are not miss proof.
    pub fn resolve<'a>(
        &'a self,
        _candidates: Candidates,
        _context: &'a OriginContext,
        _operation: PeerOperation,
        _scope: &'a RequestScope,
    ) -> Operation<'a, CandidateResolution> {
        deferred("candidates.resolve")
    }
}
pub enum CandidateResolution {
    Copy(crate::peer::wire::VerifiedResponse),
    Origin(OriginAuthority),
}
impl OriginAuthority {
    pub fn validate(&self, _object: &ObjectId, _page: PageNumber) -> Result<()> {
        pending("candidates.validate_authority")
    }
}
#[cfg(test)]
mod tests { /* Predecessor ordering, only top three, version overlap, copy-only safety. */
}
