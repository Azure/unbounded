//! Node-level ublk device manager and its `ublk-control` thread.
//!
//! The control thread owns `/dev/ublk-control`. It adds, starts, stops, and
//! deletes devices. Workers serve the queues. The two sides share one small
//! state table under a mutex. Workers report progress there and the control
//! thread waits on a condition variable. The control thread wakes workers
//! through the task wakers they register on every poll.

use std::os::fd::OwnedFd;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Condvar, Mutex, MutexGuard};
use std::task::Waker;
use std::time::{Duration, Instant};

use super::control::{Control, DevSpec, UblkError};
use super::uapi::{DevInfo, Params, UBLK_F_CMD_IOCTL_ENCODE, UBLK_F_USER_COPY};
use crate::config::{UblkConfig, UblkDeviceConfig};
use crate::error::Error;
use crate::model::StrongEtag;
use racer_control_wire::CacheId;

/// How long the control thread sleeps between checks when idle.
const IDLE_WAIT: Duration = Duration::from_millis(200);

/// The pinned object version behind a device, found by a worker.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct Resolved {
    pub cache: CacheId,
    pub etag: StrongEtag,
    pub length: u64,
}

/// What a worker needs to attach its queue of a device.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct Attach {
    pub dev_id: u32,
    pub depth: u16,
    pub max_io_bytes: u32,
    pub length: u64,
}

/// One worker's queue of one device, as reported by the worker.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub(crate) enum QueueReport {
    /// Not attached yet.
    #[default]
    Idle,
    /// The character device is open and every tag loop has started.
    Attached,
    /// The queue is gone. The worker no longer touches the device.
    Closed,
}

#[derive(Default)]
struct DeviceShared {
    resolved: Option<Result<Resolved, Error>>,
    attach: Option<Attach>,
    /// The one open `/dev/ublkcN`. The kernel allows a single open, so each
    /// worker serves its queue through a duplicate of this descriptor.
    char_fd: Option<Arc<OwnedFd>>,
    /// Workers must stop serving reads and answer every request with EIO.
    abandon: bool,
    /// This process is stopping the device. A queue whose tags all end
    /// before this is set was stopped by someone else.
    stopping: bool,
    queues: Vec<QueueReport>,
    failed: Vec<bool>,
}

#[derive(Default)]
struct Shared {
    stop: bool,
    control_done: bool,
    wakers: Vec<Option<Waker>>,
    devices: Vec<DeviceShared>,
}

/// A worker's view of one device after a poll.
#[derive(Clone, Debug)]
pub(crate) struct DeviceView {
    pub resolve: bool,
    pub attach: Option<Attach>,
    pub abandon: bool,
}

/// Opens a fresh control channel for [`Manager::settle`].
pub(super) type Reopen<'a> = dyn FnMut() -> Result<Box<dyn DeviceControl>, UblkError> + 'a;

/// The blocking control operations, as a trait so tests can fake the kernel.
pub(crate) trait DeviceControl {
    fn add_dev(&mut self, spec: &DevSpec) -> Result<DevInfo, UblkError>;
    fn set_params(&mut self, id: u32, params: &Params) -> Result<(), UblkError>;
    fn start_dev(
        &mut self,
        id: u32,
        pid: i32,
        should_stop: &dyn Fn() -> bool,
    ) -> Result<(), UblkError>;
    fn stop_dev(&mut self, id: u32) -> Result<(), UblkError>;
    fn get_dev_info(&mut self, id: u32) -> Result<DevInfo, UblkError>;
    /// Delete a device whose character device the caller holds open.
    fn del_dev(&mut self, id: u32) -> Result<(), UblkError>;
    /// Whether a command's outcome is unknown, so no more are sent.
    fn is_poisoned(&self) -> bool;

    /// Open `<directory>/ublkcN` read-write.
    fn open_char(&mut self, directory: &Path, id: u32) -> std::io::Result<OwnedFd> {
        open_char(directory, id)
    }
}

/// Shortest bound on one control command.
const MIN_COMMAND_TIMEOUT: Duration = Duration::from_secs(1);

/// How much longer than the drain budget a control command may wait.
const STOP_MARGIN: Duration = Duration::from_secs(5);

/// The bound on each control command for a given drain budget.
///
/// STOP_DEV waits until every request the kernel handed out is answered.
/// A stuck read is answered with EIO only when its worker's drain deadline,
/// `shutdown_timeout` after that worker started draining, passes. STOP_DEV
/// is sent when the first worker starts draining, so it must outlast that
/// deadline, with room for workers that start draining a little later.
/// Otherwise it times out just as the reads are answered, the channel is
/// poisoned, and the devices are left undeleted.
fn command_timeout(shutdown_timeout: Duration) -> Duration {
    shutdown_timeout
        .saturating_add(STOP_MARGIN)
        .max(MIN_COMMAND_TIMEOUT)
}

/// How long to retry opening `/dev/ublkcN` after ADD_DEV. The node is
/// normally there at once, but a host `/dev` mount can lag.
const OPEN_RETRY: Duration = Duration::from_secs(5);

fn open_char(directory: &Path, id: u32) -> std::io::Result<OwnedFd> {
    let path = directory.join(format!("ublkc{id}"));
    let deadline = Instant::now() + OPEN_RETRY;
    loop {
        match std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .open(&path)
        {
            Ok(file) => return Ok(file.into()),
            Err(error)
                if error.kind() == std::io::ErrorKind::NotFound && Instant::now() < deadline =>
            {
                std::thread::sleep(Duration::from_millis(10));
            }
            Err(error) => return Err(error),
        }
    }
}

impl DeviceControl for Control {
    fn add_dev(&mut self, spec: &DevSpec) -> Result<DevInfo, UblkError> {
        Control::add_dev(self, spec)
    }

    fn set_params(&mut self, id: u32, params: &Params) -> Result<(), UblkError> {
        Control::set_params(self, id, params)
    }

    fn start_dev(
        &mut self,
        id: u32,
        pid: i32,
        should_stop: &dyn Fn() -> bool,
    ) -> Result<(), UblkError> {
        Control::start_dev(self, id, pid, should_stop)
    }

    fn stop_dev(&mut self, id: u32) -> Result<(), UblkError> {
        Control::stop_dev(self, id)
    }

    fn get_dev_info(&mut self, id: u32) -> Result<DevInfo, UblkError> {
        Control::get_dev_info(self, id)
    }

    fn del_dev(&mut self, id: u32) -> Result<(), UblkError> {
        Control::del_dev(self, id)
    }

    fn is_poisoned(&self) -> bool {
        Control::is_poisoned(self)
    }
}

/// A control channel that replaces itself once poisoned.
///
/// A poisoned channel may still have a command in the kernel, so it is
/// leaked, not closed, and the next command goes over a fresh channel from
/// `reopen`. This keeps one stuck command, such as a START_DEV that timed
/// out, from failing every later device. If reopening fails, the poisoned
/// channel stays and commands fail with [`UblkError::Poisoned`].
pub(super) struct Reopening<'a> {
    current: Box<dyn DeviceControl + 'a>,
    reopen: Box<Reopen<'a>>,
}

impl<'a> Reopening<'a> {
    pub(super) fn new(current: Box<dyn DeviceControl + 'a>, reopen: Box<Reopen<'a>>) -> Self {
        Self { current, reopen }
    }

    /// The channel to use, replacing a poisoned one if possible.
    fn channel(&mut self) -> &mut dyn DeviceControl {
        if self.current.is_poisoned() {
            match (self.reopen)() {
                Ok(fresh) => {
                    log(None, "control", "state=reopened reason=poisoned");
                    std::mem::forget(std::mem::replace(&mut self.current, fresh));
                }
                Err(error) => log(
                    None,
                    "control",
                    &format!("state=reopen_failed error=\"{error}\""),
                ),
            }
        }
        &mut *self.current
    }
}

impl DeviceControl for Reopening<'_> {
    fn add_dev(&mut self, spec: &DevSpec) -> Result<DevInfo, UblkError> {
        self.channel().add_dev(spec)
    }

    fn set_params(&mut self, id: u32, params: &Params) -> Result<(), UblkError> {
        self.channel().set_params(id, params)
    }

    fn start_dev(
        &mut self,
        id: u32,
        pid: i32,
        should_stop: &dyn Fn() -> bool,
    ) -> Result<(), UblkError> {
        self.channel().start_dev(id, pid, should_stop)
    }

    fn stop_dev(&mut self, id: u32) -> Result<(), UblkError> {
        self.channel().stop_dev(id)
    }

    fn get_dev_info(&mut self, id: u32) -> Result<DevInfo, UblkError> {
        self.channel().get_dev_info(id)
    }

    fn del_dev(&mut self, id: u32) -> Result<(), UblkError> {
        self.channel().del_dev(id)
    }

    /// Whether the current channel is poisoned. It is replaced only when
    /// the next command is sent, so a caller can still see that the last
    /// command's outcome is unknown.
    fn is_poisoned(&self) -> bool {
        self.current.is_poisoned()
    }

    fn open_char(&mut self, directory: &Path, id: u32) -> std::io::Result<OwnedFd> {
        self.current.open_char(directory, id)
    }
}

