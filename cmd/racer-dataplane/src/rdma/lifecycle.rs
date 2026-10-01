//! Bounded native service for the EXISTING paired crypto thread. No thread is
//! spawned here. I/O only tries mailboxes; native calls and destruction run here.
pub use super::ffi::Endpoint;
use super::{FabricPort, ffi, match_publication};
use crate::{
    error::{Error, Result},
    runtime::{admission::Reservation, deadline::RequestScope, worker::CryptoService},
    topology::rails::{RailId, RailMapping},
};
use futures::task::AtomicWaker;
use std::{
    cell::{Cell, RefCell},
    rc::Rc,
    sync::{
        Arc, Mutex, MutexGuard, TryLockError,
        atomic::{AtomicBool, AtomicU8, AtomicU64, Ordering},
    },
    task::{Context, Poll, Waker, ready},
};

/// Test-only native fabric; enter its scope before constructing the crypto service.
#[cfg(test)]
pub use super::ffi::simulation;

// I/O-local proxies only access mailboxes. Native owners stay on the paired role.
// Contention is not admission failure. Retain owners across Pending; bounded worker
// ticks cover unlocks without a completion wake.
pub(crate) fn try_mailbox<T>(mailbox: &Mutex<T>) -> Poll<Result<MutexGuard<'_, T>>> {
    match mailbox.try_lock() {
        Ok(guard) => Poll::Ready(Ok(guard)),
        Err(TryLockError::WouldBlock) => Poll::Pending,
        Err(TryLockError::Poisoned(_)) => Poll::Ready(Err(Error::Io)),
    }
}
pub(crate) async fn wait<T>(
    scope: &RequestScope,
    mut poll: impl FnMut(&mut Context<'_>) -> Poll<Result<T>>,
) -> Result<T> {
    let cancellation = scope.cancellation.subscribe()?;
    std::future::poll_fn(|cx| {
        cancellation.register(cx.waker());
        scope.check()?;
        poll(cx)
    })
    .await
}
#[cfg(test)]
fn immediate<T>(poll: Poll<Result<T>>) -> Result<T> {
    match poll {
        Poll::Ready(result) => result,
        Poll::Pending => Err(Error::Overloaded),
    }
}
pub struct DeviceHandle {
    pub(crate) port: Rc<IoPort>,
    pub(crate) rail: RailId,
    pub(crate) generation: u64,
}
struct Lease {
    slot: Arc<Slot>,
    shared: Arc<Shared>,
}
impl Drop for Lease {
    fn drop(&mut self) {
        self.slot.cancel.store(true, Ordering::Release);
        self.slot.released.store(true, Ordering::Release);
        self.shared.engine.wake();
    }
}
pub(crate) struct Region {
    lease: Rc<Lease>,
    length: usize,
}
impl Region {
    #[cfg(test)]
    pub(crate) fn acquire(qp: &QueuePairHandle, length: usize) -> Result<Rc<Self>> {
        immediate(Self::poll_acquire(qp, length))
    }
    pub(crate) fn poll_acquire(qp: &QueuePairHandle, length: usize) -> Poll<Result<Rc<Self>>> {
        if length == 0 {
            return Poll::Ready(Err(Error::InvalidRange));
        }
        if qp.lease.slot.cancel.load(Ordering::Acquire)
            || qp.lease.shared.closed.load(Ordering::Acquire)
        {
            return Poll::Ready(Err(Error::Unavailable));
        }
        let mut mailbox = ready!(try_mailbox(&qp.lease.slot.mailbox))?;
        if length > mailbox.bytes.len() || mailbox.length != 0 {
            return Poll::Ready(Err(Error::Overloaded));
        }
        mailbox.length = length;
        Poll::Ready(Ok(Rc::new(Self {
            lease: qp.lease.clone(),
            length,
        })))
    }
    pub(crate) fn length(&self) -> usize {
        self.length
    }
    #[cfg(test)]
    pub(crate) fn copy_from(&self, bytes: &[u8]) -> Result<()> {
        immediate(self.poll_copy_from(bytes))
    }
    pub(crate) fn poll_copy_from(&self, bytes: &[u8]) -> Poll<Result<()>> {
        if bytes.len() != self.length {
            return Poll::Ready(Err(Error::InvalidRange));
        }
        if self.lease.slot.cancel.load(Ordering::Acquire)
            || self.lease.shared.closed.load(Ordering::Acquire)
        {
            return Poll::Ready(Err(Error::Unavailable));
        }
        let mut mailbox = ready!(try_mailbox(&self.lease.slot.mailbox))?;
        if mailbox.command.is_some() {
            return Poll::Ready(Err(Error::Unavailable));
        }
        mailbox.bytes[..self.length].copy_from_slice(bytes);
        Poll::Ready(Ok(()))
    }
    pub(crate) fn register_waiter(&self, cx: &Context<'_>) {
        self.lease.slot.waiter.register(cx.waker());
    }
    #[cfg(test)]
    pub(crate) fn copy_to(&self) -> Result<Vec<u8>> {
        immediate(self.poll_copy_to(&mut Context::from_waker(futures::task::noop_waker_ref())))
    }
    pub(crate) fn poll_copy_to(&self, cx: &mut Context<'_>) -> Poll<Result<Vec<u8>>> {
        self.lease.slot.waiter.register(cx.waker());
        if !self.lease.slot.fenced.load(Ordering::Acquire) {
            return Poll::Ready(Err(Error::Unavailable));
        }
        let mailbox = ready!(try_mailbox(&self.lease.slot.mailbox))?;
        Poll::Ready(self.copy_bytes(&mailbox))
    }
    fn copy_bytes(&self, mailbox: &Mailbox) -> Result<Vec<u8>> {
        let mut bytes = Vec::new();
        bytes
            .try_reserve_exact(self.length)
            .map_err(|_| Error::Overloaded)?;
        bytes.extend_from_slice(&mailbox.bytes[..self.length]);
        Ok(bytes)
    }
}
pub(crate) struct Window {
    pub(crate) key: Cell<u32>,
    pub(crate) address: Cell<u64>,
    _region: Rc<Region>,
}
struct TicketState {
    lease: Rc<Lease>,
    result: Cell<Option<Result<()>>>,
    window: Option<Rc<Window>>,
}
#[derive(Clone)]
pub(crate) struct Ticket(Rc<TicketState>);
impl Ticket {
    pub(crate) fn result(&self) -> Option<Result<()>> {
        match self.poll_result() {
            Poll::Ready(result) => result,
            Poll::Pending => None,
        }
    }
    fn poll_result(&self) -> Poll<Option<Result<()>>> {
        if let Some(result) = self.0.result.get() {
            return Poll::Ready(Some(result));
        }
        let mut mailbox = match ready!(try_mailbox(&self.0.lease.slot.mailbox)) {
            Ok(mailbox) => mailbox,
            Err(error) => return Poll::Ready(Some(Err(error))),
        };
        let Some(result) = mailbox.result.take() else {
            return Poll::Ready(None);
        };
        if result.is_ok() {
            if let Some(window) = &self.0.window {
                if let Some((address, key)) = mailbox.descriptor {
                    window.address.set(address);
                    window.key.set(key);
                }
            }
        }
        self.0.result.set(Some(result));
        Poll::Ready(Some(result))
    }
    pub(crate) fn poll(&self, cx: &mut Context<'_>) -> Poll<Result<()>> {
        self.0.lease.slot.waiter.register(cx.waker());
        if let Some(result) = self.result() {
            return Poll::Ready(result);
        }
        if self.0.lease.shared.closed.load(Ordering::Acquire) {
            return Poll::Ready(Err(Error::Unavailable));
        }
        Poll::Pending
    }
}
pub struct QueuePairHandle {
    lease: Rc<Lease>,
    pub endpoint: Endpoint,
    connecting: RefCell<Option<Ticket>>,
    connected: Cell<bool>,
    pending: RefCell<Option<Ticket>>,
    failure: Cell<Option<Error>>,
    expires: Cell<Option<std::time::Instant>>,
}
impl QueuePairHandle {
    #[cfg(test)]
    pub(crate) fn new(device: Rc<DeviceHandle>) -> Result<Rc<Self>> {
        immediate(Self::poll_new(device))
    }
    #[cfg(test)]
    pub(crate) fn poll_new(device: Rc<DeviceHandle>) -> Poll<Result<Rc<Self>>> {
        Self::poll_new_admitted(device, None)
    }
    pub(crate) fn poll_new_admitted(
        device: Rc<DeviceHandle>,
        permit: Option<Arc<crate::peer::adaptive::Permit>>,
    ) -> Poll<Result<Rc<Self>>> {
        if device.port.shared.closed.load(Ordering::Acquire)
            || device.generation != device.port.shared.generation.load(Ordering::Acquire)
        {
            return Poll::Ready(Err(Error::Unavailable));
        }
        let mut contended = false;
        for slot in &device.port.shared.slots {
            if slot.state.load(Ordering::Acquire) != READY {
                continue;
            }
            let mut mailbox = match try_mailbox(&slot.mailbox) {
                Poll::Pending => {
                    contended = true;
                    continue;
                }
                Poll::Ready(result) => result?,
            };
            if mailbox.rail != device.rail {
                continue;
            }
            if slot
                .state
                .compare_exchange(READY, OWNED, Ordering::AcqRel, Ordering::Acquire)
                .is_err()
            {
                continue;
            }
            let endpoint = mailbox.endpoint.ok_or(Error::Unavailable)?;
            mailbox.peer_admission = permit;
            return Poll::Ready(Ok(Rc::new(Self {
                lease: Rc::new(Lease {
                    slot: slot.clone(),
                    shared: device.port.shared.clone(),
                }),
                endpoint,
                connecting: RefCell::new(None),
                connected: Cell::new(false),
                pending: RefCell::new(None),
                failure: Cell::new(None),
                expires: Cell::new(None),
            })));
        }
        if contended {
            Poll::Pending
        } else {
            Poll::Ready(Err(Error::Overloaded))
        }
    }
    #[cfg(test)]
    fn submit(&self, command: Command, window: Option<Rc<Window>>) -> Result<Ticket> {
        immediate(self.poll_submit(command, window))
    }
    fn poll_submit(&self, command: Command, window: Option<Rc<Window>>) -> Poll<Result<Ticket>> {
        if self.lease.slot.cancel.load(Ordering::Acquire)
            || self.lease.shared.closed.load(Ordering::Acquire)
        {
            return Poll::Ready(Err(Error::Unavailable));
        }
        if let Some(ticket) = self.pending.borrow().as_ref() {
            match ready!(ticket.poll_result()) {
                None => return Poll::Ready(Err(Error::Overloaded)),
                Some(result) => result?,
            }
        }
        let mut mailbox = ready!(try_mailbox(&self.lease.slot.mailbox))?;
        if mailbox.command.is_some() {
            return Poll::Ready(Err(Error::Overloaded));
        }
        mailbox.result = None;
        mailbox.command = Some(command);
        let ticket = Ticket(Rc::new(TicketState {
            lease: self.lease.clone(),
            result: Cell::new(None),
            window,
        }));
        *self.pending.borrow_mut() = Some(ticket.clone());
        self.lease.shared.engine.wake();
        Poll::Ready(Ok(ticket))
    }
    #[cfg(test)]
    pub(crate) fn connect(&self, remote: Endpoint) -> Result<()> {
        immediate(self.poll_connect(remote))
    }
    pub(crate) fn poll_connect(&self, remote: Endpoint) -> Poll<Result<()>> {
        remote.validate()?;
        if self.connecting.borrow().is_some() || remote.link_layer != self.endpoint.link_layer {
            return Poll::Ready(Err(Error::InvalidRequest));
        }
        *self.connecting.borrow_mut() =
            Some(ready!(self.poll_submit(Command::Connect(remote), None))?);
        Poll::Ready(Ok(()))
    }
    pub(crate) fn ready(&self) -> bool {
        self.connected.get()
            && self.failure.get().is_none()
            && !self.lease.shared.closed.load(Ordering::Acquire)
            && !self.lease.slot.cancel.load(Ordering::Acquire)
    }
    pub(crate) fn stopped(&self) -> bool {
        self.lease.slot.fenced.load(Ordering::Acquire)
    }
    pub(crate) fn expire_at(&self, deadline: std::time::Instant) {
        self.expires.set(Some(deadline));
    }
    #[cfg(test)]
    pub(crate) fn bind(&self, region: Rc<Region>) -> Result<(Rc<Window>, Ticket)> {
        immediate(self.poll_bind(region))
    }
    pub(crate) fn poll_bind(&self, region: Rc<Region>) -> Poll<Result<(Rc<Window>, Ticket)>> {
        if !self.ready() || !Rc::ptr_eq(&region.lease, &self.lease) {
            return Poll::Ready(Err(Error::Unavailable));
        }
        let window = Rc::new(Window {
            key: Cell::new(0),
            address: Cell::new(0),
            _region: region,
        });
        let ticket = ready!(self.poll_submit(Command::Bind, Some(window.clone())))?;
        Poll::Ready(Ok((window, ticket)))
    }
    #[cfg(test)]
    pub(crate) fn invalidate(&self, window: Rc<Window>) -> Result<Ticket> {
        if !self.ready() || !Rc::ptr_eq(&window._region.lease, &self.lease) {
            return Err(Error::Unavailable);
        }
        self.submit(Command::Invalidate, None)
    }
    pub(crate) fn poll_invalidate(
        &self,
        window: Rc<Window>,
        cx: &mut Context<'_>,
    ) -> Poll<Result<Ticket>> {
        self.lease.slot.waiter.register(cx.waker());
        if !self.ready() || !Rc::ptr_eq(&window._region.lease, &self.lease) {
            return Poll::Ready(Err(Error::Unavailable));
        }
        self.poll_submit(Command::Invalidate, None)
    }
    #[cfg(test)]
    pub(crate) fn write(&self, region: Rc<Region>, address: u64, key: u32) -> Result<Ticket> {
        immediate(self.poll_write(region, address, key))
    }
    pub(crate) fn poll_write(
        &self,
        region: Rc<Region>,
        address: u64,
        key: u32,
    ) -> Poll<Result<Ticket>> {
        if !self.ready() || !Rc::ptr_eq(&region.lease, &self.lease) {
            return Poll::Ready(Err(Error::Unavailable));
        }
        self.poll_submit(Command::Write { address, key }, None)
    }
    pub(crate) fn register_waiter(&self, cx: &Context<'_>) {
        self.lease.slot.waiter.register(cx.waker());
    }
    pub fn progress(&self) -> Result<usize> {
        if self.lease.shared.closed.load(Ordering::Acquire) && !self.stopped() {
            self.failure.set(Some(Error::Unavailable));
        }
        if self
            .expires
            .get()
            .is_some_and(|d| crate::runtime::environment::now() >= d)
            && !self.stopped()
        {
            self.failure.set(Some(Error::DeadlineExceeded));
        }
        if let Some(ticket) = self.connecting.borrow().as_ref() {
            if ticket.result() == Some(Ok(())) {
                self.connected.set(true);
            }
        }
        let mut count = 0;
        if let Some(ticket) = self.pending.borrow().as_ref() {
            if let Some(result) = ticket.result() {
                count = 1;
                if let Err(error) = result {
                    self.failure.set(Some(error));
                }
            }
        }
        if let Some(error) = self.failure.get() {
            self.stop()?;
            self.lease.slot.waiter.wake();
            return Err(error);
        }
        Ok(count)
    }
    /// Cancellation request only. poll_stopped is the actual asynchronous fence.
    pub fn stop(&self) -> Result<()> {
        self.lease.slot.cancel.store(true, Ordering::Release);
        self.lease.shared.engine.wake();
        Ok(())
    }
    pub(crate) fn poll_stopped(&self, cx: &mut Context<'_>) -> Poll<Result<()>> {
        self.lease.slot.waiter.register(cx.waker());
        self.stop()?;
        if self.stopped() {
            Poll::Ready(Ok(()))
        } else if self.lease.shared.closed.load(Ordering::Acquire) {
            Poll::Ready(Err(Error::Unavailable))
        } else {
            Poll::Pending
        }
    }
    pub(crate) fn poll_connected(&self, cx: &mut Context<'_>) -> Poll<Result<()>> {
        self.progress()?;
        let poll = self
            .connecting
            .borrow()
            .as_ref()
            .ok_or(Error::Unavailable)?
            .poll(cx);
        if poll == Poll::Ready(Ok(())) {
            self.connected.set(true);
        }
        poll
    }
}
impl Drop for QueuePairHandle {
    fn drop(&mut self) {
        let _ = self.stop();
    }
}

pub(crate) const IDLE: u8 = 0;
pub(crate) const READY: u8 = 1;
pub(crate) const OWNED: u8 = 2;
pub(crate) const RETIRED: u8 = 3;

pub(crate) enum Command {
    Connect(Endpoint),
    Bind,
    Write { address: u64, key: u32 },
    Invalidate,
}
pub(crate) struct Mailbox {
    pub peer_admission: Option<Arc<crate::peer::adaptive::Permit>>,
    pub endpoint: Option<Endpoint>,
    pub rail: RailId,
    pub length: usize,
    pub bytes: Vec<u8>,
    pub command: Option<Command>,
    pub result: Option<Result<()>>,
    pub descriptor: Option<(u64, u32)>,
    pub quota: Option<Arc<Reservation>>,
}
pub(crate) struct Slot {
    pub state: AtomicU8,
    pub cancel: AtomicBool,
    pub released: AtomicBool,
    pub fenced: AtomicBool,
    pub mailbox: Mutex<Mailbox>,
    pub waiter: AtomicWaker,
}
impl Drop for Slot {
    fn drop(&mut self) {
        // Failed native teardown may intentionally outlive both paired roles.
        // The bounded admission slot must quarantine with that DMA ownership.
        if !self.fenced.load(Ordering::Acquire) {
            let mailbox = self.mailbox.get_mut().unwrap_or_else(|e| e.into_inner());
            if let Some(permit) = mailbox.peer_admission.take() {
                std::mem::forget(permit);
            }
        }
    }
}
pub(crate) struct Shared {
    pub slots: Vec<Arc<Slot>>,
    pub engine: AtomicWaker,
    pub io: AtomicWaker,
    pub closed: AtomicBool,
    pub generation: AtomicU64,
    drained: AtomicBool,
    alive: AtomicBool,
    configured: AtomicBool,
    config: Mutex<Option<Configuration>>,
    activation: Mutex<Option<Result<Vec<RailMapping>>>>,
}
struct Configuration {
    publication: Vec<RailMapping>,
    associations: Vec<FabricPort>,
    quotas: Vec<Reservation>,
    bytes: usize,
}
/// Send endpoints are created by the factory before either paired role starts.
pub struct IoPort {
    pub(crate) shared: Arc<Shared>,
}
pub struct NativePort {
    shared: Arc<Shared>,
}
impl Drop for NativePort {
    fn drop(&mut self) {
        self.shared.alive.store(false, Ordering::Release);
        self.shared.closed.store(true, Ordering::Release);
        for slot in &self.shared.slots {
            slot.waiter.wake();
        }
        self.shared.io.wake();
    }
}

pub fn pair(slots: usize) -> Result<(IoPort, NativePort)> {
    if slots == 0 || slots > 256 {
        return Err(Error::InvalidConfiguration);
    }
    let shared = Arc::new(Shared {
        slots: (0..slots)
            .map(|_| {
                Arc::new(Slot {
                    state: AtomicU8::new(IDLE),
                    cancel: AtomicBool::new(false),
                    released: AtomicBool::new(false),
                    fenced: AtomicBool::new(false),
                    mailbox: Mutex::new(Mailbox {
                        peer_admission: None,
                        endpoint: None,
                        rail: RailId(0),
                        length: 0,
                        bytes: Vec::new(),
                        command: None,
                        result: None,
                        descriptor: None,
                        quota: None,
                    }),
                    waiter: AtomicWaker::new(),
                })
            })
            .collect(),
        engine: AtomicWaker::new(),
        io: AtomicWaker::new(),
        closed: AtomicBool::new(false),
        generation: AtomicU64::new(1),
        drained: AtomicBool::new(false),
        alive: AtomicBool::new(true),
        configured: AtomicBool::new(false),
        config: Mutex::new(None),
        activation: Mutex::new(None),
    });
    Ok((
        IoPort {
            shared: shared.clone(),
        },
        NativePort { shared },
    ))
}
impl IoPort {
    pub fn capacity(&self) -> usize {
        self.shared.slots.len()
    }
    pub fn register_driver(&self, waker: &Waker) {
        self.shared.io.register(waker);
    }
    /// Reopen only after the native role has destroyed every device owner and
    /// all I/O leases have released their fenced staging. No timer is a fence.
    pub(crate) fn reopen(&self) -> Result<()> {
        if !self.shared.alive.load(Ordering::Acquire) {
            return Err(Error::Unavailable);
        }
        if !self.shared.closed.load(Ordering::Acquire) {
            return Ok(());
        }
        if !self.shared.drained.load(Ordering::Acquire) {
            return Err(Error::Overloaded);
        }
        let generation = self
            .shared
            .generation
            .load(Ordering::Acquire)
            .checked_add(1)
            .ok_or(Error::Unavailable)?;
        for slot in &self.shared.slots {
            slot.cancel.store(false, Ordering::Release);
            slot.released.store(false, Ordering::Release);
            slot.fenced.store(false, Ordering::Release);
            slot.state.store(IDLE, Ordering::Release);
        }
        self.shared.generation.store(generation, Ordering::Release);
        self.shared.configured.store(false, Ordering::Release);
        self.shared.drained.store(false, Ordering::Release);
        self.shared.closed.store(false, Ordering::Release);
        self.shared.engine.wake();
        Ok(())
    }
    pub(crate) async fn configure(
        &self,
        publication: Vec<RailMapping>,
        associations: Vec<FabricPort>,
        quotas: Vec<Reservation>,
        bytes: usize,
        scope: &RequestScope,
    ) -> Result<()> {
        let mut configuration = Some(Configuration {
            publication,
            associations,
            quotas,
            bytes,
        });
        wait(scope, |cx| {
            self.register_driver(cx.waker());
            if self.shared.closed.load(Ordering::Acquire)
                || configuration.as_ref().unwrap().quotas.len() != self.capacity()
            {
                return std::task::Poll::Ready(Err(Error::Unavailable));
            }
            let mut config = std::task::ready!(try_mailbox(&self.shared.config))?;
            if config.is_some()
                || self
                    .shared
                    .slots
                    .iter()
                    .any(|s| s.state.load(Ordering::Acquire) != IDLE)
            {
                return std::task::Poll::Ready(Err(Error::InvalidConfiguration));
            }
            let mut activation = std::task::ready!(try_mailbox(&self.shared.activation))?;
            if self.shared.configured.swap(true, Ordering::AcqRel) {
                return std::task::Poll::Ready(Err(Error::InvalidConfiguration));
            }
            *activation = None;
            *config = configuration.take();
            self.shared.engine.wake();
            std::task::Poll::Ready(Ok(()))
        })
        .await
    }
    pub(crate) fn activation(&self) -> Option<Result<Vec<RailMapping>>> {
        match self.shared.activation.try_lock() {
            Ok(mut activation) => activation.take(),
            Err(std::sync::TryLockError::WouldBlock) => None,
            Err(std::sync::TryLockError::Poisoned(_)) => Some(Err(Error::Io)),
        }
    }
    pub fn close(&self) {
        self.shared.drained.store(false, Ordering::Release);
        self.shared.closed.store(true, Ordering::Release);
        for slot in &self.shared.slots {
            slot.cancel.store(true, Ordering::Release);
        }
        self.shared.engine.wake();
    }
}
impl Drop for IoPort {
    fn drop(&mut self) {
        self.close();
    }
}

struct Resource {
    device: Rc<ffi::NativeDevice>,
    region: Rc<ffi::NativeRegion>,
    qp: Option<Rc<ffi::NativeQueuePair>>,
    window: Option<Rc<ffi::Window>>,
    pending: Option<ffi::Ticket>,
    stopping: bool,
    next_retry: Option<std::time::Instant>,
}
struct Activation {
    devices: Vec<Rc<ffi::NativeDevice>>,
    selected: Vec<(RailMapping, usize)>,
    quotas: std::vec::IntoIter<Reservation>,
    bytes: usize,
    next: usize,
}
/// Construct only inside build_crypto, after its NativePort crossed threads.
/// Rc native owners never cross threads, even during cancellation or shutdown.
pub struct NativeService {
    environment: crate::runtime::environment::Environment,
    port: NativePort,
    resources: Vec<Option<Resource>>,
    activation: Option<Activation>,
    cursor: usize,
    #[cfg(test)]
    simulation: Option<simulation::Simulation>,
}
impl NativeService {
    pub fn new(port: NativePort) -> Self {
        let resources = (0..port.shared.slots.len()).map(|_| None).collect();
        Self {
            environment: crate::runtime::environment::Environment::current(),
            port,
            resources,
            activation: None,
            cursor: 0,
            #[cfg(test)]
            simulation: simulation::current(),
        }
    }
    pub fn register_driver(&self, waker: &Waker) {
        self.port.shared.engine.register(waker);
    }
    fn begin_activation(&mut self, config: Configuration) -> Result<Option<Vec<RailMapping>>> {
        #[cfg(test)]
        let _environment = self.simulation.as_ref().map(simulation::Simulation::enter);
        if self.port.shared.closed.load(Ordering::Acquire) {
            return Err(Error::Unavailable);
        }
        if config.publication.is_empty() {
            return Ok(Some(Vec::new()));
        }
        let discovered = ffi::discover()?;
        let descriptions = discovered
            .iter()
            .map(super::discovered_port)
            .collect::<Result<Vec<_>>>()?;
        let selected = match_publication(&config.publication, &config.associations, &descriptions)?;
        if selected.is_empty() {
            return Ok(Some(Vec::new()));
        }
        if selected.len() > self.resources.len() {
            return Err(Error::Overloaded);
        }
        self.activation = Some(Activation {
            devices: discovered.into_iter().map(Rc::new).collect(),
            selected,
            quotas: config.quotas.into_iter(),
            bytes: config.bytes,
            next: 0,
        });
        Ok(None)
    }
    fn activate_slot(&mut self) -> Result<Option<Vec<RailMapping>>> {
        if self.port.shared.closed.load(Ordering::Acquire) {
            return Err(Error::Unavailable);
        }
        let activation = self.activation.as_mut().unwrap();
        let i = activation.next;
        let slot = &self.port.shared.slots[i];
        let mut mailbox = match slot.mailbox.try_lock() {
            Ok(mailbox) => mailbox,
            Err(std::sync::TryLockError::WouldBlock) => return Ok(None),
            Err(std::sync::TryLockError::Poisoned(_)) => return Err(Error::Io),
        };
        let (rail, index) = &activation.selected[i % activation.selected.len()];
        let device = activation.devices[*index].clone();
        let quota = Arc::new(
            activation
                .quotas
                .next()
                .ok_or(Error::InvalidConfiguration)?,
        );
        // Retain a quota observer even if registration/destruction fails. Partial
        // resources stay IDLE and are fenced by the normal close/retry path when
        // the I/O activation guard observes an error or is canceled/dropped.
        mailbox.quota = Some(quota.clone());
        mailbox
            .bytes
            .try_reserve_exact(activation.bytes)
            .map_err(|_| Error::Overloaded)?;
        mailbox.bytes.resize(activation.bytes, 0);
        let region = ffi::NativeRegion::new(device.clone(), activation.bytes, quota)?;
        self.resources[i] = Some(Resource {
            device: device.clone(),
            region,
            qp: None,
            window: None,
            pending: None,
            stopping: false,
            next_retry: None,
        });
        let resource = self.resources[i].as_mut().unwrap();
        resource.qp = Some(ffi::NativeQueuePair::new(device)?);
        let qp = resource.qp.as_ref().unwrap();
        qp.probe_window()?;
        mailbox.rail = rail.rail;
        mailbox.endpoint = Some(qp.endpoint);
        activation.next += 1;
        if activation.next != self.resources.len() {
            return Ok(None);
        }
        // No native work in this publication pass. No slot is claimable until
        // every slot has successfully provisioned, and I/O awaits the result.
        for slot in &self.port.shared.slots {
            slot.state.store(READY, Ordering::Release);
        }
        Ok(Some(
            activation
                .selected
                .iter()
                .map(|(rail, _)| rail.clone())
                .collect(),
        ))
    }
    /// Run at most budget steps: discovery, one slot's provisioning, or one slot's
    /// normal progress/fence. Native provider calls (including discovery's bounded
    /// port scan) cannot be preempted. This is a work bound, not a wall-time bound;
    /// a provider may stall all siblings on this crypto thread, but not their I/O.
    pub fn poll_budgeted(&mut self, budget: usize) -> Result<()> {
        let _environment = self.environment.enter();
        if budget == 0 {
            return Ok(());
        }
        for _ in 0..budget.min(self.resources.len()) {
            let result = if self.activation.is_some() {
                Some(self.activate_slot())
            } else {
                let config = self
                    .port
                    .shared
                    .config
                    .lock()
                    .map_err(|_| Error::Io)?
                    .take();
                config.map(|config| self.begin_activation(config))
            };
            if let Some(result) = result {
                if !matches!(result, Ok(None)) {
                    self.activation = None;
                    *self.port.shared.activation.lock().map_err(|_| Error::Io)? =
                        Some(result.map(|ready| ready.unwrap()));
                    self.port.shared.io.wake();
                }
                continue;
            }
            let index = self.cursor;
            self.cursor = (self.cursor + 1) % self.resources.len();
            self.drive(index);
        }
        if !self.port.shared.drained.load(Ordering::Acquire)
            && self.port.shared.closed.load(Ordering::Acquire)
            && self.activation.is_none()
            && self.resources.iter().all(Option::is_none)
        {
            let mut drained = true;
            for slot in &self.port.shared.slots {
                let Ok(mut mailbox) = slot.mailbox.try_lock() else {
                    drained = false;
                    continue;
                };
                if slot.state.load(Ordering::Acquire) == RETIRED
                    && !slot.released.load(Ordering::Acquire)
                {
                    drained = false;
                    continue;
                }
                // Backend destruction failures retain another quota owner.
                if mailbox
                    .quota
                    .as_ref()
                    .is_some_and(|q| Arc::strong_count(q) != 1)
                {
                    drained = false;
                    continue;
                }
                mailbox.bytes.clear();
                mailbox.bytes.shrink_to_fit();
                mailbox.quota = None;
                mailbox.endpoint = None;
                mailbox.command = None;
                mailbox.result = None;
                mailbox.descriptor = None;
                mailbox.length = 0;
            }
            if drained {
                self.port.shared.drained.store(true, Ordering::Release);
                self.port.shared.io.wake();
            }
        }
        Ok(())
    }
    fn drive(&mut self, index: usize) {
        let Some(resource) = self.resources[index].as_mut() else {
            return;
        };
        let slot = &self.port.shared.slots[index];
        let closed = self.port.shared.closed.load(Ordering::Acquire);
        let Ok(mut mailbox) = slot.mailbox.try_lock() else {
            return;
        };
        let state = slot.state.load(Ordering::Acquire);
        if closed || slot.cancel.load(Ordering::Acquire) {
            resource.stopping = true;
        }
        if resource.stopping {
            if resource
                .next_retry
                .is_some_and(|at| crate::runtime::environment::now() < at)
            {
                return;
            }
            if let Some(qp) = &resource.qp {
                if qp.stop().is_err() {
                    resource.next_retry = Some(
                        crate::runtime::environment::now() + std::time::Duration::from_millis(10),
                    );
                    return;
                } // quarantine, retry on next service turn
            }
            if resource.window.is_some() && !slot.fenced.load(Ordering::Acquire) {
                if resource
                    .region
                    .copy_into(&mut mailbox.bytes[..resource.region.length()])
                    .is_err()
                {
                    return;
                }
            }
            resource.pending = None;
            resource.window = None;
            resource.qp = None; // CQ destruction on crypto, never I/O
            slot.fenced.store(true, Ordering::Release);
            mailbox.peer_admission = None;
            mailbox.command = None;
            if mailbox.result.is_none() {
                mailbox.result = Some(Err(Error::Cancelled));
            }
            slot.waiter.wake();
            self.port.shared.io.wake();
            if closed {
                // Only the service owns native region/PD teardown.
                self.resources[index] = None;
                // A fenced receiver may still need its staging copy. That copy
                // and its quota survive with the slot's outstanding I/O leases.
                if state != OWNED || slot.released.load(Ordering::Acquire) {
                    slot.released.store(true, Ordering::Release);
                }
                slot.state.store(RETIRED, Ordering::Release);
                return;
            }
            if !slot.released.load(Ordering::Acquire) {
                return;
            }
            // Replenish the QP outside request turns. MR stays registered for the
            // entire bounded pool lifetime. Never reuse a QP/remote capability.
            match ffi::NativeQueuePair::new(resource.device.clone()) {
                Ok(qp) => {
                    mailbox.endpoint = Some(qp.endpoint);
                    resource.qp = Some(qp);
                }
                Err(_) => {
                    resource.next_retry = Some(
                        crate::runtime::environment::now() + std::time::Duration::from_millis(100),
                    );
                    slot.state.store(RETIRED, Ordering::Release);
                    return;
                }
            }
            resource.stopping = false;
            resource.next_retry = None;
            mailbox.result = None;
            mailbox.descriptor = None;
            mailbox.length = 0;
            slot.cancel.store(false, Ordering::Release);
            slot.released.store(false, Ordering::Release);
            slot.fenced.store(false, Ordering::Release);
            slot.state.store(READY, Ordering::Release);
            self.port.shared.io.wake();
            return;
        }
        if state != OWNED {
            return;
        }
        let qp = resource.qp.as_ref().unwrap();
        if let Some(ticket) = &resource.pending {
            if let Err(error) = qp.progress() {
                mailbox.result = Some(Err(error));
                resource.stopping = true;
            } else if let Some(result) = ticket.result() {
                mailbox.result = Some(result);
                resource.pending = None;
            }
            if mailbox.result.is_some() {
                slot.waiter.wake();
                self.port.shared.io.wake();
            }
            return;
        }
        let Some(command) = mailbox.command.take() else {
            return;
        };
        let result = (|| -> Result<()> {
            match command {
                Command::Connect(remote) => qp.connect(remote)?,
                Command::Bind => {
                    resource.region.resize(mailbox.length)?;
                    let (window, ticket) = qp.bind(resource.region.clone())?;
                    mailbox.descriptor = Some((resource.region.address(), window.key));
                    resource.window = Some(window);
                    resource.pending = Some(ticket);
                }
                Command::Write { address, key } => {
                    resource.region.resize(mailbox.length)?;
                    resource
                        .region
                        .copy_from(&mailbox.bytes[..mailbox.length])?;
                    resource.pending = Some(qp.write(resource.region.clone(), address, key)?);
                }
                Command::Invalidate => {
                    resource.pending =
                        Some(qp.invalidate(resource.window.clone().ok_or(Error::InvalidRequest)?)?)
                }
            }
            Ok(())
        })();
        if result.is_err() {
            resource.stopping = true;
        }
        if resource.pending.is_none() || result.is_err() {
            mailbox.result = Some(result);
            slot.waiter.wake();
            self.port.shared.io.wake();
        }
    }
    pub fn close(&self) {
        self.port.shared.closed.store(true, Ordering::Release);
        self.port.shared.io.wake();
    }
    pub fn drained(&self) -> bool {
        self.activation.is_none()
            && self
                .port
                .shared
                .config
                .lock()
                .is_ok_and(|config| config.is_none())
            && self.resources.iter().all(Option::is_none)
    }
}
impl Drop for NativeService {
    fn drop(&mut self) {
        self.close();
        // Drop native QPs before their region owners. On failure the FFI boundary
        // keeps its own DMA references and quota, even after this service dies.
        for resource in self.resources.iter_mut().flatten() {
            resource.qp.take();
            resource.window.take();
            resource.pending.take();
        }
        for slot in &self.port.shared.slots {
            slot.waiter.wake();
        }
    }
}

/// Compose around PageCryptoEngine in Application::build_crypto. Uses the same
/// OS thread and runtime wake; no changes to runtime's thread count are needed.
pub struct WithNative<S> {
    inner: S,
    native: NativeService,
}
// NativeService is already !Send once constructed: its Rc resource graph must
// stay on build_crypto's thread, including all destructor paths.
impl<S> WithNative<S> {
    pub fn new(inner: S, port: NativePort) -> Self {
        Self {
            inner,
            native: NativeService::new(port),
        }
    }
}
impl<S: CryptoService> CryptoService for WithNative<S> {
    fn register_driver(&self, waker: &Waker) {
        self.inner.register_driver(waker);
        self.native.register_driver(waker);
    }
    fn start<'a>(&'a mut self, scope: &'a RequestScope) -> crate::error::Operation<'a, ()> {
        self.inner.start(scope)
    }
    fn poll_budgeted(&mut self, budget: usize) -> Result<()> {
        self.inner.poll_budgeted(budget)?;
        self.native.poll_budgeted(budget)
    }
    fn drain<'a>(&'a mut self, scope: &'a RequestScope) -> crate::error::Operation<'a, ()> {
        Box::pin(async move {
            self.native.close();
            futures::future::poll_fn(|cx| {
                self.native.register_driver(cx.waker());
                self.native.poll_budgeted(1)?;
                if self.native.drained() {
                    std::task::Poll::Ready(Ok(()))
                } else {
                    // WorkerGroup's bounded lifecycle tick polls again. Do not
                    // self-wake into a hot loop on a failed native fence.
                    std::task::Poll::Pending
                }
            })
            .await?;
            self.inner.drain(scope).await
        })
    }
    fn shutdown<'a>(&'a mut self, scope: &'a RequestScope) -> crate::error::Operation<'a, ()> {
        self.inner.shutdown(scope)
    }
}

#[cfg(test)]
mod receive_tests {
    //! Deterministic mailbox schedules through the authenticated receive completion path.
    use super::mailbox_tests::{envelope, header, scope, verified};
    use super::tests::{claim, mark_connected, provision_test};
    use super::*;
    use crate::{
        model::{ResourceClass, TransferId},
        rdma::{
            COMPLETION_HEADER, Devices, RdmaTransfer, SessionLease, Sessions, completion_bytes,
        },
        runtime::{admission::Admission, environment},
        security::connection::signature_tests::network,
    };
    use base64::{Engine, engine::general_purpose::STANDARD};
    use std::{
        task::{Context, Poll},
        time::Duration,
    };

    #[test]
    fn receive_completion_waits_for_invalidation_mailbox() {
        receive_contended(false, None);
    }

    #[test]
    fn receive_completion_waits_for_fenced_readback_mailbox() {
        receive_contended(true, None);
    }

    #[test]
    fn receive_completion_contention_obeys_cancellation_and_deadline() {
        for readback in [false, true] {
            for error in [Error::Cancelled, Error::DeadlineExceeded] {
                receive_contended(readback, Some(error));
            }
        }
    }

    #[test]
    fn receive_completion_does_not_retry_ciphertext_quota_exhaustion() {
        receive_contended(false, Some(Error::Overloaded));
    }

    fn receive_contended(readback: bool, terminal: Option<Error>) {
        receive_case(readback, terminal, false);
    }

    #[test]
    fn successful_invalidation_cancel_and_expiry_leave_failed_terminal_fence_quarantined() {
        for error in [Error::Cancelled, Error::DeadlineExceeded] {
            receive_case(true, Some(error), true);
        }
    }

    fn receive_case(readback: bool, terminal: Option<Error>, failed_fence: bool) {
        let clock = environment::SimulationClock::new(61);
        let _time = clock.environment(0).enter();
        let (io, port) = pair(1).unwrap();
        let io = Rc::new(io);
        let mut native = NativeService::new(port);
        let charged = provision_test(&mut native, 0);
        let qp = claim(&io);
        let peer_metrics = crate::telemetry::metrics::Metrics::default();
        let peer_admission = crate::peer::adaptive::AdaptivePeers::new(
            crate::peer::adaptive::Config {
                total: 1,
                per_peer: 1,
            },
            peer_metrics.clone(),
        )
        .unwrap();
        let peer = crate::model::NodeId("native-peer".into());
        let permit = peer_admission.acquire(&peer).unwrap();
        io.shared.slots[0].mailbox.lock().unwrap().peer_admission = Some(permit);
        mark_connected(&qp, &mut native);
        let signers = network(2);
        let session = SessionLease::test(qp.clone(), signers[0].node().clone());
        let devices = Rc::new(Devices::new());
        let sessions = Rc::new(Sessions::new(devices.clone(), 1));
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(true).limits,
        ));
        let transfer = RdmaTransfer::new(sessions);
        let envelope = envelope();
        let mut scope = scope();
        let id = TransferId([4; 16]);
        native.resources[0]
            .as_ref()
            .unwrap()
            .region
            .copy_from(&[0xa5; 32])
            .unwrap();
        let grant =
            futures::executor::block_on(transfer.prepare_receive(&session, &envelope, id, &scope))
                .unwrap();
        native.poll_budgeted(1).unwrap();
        ffi::lifetime_tests::complete(1, 0, 5);
        native.poll_budgeted(1).unwrap();
        grant.descriptor().unwrap();
        let completion = verified(
            &signers,
            vec![header(
                COMPLETION_HEADER,
                STANDARD
                    .encode(completion_bytes(session.binding(), id))
                    .into_bytes(),
            )],
        );
        // Expire only the finish scope; the grant remains valid so admission/binding
        // cannot be mistaken for the receive-completion deadline check.
        if terminal == Some(Error::DeadlineExceeded) {
            scope.deadline.0 = environment::now() + Duration::from_secs(1);
        }
        let quota = (terminal == Some(Error::Overloaded)).then(|| {
            admission
                .reserve(
                    None,
                    ResourceClass::Ciphertext,
                    admission.limit(ResourceClass::Ciphertext),
                )
                .unwrap()
        });
        let mut finish =
            transfer.finish_receive(&session, grant, &completion, envelope, &admission, &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        if readback {
            assert!(finish.as_mut().poll(&mut cx).is_pending());
            native.poll_budgeted(1).unwrap();
            ffi::lifetime_tests::complete(2, 0, 6);
            native.poll_budgeted(1).unwrap();
            assert!(finish.as_mut().poll(&mut cx).is_pending());
            assert!(!qp.stopped());
            if failed_fence {
                ffi::lifetime_tests::fail_stop(true);
            }
            native.poll_budgeted(1).unwrap();
            assert_eq!(qp.stopped(), !failed_fence);
        }
        // Hold the same mutex as the native role at invalidation submission or
        // immediately after it publishes the fence, before it releases the mailbox.
        let guard = io.shared.slots[0].mailbox.lock().unwrap();
        if terminal != Some(Error::Overloaded) {
            match finish.as_mut().poll(&mut cx) {
                Poll::Pending => {}
                Poll::Ready(Err(error)) => {
                    panic!("mailbox contention failed an admitted receive: {error:?}")
                }
                Poll::Ready(Ok(_)) => panic!("readback bypassed the held mailbox"),
            }
            assert_eq!(admission.used(ResourceClass::Ciphertext), 32);
        }
        if terminal == Some(Error::Cancelled) {
            scope.cancel().unwrap();
        }
        if terminal == Some(Error::DeadlineExceeded) {
            clock.advance(Duration::from_secs(1));
        }
        if let Some(error) = terminal {
            assert!(matches!(finish.as_mut().poll(&mut cx), Poll::Ready(Err(e)) if e == error));
        }
        drop(guard);
        if terminal.is_none() {
            if !readback {
                assert!(finish.as_mut().poll(&mut cx).is_pending());
                native.poll_budgeted(1).unwrap();
                ffi::lifetime_tests::complete(2, 0, 6);
                native.poll_budgeted(1).unwrap();
                assert!(finish.as_mut().poll(&mut cx).is_pending());
                native.poll_budgeted(1).unwrap();
            }
            let Poll::Ready(Ok(page)) = finish.as_mut().poll(&mut cx) else {
                panic!("receive did not finish after mailbox release")
            };
            assert_eq!(page.bytes(), &[0xa5; 32]);
            assert_eq!(admission.used(ResourceClass::Ciphertext), 32);
            drop(page);
        }
        drop(finish);
        drop(quota);
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
        if failed_fence {
            assert!(!qp.stopped());
            assert_eq!(charged.get(), 1);
            assert!(
                native.resources[0]
                    .as_ref()
                    .unwrap()
                    .region
                    .copy_to()
                    .is_err()
            );
            // Even the final I/O owner can disappear before fencing. The native
            // allocation and quota remain quarantined and the slot cannot be claimed.
            drop(session);
            drop(qp);
            assert_eq!(
                peer_metrics.gauge(crate::telemetry::metrics::Gauge::PeerExchanges),
                1
            );
            assert!(matches!(
                peer_admission.acquire(&peer),
                Err(Error::Overloaded)
            ));
            native.resources[0].as_mut().unwrap().next_retry = None;
            native.poll_budgeted(1).unwrap();
            assert_eq!(charged.get(), 1);
            assert_eq!(io.shared.slots[0].state.load(Ordering::Acquire), OWNED);
            ffi::lifetime_tests::fail_stop(false);
            native.resources[0].as_mut().unwrap().next_retry = None;
            native.poll_budgeted(1).unwrap();
            assert_eq!(io.shared.slots[0].state.load(Ordering::Acquire), READY);
            assert_eq!(
                peer_metrics.gauge(crate::telemetry::metrics::Gauge::PeerExchanges),
                0
            );
            native.close();
            native.poll_budgeted(1).unwrap();
            assert!(native.drained());
            assert_eq!(charged.get(), 0);
            return;
        }
        native.poll_budgeted(1).unwrap();
        assert!(qp.stopped());
        assert_eq!(charged.get(), 1);
        assert_eq!(io.shared.slots[0].state.load(Ordering::Acquire), OWNED);
        drop(session);
        drop(qp);
        native.poll_budgeted(1).unwrap();
        assert_eq!(io.shared.slots[0].state.load(Ordering::Acquire), READY);
        native.close();
        native.poll_budgeted(1).unwrap();
        assert!(native.drained());
        assert_eq!(charged.get(), 0);
    }
}

