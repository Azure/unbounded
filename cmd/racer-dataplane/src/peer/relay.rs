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
use std::{
    cell::RefCell,
    collections::BTreeMap,
    rc::Rc,
    time::{Duration, Instant},
};
#[path = "relay_admission.rs"]
mod admission;
pub struct Relay {
    paths: Rc<Paths>,
    forwarding: Rc<Forwarding>,
    transport: Rc<dyn PeerTransport>,
    admission: admission::RelayAdmission,
    resources: Rc<Admission>,
    congested: RefCell<NeighborCongestion>,
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
            congested: RefCell::new(NeighborCongestion::default()),
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
            let neighbors =
                crate::topology::graph::Graph::new(membership.clone()).neighbors(&network.local)?;
            self.congested
                .borrow_mut()
                .retain(budget.membership, &neighbors, Instant::now());
            let saturated = neighbors
                .into_iter()
                .filter(|node| {
                    self.congested.borrow().entries.contains_key(node)
                        || network
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
            let version = budget.membership;
            outbound.visited.push(network.local.clone());
            outbound.remaining_links = outbound
                .remaining_links
                .checked_sub(1)
                .ok_or(Error::HopBudgetExhausted)?;
            let signed = self.forwarding.append_request(request, next, outbound)?;
            let result = super::stream::TransitBody::open(
                transfers,
                &endpoint,
                signed,
                &scope,
                chunk,
                permit,
                &self.forwarding,
                &binding,
                &previous,
                next,
            )
            .await
            .inspect_err(|error| {
                if matches!(error, Error::Io | Error::Unauthorized | Error::Replay)
                    && let Some(handshake) = &self.handshake
                {
                    handshake.invalidate(next);
                }
            });
            if let Ok((_, _, true)) = &result {
                // Remember only an authenticated immediate neighbor's overload.
                // Future independent arrivals can use another incident edge; the
                // failed envelope is returned unchanged and is never replayed.
                self.congested
                    .borrow_mut()
                    .observe(next.clone(), version, Instant::now());
            }
            result
        }
        .await;
        match prepare {
            Ok((head, body, _)) => body.send(transfers, upstream, head, &scope).await,
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

#[derive(Default)]
struct NeighborCongestion {
    entries: BTreeMap<
        crate::model::identity::NodeId,
        (crate::model::identity::MembershipVersion, Instant),
    >,
}

impl NeighborCongestion {
    fn retain(
        &mut self,
        membership: crate::model::identity::MembershipVersion,
        neighbors: &[crate::model::identity::NodeId],
        now: Instant,
    ) {
        self.entries.retain(|node, (version, until)| {
            *version == membership && *until > now && neighbors.contains(node)
        });
    }

    fn observe(
        &mut self,
        node: crate::model::identity::NodeId,
        membership: crate::model::identity::MembershipVersion,
        now: Instant,
    ) {
        // Bound even completions from different retained memberships. This is a
        // brief routing hint, not a failed-node circuit or an acquisition retry.
        if self.entries.len() == 2 * crate::topology::graph::RADIX
            && !self.entries.contains_key(&node)
        {
            let oldest = self
                .entries
                .iter()
                .min_by_key(|(_, (_, until))| *until)
                .map(|(node, _)| node.clone())
                .unwrap();
            self.entries.remove(&oldest);
        }
        self.entries
            .insert(node, (membership, now + Duration::from_millis(250)));
    }
}

#[cfg(test)]
mod congestion_tests {
    use super::*;
    use crate::model::identity::{MembershipVersion, NodeId};

    #[test]
    fn congestion_expires_and_stays_bounded_across_membership_changes() {
        let mut congestion = NeighborCongestion::default();
        let now = Instant::now();
        let nodes: Vec<_> = (0..40).map(|i| NodeId(i.to_string())).collect();
        for node in &nodes {
            congestion.observe(node.clone(), MembershipVersion(1), now);
        }
        assert_eq!(congestion.entries.len(), 36);
        congestion.retain(
            MembershipVersion(1),
            &nodes,
            now + Duration::from_millis(249),
        );
        assert_eq!(congestion.entries.len(), 36);
        congestion.retain(
            MembershipVersion(1),
            &nodes,
            now + Duration::from_millis(250),
        );
        assert!(congestion.entries.is_empty());
        congestion.observe(nodes[0].clone(), MembershipVersion(1), now);
        congestion.retain(MembershipVersion(2), &nodes, now);
        assert!(congestion.entries.is_empty());
        congestion.observe(nodes[0].clone(), MembershipVersion(2), now);
        congestion.retain(MembershipVersion(2), &nodes[1..], now);
        assert!(congestion.entries.is_empty());
    }
}
#[cfg(test)]
mod tests { /* Transit-only buffers, reverse-link failure, budgets, opaque credentials. */
}
