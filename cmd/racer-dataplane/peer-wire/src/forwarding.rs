//! Canonical monotonic route state and independently signed forwarding heads.

use crate::*;

/// Validated route fields, independent of application topology and request types.
#[derive(PartialEq, Eq)]
pub struct RouteState {
    membership: u64,

    request: String,

    attempt: String,

    destination: NodeId,

    /// Already visited nodes, in order.
    pub visited: Vec<NodeId>,

    /// Remaining signed link budget.
    pub links: u64,

    /// Remaining signed origin-attempt ceiling.
    pub attempts: u32,

    /// Original absolute authority deadline in milliseconds.
    pub deadline: u64,
}

impl RouteState {
    /// Parse canonical fields and reject invalid identifiers or exhausted budgets.
    pub fn from_head(head: &MessageHead) -> Result<Self> {
        let state = Self {
            membership: number(head, "racer-route-membership")?,
            request: field(head, "racer-route-request")?,
            attempt: field(head, "racer-route-attempt")?,
            destination: node_field(head, "racer-route-destination")?,
            visited: decode_nodes(field(head, "racer-route-visited")?.as_bytes())?,
            links: number(head, "racer-route-links")?,
            attempts: number(head, "racer-route-attempts")?
                .try_into()
                .map_err(|_| Error::Unauthorized)?,
            deadline: number(head, "racer-route-deadline")?,
        };
        if state.membership == 0
            || decode_binary(state.request.as_bytes())?.len() != 16
            || decode_binary(state.attempt.as_bytes())?.len() != 16
        {
            return Err(Error::Unauthorized);
        }
        if state.links == 0
            || state.links > MAX_HOPS as u64
            || state.visited.is_empty()
            || state.visited.len() + state.links as usize > MAX_HOPS + 1
        {
            return Err(Error::HopBudgetExhausted);
        }
        Ok(state)
    }

    /// Require exactly one consumed link with no authority or attempt refill.
    pub fn transition(&self, next: &Self, signer: &NodeId) -> Result<()> {
        let mut visited = self.visited.clone();
        visited.push(signer.clone());
        if self.links <= 1
            || next.links != self.links - 1
            || next.attempts > self.attempts
            || next.deadline > self.deadline
            || next.membership != self.membership
            || next.request != self.request
            || next.attempt != self.attempt
            || next.destination != self.destination
            || self.visited.contains(signer)
            || signer == &self.destination
            || next.visited != visited
        {
            return Err(Error::HopBudgetExhausted);
        }
        Ok(())
    }
}

/// Bind a hop to the exact original and preceding signatures.
pub fn hop_head(kind: &str, original: &SignedHead, previous: &SignedHead) -> Result<MessageHead> {
    let mut head = MessageHead {
        start: StartLine::Request {
            method: "POST".into(),
            target: "/racer/peer/v1/hop".into(),
        },
        headers: Vec::new(),
    };
    push(&mut head, "racer-kind", kind);
    push(&mut head, "content-length", 0);
    push_binary(&mut head, "racer-original", &signed_digest(original)?);
    push_binary(&mut head, "racer-previous", &signed_digest(previous)?);
    Ok(head)
}

/// Check exact hop fields after the caller verifies identity and route transition.
pub fn check_hop(
    hop: &SignedHead,
    kind: &str,
    original: &SignedHead,
    previous: &SignedHead,
    route: &RouteState,
) -> Result<()> {
    let mut expected = hop_head(kind, original, previous)?;
    for name in [
        "racer-route-membership",
        "racer-route-request",
        "racer-route-attempt",
        "racer-route-destination",
        "racer-route-visited",
        "racer-route-links",
        "racer-route-attempts",
        "racer-route-deadline",
    ] {
        push(&mut expected, name, field(&hop.head, name)?);
    }
    if route.visited.contains(&receiver(&hop.head)?) {
        return Err(Error::Unauthorized);
    }
    agrees(&hop.head, &expected, false)
}

/// Bind a reverse hop to its original request and recorded forward path.
pub fn response_hop_head(
    original: &SignedHead,
    previous: &SignedHead,
    request: &SignedHead,
    path: &[NodeId],
    index: usize,
) -> Result<MessageHead> {
    let mut head = hop_head("response-hop", original, previous)?;
    push_binary(&mut head, "racer-request-binding", &signed_digest(request)?);
    push(&mut head, "racer-response-path", nodes(path)?);
    push(&mut head, "racer-reverse-index", index);
    Ok(head)
}
