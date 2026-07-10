// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Open/close lifecycle for projected cache disks from the TOML config.
//!
//! The supervisor tracks which disk paths are currently "open" and
//! reconciles that set against the desired list each time the config
//! changes. Production opens are backed by a pinned storage core via
//! [`UringDiskTarget`]; tests plug in a mock target.
//!
//! CPU placement for disk storage cores comes from the topology
//! [`CorePlan`](crate::topology::CorePlan): the registry is seeded
//! with one [`DiskCpuSlot`](crate::topology::DiskCpuSlot) per
//! [`StorageCore`](crate::topology::StorageCore) (one per NVMe drive),
//! which are disjoint from the NIC-worker and serving-shard CPUs by
//! construction. On each reconcile a deterministic assignment maps
//! disks to slots, preferring NUMA-local slots; disks beyond the
//! available slot count run unpinned.
//!
//! Each successful open returns both a per-disk handle (whose `Drop`
//! tears down the disk thread) and a [`PageChannel`] published into
//! [`DiskChannelDirectory`] by the caller. The channel ships page
//! operations to the storage core that owns the engine and ring; see
//! the sub-module docs for the device-side details.

use std::collections::HashMap;
use std::path::{Path, PathBuf};

use crate::config::schema::DiskSpec;
use crate::storage::PageChannel;
use crate::topology::DiskCpuSlot;

mod channels;
mod shard_view;
#[cfg(target_os = "linux")]
mod uring;

pub use channels::DiskChannelDirectory;
pub use shard_view::{CacheDirectorySet, ChainLocalStore, LiveShardLocalStore};
#[cfg(target_os = "linux")]
pub use uring::{UringDiskHandle, UringDiskTarget};

/// Abstraction over "open a disk, start its storage core, hand back a
/// page channel". Production is [`UringDiskTarget`]; tests provide a
/// mock so reconciliation logic can be exercised without touching
/// real I/O.
pub trait DiskTarget: Send + Sync + 'static {
    /// The per-disk handle returned by [`Self::open`]. The handle is
    /// expected to own any background storage-core thread it spawned
    /// and to join it from its own `Drop`.
    type Handle: Send + 'static;

    /// Open the disk described by `spec`. On success the returned
    /// [`PageChannel`] is suitable for publication through
    /// [`DiskChannelDirectory::apply_channels`]; the caller retains
    /// ownership of the handle so it can drive shutdown
    /// deterministically. `pin` is a best-effort pin target
    /// drawn from the topology plan's NVMe slot list (carrying both
    /// CPU and NUMA node); implementations may ignore it.
    fn open(
        &self,
        spec: &DiskSpec,
        pin: Option<DiskCpuSlot>,
    ) -> Result<(Self::Handle, PageChannel), DiskError>;
}

/// Reasons a disk open can fail. Kept simple by design: every variant
/// carries a human-readable string so callers can log without pulling
/// in an error crate.
#[derive(Debug)]
pub enum DiskError {
    /// The underlying
    /// [`UringDevice::open`](crate::storage::blockdev::UringDevice::open) call
    /// or engine open failed.
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
    disk_slots: Vec<DiskCpuSlot>,
    entries: HashMap<PathBuf, OpenDiskEntry<T::Handle>>,
}

struct OpenDiskEntry<H> {
    channel: PageChannel,
    spec: DiskSpec,
    placement: Option<DiskCpuSlot>,
    handle: H,
}

impl<H> OpenDiskEntry<H> {
    fn close(self) {
        drop(self.channel);
        drop(self.handle);
    }
}

impl<T: DiskTarget> DiskRegistry<T> {
    /// Build an empty registry. `disk_slots` are the topology plan's
    /// disjoint NVMe CPU slots; the registry assigns them to disks
    /// (NUMA-local first) on each reconcile as best-effort pin
    /// targets. An empty slot list means every disk runs unpinned.
    pub fn new(target: T, disk_slots: Vec<DiskCpuSlot>) -> Self {
        Self {
            target,
            disk_slots,
            entries: HashMap::new(),
        }
    }

