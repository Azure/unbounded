//! Opaque bounded transit with recorded reverse-path responses, never a page cache.
//!
//! No decryption service is injected here. Reverse-link failure terminates the
//! attempt; responses are not independently rerouted. Preserve encrypted credentials.
use super::{
    transfer::Transfers,
    wire::{PeerRequest, PeerResponse},
};
use crate::{
    error::{Operation, deferred},
    runtime::{admission::Admission, deadline::RequestScope},
    security::forwarding::Forwarding,
    topology::paths::Paths,
};
use std::rc::Rc;
pub struct Relay {
    paths: Rc<Paths>,
    forwarding: Rc<Forwarding>,
    transfers: Rc<Transfers>,
    admission: Rc<Admission>,
}
impl Relay {
    pub fn new(
        paths: Rc<Paths>,
        forwarding: Rc<Forwarding>,
        transfers: Rc<Transfers>,
        admission: Rc<Admission>,
    ) -> Self {
        Self {
            paths,
            forwarding,
            transfers,
            admission,
        }
    }
    pub fn forward<'a>(
        &'a self,
        _request: PeerRequest,
        _scope: &'a RequestScope,
    ) -> Operation<'a, PeerResponse> {
        deferred("peer.relay")
    }
}
#[cfg(test)]
mod tests { /* Transit-only buffers, reverse-link failure, budgets, opaque credentials. */
}