/// Where a device is in the control thread.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Phase {
    Waiting,
    Started(u32),
    Failed,
}

/// Shared ublk state for the whole node.
pub(crate) struct Manager {
    devices: Vec<UblkDeviceConfig>,
    queue_depth: u16,
    max_io_bytes: u32,
    queues: u16,
    device_directory: PathBuf,
    /// Bound on each shutdown wait.
    shutdown_timeout: Duration,
    /// Bound on waiting for every worker to attach a queue.
    attach_timeout: Duration,
    shared: Mutex<Shared>,
    changed: Condvar,
}

impl Manager {
    /// Shared state for `queues` workers. Does not touch any device.
    pub(crate) fn new(
        config: &UblkConfig,
        queues: u16,
        device_directory: &Path,
        shutdown_timeout: Duration,
        attach_timeout: Duration,
    ) -> Self {
        let devices = config
            .devices
            .iter()
            .map(|_| DeviceShared {
                queues: vec![QueueReport::Idle; usize::from(queues)],
                failed: vec![false; usize::from(queues)],
                ..Default::default()
            })
            .collect();
        Self {
            devices: config.devices.clone(),
            queue_depth: config.queue_depth,
            max_io_bytes: config.max_io_bytes,
            queues,
            device_directory: device_directory.to_path_buf(),
            shutdown_timeout,
            attach_timeout,
            shared: Mutex::new(Shared {
                wakers: vec![None; usize::from(queues)],
                devices,
                ..Default::default()
            }),
            changed: Condvar::new(),
        }
    }

