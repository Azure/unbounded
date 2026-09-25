//! Bounded authenticated neighbor QPs, deterministic duplicate resolution, draining.
use super::{device::Devices, verbs::QueuePairHandle};
use crate::{
    error::{Operation, deferred},
    security::certificates::VerifiedPeer,
    topology::rails::RailId,
};
use std::rc::Rc;
pub struct Sessions {
    devices: Rc<Devices>,
    per_neighbor: usize,
}
pub struct SessionLease {
    qp: QueuePairHandle,
    peer: VerifiedPeer,
    rail: RailId,
}
/// Wire parameters are untrusted until signed handshake validation binds the peer,
/// full-path rail plan, transfer capabilities, and all QP parameters.
pub struct SetupParameters {
    pub rail: RailId,
    pub encoded: Vec<u8>,
}
impl Sessions {
    pub fn new(devices: Rc<Devices>, per_neighbor: usize) -> Self {
        Self {
            devices,
            per_neighbor,
        }
    }
    pub fn establish(
        &self,
        _peer: VerifiedPeer,
        _setup: SetupParameters,
    ) -> Operation<'_, SessionLease> {
        deferred("rdma.establish")
    }
    pub fn drain(&self) -> Operation<'_, ()> {
        deferred("rdma.drain")
    }
}
#[cfg(test)]
mod tests { /* Peer binding, session loss, neighbor churn under combined caps. */
}
