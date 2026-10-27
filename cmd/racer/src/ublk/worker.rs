//! The worker side of ublk: attach this worker's queue of every device and
//! serve it on the worker's own reactor.

use std::ops::Range;
use std::os::fd::{AsFd, OwnedFd};
use std::rc::Rc;
use std::sync::Arc;
use std::task::Context;
use std::time::Duration;

use uring_runtime::reactor::SendBuffer;
use uring_runtime::reactor::descriptor::Descriptor;

use super::manager::{Attach, Manager, QueueReport, Resolved};
use super::queue::IoDescriptors;
use super::service::{
    Local, PageBytes, PageSource, QueueKernel, QueueService, QueueShape, QueueState,
};
use super::uapi::IoDesc;
use crate::config::UblkDeviceConfig;
use crate::error::{Error, Result};
use crate::model::PAGE_BYTES;
use crate::runtime::{Reactor, RequestScope};

/// Resolves device versions and builds page sources on one worker.
pub(crate) trait Backend: 'static {
    type Source: PageSource;

    /// Find the pinned version and length of a configured device.
    fn resolve(self: Rc<Self>, device: UblkDeviceConfig) -> Local<'static, Result<Resolved>>;

    /// Page source for the resolved version.
    fn source(&self, device: &UblkDeviceConfig, resolved: &Resolved) -> Result<Self::Source>;
}

/// A page range handed to the reactor as an immutable send buffer.
struct Slice<P: PageBytes> {
    page: P,
    range: Range<usize>,
}

// SAFETY: `PageBytes` owners keep their bytes at a fixed address and never
// mutate them while any clone is alive, and this value retains its owner
// until the reactor's completion fence.
unsafe impl<P: PageBytes> SendBuffer for Slice<P> {
    type Error = Error;

    fn send_bytes(&self) -> Result<&[u8]> {
        self.page
            .bytes()
            .get(self.range.clone())
            .ok_or(Error::InvalidRange)
    }
}

/// One queue of a real device, served through the worker's reactor.
pub(crate) struct ReactorKernel {
    reactor: Rc<Reactor>,
    fd: Rc<Descriptor>,
    descriptors: IoDescriptors,
    scope: RequestScope,
}

impl ReactorKernel {
    /// Map queue `q`'s descriptors through this worker's duplicate of the
    /// device's `/dev/ublkcN` descriptor.
    pub(crate) fn open(
        reactor: Rc<Reactor>,
        fd: OwnedFd,
        q: u16,
        depth: u16,
    ) -> std::io::Result<Self> {
        let descriptors = IoDescriptors::map(fd.as_fd(), q, depth)?;
        let scope = RequestScope::new(
            crate::model::RequestId(*b"racer-ublk-queue"),
            uring_runtime::environment::now() + Duration::from_secs(365 * 24 * 3600),
        )
        .map_err(|_| std::io::Error::other("ublk queue scope"))?;
        Ok(Self {
            reactor,
            fd: Rc::new(Descriptor::from(fd)),
            descriptors,
            scope,
        })
    }
}

impl QueueKernel for ReactorKernel {
    fn command(&self, cmd_op: u32, cmd: [u8; 16]) -> Local<'_, Result<i32>> {
        self.reactor
            .uring_cmd(self.fd.clone(), cmd_op, cmd, (), &self.scope)
    }

    fn descriptor(&self, tag: u16) -> IoDesc {
        self.descriptors.get(tag)
    }

    fn write<P: PageBytes>(
        &self,
        page: P,
        range: Range<usize>,
        pos: u64,
    ) -> Local<'_, Result<usize>> {
        Box::pin(async move {
            let done = self
                .reactor
                .write_shared_at(self.fd.clone(), pos, Slice { page, range }, (), &self.scope)
                .await?;
            Ok(done.bytes)
        })
    }
}

/// Per-device worker state.
struct Device<S: PageSource> {
    resolving: Option<Local<'static, Result<Resolved>>>,
    resolve_started: bool,
    /// Set once this worker tried to attach, so it never attaches twice.
    attach_done: bool,
    queue: Option<QueueService<ReactorKernel, S>>,
    failure_reported: bool,
}

impl<S: PageSource> Default for Device<S> {
    fn default() -> Self {
        Self {
            resolving: None,
            resolve_started: false,
            attach_done: false,
            queue: None,
            failure_reported: false,
        }
    }
}

