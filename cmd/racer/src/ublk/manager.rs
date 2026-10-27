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
    /// Workers must drop their queues now, without waiting for aborts.
    abandon: bool,
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
    fn del_dev(&mut self, id: u32) -> Result<(), UblkError>;

    /// Open `<directory>/ublkcN` read-write.
    fn open_char(&mut self, directory: &Path, id: u32) -> std::io::Result<OwnedFd> {
        open_char(directory, id)
    }
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

    fn del_dev(&mut self, id: u32) -> Result<(), UblkError> {
        Control::del_dev(self, id)
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

    /// Whether the control thread has exited. Every device it started has
    /// been stopped by then, so no kernel request still waits for a read.
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
    pub(crate) fn run(&self) {
        match Control::open(&self.device_directory) {
            Ok(mut control) => self.run_with(&mut control),
            Err(error) => {
                log(None, "open", &format!("error=\"{error}\""));
                self.finish();
            }
        }
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
                let ready = (0..phases.len()).find(|&i| match phases[i] {
                    Phase::Waiting => shared.devices[i].resolved.is_some(),
                    Phase::Started(_) => shared.devices[i].failed.iter().any(|f| *f),
                    Phase::Failed => false,
                });
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
                Phase::Waiting => self.bring_up(control, index),
                Phase::Started(id) => {
                    log(Some(index), "serve", &format!("state=failed id={id}"));
                    self.teardown(control, &[(index, id, true)]);
                    Phase::Failed
                }
                phase => phase,
            };
        }
        let started: Vec<_> = phases
            .iter()
            .enumerate()
            .filter_map(|(i, phase)| match phase {
                Phase::Started(id) => Some((i, *id, true)),
                _ => None,
            })
            .collect();
        self.teardown(control, &started);
        self.finish();
    }

    /// Add, attach, and start one device. Failures tear it down.
    fn bring_up(&self, control: &mut dyn DeviceControl, index: usize) -> Phase {
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
            // A device left by an earlier run with this id. Missing is fine.
            let _ = control.stop_dev(id);
            if let Err(error) = control.del_dev(id) {
                log(
                    Some(index),
                    "replace",
                    &format!("state=failed id={id} error=\"{error}\""),
                );
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
        let id = match control.add_dev(&spec) {
            Ok(info) => info.dev_id,
            Err(error) => {
                log(
                    Some(index),
                    "add",
                    &format!("state=failed error=\"{error}\""),
                );
                return Phase::Failed;
            }
        };
        let size = resolved.length.div_ceil(512) * 512;
        if let Err(error) =
            control.set_params(id, &Params::read_only_basic(size, self.max_io_bytes))
        {
            log(
                Some(index),
                "params",
                &format!("state=failed id={id} error=\"{error}\""),
            );
            self.teardown(control, &[(index, id, false)]);
            return Phase::Failed;
        }
        let fd = match control.open_char(&self.device_directory, id) {
            Ok(fd) => fd,
            Err(error) => {
                log(
                    Some(index),
                    "open",
                    &format!("state=failed id={id} error=\"{error}\""),
                );
                self.teardown(control, &[(index, id, false)]);
                return Phase::Failed;
            }
        };
        let attach = Attach {
            dev_id: id,
            depth: self.queue_depth,
            max_io_bytes: self.max_io_bytes,
            length: resolved.length,
        };
        self.update(|shared| {
            let device = &mut shared.devices[index];
            device.char_fd = Some(Arc::new(fd));
            device.attach = Some(attach);
        });
        let attached = self.wait_until(Instant::now() + self.attach_timeout, |shared| {
            let device = &shared.devices[index];
            shared.stop
                || device.failed.iter().any(|f| *f)
                || device.queues.iter().all(|q| *q == QueueReport::Attached)
        });
        // Every worker that will attach has its own duplicate by now.
        self.release_char(index);
        let all_attached = {
            let shared = self.lock();
            let device = &shared.devices[index];
            !shared.stop
                && !device.failed.iter().any(|f| *f)
                && device.queues.iter().all(|q| *q == QueueReport::Attached)
        };
        if !attached || !all_attached {
            log(Some(index), "attach", &format!("state=failed id={id}"));
            self.teardown(control, &[(index, id, false)]);
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
            self.teardown(control, &[(index, id, false)]);
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

    /// Stop the given devices, let their queues drain, and delete them.
    /// `started` devices get STOP_DEV first so the kernel aborts every tag.
    /// Queues still attached after the timeout are abandoned by their workers.
    fn teardown(&self, control: &mut dyn DeviceControl, devices: &[(usize, u32, bool)]) {
        for &(index, _, _) in devices {
            self.release_char(index);
        }
        for &(index, id, started) in devices {
            if started && let Err(error) = control.stop_dev(id) {
                log(
                    Some(index),
                    "stop",
                    &format!("state=failed id={id} error=\"{error}\""),
                );
            }
        }
        let closed = |shared: &Shared| {
            devices.iter().all(|&(index, _, _)| {
                shared.devices[index]
                    .queues
                    .iter()
                    .all(|q| *q != QueueReport::Attached)
            })
        };
        // A device that never started has no requests to abort.
        let any_started = devices.iter().any(|&(_, _, started)| started);
        if !any_started || !self.wait_until(Instant::now() + self.shutdown_timeout, closed) {
            self.update(|shared| {
                for &(index, _, _) in devices {
                    shared.devices[index].abandon = true;
                }
            });
            if !self.wait_until(Instant::now() + self.shutdown_timeout, closed) {
                log(None, "teardown", "state=timeout");
            }
        }
        for &(index, id, _) in devices {
            match control.del_dev(id) {
                Ok(()) => log(Some(index), "delete", &format!("state=deleted id={id}")),
                Err(error) => log(
                    Some(index),
                    "delete",
                    &format!("state=failed id={id} error=\"{error}\""),
                ),
            }
        }
    }
}

/// An opaque per-device value stored in the kernel. It helps operators tell
/// which process created a device.
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
            self.push(format!(
                "add id={:?} queues={} depth={} max={} flags={:#x}",
                spec.id, spec.queues, spec.depth, spec.max_io_bytes, spec.flags
            ));
            Ok(DevInfo {
                dev_id: spec.id.unwrap_or(DEV_ID),
                ..Default::default()
            })
        }

        fn set_params(&mut self, id: u32, params: &Params) -> Result<(), UblkError> {
            self.push(format!("params {id} sectors={}", params.basic.dev_sectors));
            Ok(())
        }

        fn start_dev(
            &mut self,
            id: u32,
            _pid: i32,
            _should_stop: &dyn Fn() -> bool,
        ) -> Result<(), UblkError> {
            self.push(format!("start {id}"));
            if self.fail_start {
                return Err(UblkError::Kernel {
                    op: "START_DEV",
                    errno: libc::EINVAL,
                });
            }
            Ok(())
        }

        fn stop_dev(&mut self, id: u32) -> Result<(), UblkError> {
            self.push(format!("stop {id}"));
            Ok(())
        }

        fn del_dev(&mut self, id: u32) -> Result<(), UblkError> {
            self.push(format!("del {id}"));
            Ok(())
        }

        fn open_char(&mut self, _directory: &Path, _id: u32) -> std::io::Result<OwnedFd> {
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
        std::thread::spawn(move || manager.run_with(&mut fake))
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
                format!("params {DEV_ID} sectors=2"),
                format!("start {DEV_ID}"),
                format!("stop {DEV_ID}"),
                format!("del {DEV_ID}"),
            ]
        );
        assert!(manager.control_done());
        assert!(view(&manager, 0).abandon, "workers drop queues after exit");
    }

    #[test]
    fn fixed_id_replaces_a_stale_device() {
        let manager = manager(Some(9));
        let fake = Fake::default();
        let thread = spawn(&manager, &fake);
        manager.report_resolved(0, Ok(resolved()));
        eventually("attach", || view(&manager, 0).attach.is_some());
        manager.request_stop();
        thread.join().unwrap();
        let ops = fake.ops();
        assert_eq!(ops[0], "stop 9");
        assert_eq!(ops[1], "del 9");
        assert!(ops[2].starts_with("add id=Some(9)"));
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
    fn attach_failure_abandons_and_deletes_without_start() {
        let manager = manager(None);
        let fake = Fake::default();
        let thread = spawn(&manager, &fake);
        manager.report_resolved(0, Ok(resolved()));
        eventually("attach", || view(&manager, 0).attach.is_some());
        manager.report_queue(0, 0, QueueReport::Attached);
        manager.report_failure(0, 1);
        eventually("abandon", || view(&manager, 0).abandon);
        manager.report_queue(0, 0, QueueReport::Closed);
        eventually("del", || fake.ops().contains(&format!("del {DEV_ID}")));
        manager.request_stop();
        thread.join().unwrap();
        assert!(!fake.ops().iter().any(|op| op.starts_with("start")));
        assert!(!fake.ops().iter().any(|op| op.starts_with("stop")));
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
        eventually("abandon", || view(&manager, 0).abandon);
        manager.report_queue(0, 0, QueueReport::Closed);
        manager.report_queue(0, 1, QueueReport::Closed);
        eventually("del", || fake.ops().contains(&format!("del {DEV_ID}")));
        manager.request_stop();
        thread.join().unwrap();
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

    #[test]
    fn missing_control_device_finishes_without_hanging() {
        let manager = manager(None);
        manager.run();
        assert!(manager.control_done());
    }
}
