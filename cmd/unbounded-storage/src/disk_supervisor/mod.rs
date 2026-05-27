// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Open/close lifecycle for `[[disks]]` entries from the TOML config.
//!
//! The supervisor tracks which disk paths are currently "open" and
//! reconciles that set against the desired list each time the config
//! changes. Production opens are backed by [`UringBlockDevice`] via
//! [`UringDiskTarget`]; tests plug in a mock target.
//!
//! Each successful open returns both a per-disk handle (whose `Drop`
//! tears down the disk thread) and an `Arc<StorageEngine<...>>`
//! published into [`LiveDiskTopology`] by the caller. See the
//! sub-module docs for the device-side details.

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::Arc;

use crate::config::schema::DiskSpec;
use crate::storage::StorageEngine;
use crate::storage::blockdev::BlockDevice;

mod shard_view;
mod topology;
#[cfg(target_os = "linux")]
mod uring;

pub use shard_view::LiveShardLocalStore;
pub use topology::{LiveDiskTopology, LocalStorageSnapshot};
#[cfg(target_os = "linux")]
pub use uring::{UringDiskHandle, UringDiskTarget};

/// Abstraction over "open a disk, start its progress loop, hand back
/// an engine". Production is [`UringDiskTarget`]; tests provide a
/// mock so reconciliation logic can be exercised without touching
/// real I/O.
pub trait DiskTarget: Send + Sync + 'static {
    /// The block-device flavor produced by this target. Production
    /// targets use a `Send + Sync` proxy so the resulting engine can
    /// be published to other threads even though the underlying
    /// device is `!Send`. The trait itself does not require
    /// `Self::Device: Send + Sync`; test mocks may use `!Send`
    /// devices and confine their use to a single thread.
    type Device: BlockDevice + 'static;

    /// The per-disk handle returned by [`Self::open`]. The handle is
    /// expected to own any background progress thread it spawned and
    /// to join it from its own `Drop`.
    type Handle: Send + 'static;

    /// Open the disk described by `spec`. On success the returned
    /// engine `Arc` is suitable for publication through
    /// [`LiveDiskTopology::apply_engines`]; the caller retains
    /// ownership of the handle so it can drive shutdown deterministi-
    /// cally. `cpu_hint` is a best-effort pin target drawn from
    /// the topology plan's NVMe slot list; implementations may
    /// ignore it.
    fn open(
        &self,
        spec: &DiskSpec,
        cpu_hint: Option<usize>,
    ) -> Result<(Self::Handle, Arc<StorageEngine<Self::Device>>), DiskError>;
}

/// Reasons a disk open can fail. Kept simple by design: every variant
/// carries a human-readable string so callers can log without pulling
/// in an error crate.
#[derive(Debug)]
pub enum DiskError {
    /// The underlying [`UringBlockDevice::open`] call or engine open
    /// failed.
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

/// Maps a disk's NUMA preference to a best-effort CPU pin target.
/// Production builds this from [`crate::topology::Host`]; tests
/// pass a closure (or a no-op) so the registry can be exercised
/// without a real `Host`. A blanket impl below makes any
/// `Fn(Option<u16>) -> Option<usize>` usable directly.
pub trait CpuPlacer: Send + Sync + 'static {
    fn cpu_for_numa(&self, numa: Option<u16>) -> Option<usize>;
}

impl<F> CpuPlacer for F
where
    F: Fn(Option<u16>) -> Option<usize> + Send + Sync + 'static,
{
    fn cpu_for_numa(&self, numa: Option<u16>) -> Option<usize> {
        (self)(numa)
    }
}

/// Tracks the set of currently-open disks and reconciles it against a
/// desired list each time the config changes. Generic over a
/// [`DiskTarget`] so production and tests share the algorithm.
pub struct DiskRegistry<T: DiskTarget> {
    target: T,
    placer: Box<dyn CpuPlacer>,
    handles: HashMap<PathBuf, T::Handle>,
    engines: HashMap<PathBuf, Arc<StorageEngine<T::Device>>>,
    applied: HashMap<PathBuf, DiskSpec>,
}

impl<T: DiskTarget> DiskRegistry<T> {
    /// Build an empty registry. `placer` resolves each disk's `numa`
    /// preference to a CPU id used as a best-effort pin target when
    /// the disk is opened.
    pub fn new<P: CpuPlacer>(target: T, placer: P) -> Self {
        Self {
            target,
            placer: Box::new(placer),
            handles: HashMap::new(),
            engines: HashMap::new(),
            applied: HashMap::new(),
        }
    }

