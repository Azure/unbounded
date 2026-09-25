//! Registered allocations carry their physical quota through the terminal fence.
use super::{
    device::Devices,
    session::SessionLease,
    verbs::{DeviceHandle, Region},
};
use crate::{
    error::{Error, Result},
    model::limits::ResourceClass,
    runtime::admission::Admission,
    topology::rails::RailId,
};
use std::rc::Rc;

pub const MAX_CIPHERTEXT: usize = 16 * 1024 * 1024 + 16;
pub struct RegisteredPool {
    devices: Rc<Devices>,
    admission: Rc<Admission>,
}
pub struct RegisteredLease {
    pub(crate) region: Rc<Region>,
    pub(crate) rail: RailId,
}
impl RegisteredPool {
    pub fn new(devices: Rc<Devices>, admission: Rc<Admission>) -> Self {
        Self { devices, admission }
    }
    fn on_device(
        &self,
        device: Rc<DeviceHandle>,
        rail: RailId,
        length: usize,
    ) -> Result<RegisteredLease> {
        if length == 0 || length > MAX_CIPHERTEXT {
            return Err(Error::InvalidRange);
        }
        let quota =
            self.admission
                .reserve(None, ResourceClass::Registered, registered_charge(length)?)?;
        let region = Region::new(device, length, Box::new(quota))?;
        Ok(RegisteredLease { region, rail })
    }
    pub fn acquire(&self, rail: RailId, length: usize) -> Result<RegisteredLease> {
        self.on_device(self.devices.select(rail)?.handle, rail, length)
    }
    pub fn acquire_for(&self, session: &SessionLease, length: usize) -> Result<RegisteredLease> {
        self.on_device(session.qp.device().clone(), session.rail(), length)
    }
}

fn registered_charge(length: usize) -> Result<usize> {
    if length == 0 || length > MAX_CIPHERTEXT {
        return Err(Error::InvalidRange);
    }
    // Native allocation is 4 KiB aligned. Charge all pinned pages, including a
    // short final page, rather than just the remotely visible byte range.
    length
        .checked_add(4095)
        .map(|n| n & !4095)
        .ok_or(Error::Overloaded)
}
impl RegisteredLease {
    pub fn len(&self) -> usize {
        self.region.length()
    }
    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }
    pub fn copy_from(&mut self, ciphertext: &[u8]) -> Result<()> {
        self.region.copy_from(ciphertext)
    }
    pub fn to_vec(&self) -> Result<Vec<u8>> {
        self.region.copy_to()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn registered_quota_accounts_for_short_and_final_physical_pages() {
        assert_eq!(registered_charge(1), Ok(4096));
        assert_eq!(registered_charge(4096), Ok(4096));
        assert_eq!(registered_charge(4097), Ok(8192));
        assert_eq!(
            registered_charge(MAX_CIPHERTEXT),
            Ok(16 * 1024 * 1024 + 4096)
        );
        assert_eq!(registered_charge(0), Err(Error::InvalidRange));
        assert_eq!(registered_charge(usize::MAX), Err(Error::InvalidRange));
    }
}
