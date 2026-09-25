//! Bounded NIC-local registered allocations retained until all DMA fences complete.
use super::device::Devices;
use crate::{
    error::{Result, pending},
    runtime::admission::Admission,
    topology::rails::RailId,
};
use std::rc::Rc;
pub struct RegisteredPool {
    devices: Rc<Devices>,
    admission: Rc<Admission>,
}
pub struct RegisteredLease {
    region: u64,
    offset: usize,
    length: usize,
}
impl RegisteredPool {
    pub fn new(devices: Rc<Devices>, admission: Rc<Admission>) -> Self {
        Self { devices, admission }
    }
    pub fn acquire(&self, _rail: RailId, _length: usize) -> Result<RegisteredLease> {
        pending("rdma.registered_acquire")
    }
}
#[cfg(test)]
mod tests { /* Registration caps, shared physical accounting, quarantine before reuse. */
}