    /// Reconcile the open set against `desired`.
    ///
    /// New disks are staged before the live set is changed. If any open fails,
    /// all staged handles are closed and the old set remains intact. Changes to
    /// open-time settings on an existing path require a restart because the old
    /// handle cannot be safely closed before its replacement is known to open.
    pub fn reconcile(&mut self, desired: &[DiskSpec]) -> DiskReport {
        let mut report = DiskReport::default();

        let mut desired_paths: HashMap<&Path, &DiskSpec> = HashMap::new();
        for spec in desired {
            desired_paths.insert(Path::new(disk_path(spec)), spec);
        }

        for (path, entry) in &self.entries {
            if let Some(spec) = desired_paths.get(path.as_path())
                && specs_drifted(Some(&entry.spec), spec)
            {
                report.failures.push((
                    path.clone(),
                    "open-time disk settings changed; restart required".to_string(),
                ));
            }
        }
        if !report.failures.is_empty() {
            return report;
        }

        // Compute the slot assignment over the FULL desired set rather
        // than handing out slots as disks open. Currently-open disks
        // physically keep the pin they were opened with; their slots
        // are reserved first so a freshly-opened disk can never be
        // routed onto a CPU a survivor still occupies (the disjoint-CPU
        // invariant). This is idempotent across reconciles (same
        // desired set -> same assignment) and never leaks a slot when a
        // disk churns out and back in.
        let open_placements: HashMap<PathBuf, Option<DiskCpuSlot>> = self
            .entries
            .iter()
            .filter(|(path, _)| desired_paths.contains_key(path.as_path()))
            .map(|(path, entry)| (path.clone(), entry.placement))
            .collect();
        let assignment = assign_disk_cpus(desired, &self.disk_slots, &open_placements);

        let mut staged = HashMap::new();
        for spec in desired {
            let path = disk_path(spec);
            if self.entries.contains_key(Path::new(path)) {
                continue;
            }
            let pin = assignment.get(Path::new(path)).copied().flatten();
            match self.target.open(spec, pin) {
                Ok((handle, channel)) => {
                    staged.insert(
                        PathBuf::from(path),
                        OpenDiskEntry {
                            channel,
                            spec: spec.clone(),
                            placement: pin,
                            handle,
                        },
                    );
                }
                Err(e) => {
                    report.failures.push((PathBuf::from(path), e.to_string()));
                }
            }
        }

        if !report.failures.is_empty() {
            for (_, entry) in staged {
                entry.close();
            }
            return report;
        }

        let to_remove: Vec<PathBuf> = self
            .entries
            .keys()
            .filter(|path| !desired_paths.contains_key(path.as_path()))
            .cloned()
            .collect();
        for path in to_remove {
            if let Some(entry) = self.entries.remove(&path) {
                entry.close();
                report.removed += 1;
            }
        }
        for spec in desired {
            if let Some(entry) = self.entries.get_mut(Path::new(disk_path(spec))) {
                entry.spec = spec.clone();
            }
        }
        report.added = staged.len();
        self.entries.extend(staged);

        report
    }

    /// Snapshot of currently-open page channels suitable for handing
    /// to [`DiskChannelDirectory::apply_channels`]. Returned in
    /// path-sorted order so downstream hashing is stable across
    /// reconciles. Each entry carries the drive's NUMA node, taken
    /// from its assigned CPU slot (the node its storage core runs on);
    /// an unpinned disk carries `None`. The router uses this to keep a
    /// stripe's serving shard on the same node as its disk.
    pub fn channels_snapshot(&self) -> Vec<(PathBuf, PageChannel, Option<u16>, bool)> {
        let mut out: Vec<(PathBuf, PageChannel, Option<u16>, bool)> = self
            .entries
            .iter()
            .map(|(path, entry)| {
                let numa = entry.placement.and_then(|slot| slot.numa);
                (
                    path.clone(),
                    entry.channel.clone(),
                    numa,
                    !entry.spec.disable_page_cache,
                )
            })
            .collect();
        out.sort_by(|a, b| a.0.cmp(&b.0));
        out
    }

    /// Close every open handle. Channels are dropped first so the
    /// registry's clones are gone before the handles signal shutdown
    /// to the storage cores; that lets the storage cores observe the
    /// channel disconnect and exit promptly.
    pub fn drain(mut self) {
        for (_, entry) in self.entries.drain() {
            entry.close();
        }
    }

