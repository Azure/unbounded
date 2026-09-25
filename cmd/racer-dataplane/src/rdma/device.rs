//! Discover ports, protection domains, CQs, NUMA locality, and scoped-grant support.
use super::verbs::{DeviceHandle, Verbs};
use crate::{
    error::{Result, pending},
    topology::rails::RailId,
};
use std::rc::Rc;
pub struct Devices {
    verbs: Rc<Verbs>,
}
pub struct Device {
    handle: DeviceHandle,
    rail: RailId,
}
impl Devices {
    pub fn new(verbs: Rc<Verbs>) -> Self {
        Self { verbs }
    }
    pub fn select(&self, _rail: RailId) -> Result<Device> {
        pending("rdma.select_device")
    }
}
#[cfg(test)]
mod tests { /* Missing device, NUMA mapping, unsupported permission isolation. */
}