#[cfg(test)]
mod mailbox_tests {
    //! Hold native mailboxes at each public handoff, independently of thread timing.
    use super::tests::{claim, mark_connected, provision_test};
    use super::*;
    use crate::{
        error::Operation,
        http::{Header, MessageHead, StartLine},
        memory::pool::BufferPool,
        model::{
            CacheId, CacheKey, KeyId, Nonce, ObjectId, ObjectVersion, PageEnvelope, PageId,
            PageNumber, RequestId, ResourceClass, StrongEtag, TransferId,
        },
        rdma::{
            AuthenticatedDescriptor, DESCRIPTOR_HEADER, Devices, Grant, RdmaTransfer,
            RegisteredLease, SETUP_BINDING_HEADER, SETUP_HEADER, SessionLease, Sessions,
            lifecycle::{QueuePairHandle, Region},
        },
        runtime::{admission::Admission, environment},
        security::connection::{Signatures, VerifiedHead, signature_tests::network},
    };
    use std::{
        task::{Context, Poll},
        time::Duration,
    };

    pub(super) fn scope() -> RequestScope {
        RequestScope::new(
            RequestId([1; 16]),
            environment::now() + Duration::from_secs(10),
        )
        .unwrap()
    }
    pub(super) fn poll<T>(operation: &mut Operation<'_, T>) -> Poll<Result<T>> {
        operation
            .as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
    }
    fn done<T>(operation: &mut Operation<'_, T>) -> T {
        match poll(operation) {
            Poll::Ready(Ok(value)) => value,
            Poll::Ready(Err(error)) => panic!("unexpected error: {error:?}"),
            Poll::Pending => panic!("unexpected pending operation"),
        }
    }
    pub(super) fn verified(signers: &[Rc<Signatures>], headers: Vec<Header>) -> VerifiedHead {
        let mut headers = headers;
        headers.push(Header {
            name: "racer-receiver".into(),
            value: signers[1].node().0.as_bytes().to_vec(),
        });
        signers[1]
            .verify_proof(
                signers[0]
                    .sign(MessageHead {
                        start: StartLine::Request {
                            method: "POST".into(),
                            target: "/racer/peer/v1/rdma".into(),
                        },
                        headers,
                    })
                    .unwrap(),
            )
            .unwrap()
    }
    pub(super) fn header(name: &str, value: Vec<u8>) -> Header {
        Header {
            name: name.into(),
            value,
        }
    }