    /// Paths whose handle is currently open. Order is unspecified.
    pub fn current_paths(&self) -> Vec<PathBuf> {
        self.entries.keys().cloned().collect()
    }

    /// The CPU slot assigned to each currently-open disk, in
    /// path-sorted order. `None` means the disk runs unpinned (no
    /// slot was available). Used to surface placement decisions in
    /// logs.
    pub fn placement_snapshot(&self) -> Vec<(PathBuf, Option<DiskCpuSlot>)> {
        let mut out: Vec<(PathBuf, Option<DiskCpuSlot>)> = self
            .entries
            .iter()
            .map(|(path, entry)| (path.clone(), entry.placement))
            .collect();
        out.sort_by(|a, b| a.0.cmp(&b.0));
        out
    }
}

/// Deterministically assign disk CPU slots to the `desired` disks.
///
/// `open` holds the slot each currently-open (surviving) disk already
/// occupies; those disks cannot move their physical pin on a reconcile,
/// so their slots are reserved first and re-emitted verbatim. The
/// remaining free slots are then handed to the not-yet-open disks in
/// path-sorted order: each such disk first tries to claim an unused
/// slot on its own NUMA node; failing that it takes the first unused
/// slot of any node. When no slots remain the disk maps to `None` and
/// runs unpinned. Reserving survivor slots up front is what keeps a
/// freshly-opened disk off any CPU another open disk still holds, so
/// the assignment stays disjoint and idempotent across churn.
fn assign_disk_cpus(
    desired: &[DiskSpec],
    slots: &[DiskCpuSlot],
    open: &HashMap<PathBuf, Option<DiskCpuSlot>>,
) -> HashMap<PathBuf, Option<DiskCpuSlot>> {
    let mut sorted: Vec<&DiskSpec> = desired.iter().collect();
    sorted.sort_by(|a, b| disk_path(a).cmp(disk_path(b)));

    let mut used = vec![false; slots.len()];
    let mut out: HashMap<PathBuf, Option<DiskCpuSlot>> = HashMap::new();

    // Lock in the slots held by surviving open disks before assigning
    // anything new. Their pin is physically fixed for the lifetime of
    // the handle, so the fresh assignment must treat those slots as
    // reserved and echo the survivor's existing slot.
    for spec in &sorted {
        let path = disk_path(spec);
        if let Some(held) = open.get(Path::new(path)) {
            if let Some(slot) = held {
                if let Some(i) = slots.iter().position(|s| s == slot) {
                    used[i] = true;
                }
            }
            out.insert(PathBuf::from(path), *held);
        }
    }

    // Hand the remaining free slots to the not-yet-open disks.
    for spec in sorted {
        let path = disk_path(spec);
        if out.contains_key(Path::new(path)) {
            continue;
        }
        let local = slots
            .iter()
            .enumerate()
            .find(|(i, s)| !used[*i] && s.numa == spec.numa().map(|n| n as u16));
        let pick = local.or_else(|| slots.iter().enumerate().find(|(i, _)| !used[*i]));
        match pick {
            Some((i, slot)) => {
                used[i] = true;
                out.insert(PathBuf::from(path), Some(*slot));
            }
            None => {
                out.insert(PathBuf::from(path), None);
            }
        }
    }

    out
}

fn specs_drifted(prev: Option<&DiskSpec>, next: &DiskSpec) -> bool {
    match prev {
        None => false,
        Some(p) => {
            p.config != next.config
                || p.queue_depth != next.queue_depth
                || p.page_size_bytes != next.page_size_bytes
                || p.skip_recovery_scan != next.skip_recovery_scan
                || p.force_format != next.force_format
                || p.bypass_admission != next.bypass_admission
                || p.bypass_index_read != next.bypass_index_read
                || p.bypass_checksum != next.bypass_checksum
        }
    }
}

