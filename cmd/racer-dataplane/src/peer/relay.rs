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
    runtime::{admission::Admission, deadline::RequestScope},
    security::forwarding::Forwarding,
    topology::paths::Paths,
};
use std::rc::Rc;
#[path = "relay_admission.rs"]
mod admission;
pub struct Relay {
    paths: Rc<Paths>,
    forwarding: Rc<Forwarding>,
    transport: Rc<dyn PeerTransport>,
    admission: admission::RelayAdmission,
    resources: Rc<Admission>,
    #[cfg(test)]
    overloads: std::cell::Cell<usize>,
    network: Option<Rc<super::PeerNetwork>>,
    handshake: Option<Rc<super::handshake::Handshake>>,
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
            admission: admission::RelayAdmission::new(admission.clone()),
            resources: admission,
            #[cfg(test)]
            overloads: std::cell::Cell::new(0),
            network: None,
            handshake: None,
        }
    }
    pub fn with_network(mut self, network: Rc<super::PeerNetwork>) -> Self {
        self.network = Some(network);
        self
    }
    pub(crate) async fn serve_stream(
        &self,
        request: VerifiedRequest,
        upstream: crate::http::pool::ConnectionLease,
        transfers: &super::transfer::Transfers,
        scope: &RequestScope,
    ) -> crate::error::Result<crate::http::pool::ConnectionLease> {
        let scope = super::request_scope(request.request(), scope)?;
        let binding = request.binding().clone();
        let prepare = async {
            let network = self.network.as_ref().ok_or(Error::InvalidConfiguration)?;
            let budget = &request.request().route;
            let previous = budget.visited.last().ok_or(Error::InvalidRequest)?.clone();
            network.endpoint(budget.membership, &previous)?;
            let membership = network.membership(budget.membership)?;
            let saturated = crate::topology::graph::Graph::new(membership.clone())
                .neighbors(&network.local)?
                .into_iter()
                .filter(|node| {
                    network
                        .endpoint(budget.membership, node)
                        .is_ok_and(|endpoint| transfers.http.endpoint_saturated(&endpoint))
                })
                .collect::<Vec<_>>();
            let route = self
                .paths
                .shortest_available_async(
                    membership,
                    &network.local,
                    &super::search_budget(budget, &network.local)?,
                    &saturated,
                )
                .await
                .map_err(|error| {
                    // If the available subgraph has no route, preserve overload
                    // backoff at the acquisition owner rather than imply a miss.
                    if error == Error::Unavailable && !saturated.is_empty() {
                        Error::Overloaded
                    } else {
                        error
                    }
                })?;
            let next = route.nodes.get(1).ok_or(Error::Unavailable)?;
            // No wait for another exchange's scarce resources, including the
            // discovery handshake. A failure unwinds before a page head is sent.
            let permit =
                self.resources
                    .reserve(None, crate::model::limits::ResourceClass::Relay, 1)?;
            let chunk = self.resources.reserve_transit()?;
            if let Some(handshake) = &self.handshake {
                handshake
                    .negotiate_transit(next, budget.membership, &scope)
                    .await?;
            }
            let endpoint = network.endpoint(budget.membership, next)?;
            // The signed security profile permits visited+links == MAX_HOPS+1;
            // preserve the same effective-budget transition as buffered relay.
            let mut outbound = budget.clone();
            outbound.visited.push(network.local.clone());
            outbound.remaining_links = outbound
                .remaining_links
                .checked_sub(1)
                .ok_or(Error::HopBudgetExhausted)?;
            let signed = self.forwarding.append_request(request, next, outbound)?;
            super::stream::TransitBody::open(
                transfers,
                &endpoint,
                signed,
                &scope,
                chunk,
                permit,
                &self.forwarding,
                &binding,
                &previous,
            )
            .await
            .inspect_err(|error| {
                if matches!(error, Error::Io | Error::Unauthorized | Error::Replay)
                    && let Some(handshake) = &self.handshake
                {
                    handshake.invalidate(next);
                }
            })
        }
        .await;
        match prepare {
            Ok((head, body)) => body.send(transfers, upstream, head, &scope).await,
            Err(error) => {
                let outcome = match error {
                    Error::Overloaded => {
                        #[cfg(test)]
                        self.overloads.set(self.overloads.get() + 1);
                        super::wire::PeerResponse::Overloaded
                    }
                    Error::Unavailable
                    | Error::Io
                    | Error::HopBudgetExhausted
                    | Error::IncompatibleMembership => super::wire::PeerResponse::Unavailable,
                    _ => return Err(error),
                };
                scope.check()?;
                let response = self.forwarding.sign_response(&binding, outcome)?;
                let sent = transfers
                    .io
                    .send_head(
                        upstream,
                        super::wire::WireCodec::encode(&response.authentication, true, 0)?,
                        &scope,
                    )
                    .await?;
                let mut connection = sent.connection;
                connection.finish_exchange()?;
                Ok(connection)
            }
        }
    }
    pub(super) fn poll_admission_deadlines(&self) {
        self.admission.poll_deadlines();
    }
    #[cfg(test)]
    pub(super) fn admission_waits(&self) -> usize {
        self.admission.waits.get() + self.overloads.get()
    }
    pub fn with_handshake(mut self, handshake: Rc<super::handshake::Handshake>) -> Self {
        self.handshake = Some(handshake);
        self
    }
    /// Retain ingress binding and reverse path, append a signed request hop, and
    /// exchange complete envelopes. Verify the downstream response against that
    /// binding before appending a reverse hop. Never re-sign the original response.
    ///
    /// ```compile_fail
    /// use racer_dataplane::{peer::{relay::Relay, wire::SignedRequest},
    ///     runtime::deadline::RequestScope};
    /// fn unverified(relay: &Relay, request: SignedRequest, scope: &RequestScope) {
    ///     relay.forward(request, scope);
    /// }
    /// ```
    pub fn forward<'a>(
        &'a self,
        request: VerifiedRequest,
        scope: &'a RequestScope,
    ) -> Operation<'a, SignedResponse> {
        Box::pin(async move {
            let scope = super::request_scope(request.request(), scope)?;
            let network = self.network.as_ref().ok_or(Error::InvalidConfiguration)?;
            let budget = &request.request().route;
            if budget.destination == network.local || budget.visited.contains(&network.local) {
                return Err(Error::InvalidRequest);
            }
            let _reservation = self.admission.acquire(&scope).await?;
            let membership = network.membership(budget.membership)?;
            let search_budget = super::search_budget(budget, &network.local)?;
            let route = self
                .paths
                .shortest_async(membership, &network.local, &search_budget)
                .await?;
            let next = route.nodes.get(1).ok_or(Error::Unavailable)?;
            if let Some(handshake) = &self.handshake {
                handshake
                    .negotiate_at(next, budget.membership, &scope)
                    .await?;
            }
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
            let response = self.transport.exchange(outbound, &scope).await?;
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
