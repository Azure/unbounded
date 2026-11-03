//! Managed-mode disk status reporting and approval-change restarts.
//!
//! Raw-device storage is planned once at startup. This monitor runs on one
//! unpinned, mostly sleeping thread beside embedded control; managed mode
//! reserves a thread-cap slot for it. It republishes
//! per-disk status in the Pod report every [`RESCAN`], so newly attached disks
//! appear without a restart, and it requests a process restart when:
//!
//! - this Node's approved disk set differs from the startup set for
//!   [`DEBOUNCE`] (the latest value wins, and reverting cancels), or
//! - an approved disk that was missing at startup appears.
//!
//! While the latest approved set equals the startup set, the monitor echoes
//! the Node's approval revision in the Pod report, so a writer can tell that
//! the reported states reflect its change. It stops echoing once a restart is
//! requested, so a draining process never vouches for a newer approval.
//!
//! A restart drains workers through the normal shutdown path and exits with
//! [`Error::DiskApprovalChanged`](crate::error::Error::DiskApprovalChanged), so
//! the DaemonSet restarts the container and storage is planned again. The
//! node's cache contents are dropped.

use crate::control::managed::ClusterControl;
use crate::error::{Error, Result};
use crate::hardware::{Context, WORKING_BYTES, Walk};
use racer_cluster_control::disks::DiskState;
use std::os::unix::fs::FileTypeExt;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Weak, mpsc};
use std::time::{Duration, Instant};

/// Settling time for approval changes before restarting.
pub(super) const DEBOUNCE: Duration = Duration::from_secs(10);

/// Interval between sysfs rescans for the Pod report.
const RESCAN: Duration = Duration::from_secs(60);

/// Longest teardown waits for the monitor thread. A thread still blocked in
/// sysfs I/O after this is detached; it holds only a weak control reference.
const JOIN_DEADLINE: Duration = Duration::from_secs(2);

/// Interval between approval checks.
const TICK: Duration = Duration::from_secs(1);

/// Set once a restart has been requested; workers stop with
/// [`Error::DiskApprovalChanged`].
static RESTART_REQUESTED: AtomicBool = AtomicBool::new(false);

/// Whether the monitor has requested a restart.
pub(super) fn restart_requested() -> bool {
    RESTART_REQUESTED.load(Ordering::Relaxed)
}

/// Whether two approved disk lists name the same set of IDs.
fn same_set(a: &[String], b: &[String]) -> bool {
    crate::control::same_disks(a, b)
}

/// Debounces approval changes against the startup set.
pub(super) struct Debounce {
    startup: Vec<String>,
    pending: Option<(Vec<String>, Instant)>,
}

impl Debounce {
    pub(super) fn new(startup: Vec<String>) -> Self {
        Self {
            startup,
            pending: None,
        }
    }

    /// Observe the latest approval at `now`; true once a differing set has
    /// been stable for [`DEBOUNCE`].
    pub(super) fn observe(&mut self, now: Instant, latest: Option<&[String]>) -> bool {
        let Some(latest) = latest else {
            return false;
        };
        if same_set(latest, &self.startup) {
            self.pending = None;
            return false;
        }
        match &self.pending {
            Some((pending, since)) if same_set(pending, latest) => {
                now.saturating_duration_since(*since) >= DEBOUNCE
            }
            _ => {
                self.pending = Some((latest.to_vec(), now));
                false
            }
        }
    }
}

/// Revision to report for `latest`: `Some` only when its disk set is the one
/// applied at startup. A changed set keeps the last reported revision.
fn applied_revision(
    debounce: &Debounce,
    latest: Option<racer_cluster_control::disks::Approval>,
) -> Option<Option<String>> {
    latest
        .filter(|a| same_set(&a.disks, &debounce.startup))
        .map(|a| a.revision)
}

/// Approved IDs missing at startup that now resolve to a block device.
fn appeared(context: &Context, device_directory: &Path) -> Option<String> {
    context
        .approvals
        .iter()
        .filter(|a| a.state == DiskState::Missing)
        .find(|a| {
            std::fs::metadata(device_directory.join("disk/by-id").join(&a.id))
                .is_ok_and(|m| m.file_type().is_block_device())
        })
        .map(|a| a.id.clone())
}