    #[test]
    fn sessions_admit_64_neighbors_but_keep_per_neighbor_and_total_bounds() {
        let signers = network(2);
        let peer = verified(&signers, vec![]);
        let (io, port) = pair(2).unwrap();
        let io = Rc::new(io);
        let mut native = NativeService::new(port);
        provision_test(&mut native, 0);
        provision_test(&mut native, 1);
        let devices = Rc::new(Devices::test(io));
        let qp = match QueuePairHandle::poll_new(devices.select(RailId(0)).unwrap().handle) {
            Poll::Ready(Ok(qp)) => qp,
            _ => panic!("fixture QP unavailable"),
        };
        let sessions = Sessions::new(devices, 1);
        // Synthetic live entries isolate session admission from the native QP pool.
        for i in 0..63 {
            sessions.track_peer_test(crate::model::NodeId(format!("peer-{i}")), qp.clone());
        }
        let scope = scope();
        let prepared = done(&mut sessions.prepare(&peer.peer, RailId(0), &scope));
        assert!(matches!(
            poll(&mut sessions.prepare(&peer.peer, RailId(0), &scope)),
            Poll::Ready(Err(Error::Overloaded))
        ));
        drop(prepared);
        sessions.track_peer_test(crate::model::NodeId("peer-63".into()), qp);
        assert!(matches!(
            poll(&mut sessions.prepare(&peer.peer, RailId(0), &scope)),
            Poll::Ready(Err(Error::Overloaded))
        ));
    }