    /// Reconcile the open set against `desired`.
    ///
    /// Paths missing from `desired` are dropped (closing the handle).
    /// Paths present in `desired` but not currently open are opened.
    /// Paths whose [`DiskSpec`] drifted in any field that affects how
    /// the disk is opened (kind / numa / queue_depth / page_size_bytes
    /// / bypass_admission / skip_recovery_scan_if_no_meta) are treated
    /// as a remove followed by an add. Partial failures during opens
    /// are reported but do not abort the reconcile.
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
                self.engines.remove(&path);
                self.applied.remove(&path);
                report.removed += 1;
            }
        }

        for spec in desired {
            if self.handles.contains_key(&spec.path) {
                continue;
            }
            let hint = self.placer.cpu_for_numa(spec.numa);
            match self.target.open(spec, hint) {
                Ok((handle, engine)) => {
                    self.handles.insert(spec.path.clone(), handle);
                    self.engines.insert(spec.path.clone(), engine);
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

    /// Snapshot of currently-open engines suitable for handing to
    /// [`LiveDiskTopology::apply_engines`]. Returned in path-sorted
    /// order so downstream hashing is stable across reconciles.
    pub fn engines_snapshot(&self) -> Vec<(PathBuf, Arc<StorageEngine<T::Device>>)> {
        let mut out: Vec<(PathBuf, Arc<StorageEngine<T::Device>>)> = self
            .engines
            .iter()
            .map(|(p, e)| (p.clone(), e.clone()))
            .collect();
        out.sort_by(|a, b| a.0.cmp(&b.0));
        out
    }

    /// Close every open handle. Engines are dropped first so the
    /// registry's `Arc` clones are gone before the handles signal
    /// shutdown to the disk threads; that lets the disk threads exit
    /// promptly once any outside `Arc<StorageEngine>` clones (held
    /// by the topology / shards) are also released.
    pub fn drain(mut self) {
        self.engines.clear();
        self.handles.clear();
        self.applied.clear();
    }

    /// Paths whose handle is currently open. Order is unspecified.
    pub fn current_paths(&self) -> Vec<PathBuf> {
        self.handles.keys().cloned().collect()
    }
}

fn specs_drifted(prev: Option<&DiskSpec>, next: &DiskSpec) -> bool {
    match prev {
        None => false,
        Some(p) => {
            p.kind != next.kind
                || p.numa != next.numa
                || p.queue_depth != next.queue_depth
                || p.page_size_bytes != next.page_size_bytes
                || p.bypass_admission != next.bypass_admission
                || p.skip_recovery_scan_if_no_meta != next.skip_recovery_scan_if_no_meta
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::schema::DiskKind;
    use crate::storage::EngineConfig;
    use crate::storage::blockdev::{MockDevice, MockDeviceConfig};
    use std::collections::HashSet;
    use std::future::Future;
    use std::pin::pin;
    use std::sync::Mutex;
    use std::task::{Context, Poll, RawWaker, RawWakerVTable, Waker};

    fn noop_waker() -> Waker {
        fn raw() -> RawWaker {
            RawWaker::new(std::ptr::null(), &VTABLE)
        }
        static VTABLE: RawWakerVTable = RawWakerVTable::new(|_| raw(), |_| {}, |_| {}, |_| {});
        unsafe { Waker::from_raw(raw()) }
    }

    fn block_on<F: Future>(f: F) -> F::Output {
        let w = noop_waker();
        let mut cx = Context::from_waker(&w);
        let mut f = pin!(f);
        let mut spins = 0u64;
        loop {
            match f.as_mut().poll(&mut cx) {
                Poll::Ready(v) => return v,
                Poll::Pending => {
                    spins += 1;
                    assert!(spins < 1_000_000, "stuck");
                }
            }
        }
    }

    fn mock_engine() -> Arc<StorageEngine<MockDevice>> {
        let device = Arc::new(MockDevice::new(MockDeviceConfig {
            page_size: 4096,
            capacity_pages: 64,
            ..Default::default()
        }));
        let mut cfg = EngineConfig::default();
        cfg.page_size_bytes = 4096;
        cfg.btree_page_bytes = 4096;
        Arc::new(block_on(StorageEngine::open(device, cfg)).unwrap())
    }

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
        type Device = MockDevice;
        type Handle = MockHandle;

        fn open(
            &self,
            spec: &DiskSpec,
            _cpu_hint: Option<usize>,
        ) -> Result<(MockHandle, Arc<StorageEngine<MockDevice>>), DiskError> {
            let mut s = self.state.lock().unwrap();
            s.open_calls += 1;
            if s.fail_on.contains(&spec.path) {
                return Err(DiskError::Open("injected".into()));
            }
            s.opened.insert(spec.path.clone());
            drop(s);
            Ok((
                MockHandle {
                    path: spec.path.clone(),
                    state: self.state.clone(),
                },
                mock_engine(),
            ))
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

    fn no_placer() -> impl CpuPlacer {
        |_: Option<u16>| -> Option<usize> { None }
    }

    #[test]
    fn empty_to_two_adds_both() {
        let (target, state) = MockDiskTarget::new();
        let mut reg = DiskRegistry::new(target, no_placer());
        let report = reg.reconcile(&[spec("/a", None), spec("/b", None)]);
        assert_eq!(report.added, 2);
        assert_eq!(report.removed, 0);
        assert!(report.failures.is_empty());
        assert_eq!(state.lock().unwrap().opened.len(), 2);
        assert_eq!(reg.engines_snapshot().len(), 2);
    }

    #[test]
    fn swap_adds_and_removes() {
        let (target, state) = MockDiskTarget::new();
        let mut reg = DiskRegistry::new(target, no_placer());
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
        let mut reg = DiskRegistry::new(target, no_placer());
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
        let mut reg = DiskRegistry::new(target, no_placer());
        reg.reconcile(&[spec("/a", Some(8))]);
        let report = reg.reconcile(&[spec("/a", Some(32))]);
        assert_eq!(report.added, 1);
        assert_eq!(report.removed, 1);
        assert_eq!(reg.applied[&PathBuf::from("/a")].queue_depth, Some(32));
    }

    #[test]
    fn engine_field_drift_triggers_remove_add() {
        let (target, _state) = MockDiskTarget::new();
        let mut reg = DiskRegistry::new(target, no_placer());
        reg.reconcile(&[spec("/a", None)]);
        let mut next = spec("/a", None);
        next.bypass_admission = true;
        let report = reg.reconcile(&[next]);
        assert_eq!(report.added, 1);
        assert_eq!(report.removed, 1);
        assert!(reg.applied[&PathBuf::from("/a")].bypass_admission);
    }

    #[test]
    fn failure_injection_does_not_block_others() {
        let (target, state) = MockDiskTarget::new();
        state
            .lock()
            .unwrap()
            .fail_on
            .insert(PathBuf::from("/bad"));
        let mut reg = DiskRegistry::new(target, no_placer());
        let report = reg.reconcile(&[spec("/bad", None), spec("/good", None)]);
        assert_eq!(report.added, 1);
        assert_eq!(report.failures.len(), 1);
        assert_eq!(report.failures[0].0, PathBuf::from("/bad"));
        assert!(state.lock().unwrap().opened.contains(&PathBuf::from("/good")));
        // Failed open must not pollute the engine map.
        assert_eq!(reg.engines_snapshot().len(), 1);
        assert_eq!(reg.engines_snapshot()[0].0, PathBuf::from("/good"));
    }

    #[test]
    fn cpu_hint_per_disk_from_numa() {
        struct CpuRecorder {
            hints: Arc<Mutex<Vec<Option<usize>>>>,
        }
        struct H;
        impl DiskTarget for CpuRecorder {
            type Device = MockDevice;
            type Handle = H;
            fn open(
                &self,
                _spec: &DiskSpec,
                hint: Option<usize>,
            ) -> Result<(H, Arc<StorageEngine<MockDevice>>), DiskError> {
                self.hints.lock().unwrap().push(hint);
                Ok((H, mock_engine()))
            }
        }
        let placer = |numa: Option<u16>| -> Option<usize> {
            match numa {
                Some(0) => Some(7),
                Some(1) => Some(11),
                _ => None,
            }
        };
        let hints = Arc::new(Mutex::new(Vec::new()));
        let mut reg = DiskRegistry::new(
            CpuRecorder {
                hints: hints.clone(),
            },
            placer,
        );
        let mut d0 = spec("/a", None);
        d0.numa = Some(0);
        let mut d1 = spec("/b", None);
        d1.numa = Some(1);
        let d2 = spec("/c", None); // numa: None
        reg.reconcile(&[d0, d1, d2]);
        let got = hints.lock().unwrap().clone();
        assert_eq!(got, vec![Some(7), Some(11), None]);
    }

    #[test]
    fn drain_closes_all() {
        let (target, state) = MockDiskTarget::new();
        let mut reg = DiskRegistry::new(target, no_placer());
        reg.reconcile(&[spec("/a", None), spec("/b", None)]);
        assert_eq!(state.lock().unwrap().opened.len(), 2);
        reg.drain();
        assert!(state.lock().unwrap().opened.is_empty());
    }

    #[test]
    fn engines_snapshot_is_path_sorted() {
        let (target, _state) = MockDiskTarget::new();
        let mut reg = DiskRegistry::new(target, no_placer());
        reg.reconcile(&[spec("/c", None), spec("/a", None), spec("/b", None)]);
        let snap = reg.engines_snapshot();
        let paths: Vec<PathBuf> = snap.into_iter().map(|(p, _)| p).collect();
        assert_eq!(
            paths,
            vec![
                PathBuf::from("/a"),
                PathBuf::from("/b"),
                PathBuf::from("/c"),
            ]
        );
    }
}
