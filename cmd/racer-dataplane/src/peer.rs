//! Peer transport interfaces injected into reads; local service injected into server.
mod decode;
pub mod handshake;
mod native;
mod native_io;
pub mod relay;
pub mod requester;
pub mod server;
#[cfg(test)]
mod tests;
pub mod transfer;
pub mod wire;

use crate::{
    error::{Error, Result},
    model::identity::{MembershipVersion, NodeId},
    topology::membership::MembershipLease,
};
use std::sync::Arc;

/// Worker-local identity and a handle to the sole node-wide incoming registry.
/// Outbound operations route directly from their retained membership lease.
pub struct PeerNetwork {
    pub local: NodeId,
    published: Arc<crate::control::snapshot::PublishedState>,
}

impl PeerNetwork {
    pub fn new(
        local: NodeId,
        published: Arc<crate::control::snapshot::PublishedState>,
    ) -> Result<Self> {
        if local.0.is_empty() {
            return Err(Error::InvalidConfiguration);
        }
        Ok(Self { local, published })
    }

    pub fn membership(&self, version: MembershipVersion) -> Result<MembershipLease> {
        self.published.membership(version)
    }

    pub fn endpoint(
        &self,
        membership: &MembershipLease,
        node: &NodeId,
    ) -> Result<crate::http::pool::Endpoint> {
        if !crate::topology::graph::Graph::new(membership.clone())
            .neighbors(&self.local)?
            .contains(node)
        {
            return Err(Error::InvalidRequest);
        }
        let member = membership
            .members()
            .iter()
            .find(|member| &member.node == node)
            .ok_or(Error::IncompatibleMembership)?;
        Ok(crate::http::pool::Endpoint::Peer(
            member.peer_endpoint.clone(),
        ))
    }
}

/// Narrow a caller's scope to the signed route without creating a new cancellation
/// domain or extending the original deadline.
pub(crate) fn request_scope(
    request: &wire::PeerRequest,
    scope: &crate::runtime::deadline::RequestScope,
) -> Result<crate::runtime::deadline::RequestScope> {
    scope.check()?;
    if request.route.request != scope.request
        || request.origin.request != scope.request
        || request.route.attempt != request.origin.attempt
    {
        return Err(Error::InvalidRequest);
    }
    let mut narrowed = scope.clone();
    narrowed.deadline.0 = narrowed
        .deadline
        .0
        .min(request.route.deadline.0)
        .min(request.origin.scope().deadline.0);
    narrowed.check()?;
    Ok(narrowed)
}

pub(crate) fn check_membership(
    request: &wire::PeerRequest,
    membership: &MembershipLease,
) -> Result<()> {
    if request.route.membership != membership.version {
        return Err(Error::IncompatibleMembership);
    }
    Ok(())
}

pub(crate) fn search_budget(
    route: &crate::topology::paths::RouteBudget,
    local: &NodeId,
) -> Result<crate::topology::paths::RouteBudget> {
    let mut budget = route.clone();
    if budget.visited.last() == Some(local) {
        budget.visited.pop();
    } else {
        budget.remaining_links = budget
            .remaining_links
            .checked_sub(1)
            .ok_or(Error::HopBudgetExhausted)?;
    }
    Ok(budget)
}
