//! Ciphertext-only movement and fenced abort. A fallback is a new transfer attempt.
use super::{
    permission::Permissions,
    registered::RegisteredPool,
    session::{SessionLease, Sessions},
};
use crate::{
    error::{Operation, deferred},
    memory::pool::CiphertextPage,
    runtime::deadline::RequestScope,
};
use std::rc::Rc;
pub struct RdmaTransfer {
    sessions: Rc<Sessions>,
    buffers: Rc<RegisteredPool>,
    permissions: Rc<Permissions>,
}
impl RdmaTransfer {
    pub fn new(
        sessions: Rc<Sessions>,
        buffers: Rc<RegisteredPool>,
        permissions: Rc<Permissions>,
    ) -> Self {
        Self {
            sessions,
            buffers,
            permissions,
        }
    }
    pub fn send<'a>(
        &'a self,
        _session: &'a SessionLease,
        _page: CiphertextPage,
        _scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        deferred("rdma.send")
    }
    pub fn drain(&self) -> Operation<'_, ()> {
        deferred("rdma.transfer_drain")
    }
    pub fn receive<'a>(
        &'a self,
        _session: &'a SessionLease,
        _envelope: crate::model::envelope::PageEnvelope,
        _scope: &'a RequestScope,
    ) -> Operation<'a, CiphertextPage> {
        deferred("rdma.receive")
    }
}
#[cfg(test)]
mod tests { /* Completion order, aborted fallback, partial bodies, no new fill rights. */
}
