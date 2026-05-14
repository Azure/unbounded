// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use crate::bufferpool::{NodeId, Req, StripeKey, TraceCtx};
use std::time::Instant;

#[derive(Clone, Debug)]
pub struct P2pReq {
    pub key: StripeKey,
    pub deadline: Instant,
    pub trace: TraceCtx,
    pub path: Vec<NodeId>,
}

impl P2pReq {
    pub fn new(key: StripeKey, deadline: Instant, trace: TraceCtx, path: Vec<NodeId>) -> Self {
        Self {
            key,
            deadline,
            trace,
            path,
        }
    }

    /// The next hop, if any. `None` means "fetch from regional storage".
    pub fn next_hop(&self) -> Option<NodeId> {
        self.path.first().copied()
    }

    /// Pop the head of `path` and return a forwardable request for
    /// the next relay.
    pub fn pop_hop(mut self) -> (Option<NodeId>, Self) {
        let head = if self.path.is_empty() {
            None
        } else {
            Some(self.path.remove(0))
        };
        (head, self)
    }

    /// Inherent accessor; see `designs/bufferpool.md`. Deadlines
    /// live on `P2pReq` itself rather than on the [`Req`] trait
    /// because the pool does not need them for single-flight
    /// coalescing.
    pub fn deadline(&self) -> Instant {
        self.deadline
    }

    /// Inherent accessor for the opaque tracing context.
    pub fn trace(&self) -> &TraceCtx {
        &self.trace
    }
}

impl Req for P2pReq {
    fn key(&self) -> StripeKey {
        self.key
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::Duration;

    #[test]
    fn pop_hop_advances_path() {
        let key = StripeKey([0u8; 32]);
        let now = Instant::now() + Duration::from_secs(1);
        let req = P2pReq::new(
            key,
            now,
            TraceCtx::default(),
            vec![NodeId(2), NodeId(1), NodeId(0)],
        );
        assert_eq!(req.next_hop(), Some(NodeId(2)));
        let (head, rest) = req.pop_hop();
        assert_eq!(head, Some(NodeId(2)));
        assert_eq!(rest.next_hop(), Some(NodeId(1)));
        assert_eq!(rest.path.len(), 2);
    }

    #[test]
    fn empty_path_means_owner_fetch() {
        let key = StripeKey([0u8; 32]);
        let now = Instant::now() + Duration::from_secs(1);
        let req = P2pReq::new(key, now, TraceCtx::default(), Vec::new());
        assert!(req.next_hop().is_none());
    }
}
