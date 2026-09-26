//! I/O-local proxies. All native calls run on the paired NativeService.
pub use super::backend::Endpoint;
use super::lifecycle::{Command, IoPort, OWNED, READY, Shared, Slot};
use crate::error::{Error, Result};
use std::{
    cell::{Cell, RefCell},
    rc::Rc,
    sync::{Arc, Mutex, MutexGuard, TryLockError, atomic::Ordering},
    task::{Context, Poll, ready},
};

// Contention is not admission failure. Callers register wakes and retain owners
// across Pending; bounded worker ticks cover unlocks without a completion wake.
pub(crate) fn try_mailbox<T>(mailbox: &Mutex<T>) -> Poll<Result<MutexGuard<'_, T>>> {
    match mailbox.try_lock() {
        Ok(guard) => Poll::Ready(Ok(guard)),
        Err(TryLockError::WouldBlock) => Poll::Pending,
        Err(TryLockError::Poisoned(_)) => Poll::Ready(Err(Error::Io)),
    }
}

pub(crate) async fn wait<T>(
    scope: &crate::runtime::deadline::RequestScope,
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

pub struct Verbs;
impl Verbs {
    /// Serving workers must use Devices::activate; synchronous discovery is not
    /// supported here. The real discovery operation lives on NativeService.
    pub fn discover(&self) -> Result<Vec<DeviceHandle>> {
        Err(Error::Unavailable)
    }
}
pub struct DeviceHandle {
    pub(crate) port: Rc<IoPort>,
    pub(crate) rail: crate::topology::rails::RailId,
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
    fn copy_bytes(&self, mailbox: &super::lifecycle::Mailbox) -> Result<Vec<u8>> {
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
    device: Rc<DeviceHandle>,
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
    pub(crate) fn poll_new(device: Rc<DeviceHandle>) -> Poll<Result<Rc<Self>>> {
        if device.port.shared.closed.load(Ordering::Acquire) {
            return Poll::Ready(Err(Error::Unavailable));
        }
        let mut contended = false;
        for slot in &device.port.shared.slots {
            if slot.state.load(Ordering::Acquire) != READY {
                continue;
            }
            let mailbox = match try_mailbox(&slot.mailbox) {
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
            return Poll::Ready(Ok(Rc::new(Self {
                device: device.clone(),
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
    pub(crate) fn device(&self) -> &Rc<DeviceHandle> {
        &self.device
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
            .is_some_and(|d| std::time::Instant::now() >= d)
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
