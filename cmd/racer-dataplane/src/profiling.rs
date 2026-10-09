//! Opt-in CPU sampling. One session owns the process-wide profiling lease.

use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};
use std::sync::{
    Arc, Mutex,
    atomic::{AtomicBool, AtomicUsize, Ordering},
};
use std::time::Duration;

#[cfg(all(
    target_os = "linux",
    any(target_arch = "x86_64", target_arch = "aarch64")
))]
mod perf;
mod profile;

pub(crate) const MAX_SAMPLES: usize = 65_536;
pub(crate) const MAX_DEPTH: usize = 64;
pub(crate) const MAX_OUTPUT: usize = 16 * 1024 * 1024;
/// Independent process-wide diagnostic budget, not worker admission or an RSS cap.
pub const PROFILE_MEMORY_BUDGET_BYTES: usize = 192 * 1024 * 1024;
static RESERVED_BYTES: AtomicUsize = AtomicUsize::new(0);

struct ProcessReservation;
impl ProcessReservation {
    fn acquire() -> Result<Self, ProfileError> {
        memory_plan(PROFILE_MEMORY_BUDGET_BYTES)?;
        RESERVED_BYTES
            .compare_exchange(
                0,
                PROFILE_MEMORY_BUDGET_BYTES,
                Ordering::AcqRel,
                Ordering::Acquire,
            )
            .map_err(|_| ProfileError::Busy)?;
        Ok(Self)
    }
}
impl Drop for ProcessReservation {
    fn drop(&mut self) {
        RESERVED_BYTES.store(0, Ordering::Release);
    }
}

/// Reserve the worst-case session footprint before opening rings or allocating.
/// Includes 64 MiB rings, fixed samples/index, 64 MiB export workspace/output,
/// and 8 MiB for proc snapshots, task bookkeeping, and the helper stack.
fn memory_plan(limit: usize) -> Result<(), ProfileError> {
    let bytes = 64 * 1024 * 1024
        + MAX_SAMPLES * std::mem::size_of::<Sample>()
        + MAX_SAMPLES * 2 * std::mem::size_of::<usize>()
        + 64 * 1024 * 1024
        + 8 * 1024 * 1024;
    if bytes > limit {
        Err(ProfileError::ResourceLimit)
    } else {
        Ok(())
    }
}

fn check(cancel: &AtomicBool, deadline: std::time::Instant) -> Result<(), ProfileError> {
    if cancel.load(Ordering::Acquire) || std::time::Instant::now() >= deadline {
        Err(ProfileError::Cancelled)
    } else {
        Ok(())
    }
}

/// Fixed errors contain no request data or filesystem paths.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ProfileError {
    Disabled,
    Unsupported,
    InvalidDuration,
    Busy,
    PermissionDenied,
    ResourceLimit,
    Io,
    MalformedRecord,
    SampleLoss,
    MappingChanged,
    Cancelled,
    Empty,
}

impl ProfileError {
    pub fn code(self) -> &'static str {
        match self {
            Self::Disabled => "disabled",
            Self::Unsupported => "unsupported",
            Self::InvalidDuration => "invalid_duration",
            Self::Busy => "busy",
            Self::PermissionDenied => "permission_denied",
            Self::ResourceLimit => "resource_limit",
            Self::Io => "io_error",
            Self::MalformedRecord => "malformed_record",
            Self::SampleLoss => "sample_loss",
            Self::MappingChanged => "mapping_changed",
            Self::Cancelled => "cancelled",
            Self::Empty => "no_samples",
        }
    }

    fn os(error: std::io::Error) -> Self {
        match error.raw_os_error() {
            Some(libc::EPERM | libc::EACCES) => Self::PermissionDenied,
            Some(libc::ENOMEM | libc::ENOSPC | libc::EMFILE | libc::ENFILE) => Self::ResourceLimit,
            Some(libc::ENOSYS | libc::EOPNOTSUPP) => Self::Unsupported,
            _ => Self::Io,
        }
    }
}

impl std::fmt::Display for ProfileError {
    fn fmt(&self, out: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        out.write_str(self.code())
    }
}
impl std::error::Error for ProfileError {}

pub struct ProfilingController {
    enabled: bool,
    stopped: AtomicBool,
    state: Mutex<ControllerState>,
}
#[derive(Default)]
struct ControllerState {
    active: Option<std::sync::Weak<SessionState>>,
    helper: Option<std::thread::JoinHandle<()>>,
}

impl ProfilingController {
    pub fn new(enabled: bool) -> Arc<Self> {
        Arc::new(Self {
            enabled,
            stopped: AtomicBool::new(false),
            state: Mutex::new(ControllerState::default()),
        })
    }