/// This worker's queues of every configured device.
pub(crate) struct UblkWorker<B: Backend> {
    q: u16,
    manager: Arc<Manager>,
    backend: Rc<B>,
    reactor: Rc<Reactor>,
    devices: Vec<Device<B::Source>>,
}

impl<B: Backend> UblkWorker<B> {
    pub(crate) fn new(q: u16, manager: Arc<Manager>, backend: Rc<B>, reactor: Rc<Reactor>) -> Self {
        let devices = (0..manager.device_count())
            .map(|_| Device::default())
            .collect();
        Self {
            q,
            manager,
            backend,
            reactor,
            devices,
        }
    }

    /// No queue is attached and no resolution is running.
    pub(crate) fn is_idle(&self) -> bool {
        self.devices
            .iter()
            .all(|device| device.queue.is_none() && device.resolving.is_none())
    }

    /// Drop every queue and resolution now. Outstanding kernel commands are
    /// canceled by the reactor, which keeps their descriptors until the fence.
    pub(crate) fn abandon(&mut self) {
        for (index, device) in self.devices.iter_mut().enumerate() {
            device.resolving = None;
            if device.queue.take().is_some() {
                self.manager
                    .report_queue(index, self.q, QueueReport::Closed);
            }
        }
    }

    /// Advance resolution, attachment, and every attached queue.
    pub(crate) fn poll(&mut self, cx: &mut Context<'_>) {
        let views = self.manager.view(self.q, cx.waker());
        for (index, view) in views.into_iter().enumerate() {
            let device = &mut self.devices[index];
            if view.resolve && !device.resolve_started {
                device.resolve_started = true;
                device.resolving = Some(
                    self.backend
                        .clone()
                        .resolve(self.manager.device_config(index).clone()),
                );
            }
            if !view.resolve {
                device.resolving = None;
            }
            if let Some(resolving) = &mut device.resolving
                && let std::task::Poll::Ready(result) = resolving.as_mut().poll(cx)
            {
                device.resolving = None;
                self.manager.report_resolved(index, result);
            }
            if view.abandon {
                if device.queue.take().is_some() {
                    self.manager
                        .report_queue(index, self.q, QueueReport::Closed);
                }
                device.attach_done = true;
                continue;
            }
            if let Some(attach) = view.attach
                && !device.attach_done
            {
                Self::attach(
                    &self.manager,
                    &*self.backend,
                    &self.reactor,
                    self.q,
                    index,
                    device,
                    attach,
                );
            }
            if let Some(queue) = &mut device.queue {
                match queue.poll(cx) {
                    QueueState::Running => {
                        if queue.failure().is_some() && !device.failure_reported {
                            device.failure_reported = true;
                            self.manager.report_failure(index, self.q);
                        }
                    }
                    QueueState::Finished { failure } => {
                        device.queue = None;
                        if failure.is_some() && !device.failure_reported {
                            device.failure_reported = true;
                            self.manager.report_failure(index, self.q);
                        }
                        self.manager
                            .report_queue(index, self.q, QueueReport::Closed);
                    }
                }
            }
        }
    }

    fn attach(
        manager: &Manager,
        backend: &B,
        reactor: &Rc<Reactor>,
        q: u16,
        index: usize,
        device: &mut Device<B::Source>,
        attach: Attach,
    ) {
        let config = manager.device_config(index);
        device.attach_done = true;
        let kernel = match manager
            .char_fd(index)
            .and_then(|fd| ReactorKernel::open(reactor.clone(), fd, q, attach.depth))
        {
            Ok(kernel) => kernel,
            Err(error) => {
                eprintln!(
                    "racer: component=ublk device={index} queue={q} stage=open state=failed error=\"{error}\""
                );
                manager.report_failure(index, q);
                return;
            }
        };
        let source = match manager
            .resolved(index)
            .and_then(|r| backend.source(config, &r))
        {
            Ok(source) => source,
            Err(error) => {
                eprintln!(
                    "racer: component=ublk device={index} queue={q} stage=source state=failed error={error:?}"
                );
                manager.report_failure(index, q);
                return;
            }
        };
        let shape = QueueShape {
            q,
            depth: attach.depth,
            max_io_bytes: attach.max_io_bytes,
            length: attach.length,
            page_bytes: PAGE_BYTES,
        };
        device.queue = Some(QueueService::new(Rc::new(kernel), Rc::new(source), shape));
        manager.report_queue(index, q, QueueReport::Attached);
    }
}
