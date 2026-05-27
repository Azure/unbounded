// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Open/close lifecycle for `[[disks]]` entries from the TOML config.
//!
//! The supervisor tracks which disk paths are currently "open" and
//! reconciles that set against the desired list each time the config
//! changes. Production opens are backed by [`UringBlockDevice`]; tests
//! plug in a [`MockDiskTarget`].
//!
//! This module deliberately does not wire devices into the data path -
//! shards still do not consume the opened handles. Phase 6 will hand
//! the handles to the bufferpool / `LocalStorage`.

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc;
use std::thread::{self, JoinHandle};
use std::time::Duration;

use crate::config::schema::DiskSpec;
#[cfg(target_os = "linux")]
use crate::storage::blockdev::{UringBlockDevice, UringConfig};

mod shard_view;
mod topology;

pub use shard_view::LiveShardLocalStore;
pub use topology::{LiveDiskTopology, LocalStorageSnapshot};

/// Abstraction over "open a disk and start its progress loop". The
/// production implementation is [`UringDiskTarget`]; tests provide a
/// mock so reconciliation logic can be exercised without touching the
/// filesystem or io_uring.
pub trait DiskTarget: Send + Sync + 'static {
    /// The per-disk handle returned by [`Self::open`]. The handle is
    /// expected to own any background progress thread it spawned and
    /// to join it from its own `Drop`.
    type Handle: Send + 'static;

    /// Open the disk described by `spec`. `cpu_hint` is a best-effort
    /// pin target drawn from [`Plan`]'s NVMe slot list. Implementations
    /// may ignore the hint.
    fn open(&self, spec: &DiskSpec, cpu_hint: Option<usize>) -> Result<Self::Handle, DiskError>;
}

/// Reasons a disk open can fail. Kept simple by design: every variant
/// carries a human-readable string so callers can log without pulling
/// in an error crate.
#[derive(Debug)]
pub enum DiskError {
    /// The underlying [`UringBlockDevice::open`] call failed.
    Open(String),
}

impl std::fmt::Display for DiskError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            DiskError::Open(s) => write!(f, "open: {s}"),
        }
    }
}

impl std::error::Error for DiskError {}

/// Summary of one [`DiskRegistry::reconcile`] call. `added` and
/// `removed` count successful transitions; `failures` carries the path
/// and reason for every spec whose open failed.
#[derive(Debug, Default)]
pub struct DiskReport {
    pub added: usize,
    pub removed: usize,
    pub failures: Vec<(PathBuf, String)>,
}

/// Tracks the set of currently-open disks and reconciles it against a
/// desired list each time the config changes. Generic over a
/// [`DiskTarget`] so production and tests share the algorithm.
pub struct DiskRegistry<T: DiskTarget> {
    target: T,
    handles: HashMap<PathBuf, T::Handle>,
    applied: HashMap<PathBuf, DiskSpec>,
    plan_slots: Vec<usize>,
    next_slot: usize,
}

impl<T: DiskTarget> DiskRegistry<T> {
    /// Build an empty registry. `plan_slots` is a best-effort list of
    /// CPU ids drawn from the topology plan's NVMe placements; opens
    /// consume the list round-robin to pin their progress threads.
    pub fn new(target: T, plan_slots: Vec<usize>) -> Self {
        Self {
            target,
            handles: HashMap::new(),
            applied: HashMap::new(),
            plan_slots,
            next_slot: 0,
        }
    }

    /// Reconcile the open set against `desired`.
    ///
    /// Paths missing from `desired` are dropped (closing the handle).
    /// Paths present in `desired` but not currently open are opened.
    /// Paths whose [`DiskSpec`] drifted (`kind`/`numa`/`queue_depth`
    /// changed) are treated as a remove followed by an add. Partial
    /// failures during opens are reported but do not abort the
    /// reconcile.
    pub fn reconcile(&mut self, desired: &[DiskSpec]) -> DiskReport {
        let mut report = DiskReport::default();

        let mut desired_paths: HashMap<&Path, &DiskSpec> = HashMap::new();
        for spec in desired {
            desired_paths.insert(spec.path.as_path(), spec);
        }

        let to_remove: Vec<PathBuf> = self
            .handles
            .keys()
            .filter(|p| match desired_paths.get(p.as_path()) {
                None => true,
                Some(spec) => specs_drifted(self.applied.get(*p), spec),
            })
            .cloned()
            .collect();
        for path in to_remove {
            if self.handles.remove(&path).is_some() {
                self.applied.remove(&path);
                report.removed += 1;
            }
        }

        for spec in desired {
            if self.handles.contains_key(&spec.path) {
                continue;
            }
            let hint = self.next_cpu_hint();
            match self.target.open(spec, hint) {
                Ok(handle) => {
                    self.handles.insert(spec.path.clone(), handle);
                    self.applied.insert(spec.path.clone(), spec.clone());
                    report.added += 1;
                }
                Err(e) => {
                    report.failures.push((spec.path.clone(), e.to_string()));
                }
            }
        }

        report
    }