    pub fn start(self: &Arc<Self>, duration: Duration) -> Result<ProfileSession, ProfileError> {
        if !self.enabled {
            return Err(ProfileError::Disabled);
        }
        if !(1..=60).contains(&duration.as_secs()) || duration.subsec_nanos() != 0 {
            return Err(ProfileError::InvalidDuration);
        }
        if !cfg!(all(
            target_os = "linux",
            any(target_arch = "x86_64", target_arch = "aarch64")
        )) {
            return Err(ProfileError::Unsupported);
        }
        // Serialize stop with startup and handle publication. The helper never
        // acquires this lock, and joins below only touch already-finished threads.
        let mut control = self.state.lock().unwrap_or_else(|e| e.into_inner());
        if self.stopped.load(Ordering::Acquire) {
            return Err(ProfileError::Cancelled);
        }
        if control.helper.as_ref().is_some_and(|h| !h.is_finished()) {
            return Err(ProfileError::Busy);
        }
        if let Some(handle) = control.helper.take() {
            let _ = handle.join();
        }
        let lease = ProcessReservation::acquire()?;
        // SAFETY: eventfd has no pointer arguments; ownership transfers only on success.
        let raw = unsafe { libc::eventfd(0, libc::EFD_CLOEXEC | libc::EFD_NONBLOCK) };
        if raw < 0 {
            return Err(ProfileError::os(std::io::Error::last_os_error()));
        }
        let state = Arc::new(SessionState {
            _lease: lease,
            // SAFETY: raw is a new, successful eventfd descriptor.
            completion: unsafe { OwnedFd::from_raw_fd(raw) },
            cancelled: AtomicBool::new(false),
            result: Mutex::new(None),
        });
        let helper = state.clone();
        control.active = Some(Arc::downgrade(&state));
        let handle = std::thread::Builder::new()
            .name("racer-profiler".into())
            .stack_size(2 * 1024 * 1024)
            .spawn(move || {
                let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                    #[cfg(all(
                        target_os = "linux",
                        any(target_arch = "x86_64", target_arch = "aarch64")
                    ))]
                    {
                        perf::capture(duration, &helper.cancelled)
                    }
                    #[cfg(not(all(
                        target_os = "linux",
                        any(target_arch = "x86_64", target_arch = "aarch64")
                    )))]
                    {
                        Err(ProfileError::Unsupported)
                    }
                }))
                .unwrap_or(Err(ProfileError::Io));
                *helper.result.lock().unwrap_or_else(|e| e.into_inner()) = Some(result);
                let one = 1u64;
                // SAFETY: the descriptor and eight-byte value remain live during write.
                loop {
                    let n = unsafe {
                        libc::write(
                            helper.completion.as_raw_fd(),
                            (&one as *const u64).cast(),
                            8,
                        )
                    };
                    if n >= 0
                        || std::io::Error::last_os_error().kind() != std::io::ErrorKind::Interrupted
                    {
                        break;
                    }
                }
            })
            .map_err(ProfileError::os)?;
        control.helper = Some(handle);
        Ok(ProfileSession { state })
    }

    /// Request shutdown without joining on an I/O worker.
    pub fn cancel_active(&self) {
        let control = self.state.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(state) = control.active.as_ref().and_then(std::sync::Weak::upgrade) {
            state.cancelled.store(true, Ordering::Release);
        }
    }

    /// Irreversibly reject new sessions and cancel any startup or running job.
    pub fn stop(&self) {
        self.stopped.store(true, Ordering::Release);
        self.cancel_active();
    }

    /// Join only a finished helper. Shutdown should poll this before exiting.
    pub fn poll_stopped(&self) -> bool {
        let Ok(mut control) = self.state.try_lock() else {
            return false;
        };
        if control.helper.as_ref().is_some_and(|h| !h.is_finished()) {
            return false;
        }
        if let Some(handle) = control.helper.take() {
            let _ = handle.join();
        }
        true
    }
}

struct SessionState {
    completion: OwnedFd,
    cancelled: AtomicBool,
    result: Mutex<Option<Result<Vec<u8>, ProfileError>>>,
    // Drop the budget last, after any retained result bytes and descriptor.
    _lease: ProcessReservation,
}

pub struct ProfileSession {
    state: Arc<SessionState>,
}
impl ProfileSession {
    /// Duplicate the notification descriptor for caller-owned readiness polling.
    pub fn completion_fd(&self) -> Result<OwnedFd, ProfileError> {
        self.state.completion.try_clone().map_err(ProfileError::os)
    }
    /// Take the result once, without waiting for the helper or its mutex.
    pub fn result(&self) -> Option<Result<Vec<u8>, ProfileError>> {
        self.state.result.try_lock().ok()?.take()
    }
    pub fn cancel(&self) {
        self.state.cancelled.store(true, Ordering::Release);
    }
}
impl Drop for ProfileSession {
    fn drop(&mut self) {
        self.cancel();
    }
}

