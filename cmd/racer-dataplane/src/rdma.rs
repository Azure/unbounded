//! Match discovered ports to trusted local fabric associations and publication.
//! Fabric strings are opaque labels: a GID or enumeration order is never a label.
//!
//! Public contracts always compile; native FFI is gated by `rdma`. Attach bounded
//! lifecycle endpoints, activate devices against publication, and run `WithNative`
//! on the crypto role. I/O turns consume mailboxes and drive session progress.
mod ffi;
pub mod lifecycle;
pub mod permission;
pub mod session;

use self::{
    lifecycle::{DeviceHandle, IoPort, QueuePairHandle, Region, wait},
    permission::{AuthenticatedDescriptor, Grant, completion_bytes},
    session::{SessionLease, Sessions},
};
use crate::{
    error::{Error, Operation, Result},
    memory::pool::{BufferPool, CiphertextPage},
    model::{PageEnvelope, ResourceClass, TransferId},
    runtime::{admission::Admission, deadline::RequestScope},
    security::signing::VerifiedHead,
    topology::rails::{RailId, RailMapping},
};
use base64::{Engine, engine::general_purpose::STANDARD};
use std::{cell::RefCell, future::poll_fn, rc::Rc, task::Poll};

/// Ciphertext-only movement. Failed attempts are fenced before HTTP fallback.
pub struct RdmaTransfer {
    sessions: Rc<Sessions>,
}
/// Produced only by a successful native write CQE. Sign this header as part of
/// the request-bound HTTP control response; the ciphertext is not hashed here.
pub struct SendCompletion {
    binding: [u8; 32],
    transfer: TransferId,
}
impl SendCompletion {
    pub fn header_value(&self) -> Vec<u8> {
        STANDARD
            .encode(completion_bytes(self.binding, self.transfer))
            .into_bytes()
    }
}
struct AbortOnDrop(Rc<QueuePairHandle>);
impl Drop for AbortOnDrop {
    fn drop(&mut self) {
        let _ = self.0.stop();
    }
}
impl RdmaTransfer {
    pub fn register_driver(&self, waker: &std::task::Waker) {
        self.sessions.register_driver(waker);
    }
    pub fn new(sessions: Rc<Sessions>) -> Self {
        Self { sessions }
    }
    pub fn ready(&self, rail: RailId) -> bool {
        self.sessions.ready(rail)
    }
    pub fn progress(&self) -> Result<usize> {
        self.sessions.progress()
    }
    pub fn send_to<'a>(
        &'a self,
        session: &'a SessionLease,
        page: CiphertextPage,
        descriptor: AuthenticatedDescriptor,
        scope: &'a RequestScope,
    ) -> Operation<'a, SendCompletion> {
        Box::pin(async move {
            scope.check()?;
            descriptor.validate(session, page.bytes().len())?;
            validate_envelope(page.envelope())?;
            if page.bytes().len() != page.envelope().ciphertext_length as usize {
                return Err(Error::InvalidRange);
            }
            session.wait_ready(scope).await?;
            session.claim()?;
            session.qp.expire_at(scope.deadline.0);
            let _abort = AbortOnDrop(session.qp.clone());
            let mut buffer = RegisteredLease::acquire(session, page.bytes().len(), scope).await?;
            buffer.copy_from(page.bytes(), scope).await?;
            let ticket = wait(scope, |cx| {
                session.qp.register_waiter(cx);
                session.qp.poll_write(
                    buffer.region.clone(),
                    descriptor.descriptor.address,
                    descriptor.descriptor.scoped_key,
                )
            })
            .await?;
            let cancellation = scope.cancellation.subscribe()?;
            poll_fn(|cx| {
                cancellation.register(cx.waker());
                if let Err(error) = scope.check() {
                    return Poll::Ready(Err(error));
                }
                if let Err(error) = session.progress() {
                    return Poll::Ready(Err(error));
                }
                ticket.poll(cx)
            })
            .await?;
            // Source buffer is safe after its write CQE. Stop this single-use QP
            // before returning a control completion or admitting a fallback.
            // Request termination abandons this wait, not the native owner's
            // quarantine. Only a successful terminal fence permits completion.
            wait(scope, |cx| session.qp.poll_stopped(cx)).await?;
            Ok(SendCompletion {
                binding: session.binding(),
                transfer: descriptor.descriptor.transfer,
            })
        })
    }
    pub fn prepare_receive<'a>(
        &'a self,
        session: &'a SessionLease,
        envelope: &'a PageEnvelope,
        transfer: TransferId,
        scope: &'a RequestScope,
    ) -> Operation<'a, Grant> {
        Box::pin(async move {
            scope.check()?;
            validate_envelope(envelope)?;
            let buffer =
                RegisteredLease::acquire(session, envelope.ciphertext_length as usize, scope)
                    .await?;
            Grant::bind(session, buffer, transfer, scope).await
        })
    }
    /// This handoff requires a signed completion and returns ciphertext only.
    pub fn finish_receive<'a>(
        &'a self,
        session: &'a SessionLease,
        grant: Grant,
        head: &'a VerifiedHead,
        envelope: PageEnvelope,
        admission: &'a Rc<Admission>,
        scope: &'a RequestScope,
    ) -> Operation<'a, CiphertextPage> {
        Box::pin(async move {
            validate_envelope(&envelope)?;
            let reservation = admission.reserve(
                Some(&envelope.page.version.object.cache),
                ResourceClass::Ciphertext,
                envelope.ciphertext_length as usize,
            )?;
            let buffer: RegisteredLease = grant.finish(head, session, scope).await?;
            if buffer.len() != envelope.ciphertext_length as usize {
                return Err(Error::InvalidRange);
            }
            let bytes = buffer.to_vec(scope).await?;
            BufferPool::new(admission.clone()).ciphertext(reservation, envelope, bytes)
        })
    }
    pub fn drain(&self) -> Operation<'_, ()> {
        self.sessions.drain()
    }
    pub fn fence_cut(&self) -> Operation<'static, ()> {
        self.sessions.fence_cut()
    }
}
fn validate_envelope(envelope: &PageEnvelope) -> Result<()> {
    if envelope.plaintext_length == 0
        || envelope.plaintext_length > 16 * 1024 * 1024
        || envelope.ciphertext_length != envelope.plaintext_length + 16
    {
        return Err(Error::InvalidRange);
    }
    Ok(())
}