    #[test]
    fn signed_setup_waits_for_slot_and_connect_mailboxes_without_consuming_admission() {
        let signers = network(2);
        let peer = verified(&signers, vec![]);
        let (io, port) = pair(2).unwrap();
        let io = Rc::new(io);
        let mut native = NativeService::new(port);
        provision_test(&mut native, 0);
        provision_test(&mut native, 1);
        let sessions = Sessions::new(Rc::new(Devices::test(io.clone())), 2);
        let scope = scope();
        let mut prepare = sessions.prepare(&peer.peer, RailId(0), &scope);
        let guard0 = io.shared.slots[0].mailbox.lock().unwrap();
        let guard1 = io.shared.slots[1].mailbox.lock().unwrap();
        assert!(poll(&mut prepare).is_pending());
        assert_eq!(io.shared.slots[0].state.load(Ordering::Acquire), READY);
        drop((guard0, guard1));
        let prepared = done(&mut prepare);
        drop(prepare);
        let remote = done(&mut sessions.prepare(&peer.peer, RailId(0), &scope));
        let device = Rc::new(crate::rdma::lifecycle::DeviceHandle {
            port: io.clone(),
            rail: RailId(0),
            generation: io.shared.generation.load(Ordering::Acquire),
        });
        assert!(matches!(
            QueuePairHandle::poll_new(device),
            Poll::Ready(Err(Error::Overloaded))
        ));
        assert!(matches!(
            poll(&mut sessions.prepare(&peer.peer, RailId(0), &scope)),
            Poll::Ready(Err(Error::Overloaded))
        ));
        let ack = verified(
            &signers,
            vec![
                header(SETUP_HEADER, remote.setup().header_value()),
                header(
                    SETUP_BINDING_HEADER,
                    prepared.setup().binding_header_value(),
                ),
            ],
        );
        let mut finish = prepared.finish(&ack, &scope);
        let guard = io.shared.slots[0].mailbox.lock().unwrap();
        assert!(poll(&mut finish).is_pending());
        assert!(!io.shared.slots[0].cancel.load(Ordering::Acquire));
        drop(guard);
        let session = done(&mut finish);
        drop(finish);
        let mut ready = session.wait_ready(&scope);
        assert!(poll(&mut ready).is_pending());
        native.poll_budgeted(2).unwrap();
        let guard = io.shared.slots[0].mailbox.lock().unwrap();
        assert!(poll(&mut ready).is_pending());
        drop(guard);
        done(&mut ready);
        assert!(session.ready());
    }