    fn lock(&self) -> MutexGuard<'_, Shared> {
        self.shared.lock().unwrap_or_else(|e| e.into_inner())
    }

    pub(crate) fn device_config(&self, index: usize) -> &UblkDeviceConfig {
        &self.devices[index]
    }

    pub(crate) fn device_count(&self) -> usize {
        self.devices.len()
    }

    pub(crate) fn queues(&self) -> u16 {
        self.queues
    }

    /// Ask the control thread to stop and remove every device.
    pub(crate) fn request_stop(&self) {
        let mut shared = self.lock();
        if !shared.stop {
            shared.stop = true;
            self.changed.notify_all();
        }
    }

    /// Record the waker of worker `q` and return its view of every device.
    pub(crate) fn view(&self, q: u16, waker: &Waker) -> Vec<DeviceView> {
        let mut shared = self.lock();
        let slot = &mut shared.wakers[usize::from(q)];
        if !slot.as_ref().is_some_and(|w| w.will_wake(waker)) {
            *slot = Some(waker.clone());
        }
        let stop = shared.stop;
        shared
            .devices
            .iter()
            .map(|device| DeviceView {
                resolve: q == 0 && !stop && device.resolved.is_none(),
                attach: device.attach,
                abandon: device.abandon || shared.control_done,
            })
            .collect()
    }

    /// The kernel id of a device once ADD_DEV succeeded.
    #[cfg(test)]
    pub(crate) fn dev_id(&self, index: usize) -> Option<u32> {
        self.lock().devices[index]
            .attach
            .map(|attach| attach.dev_id)
    }

    /// Whether the control thread has finished its loop. Every device it
    /// started has been stopped by then, or is being stopped by
    /// [`Manager::settle`], so no kernel request still waits for a read.
    pub(crate) fn control_done(&self) -> bool {
        self.lock().control_done
    }

    /// A duplicate of a device's character descriptor for one worker.
    pub(crate) fn char_fd(&self, index: usize) -> std::io::Result<OwnedFd> {
        let fd = self.lock().devices[index].char_fd.clone();
        fd.ok_or_else(|| std::io::Error::from_raw_os_error(libc::ENODEV))?
            .try_clone()
    }

    /// Drop the manager's own character descriptor. Workers keep theirs.
    fn release_char(&self, index: usize) {
        self.lock().devices[index].char_fd = None;
    }

    /// Keep the manager's character descriptor open until the process
    /// exits. A delete whose outcome is unknown may still be running, and
    /// closing the device could let its id be freed and reused under it.
    fn leak_char(&self, index: usize) {
        if let Some(fd) = self.lock().devices[index].char_fd.take() {
            std::mem::forget(fd);
        }
    }

    /// Whether this process has started stopping the device.
    pub(crate) fn stopping(&self, index: usize) -> bool {
        self.lock().devices[index].stopping
    }

    /// The resolved version of a device, once a worker found it.
    pub(crate) fn resolved(&self, index: usize) -> Result<Resolved, Error> {
        self.lock().devices[index]
            .resolved
            .clone()
            .unwrap_or(Err(Error::Unavailable))
    }

    pub(crate) fn report_resolved(&self, index: usize, result: Result<Resolved, Error>) {
        let mut shared = self.lock();
        shared.devices[index].resolved = Some(result);
        self.changed.notify_all();
    }

    pub(crate) fn report_queue(&self, index: usize, q: u16, report: QueueReport) {
        let mut shared = self.lock();
        shared.devices[index].queues[usize::from(q)] = report;
        self.changed.notify_all();
    }

    /// Mark queue `q` of a device attached, unless this process has started
    /// stopping the device or the control thread is done. Returns whether
    /// the queue may send its FETCH commands.
    ///
    /// The check and the mark happen under one lock. A queue marked here is
    /// seen by teardown's wait and by [`Manager::settle`], which mark the
    /// device stopping, or the control thread done, before they look. A
    /// queue refused here sends nothing the kernel could keep.
    pub(crate) fn claim_queue(&self, index: usize, q: u16) -> bool {
        let mut shared = self.lock();
        if shared.control_done || shared.devices[index].stopping {
            return false;
        }
        shared.devices[index].queues[usize::from(q)] = QueueReport::Attached;
        self.changed.notify_all();
        true
    }

    pub(crate) fn report_failure(&self, index: usize, q: u16) {
        let mut shared = self.lock();
        shared.devices[index].failed[usize::from(q)] = true;
        self.changed.notify_all();
    }

    fn wake_workers(shared: &Shared) {
        for waker in shared.wakers.iter().flatten() {
            waker.wake_by_ref();
        }
    }

    /// Change shared state, then wake workers.
    fn update(&self, change: impl FnOnce(&mut Shared)) {
        let mut shared = self.lock();
        change(&mut shared);
        Self::wake_workers(&shared);
        self.changed.notify_all();
    }

    /// Wait until `done` holds or `deadline` passes. Returns whether it held.
    fn wait_until(&self, deadline: Instant, done: impl Fn(&Shared) -> bool) -> bool {
        let mut shared = self.lock();
        loop {
            if done(&shared) {
                return true;
            }
            let now = Instant::now();
            if now >= deadline {
                return false;
            }
            let wait = (deadline - now).min(IDLE_WAIT);
            shared = self
                .changed
                .wait_timeout(shared, wait)
                .unwrap_or_else(|e| e.into_inner())
                .0;
        }
    }

    /// Open `/dev/ublk-control` and run the control loop until stop.
    ///
    /// Whatever way the loop ends, including a panic, no worker queue may be
    /// left with FETCH commands only the kernel can complete: they cannot be
    /// canceled, and the worker's runtime fence waits for them. See
    /// [`Manager::settle`]. If that fails, the process exits, which makes
    /// the kernel abort them.
    pub(crate) fn run(&self) {
        let directory = self.device_directory.clone();
        let timeout = command_timeout(self.shutdown_timeout);
        let settled = self.run_guarded(
            |manager| {
                let open = move |directory: &Path| -> Result<Box<dyn DeviceControl>, UblkError> {
                    let mut control = Control::open(directory)?;
                    control.set_timeout(timeout);
                    Ok(Box::new(control))
                };
                match open(&manager.device_directory) {
                    Ok(control) => {
                        let directory = manager.device_directory.clone();
                        let mut control =
                            Reopening::new(control, Box::new(move || open(&directory)));
                        manager.run_with(&mut control);
                    }
                    Err(error) => log(None, "open", &format!("error=\"{error}\"")),
                }
            },
            &mut || {
                let mut control = Control::open(&directory)?;
                control.set_timeout(timeout);
                Ok(Box::new(control))
            },
        );
        if !settled {
            log(
                None,
                "settle",
                "state=failed action=exit reason=\"queues hold kernel commands\"",
            );
            std::process::exit(1);
        }
    }

    /// Run `body`, even if it panics, then [`settle`](Manager::settle) with
    /// `reopen` and mark the control thread done.
    pub(super) fn run_guarded(&self, body: impl FnOnce(&Self), reopen: &mut Reopen<'_>) -> bool {
        let outcome = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| body(self)));
        if outcome.is_err() {
            log(None, "control", "state=panicked");
        }
        // Workers keep serving until settle returns: STOP_DEV waits for every
        // request the kernel hands out, and a worker that saw the control
        // thread done could finish its drain and stop answering them.
        // Marking every device stopping refuses late queue claims meanwhile.
        self.update(|shared| {
            for device in &mut shared.devices {
                device.stopping = true;
            }
        });
        let settled = self.settle(reopen);
        self.finish();
        settled
    }

    /// Make sure no attached queue still waits on the kernel once the
    /// control thread is done. Returns false when that cannot be ensured.
    ///
    /// A queue still attached here means teardown did not finish: it timed
    /// out, its channel broke, or the loop panicked. Its FETCH commands end
    /// only when the device is stopped or the process exits. This sends
    /// STOP_DEV for each such device over a fresh `/dev/ublk-control`
    /// descriptor, bounded by the command timeout. STOP_DEV on an already
    /// stopped device succeeds, and ENODEV means the device is gone; both
    /// leave no FETCH outstanding. Devices are not deleted here.
    fn settle(&self, reopen: &mut Reopen<'_>) -> bool {
        let stuck: Vec<(usize, u32)> = {
            let shared = self.lock();
            shared
                .devices
                .iter()
                .enumerate()
                .filter_map(|(index, device)| {
                    let attached = device.queues.contains(&QueueReport::Attached);
                    device
                        .attach
                        .filter(|_| attached)
                        .map(|attach| (index, attach.dev_id))
                })
                .collect()
        };
        if stuck.is_empty() {
            return true;
        }
        self.update(|shared| {
            for &(index, _) in &stuck {
                shared.devices[index].stopping = true;
            }
        });
        let mut control = match reopen() {
            Ok(control) => control,
            Err(error) => {
                log(None, "settle", &format!("state=failed error=\"{error}\""));
                return false;
            }
        };
        let mut settled = true;
        for (index, id) in stuck {
            match control.stop_dev(id) {
                Ok(()) => log(Some(index), "settle", &format!("state=stopped id={id}")),
                Err(error) if error.errno() == Some(libc::ENODEV) => {
                    log(Some(index), "settle", &format!("state=gone id={id}"))
                }
                Err(error) => {
                    log(
                        Some(index),
                        "settle",
                        &format!("state=failed id={id} error=\"{error}\""),
                    );
                    settled = false;
                }
            }
        }
        settled
    }

    fn finish(&self) {
        self.update(|shared| shared.control_done = true);
    }

    /// The control loop over any [`DeviceControl`].
    pub(crate) fn run_with(&self, control: &mut dyn DeviceControl) {
        let mut phases = vec![Phase::Waiting; self.devices.len()];
        loop {
            let ready = {
                let mut shared = self.lock();
                if shared.stop {
                    break;
                }
                let ready = next_ready(
                    &phases,
                    &self.devices,
                    |i| shared.devices[i].resolved.is_some(),
                    |i| shared.devices[i].failed.iter().any(|f| *f),
                );
                if ready.is_none() {
                    shared = self
                        .changed
                        .wait_timeout(shared, IDLE_WAIT)
                        .unwrap_or_else(|e| e.into_inner())
                        .0;
                    drop(shared);
                }
                ready
            };
            let Some(index) = ready else {
                continue;
            };
            phases[index] = match phases[index] {
                Phase::Waiting => {
                    let owned = owned_ids(&phases);
                    self.bring_up(control, index, &owned)
                }
                Phase::Started(id) => {
                    log(Some(index), "serve", &format!("state=failed id={id}"));
                    self.teardown(control, &[(index, id)]);
                    Phase::Failed
                }
                phase => phase,
            };
        }
        let started: Vec<_> = phases
            .iter()
            .enumerate()
            .filter_map(|(i, phase)| match phase {
                Phase::Started(id) => Some((i, *id)),
                _ => None,
            })
            .collect();
        self.teardown(control, &started);
        self.finish();
    }

    /// Add, attach, and start one device. Failures tear it down.
    ///
    /// `owned` lists the ids of devices this manager holds; a fixed id in it
    /// is never treated as stale.
    fn bring_up(&self, control: &mut dyn DeviceControl, index: usize, owned: &[u32]) -> Phase {
        let resolved = self.lock().devices[index].resolved.clone();
        let resolved = match resolved {
            Some(Ok(resolved)) => resolved,
            Some(Err(error)) => {
                log(
                    Some(index),
                    "resolve",
                    &format!("state=failed error={error:?}"),
                );
                return Phase::Failed;
            }
            None => return Phase::Waiting,
        };
        if resolved.length == 0 {
            log(Some(index), "resolve", "state=failed error=EmptyObject");
            return Phase::Failed;
        }
        let device = &self.devices[index];
        if let Some(id) = device.id {
            if owned.contains(&id) {
                // Unreachable with fixed ids added first, but never stop or
                // delete a device this manager serves.
                log(
                    Some(index),
                    "replace",
                    &format!(
                        "state=failed id={id} error=\"id is held by another device of this dataplane\""
                    ),
                );
                return Phase::Failed;
            }
            if !self.replace_stale(control, index, id) {
                return Phase::Failed;
            }
        }
        let spec = DevSpec {
            id: device.id,
            queues: self.queues,
            depth: self.queue_depth,
            max_io_bytes: self.max_io_bytes,
            flags: UBLK_F_CMD_IOCTL_ENCODE | UBLK_F_USER_COPY,
            cookie: cookie(index),
        };
        let info = match control.add_dev(&spec) {
            Ok(info) => info,
            Err(error) => {
                log(
                    Some(index),
                    "add",
                    &format!("state=failed error=\"{error}\""),
                );
                return Phase::Failed;
            }
        };
        let id = info.dev_id;
        // Hold the character device from here on: while it is open the
        // kernel cannot free the id, so a later command by id can only reach
        // this device.
        let fd = match control.open_char(&self.device_directory, id) {
            Ok(fd) => fd,
            Err(error) => {
                // Nothing pins the id, so it may already belong to another
                // device. Send nothing by id and leave it for an operator.
                log(
                    Some(index),
                    "open",
                    &format!(
                        "state=failed id={id} error=\"{error}\" action=left_for_manual_cleanup"
                    ),
                );
                return Phase::Failed;
            }
        };
        self.update(|shared| shared.devices[index].char_fd = Some(Arc::new(fd)));
        // The kernel may adjust what was asked for, for example rounding the
        // buffer size to a page. Serve with what it granted.
        let Some((depth, max_io_bytes)) = self.granted(&spec, &info) else {
            log(
                Some(index),
                "add",
                &format!(
                    "state=failed id={id} error=\"unusable device info: queues={} depth={} max_io_bytes={}\"",
                    info.nr_hw_queues, info.queue_depth, info.max_io_buf_bytes
                ),
            );
            self.teardown(control, &[(index, id)]);
            return Phase::Failed;
        };
        let size = resolved.length.div_ceil(512) * 512;
        if let Err(error) = control.set_params(id, &Params::read_only_basic(size, max_io_bytes)) {
            log(
                Some(index),
                "params",
                &format!("state=failed id={id} error=\"{error}\""),
            );
            self.teardown(control, &[(index, id)]);
            return Phase::Failed;
        }
        let attach = Attach {
            dev_id: id,
            depth,
            max_io_bytes,
            length: resolved.length,
        };
        self.update(|shared| shared.devices[index].attach = Some(attach));
        let attached = self.wait_until(Instant::now() + self.attach_timeout, |shared| {
            let device = &shared.devices[index];
            shared.stop
                || device.failed.iter().any(|f| *f)
                || device.queues.iter().all(|q| *q == QueueReport::Attached)
        });
        let all_attached = {
            let shared = self.lock();
            let device = &shared.devices[index];
            !shared.stop
                && !device.failed.iter().any(|f| *f)
                && device.queues.iter().all(|q| *q == QueueReport::Attached)
        };
        if !attached || !all_attached {
            log(Some(index), "attach", &format!("state=failed id={id}"));
            self.teardown(control, &[(index, id)]);
            return Phase::Failed;
        }
        let should_stop = || {
            let shared = self.lock();
            shared.stop || shared.devices[index].failed.iter().any(|f| *f)
        };
        if let Err(error) = control.start_dev(id, std::process::id() as i32, &should_stop) {
            log(
                Some(index),
                "start",
                &format!("state=failed id={id} error=\"{error}\""),
            );
            if control.is_poisoned() {
                // START_DEV may still be in the kernel, waiting on reads
                // from its partition scan while holding the device lock.
                // Failing every read lets it finish, so STOP_DEV over a
                // fresh channel can proceed.
                self.update(|shared| shared.devices[index].abandon = true);
            }
            self.teardown(control, &[(index, id)]);
            return Phase::Failed;
        }
        log(
            Some(index),
            "start",
            &format!(
                "state=started id={id} path=/dev/ublkb{id} cache={} etag={} length={}",
                device.cache,
                resolved.etag.as_str(),
                resolved.length
            ),
        );
        Phase::Started(id)
    }

    /// The queue depth and buffer size the kernel granted in ADD_DEV, or
    /// `None` if they cannot be served.
    fn granted(&self, spec: &DevSpec, info: &DevInfo) -> Option<(u16, u32)> {
        let usable = info.nr_hw_queues == spec.queues
            && (1..=spec.depth).contains(&info.queue_depth)
            && info.max_io_buf_bytes >= 4096
            && info.max_io_buf_bytes.is_multiple_of(512);
        usable.then_some((info.queue_depth, info.max_io_buf_bytes))
    }

    /// Remove a device an earlier run left with this fixed id. Returns
    /// whether the id is free now.
    fn replace_stale(&self, control: &mut dyn DeviceControl, index: usize, id: u32) -> bool {
        let fail = |what: &str, error: &dyn std::fmt::Display| {
            log(
                Some(index),
                "replace",
                &format!("state=failed id={id} step={what} error=\"{error}\""),
            );
            false
        };
        match control.get_dev_info(id) {
            Ok(_) => {}
            Err(error) if error.errno() == Some(libc::ENODEV) => return true,
            Err(error) => return fail("info", &error),
        }
        if let Err(error) = control.stop_dev(id)
            && error.errno() != Some(libc::ENODEV)
        {
            return fail("stop", &error);
        }
        // Hold the character device so the id cannot be reused while it is
        // deleted. Without it, a DEL_DEV retry could delete another
        // process's new device that took the id between attempts, so a
        // device whose node cannot be opened is left for manual cleanup.
        // A live server elsewhere holds it and makes the open fail.
        let fd = match control.open_char(&self.device_directory, id) {
            Ok(fd) => fd,
            Err(error) if error.raw_os_error() == Some(libc::EBUSY) => {
                return fail("open", &"device is in use by another server");
            }
            Err(error) => {
                return fail(
                    "open",
                    &format!(
                        "cannot hold /dev/ublkc{id} ({error}); not deleting it, \
                         delete the stale device manually"
                    ),
                );
            }
        };
        match control.del_dev(id) {
            Ok(()) => true,
            Err(error) => {
                if control.is_poisoned() {
                    std::mem::forget(fd);
                }
                fail("delete", &error)
            }
        }
    }

    /// Stop the given devices, let their queues finish, and delete them.
    ///
    /// Every device gets STOP_DEV, started or not: it makes the kernel
    /// abort every outstanding fetch, which is the only way the queues end.
    /// Dropping a queue's commands does not cancel them. Queues still
    /// attached after the timeout are told to answer every request with
    /// EIO, which lets a STOP_DEV still waiting on them finish. The manager
    /// keeps the character device open until the delete is done.
    fn teardown(&self, control: &mut dyn DeviceControl, devices: &[(usize, u32)]) {
        self.update(|shared| {
            for &(index, _) in devices {
                shared.devices[index].stopping = true;
            }
        });
        for &(index, id) in devices {
            match control.stop_dev(id) {
                Ok(()) => {}
                Err(error) if error.errno() == Some(libc::ENODEV) => {
                    log(Some(index), "stop", &format!("state=gone id={id}"))
                }
                Err(error) => log(
                    Some(index),
                    "stop",
                    &format!("state=failed id={id} error=\"{error}\""),
                ),
            }
        }
        let closed = |shared: &Shared| {
            devices.iter().all(|&(index, _)| {
                shared.devices[index]
                    .queues
                    .iter()
                    .all(|q| *q != QueueReport::Attached)
            })
        };
        if !self.wait_until(Instant::now() + self.shutdown_timeout, closed) {
            self.update(|shared| {
                for &(index, _) in devices {
                    shared.devices[index].abandon = true;
                }
            });
            if !self.wait_until(Instant::now() + self.shutdown_timeout, closed) {
                log(None, "teardown", "state=timeout");
            }
        }
        for &(index, id) in devices {
            if control.is_poisoned() {
                log(
                    Some(index),
                    "delete",
                    &format!(
                        "state=skipped id={id} reason=control_unusable action=left_for_manual_cleanup"
                    ),
                );
                self.leak_char(index);
                continue;
            }
            match control.del_dev(id) {
                Ok(()) => {
                    log(Some(index), "delete", &format!("state=deleted id={id}"));
                    self.release_char(index);
                }
                Err(error) => {
                    log(
                        Some(index),
                        "delete",
                        &format!("state=failed id={id} error=\"{error}\""),
                    );
                    if control.is_poisoned() {
                        self.leak_char(index);
                    } else {
                        self.release_char(index);
                    }
                }
            }
        }
    }
}

