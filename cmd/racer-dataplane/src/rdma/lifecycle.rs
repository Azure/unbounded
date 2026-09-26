//! Bounded native service for the EXISTING paired crypto thread. No thread is
//! spawned here. I/O only tries mailboxes; native calls and destruction run here.
use super::{
    backend,
    device::{FabricPort, match_publication},
    verbs::Endpoint,
};
use crate::{
    error::{Error, Result},
    runtime::{admission::Reservation, deadline::RequestScope, worker::CryptoService},
    topology::rails::{RailId, RailMapping},
};
use futures::task::AtomicWaker;
use std::{
    rc::Rc,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicU8, Ordering},
    },
    task::Waker,
};

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
pub(crate) struct Shared {
    pub slots: Vec<Arc<Slot>>,
    pub engine: AtomicWaker,
    pub io: AtomicWaker,
    pub closed: AtomicBool,
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
    pub(crate) fn configure(
        &self,
        publication: Vec<RailMapping>,
        associations: Vec<FabricPort>,
        quotas: Vec<Reservation>,
        bytes: usize,
    ) -> Result<()> {
        if self.shared.closed.load(Ordering::Acquire) || quotas.len() != self.capacity() {
            return Err(Error::Unavailable);
        }
        let mut config = self
            .shared
            .config
            .try_lock()
            .map_err(|_| Error::Overloaded)?;
        if config.is_some()
            || self
                .shared
                .slots
                .iter()
                .any(|s| s.state.load(Ordering::Acquire) != IDLE)
        {
            return Err(Error::InvalidConfiguration);
        }
        *self
            .shared
            .activation
            .try_lock()
            .map_err(|_| Error::Overloaded)? = None;
        if self.shared.configured.swap(true, Ordering::AcqRel) {
            return Err(Error::InvalidConfiguration);
        }
        *config = Some(Configuration {
            publication,
            associations,
            quotas,
            bytes,
        });
        self.shared.engine.wake();
        Ok(())
    }
    pub(crate) fn activation(&self) -> Option<Result<Vec<RailMapping>>> {
        self.shared.activation.try_lock().ok()?.take()
    }
    pub fn close(&self) {
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
    device: Rc<backend::DeviceHandle>,
    region: Rc<backend::Region>,
    qp: Option<Rc<backend::QueuePairHandle>>,
    window: Option<Rc<backend::Window>>,
    pending: Option<backend::Ticket>,
    stopping: bool,
    next_retry: Option<std::time::Instant>,
}
/// Construct only inside build_crypto, after its NativePort crossed threads.
/// Rc native owners never cross threads, even during cancellation or shutdown.
pub struct NativeService {
    port: NativePort,
    resources: Vec<Option<Resource>>,
    cursor: usize,
}
impl NativeService {
    pub fn new(port: NativePort) -> Self {
        let resources = (0..port.shared.slots.len()).map(|_| None).collect();
        Self {
            port,
            resources,
            cursor: 0,
        }
    }
    pub fn register_driver(&self, waker: &Waker) {
        self.port.shared.engine.register(waker);
    }
    fn activate(&mut self, config: Configuration) -> Result<Vec<RailMapping>> {
        if self.port.shared.closed.load(Ordering::Acquire) {
            return Err(Error::Unavailable);
        }
        if config.publication.is_empty() {
            return Ok(Vec::new());
        }
        let discovered = backend::Verbs.discover()?;
        let descriptions = discovered
            .iter()
            .map(super::device::discovered_port)
            .collect::<Result<Vec<_>>>()?;
        let selected = match_publication(&config.publication, &config.associations, &descriptions)?;
        if selected.is_empty() {
            return Ok(Vec::new());
        }
        if selected.len() > self.resources.len() {
            return Err(Error::Overloaded);
        }
        let devices: Vec<_> = discovered.into_iter().map(Rc::new).collect();
        // Build into temporary owners so partial startup cannot publish readiness.
        let mut provisioned = Vec::new();
        for (i, quota) in config.quotas.into_iter().enumerate() {
            let (rail, index) = &selected[i % selected.len()];
            let device = devices[*index].clone();
            let quota = Arc::new(quota);
            let region =
                backend::Region::new(device.clone(), config.bytes, Box::new(quota.clone()))?;
            let qp = backend::QueuePairHandle::new(device.clone())?;
            qp.probe_window()?;
            let mut bytes = Vec::new();
            bytes
                .try_reserve_exact(config.bytes)
                .map_err(|_| Error::Overloaded)?;
            bytes.resize(config.bytes, 0);
            provisioned.push((
                rail.clone(),
                Resource {
                    device,
                    region,
                    qp: Some(qp),
                    window: None,
                    pending: None,
                    stopping: false,
                    next_retry: None,
                },
                bytes,
                quota,
            ));
        }
        let ready = selected
            .iter()
            .map(|(mapping, _)| mapping.clone())
            .collect();
        for (i, (rail, resource, bytes, quota)) in provisioned.into_iter().enumerate() {
            let slot = &self.port.shared.slots[i];
            let mut mailbox = slot.mailbox.lock().map_err(|_| Error::Io)?;
            mailbox.rail = rail.rail;
            mailbox.endpoint = Some(resource.qp.as_ref().unwrap().endpoint);
            mailbox.bytes = bytes;
            mailbox.quota = Some(quota);
            self.resources[i] = Some(resource);
            slot.state.store(READY, Ordering::Release);
        }
        Ok(ready)
    }
    /// Run at most budget slots. Each slot may execute one native syscall/job.
    /// Calls may block this crypto role, but cannot block the paired I/O reactor.
    pub fn poll_budgeted(&mut self, budget: usize) -> Result<()> {
        if budget == 0 {
            return Ok(());
        }
        let config = self
            .port
            .shared
            .config
            .lock()
            .map_err(|_| Error::Io)?
            .take();
        if let Some(config) = config {
            let result = self.activate(config);
            *self.port.shared.activation.lock().map_err(|_| Error::Io)? = Some(result);
            self.port.shared.io.wake();
        }
        for _ in 0..budget.min(self.resources.len()) {
            let index = self.cursor;
            self.cursor = (self.cursor + 1) % self.resources.len();
            self.drive(index);
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
                .is_some_and(|at| std::time::Instant::now() < at)
            {
                return;
            }
            if let Some(qp) = &resource.qp {
                if qp.stop().is_err() {
                    resource.next_retry =
                        Some(std::time::Instant::now() + std::time::Duration::from_millis(10));
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
                    mailbox.bytes = Vec::new();
                    mailbox.quota = None;
                }
                slot.state.store(RETIRED, Ordering::Release);
                return;
            }
            if !slot.released.load(Ordering::Acquire) {
                return;
            }
            // Replenish the QP outside request turns. MR stays registered for the
            // entire bounded pool lifetime. Never reuse a QP/remote capability.
            match backend::QueuePairHandle::new(resource.device.clone()) {
                Ok(qp) => {
                    mailbox.endpoint = Some(qp.endpoint);
                    resource.qp = Some(qp);
                }
                Err(_) => {
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
        self.resources.iter().all(Option::is_none)
    }
}
impl Drop for NativeService {
    fn drop(&mut self) {
        self.close();
        // Drop backend QPs before their region owners. On failure the backend
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
                self.native.poll_budgeted(32)?;
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
#[path = "receive_tests.rs"]
mod receive_tests;

#[cfg(test)]
mod tests {
    use super::*;
    use crate::rdma::{
        device::Devices,
        session::Sessions,
        verbs::{DeviceHandle, QueuePairHandle, Region, Verbs},
    };
    use std::task::Context;

    pub(super) fn provision_test(
        service: &mut NativeService,
        index: usize,
    ) -> Rc<std::cell::Cell<usize>> {
        let (qp, region, charged) = backend::lifetime_tests::fresh_fixture();
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
        backend::lifetime_tests::complete(1, 0, 5);
        native.poll_budgeted(2).unwrap();
        assert_eq!(binding.result(), Some(Ok(())));
        let source = Region::acquire(&healthy, 16).unwrap();
        source.copy_from(&[7; 16]).unwrap();
        let written = healthy.write(source, 4096, 17).unwrap();
        native.poll_budgeted(2).unwrap();
        failed.expire_at(std::time::Instant::now());
        let sessions = Sessions::new(Rc::new(Devices::new(Rc::new(Verbs))), 2);
        sessions.track_test(failed.clone());
        sessions.track_test(healthy.clone());
        backend::lifetime_tests::fail_stop(true);
        assert!(
            sessions.progress().is_ok(),
            "attempt timeout must not fail app's worker poll"
        );
        assert_eq!(failed.progress(), Err(Error::DeadlineExceeded));
        assert!(!failed.stopped());
        assert!(healthy.ready());
        backend::lifetime_tests::complete(1, 0, 1);
        native.poll_budgeted(2).unwrap();
        assert!(sessions.progress().is_ok());
        assert_eq!(written.result(), Some(Ok(())));
        assert_eq!(failed_charge.get(), 1);
        assert_eq!(healthy_charge.get(), 1);
        assert_eq!(receive.copy_to(), Err(Error::Unavailable));
        backend::lifetime_tests::fail_stop(false);
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
        let sessions = Sessions::new(Rc::new(Devices::new(Rc::new(Verbs))), 2);
        sessions.track_test(first.clone());
        let mut cut = sessions.fence_cut();
        let second = claim(&io);
        mark_connected(&second, &mut native);
        sessions.track_test(second.clone());
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(cut.as_mut().poll(&mut cx).is_pending());
        assert!(!first.ready());
        assert!(second.ready());
        native.poll_budgeted(2).unwrap();
        assert!(matches!(
            cut.as_mut().poll(&mut cx),
            std::task::Poll::Ready(Ok(()))
        ));
        assert!(first.stopped());
        assert!(second.ready());
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
        backend::lifetime_tests::complete(1, 10, u32::MAX);
        native.poll_budgeted(2).unwrap();
        let sessions = Sessions::new(Rc::new(Devices::new(Rc::new(Verbs))), 2);
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
    #[test]
    fn accepted_cut_fences_snapshot_without_closing_future_sessions() {
        let (io, port) = pair(2).unwrap();
        let io = Rc::new(io);
        let mut native = NativeService::new(port);
        provision_test(&mut native, 0);
        provision_test(&mut native, 1);
        let first = claim(&io);
        mark_connected(&first, &mut native);
        let sessions = Sessions::new(Rc::new(Devices::new(Rc::new(Verbs))), 2);
        sessions.track_test(first.clone());
        let mut cut = sessions.fence_cut();
        let later = claim(&io);
        mark_connected(&later, &mut native);
        sessions.track_test(later.clone());
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(cut.as_mut().poll(&mut cx).is_pending());
        assert!(later.ready());
        assert!(!first.stopped());
        native.poll_budgeted(2).unwrap();
        assert!(matches!(
            cut.as_mut().poll(&mut cx),
            std::task::Poll::Ready(Ok(()))
        ));
        assert!(later.ready());
        assert!(!io.shared.closed.load(Ordering::Acquire));
        assert!(sessions.progress().is_ok());
    }
    #[cfg(feature = "rdma")]
    #[test]
    #[ignore = "requires built native ABI v2 adapter and zero usable type-2B ports"]
    fn native_no_device_activation_runs_on_paired_role_and_releases_quota() {
        use crate::{
            model::identity::RequestId,
            model::limits::ResourceClass,
            runtime::{admission::Admission, deadline::RequestScope},
        };
        assert!(
            backend::Verbs
                .discover()
                .expect("real adapter must load")
                .is_empty()
        );
        let (io, port) = pair(1).unwrap();
        let devices = Devices::new(Rc::new(Verbs));
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
            model::identity::RequestId,
            model::limits::ResourceClass,
            runtime::{admission::Admission, deadline::RequestScope},
        };
        let (io, port) = pair(1).unwrap();
        let devices = Devices::new(Rc::new(Verbs));
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
            model::identity::RequestId,
            runtime::{admission::Admission, deadline::RequestScope},
        };
        let name = std::env::var("RACER_RDMA_TEST_DEVICE")
            .expect("select native test provider explicitly");
        let ports = backend::Verbs.discover().expect("real native adapter");
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
        let devices = Devices::new(Rc::new(Verbs));
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
            ticket: &crate::rdma::verbs::Ticket,
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