#[derive(Clone)]
pub(crate) struct Sample {
    tid: u32,
    birth: u64,
    count: u64,
    period: u64,
    depth: usize,
    frames: [u64; MAX_DEPTH],
}

#[cfg(test)]
mod tests {
    use super::*;
    static TEST_LOCK: Mutex<()> = Mutex::new(());
    #[test]
    fn admission_and_errno_are_explicit() {
        let _test = TEST_LOCK.lock().unwrap();
        assert!(matches!(
            ProfilingController::new(false).start(Duration::from_secs(1)),
            Err(ProfileError::Disabled)
        ));
        let controller = ProfilingController::new(true);
        for duration in [
            Duration::ZERO,
            Duration::from_secs(61),
            Duration::from_millis(1500),
        ] {
            assert!(matches!(
                controller.start(duration),
                Err(ProfileError::InvalidDuration)
            ));
        }
        let reservation = ProcessReservation::acquire().unwrap();
        assert!(matches!(
            controller.start(Duration::from_secs(1)),
            Err(ProfileError::Busy)
        ));
        assert_eq!(
            ProfileError::os(std::io::Error::from_raw_os_error(libc::EPERM)),
            ProfileError::PermissionDenied
        );
        assert_eq!(
            RESERVED_BYTES.load(Ordering::Acquire),
            PROFILE_MEMORY_BUDGET_BYTES
        );
        drop(reservation);
        assert_eq!(RESERVED_BYTES.load(Ordering::Acquire), 0);
        assert!(memory_plan(1).is_err());
        memory_plan(192 * 1024 * 1024).unwrap();
    }

    #[test]
    fn cancellation_signals_completion_and_retains_lease_until_session_drop() {
        let _test = TEST_LOCK.lock().unwrap();
        let controller = ProfilingController::new(true);
        let session = controller.start(Duration::from_secs(1)).unwrap();
        assert!(matches!(
            ProfilingController::new(true).start(Duration::from_secs(1)),
            Err(ProfileError::Busy)
        ));
        let fd = session.completion_fd().unwrap();
        session.cancel();
        let deadline = std::time::Instant::now() + Duration::from_secs(3);
        let result = loop {
            if let Some(result) = session.result() {
                break result;
            }
            assert!(std::time::Instant::now() < deadline, "helper did not stop");
            std::thread::sleep(Duration::from_millis(10));
        };
        assert!(matches!(
            result,
            Err(ProfileError::Cancelled | ProfileError::PermissionDenied)
        ));
        assert!(session.result().is_none());
        assert!(matches!(
            controller.start(Duration::from_secs(1)),
            Err(ProfileError::Busy)
        ));
        let mut notification = libc::pollfd {
            fd: fd.as_raw_fd(),
            events: libc::POLLIN,
            revents: 0,
        };
        // SAFETY: poll receives one initialized pollfd and a bounded timeout.
        assert_eq!(unsafe { libc::poll(&mut notification, 1, 1000) }, 1);
        drop(session);
        while !controller.poll_stopped() {
            assert!(std::time::Instant::now() < deadline);
            std::thread::sleep(Duration::from_millis(1));
        }
        assert_eq!(RESERVED_BYTES.load(Ordering::Acquire), 0);
    }

    #[test]
    fn stop_is_irreversible_and_serializes_with_start() {
        let _test = TEST_LOCK.lock().unwrap();
        for _ in 0..8 {
            let controller = ProfilingController::new(true);
            let start = controller.clone();
            let handle = std::thread::spawn(move || start.start(Duration::from_secs(1)));
            controller.stop();
            match handle.join().unwrap() {
                Ok(session) => {
                    session.cancel();
                    drop(session);
                }
                Err(error) => assert_eq!(error, ProfileError::Cancelled),
            }
            let until = std::time::Instant::now() + Duration::from_secs(2);
            while !controller.poll_stopped() {
                assert!(std::time::Instant::now() < until);
                std::thread::sleep(Duration::from_millis(1));
            }
            assert!(matches!(
                controller.start(Duration::from_secs(1)),
                Err(ProfileError::Cancelled)
            ));
            assert_eq!(RESERVED_BYTES.load(Ordering::Acquire), 0);
        }
    }