/// An opaque per-device value stored in the kernel. It helps operators tell
/// which process created a device.
/// The next device the control loop should act on.
///
/// A started device that reported a failure is torn down. A waiting device
/// with a resolve result is brought up, but devices with a fixed id go
/// first: an automatic device waits until no fixed-id device is still
/// waiting, so the kernel cannot hand one of our automatic devices an id that
/// a configured fixed-id device is about to claim (which would then look
/// like a stale device and be stopped). A fixed-id device that has failed no
/// longer reserves its id.
fn next_ready(
    phases: &[Phase],
    devices: &[UblkDeviceConfig],
    resolved: impl Fn(usize) -> bool,
    failed: impl Fn(usize) -> bool,
) -> Option<usize> {
    let fixed_waiting = phases
        .iter()
        .zip(devices)
        .any(|(phase, device)| matches!(phase, Phase::Waiting) && device.id.is_some());
    (0..phases.len()).find(|&i| match phases[i] {
        Phase::Waiting => resolved(i) && (devices[i].id.is_some() || !fixed_waiting),
        Phase::Started(_) => failed(i),
        Phase::Failed => false,
    })
}

/// Ids of the devices this manager currently holds.
fn owned_ids(phases: &[Phase]) -> Vec<u32> {
    phases
        .iter()
        .filter_map(|phase| match phase {
            Phase::Started(id) => Some(*id),
            _ => None,
        })
        .collect()
}

fn cookie(index: usize) -> u64 {
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |d| d.as_nanos() as u64);
    (u64::from(std::process::id()) << 32) ^ nanos ^ index as u64
}

fn log(index: Option<usize>, stage: &str, rest: &str) {
    match index {
        Some(index) => {
            eprintln!("racer: component=ublk device={index} stage={stage} {rest}")
        }
        None => eprintln!("racer: component=ublk stage={stage} {rest}"),
    }
}

#[cfg(test)]
mod tests {
    //! The control loop against a fake kernel and scripted workers.

    use super::*;
    use crate::model::CacheKey;
    use std::sync::Arc;

    const DEV_ID: u32 = 42;

    #[derive(Clone, Default)]
    struct Fake {
        ops: Arc<Mutex<Vec<String>>>,
        fail_start: bool,
        /// START_DEV times out with an unknown outcome and poisons the channel.
        start_timeout: bool,
        /// A device with the fixed id already exists.
        stale: bool,
        /// ADD_DEV grants this depth and buffer size instead.
        grant: Option<(u16, u32)>,
        stop_errno: Arc<Mutex<Option<i32>>>,
        /// DEL_DEV fails with an unknown outcome and poisons the channel.
        del_unknown: bool,
        poisoned: Arc<Mutex<bool>>,
        /// The manager, to check it still holds the char device on delete.
        manager: Arc<Mutex<Option<Arc<Manager>>>>,
        /// Opening a character device fails with this errno.
        open_errno: Option<i32>,
    }

