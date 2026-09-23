// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Candidate budgets and request-local route selection over shared peer health.
use super::*;

impl Provider {
    pub(super) fn attempt(&mut self, context: String) -> cache::Result<Option<Attempt>> {
        let (Some(state), Some(routing), Some(peer)) = (&self.active, &self.routing, &self.peer)
        else {
            return Ok(None);
        };
        let cursor = state.borrow().cursor.clone();
        let peer = peer.borrow();
        let candidate = routing.destination(&cursor);
        let route = AttemptRoute {
            cursor: cursor.clone(),
            candidate,
            endpoint: peer.http.endpoint.address.tcp().expect("TCP peer"),
            final_hop: routing.last_hop(&cursor),
            context,
        };
        if self.owner_evidence(&cursor) {
            return Err(AttemptFailure {
                route,
                evidence: None,
                reported: true,
            }
            .into());
        }
        Ok(Some(Attempt {
            route,
            owner: Some(self.owners.borrow_mut().acquire_final(
                cursor.identity,
                candidate,
                routing.final_peer(&cursor),
            )?),
        }))
    }

    pub(super) fn owner_evidence(&self, cursor: &crate::routing::Cursor) -> bool {
        let Some(routing) = &self.routing else {
            return false;
        };
        let owners = self.owners.borrow();
        owners
            .evidence(cursor.identity, routing.destination(cursor))
            .is_some()
            || routing
                .final_peer(cursor)
                .is_some_and(|peer| owners.physical_evidence(cursor.identity, peer).is_some())
    }

    pub(super) fn service_end(&self, deadline: Instant) -> Instant {
        let now = crate::environment::now();
        let window = self.active.as_ref().map_or(COOLDOWN, |_| MAX_CANDIDATE);
        (now + window).min(deadline.checked_sub(RETURN_SLACK).unwrap_or(now).max(now))
    }

    /// Create an independently routed request over the shared endpoint registry.
    pub(super) fn routed(&self, state: Option<Rc<RefCell<RouteState>>>) -> Self {
        static NEXT_FLIGHT: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(1);
        let mut request = Self {
            namespace: self.namespace,
            chain: Rc::new(RefCell::new(Chain::default())),
            flight: NEXT_FLIGHT.fetch_add(1, std::sync::atomic::Ordering::Relaxed),
            reply_route: None,
            volume: self.volume.clone(),
            metrics: self.metrics.clone(),
            authentication: self.authentication.clone(),
            max_attempts: self.max_attempts,
            backend: self.backend.clone(),
            peer: self.peer.clone(),
            peers: self.peers.clone(),
            selected: self.selected.clone(),
            routing: self.routing.clone(),
            active: state,
            owners: self.owners.clone(),
            negotiations: self.negotiations.clone(),
        };
        request.select_peer();
        request
    }

    /// A new client page is a new bounded resolution. Retries and transport
    /// recovery retain that page's Provider and never call this constructor.
    pub(super) fn page_provider(&self) -> Self {
        assert!(
            self.reply_route.is_none(),
            "relayed resolutions cannot mint budgets"
        );
        self.routed(
            self.active
                .as_ref()
                .map(|s| Rc::new(RefCell::new(s.borrow().clone()))),
        )
    }

    pub(super) fn select_peer(&mut self) {
        if let Some(routing) = &self.routing {
            let selected = self
                .active
                .as_ref()
                .and_then(|s| routing.next(&s.borrow().cursor).ok().flatten().map(|n| n.0));
            self.peer = selected
                .as_ref()
                .and_then(|id| self.peers.borrow().get(id).cloned());
            self.selected = selected;
        }
    }
}

pub(super) fn candidate_end(now: Instant, caller: Instant, remaining: u32) -> Instant {
    now + (caller
        .saturating_duration_since(now)
        .saturating_sub(RETURN_SLACK)
        / remaining.max(1))
    .min(MAX_CANDIDATE)
}
