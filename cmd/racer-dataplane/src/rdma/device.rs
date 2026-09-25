//! Match discovered ports to trusted local fabric associations and publication.
//! Fabric strings are opaque labels: a GID or enumeration order is never a label.
use super::{
    backend,
    lifecycle::IoPort,
    verbs::{DeviceHandle, Verbs},
};
use crate::{
    error::{Error, Operation, Result},
    model::limits::ResourceClass,
    runtime::{admission::Admission, deadline::RequestScope},
    topology::rails::{RailId, RailMapping},
};
use std::{cell::RefCell, rc::Rc};

pub struct Devices {
    _verbs: Rc<Verbs>,
    port: RefCell<Option<Rc<IoPort>>>,
    selected: RefCell<Vec<Device>>,
    mappings: RefCell<Vec<RailMapping>>,
}
#[derive(Clone)]
pub struct Device {
    pub(crate) handle: Rc<DeviceHandle>,
    pub rail: RailId,
}
#[derive(Clone, Debug)]
pub struct RailPort {
    pub rail: RailId,
    pub device: String,
    pub port: u8,
    pub gid: [u8; 16],
}
/// Administrator-provided local association. Publication only contains an opaque
/// fabric name and NUMA hint; it cannot identify a physical NIC by itself.
#[derive(Clone, Debug)]
pub struct FabricPort {
    pub fabric: String,
    pub device: String,
    pub port: u8,
    pub gid: Option<[u8; 16]>,
}
#[derive(Clone, Debug)]
pub struct DiscoveredPort {
    pub device: String,
    pub port: u8,
    pub gid: [u8; 16],
    pub numa_node: Option<usize>,
}
pub(crate) fn discovered_port(device: &backend::DeviceHandle) -> Result<DiscoveredPort> {
    Ok(DiscoveredPort {
        device: device.name.clone(),
        port: device.endpoint.port,
        gid: device.endpoint.gid,
        numa_node: device.numa_node(),
    })
}
/// Pure deterministic matching used by native activation. An absent association,
/// duplicate candidate, reused physical port or mismatched NUMA fails closed.
pub fn match_publication(
    publication: &[RailMapping],
    associations: &[FabricPort],
    discovered: &[DiscoveredPort],
) -> Result<Vec<(RailMapping, usize)>> {
    if publication.len() > 64 || associations.len() > 64 || discovered.len() > 64 {
        return Err(Error::InvalidConfiguration);
    }
    let mut result: Vec<(RailMapping, usize)> = Vec::new();
    for published in publication {
        if published.fabric.is_empty() {
            return Err(Error::InvalidConfiguration);
        }
        if result.iter().any(|(r, _)| r.rail == published.rail) {
            return Err(Error::InvalidConfiguration);
        }
        let mappings: Vec<_> = associations
            .iter()
            .filter(|a| a.fabric == published.fabric)
            .collect();
        if mappings.len() != 1 {
            return Err(Error::Unavailable);
        }
        let mapping = mappings[0];
        if mapping.port == 0 || mapping.device.is_empty() {
            return Err(Error::InvalidConfiguration);
        }
        let candidates: Vec<_> = discovered
            .iter()
            .enumerate()
            .filter(|(_, d)| {
                d.device == mapping.device
                    && d.port == mapping.port
                    && d.gid != [0; 16]
                    && mapping.gid.is_none_or(|gid| d.gid == gid)
                    && published
                        .numa_node
                        .is_none_or(|numa| d.numa_node == Some(numa))
            })
            .collect();
        if candidates.len() != 1 || result.iter().any(|(_, index)| *index == candidates[0].0) {
            return Err(Error::Unavailable);
        }
        let mut actual = published.clone();
        actual.numa_node = candidates[0].1.numa_node;
        result.push((actual, candidates[0].0));
    }
    Ok(result)
}
impl Devices {
    pub fn new(verbs: Rc<Verbs>) -> Self {
        Self {
            _verbs: verbs,
            port: RefCell::new(None),
            selected: RefCell::new(Vec::new()),
            mappings: RefCell::new(Vec::new()),
        }
    }
    pub fn attach(&self, port: IoPort) -> Result<()> {
        if self.port.borrow().is_some() {
            return Err(Error::InvalidConfiguration);
        }
        *self.port.borrow_mut() = Some(Rc::new(port));
        Ok(())
    }
    /// Allocate quota on I/O, then discover/register/provision on the paired native
    /// service. Awaiting this operation performs no filesystem or native syscall.
    pub fn activate<'a>(
        &'a self,
        publication: Vec<RailMapping>,
        associations: Vec<FabricPort>,
        admission: &'a Admission,
        bytes_per_slot: usize,
        scope: &'a RequestScope,
    ) -> Operation<'a, Vec<RailMapping>> {
        Box::pin(async move {
            scope.check()?;
            if bytes_per_slot == 0 || bytes_per_slot > super::registered::MAX_CIPHERTEXT {
                return Err(Error::InvalidConfiguration);
            }
            let port = self.port.borrow().clone().ok_or(Error::Unavailable)?;
            let charge = bytes_per_slot
                .checked_add(4095)
                .and_then(|n| (n & !4095).checked_mul(2))
                .ok_or(Error::Overloaded)?;
            // One native registered allocation plus one bounded handoff staging
            // allocation per slot. Both remain charged through native quarantine.
            let quotas = (0..port.capacity())
                .map(|_| admission.reserve(None, ResourceClass::Registered, charge))
                .collect::<Result<Vec<_>>>()?;
            port.configure(publication, associations, quotas, bytes_per_slot)?;
            struct ActivationGuard<'a> {
                port: &'a IoPort,
                completed: bool,
            }
            impl Drop for ActivationGuard<'_> {
                fn drop(&mut self) {
                    if !self.completed {
                        self.port.close();
                    }
                }
            }
            let mut guard = ActivationGuard {
                port: &port,
                completed: false,
            };
            let cancel = scope.cancellation.subscribe()?;
            let mappings = futures::future::poll_fn(|cx| {
                port.register_driver(cx.waker());
                cancel.register(cx.waker());
                if port
                    .shared
                    .closed
                    .load(std::sync::atomic::Ordering::Acquire)
                {
                    return std::task::Poll::Ready(Err(Error::Unavailable));
                }
                if let Err(error) = scope.check() {
                    port.close();
                    return std::task::Poll::Ready(Err(error));
                }
                port.activation()
                    .map_or(std::task::Poll::Pending, std::task::Poll::Ready)
            })
            .await?;
            guard.completed = true;
            *self.selected.borrow_mut() = mappings
                .iter()
                .map(|mapping| Device {
                    handle: Rc::new(DeviceHandle {
                        port: port.clone(),
                        rail: mapping.rail,
                    }),
                    rail: mapping.rail,
                })
                .collect();
            *self.mappings.borrow_mut() = mappings.clone();
            Ok(mappings)
        })
    }
    /// Old synchronous activation cannot safely provision a serving worker.
    pub fn configure(&self, mappings: &[RailPort]) -> Result<()> {
        if mappings.is_empty() {
            Ok(())
        } else {
            Err(Error::Unavailable)
        }
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
            && self.port.borrow().as_ref().is_some_and(|port| {
                !port
                    .shared
                    .closed
                    .load(std::sync::atomic::Ordering::Acquire)
            })
    }
    pub fn close(&self) {
        if let Some(port) = self.port.borrow().as_ref() {
            port.close();
        }
    }
    /// Call on every local membership publication. A mapping change revokes all
    /// old capabilities; rebuild the lifecycle pair before activating new rails.
    pub fn revalidate(&self, published: &[RailMapping], alignment_enabled: bool) -> bool {
        let actual = self.mappings.borrow();
        let valid = alignment_enabled
            && !actual.is_empty()
            && actual.len() == published.len()
            && actual.iter().all(|a| {
                published.iter().any(|p| {
                    p.rail == a.rail
                        && p.fabric == a.fabric
                        && p.numa_node.is_none_or(|numa| a.numa_node == Some(numa))
                })
            });
        if !valid {
            self.close();
            self.selected.borrow_mut().clear();
        }
        valid
    }
    pub fn register_driver(&self, waker: &std::task::Waker) {
        if let Some(port) = self.port.borrow().as_ref() {
            port.register_driver(waker);
        }
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
    #[test]
    fn discovery_never_invents_fabric_matches_and_rejects_ambiguity() {
        let publication = vec![RailMapping {
            rail: RailId(7),
            fabric: "fabric-a".into(),
            numa_node: Some(1),
        }];
        let mapping = FabricPort {
            fabric: "fabric-a".into(),
            device: "mlx5_0".into(),
            port: 1,
            gid: None,
        };
        let port = DiscoveredPort {
            device: "mlx5_0".into(),
            port: 1,
            gid: [1; 16],
            numa_node: Some(1),
        };
        assert!(match_publication(&publication, &[], &[port.clone()]).is_err());
        assert!(
            match_publication(
                &publication,
                &[mapping.clone(), mapping.clone()],
                &[port.clone()]
            )
            .is_err()
        );
        assert!(
            match_publication(
                &publication,
                &[mapping.clone()],
                &[port.clone(), port.clone()]
            )
            .is_err()
        );
        assert!(
            match_publication(
                &publication,
                &[mapping.clone()],
                &[DiscoveredPort {
                    numa_node: Some(0),
                    ..port.clone()
                }]
            )
            .is_err()
        );
        assert_eq!(
            match_publication(&publication, &[mapping], &[port]).unwrap()[0].0,
            publication[0]
        );
    }
}
