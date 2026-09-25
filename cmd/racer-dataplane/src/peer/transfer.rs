//! Transport-neutral ciphertext lifecycle, selecting HTTP or authenticated RDMA.
use crate::{
    error::{Operation, deferred},
    http::{io::HttpIo, pool::HttpPool},
    memory::pool::CiphertextPage,
    rdma::transfer::RdmaTransfer,
    runtime::deadline::RequestScope,
    topology::rails::TransportPlan,
};
use std::rc::Rc;
pub struct Transfers {
    http: Rc<HttpPool>,
    io: Rc<HttpIo>,
    rdma: Option<Rc<RdmaTransfer>>,
}
impl Transfers {
    pub fn new(http: Rc<HttpPool>, io: Rc<HttpIo>, rdma: Option<Rc<RdmaTransfer>>) -> Self {
        Self { http, io, rdma }
    }
    pub fn send<'a>(
        &'a self,
        _destination: &'a crate::model::identity::NodeId,
        _transfer: crate::model::identity::TransferId,
        _plan: TransportPlan,
        _page: CiphertextPage,
        _scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        deferred("peer.transfer")
    }
    pub fn receive<'a>(
        &'a self,
        _source: &'a crate::model::identity::NodeId,
        _transfer: crate::model::identity::TransferId,
        _plan: TransportPlan,
        _envelope: crate::model::envelope::PageEnvelope,
        _scope: &'a RequestScope,
    ) -> Operation<'a, CiphertextPage> {
        deferred("peer.receive")
    }
}
#[cfg(test)]
mod tests { /* HTTP fallback, exact ciphertext preservation, bounded transfer credits. */
}