    pub(super) fn envelope() -> PageEnvelope {
        PageEnvelope {
            page: PageId {
                version: ObjectVersion {
                    object: ObjectId {
                        cache: CacheId("mailbox-test".into()),
                        key: CacheKey([1; 32]),
                    },
                    etag: StrongEtag::test_value("v1"),
                },
                number: PageNumber(0),
            },
            key_id: KeyId([1; 16]),
            nonce: Nonce([2; 24]),
            plaintext_length: 16,
            ciphertext_length: 32,
        }
    }

    #[test]
    fn receive_preparation_and_sender_wait_at_every_buffer_and_command_boundary() {
        sender_case(None);
    }

    #[test]
    fn successful_write_cancel_and_expiry_leave_failed_terminal_fence_quarantined() {
        for error in [Error::Cancelled, Error::DeadlineExceeded] {
            sender_case(Some(error));
        }
    }

    fn sender_case(terminal: Option<Error>) {
        let clock = environment::SimulationClock::new(62);
        let _time = clock.environment(0).enter();
        let signers = network(2);
        let (io, port) = pair(2).unwrap();
        let io = Rc::new(io);
        let mut native = NativeService::new(port);
        provision_test(&mut native, 0);
        let charged = provision_test(&mut native, 1);
        let receiver = claim(&io);
        let sender = claim(&io);
        mark_connected(&receiver, &mut native);
        mark_connected(&sender, &mut native);
        let receive = SessionLease::test(receiver.clone(), signers[0].node().clone());
        let send = SessionLease::test(sender.clone(), signers[0].node().clone());
        let devices = Rc::new(Devices::test(io.clone()));
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(true).limits,
        ));
        let transfer = RdmaTransfer::new(Rc::new(Sessions::new(devices, 2)));
        let mut scope = scope();
        if terminal == Some(Error::DeadlineExceeded) {
            scope.deadline.0 = environment::now() + Duration::from_secs(1);
        }
        let envelope = envelope();
        let id = TransferId([9; 16]);
        let mut prepare = transfer.prepare_receive(&receive, &envelope, id, &scope);
        let guard = io.shared.slots[0].mailbox.lock().unwrap();
        assert!(poll(&mut prepare).is_pending());
        drop(guard);
        let grant = done(&mut prepare);
        drop(prepare);
        native.poll_budgeted(2).unwrap();
        ffi::lifetime_tests::complete(1, 0, 5);
        native.poll_budgeted(2).unwrap();
        done(&mut grant.wait_bound(&scope));
        let signed = verified(
            &signers,
            vec![header(DESCRIPTOR_HEADER, grant.header_value().unwrap())],
        );
        let descriptor = AuthenticatedDescriptor::from_verified(&signed, &send, id).unwrap();
        let page = BufferPool::new(admission.clone())
            .ciphertext(
                admission
                    .reserve(
                        Some(&envelope.page.version.object.cache),
                        ResourceClass::Ciphertext,
                        32,
                    )
                    .unwrap(),
                envelope,
                vec![0xa5; 32],
            )
            .unwrap();
        let mut sending = transfer.send_to(&send, page, descriptor, &scope);
        let guard = io.shared.slots[1].mailbox.lock().unwrap();
        assert!(poll(&mut sending).is_pending());
        assert_eq!(admission.used(ResourceClass::Ciphertext), 32);
        drop(guard);
        assert!(poll(&mut sending).is_pending());
        native.poll_budgeted(2).unwrap();
        ffi::lifetime_tests::complete(1, 0, 1);
        native.poll_budgeted(2).unwrap();
        assert!(poll(&mut sending).is_pending());
        if let Some(error) = terminal {
            ffi::lifetime_tests::fail_stop(true);
            native.poll_budgeted(2).unwrap();
            assert!(!sender.stopped());
            assert!(poll(&mut sending).is_pending());
            if error == Error::Cancelled {
                scope.cancel().unwrap();
            } else {
                clock.advance(Duration::from_secs(1));
            }
            assert!(matches!(poll(&mut sending), Poll::Ready(Err(e)) if e == error));
            drop(sending);
            assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
            drop((send, sender));
            native.resources[1].as_mut().unwrap().next_retry = None;
            native.poll_budgeted(2).unwrap();
            assert_eq!(charged.get(), 1);
            assert_eq!(io.shared.slots[1].state.load(Ordering::Acquire), OWNED);
            assert!(!io.shared.slots[1].fenced.load(Ordering::Acquire));
            ffi::lifetime_tests::fail_stop(false);
            native.resources[1].as_mut().unwrap().next_retry = None;
            native.poll_budgeted(2).unwrap();
            assert_eq!(io.shared.slots[1].state.load(Ordering::Acquire), READY);
            drop((grant, receive, receiver));
            native.close();
            native.poll_budgeted(2).unwrap();
            assert!(native.drained());
            assert_eq!(charged.get(), 0);
            return;
        }
        native.poll_budgeted(2).unwrap();
        done(&mut sending);
        drop(sending);
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);

        // Independent public buffer copy and grant bind boundaries after acquisition.
        drop(grant);
        native.poll_budgeted(2).unwrap();
        drop((receive, send, receiver, sender));
        native.poll_budgeted(2).unwrap();
        let qp = claim(&io);
        mark_connected(&qp, &mut native);
        let session = SessionLease::test(qp.clone(), signers[0].node().clone());
        let mut buffer = done(&mut RegisteredLease::acquire(&session, 32, &scope));
        let mut copy = buffer.copy_from(&[0x5a; 32], &scope);
        let guard = io.shared.slots[0].mailbox.lock().unwrap();
        assert!(poll(&mut copy).is_pending());
        drop(guard);
        done(&mut copy);
        drop(copy);
        let mut bind = Grant::bind(&session, buffer, id, &scope);
        let guard = io.shared.slots[0].mailbox.lock().unwrap();
        assert!(poll(&mut bind).is_pending());
        assert!(!io.shared.slots[0].cancel.load(Ordering::Acquire));
        drop(guard);
        let grant = done(&mut bind);
        drop(bind);
        native.poll_budgeted(2).unwrap();
        ffi::lifetime_tests::complete(1, 0, 5);
        native.poll_budgeted(2).unwrap();
        done(&mut grant.wait_bound(&scope));
        let guard = io.shared.slots[0].mailbox.lock().unwrap();
        assert!(grant.header_value().is_ok(), "bound descriptor is cached");
        drop(guard);
    }

    #[test]
    fn command_poll_distinguishes_contended_completion_from_full_queue() {
        let (io, port) = pair(1).unwrap();
        let io = Rc::new(io);
        let mut native = NativeService::new(port);
        provision_test(&mut native, 0);
        let qp = claim(&io);
        mark_connected(&qp, &mut native);
        let region = Region::acquire(&qp, 32).unwrap();
        assert!(matches!(
            Region::poll_acquire(&qp, 32),
            Poll::Ready(Err(Error::Overloaded))
        ));
        let guard = io.shared.slots[0].mailbox.lock().unwrap();
        assert!(qp.poll_write(region.clone(), 4096, 7).is_pending());
        drop(guard);
        assert!(matches!(
            qp.poll_write(region.clone(), 4096, 7),
            Poll::Ready(Ok(_))
        ));
        assert!(matches!(
            qp.poll_write(region.clone(), 4096, 7),
            Poll::Ready(Err(Error::Overloaded))
        ));
        native.poll_budgeted(1).unwrap();
        ffi::lifetime_tests::complete(1, 0, 1);
        native.poll_budgeted(1).unwrap();
        let guard = io.shared.slots[0].mailbox.lock().unwrap();
        assert!(qp.poll_write(region.clone(), 4096, 7).is_pending());
        drop(guard);
        assert!(matches!(qp.poll_write(region, 4096, 7), Poll::Ready(Ok(_))));
    }

    #[test]
    fn contended_grant_cancel_expiry_and_drop_abort_without_submitting_bind() {
        for mode in 0..3 {
            let clock = environment::SimulationClock::new(63);
            let _time = clock.environment(0).enter();
            let (io, port) = pair(1).unwrap();
            let io = Rc::new(io);
            let mut native = NativeService::new(port);
            let charged = provision_test(&mut native, 0);
            let qp = claim(&io);
            mark_connected(&qp, &mut native);
            let session = SessionLease::test(qp.clone(), crate::model::NodeId("peer".into()));
            let mut scope = scope();
            assert!(matches!(
                poll(&mut RegisteredLease::acquire(&session, 33, &scope)),
                Poll::Ready(Err(Error::Overloaded))
            ));
            let buffer = done(&mut RegisteredLease::acquire(&session, 32, &scope));
            if mode == 1 {
                scope.deadline.0 = environment::now() + Duration::from_secs(1);
            }
            let mut bind = Grant::bind(&session, buffer, TransferId([1; 16]), &scope);
            let guard = io.shared.slots[0].mailbox.lock().unwrap();
            assert!(poll(&mut bind).is_pending());
            match mode {
                0 => {
                    scope.cancel().unwrap();
                    assert!(matches!(
                        poll(&mut bind),
                        Poll::Ready(Err(Error::Cancelled))
                    ));
                }
                1 => {
                    clock.advance(Duration::from_secs(1));
                    assert!(matches!(
                        poll(&mut bind),
                        Poll::Ready(Err(Error::DeadlineExceeded))
                    ));
                }
                _ => {}
            }
            drop(bind);
            assert!(guard.command.is_none());
            assert!(io.shared.slots[0].cancel.load(Ordering::Acquire));
            assert!(!qp.stopped());
            assert_eq!(charged.get(), 1);
            drop(guard);
            native.poll_budgeted(1).unwrap();
            assert!(qp.stopped());
            drop((session, qp));
            native.poll_budgeted(1).unwrap();
            assert_eq!(io.shared.slots[0].state.load(Ordering::Acquire), READY);
            native.close();
            native.poll_budgeted(1).unwrap();
            assert_eq!(charged.get(), 0);
        }
    }

    #[test]
    fn canceled_or_abandoned_contended_signed_setup_releases_only_after_fence() {
        let signers = network(2);
        let peer = verified(&signers, vec![]);
        for cancel in [false, true] {
            let (io, port) = pair(2).unwrap();
            let io = Rc::new(io);
            let mut native = NativeService::new(port);
            provision_test(&mut native, 0);
            provision_test(&mut native, 1);
            let sessions = Sessions::new(Rc::new(Devices::test(io.clone())), 2);
            let scope = scope();
            let prepared = done(&mut sessions.prepare(&peer.peer, RailId(0), &scope));
            let remote = done(&mut sessions.prepare(&peer.peer, RailId(0), &scope));
            let ack = verified(
                &signers,
                vec![
                    header(SETUP_HEADER, remote.setup().header_value()),
                    header(
                        SETUP_BINDING_HEADER,
                        prepared.setup().binding_header_value(),
                    ),
                ],
            );
            let mut finish = prepared.finish(&ack, &scope);
            let guard = io.shared.slots[0].mailbox.lock().unwrap();
            assert!(poll(&mut finish).is_pending());
            if cancel {
                scope.cancel().unwrap();
                assert!(matches!(
                    poll(&mut finish),
                    Poll::Ready(Err(Error::Cancelled))
                ));
            }
            drop(finish);
            assert!(guard.command.is_none());
            assert!(io.shared.slots[0].cancel.load(Ordering::Acquire));
            assert!(!io.shared.slots[0].fenced.load(Ordering::Acquire));
            drop(guard);
            native.poll_budgeted(2).unwrap();
            sessions.progress().unwrap();
            native.poll_budgeted(2).unwrap();
            assert_eq!(io.shared.slots[0].state.load(Ordering::Acquire), READY);
        }
    }

    #[test]
    fn activation_contention_retains_quota_and_cancellation_releases_unsubmitted_configuration() {
        for activation_lock in [false, true] {
            let (io, port) = pair(1).unwrap();
            let mut native = NativeService::new(port);
            let admission = Admission::new(crate::test_support::cluster::config(true).limits);
            let scope = scope();
            let quotas = vec![
                admission
                    .reserve(None, ResourceClass::Registered, 8192)
                    .unwrap(),
            ];
            let mut configure: Operation<'_, ()> =
                Box::pin(io.configure(vec![], vec![], quotas, 4096, &scope));
            let config = (!activation_lock).then(|| io.shared.config.lock().unwrap());
            let activation = activation_lock.then(|| io.shared.activation.lock().unwrap());
            assert!(poll(&mut configure).is_pending());
            assert_eq!(admission.used(ResourceClass::Registered), 8192);
            scope.cancel().unwrap();
            assert!(matches!(
                poll(&mut configure),
                Poll::Ready(Err(Error::Cancelled))
            ));
            drop(configure);
            assert_eq!(admission.used(ResourceClass::Registered), 0);
            assert!(!io.shared.configured.load(Ordering::Acquire));
            drop((config, activation));
            let scope = super::mailbox_tests::scope();
            let mut configure: Operation<'_, ()> = Box::pin(io.configure(
                vec![],
                vec![],
                vec![admission.reserve(None, ResourceClass::Registered, 8192).unwrap()],
                4096,
                &scope,
            ));
            done(&mut configure);
            native.poll_budgeted(1).unwrap();
            assert_eq!(admission.used(ResourceClass::Registered), 0);
        }
    }

    #[test]
    fn poisoned_mailbox_is_terminal_io_error_not_contention() {
        let mutex = std::sync::Mutex::new(());
        let _ = std::panic::catch_unwind(|| {
            let _guard = mutex.lock().unwrap();
            panic!("poison test mailbox");
        });
        assert!(matches!(
            crate::rdma::lifecycle::try_mailbox(&mutex),
            Poll::Ready(Err(Error::Io))
        ));
    }
}

