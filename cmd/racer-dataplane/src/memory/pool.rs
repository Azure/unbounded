//! Bounded immutable page leases. No buffers contain request credentials.
use crate::{
    error::{Result, pending},
    model::{envelope::PageEnvelope, identity::PageId},
    runtime::admission::{Admission, Reservation},
};
use std::{rc::Rc, sync::Arc};

pub struct BufferPool {
    admission: Rc<Admission>,
}
/// Mutable staging buffer, not proof of authentication and not client-deliverable.
/// Fixed-size owned backing stays at the same address when this owner moves.
pub struct PlaintextBuffer {
    bytes: Box<[u8]>,
    reservation: Reservation,
}
impl crate::runtime::reactor::sealed::Sealed for PlaintextBuffer {}
impl crate::runtime::reactor::IoBuffer for PlaintextBuffer {
    fn bytes(&self) -> Result<&[u8]> {
        pending("pool.plaintext_bytes")
    }
    fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        pending("pool.plaintext_bytes_mut")
    }
}
/// Only page authentication/origin validation may create this publishable type.
#[derive(Clone)]
pub struct VerifiedPage {
    pub(crate) inner: Arc<VerifiedBytes>,
}
pub(crate) struct VerifiedBytes {
    pub page: PageId,
    pub bytes: Vec<u8>,
    pub reservation: Reservation,
}
#[derive(Clone)]
pub struct CiphertextPage {
    pub(crate) inner: Arc<CiphertextBytes>,
}
pub(crate) struct CiphertextBytes {
    pub envelope: PageEnvelope,
    pub bytes: Vec<u8>,
    pub reservation: Reservation,
}
impl BufferPool {
    pub fn new(admission: Rc<Admission>) -> Self {
        Self { admission }
    }
    pub fn plaintext(&self, _reservation: Reservation, _length: usize) -> Result<PlaintextBuffer> {
        pending("pool.plaintext")
    }
    pub fn ciphertext(
        &self,
        _reservation: Reservation,
        _envelope: PageEnvelope,
        _bytes: Vec<u8>,
    ) -> Result<CiphertextPage> {
        pending("pool.ciphertext")
    }
}
impl VerifiedPage {
    pub fn page(&self) -> &PageId {
        &self.inner.page
    }
    pub fn bytes(&self) -> &[u8] {
        &self.inner.bytes
    }
}
impl CiphertextPage {
    pub fn envelope(&self) -> &PageEnvelope {
        &self.inner.envelope
    }
    pub fn bytes(&self) -> &[u8] {
        &self.inner.bytes
    }
}
#[cfg(test)]
mod tests { /* Bound pools, charge once, retain leases through kernel completion. */
}