    /// Close every open handle. Called at shutdown so progress
    /// threads join deterministically before the rest of the daemon
    /// tears down.
    pub fn drain(mut self) {
        self.handles.clear();
        self.applied.clear();
    }

    /// Paths whose handle is currently open. Order is unspecified.
    /// Production handles (`UringDiskHandle`) do not expose their
    /// `Arc<UringBlockDevice>` because the device is `!Send` and
    /// lives on the per-disk progress thread; engines therefore
    /// cannot be constructed from the caller's thread here. See the
    /// `disk_supervisor::topology` module doc for the open-path
    /// constraint.
    pub fn current_paths(&self) -> Vec<PathBuf> {
        self.handles.keys().cloned().collect()
    }

    fn next_cpu_hint(&mut self) -> Option<usize> {
        if self.plan_slots.is_empty() {
            return None;
        }
        let cpu = self.plan_slots[self.next_slot % self.plan_slots.len()];
        self.next_slot = self.next_slot.wrapping_add(1);
        Some(cpu)
    }
}

fn specs_drifted(prev: Option<&DiskSpec>, next: &DiskSpec) -> bool {
    match prev {
        None => false,
        Some(p) => p.kind != next.kind || p.numa != next.numa || p.queue_depth != next.queue_depth,
    }
}

/// Production [`DiskTarget`] that opens a real [`UringBlockDevice`]
/// and spawns a dedicated thread to drive its `progress()` loop.
pub struct UringDiskTarget;

/// Handle owning the progress thread that drives a [`UringBlockDevice`]
/// opened on that thread. Dropping the handle sets the stop flag and
/// joins the thread (which drops the device on its own stack since
/// `UringBlockDevice` is `!Send`).
pub struct UringDiskHandle {
    stop: Arc<AtomicBool>,
    join: Option<JoinHandle<()>>,
}

impl Drop for UringDiskHandle {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Release);
        if let Some(j) = self.join.take() {
            let _ = j.join();
        }
    }
}

impl DiskTarget for UringDiskTarget {
    type Handle = UringDiskHandle;