#[cfg(test)]
mod activation_tests {
    //! Configured production activation through the simulated native ABI, not injected slots.
    use super::*;
    use crate::{
        memory::pool::BufferPool,
        model::{ResourceClass, *},
        rdma::Devices,
        runtime::{admission::Admission, crypto, worker::CryptoRuntime},
        security::{aead::PageCryptoEngine, identity::KeyPurpose},
    };
    use simulation::{Fault, Operation as NativeOp};
    use std::task::{Context, Poll};

    fn fixture(
        slots: usize,
    ) -> (
        simulation::Simulation,
        Devices,
        NativeService,
        Admission,
        RequestScope,
    ) {
        let sim = simulation::Simulation::new()
            .with_devices(vec![simulation::Device::new("sim0", [1; 16])])
            .unwrap();
        let (io, port) = pair(slots).unwrap();
        let native = {
            let _environment = sim.enter();
            NativeService::new(port)
        };
        let devices = Devices::new();
        devices.attach(io).unwrap();
        let admission = Admission::new(crate::test_support::cluster::config(true).limits);
        let scope = RequestScope::new(
            RequestId([1; 16]),
            crate::runtime::environment::now() + std::time::Duration::from_secs(30),
        )
        .unwrap();
        (sim, devices, native, admission, scope)
    }

