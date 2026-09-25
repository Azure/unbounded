//! Native lifecycle endpoints share the existing I/O/crypto worker pair.
use super::*;
use crate::rdma::{
    device::FabricPort,
    lifecycle::{IoPort, NativePort, WithNative},
};
use std::collections::HashMap;

#[derive(Default)]
pub(super) struct NativePairs {
    ports: Mutex<HashMap<WorkerId, (Option<IoPort>, Option<NativePort>)>>,
}
impl NativePairs {
    pub(super) fn prepare(
        &self,
        workers: impl Iterator<Item = WorkerId>,
        limits: &Limits,
    ) -> Result<()> {
        let bytes = crate::rdma::registered::MAX_CIPHERTEXT;
        let charge = ((bytes + 4095) & !4095) * 2;
        let slots = (limits.registered_bytes.get() / charge)
            .min(limits.queue_entries.get())
            .min(256);
        // Insufficient optional native capacity uses HTTP without inventing quota.
        if slots == 0 {
            return Ok(());
        }
        let mut ports = self.ports.lock().map_err(|_| Error::Unavailable)?;
        for worker in workers {
            let (io, native) = crate::rdma::lifecycle::pair(slots)?;
            ports.insert(worker, (Some(io), Some(native)));
        }
        Ok(())
    }
    pub(super) fn io(&self, worker: WorkerId) -> Result<Option<IoPort>> {
        Ok(self
            .ports
            .lock()
            .map_err(|_| Error::Unavailable)?
            .get_mut(&worker)
            .and_then(|pair| pair.0.take()))
    }
    pub(super) fn crypto(
        &self,
        worker: WorkerId,
        engine: PageCryptoEngine,
    ) -> Result<Box<dyn CryptoService>> {
        let native = self
            .ports
            .lock()
            .map_err(|_| Error::Unavailable)?
            .get_mut(&worker)
            .and_then(|pair| pair.1.take());
        Ok(match native {
            Some(native) => Box::new(WithNative::new(engine, native)),
            None => Box::new(engine),
        })
    }
}
impl Application {
    /// Trusted physical associations only. Rail IDs and alignment remain exclusively
    /// controller-owned. No I/O, native discovery, or extra threads are created.
    pub fn with_fabric_ports(mut self, ports: Vec<FabricPort>) -> Result<Self> {
        if ports.len() > 64
            || ports
                .iter()
                .any(|p| p.fabric.is_empty() || p.device.is_empty() || p.port == 0)
        {
            return Err(Error::InvalidConfiguration);
        }
        self.fabric_ports = ports;
        Ok(self)
    }
}
impl WorkerApplication {
    pub(super) async fn activate_native(&mut self, startup: &RequestScope) -> Result<()> {
        let Some(devices) = &self.devices else {
            return Ok(());
        };
        let snapshot = self.snapshots.current()?;
        let member = snapshot.membership.member(self.keys.node())?;
        if !member.alignment_enabled || member.rails.is_empty() || self.fabric_ports.is_empty() {
            return Ok(());
        }
        match devices
            .activate(
                member.rails.clone(),
                self.fabric_ports.clone(),
                &self.runtime.admission,
                crate::rdma::registered::MAX_CIPHERTEXT,
                startup,
            )
            .await
        {
            Ok(actual) => {
                self.actual_rails = actual;
            }
            Err(Error::Cancelled | Error::DeadlineExceeded) => {
                startup.check()?;
            }
            Err(_) => {
                devices.close();
            }
        }
        Ok(())
    }
}
