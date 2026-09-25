//! Peer transport interfaces injected into reads; local service injected into server.
mod decode;
pub mod handshake;
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
use std::{cell::RefCell, collections::BTreeMap};

/// Worker-local routing inputs. A request always uses its exact retained snapshot.
/// The controller installs snapshots; readiness must not rewrite their contents.
pub struct PeerNetwork {
    pub local: NodeId,
    snapshots: RefCell<BTreeMap<u64, MembershipLease>>,
    capacity: usize,
}

impl PeerNetwork {
    pub fn new(local: NodeId, capacity: usize) -> Result<Self> {
        if local.0.is_empty() || capacity == 0 {
            return Err(Error::InvalidConfiguration);
        }
        Ok(Self {
            local,
            snapshots: RefCell::new(BTreeMap::new()),
            capacity,
        })
    }

    pub fn install(&self, membership: MembershipLease) -> Result<()> {
        let mut snapshots = self.snapshots.borrow_mut();
        if let Some(existing) = snapshots.get(&membership.version.0) {
            return if std::sync::Arc::ptr_eq(existing, &membership) {
                Ok(())
            } else {
                Err(Error::IncompatibleMembership)
            };
        }
        if snapshots.len() == self.capacity {
            return Err(Error::Overloaded);
        }
        snapshots.insert(membership.version.0, membership);
        Ok(())
    }

    pub fn retire(&self, version: MembershipVersion) {
        self.snapshots.borrow_mut().remove(&version.0);
    }

    pub fn membership(&self, version: MembershipVersion) -> Result<MembershipLease> {
        self.snapshots
            .borrow()
            .get(&version.0)
            .cloned()
            .ok_or(Error::IncompatibleMembership)
    }

    pub fn endpoint(
        &self,
        version: MembershipVersion,
        node: &NodeId,
    ) -> Result<crate::http::pool::Endpoint> {
        let membership = self.membership(version)?;
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