    fn activate<'a>(
        devices: &'a Devices,
        admission: &'a Admission,
        scope: &'a RequestScope,
    ) -> crate::error::Operation<'a, Vec<RailMapping>> {
        devices.activate(
            vec![RailMapping {
                rail: RailId(0),
                fabric: "sim".into(),
                numa_node: None,
            }],
            vec![FabricPort {
                fabric: "sim".into(),
                device: "sim0".into(),
                port: 1,
                gid: None,
            }],
            admission,
            4096,
            scope,
        )
    }

    #[test]
    fn configured_activation_spends_budget_and_yields_to_sibling_page_jobs() {
        let (sim, devices, mut native, admission, scope) = fixture(4);
        let shared = native.port.shared.clone();
        let mut activation = activate(&devices, &admission, &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(activation.as_mut().poll(&mut cx).is_pending());
        assert!(
            !native.drained(),
            "queued configuration owns accepted quota"
        );
        native.poll_budgeted(0).unwrap();
        assert!(sim.trace().is_empty());
        assert_eq!(admission.used(ResourceClass::Registered), 4 * 8192);

        // Real page jobs on a sibling engine, polled on this same thread between
        // native turns just as the shared executor does. No synthetic progress counter.
        let keys = crate::security::identity::keyring_tests::keys();
        let page = PageId {
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId(crate::security::identity::tests::CACHE.into()),
                    key: CacheKey([3; 32]),
                },
                etag: StrongEtag::test_value("v1"),
            },
            number: PageNumber(0),
        };
        let cache = &page.version.object.cache;
        let sibling_admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let pool = BufferPool::new(sibling_admission.clone());
        let (io, port) = crypto::pair(WorkerId(1), 1, std::num::NonZeroUsize::new(1).unwrap());
        let mut sibling = PageCryptoEngine::new(CryptoRuntime { port });
        for turn in 0..5 {
            let Poll::Ready(Ok(permit)) = io.poll_reserve(
                &mut cx,
                crypto::CryptoId {
                    worker: WorkerId(1),
                    generation: 1,
                    sequence: turn + 1,
                },
            ) else {
                panic!("reserve")
            };
            assert!(
                io.try_submit(
                    permit.job(
                        crypto::CryptoInput::Encrypt {
                            page: page.clone(),
                            plaintext: pool
                                .plaintext(
                                    sibling_admission
                                        .reserve(Some(cache), ResourceClass::Plaintext, 5)
                                        .unwrap(),
                                    5
                                )
                                .unwrap(),
                            ciphertext: sibling_admission
                                .reserve(Some(cache), ResourceClass::Ciphertext, 21)
                                .unwrap(),
                        },
                        keys.active(cache, KeyPurpose::Page).unwrap(),
                        scope.clone(),
                    )
                )
                .is_ok()
            );
            native.poll_budgeted(1).unwrap();
            let trace = sim.take_trace();
            assert_eq!(
                trace
                    .iter()
                    .filter(|event| event.operation == NativeOp::Register)
                    .count(),
                usize::from(turn != 0)
            );
            assert_eq!(native.resources.iter().flatten().count(), turn as usize);
            if turn < 4 {
                assert!(activation.as_mut().poll(&mut cx).is_pending());
                assert!(
                    shared
                        .slots
                        .iter()
                        .all(|slot| slot.state.load(Ordering::Acquire) == IDLE)
                );
                assert!(!devices.ready(RailId(0)));
            }
            sibling.poll_budgeted(1).unwrap();
            let Poll::Ready(Ok(Some(completion))) = io.poll_completion(&mut cx) else {
                panic!("sibling must progress")
            };
            assert!(matches!(
                completion.outcome,
                crypto::CryptoOutcome::Completed(_)
            ));
            drop(completion);
            assert_eq!(sibling_admission.used(ResourceClass::Plaintext), 0);
        }
        assert!(matches!(
            activation.as_mut().poll(&mut cx),
            Poll::Ready(Ok(_))
        ));
        drop(activation);
        assert!(devices.ready(RailId(0)));
        devices.close();
        for remaining in (0..4).rev() {
            native.poll_budgeted(1).unwrap();
            assert_eq!(native.resources.iter().flatten().count(), remaining);
        }
        assert_eq!(admission.used(ResourceClass::Registered), 0);
        assert_eq!(sim.live_resources(), 0);
    }

    #[test]
    fn partial_activation_errors_fence_before_retry_without_publishing_readiness() {
        for failure in [NativeOp::Register, NativeOp::Qp, NativeOp::Window] {
            let (sim, devices, mut native, admission, scope) = fixture(3);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            let mut activation = activate(&devices, &admission, &scope);
            assert!(activation.as_mut().poll(&mut cx).is_pending());
            native.poll_budgeted(2).unwrap(); // Discovery plus exactly one slot.
            assert_eq!(native.resources.iter().flatten().count(), 1);
            sim.fault(failure, Fault::Reject);
            native.poll_budgeted(1).unwrap();
            assert!(matches!(
                activation.as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::Unavailable))
            ));
            drop(activation);
            assert!(!devices.ready(RailId(0)));
            assert!(
                native
                    .port
                    .shared
                    .slots
                    .iter()
                    .all(|slot| slot.state.load(Ordering::Acquire) == IDLE)
            );
            sim.fault(NativeOp::Stop, Fault::Reject);
            native.poll_budgeted(1).unwrap();
            assert!(native.resources[0].is_some());
            assert!(!native.port.shared.drained.load(Ordering::Acquire));
            assert!(admission.used(ResourceClass::Registered) >= 8192);
            let mut retry = activate(&devices, &admission, &scope);
            assert!(matches!(
                retry.as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::Overloaded))
            ));
            drop(retry);
            native.resources[0].as_mut().unwrap().next_retry = None;
            native.poll_budgeted(3).unwrap();
            assert!(native.port.shared.drained.load(Ordering::Acquire));
            assert_eq!(admission.used(ResourceClass::Registered), 0);
            assert_eq!(sim.live_resources(), 0);
            let mut retry = activate(&devices, &admission, &scope);
            assert!(retry.as_mut().poll(&mut cx).is_pending());
            native.poll_budgeted(3).unwrap();
            assert!(retry.as_mut().poll(&mut cx).is_pending());
            native.poll_budgeted(1).unwrap();
            assert!(matches!(retry.as_mut().poll(&mut cx), Poll::Ready(Ok(_))));
            drop(retry);
            devices.close();
            native.poll_budgeted(3).unwrap();
            assert_eq!(admission.used(ResourceClass::Registered), 0);
            assert_eq!(sim.live_resources(), 0);
        }
    }

    #[test]
    fn abandoned_activation_cleans_queued_discovered_and_partial_owners() {
        for turns in 0..=3 {
            let (sim, devices, mut native, admission, scope) = fixture(4);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            let mut activation = activate(&devices, &admission, &scope);
            assert!(activation.as_mut().poll(&mut cx).is_pending());
            native.poll_budgeted(turns).unwrap();
            assert!(!native.drained());
            drop(activation);
            native.poll_budgeted(1).unwrap();
            native.poll_budgeted(4).unwrap();
            assert!(native.drained());
            assert!(native.port.shared.drained.load(Ordering::Acquire));
            assert!(!devices.ready(RailId(0)));
            assert_eq!(admission.used(ResourceClass::Registered), 0);
            assert_eq!(sim.live_resources(), 0);
        }
    }

    #[test]
    fn activation_mailbox_contention_consumes_turn_without_losing_quota() {
        let (sim, devices, mut native, admission, scope) = fixture(2);
        let shared = native.port.shared.clone();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut activation = activate(&devices, &admission, &scope);
        assert!(activation.as_mut().poll(&mut cx).is_pending());
        native.poll_budgeted(1).unwrap();
        sim.take_trace();
        let guard = shared.slots[0].mailbox.lock().unwrap();
        native.poll_budgeted(1).unwrap();
        assert!(sim.trace().is_empty());
        assert_eq!(admission.used(ResourceClass::Registered), 2 * 8192);
        drop(guard);
        native.poll_budgeted(2).unwrap();
        assert!(matches!(
            activation.as_mut().poll(&mut cx),
            Poll::Ready(Ok(_))
        ));
        drop(activation);
        devices.close();
        native.poll_budgeted(2).unwrap();
        assert_eq!(admission.used(ResourceClass::Registered), 0);
        assert_eq!(sim.live_resources(), 0);
    }

    #[test]
    fn configured_native_wrapper_drain_fences_one_slot_per_poll() {
        let (sim, devices, native, admission, scope) = fixture(4);
        let (io, port) = crypto::pair(WorkerId(0), 1, std::num::NonZeroUsize::new(1).unwrap());
        io.close_submissions().unwrap();
        let mut service = WithNative {
            inner: PageCryptoEngine::new(CryptoRuntime { port }),
            native,
        };
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut activation = activate(&devices, &admission, &scope);
        assert!(activation.as_mut().poll(&mut cx).is_pending());
        service.poll_budgeted(4).unwrap();
        service.poll_budgeted(1).unwrap();
        assert!(matches!(
            activation.as_mut().poll(&mut cx),
            Poll::Ready(Ok(_))
        ));
        drop(activation);
        sim.take_trace();
        let mut drain = service.drain(&scope);
        for slot in 0..4 {
            let result = drain.as_mut().poll(&mut cx);
            if slot < 3 {
                assert!(result.is_pending());
                assert_eq!(admission.used(ResourceClass::Registered), 4 * 8192);
            } else {
                assert_eq!(result, Poll::Ready(Ok(())));
            }
            assert_eq!(
                sim.take_trace()
                    .iter()
                    .filter(|event| event.operation == NativeOp::Stop)
                    .count(),
                1
            );
        }
        drop(drain);
        assert_eq!(admission.used(ResourceClass::Registered), 0);
        assert_eq!(sim.live_resources(), 0);
    }

    #[test]
    fn cancellation_between_activation_turns_preserves_partial_owners_until_fenced() {
        let (sim, devices, mut native, admission, scope) = fixture(4);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut activation = activate(&devices, &admission, &scope);
        assert!(activation.as_mut().poll(&mut cx).is_pending());
        native.poll_budgeted(2).unwrap();
        scope.cancel().unwrap();
        assert!(matches!(
            activation.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Cancelled))
        ));
        drop(activation);
        assert_eq!(admission.used(ResourceClass::Registered), 4 * 8192);
        native.poll_budgeted(1).unwrap(); // Drop only the unprovisioned configuration.
        assert_eq!(admission.used(ResourceClass::Registered), 8192);
        assert!(!native.drained());
        native.poll_budgeted(1).unwrap();
        assert!(native.drained());
        assert_eq!(admission.used(ResourceClass::Registered), 0);
        assert_eq!(sim.live_resources(), 0);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::rdma::{Devices, Sessions};
    use std::task::Context;

    pub(super) fn provision_test(
        service: &mut NativeService,
        index: usize,
    ) -> ffi::lifetime_tests::QuotaObserver {
        let (qp, region, charged) = ffi::lifetime_tests::fresh_fixture();
        let device = qp.device().clone();
        let slot = &service.port.shared.slots[index];
        let mut mailbox = slot.mailbox.lock().unwrap();
        mailbox.endpoint = Some(qp.endpoint);
        mailbox.rail = RailId(0);
        mailbox.bytes = vec![0; 32];
        slot.state.store(READY, Ordering::Release);
        service.resources[index] = Some(Resource {
            device,
            region,
            qp: Some(qp),
            window: None,
            pending: None,
            stopping: false,
            next_retry: None,
        });
        charged
    }
    pub(super) fn claim(io: &Rc<IoPort>) -> Rc<QueuePairHandle> {
        QueuePairHandle::new(Rc::new(DeviceHandle {
            port: io.clone(),
            rail: RailId(0),
            generation: io.shared.generation.load(Ordering::Acquire),
        }))
        .unwrap()
    }
    #[test]
    fn admitted_native_claim_keeps_capacity_after_proxy_drop_until_service_fence() {
        let (io, port) = pair(1).unwrap();
        let io = Rc::new(io);
        let mut native = NativeService::new(port);
        let _charged = provision_test(&mut native, 0);
        let metrics = crate::telemetry::metrics::Metrics::default();
        let admission = crate::peer::adaptive::AdaptivePeers::new(
            crate::peer::adaptive::Config {
                total: 1,
                per_peer: 1,
            },
            metrics.clone(),
        )
        .unwrap();
        let peer = crate::model::NodeId("native-peer".into());
        let permit = admission.acquire(&peer).unwrap();
        let std::task::Poll::Ready(Ok(qp)) = QueuePairHandle::poll_new_admitted(
            Rc::new(DeviceHandle {
                port: io.clone(),
                rail: RailId(0),
                generation: 1,
            }),
            Some(permit),
        ) else {
            panic!("prepared native slot");
        };
        ffi::lifetime_tests::fail_stop(true);
        drop(qp);
        native.poll_budgeted(1).unwrap();
        assert_eq!(
            metrics.gauge(crate::telemetry::metrics::Gauge::PeerExchanges),
            1
        );
        assert!(matches!(admission.acquire(&peer), Err(Error::Overloaded)));
        ffi::lifetime_tests::fail_stop(false);
        native.resources[0].as_mut().unwrap().next_retry = None;
        native.poll_budgeted(1).unwrap();
        assert_eq!(
            metrics.gauge(crate::telemetry::metrics::Gauge::PeerExchanges),
            0
        );
    }

    #[test]
    fn failed_native_service_teardown_quarantines_adaptive_permit_after_both_roles_drop() {
        let (io, port) = pair(1).unwrap();
        let io = Rc::new(io);
        let mut native = NativeService::new(port);
        let charged = provision_test(&mut native, 0);
        let metrics = crate::telemetry::metrics::Metrics::default();
        let admission = crate::peer::adaptive::AdaptivePeers::new(
            crate::peer::adaptive::Config {
                total: 1,
                per_peer: 1,
            },
            metrics.clone(),
        )
        .unwrap();
        let peer = crate::model::NodeId("native-quarantine".into());
        let permit = admission.acquire(&peer).unwrap();
        let std::task::Poll::Ready(Ok(qp)) = QueuePairHandle::poll_new_admitted(
            Rc::new(DeviceHandle {
                port: io.clone(),
                rail: RailId(0),
                generation: 1,
            }),
            Some(permit),
        ) else {
            panic!("prepared native slot");
        };
        mark_connected(&qp, &mut native);
        let region = Region::acquire(&qp, 16).unwrap();
        let (window, ticket) = qp.bind(region).unwrap();
        native.poll_budgeted(1).unwrap();
        drop((window, ticket));
        ffi::lifetime_tests::fail_stop(true);
        drop(qp);
        drop(native);
        drop(io);
        assert_eq!(charged.get(), 1, "failed teardown keeps native ownership");
        assert_eq!(
            metrics.gauge(crate::telemetry::metrics::Gauge::PeerExchanges),
            1
        );
        assert!(matches!(admission.acquire(&peer), Err(Error::Overloaded)));
        ffi::lifetime_tests::fail_stop(false);
    }
    pub(super) fn mark_connected(qp: &QueuePairHandle, service: &mut NativeService) {
        qp.connect(qp.endpoint).unwrap();
        assert!(!qp.ready(), "connect is not executed on the I/O caller");
        service.poll_budgeted(8).unwrap();
        qp.progress().unwrap();
        assert!(qp.ready());
    }
    #[test]
    fn restart_requires_native_fence_and_last_lease_and_revokes_old_devices() {
        let (io, port) = pair(1).unwrap();
        let io = Rc::new(io);
        let mut native = NativeService::new(port);
        let charged = provision_test(&mut native, 0);
        let old_device = Rc::new(DeviceHandle {
            port: io.clone(),
            rail: RailId(0),
            generation: 1,
        });
        let qp = QueuePairHandle::new(old_device.clone()).unwrap();
        mark_connected(&qp, &mut native);
        let region = Region::acquire(&qp, 16).unwrap();
        io.close();
        ffi::lifetime_tests::fail_stop(true);
        native.poll_budgeted(1).unwrap();
        assert_eq!(io.reopen(), Err(Error::Overloaded));
        assert_eq!(charged.get(), 1);
        ffi::lifetime_tests::fail_stop(false);
        native.resources[0].as_mut().unwrap().next_retry = None;
        native.poll_budgeted(1).unwrap();
        assert!(qp.stopped());
        assert_eq!(io.reopen(), Err(Error::Overloaded));
        assert_eq!(region.copy_to().unwrap().len(), 16);
        drop(qp);
        native.poll_budgeted(1).unwrap();
        assert_eq!(io.reopen(), Err(Error::Overloaded));
        drop(region);
        native.poll_budgeted(1).unwrap();
        io.reopen().unwrap();
        assert_eq!(io.shared.generation.load(Ordering::Acquire), 2);
        provision_test(&mut native, 0);
        assert!(matches!(
            QueuePairHandle::new(old_device),
            Err(Error::Unavailable)
        ));
        let fresh = claim(&io);
        mark_connected(&fresh, &mut native);
        drop(fresh);
        io.close();
        native.poll_budgeted(1).unwrap();
        assert!(native.drained());
    }
    #[test]
    fn simultaneous_timeout_and_healthy_write_preserve_worker_and_quarantine() {
        let (io, port) = pair(2).unwrap();
        let io = Rc::new(io);
        let mut native = NativeService::new(port);
        let failed_charge = provision_test(&mut native, 0);
        let healthy_charge = provision_test(&mut native, 1);
        let failed = claim(&io);
        let healthy = claim(&io);
        mark_connected(&failed, &mut native);
        mark_connected(&healthy, &mut native);
        let receive = Region::acquire(&failed, 16).unwrap();
        let (_grant, binding) = failed.bind(receive.clone()).unwrap();
        native.poll_budgeted(2).unwrap();
        ffi::lifetime_tests::complete(1, 0, 5);
        native.poll_budgeted(2).unwrap();
        assert_eq!(binding.result(), Some(Ok(())));
        let source = Region::acquire(&healthy, 16).unwrap();
        source.copy_from(&[7; 16]).unwrap();
        let written = healthy.write(source, 4096, 17).unwrap();
        native.poll_budgeted(2).unwrap();
        failed.expire_at(std::time::Instant::now());
        let sessions = Sessions::new(Rc::new(Devices::new()), 2);
        sessions.track_test(failed.clone());
        sessions.track_test(healthy.clone());
        ffi::lifetime_tests::fail_stop(true);
        assert!(
            sessions.progress().is_ok(),
            "attempt timeout must not fail app's worker poll"
        );
        assert_eq!(failed.progress(), Err(Error::DeadlineExceeded));
        assert!(!failed.stopped());
        assert!(healthy.ready());
        ffi::lifetime_tests::complete(1, 0, 1);
        native.poll_budgeted(2).unwrap();
        assert!(sessions.progress().is_ok());
        assert_eq!(written.result(), Some(Ok(())));
        assert_eq!(failed_charge.get(), 1);
        assert_eq!(healthy_charge.get(), 1);
        assert_eq!(receive.copy_to(), Err(Error::Unavailable));
        ffi::lifetime_tests::fail_stop(false);
        native.resources[0].as_mut().unwrap().next_retry = None;
        native.poll_budgeted(2).unwrap();
        assert!(failed.stopped());
        assert!(receive.copy_to().is_ok());
        assert!(healthy.ready());
    }
    #[test]
    fn io_cancel_and_poll_do_not_wait_for_native_thread_or_mailbox_lock() {
        let (io, port) = pair(1).unwrap();
        let io = Rc::new(io);
        let (ready_tx, ready_rx) = std::sync::mpsc::channel();
        let (release_tx, release_rx) = std::sync::mpsc::channel();
        let thread = std::thread::spawn(move || {
            let mut service = NativeService::new(port);
            let charged = provision_test(&mut service, 0);
            ready_tx.send(()).unwrap();
            release_rx.recv().unwrap();
            service.poll_budgeted(1).unwrap();
            assert_eq!(charged.get(), 1);
            service.close();
            service.poll_budgeted(1).unwrap();
        });
        ready_rx.recv().unwrap();
        let qp = claim(&io);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        qp.stop().unwrap();
        assert!(
            qp.poll_stopped(&mut cx).is_pending(),
            "must await actual native destroy"
        );
        assert!(!qp.stopped());
        // The service has not run at all while these I/O operations complete.
        release_tx.send(()).unwrap();
        thread.join().unwrap();
        assert!(qp.stopped());
        assert!(qp.poll_stopped(&mut cx).is_ready());
    }
    #[test]
    fn endpoint_handoff_is_send_and_capacity_is_bounded() {
        fn send<T: Send>() {}
        send::<IoPort>();
        send::<NativePort>();
        assert!(pair(0).is_err());
        assert!(pair(257).is_err());
    }
    #[test]
    fn retirement_cut_is_captured_and_does_not_stop_later_sessions() {
        let (io, port) = pair(2).unwrap();
        let io = Rc::new(io);
        let mut native = NativeService::new(port);
        provision_test(&mut native, 0);
        provision_test(&mut native, 1);
        let first = claim(&io);
        mark_connected(&first, &mut native);
        let sessions = Sessions::new(Rc::new(Devices::new()), 2);
        sessions.track_test(first.clone());
        let mut cut = sessions.fence_cut();
        let second = claim(&io);
        mark_connected(&second, &mut native);
        sessions.track_test(second.clone());
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(cut.as_mut().poll(&mut cx).is_pending());
        assert!(!first.ready());
        assert!(!first.stopped());
        assert!(second.ready());
        native.poll_budgeted(2).unwrap();
        assert!(matches!(
            cut.as_mut().poll(&mut cx),
            std::task::Poll::Ready(Ok(()))
        ));
        assert!(first.stopped());
        assert!(second.ready());
        assert!(!io.shared.closed.load(Ordering::Acquire));
        assert!(sessions.progress().is_ok());
    }
    #[test]
    fn cq_failure_is_attempt_local_and_slot_waits_for_all_leases_before_reuse() {
        let (io, port) = pair(2).unwrap();
        let io = Rc::new(io);
        let mut native = NativeService::new(port);
        provision_test(&mut native, 0);
        provision_test(&mut native, 1);
        let failed = claim(&io);
        let healthy = claim(&io);
        mark_connected(&failed, &mut native);
        mark_connected(&healthy, &mut native);
        let region = Region::acquire(&failed, 16).unwrap();
        region.copy_from(&[1; 16]).unwrap();
        let ticket = failed.write(region.clone(), 4096, 7).unwrap();
        native.poll_budgeted(2).unwrap();
        ffi::lifetime_tests::complete(1, 10, u32::MAX);
        native.poll_budgeted(2).unwrap();
        let sessions = Sessions::new(Rc::new(Devices::new()), 2);
        sessions.track_test(failed.clone());
        sessions.track_test(healthy.clone());
        assert!(sessions.progress().is_ok());
        assert_eq!(ticket.result(), Some(Err(Error::Io)));
        assert!(healthy.ready());
        assert!(!failed.stopped());
        native.poll_budgeted(2).unwrap();
        sessions.progress().unwrap();
        assert!(failed.stopped());
        drop(failed);
        drop(ticket);
        native.poll_budgeted(2).unwrap();
        assert_eq!(io.shared.slots[0].state.load(Ordering::Acquire), OWNED);
        drop(region);
        native.poll_budgeted(2).unwrap();
        assert_eq!(io.shared.slots[0].state.load(Ordering::Acquire), READY);
        assert!(healthy.ready());
    }
    #[test]
    fn mailbox_contention_never_blocks_cancel_or_other_session() {
        let (io, port) = pair(2).unwrap();
        let io = Rc::new(io);
        let mut native = NativeService::new(port);
        provision_test(&mut native, 0);
        provision_test(&mut native, 1);
        let one = claim(&io);
        let two = claim(&io);
        mark_connected(&one, &mut native);
        mark_connected(&two, &mut native);
        let _guard = io.shared.slots[0].mailbox.lock().unwrap();
        one.stop().unwrap();
        assert!(!one.stopped());
        assert!(one.progress().is_ok());
        assert!(two.progress().is_ok());
        assert!(two.ready());
    }
    #[cfg(feature = "rdma")]
    #[test]
    #[ignore = "requires built native ABI v2 adapter and zero usable type-2B ports"]
    fn native_no_device_activation_runs_on_paired_role_and_releases_quota() {
        use crate::{
            model::RequestId,
            model::ResourceClass,
            runtime::{admission::Admission, deadline::RequestScope},
        };
        assert!(ffi::discover().expect("real adapter must load").is_empty());
        let (io, port) = pair(1).unwrap();
        let devices = Devices::new();
        devices.attach(io).unwrap();
        let admission = Admission::new(crate::test_support::cluster::config(true).limits);
        let scope = RequestScope::new(
            RequestId([8; 16]),
            std::time::Instant::now() + std::time::Duration::from_secs(10),
        )
        .unwrap();
        let mut activation = devices.activate(
            vec![RailMapping {
                rail: RailId(1),
                fabric: "known".into(),
                numa_node: None,
            }],
            vec![FabricPort {
                fabric: "known".into(),
                device: "missing".into(),
                port: 1,
                gid: None,
            }],
            &admission,
            4096,
            &scope,
        );
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(activation.as_mut().poll(&mut cx).is_pending());
        assert_eq!(admission.used(ResourceClass::Registered), 8192);
        std::thread::spawn(move || {
            let mut service = NativeService::new(port);
            service.poll_budgeted(1).unwrap();
        })
        .join()
        .unwrap();
        assert!(matches!(
            activation.as_mut().poll(&mut cx),
            std::task::Poll::Ready(Err(Error::Unavailable))
        ));
        drop(activation);
        assert_eq!(admission.used(ResourceClass::Registered), 0);
        assert!(!devices.ready(RailId(1)));
    }
    #[test]
    fn dropped_activation_does_not_publish_readiness_or_release_accepted_quota_early() {
        use crate::{
            model::RequestId,
            model::ResourceClass,
            runtime::{admission::Admission, deadline::RequestScope},
        };
        let (io, port) = pair(1).unwrap();
        let devices = Devices::new();
        devices.attach(io).unwrap();
        let admission = Admission::new(crate::test_support::cluster::config(true).limits);
        let scope = RequestScope::new(
            RequestId([9; 16]),
            std::time::Instant::now() + std::time::Duration::from_secs(10),
        )
        .unwrap();
        let mut operation = devices.activate(Vec::new(), Vec::new(), &admission, 4096, &scope);
        assert!(
            operation
                .as_mut()
                .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
                .is_pending()
        );
        drop(operation);
        assert_eq!(admission.used(ResourceClass::Registered), 8192);
        let mut service = NativeService::new(port);
        service.poll_budgeted(1).unwrap();
        assert_eq!(admission.used(ResourceClass::Registered), 0);
        assert!(!devices.ready(RailId(0)));
    }
    #[cfg(feature = "rdma")]
    #[test]
    #[ignore = "requires operator-selected active type-2B provider; real pooled RC loopback"]
    fn native_available_provider_pooled_service_roundtrip() {
        use crate::{
            model::RequestId,
            runtime::{admission::Admission, deadline::RequestScope},
        };
        let name = std::env::var("RACER_RDMA_TEST_DEVICE")
            .expect("select native test provider explicitly");
        let ports = ffi::discover().expect("real native adapter");
        let selected = ports
            .iter()
            .find(|d| d.name == name)
            .expect("active type-2B provider");
        let association = FabricPort {
            fabric: "operator-test-loopback".into(),
            device: name,
            port: selected.endpoint.port,
            gid: Some(selected.endpoint.gid),
        };
        let publication = RailMapping {
            rail: RailId(0),
            fabric: association.fabric.clone(),
            numa_node: selected.numa_node(),
        };
        let (io, port) = pair(2).unwrap();
        let devices = Devices::new();
        devices.attach(io).unwrap();
        let admission = Admission::new(crate::test_support::cluster::config(true).limits);
        let scope = RequestScope::new(
            RequestId([4; 16]),
            std::time::Instant::now() + std::time::Duration::from_secs(15),
        )
        .unwrap();
        let mut native = NativeService::new(port);
        let mut activate = devices.activate(
            vec![publication],
            vec![association],
            &admission,
            4096,
            &scope,
        );
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(activate.as_mut().poll(&mut cx).is_pending());
        native.poll_budgeted(2).unwrap();
        assert!(activate.as_mut().poll(&mut cx).is_pending());
        native.poll_budgeted(1).unwrap();
        assert!(matches!(
            activate.as_mut().poll(&mut cx),
            std::task::Poll::Ready(Ok(_))
        ));
        drop(activate);
        let device = devices.select(RailId(0)).unwrap();
        let sender = QueuePairHandle::new(device.handle.clone()).unwrap();
        let receiver = QueuePairHandle::new(device.handle).unwrap();
        sender.connect(receiver.endpoint).unwrap();
        receiver.connect(sender.endpoint).unwrap();
        native.poll_budgeted(2).unwrap();
        sender.progress().unwrap();
        receiver.progress().unwrap();
        assert!(sender.ready());
        assert!(receiver.ready());
        let target = Region::acquire(&receiver, 17).unwrap();
        let (window, bind) = receiver.bind(target.clone()).unwrap();
        fn wait(
            native: &mut NativeService,
            ticket: &crate::rdma::lifecycle::Ticket,
            until: std::time::Instant,
        ) {
            while ticket.result().is_none() {
                assert!(std::time::Instant::now() < until);
                native.poll_budgeted(2).unwrap();
                std::thread::sleep(std::time::Duration::from_millis(1));
            }
            ticket.result().unwrap().unwrap();
        }
        wait(&mut native, &bind, scope.deadline.0);
        let source = Region::acquire(&sender, 17).unwrap();
        source.copy_from(&[0xa5; 17]).unwrap();
        let write = sender
            .write(source, window.address.get(), window.key.get())
            .unwrap();
        wait(&mut native, &write, scope.deadline.0);
        let invalidate = receiver.invalidate(window).unwrap();
        wait(&mut native, &invalidate, scope.deadline.0);
        assert_eq!(target.copy_to(), Err(Error::Unavailable));
        receiver.stop().unwrap();
        sender.stop().unwrap();
        native.poll_budgeted(2).unwrap();
        assert!(receiver.stopped());
        assert_eq!(target.copy_to().unwrap(), [0xa5; 17]);
        // A short transfer used the existing 4KiB MR; it never exposed padding.
        assert_eq!(native.resources[0].as_ref().unwrap().region.length(), 17);
        native.close();
        native.poll_budgeted(2).unwrap();
        assert!(native.drained());
    }
}