    #[test]
    fn diagnostic_budget_releases_only_after_last_owner_and_failed_setup() {
        let _test = TEST_LOCK.lock().unwrap();
        let response = Arc::new(ProcessReservation::acquire().unwrap());
        let helper = response.clone();
        drop(response);
        assert_eq!(
            RESERVED_BYTES.load(Ordering::Acquire),
            PROFILE_MEMORY_BUDGET_BYTES
        );
        assert!(matches!(
            ProcessReservation::acquire(),
            Err(ProfileError::Busy)
        ));
        drop(helper);
        assert_eq!(RESERVED_BYTES.load(Ordering::Acquire), 0);
        fn failed_setup() -> Result<(), ProfileError> {
            let _reservation = ProcessReservation::acquire()?;
            Err(ProfileError::Io)
        }
        assert_eq!(failed_setup(), Err(ProfileError::Io));
        assert_eq!(RESERVED_BYTES.load(Ordering::Acquire), 0);
        drop(ProcessReservation::acquire().unwrap());
    }

    #[inline(never)]
    fn native_profile_burn(mut value: u64) -> u64 {
        for index in 0..16_384u64 {
            value = std::hint::black_box(
                value.wrapping_mul(6364136223846793005).rotate_left(17) ^ index,
            );
        }
        value
    }

    /// Isolated fixture: no application, storage, network, RDMA, or cluster setup.
    /// Parent must review the binary and explicitly run this ignored test.
    #[test]
    #[ignore = "requires explicitly authorized perf access and a prepared output file"]
    fn native_two_worker_capture() {
        use std::io::Write;
        use std::os::unix::fs::{MetadataExt, OpenOptionsExt};
        let _test = TEST_LOCK.lock().unwrap();
        let path = std::path::PathBuf::from(
            std::env::var_os("RACER_PPROF_TEST_OUTPUT").expect("output path"),
        );
        let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../tmp")
            .canonicalize()
            .unwrap();
        let parent = path.parent().unwrap().canonicalize().unwrap();
        assert!(
            parent.starts_with(&root),
            "output must be inside this worktree tmp"
        );
        let owner: u32 = std::env::var("RACER_PPROF_TEST_OUTPUT_UID")
            .expect("expected output owner UID")
            .parse()
            .unwrap();
        // Open only a precreated regular empty file. Never truncate or follow a symlink.
        let mut output = std::fs::OpenOptions::new()
            .write(true)
            .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK | libc::O_CLOEXEC)
            .open(&path)
            .unwrap();
        let metadata = output.metadata().unwrap();
        assert!(metadata.is_file());
        assert_eq!(metadata.uid(), owner);
        assert_eq!(metadata.nlink(), 1);
        assert_eq!(metadata.len(), 0);
        assert_eq!(metadata.mode() & 0o777, 0o600);
        let stop = Arc::new(AtomicBool::new(false));
        struct Workers {
            stop: Arc<AtomicBool>,
            handles: Vec<std::thread::JoinHandle<()>>,
        }
        impl Drop for Workers {
            fn drop(&mut self) {
                self.stop.store(true, Ordering::Release);
                for handle in self.handles.drain(..) {
                    let _ = handle.join();
                }
            }
        }
        let mut workers = Workers {
            stop: stop.clone(),
            handles: Vec::new(),
        };
        let (sender, receiver) = std::sync::mpsc::channel();
        for index in 0..2 {
            let stop = stop.clone();
            let sender = sender.clone();
            workers.handles.push(
                std::thread::Builder::new()
                    .name(format!("racer-prof-{index}"))
                    .spawn(move || {
                        // SAFETY: gettid has no pointer arguments.
                        sender
                            .send(unsafe { libc::syscall(libc::SYS_gettid) } as u32)
                            .unwrap();
                        let mut value = index as u64 + 1;
                        while !stop.load(Ordering::Acquire) {
                            value = native_profile_burn(value);
                        }
                        std::hint::black_box(value);
                    })
                    .unwrap(),
            );
        }
        let tids = [
            receiver.recv_timeout(Duration::from_secs(1)).unwrap(),
            receiver.recv_timeout(Duration::from_secs(1)).unwrap(),
        ];
        let controller = ProfilingController::new(true);
        let session = controller.start(Duration::from_secs(2)).unwrap();
        let deadline = std::time::Instant::now() + Duration::from_secs(12);
        let bytes = loop {
            if let Some(result) = session.result() {
                break result.expect("native capture must succeed, permission denial is a failure");
            }
            assert!(std::time::Instant::now() < deadline);
            std::thread::sleep(Duration::from_millis(10));
        };
        profile::assert_native_threads(&bytes, &tids);
        output.write_all(&bytes).unwrap();
        output.sync_all().unwrap();
        drop(session);
        controller.stop();
        while !controller.poll_stopped() {
            assert!(std::time::Instant::now() < deadline);
            std::thread::sleep(Duration::from_millis(1));
        }
        assert_eq!(RESERVED_BYTES.load(Ordering::Acquire), 0);
        eprintln!("native profile bytes={} busy_tids={tids:?}", bytes.len());
        drop(workers);
    }
}