fn disk_path(spec: &DiskSpec) -> &str {
    spec.path().expect("disk path is validated at config load")
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::schema::{BlockDiskConfig, FileDiskConfig, disk_spec};
    use std::collections::HashSet;
    use std::sync::{Arc, Mutex};

    /// A `PageChannel` whose receiver is immediately dropped. These
    /// reconcile-bookkeeping tests never exercise the data path, so
    /// the channel only needs to exist and be clonable.
    fn dummy_channel() -> PageChannel {
        PageChannel::new().0
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
        type Handle = MockHandle;

        fn open(
            &self,
            spec: &DiskSpec,
            _pin: Option<DiskCpuSlot>,
        ) -> Result<(MockHandle, PageChannel), DiskError> {
            let mut s = self.state.lock().unwrap();
            s.open_calls += 1;
            let path = disk_path(spec);
            if s.fail_on.contains(Path::new(path)) {
                return Err(DiskError::Open("injected".into()));
            }
            s.opened.insert(PathBuf::from(path));
            drop(s);
            Ok((
                MockHandle {
                    path: PathBuf::from(path),
                    state: self.state.clone(),
                },
                dummy_channel(),
            ))
        }
    }

    fn spec(path: &str, qd: Option<u32>) -> DiskSpec {
        DiskSpec {
            queue_depth: qd,
            page_size_bytes: None,
            skip_recovery_scan: false,
            force_format: false,
            bypass_admission: false,
            bypass_index_read: false,
            bypass_checksum: false,
            disable_page_cache: false,
            config: Some(disk_spec::Config::Block(BlockDiskConfig {
                numa: None,
                path: path.to_string(),
            })),
        }
    }

    fn set_numa(spec: &mut DiskSpec, numa: u32) {
        let Some(disk_spec::Config::Block(cfg)) = spec.config.as_mut() else {
            panic!("expected block disk config")
        };
        cfg.numa = Some(numa);
    }

    fn slot(cpu: u32, numa: Option<u16>) -> DiskCpuSlot {
        DiskCpuSlot { cpu, numa }
    }

    #[test]
    fn empty_to_two_adds_both() {
        let (target, state) = MockDiskTarget::new();
        let mut reg = DiskRegistry::new(target, vec![]);
        let report = reg.reconcile(&[spec("/a", None), spec("/b", None)]);
        assert_eq!(report.added, 2);
        assert_eq!(report.removed, 0);
        assert!(report.failures.is_empty());
        assert_eq!(state.lock().unwrap().opened.len(), 2);
        assert_eq!(reg.channels_snapshot().len(), 2);
    }

    #[test]
    fn swap_adds_and_removes() {
        let (target, state) = MockDiskTarget::new();
        let mut reg = DiskRegistry::new(target, vec![]);
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
        let mut reg = DiskRegistry::new(target, vec![]);
        let desired = [spec("/a", None), spec("/b", None)];
        reg.reconcile(&desired);
        let calls_after_first = state.lock().unwrap().open_calls;
        let report = reg.reconcile(&desired);
        assert_eq!(report.added, 0);
        assert_eq!(report.removed, 0);
        assert_eq!(state.lock().unwrap().open_calls, calls_after_first);
    }

    #[test]
    fn page_cache_policy_drift_updates_snapshot_without_reopen() {
        let (target, state) = MockDiskTarget::new();
        let mut reg = DiskRegistry::new(target, vec![]);
        reg.reconcile(&[spec("/a", None)]);
        let calls_after_first = state.lock().unwrap().open_calls;

        let mut next = spec("/a", None);
        next.disable_page_cache = true;
        let report = reg.reconcile(&[next]);

        assert_eq!(report.added, 0);
        assert_eq!(report.removed, 0);
        assert_eq!(state.lock().unwrap().open_calls, calls_after_first);
        assert!(reg.entries[&PathBuf::from("/a")].spec.disable_page_cache);
        assert!(!reg.channels_snapshot()[0].3);
    }

    #[test]
    fn queue_depth_drift_requires_restart_and_preserves_old_disk() {
        let (target, _state) = MockDiskTarget::new();
        let mut reg = DiskRegistry::new(target, vec![]);
        reg.reconcile(&[spec("/a", Some(8))]);
        let report = reg.reconcile(&[spec("/a", Some(32))]);
        assert_eq!(report.added, 0);
        assert_eq!(report.removed, 0);
        assert_eq!(report.failures.len(), 1);
        assert_eq!(reg.entries[&PathBuf::from("/a")].spec.queue_depth, Some(8));
    }

    #[test]
    fn config_path_drift_triggers_remove_add() {
        let (target, _state) = MockDiskTarget::new();
        let mut reg = DiskRegistry::new(target, vec![]);
        reg.reconcile(&[spec("/a", None)]);
        let report = reg.reconcile(&[spec("/b", None)]);
        assert_eq!(report.added, 1);
        assert_eq!(report.removed, 1);
        assert!(reg.entries.contains_key(&PathBuf::from("/b")));
        assert!(!reg.entries.contains_key(&PathBuf::from("/a")));
    }

    #[test]
    fn failed_staging_rolls_back_other_new_disks() {
        let (target, state) = MockDiskTarget::new();
        state.lock().unwrap().fail_on.insert(PathBuf::from("/bad"));
        let mut reg = DiskRegistry::new(target, vec![]);
        let report = reg.reconcile(&[spec("/bad", None), spec("/good", None)]);
        assert_eq!(report.added, 0);
        assert_eq!(report.failures.len(), 1);
        assert_eq!(report.failures[0].0, PathBuf::from("/bad"));
        assert!(state.lock().unwrap().opened.is_empty());
        assert!(reg.channels_snapshot().is_empty());
    }

    #[test]
    fn failed_open_is_retried_by_the_same_desired_set() {
        let (target, state) = MockDiskTarget::new();
        state.lock().unwrap().fail_on.insert(PathBuf::from("/a"));
        let mut reg = DiskRegistry::new(target, vec![]);
        let desired = [spec("/a", None)];

        let failed = reg.reconcile(&desired);
        assert_eq!(failed.failures.len(), 1);
        assert!(reg.entries.is_empty());

        state.lock().unwrap().fail_on.clear();
        let retried = reg.reconcile(&desired);
        assert_eq!(retried.added, 1);
        assert!(retried.failures.is_empty());
        assert_eq!(reg.entries[&PathBuf::from("/a")].spec, desired[0]);
    }

    #[test]
    fn open_time_drift_is_rejected_without_closing_old_disk() {
        let (target, state) = MockDiskTarget::new();
        let mut reg = DiskRegistry::new(target, vec![]);
        reg.reconcile(&[spec("/a", Some(8))]);

        let desired = [spec("/a", Some(32))];
        let failed = reg.reconcile(&desired);
        assert_eq!(failed.removed, 0);
        assert_eq!(failed.added, 0);
        assert_eq!(failed.failures.len(), 1);
        assert_eq!(reg.entries[&PathBuf::from("/a")].spec.queue_depth, Some(8));
        assert!(state.lock().unwrap().opened.contains(&PathBuf::from("/a")));
    }

    /// Records the per-disk `(path, cpu_hint)` decisions so tests can
    /// assert on slot assignment without inspecting registry internals.
    struct CpuRecorder {
        hints: Arc<Mutex<Vec<(PathBuf, Option<usize>)>>>,
    }
    struct RecorderHandle;
    impl DiskTarget for CpuRecorder {
        type Handle = RecorderHandle;
        fn open(
            &self,
            spec: &DiskSpec,
            pin: Option<DiskCpuSlot>,
        ) -> Result<(RecorderHandle, PageChannel), DiskError> {
            self.hints
                .lock()
                .unwrap()
                .push((PathBuf::from(disk_path(spec)), pin.map(|s| s.cpu as usize)));
            Ok((RecorderHandle, dummy_channel()))
        }
    }

    #[test]
    fn cpu_hint_per_disk_from_slots() {
        // New slot model: the registry is seeded with explicit
        // DiskCpuSlots (one on numa 0, one on numa 1). Each disk
        // claims the first unused slot on its own NUMA node; a disk
        // whose numa has no remaining slot (here the numa: None disk,
        // since both slots are NUMA-tagged and already taken) gets no
        // hint and runs unpinned. Hints are NUMA-local and disjoint.
        let slots = vec![slot(7, Some(0)), slot(11, Some(1))];
        let hints = Arc::new(Mutex::new(Vec::new()));
        let mut reg = DiskRegistry::new(
            CpuRecorder {
                hints: hints.clone(),
            },
            slots,
        );
        let mut d0 = spec("/a", None);
        set_numa(&mut d0, 0);
        let mut d1 = spec("/b", None);
        set_numa(&mut d1, 1);
        let d2 = spec("/c", None); // numa: None
        reg.reconcile(&[d0, d1, d2]);
        let got = hints.lock().unwrap().clone();
        assert_eq!(
            got,
            vec![
                (PathBuf::from("/a"), Some(7)),
                (PathBuf::from("/b"), Some(11)),
                (PathBuf::from("/c"), None),
            ]
        );
    }

    #[test]
    fn two_disks_same_numa_get_distinct_hints() {
        // Two slots on the same NUMA node, two disks on that node:
        // each disk must claim a different slot (disjointness among
        // disks).
        let slots = vec![slot(4, Some(0)), slot(5, Some(0))];
        let hints = Arc::new(Mutex::new(Vec::new()));
        let mut reg = DiskRegistry::new(
            CpuRecorder {
                hints: hints.clone(),
            },
            slots,
        );
        let mut d0 = spec("/a", None);
        set_numa(&mut d0, 0);
        let mut d1 = spec("/b", None);
        set_numa(&mut d1, 0);
        reg.reconcile(&[d0, d1]);
        let got = hints.lock().unwrap().clone();
        let assigned: Vec<Option<usize>> = got.iter().map(|(_, h)| *h).collect();
        assert!(assigned.contains(&Some(4)));
        assert!(assigned.contains(&Some(5)));
        // Distinct cpus for the two disks.
        assert_ne!(got[0].1, got[1].1);
    }

    #[test]
    fn disks_beyond_slot_count_get_none() {
        // One slot, three disks: the first (path-sorted) disk claims
        // the slot, the rest run unpinned.
        let slots = vec![slot(9, Some(0))];
        let hints = Arc::new(Mutex::new(Vec::new()));
        let mut reg = DiskRegistry::new(
            CpuRecorder {
                hints: hints.clone(),
            },
            slots,
        );
        let mut d0 = spec("/a", None);
        set_numa(&mut d0, 0);
        let mut d1 = spec("/b", None);
        set_numa(&mut d1, 0);
        let mut d2 = spec("/c", None);
        set_numa(&mut d2, 0);
        reg.reconcile(&[d0, d1, d2]);
        let got = hints.lock().unwrap().clone();
        let some_count = got.iter().filter(|(_, h)| h.is_some()).count();
        let none_count = got.iter().filter(|(_, h)| h.is_none()).count();
        assert_eq!(some_count, 1, "exactly one disk should be pinned");
        assert_eq!(none_count, 2, "remaining disks should be unpinned");
        // The single pinned disk got the only slot.
        assert!(got.iter().any(|(_, h)| *h == Some(9)));
    }

    #[test]
    fn churn_keeps_open_disks_on_disjoint_slots() {
        // Regression for the H2 CPU-slot collision under disk churn.
        // A surviving disk physically keeps its pin, so a freshly-
        // opened disk must never be handed a slot a survivor still
        // occupies. Reproduces the exact 4-step sequence from the bug
        // report over two NUMA-free slots [s0, s1].
        let slots = vec![slot(0, None), slot(1, None)];
        let (target, _state) = MockDiskTarget::new();
        let mut reg = DiskRegistry::new(target, slots);

        // 1. reconcile([/a]) -> /a claims the first free slot.
        reg.reconcile(&[spec("/a", None)]);
        assert_disjoint_open_slots(&reg);
        let a_slot = open_cpu(&reg, "/a");

        // 2. reconcile([/a,/b]) -> /b claims the other slot; /a kept.
        reg.reconcile(&[spec("/a", None), spec("/b", None)]);
        assert_disjoint_open_slots(&reg);
        assert_eq!(open_cpu(&reg, "/a"), a_slot, "/a must not move");
        let b_slot = open_cpu(&reg, "/b");

        // 3. reconcile([/b]) -> /a removed; /b stays physically put.
        reg.reconcile(&[spec("/b", None)]);
        assert_disjoint_open_slots(&reg);
        assert_eq!(open_cpu(&reg, "/b"), b_slot, "/b must not move on churn");

        // 4. reconcile([/b,/c]) -> the fresh assignment must route /c
        //    to the freed slot, NOT collide with /b.
        reg.reconcile(&[spec("/b", None), spec("/c", None)]);
        assert_disjoint_open_slots(&reg);
        assert_eq!(open_cpu(&reg, "/b"), b_slot, "/b must not move");
        assert_ne!(open_cpu(&reg, "/c"), b_slot, "/c must not share /b's slot");

        // Idempotent re-reconcile of the same desired set moves nobody.
        let before = reg.placement_snapshot();
        let report = reg.reconcile(&[spec("/b", None), spec("/c", None)]);
        assert_eq!(report.added, 0);
        assert_eq!(report.removed, 0);
        assert_eq!(
            reg.placement_snapshot(),
            before,
            "idempotent reconcile moved a disk"
        );
    }

    /// The CPU of the slot currently pinned to `path`. Panics if the
    /// disk is not open or runs unpinned, which the churn test never
    /// expects (every disk in it fits within the two slots).
    fn open_cpu<T: DiskTarget>(reg: &DiskRegistry<T>, path: &str) -> u32 {
        reg.placement_snapshot()
            .into_iter()
            .find(|(p, _)| p == &PathBuf::from(path))
            .and_then(|(_, s)| s)
            .unwrap_or_else(|| panic!("{path} is not pinned"))
            .cpu
    }

    /// Assert no two currently-open disks share a CPU slot.
    fn assert_disjoint_open_slots<T: DiskTarget>(reg: &DiskRegistry<T>) {
        let cpus: Vec<u32> = reg
            .placement_snapshot()
            .into_iter()
            .filter_map(|(_, s)| s.map(|s| s.cpu))
            .collect();
        let unique: HashSet<u32> = cpus.iter().copied().collect();
        assert_eq!(
            cpus.len(),
            unique.len(),
            "two open disks share a CPU slot: {cpus:?}"
        );
    }

    #[test]
    fn page_size_drift_requires_restart() {
        let (target, _state) = MockDiskTarget::new();
        let mut reg = DiskRegistry::new(target, vec![]);
        reg.reconcile(&[spec("/a", None)]);
        let mut next = spec("/a", None);
        next.page_size_bytes = Some(4096);
        let report = reg.reconcile(&[next]);
        assert_eq!(report.added, 0);
        assert_eq!(report.removed, 0);
        assert_eq!(report.failures.len(), 1);
        assert_eq!(reg.entries[&PathBuf::from("/a")].spec.page_size_bytes, None);
    }

    /// A file-backed spec that differs only in `size` must be detected
    /// as drifted, while two identical specs must not. Also pins the
    /// `prev == None` contract (a first-time open never reports drift).
    #[test]
    fn size_drift_is_detected() {
        let file_spec = |size: u64| DiskSpec {
            queue_depth: None,
            page_size_bytes: None,
            skip_recovery_scan: false,
            force_format: false,
            bypass_admission: false,
            bypass_index_read: false,
            bypass_checksum: false,
            disable_page_cache: false,
            config: Some(disk_spec::Config::File(FileDiskConfig {
                size: Some(size),
                path: "/a".to_string(),
            })),
        };
        let a = file_spec(64 * 4096);
        let b = file_spec(128 * 4096);

        // Differ only in size -> drifted.
        assert!(specs_drifted(Some(&a), &b));
        // Identical -> not drifted.
        assert!(!specs_drifted(Some(&a), &a.clone()));
        // No previous applied spec -> never drifted.
        assert!(!specs_drifted(None, &a));
    }

    #[test]
    fn skip_recovery_scan_drift_requires_restart() {
        let (target, _state) = MockDiskTarget::new();
        let mut reg = DiskRegistry::new(target, vec![]);
        reg.reconcile(&[spec("/a", None)]);
        let mut next = spec("/a", None);
        next.skip_recovery_scan = true;
        let report = reg.reconcile(&[next]);
        assert_eq!(report.failures.len(), 1);
        assert!(!reg.entries[&PathBuf::from("/a")].spec.skip_recovery_scan);
    }

    #[test]
    fn force_format_drift_requires_restart() {
        let (target, _state) = MockDiskTarget::new();
        let mut reg = DiskRegistry::new(target, vec![]);
        reg.reconcile(&[spec("/a", None)]);
        let mut next = spec("/a", None);
        next.force_format = true;
        let report = reg.reconcile(&[next]);
        assert_eq!(report.failures.len(), 1);
        assert!(!reg.entries[&PathBuf::from("/a")].spec.force_format);
    }

    #[test]
    fn bypass_admission_drift_requires_restart() {
        let (target, _state) = MockDiskTarget::new();
        let mut reg = DiskRegistry::new(target, vec![]);
        reg.reconcile(&[spec("/a", None)]);
        let mut next = spec("/a", None);
        next.bypass_admission = true;
        let report = reg.reconcile(&[next]);
        assert_eq!(report.failures.len(), 1);
        assert!(!reg.entries[&PathBuf::from("/a")].spec.bypass_admission);
    }

    #[test]
    fn bypass_index_read_drift_requires_restart() {
        let (target, _state) = MockDiskTarget::new();
        let mut reg = DiskRegistry::new(target, vec![]);
        reg.reconcile(&[spec("/a", None)]);
        let mut next = spec("/a", None);
        next.bypass_index_read = true;
        let report = reg.reconcile(&[next]);
        assert_eq!(report.failures.len(), 1);
        assert!(!reg.entries[&PathBuf::from("/a")].spec.bypass_index_read);
    }

    #[test]
    fn bypass_checksum_drift_requires_restart() {
        let (target, _state) = MockDiskTarget::new();
        let mut reg = DiskRegistry::new(target, vec![]);
        reg.reconcile(&[spec("/a", None)]);
        let mut next = spec("/a", None);
        next.bypass_checksum = true;
        let report = reg.reconcile(&[next]);
        assert_eq!(report.failures.len(), 1);
        assert!(!reg.entries[&PathBuf::from("/a")].spec.bypass_checksum);
    }

    #[test]
    fn drain_closes_all() {
        let (target, state) = MockDiskTarget::new();
        let mut reg = DiskRegistry::new(target, vec![]);
        reg.reconcile(&[spec("/a", None), spec("/b", None)]);
        assert_eq!(state.lock().unwrap().opened.len(), 2);
        reg.drain();
        assert!(state.lock().unwrap().opened.is_empty());
    }

    #[test]
    fn channels_snapshot_is_path_sorted() {
        let (target, _state) = MockDiskTarget::new();
        let mut reg = DiskRegistry::new(target, vec![]);
        reg.reconcile(&[spec("/c", None), spec("/a", None), spec("/b", None)]);
        let snap = reg.channels_snapshot();
        let paths: Vec<PathBuf> = snap.into_iter().map(|(p, _, _, _)| p).collect();
        assert_eq!(
            paths,
            vec![
                PathBuf::from("/a"),
                PathBuf::from("/b"),
                PathBuf::from("/c"),
            ]
        );
    }

    #[test]
    fn channels_snapshot_carries_numa_from_placement() {
        // The published NUMA per drive is the node of its assigned CPU
        // slot; a disk that ran out of NUMA-local slots and fell back
        // to unpinned carries None. Mirrors `cpu_hint_per_disk_from_slots`.
        let slots = vec![slot(7, Some(0)), slot(11, Some(1))];
        let (target, _state) = MockDiskTarget::new();
        let mut reg = DiskRegistry::new(target, slots);
        let mut d0 = spec("/a", None);
        set_numa(&mut d0, 0);
        let mut d1 = spec("/b", None);
        set_numa(&mut d1, 1);
        let d2 = spec("/c", None); // unpinned: both NUMA slots taken
        reg.reconcile(&[d0, d1, d2]);
        let numa: Vec<(PathBuf, Option<u16>)> = reg
            .channels_snapshot()
            .into_iter()
            .map(|(p, _, n, _)| (p, n))
            .collect();
        assert_eq!(
            numa,
            vec![
                (PathBuf::from("/a"), Some(0)),
                (PathBuf::from("/b"), Some(1)),
                (PathBuf::from("/c"), None),
            ]
        );
    }
}