pub const MAX_CIPHERTEXT: usize = 16 * 1024 * 1024 + 16;
/// Registered allocations carry their physical quota through the terminal fence.
pub struct RegisteredLease {
    pub(crate) region: Rc<Region>,
    pub(crate) rail: RailId,
}
impl RegisteredLease {
    /// Acquire the registered buffer preprovisioned for this session slot.
    pub fn acquire<'a>(
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
/// A native slot owns both a registered buffer and bounded handoff staging.
pub(crate) fn native_slot_charge(length: usize) -> Result<usize> {
    registered_charge(length)?
        .checked_mul(2)
        .ok_or(Error::Overloaded)
}

#[cfg(test)]
mod registered_tests {
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
    #[test]
    fn native_slot_quota_includes_aligned_staging_and_registration() {
        for (length, expected) in [
            (1, 8192),
            (4096, 8192),
            (4097, 16384),
            (MAX_CIPHERTEXT, 32 * 1024 * 1024 + 8192),
        ] {
            assert_eq!(native_slot_charge(length), Ok(expected));
        }
        for length in [0, MAX_CIPHERTEXT + 1, usize::MAX] {
            assert_eq!(native_slot_charge(length), Err(Error::InvalidRange));
        }
    }
}

pub struct Devices {
    port: RefCell<Option<Rc<IoPort>>>,
    selected: RefCell<Vec<Device>>,
    mappings: RefCell<Vec<RailMapping>>,
}
#[derive(Clone)]
pub struct Device {
    pub(crate) handle: Rc<DeviceHandle>,
    pub rail: RailId,
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
pub(crate) fn discovered_port(device: &ffi::NativeDevice) -> Result<DiscoveredPort> {
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
    #[cfg(test)]
    pub(crate) fn test(port: std::rc::Rc<IoPort>) -> Self {
        let devices = Self::new();
        devices.selected.borrow_mut().push(Device {
            handle: std::rc::Rc::new(DeviceHandle {
                port: port.clone(),
                rail: RailId(0),
                generation: port
                    .shared
                    .generation
                    .load(std::sync::atomic::Ordering::Acquire),
            }),
            rail: RailId(0),
        });
        *devices.port.borrow_mut() = Some(port);
        devices
    }
    pub fn new() -> Self {
        Self {
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
            if bytes_per_slot == 0 || bytes_per_slot > MAX_CIPHERTEXT {
                return Err(Error::InvalidConfiguration);
            }
            let port = self.port.borrow().clone().ok_or(Error::Unavailable)?;
            port.reopen()?;
            let charge = native_slot_charge(bytes_per_slot)?;
            // One native registered allocation plus one bounded handoff staging
            // allocation per slot. Both remain charged through native quarantine.
            let quotas = (0..port.capacity())
                .map(|_| admission.reserve(None, ResourceClass::Registered, charge))
                .collect::<Result<Vec<_>>>()?;
            port.configure(publication, associations, quotas, bytes_per_slot, scope)
                .await?;
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
                        generation: port
                            .shared
                            .generation
                            .load(std::sync::atomic::Ordering::Acquire),
                    }),
                    rail: mapping.rail,
                })
                .collect();
            *self.mappings.borrow_mut() = mappings.clone();
            Ok(mappings)
        })
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
    pub fn capacity(&self) -> usize {
        self.port
            .borrow()
            .as_ref()
            .map_or(0, |port| port.capacity())
    }
    /// Call on every local membership publication. A mapping change revokes all
    /// old capabilities; drain the lifecycle generation before activating new rails.
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
        let devices = Devices::new();
        assert!(!devices.ready(RailId(0)));
        assert!(devices.select(RailId(0)).is_err());
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