    impl Fake {
        fn ops(&self) -> Vec<String> {
            self.ops.lock().unwrap().clone()
        }

        fn push(&self, op: String) {
            self.ops.lock().unwrap().push(op);
        }
    }

    impl DeviceControl for Fake {
        fn add_dev(&mut self, spec: &DevSpec) -> Result<DevInfo, UblkError> {
            if self.is_poisoned() {
                return Err(UblkError::Poisoned);
            }
            self.push(format!(
                "add id={:?} queues={} depth={} max={} flags={:#x}",
                spec.id, spec.queues, spec.depth, spec.max_io_bytes, spec.flags
            ));
            let (depth, max) = self.grant.unwrap_or((spec.depth, spec.max_io_bytes));
            Ok(DevInfo {
                dev_id: spec.id.unwrap_or(DEV_ID),
                nr_hw_queues: spec.queues,
                queue_depth: depth,
                max_io_buf_bytes: max,
                ..Default::default()
            })
        }

        fn set_params(&mut self, id: u32, params: &Params) -> Result<(), UblkError> {
            self.push(format!(
                "params {id} sectors={} max_sectors={}",
                params.basic.dev_sectors, params.basic.max_sectors
            ));
            Ok(())
        }

        fn start_dev(
            &mut self,
            id: u32,
            _pid: i32,
            _should_stop: &dyn Fn() -> bool,
        ) -> Result<(), UblkError> {
            self.push(format!("start {id}"));
            if self.start_timeout {
                *self.poisoned.lock().unwrap() = true;
                return Err(UblkError::TimedOut { op: "START_DEV" });
            }
            if self.fail_start {
                return Err(UblkError::Kernel {
                    op: "START_DEV",
                    errno: libc::EINVAL,
                });
            }
            Ok(())
        }

        fn stop_dev(&mut self, id: u32) -> Result<(), UblkError> {
            if self.is_poisoned() {
                self.push(format!("stop {id} poisoned"));
                return Err(UblkError::Poisoned);
            }
            self.push(format!("stop {id}"));
            match *self.stop_errno.lock().unwrap() {
                Some(errno) => Err(UblkError::Kernel {
                    op: "STOP_DEV",
                    errno,
                }),
                None => Ok(()),
            }
        }

        fn get_dev_info(&mut self, id: u32) -> Result<DevInfo, UblkError> {
            self.push(format!("info {id}"));
            if self.stale {
                Ok(DevInfo::default())
            } else {
                Err(UblkError::Kernel {
                    op: "GET_DEV_INFO",
                    errno: libc::ENODEV,
                })
            }
        }

        fn del_dev(&mut self, id: u32) -> Result<(), UblkError> {
            let held =
                self.manager.lock().unwrap().as_ref().is_none_or(|manager| {
                    manager.dev_id(0) != Some(id) || manager.char_fd(0).is_ok()
                });
            self.push(format!("del {id}{}", if held { "" } else { " unpinned" }));
            if self.del_unknown {
                *self.poisoned.lock().unwrap() = true;
                return Err(UblkError::TimedOut { op: "DEL_DEV" });
            }
            Ok(())
        }

        fn is_poisoned(&self) -> bool {
            *self.poisoned.lock().unwrap()
        }

        fn open_char(&mut self, _directory: &Path, id: u32) -> std::io::Result<OwnedFd> {
            if let Some(errno) = self.open_errno {
                self.push(format!("open {id} failed"));
                return Err(std::io::Error::from_raw_os_error(errno));
            }
            Ok(std::fs::File::open("/dev/null")?.into())
        }
    }

    fn config(id: Option<u32>) -> UblkConfig {
        UblkConfig {
            devices: vec![UblkDeviceConfig {
                cache: "models".into(),
                key: CacheKey([7; 32]),
                etag: None,
                id,
            }],
            queue_depth: 4,
            max_io_bytes: 1 << 20,
        }
    }

    fn manager(id: Option<u32>) -> Arc<Manager> {
        Arc::new(Manager::new(
            &config(id),
            2,
            Path::new("/nonexistent"),
            Duration::from_secs(2),
            Duration::from_secs(2),
        ))
    }

    fn resolved() -> Resolved {
        Resolved {
            cache: CacheId("models-id".into()),
            etag: StrongEtag::parse(b"\"v1\"").unwrap(),
            length: 1000,
        }
    }

    fn eventually(what: &str, check: impl Fn() -> bool) {
        let deadline = Instant::now() + Duration::from_secs(5);
        while !check() {
            assert!(Instant::now() < deadline, "timed out waiting for {what}");
            std::thread::sleep(Duration::from_millis(5));
        }
    }

    fn view(manager: &Manager, q: u16) -> DeviceView {
        manager.view(q, futures::task::noop_waker_ref()).remove(0)
    }

    fn spawn(manager: &Arc<Manager>, fake: &Fake) -> std::thread::JoinHandle<()> {
        let manager = manager.clone();
        let mut fake = fake.clone();
        *fake.manager.lock().unwrap() = Some(manager.clone());
        std::thread::spawn(move || manager.run_with(&mut fake))
    }

    #[test]
    fn command_timeout_outlasts_the_worker_drain_deadline() {
        for shutdown in [
            Duration::ZERO,
            Duration::from_millis(10),
            Duration::from_secs(30),
            Duration::from_secs(3600),
        ] {
            let timeout = command_timeout(shutdown);
            assert!(timeout >= shutdown + STOP_MARGIN, "{shutdown:?}");
            assert!(timeout >= MIN_COMMAND_TIMEOUT);
        }
        assert_eq!(command_timeout(Duration::MAX), Duration::MAX);
    }

    #[test]
    fn late_queue_claims_are_refused_once_stopping_or_done() {
        let manager = manager(None);
        let fake = Fake::default();
        let thread = spawn(&manager, &fake);
        manager.report_resolved(0, Ok(resolved()));
        eventually("attach", || view(&manager, 0).attach.is_some());
        // Queue 1 attaches before the stop; queue 0 is late.
        assert!(manager.claim_queue(0, 1));
        manager.request_stop();
        eventually("stopping", || manager.stopping(0));
        assert!(
            !manager.claim_queue(0, 0),
            "a device being stopped takes no new queue"
        );
        assert_ne!(manager.lock().devices[0].queues[0], QueueReport::Attached);
        manager.report_queue(0, 1, QueueReport::Closed);
        thread.join().unwrap();
        assert!(manager.control_done());
        assert!(!manager.claim_queue(0, 0), "nothing attaches once done");
        // Nothing attached is left for settle to stop.
        let mut reopened = false;
        assert!(manager.settle(&mut || {
            reopened = true;
            Err(UblkError::Poisoned)
        }));
        assert!(!reopened);
    }

    #[test]
    fn device_lifecycle_runs_in_order() {
        let manager = manager(None);
        let fake = Fake::default();
        let thread = spawn(&manager, &fake);
        assert!(view(&manager, 0).resolve);
        assert!(!view(&manager, 1).resolve, "only worker 0 resolves");
        manager.report_resolved(0, Ok(resolved()));
        eventually("attach", || view(&manager, 1).attach.is_some());
        let attach = view(&manager, 0).attach.unwrap();
        assert_eq!(
            attach,
            Attach {
                dev_id: DEV_ID,
                depth: 4,
                max_io_bytes: 1 << 20,
                length: 1000
            }
        );
        assert!(!view(&manager, 0).resolve);
        manager.report_queue(0, 0, QueueReport::Attached);
        assert!(!fake.ops().iter().any(|op| op.starts_with("start")));
        manager.report_queue(0, 1, QueueReport::Attached);
        eventually("start", || fake.ops().contains(&format!("start {DEV_ID}")));
        manager.request_stop();
        eventually("stop", || fake.ops().contains(&format!("stop {DEV_ID}")));
        // Queues still serve until the kernel aborts them.
        assert!(!view(&manager, 0).abandon);
        manager.report_queue(0, 0, QueueReport::Closed);
        manager.report_queue(0, 1, QueueReport::Closed);
        thread.join().unwrap();
        assert_eq!(
            fake.ops(),
            vec![
                format!(
                    "add id=None queues=2 depth=4 max=1048576 flags={:#x}",
                    UBLK_F_CMD_IOCTL_ENCODE | UBLK_F_USER_COPY
                ),
                format!("params {DEV_ID} sectors=2 max_sectors=2048"),
                format!("start {DEV_ID}"),
                format!("stop {DEV_ID}"),
                format!("del {DEV_ID}"),
            ]
        );
        assert!(manager.control_done());
        assert!(view(&manager, 0).abandon, "workers drop queues after exit");
        assert!(manager.char_fd(0).is_err(), "closed after the delete");
    }

