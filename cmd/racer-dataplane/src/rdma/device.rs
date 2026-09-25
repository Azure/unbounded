//! Explicit administrator rail mapping. Discovery never invents fabric alignment.
use super::verbs::{DeviceHandle, QueuePairHandle, Verbs};
use crate::{
    error::{Error, Result},
    topology::rails::RailId,
};
use std::{cell::RefCell, rc::Rc};

pub struct Devices {
    verbs: Rc<Verbs>,
    selected: RefCell<Vec<Device>>,
}
#[derive(Clone)]
pub struct Device {
    pub(crate) handle: Rc<DeviceHandle>,
    pub rail: RailId,
}

/// Match an authenticated membership rail to an actual local port. GID index zero
/// is the native v1 profile; unsupported RoCE GID configurations select HTTP.
pub struct RailPort {
    pub rail: RailId,
    pub device: String,
    pub port: u8,
    pub gid: [u8; 16],
}

impl Devices {
    pub fn new(verbs: Rc<Verbs>) -> Self {
        Self {
            verbs,
            selected: RefCell::new(Vec::new()),
        }
    }
    /// Explicit startup I/O. Empty configuration disables RDMA. Failure leaves
    /// no partially published mappings. Constructors have no native side effects.
    pub fn configure(&self, mappings: &[RailPort]) -> Result<()> {
        if !self.selected.borrow().is_empty() {
            return Err(Error::InvalidConfiguration);
        }
        if mappings.is_empty() {
            return Ok(());
        }
        if mappings.len() > 64 {
            return Err(Error::InvalidConfiguration);
        }
        let handles: Vec<_> = self.verbs.discover()?.into_iter().map(Rc::new).collect();
        let mut selected = Vec::new();
        for mapping in mappings {
            if selected.iter().any(|d: &Device| d.rail == mapping.rail) {
                return Err(Error::InvalidConfiguration);
            }
            let handle = handles
                .iter()
                .find(|d| {
                    d.name == mapping.device
                        && d.endpoint.port == mapping.port
                        && d.endpoint.gid == mapping.gid
                })
                .ok_or(Error::Unavailable)?
                .clone();
            // Probe real PD/CQ/QP allocation. Type-2 allocation is tested here too;
            // the eventual bind completion remains mandatory before advertising.
            let qp = QueuePairHandle::new(handle.clone())?;
            qp.probe_window()?;
            qp.stop()?;
            selected.push(Device {
                handle,
                rail: mapping.rail,
            });
        }
        *self.selected.borrow_mut() = selected;
        Ok(())
    }
    pub fn select(&self, rail: RailId) -> Result<Device> {
        self.selected
            .borrow()
            .iter()
            .find(|d| d.rail == rail)
            .cloned()
            .ok_or(Error::Unavailable)
    }
    pub fn ready(&self, rail: RailId) -> bool {
        self.select(rail).is_ok()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn unconfigured_rails_require_http() {
        let devices = Devices::new(Rc::new(Verbs));
        assert!(!devices.ready(RailId(0)));
        assert!(devices.select(RailId(0)).is_err());
        assert!(devices.configure(&[]).is_ok());
        assert!(!devices.ready(RailId(0)));
    }
}
