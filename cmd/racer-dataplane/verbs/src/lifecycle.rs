//! Bounded native service for an existing paired native thread. No thread is
//! spawned here. I/O only tries mailboxes; native calls and destruction run here.
use super::ffi;
pub use super::ffi::Endpoint;
use crate::{Error, Guard, GuardOwner, Result};
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
#[cfg(any(test, feature = "simulation"))]
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
#[cfg(test)]
fn immediate<T>(poll: Poll<Result<T>>) -> Result<T> {
    match poll {
        Poll::Ready(result) => result,
        Poll::Pending => Err(Error::Overloaded),
    }
}
pub struct DeviceHandle {
    pub(crate) port: Rc<IoPort>,
    pub(crate) rail: u32,
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
pub struct Region {
    lease: Rc<Lease>,
    length: usize,
}
impl Region {
    #[cfg(test)]
    pub(crate) fn acquire(qp: &QueuePairHandle, length: usize) -> Result<Rc<Self>> {
        immediate(Self::poll_acquire(qp, length))
    }
    pub fn poll_acquire(qp: &QueuePairHandle, length: usize) -> Poll<Result<Rc<Self>>> {
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
    pub fn length(&self) -> usize {
        self.length
    }
    #[cfg(test)]
    pub(crate) fn copy_from(&self, bytes: &[u8]) -> Result<()> {
        immediate(self.poll_copy_from(bytes))
    }
    pub fn poll_copy_from(&self, bytes: &[u8]) -> Poll<Result<()>> {
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
    pub fn register_waiter(&self, cx: &Context<'_>) {
        self.lease.slot.waiter.register(cx.waker());
    }
    #[cfg(test)]
    pub(crate) fn copy_to(&self) -> Result<Vec<u8>> {
        immediate(self.poll_copy_to(&mut Context::from_waker(futures::task::noop_waker_ref())))
    }
    pub fn poll_copy_to(&self, cx: &mut Context<'_>) -> Poll<Result<Vec<u8>>> {
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
pub struct Window {
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
pub struct Ticket(Rc<TicketState>);
impl Ticket {
    pub fn result(&self) -> Option<Result<()>> {
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
    pub fn poll(&self, cx: &mut Context<'_>) -> Poll<Result<()>> {
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
    pub fn poll_new(device: Rc<DeviceHandle>) -> Poll<Result<Rc<Self>>> {
        Self::poll_new_admitted(device, None)
    }
    pub fn poll_new_admitted(
        device: Rc<DeviceHandle>,
        permit: Option<Guard>,
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
    pub fn poll_connect(&self, remote: Endpoint) -> Poll<Result<()>> {
        remote.validate()?;
        if self.connecting.borrow().is_some() || remote.link_layer != self.endpoint.link_layer {
            return Poll::Ready(Err(Error::InvalidRequest));
        }
        *self.connecting.borrow_mut() =
            Some(ready!(self.poll_submit(Command::Connect(remote), None))?);
        Poll::Ready(Ok(()))
    }
    pub fn ready(&self) -> bool {
        self.connected.get()
            && self.failure.get().is_none()
            && !self.lease.shared.closed.load(Ordering::Acquire)
            && !self.lease.slot.cancel.load(Ordering::Acquire)
    }
    pub fn stopped(&self) -> bool {
        self.lease.slot.fenced.load(Ordering::Acquire)
    }
    pub fn expire_at(&self, deadline: std::time::Instant) {
        self.expires.set(Some(deadline));
    }
    #[cfg(test)]
    pub(crate) fn bind(&self, region: Rc<Region>) -> Result<(Rc<Window>, Ticket)> {
        immediate(self.poll_bind(region))
    }
    pub fn poll_bind(&self, region: Rc<Region>) -> Poll<Result<(Rc<Window>, Ticket)>> {
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
    pub fn poll_invalidate(
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
    pub fn poll_write(&self, region: Rc<Region>, address: u64, key: u32) -> Poll<Result<Ticket>> {
        if !self.ready() || !Rc::ptr_eq(&region.lease, &self.lease) {
            return Poll::Ready(Err(Error::Unavailable));
        }
        self.poll_submit(Command::Write { address, key }, None)
    }
    pub fn register_waiter(&self, cx: &Context<'_>) {
        self.lease.slot.waiter.register(cx.waker());
    }
    pub fn progress(&self) -> Result<usize> {
        if self.lease.shared.closed.load(Ordering::Acquire) && !self.stopped() {
            self.failure.set(Some(Error::Unavailable));
        }
        if self
            .expires
            .get()
            .is_some_and(|d| uring_runtime::environment::now() >= d)
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
    pub fn poll_stopped(&self, cx: &mut Context<'_>) -> Poll<Result<()>> {
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
    pub fn poll_connected(&self, cx: &mut Context<'_>) -> Poll<Result<()>> {
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
    pub peer_admission: Option<Guard>,
    pub endpoint: Option<Endpoint>,
    pub rail: u32,
    pub length: usize,
    pub bytes: Vec<u8>,
    pub command: Option<Command>,
    pub result: Option<Result<()>>,
    pub descriptor: Option<(u64, u32)>,
    pub quota: Option<Arc<GuardOwner>>,
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
    activation: Mutex<Option<Result<Vec<Selection>>>>,
}
/// Description available to the selector on the native owner thread.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct PortInfo {
    pub device: String,
    pub port: u8,
    pub gid: [u8; 16],
    pub numa_node: Option<usize>,
}
/// Caller tag associated with one discovered physical port.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Selection {
    pub tag: u32,
    pub index: usize,
    pub port: PortInfo,
}
/// Owned plan retained while configuration mailboxes are contended.
pub struct Configuration {
    /// Skip native discovery for an intentionally empty plan. The selector
    /// receives an empty slice and must return an empty selection.
    pub discover: bool,
    pub selector: Box<dyn FnOnce(&[PortInfo]) -> Result<Vec<(u32, usize)>> + Send>,
    pub guards: Vec<Guard>,
    pub bytes: usize,
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
                        rail: 0,
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
    pub fn device(self: &Rc<Self>, tag: u32) -> Rc<DeviceHandle> {
        Rc::new(DeviceHandle {
            port: self.clone(),
            rail: tag,
            generation: self.shared.generation.load(Ordering::Acquire),
        })
    }
    pub fn closed(&self) -> bool {
        self.shared.closed.load(Ordering::Acquire)
    }
    pub fn capacity(&self) -> usize {
        self.shared.slots.len()
    }
    pub fn register_driver(&self, waker: &Waker) {
        self.shared.io.register(waker);
    }
    /// Reopen only after the native role has destroyed every device owner and
    /// all I/O leases have released their fenced staging. No timer is a fence.
    pub fn reopen(&self) -> Result<()> {
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
    pub async fn configure(&self, configuration: Configuration) -> Result<()> {
        let mut configuration = Some(configuration);
        std::future::poll_fn(|cx| {
            self.register_driver(cx.waker());
            if self.shared.closed.load(Ordering::Acquire)
                || configuration.as_ref().unwrap().guards.len() != self.capacity()
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
    pub fn activation(&self) -> Option<Result<Vec<Selection>>> {
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
    selected: Vec<Selection>,
    quotas: std::vec::IntoIter<Guard>,
    bytes: usize,
    next: usize,
}
/// Construct only inside build_crypto, after its NativePort crossed threads.
/// Rc native owners never cross threads, even during cancellation or shutdown.
pub struct NativeService {
    environment: uring_runtime::environment::Environment,
    port: NativePort,
    resources: Vec<Option<Resource>>,
    activation: Option<Activation>,
    cursor: usize,
    #[cfg(any(test, feature = "simulation"))]
    simulation: Option<simulation::Simulation>,
}
impl NativeService {
    pub fn new(port: NativePort) -> Self {
        let resources = (0..port.shared.slots.len()).map(|_| None).collect();
        Self {
            environment: uring_runtime::environment::Environment::current(),
            port,
            resources,
            activation: None,
            cursor: 0,
            #[cfg(any(test, feature = "simulation"))]
            simulation: simulation::current(),
        }
    }
    pub fn register_driver(&self, waker: &Waker) {
        self.port.shared.engine.register(waker);
    }
    fn begin_activation(&mut self, config: Configuration) -> Result<Option<Vec<Selection>>> {
        #[cfg(any(test, feature = "simulation"))]
        let _environment = self.simulation.as_ref().map(simulation::Simulation::enter);
        if self.port.shared.closed.load(Ordering::Acquire) {
            return Err(Error::Unavailable);
        }
        if config.bytes == 0 || config.bytes > u32::MAX as usize {
            return Err(Error::InvalidRange);
        }
        let discovered = if config.discover {
            ffi::discover()?
        } else {
            Vec::new()
        };
        let descriptions = discovered
            .iter()
            .map(|d| PortInfo {
                device: d.name.clone(),
                port: d.endpoint.port,
                gid: d.endpoint.gid,
                numa_node: d.numa_node(),
            })
            .collect::<Vec<_>>();
        let plan = (config.selector)(&descriptions)?;
        let mut selected: Vec<Selection> = Vec::new();
        for (tag, index) in plan {
            if selected.iter().any(|s| s.tag == tag || s.index == index) {
                return Err(Error::InvalidConfiguration);
            }
            let port = descriptions
                .get(index)
                .ok_or(Error::InvalidConfiguration)?
                .clone();
            selected.push(Selection { tag, index, port });
        }
        if selected.is_empty() {
            return Ok(Some(Vec::new()));
        }
        if selected.len() > self.resources.len() {
            return Err(Error::Overloaded);
        }
        self.activation = Some(Activation {
            devices: discovered.into_iter().map(Rc::new).collect(),
            selected,
            quotas: config.guards.into_iter(),
            bytes: config.bytes,
            next: 0,
        });
        Ok(None)
    }
    fn activate_slot(&mut self) -> Result<Option<Vec<Selection>>> {
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
        let selected = &activation.selected[i % activation.selected.len()];
        let device = activation.devices[selected.index].clone();
        let quota = GuardOwner::new(
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
        mailbox.rail = selected.tag;
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
        Ok(Some(activation.selected.iter().cloned().collect()))
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
                .is_some_and(|at| uring_runtime::environment::now() < at)
            {
                return;
            }
            if let Some(qp) = &resource.qp {
                if qp.stop().is_err() {
                    resource.next_retry = Some(
                        uring_runtime::environment::now() + std::time::Duration::from_millis(10),
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
                        uring_runtime::environment::now() + std::time::Duration::from_millis(100),
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

impl Window {
    pub fn key(&self) -> u32 {
        self.key.get()
    }
    pub fn address(&self) -> u64 {
        self.address.get()
    }
}

/// Narrow observation and contention controls for deterministic integration tests.
/// These never expose a mailbox, native owner, pointer, or lock guard.
#[cfg(feature = "simulation")]
pub mod testing {
    use super::*;
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub enum State {
        Idle,
        Ready,
        Owned,
        Retired,
    }
    #[derive(Clone, Copy, Debug)]
    pub struct Snapshot {
        pub state: State,
        pub cancelled: bool,
        pub fenced: bool,
    }
    pub enum Contention {
        Slot(usize),
        Configuration,
        Activation,
    }
    impl IoPort {
        pub fn with_contention<T>(&self, at: Contention, f: impl FnOnce() -> T) -> T {
            match at {
                Contention::Slot(i) => {
                    let _lock = self.shared.slots[i].mailbox.lock().unwrap();
                    f()
                }
                Contention::Configuration => {
                    let _lock = self.shared.config.lock().unwrap();
                    f()
                }
                Contention::Activation => {
                    let _lock = self.shared.activation.lock().unwrap();
                    f()
                }
            }
        }
        pub fn snapshot(&self, i: usize) -> Snapshot {
            let slot = &self.shared.slots[i];
            Snapshot {
                state: match slot.state.load(Ordering::Acquire) {
                    IDLE => State::Idle,
                    READY => State::Ready,
                    OWNED => State::Owned,
                    _ => State::Retired,
                },
                cancelled: slot.cancel.load(Ordering::Acquire),
                fenced: slot.fenced.load(Ordering::Acquire),
            }
        }
        pub fn configuration_submitted(&self) -> bool {
            self.shared.configured.load(Ordering::Acquire)
        }
        pub fn pool_drained(&self) -> bool {
            self.shared.drained.load(Ordering::Acquire)
        }
        pub fn generation(&self) -> u64 {
            self.shared.generation.load(Ordering::Acquire)
        }
        pub fn command_pending(&self, i: usize) -> bool {
            self.shared.slots[i]
                .mailbox
                .lock()
                .unwrap()
                .command
                .is_some()
        }
    }
    impl NativeService {
        pub fn resource_count(&self) -> usize {
            self.resources.iter().flatten().count()
        }
        pub fn resource_present(&self, i: usize) -> bool {
            self.resources[i].is_some()
        }
        pub fn retry_now(&mut self, i: usize) {
            self.resources[i].as_mut().unwrap().next_retry = None;
        }
        pub fn region_length(&self, i: usize) -> usize {
            self.resources[i].as_ref().unwrap().region.length()
        }
    }
}

#[cfg(test)]
mod mailbox_tests {
    //! Hold native mailboxes at each public handoff, independently of thread timing.
    use super::tests::{claim, mark_connected, provision_test};
    use super::*;

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
    fn poisoned_mailbox_is_terminal_io_error_not_contention() {
        let mutex = std::sync::Mutex::new(());
        let _ = std::panic::catch_unwind(|| {
            let _guard = mutex.lock().unwrap();
            panic!("poison test mailbox");
        });
        assert!(matches!(try_mailbox(&mutex), Poll::Ready(Err(Error::Io))));
    }
}

#[cfg(test)]
mod tests {
    use super::*;
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
        mailbox.rail = 0;
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
            rail: 0,
            generation: io.shared.generation.load(Ordering::Acquire),
        }))
        .unwrap()
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
            rail: 0,
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
    #[cfg(feature = "native")]
    #[test]
    #[ignore = "requires built native ABI v2 adapter and zero usable type-2B ports"]
    fn native_no_device_activation_runs_on_paired_role_and_releases_quota() {
        assert!(ffi::discover().expect("real adapter must load").is_empty());
        let (io, port) = pair(1).unwrap();
        let (guard, charged) = crate::test_guard::guard();
        futures::executor::block_on(io.configure(Configuration {
            discover: true,
            bytes: 4096,
            guards: vec![guard],
            selector: Box::new(|ports| {
                assert!(ports.is_empty());
                Err(Error::Unavailable)
            }),
        }))
        .unwrap();
        let mut activation = Box::pin(std::future::poll_fn(|_| {
            io.activation().map_or(Poll::Pending, Poll::Ready)
        }));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(activation.as_mut().poll(&mut cx).is_pending());
        assert_eq!(charged.get(), 1);
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
        assert_eq!(charged.get(), 0);
        assert!(io.closed());
    }
    #[cfg(feature = "native")]
    #[test]
    #[ignore = "requires operator-selected active type-2B provider; real pooled RC loopback"]
    fn native_available_provider_pooled_service_roundtrip() {
        let name = std::env::var("RDMA_VERBS_TEST_DEVICE")
            .expect("select native test provider explicitly");
        let ports = ffi::discover().expect("real native adapter");
        let selected = ports
            .iter()
            .find(|d| d.name == name)
            .expect("active type-2B provider");
        let expected = (
            selected.endpoint.port,
            selected.endpoint.gid,
            selected.numa_node(),
        );
        let (io, port) = pair(2).unwrap();
        let devices = Rc::new(io);
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(15);
        let mut native = NativeService::new(port);
        futures::executor::block_on(devices.configure(Configuration {
            discover: true,
            bytes: 4096,
            guards: vec![crate::test_guard::guard().0, crate::test_guard::guard().0],
            selector: Box::new(move |ports| {
                let index = ports
                    .iter()
                    .position(|p| p.device == name && (p.port, p.gid, p.numa_node) == expected)
                    .ok_or(Error::Unavailable)?;
                Ok(vec![(0, index)])
            }),
        }))
        .unwrap();
        let mut activate = Box::pin(std::future::poll_fn(|_| {
            devices.activation().map_or(Poll::Pending, Poll::Ready)
        }));
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
        let device = devices.device(0);
        let sender = QueuePairHandle::new(device.clone()).unwrap();
        let receiver = QueuePairHandle::new(device).unwrap();
        sender.connect(receiver.endpoint).unwrap();
        receiver.connect(sender.endpoint).unwrap();
        native.poll_budgeted(2).unwrap();
        sender.progress().unwrap();
        receiver.progress().unwrap();
        assert!(sender.ready());
        assert!(receiver.ready());
        let target = Region::acquire(&receiver, 17).unwrap();
        let (window, bind) = receiver.bind(target.clone()).unwrap();
        fn wait(native: &mut NativeService, ticket: &Ticket, until: std::time::Instant) {
            while ticket.result().is_none() {
                assert!(std::time::Instant::now() < until);
                native.poll_budgeted(2).unwrap();
                std::thread::sleep(std::time::Duration::from_millis(1));
            }
            ticket.result().unwrap().unwrap();
        }
        wait(&mut native, &bind, deadline);
        let source = Region::acquire(&sender, 17).unwrap();
        source.copy_from(&[0xa5; 17]).unwrap();
        let write = sender
            .write(source, window.address.get(), window.key.get())
            .unwrap();
        wait(&mut native, &write, deadline);
        let invalidate = receiver.invalidate(window).unwrap();
        wait(&mut native, &invalidate, deadline);
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
