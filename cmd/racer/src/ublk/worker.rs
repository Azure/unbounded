//! The worker side of ublk: attach this worker's queue of every device and
//! serve it on the worker's own reactor.

use std::ops::Range;
use std::os::fd::{AsFd, OwnedFd};
use std::rc::Rc;
use std::sync::Arc;
use std::task::Context;
use std::time::Duration;

use uring_runtime::reactor::descriptor::Descriptor;
use uring_runtime::reactor::{SendBuffer, SubmissionCapacity};

use super::manager::{Attach, Manager, QueueReport, Resolved};
use super::queue::IoDescriptors;
use super::service::{
    Local, PageBytes, PageSource, QueueKernel, QueueService, QueueShape, QueueState,
};
use super::uapi::IoDesc;
use super::window::PageWindow;
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

// SAFETY: the `unsafe trait PageBytes` contract guarantees owners keep their
// bytes at a fixed address across moves and never mutate them while any clone
// is alive, and this value retains its owner
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

/// Longest pause between retries of a refused queue command.
const MAX_PAUSE: Duration = Duration::from_millis(100);

/// One queue of a real device, served through the worker's reactor.
pub(crate) struct ReactorKernel {
    reactor: Rc<Reactor>,
    fd: Rc<Descriptor>,
    descriptors: IoDescriptors,
    scope: RequestScope,
    /// Slots kept for queue commands, one per tag of every device.
    commands: Option<Rc<SubmissionCapacity>>,
}

impl ReactorKernel {
    /// Map queue `q`'s descriptors through this worker's duplicate of the
    /// device's `/dev/ublkcN` descriptor.
    pub(crate) fn open(
        reactor: Rc<Reactor>,
        commands: Option<Rc<SubmissionCapacity>>,
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
            commands,
        })
    }
}

impl QueueKernel for ReactorKernel {
    fn command(&self, cmd_op: u32, cmd: [u8; 16]) -> Local<'_, Result<i32>> {
        // Queue commands are FETCH_REQ / COMMIT_AND_FETCH_REQ built by
        // `io_cmd_bytes` (src/ublk/queue.rs:157), whose only pointer field,
        // `ublksrv_io_cmd.addr` at bytes 8..16, is 0 because every device is
        // added with `UBLK_F_USER_COPY` (src/ublk/manager.rs:476). Data moves
        // through `pread`/`pwrite` on the char device instead.
        debug_assert_eq!(
            u64::from_ne_bytes(cmd[8..16].try_into().unwrap()),
            0,
            "ublk queue command must not carry a buffer address under USER_COPY"
        );
        match &self.commands {
            // SAFETY: the payload carries no user address (addr is 0, checked
            // above in debug builds), so the kernel accesses no caller memory
            // through it; `fd` is retained by the reactor until the fence.
            Some(commands) => unsafe {
                self.reactor.uring_cmd_reserved(
                    self.fd.clone(),
                    cmd_op,
                    cmd,
                    (),
                    commands.clone(),
                    &self.scope,
                )
            },
            // SAFETY: as above; the payload references no caller memory.
            None => unsafe {
                self.reactor
                    .uring_cmd(self.fd.clone(), cmd_op, cmd, (), &self.scope)
            },
        }
    }

    fn pause(&self, attempt: u32) -> Local<'_, ()> {
        Box::pin(async move {
            let wait = Duration::from_millis(1 << attempt.min(7)).min(MAX_PAUSE);
            let until = uring_runtime::environment::now() + wait;
            let slept = self
                .reactor
                .sleep_until(
                    until,
                    uring_runtime::reactor::timer::SleepMode::Kernel,
                    &self.scope,
                )
                .await;
            if slept.is_err() {
                // The timer was refused too. Yield once so other work runs.
                let mut yielded = false;
                std::future::poll_fn(|cx| {
                    if yielded {
                        return std::task::Poll::Ready(());
                    }
                    yielded = true;
                    cx.waker().wake_by_ref();
                    std::task::Poll::Pending
                })
                .await;
            }
        })
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
    commands: Option<Rc<SubmissionCapacity>>,
    devices: Vec<Device<B::Source>>,
    /// Plaintext pages kept across requests by every queue of this worker.
    window: Rc<PageWindow<<B::Source as PageSource>::Page>>,
}