    #[test]
    fn kernel_adjusted_device_info_is_used() {
        let manager = manager(None);
        let fake = Fake {
            grant: Some((2, 64 << 10)),
            ..Default::default()
        };
        let thread = spawn(&manager, &fake);
        manager.report_resolved(0, Ok(resolved()));
        eventually("attach", || view(&manager, 0).attach.is_some());
        let attach = view(&manager, 0).attach.unwrap();
        assert_eq!((attach.depth, attach.max_io_bytes), (2, 64 << 10));
        assert!(
            fake.ops()
                .contains(&format!("params {DEV_ID} sectors=2 max_sectors=128"))
        );
        manager.request_stop();
        thread.join().unwrap();
    }

    #[test]
    fn unusable_device_info_is_deleted() {
        let manager = manager(None);
        let fake = Fake {
            grant: Some((0, 1 << 20)),
            ..Default::default()
        };
        let thread = spawn(&manager, &fake);
        manager.report_resolved(0, Ok(resolved()));
        eventually("del", || fake.ops().contains(&format!("del {DEV_ID}")));
        manager.request_stop();
        thread.join().unwrap();
        assert!(view(&manager, 0).attach.is_none());
        assert!(!fake.ops().iter().any(|op| op.starts_with("params")));
    }

    #[test]
    fn external_stop_tears_down_and_ignores_enodev() {
        let manager = manager(None);
        let fake = Fake {
            stop_errno: Arc::new(Mutex::new(Some(libc::ENODEV))),
            ..Default::default()
        };
        let thread = spawn(&manager, &fake);
        manager.report_resolved(0, Ok(resolved()));
        eventually("attach", || view(&manager, 0).attach.is_some());
        manager.report_queue(0, 0, QueueReport::Attached);
        manager.report_queue(0, 1, QueueReport::Attached);
        eventually("start", || fake.ops().contains(&format!("start {DEV_ID}")));
        assert!(!manager.stopping(0));
        // A worker whose tags all aborted while the device was not being
        // stopped reports a failure and closes.
        manager.report_failure(0, 0);
        manager.report_queue(0, 0, QueueReport::Closed);
        manager.report_queue(0, 1, QueueReport::Closed);
        eventually("del", || fake.ops().contains(&format!("del {DEV_ID}")));
        assert!(manager.stopping(0));
        manager.request_stop();
        thread.join().unwrap();
    }

    #[test]
    fn unknown_delete_keeps_the_char_device_open() {
        let manager = manager(None);
        let fake = Fake {
            del_unknown: true,
            ..Default::default()
        };
        let thread = spawn(&manager, &fake);
        manager.report_resolved(0, Ok(resolved()));
        eventually("attach", || view(&manager, 0).attach.is_some());
        manager.report_queue(0, 0, QueueReport::Attached);
        manager.report_queue(0, 1, QueueReport::Attached);
        eventually("start", || fake.ops().contains(&format!("start {DEV_ID}")));
        manager.request_stop();
        eventually("stop", || fake.ops().contains(&format!("stop {DEV_ID}")));
        manager.report_queue(0, 0, QueueReport::Closed);
        manager.report_queue(0, 1, QueueReport::Closed);
        thread.join().unwrap();
        assert_eq!(fake.ops().last().unwrap(), &format!("del {DEV_ID}"));
        // Leaked, not closed: the manager no longer hands it out either.
        assert!(manager.char_fd(0).is_err());
    }

    #[test]
    fn fixed_id_without_a_stale_device_adds_directly() {
        let manager = manager(Some(9));
        let fake = Fake::default();
        let thread = spawn(&manager, &fake);
        manager.report_resolved(0, Ok(resolved()));
        eventually("attach", || view(&manager, 0).attach.is_some());
        manager.request_stop();
        thread.join().unwrap();
        let ops = fake.ops();
        assert_eq!(ops[0], "info 9");
        assert!(
            ops[1].starts_with("add id=Some(9)"),
            "no stale device: {ops:?}"
        );
    }

    #[test]
    fn fixed_id_replaces_a_stale_device() {
        let manager = manager(Some(9));
        let fake = Fake {
            stale: true,
            ..Default::default()
        };
        let thread = spawn(&manager, &fake);
        manager.report_resolved(0, Ok(resolved()));
        eventually("attach", || view(&manager, 0).attach.is_some());
        manager.request_stop();
        thread.join().unwrap();
        let ops = fake.ops();
        assert_eq!(ops[0], "info 9");
        assert_eq!(ops[1], "stop 9");
        assert_eq!(ops[2], "del 9");
        assert!(ops[3].starts_with("add id=Some(9)"));
    }

    #[test]
    fn fixed_id_stale_device_without_char_node_is_left_alone() {
        for errno in [libc::ENOENT, libc::EBUSY, libc::EACCES] {
            let manager = manager(Some(9));
            let fake = Fake {
                stale: true,
                open_errno: Some(errno),
                ..Default::default()
            };
            let thread = spawn(&manager, &fake);
            manager.report_resolved(0, Ok(resolved()));
            eventually("open", || fake.ops().contains(&"open 9 failed".to_string()));
            manager.request_stop();
            thread.join().unwrap();
            let ops = fake.ops();
            assert_eq!(ops[..3], ["info 9", "stop 9", "open 9 failed"], "{ops:?}");
            assert!(
                !ops.iter()
                    .any(|op| op.starts_with("del") || op.starts_with("add")),
                "errno {errno}: no delete without a held char device: {ops:?}"
            );
            assert!(view(&manager, 0).attach.is_none());
        }
    }

    fn device(id: Option<u32>, seed: u8) -> UblkDeviceConfig {
        UblkDeviceConfig {
            cache: "models".into(),
            key: CacheKey([seed; 32]),
            etag: None,
            id,
        }
    }

    #[test]
    fn next_ready_puts_fixed_ids_before_automatic_ids() {
        let devices = [device(None, 1), device(Some(5), 2)];
        let waiting = [Phase::Waiting, Phase::Waiting];
        // The automatic device is resolved but waits for the fixed one.
        assert_eq!(next_ready(&waiting, &devices, |i| i == 0, |_| false), None);
        assert_eq!(next_ready(&waiting, &devices, |_| true, |_| false), Some(1));
        // Once the fixed device is started or failed, automatic ones proceed.
        for done in [Phase::Started(5), Phase::Failed] {
            let phases = [Phase::Waiting, done];
            assert_eq!(next_ready(&phases, &devices, |_| true, |_| false), Some(0));
        }
    }

    #[test]
    fn next_ready_handles_failures_and_unresolved_devices() {
        let devices = [device(None, 1), device(None, 2), device(Some(5), 3)];
        let phases = [Phase::Started(1), Phase::Waiting, Phase::Failed];
        assert_eq!(next_ready(&phases, &devices, |_| false, |_| false), None);
        assert_eq!(next_ready(&phases, &devices, |_| true, |_| false), Some(1));
        assert_eq!(next_ready(&phases, &devices, |_| true, |i| i == 0), Some(0));
        assert_eq!(next_ready(&[], &[], |_| true, |_| true), None);
    }

    #[test]
    fn owned_ids_lists_started_devices_only() {
        let phases = [
            Phase::Started(3),
            Phase::Waiting,
            Phase::Failed,
            Phase::Started(0),
        ];
        assert_eq!(owned_ids(&phases), vec![3, 0]);
        assert!(owned_ids(&[]).is_empty());
    }

    #[test]
    fn fixed_id_held_by_this_manager_is_not_replaced() {
        let manager = manager(Some(9));
        let mut fake = Fake {
            stale: true,
            ..Default::default()
        };
        manager.report_resolved(0, Ok(resolved()));
        assert_eq!(manager.bring_up(&mut fake, 0, &[9]), Phase::Failed);
        assert!(
            fake.ops().is_empty(),
            "touched our own device: {:?}",
            fake.ops()
        );
    }

