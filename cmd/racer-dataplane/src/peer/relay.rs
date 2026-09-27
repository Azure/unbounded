//! Opaque bounded transit with recorded reverse-path responses, never a page cache.
//!
//! No decryption service is injected here. Reverse-link failure terminates the
//! attempt; responses are not independently rerouted. Preserve encrypted credentials.
use super::{
    requester::PeerTransport,
    wire::{SignedResponse, VerifiedRequest},
};
use crate::{
    error::{Error, Operation},
    model::limits::ResourceClass,
    runtime::{admission::Admission, deadline::RequestScope},
    security::forwarding::Forwarding,
    topology::{membership::MembershipLease, paths::Paths},
};
use std::rc::Rc;
pub struct Relay {
    paths: Rc<Paths>,
    forwarding: Rc<Forwarding>,
    transport: Rc<dyn PeerTransport>,
    admission: Rc<Admission>,
    network: Option<Rc<super::PeerNetwork>>,
}
impl Relay {
    pub fn new(
        paths: Rc<Paths>,
        forwarding: Rc<Forwarding>,
        transport: Rc<dyn PeerTransport>,
        admission: Rc<Admission>,
    ) -> Self {
        Self {
            paths,
            forwarding,
            transport,
            admission,
            network: None,
        }
    }
    pub fn with_network(mut self, network: Rc<super::PeerNetwork>) -> Self {
        self.network = Some(network);
        self
    }
    /// Retain ingress binding and reverse path, append a signed request hop, and
    /// exchange complete envelopes. Verify the downstream response against that
    /// binding before appending a reverse hop. Never re-sign the original response.
    ///
    /// ```compile_fail
    /// use racer_dataplane::{peer::{relay::Relay, wire::SignedRequest},
    ///     runtime::deadline::RequestScope, topology::membership::MembershipLease};
    /// fn unverified(relay: &Relay, request: SignedRequest,
    ///     membership: MembershipLease, scope: &RequestScope) {
    ///     relay.forward(request, membership, scope);
    /// }
    /// ```
    pub fn forward<'a>(
        &'a self,
        request: VerifiedRequest,
        membership: MembershipLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, SignedResponse> {
        Box::pin(async move {
            let scope = super::request_scope(request.request(), scope)?;
            super::check_membership(request.request(), &membership)?;
            let network = self.network.as_ref().ok_or(Error::InvalidConfiguration)?;
            let budget = &request.request().route;
            if budget.destination == network.local || budget.visited.contains(&network.local) {
                return Err(Error::InvalidRequest);
            }
            let _reservation = self.admission.reserve(None, ResourceClass::Relay, 1)?;
            let search_budget = super::search_budget(budget, &network.local)?;
            let route = self
                .paths
                .shortest_async(membership.clone(), &network.local, &search_budget, &scope)
                .await?;
            let next = route.nodes.get(1).ok_or(Error::Unavailable)?;
            let previous = request
                .forwarders()
                .last()
                .unwrap_or(request.origin())
                .node()
                .clone();
            let binding = request.binding().clone();
            let mut visited = budget.visited.clone();
            visited.push(network.local.clone());
            let remaining_links = budget
                .remaining_links
                .checked_sub(1)
                .ok_or(Error::HopBudgetExhausted)?;
            let outbound_budget = crate::topology::paths::RouteBudget {
                membership: budget.membership,
                request: budget.request,
                attempt: budget.attempt,
                destination: budget.destination.clone(),
                visited,
                remaining_links,
                remaining_attempts: budget.remaining_attempts,
                deadline: budget.deadline,
            };
            let outbound = self
                .forwarding
                .append_request(request, next, outbound_budget)?;
            let response = self
                .transport
                .exchange(outbound, membership, &scope)
                .await?;
            scope.check()?;
            let response = self.forwarding.verify_response(response, &binding)?;
            // The caller owns the ingress connection. Return on that connection;
            // never choose a new route for a reverse-link failure.
            self.forwarding.append_response(response, &previous)
        })
    }
}
#[cfg(test)]
mod tests { /* Transit-only buffers, reverse-link failure, budgets, opaque credentials. */
}