/// Running monitor; dropping it stops the thread and waits up to
/// [`JOIN_DEADLINE`] for it to exit.
pub(super) struct Monitor {
    stop: Arc<AtomicBool>,
    thread: Option<std::thread::JoinHandle<()>>,
    exited: mpsc::Receiver<()>,
}

impl Monitor {
    pub(super) fn start(
        control: Arc<dyn ClusterControl>,
        context: Arc<Context>,
        device_directory: PathBuf,
        startup: Vec<String>,
    ) -> Result<Self> {
        let stop = Arc::new(AtomicBool::new(false));
        let flag = stop.clone();
        // The thread holds only a weak reference, so a detached thread
        // never keeps embedded control alive after teardown.
        let control = Arc::downgrade(&control);
        let (done, exited) = mpsc::channel::<()>();
        let thread = std::thread::Builder::new()
            .name("racer-disks".into())
            .spawn(move || {
                // Dropped on exit, including unwinding, to signal teardown.
                let _done = done;
                run(&control, &context, &device_directory, startup, &flag);
            })
            .map_err(|_| Error::Unavailable)?;
        Ok(Self {
            stop,
            thread: Some(thread),
            exited,
        })
    }
}

impl Drop for Monitor {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Relaxed);
        if let Some(thread) = self.thread.take() {
            thread.thread().unpark();
            match self.exited.recv_timeout(JOIN_DEADLINE) {
                Err(mpsc::RecvTimeoutError::Timeout) => {
                    eprintln!("racer: disk monitor did not stop in time; detaching it");
                }
                _ => {
                    let _ = thread.join();
                }
            }
        }
    }
}

fn run(
    control: &Weak<dyn ClusterControl>,
    context: &Context,
    device_directory: &Path,
    startup: Vec<String>,
    stop: &Arc<AtomicBool>,
) {
    let mut debounce = Debounce::new(startup);
    let mut next_scan = Instant::now();
    while !stop.load(Ordering::Relaxed) {
        let now = Instant::now();
        if now >= next_scan {
            next_scan = now + RESCAN;
            let disks = scan(context, device_directory, stop);
            if stop.load(Ordering::Relaxed) {
                return;
            }
            let Some(control) = control.upgrade() else {
                return;
            };
            control.report_disks(disks);
            if let Some(id) = appeared(context, device_directory) {
                eprintln!("racer: approved disk {id:?} appeared; restarting to claim it");
                RESTART_REQUESTED.store(true, Ordering::Relaxed);
                return;
            }
        }
        let Some(control) = control.upgrade() else {
            return;
        };
        let latest = control.approval();
        let disks = latest.as_ref().map(|a| a.disks.as_slice());
        if debounce.observe(now, disks) {
            eprintln!(
                "racer: approved disks changed to {:?}; restarting to apply (this node's cache is dropped)",
                latest.map(|a| a.disks).unwrap_or_default()
            );
            RESTART_REQUESTED.store(true, Ordering::Relaxed);
            return;
        }
        if let Some(revision) = applied_revision(&debounce, latest) {
            control.report_disks_revision(revision);
        }
        drop(control);
        std::thread::park_timeout(TICK);
    }
}

/// Classify every disk; the scan aborts between reads once `stop` is set.
fn scan(
    context: &Context,
    device_directory: &Path,
    stop: &Arc<AtomicBool>,
) -> Vec<racer_cluster_control::disks::DiskStatus> {
    let walk = Walk::new(stop.clone(), WORKING_BYTES);
    statuses(&walk, Path::new("/sys"), device_directory, context)
}