    #[test]
    fn fixed_id_device_is_added_before_an_automatic_device() {
        let config = UblkConfig {
            devices: vec![device(None, 1), device(Some(9), 2)],
            queue_depth: 4,
            max_io_bytes: 1 << 20,
        };
        let manager = Arc::new(Manager::new(
            &config,
            1,
            Path::new("/nonexistent"),
            Duration::from_secs(2),
            Duration::from_secs(2),
        ));
        let fake = Fake::default();
        let thread = spawn(&manager, &fake);
        manager.report_resolved(0, Ok(resolved()));
        std::thread::sleep(Duration::from_millis(50));
        assert!(fake.ops().is_empty(), "automatic device went first");
        manager.report_resolved(1, Ok(resolved()));
        eventually("both added", || {
            fake.ops().iter().filter(|op| op.starts_with("add")).count() == 2
        });
        manager.request_stop();
        thread.join().unwrap();
        let adds: Vec<_> = fake
            .ops()
            .into_iter()
            .filter(|op| op.starts_with("add") || op.starts_with("info"))
            .collect();
        assert_eq!(adds[0], "info 9");
        assert!(adds[1].starts_with("add id=Some(9)"), "{adds:?}");
        assert!(adds[2].starts_with("add id=None"), "{adds:?}");
    }

    #[test]
    fn resolve_failure_adds_nothing() {
        let manager = manager(None);
        let fake = Fake::default();
        let thread = spawn(&manager, &fake);
        manager.report_resolved(0, Err(Error::VersionUnavailable));
        std::thread::sleep(Duration::from_millis(50));
        manager.request_stop();
        thread.join().unwrap();
        assert!(fake.ops().is_empty());
    }

    #[test]
    fn empty_object_adds_nothing() {
        let manager = manager(None);
        let fake = Fake::default();
        let thread = spawn(&manager, &fake);
        manager.report_resolved(
            0,
            Ok(Resolved {
                length: 0,
                ..resolved()
            }),
        );
        std::thread::sleep(Duration::from_millis(50));
        manager.request_stop();
        thread.join().unwrap();
        assert!(fake.ops().is_empty());
    }

    #[test]
    fn attach_failure_stops_and_deletes_without_start() {
        let manager = manager(None);
        let fake = Fake::default();
        let thread = spawn(&manager, &fake);
        manager.report_resolved(0, Ok(resolved()));
        eventually("attach", || view(&manager, 0).attach.is_some());
        manager.report_queue(0, 0, QueueReport::Attached);
        manager.report_failure(0, 1);
        // STOP_DEV aborts the fetches of a never-started device, so the
        // attached queue closes without being abandoned.
        eventually("stop", || fake.ops().contains(&format!("stop {DEV_ID}")));
        assert!(!view(&manager, 0).abandon);
        manager.report_queue(0, 0, QueueReport::Closed);
        eventually("del", || fake.ops().contains(&format!("del {DEV_ID}")));
        manager.request_stop();
        thread.join().unwrap();
        assert!(!fake.ops().iter().any(|op| op.starts_with("start")));
        assert!(!fake.ops().iter().any(|op| op.contains("unpinned")));
    }

    #[test]
    fn start_failure_abandons_and_deletes() {
        let manager = manager(None);
        let fake = Fake {
            fail_start: true,
            ..Default::default()
        };
        let thread = spawn(&manager, &fake);
        manager.report_resolved(0, Ok(resolved()));
        eventually("attach", || view(&manager, 0).attach.is_some());
        manager.report_queue(0, 0, QueueReport::Attached);
        manager.report_queue(0, 1, QueueReport::Attached);
        eventually("stop", || fake.ops().contains(&format!("stop {DEV_ID}")));
        manager.report_queue(0, 0, QueueReport::Closed);
        manager.report_queue(0, 1, QueueReport::Closed);
        eventually("del", || fake.ops().contains(&format!("del {DEV_ID}")));
        manager.request_stop();
        thread.join().unwrap();
    }

    /// Run the control loop over a [`Reopening`] channel whose first
    /// channel is `first`. Each reopen yields a healthy channel that shares
    /// the op log, or fails when `reopen_fails` is set.
    fn spawn_reopening(
        manager: &Arc<Manager>,
        first: &Fake,
        reopen_fails: bool,
    ) -> std::thread::JoinHandle<()> {
        let manager = manager.clone();
        let first = first.clone();
        *first.manager.lock().unwrap() = Some(manager.clone());
        std::thread::spawn(move || {
            let fresh = Fake {
                ops: first.ops.clone(),
                manager: first.manager.clone(),
                ..Default::default()
            };
            let log = first.ops.clone();
            let mut control = Reopening::new(
                Box::new(first),
                Box::new(move || {
                    log.lock().unwrap().push("reopen".into());
                    if reopen_fails {
                        return Err(UblkError::Poisoned);
                    }
                    Ok(Box::new(fresh.clone()) as Box<dyn DeviceControl>)
                }),
            );
            manager.run_with(&mut control);
        })
    }

    #[test]
    fn start_timeout_tears_down_over_a_fresh_channel() {
        let manager = manager(None);
        let fake = Fake {
            start_timeout: true,
            ..Default::default()
        };
        let thread = spawn_reopening(&manager, &fake, false);
        manager.report_resolved(0, Ok(resolved()));
        eventually("attach", || view(&manager, 0).attach.is_some());
        manager.report_queue(0, 0, QueueReport::Attached);
        manager.report_queue(0, 1, QueueReport::Attached);
        eventually("stop", || fake.ops().contains(&format!("stop {DEV_ID}")));
        assert!(
            view(&manager, 0).abandon,
            "reads of a stuck START_DEV fail so it can finish"
        );
        manager.report_queue(0, 0, QueueReport::Closed);
        manager.report_queue(0, 1, QueueReport::Closed);
        eventually("del", || fake.ops().contains(&format!("del {DEV_ID}")));
        manager.request_stop();
        thread.join().unwrap();
        let ops = fake.ops();
        let start = ops.iter().position(|op| op.starts_with("start")).unwrap();
        assert_eq!(
            ops[start..start + 3],
            [
                format!("start {DEV_ID}"),
                "reopen".to_string(),
                format!("stop {DEV_ID}")
            ]
        );
        assert_eq!(ops.iter().filter(|op| *op == "reopen").count(), 1);
    }

    #[test]
    fn start_timeout_without_a_fresh_channel_leaves_the_device() {
        let manager = manager(None);
        let fake = Fake {
            start_timeout: true,
            ..Default::default()
        };
        let thread = spawn_reopening(&manager, &fake, true);
        manager.report_resolved(0, Ok(resolved()));
        eventually("attach", || view(&manager, 0).attach.is_some());
        manager.report_queue(0, 0, QueueReport::Attached);
        manager.report_queue(0, 1, QueueReport::Attached);
        eventually("stop", || {
            fake.ops().contains(&format!("stop {DEV_ID} poisoned"))
        });
        manager.report_queue(0, 0, QueueReport::Closed);
        manager.report_queue(0, 1, QueueReport::Closed);
        manager.request_stop();
        thread.join().unwrap();
        assert!(
            !fake.ops().iter().any(|op| op.starts_with("del")),
            "no delete over a poisoned channel"
        );
    }

    #[test]
    fn reopening_keeps_a_healthy_channel() {
        let fake = Fake::default();
        let mut control = Reopening::new(
            Box::new(fake.clone()),
            Box::new(|| panic!("a healthy channel is not reopened")),
        );
        control.stop_dev(DEV_ID).unwrap();
        control.stop_dev(DEV_ID).unwrap();
        assert!(!control.is_poisoned());
        assert_eq!(
            fake.ops(),
            [format!("stop {DEV_ID}"), format!("stop {DEV_ID}")]
        );
    }

    #[test]
    fn reopening_reports_poison_until_the_next_command() {
        let first = Fake {
            del_unknown: true,
            ..Default::default()
        };
        let fresh = Fake {
            ops: first.ops.clone(),
            ..Default::default()
        };
        let mut control = Reopening::new(
            Box::new(first.clone()),
            Box::new(move || Ok(Box::new(fresh.clone()) as Box<dyn DeviceControl>)),
        );
        assert!(control.del_dev(DEV_ID).is_err());
        assert!(control.is_poisoned(), "the caller sees the unknown outcome");
        control.stop_dev(DEV_ID).unwrap();
        assert!(!control.is_poisoned());
        assert_eq!(
            first.ops(),
            [format!("del {DEV_ID}"), format!("stop {DEV_ID}")]
        );
    }