    fn open(&self, spec: &DiskSpec, _cpu_hint: Option<usize>) -> Result<Self::Handle, DiskError> {
        // `UringBlockDevice` is `!Send`, so it can only ever live on
        // the progress thread. Open it there and signal success back
        // via a oneshot channel so this call still surfaces open
        // errors synchronously.
        // TODO(phase6): pin the progress thread to `_cpu_hint` and
        // share the opened device with the shard's bufferpool via a
        // channel-based command surface.
        let stop = Arc::new(AtomicBool::new(false));
        let stop_thr = stop.clone();
        let path = spec.path.clone();
        let mut cfg = UringConfig::default();
        if let Some(qd) = spec.queue_depth {
            cfg.queue_depth = qd;
        }
        let (ready_tx, ready_rx) = mpsc::sync_channel::<Result<(), String>>(1);
        let join = thread::Builder::new()
            .name(format!("ub-disk-{}", path.display()))
            .spawn(move || {
                let dev = match UringBlockDevice::open(&path, cfg) {
                    Ok(d) => {
                        let _ = ready_tx.send(Ok(()));
                        d
                    }
                    Err(e) => {
                        let _ = ready_tx.send(Err(format!("{e:?}")));
                        return;
                    }
                };
                while !stop_thr.load(Ordering::Acquire) {
                    let _ = dev.progress();
                    thread::sleep(Duration::from_micros(100));
                }
                drop(dev);
            })
            .map_err(|e| DiskError::Open(format!("spawn progress thread: {e}")))?;
        match ready_rx.recv() {
            Ok(Ok(())) => Ok(UringDiskHandle {
                stop,
                join: Some(join),
            }),
            Ok(Err(msg)) => {
                let _ = join.join();
                Err(DiskError::Open(msg))
            }
            Err(_) => {
                let _ = join.join();
                Err(DiskError::Open("progress thread exited without status".into()))
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::schema::DiskKind;
    use std::collections::HashSet;
    use std::sync::Mutex;

    struct MockDiskTarget {
        state: Arc<Mutex<MockState>>,
    }

    #[derive(Default)]
    struct MockState {
        opened: HashSet<PathBuf>,
        fail_on: HashSet<PathBuf>,
        open_calls: usize,
    }

    struct MockHandle {
        path: PathBuf,
        state: Arc<Mutex<MockState>>,
    }

    impl Drop for MockHandle {
        fn drop(&mut self) {
            self.state.lock().unwrap().opened.remove(&self.path);
        }
    }

    impl MockDiskTarget {
        fn new() -> (Self, Arc<Mutex<MockState>>) {
            let state = Arc::new(Mutex::new(MockState::default()));
            (
                Self {
                    state: state.clone(),
                },
                state,
            )
        }
    }

    impl DiskTarget for MockDiskTarget {
        type Handle = MockHandle;

        fn open(&self, spec: &DiskSpec, _cpu_hint: Option<usize>) -> Result<MockHandle, DiskError> {
            let mut s = self.state.lock().unwrap();
            s.open_calls += 1;
            if s.fail_on.contains(&spec.path) {
                return Err(DiskError::Open("injected".into()));
            }
            s.opened.insert(spec.path.clone());
            Ok(MockHandle {
                path: spec.path.clone(),
                state: self.state.clone(),
            })
        }
    }

    fn spec(path: &str, qd: Option<u32>) -> DiskSpec {
        DiskSpec {
            path: PathBuf::from(path),
            kind: DiskKind::Nvme,
            numa: None,
            queue_depth: qd,
            page_size_bytes: None,
            bypass_admission: false,
            skip_recovery_scan_if_no_meta: false,
        }
    }

    #[test]
    fn empty_to_two_adds_both() {
        let (target, state) = MockDiskTarget::new();
        let mut reg = DiskRegistry::new(target, Vec::new());
        let report = reg.reconcile(&[spec("/a", None), spec("/b", None)]);
        assert_eq!(report.added, 2);
        assert_eq!(report.removed, 0);
        assert!(report.failures.is_empty());
        assert_eq!(state.lock().unwrap().opened.len(), 2);
    }

    #[test]
    fn swap_adds_and_removes() {
        let (target, state) = MockDiskTarget::new();
        let mut reg = DiskRegistry::new(target, Vec::new());
        reg.reconcile(&[spec("/a", None), spec("/b", None)]);
        let report = reg.reconcile(&[spec("/b", None), spec("/c", None)]);
        assert_eq!(report.added, 1);
        assert_eq!(report.removed, 1);
        let opened = &state.lock().unwrap().opened;
        assert!(opened.contains(&PathBuf::from("/b")));
        assert!(opened.contains(&PathBuf::from("/c")));
        assert!(!opened.contains(&PathBuf::from("/a")));
    }

    #[test]
    fn same_desired_twice_is_idempotent() {
        let (target, state) = MockDiskTarget::new();
        let mut reg = DiskRegistry::new(target, Vec::new());
        let desired = [spec("/a", None), spec("/b", None)];
        reg.reconcile(&desired);
        let calls_after_first = state.lock().unwrap().open_calls;
        let report = reg.reconcile(&desired);
        assert_eq!(report.added, 0);
        assert_eq!(report.removed, 0);
        assert_eq!(state.lock().unwrap().open_calls, calls_after_first);
    }

    #[test]
    fn queue_depth_drift_triggers_remove_add() {
        let (target, _state) = MockDiskTarget::new();
        let mut reg = DiskRegistry::new(target, Vec::new());
        reg.reconcile(&[spec("/a", Some(8))]);
        let report = reg.reconcile(&[spec("/a", Some(32))]);
        assert_eq!(report.added, 1);
        assert_eq!(report.removed, 1);
        assert_eq!(reg.applied[&PathBuf::from("/a")].queue_depth, Some(32));
    }

    #[test]
    fn failure_injection_does_not_block_others() {
        let (target, state) = MockDiskTarget::new();
        state
            .lock()
            .unwrap()
            .fail_on
            .insert(PathBuf::from("/bad"));
        let mut reg = DiskRegistry::new(target, Vec::new());
        let report = reg.reconcile(&[spec("/bad", None), spec("/good", None)]);
        assert_eq!(report.added, 1);
        assert_eq!(report.failures.len(), 1);
        assert_eq!(report.failures[0].0, PathBuf::from("/bad"));
        assert!(state.lock().unwrap().opened.contains(&PathBuf::from("/good")));
    }

    #[test]
    fn round_robin_cpu_hint() {
        struct CpuRecorder {
            hints: Arc<Mutex<Vec<Option<usize>>>>,
        }
        struct H;
        impl DiskTarget for CpuRecorder {
            type Handle = H;
            fn open(&self, _spec: &DiskSpec, hint: Option<usize>) -> Result<H, DiskError> {
                self.hints.lock().unwrap().push(hint);
                Ok(H)
            }
        }
        let hints = Arc::new(Mutex::new(Vec::new()));
        let mut reg = DiskRegistry::new(
            CpuRecorder {
                hints: hints.clone(),
            },
            vec![3, 5],
        );
        reg.reconcile(&[spec("/a", None), spec("/b", None), spec("/c", None)]);
        let got = hints.lock().unwrap().clone();
        assert_eq!(got, vec![Some(3), Some(5), Some(3)]);
    }

    #[test]
    fn drain_closes_all() {
        let (target, state) = MockDiskTarget::new();
        let mut reg = DiskRegistry::new(target, Vec::new());
        reg.reconcile(&[spec("/a", None), spec("/b", None)]);
        assert_eq!(state.lock().unwrap().opened.len(), 2);
        reg.drain();
        assert!(state.lock().unwrap().opened.is_empty());
    }
}