/// Per-disk status from one bounded scan. When the scan exceeds its working
/// budget, for example on a host with many loop devices, the optional
/// inventory is dropped but approved disks still report the outcome recorded
/// at startup, so a writer waiting on a claim or rejection still sees it.
fn statuses(
    walk: &Walk,
    sys_root: &Path,
    device_directory: &Path,
    context: &Context,
) -> Vec<racer_cluster_control::disks::DiskStatus> {
    match crate::hardware::discover(walk, sys_root, device_directory) {
        Ok(found) => {
            let mounts = std::fs::read_to_string(
                context
                    .mountinfo
                    .as_deref()
                    .unwrap_or(Path::new("/proc/self/mountinfo")),
            )
            .unwrap_or_default();
            crate::hardware::classify_disks(&found.block_devices, &mounts, context)
        }
        Err(abort) => {
            eprintln!("racer: disk status scan stopped: {abort:?}; reporting approved disks only");
            crate::hardware::classify_disks(&[], "", context)
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ids(v: &[&str]) -> Vec<String> {
        v.iter().map(|s| (*s).to_owned()).collect()
    }

    #[test]
    fn approval_changes_restart_only_after_a_stable_debounce() {
        let start = Instant::now();
        let at = |s: u64| start + Duration::from_secs(s);
        let mut d = Debounce::new(ids(&["a", "b"]));
        // Unknown and reordered sets never restart.
        assert!(!d.observe(at(0), None));
        assert!(!d.observe(at(0), Some(&ids(&["b", "a"]))));
        assert!(!d.observe(at(100), Some(&ids(&["b", "a", "a"]))));
        // A change waits for the debounce.
        assert!(!d.observe(at(100), Some(&ids(&["a"]))));
        assert!(!d.observe(at(109), Some(&ids(&["a"]))));
        // A newer value restarts the timer; the latest value wins.
        assert!(!d.observe(at(109), Some(&ids(&["a", "c"]))));
        assert!(!d.observe(at(118), Some(&ids(&["c", "a"]))));
        assert!(d.observe(at(119), Some(&ids(&["a", "c"]))));
        // Reverting before the debounce cancels the change.
        let mut d = Debounce::new(ids(&["a"]));
        assert!(!d.observe(at(0), Some(&ids(&[]))));
        assert!(!d.observe(at(5), Some(&ids(&["a"]))));
        assert!(!d.observe(at(20), Some(&ids(&["a"]))));
        assert!(!d.observe(at(20), Some(&ids(&[]))));
        assert!(d.observe(at(30), Some(&ids(&[]))));
    }

    #[test]
    fn revisions_are_echoed_only_for_the_applied_set() {
        use racer_cluster_control::disks::Approval;
        let d = Debounce::new(ids(&["a", "b"]));
        let approval = |v: &[&str], r: Option<&str>| Approval {
            disks: ids(v),
            revision: r.map(str::to_owned),
        };
        assert_eq!(applied_revision(&d, None), None);
        assert_eq!(
            applied_revision(&d, Some(approval(&["b", "a"], Some("r2")))),
            Some(Some("r2".into()))
        );
        assert_eq!(
            applied_revision(&d, Some(approval(&["a", "b"], None))),
            Some(None)
        );
        assert_eq!(
            applied_revision(&d, Some(approval(&["a"], Some("r3")))),
            None
        );
    }

    #[test]
    fn missing_approvals_that_appear_are_detected() {
        let f = super::super::devices::tests::Fixture::new();
        std::fs::create_dir_all(f.0.join("disk/by-id")).unwrap();
        let context = Context {
            approvals: vec![crate::hardware::Approved {
                id: "wwn-late".into(),
                dev: None,
                state: DiskState::Missing,
                reason: None,
            }],
            ..Default::default()
        };
        assert_eq!(appeared(&context, &f.0), None);
        // A regular file is not a disk.
        std::fs::write(f.0.join("disk/by-id/wwn-late"), "").unwrap();
        assert_eq!(appeared(&context, &f.0), None);
        std::fs::remove_file(f.0.join("disk/by-id/wwn-late")).unwrap();
        // Any block device node works as a stand-in for the attached disk.
        if let Some(block) = std::fs::read_dir("/dev")
            .unwrap()
            .flatten()
            .find(|e| e.file_type().is_ok_and(|t| t.is_block_device()))
        {
            std::os::unix::fs::symlink(block.path(), f.0.join("disk/by-id/wwn-late")).unwrap();
            assert_eq!(appeared(&context, &f.0).as_deref(), Some("wwn-late"));
        }
    }

    #[test]
    fn exhausted_scan_still_reports_approved_outcomes() {
        let f = super::super::devices::tests::Fixture::new();
        let context = Context {
            approvals: vec![
                crate::hardware::Approved {
                    id: "wwn-claimed".into(),
                    dev: Some("259:0".into()),
                    state: DiskState::Claimed,
                    reason: None,
                },
                crate::hardware::Approved {
                    id: "wwn-late".into(),
                    dev: None,
                    state: DiskState::Missing,
                    reason: None,
                },
            ],
            ..Default::default()
        };
        // A zero budget aborts discovery before any device is read.
        let walk = Walk::new(Arc::default(), 0);
        let disks = statuses(&walk, &f.0.join("sys"), &f.0, &context);
        let got: Vec<_> = disks.iter().map(|d| (d.id.as_str(), d.state)).collect();
        assert_eq!(
            got,
            [
                ("wwn-claimed", DiskState::Claimed),
                ("wwn-late", DiskState::Missing)
            ]
        );
        // Without approvals the fallback is empty rather than an error.
        let walk = Walk::new(Arc::default(), 0);
        assert!(statuses(&walk, &f.0.join("sys"), &f.0, &Context::default()).is_empty());
    }

    /// Control whose disk report blocks until released, standing in for a
    /// scan stalled in sysfs I/O.
    struct Stalled {
        entered: std::sync::Mutex<mpsc::Sender<()>>,
        release: std::sync::Mutex<mpsc::Receiver<()>>,
        block: bool,
    }

    impl ClusterControl for Stalled {
        fn bootstrap(
            &self,
            _: Duration,
        ) -> std::result::Result<
            crate::control::managed::Bootstrapped,
            crate::control::managed::BootstrapError,
        > {
            Err(crate::control::managed::BootstrapError::Fatal)
        }

        fn endpoints(&self) -> wire_codec::DiscoveredEndpoints {
            Default::default()
        }

        fn report(&self, _: u32, _: Vec<racer_control_wire::RailMapping>) {}

        fn report_disks(&self, _: Vec<racer_cluster_control::disks::DiskStatus>) {
            let _ = self.entered.lock().unwrap().send(());
            if self.block {
                let _ = self.release.lock().unwrap().recv();
            }
        }
    }

    fn stalled(block: bool) -> (Arc<Stalled>, mpsc::Receiver<()>, mpsc::Sender<()>) {
        let (entered, on_enter) = mpsc::channel();
        let (release, on_release) = mpsc::channel();
        let control = Arc::new(Stalled {
            entered: std::sync::Mutex::new(entered),
            release: std::sync::Mutex::new(on_release),
            block,
        });
        (control, on_enter, release)
    }

    #[test]
    fn teardown_joins_and_releases_control() {
        let f = super::super::devices::tests::Fixture::new();
        let (control, entered, _release) = stalled(false);
        let monitor = Monitor::start(
            control.clone(),
            Arc::new(Context::default()),
            f.0.clone(),
            Vec::new(),
        )
        .unwrap();
        entered.recv_timeout(Duration::from_secs(60)).unwrap();
        drop(monitor);
        assert_eq!(Arc::strong_count(&control), 1);
    }

    #[test]
    fn teardown_detaches_a_stalled_thread_after_the_deadline() {
        let f = super::super::devices::tests::Fixture::new();
        let (control, entered, release) = stalled(true);
        let monitor = Monitor::start(
            control.clone(),
            Arc::new(Context::default()),
            f.0.clone(),
            Vec::new(),
        )
        .unwrap();
        entered.recv_timeout(Duration::from_secs(60)).unwrap();
        let start = Instant::now();
        drop(monitor);
        let waited = start.elapsed();
        assert!(waited >= JOIN_DEADLINE, "returned early: {waited:?}");
        assert!(waited < JOIN_DEADLINE * 10, "blocked: {waited:?}");
        // Once the stall clears, the detached thread exits without
        // touching control again and drops its temporary reference.
        release.send(()).unwrap();
        let deadline = Instant::now() + Duration::from_secs(60);
        while Arc::strong_count(&control) > 1 && Instant::now() < deadline {
            std::thread::sleep(Duration::from_millis(10));
        }
        assert_eq!(Arc::strong_count(&control), 1);
        assert!(!restart_requested());
    }
}