    #[test]
    fn serving_failure_tears_down_a_started_device() {
        let manager = manager(None);
        let fake = Fake::default();
        let thread = spawn(&manager, &fake);
        manager.report_resolved(0, Ok(resolved()));
        eventually("attach", || view(&manager, 0).attach.is_some());
        manager.report_queue(0, 0, QueueReport::Attached);
        manager.report_queue(0, 1, QueueReport::Attached);
        eventually("start", || fake.ops().contains(&format!("start {DEV_ID}")));
        manager.report_failure(0, 1);
        eventually("stop", || fake.ops().contains(&format!("stop {DEV_ID}")));
        manager.report_queue(0, 0, QueueReport::Closed);
        manager.report_queue(0, 1, QueueReport::Closed);
        eventually("del", || fake.ops().contains(&format!("del {DEV_ID}")));
        manager.request_stop();
        thread.join().unwrap();
        let stops = fake
            .ops()
            .iter()
            .filter(|op| op.starts_with("stop"))
            .count();
        assert_eq!(stops, 1, "a failed device is not stopped again at exit");
    }

    #[test]
    fn stuck_queues_are_abandoned_after_the_timeout() {
        let manager = Arc::new(Manager::new(
            &config(None),
            1,
            Path::new("/nonexistent"),
            Duration::from_millis(100),
            Duration::from_secs(2),
        ));
        let fake = Fake::default();
        let thread = spawn(&manager, &fake);
        manager.report_resolved(0, Ok(resolved()));
        eventually("attach", || view(&manager, 0).attach.is_some());
        manager.report_queue(0, 0, QueueReport::Attached);
        eventually("start", || fake.ops().contains(&format!("start {DEV_ID}")));
        manager.request_stop();
        eventually("abandon", || view(&manager, 0).abandon);
        manager.report_queue(0, 0, QueueReport::Closed);
        thread.join().unwrap();
        assert_eq!(fake.ops().last().unwrap(), &format!("del {DEV_ID}"));
    }

    /// A manager with one started device whose single queue stays attached.
    fn started_with_stuck_queue(fake: &Fake) -> Arc<Manager> {
        let manager = Arc::new(Manager::new(
            &config(None),
            1,
            Path::new("/nonexistent"),
            Duration::from_millis(20),
            Duration::from_secs(2),
        ));
        *fake.manager.lock().unwrap() = Some(manager.clone());
        manager.report_resolved(0, Ok(resolved()));
        manager
    }

    /// Runs the guarded loop on a thread; the scripted worker attaches the
    /// queue and never closes it.
    fn run_stuck(
        manager: &Arc<Manager>,
        fake: &Fake,
        panic_after_start: bool,
        reopen_fails: bool,
    ) -> std::thread::JoinHandle<bool> {
        let (runner, mut loop_fake, reopen_fake) = (manager.clone(), fake.clone(), fake.clone());
        std::thread::spawn(move || {
            runner.run_guarded(
                |manager| {
                    if panic_after_start {
                        // The loop added the device and a worker attached,
                        // then the loop died before any teardown.
                        manager.update(|shared| {
                            shared.devices[0].attach = Some(Attach {
                                dev_id: DEV_ID,
                                depth: 4,
                                max_io_bytes: 1 << 20,
                                length: 1000,
                            });
                            shared.devices[0].queues[0] = QueueReport::Attached;
                        });
                        panic!("injected control panic");
                    }
                    manager.run_with(&mut loop_fake);
                },
                &mut || {
                    if reopen_fails {
                        return Err(UblkError::Kernel {
                            op: "open",
                            errno: libc::EACCES,
                        });
                    }
                    reopen_fake.push("reopen".into());
                    Ok(Box::new(reopen_fake.clone()))
                },
            )
        })
    }

    #[test]
    fn teardown_timeout_stops_stuck_devices_again() {
        let fake = Fake::default();
        let manager = started_with_stuck_queue(&fake);
        let thread = run_stuck(&manager, &fake, false, false);
        eventually("attach", || view(&manager, 0).attach.is_some());
        manager.report_queue(0, 0, QueueReport::Attached);
        eventually("start", || fake.ops().contains(&format!("start {DEV_ID}")));
        manager.request_stop();
        assert!(thread.join().unwrap(), "a second STOP_DEV settles");
        let ops = fake.ops();
        let tail = &ops[ops.len() - 3..];
        assert_eq!(
            tail,
            [
                format!("del {DEV_ID}"),
                "reopen".into(),
                format!("stop {DEV_ID}")
            ]
        );
        assert!(manager.control_done());
        assert!(manager.stopping(0));
    }

    #[test]
    fn panicked_control_loop_still_stops_devices() {
        let fake = Fake::default();
        let manager = started_with_stuck_queue(&fake);
        let thread = run_stuck(&manager, &fake, true, false);
        assert!(thread.join().unwrap());
        assert!(manager.control_done(), "a panic still marks the loop done");
        assert!(view(&manager, 0).abandon, "workers stop serving reads");
        assert_eq!(fake.ops(), ["reopen".to_string(), format!("stop {DEV_ID}")]);
    }

    #[test]
    fn workers_keep_serving_while_settling() {
        let fake = Fake::default();
        let manager = started_with_stuck_queue(&fake);
        let observed = Arc::new(std::sync::Mutex::new(None));
        let (runner, seen, reopen_fake) = (manager.clone(), observed.clone(), fake.clone());
        let thread = std::thread::spawn(move || {
            let inner = runner.clone();
            runner.run_guarded(
                |manager| {
                    manager.update(|shared| {
                        shared.devices[0].attach = Some(Attach {
                            dev_id: DEV_ID,
                            depth: 4,
                            max_io_bytes: 1 << 20,
                            length: 1000,
                        });
                        shared.devices[0].queues[0] = QueueReport::Attached;
                    });
                    panic!("injected control panic");
                },
                &mut || {
                    // STOP_DEV waits on requests only workers can answer.
                    *seen.lock().unwrap() = Some((inner.control_done(), inner.claim_queue(0, 0)));
                    Ok(Box::new(reopen_fake.clone()))
                },
            )
        });
        assert!(thread.join().unwrap());
        assert_eq!(*observed.lock().unwrap(), Some((false, false)));
        assert!(manager.control_done());
    }

    #[test]
    fn failed_second_stop_reports_unsettled() {
        let fake = Fake::default();
        let manager = started_with_stuck_queue(&fake);
        let thread = run_stuck(&manager, &fake, false, false);
        eventually("attach", || view(&manager, 0).attach.is_some());
        manager.report_queue(0, 0, QueueReport::Attached);
        eventually("start", || fake.ops().contains(&format!("start {DEV_ID}")));
        // Neither STOP_DEV can finish.
        *fake.stop_errno.lock().unwrap() = Some(libc::ETIMEDOUT);
        manager.request_stop();
        assert!(!thread.join().unwrap());
    }

    #[test]
    fn unreachable_control_device_reports_unsettled() {
        let fake = Fake::default();
        let manager = started_with_stuck_queue(&fake);
        let thread = run_stuck(&manager, &fake, false, true);
        eventually("attach", || view(&manager, 0).attach.is_some());
        manager.report_queue(0, 0, QueueReport::Attached);
        eventually("start", || fake.ops().contains(&format!("start {DEV_ID}")));
        manager.request_stop();
        assert!(!thread.join().unwrap());
    }

    #[test]
    fn gone_device_counts_as_settled() {
        let fake = Fake::default();
        let manager = started_with_stuck_queue(&fake);
        let thread = run_stuck(&manager, &fake, false, false);
        eventually("attach", || view(&manager, 0).attach.is_some());
        manager.report_queue(0, 0, QueueReport::Attached);
        eventually("start", || fake.ops().contains(&format!("start {DEV_ID}")));
        *fake.stop_errno.lock().unwrap() = Some(libc::ENODEV);
        manager.request_stop();
        assert!(thread.join().unwrap());
    }

    #[test]
    fn closed_queues_need_no_second_stop() {
        let fake = Fake::default();
        let manager = started_with_stuck_queue(&fake);
        let thread = run_stuck(&manager, &fake, false, true);
        eventually("attach", || view(&manager, 0).attach.is_some());
        manager.report_queue(0, 0, QueueReport::Attached);
        eventually("start", || fake.ops().contains(&format!("start {DEV_ID}")));
        manager.request_stop();
        eventually("stop", || fake.ops().contains(&format!("stop {DEV_ID}")));
        manager.report_queue(0, 0, QueueReport::Closed);
        // Reopening would fail, so success means it was never tried.
        assert!(thread.join().unwrap());
        assert!(!fake.ops().contains(&"reopen".to_string()));
    }

    #[test]
    fn missing_control_device_finishes_without_hanging() {
        let manager = manager(None);
        manager.run();
        assert!(manager.control_done());
    }
}