impl<B: Backend> UblkWorker<B> {
    /// `commands` holds one submission slot per tag of every device. Without
    /// it, queue commands use ordinary slots.
    pub(crate) fn new(
        q: u16,
        manager: Arc<Manager>,
        backend: Rc<B>,
        reactor: Rc<Reactor>,
        commands: Option<Rc<SubmissionCapacity>>,
    ) -> Self {
        let devices = (0..manager.device_count())
            .map(|_| Device::default())
            .collect();
        Self {
            q,
            manager,
            backend,
            reactor,
            commands,
            devices,
            window: Rc::new(PageWindow::default()),
        }
    }

    /// Size the page window to the worker's plaintext quota; see
    /// [`PageWindow::for_quota`]. Call before any device attaches.
    pub(crate) fn with_window_quota(
        mut self,
        plaintext_bytes: usize,
        range_window_pages: usize,
    ) -> Self {
        self.window = Rc::new(PageWindow::for_quota(plaintext_bytes, range_window_pages));
        self
    }

    /// Nothing here can still hold up a device. Either no queue is
    /// attached, or the control thread has finished and no queue holds a
    /// request the kernel waits on. A queue left in the second case only
    /// has FETCH commands outstanding. The control thread already stopped
    /// its device, or stops it again on finishing, or exits the process
    /// (see `Manager::settle`), so the runtime fence cannot wait on them
    /// forever.
    pub(crate) fn is_idle(&self) -> bool {
        let control_done = self.manager.control_done();
        self.devices.iter().all(|device| {
            device.resolving.is_none()
                && device.queue.as_ref().is_none_or(|queue| {
                    control_done && queue.owned() == 0 && queue.held_pages() == 0
                })
        })
    }

    /// Stop serving reads now. Every queue answers the requests it holds,
    /// and any it is handed later, with `EIO`. The queues are not dropped:
    /// dropping a pending ublk command does not cancel it, and a dropped
    /// request would block STOP_DEV. They end when the kernel aborts them.
    pub(crate) fn abandon(&mut self) {
        for device in &mut self.devices {
            device.resolving = None;
            device.attach_done = true;
            if let Some(queue) = &mut device.queue {
                queue.abandon();
            }
        }
    }

    /// Advance resolution, attachment, and every attached queue.
    pub(crate) fn poll(&mut self, cx: &mut Context<'_>) {
        let views = self.manager.view(self.q, cx.waker());
        // Once the control thread is done, no read is coming that a kept
        // page could serve, and kept pages must not hold up the fence.
        let control_done = self.manager.control_done();
        let (reactor, commands, q) = (&self.reactor, &self.commands, self.q);
        let open = |fd, depth| ReactorKernel::open(reactor.clone(), commands.clone(), fd, q, depth);
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
                device.attach_done = true;
                if let Some(queue) = &mut device.queue {
                    queue.abandon();
                }
            }
            if let Some(attach) = view.attach
                && !device.attach_done
            {
                Self::attach(
                    &self.manager,
                    &*self.backend,
                    &open,
                    &self.window,
                    self.q,
                    index,
                    device,
                    attach,
                );
            }
            if let Some(queue) = &mut device.queue {
                if control_done {
                    queue.release_pages();
                }
                match queue.poll(cx) {
                    QueueState::Running => {
                        if queue.failure().is_some() && !device.failure_reported {
                            device.failure_reported = true;
                            self.manager.report_failure(index, self.q);
                        }
                    }
                    QueueState::Finished { failure } => {
                        device.queue = None;
                        // Tags end without a failure only when the kernel
                        // aborts them. If this process did not stop the
                        // device, someone else did.
                        let external = failure.is_none() && !self.manager.stopping(index);
                        if external {
                            eprintln!(
                                "racer: component=ublk device={index} queue={} stage=serve state=stopped_externally",
                                self.q
                            );
                        }
                        if (failure.is_some() || external) && !device.failure_reported {
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

    #[allow(clippy::too_many_arguments)]
    fn attach(
        manager: &Manager,
        backend: &B,
        open: &dyn Fn(OwnedFd, u16) -> std::io::Result<ReactorKernel>,
        window: &Rc<PageWindow<<B::Source as PageSource>::Page>>,
        q: u16,
        index: usize,
        device: &mut Device<B::Source>,
        attach: Attach,
    ) {
        let config = manager.device_config(index);
        device.attach_done = true;
        let kernel = match manager.char_fd(index).and_then(|fd| open(fd, attach.depth)) {
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
        device.queue = Some(QueueService::with_window(
            Rc::new(kernel),
            Rc::new(source),
            shape,
            window.clone(),
        ));
        manager.report_queue(index, q, QueueReport::Attached);
    }
}
