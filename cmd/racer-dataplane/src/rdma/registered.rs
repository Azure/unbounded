//! Registered allocations carry their physical quota through the terminal fence.
use super::{
    device::Devices,
    session::SessionLease,
    verbs::{Region, wait},
};
use crate::{
    error::{Error, Operation, Result},
    runtime::{admission::Admission, deadline::RequestScope},
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
    pub fn acquire(&self, _rail: RailId, _length: usize) -> Result<RegisteredLease> {
        // Registered resources are preprovisioned and bound to a session slot.
        Err(Error::Unavailable)
    }
    pub fn acquire_for<'a>(
        &'a self,
        session: &'a SessionLease,
        length: usize,
        scope: &'a RequestScope,
    ) -> Operation<'a, RegisteredLease> {
        Box::pin(async move {
            registered_charge(length)?;
            let region = wait(scope, |cx| {
                session.qp.register_waiter(cx);
                Region::poll_acquire(&session.qp, length)
            })
            .await?;
            Ok(RegisteredLease {
                region,
                rail: session.rail(),
            })
        })
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
    pub fn copy_from<'a>(
        &'a mut self,
        ciphertext: &'a [u8],
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        Box::pin(wait(scope, |cx| {
            self.region.register_waiter(cx);
            self.region.poll_copy_from(ciphertext)
        }))
    }
    pub fn to_vec<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, Vec<u8>> {
        Box::pin(wait(scope, |cx| self.region.poll_copy_to(cx)))
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
