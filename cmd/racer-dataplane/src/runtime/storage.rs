// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! One process-local replacement transaction. Filesystem work runs exclusively
//! on the setup thread. Workers only stage empty affine state and acknowledge
//! fences. The rename is the commit point; after it we only move forward.
use super::*;
use crate::{
    allocator::{CheckpointBudget, EmptyShard, LayoutPlan, ResourceEstimate, Slab, SlabFile},
    control::{StorageRequest, StorageResult},
    sharding::{Placement, ShardState, StorageGeneration, WorkerContext},
};
use std::{
    fs::{File, OpenOptions},
    os::{fd::AsRawFd, unix::fs::OpenOptionsExt},
    path::{Path, PathBuf},
    sync::{
        Mutex,
        atomic::{AtomicBool, Ordering},
    },
    thread::{self, JoinHandle},
};

const TICK: Duration = Duration::from_millis(10);
const PRECOMMIT_TIMEOUT: Duration = Duration::from_secs(30);

/// Stable sidecar lock, acquired before opening the slab and retained through
/// teardown. Inode flock alone cannot exclude another process across rename.
pub struct StoragePath {
    _lock: File,
    directory: File,
    active: PathBuf,
    candidate: PathBuf,
}
impl StoragePath {
    pub fn lock(path: impl AsRef<Path>) -> io::Result<Self> {
        let path = path.as_ref();
        let name = path
            .file_name()
            .ok_or_else(|| io::Error::other("missing slab filename"))?;
        let parent = path
            .parent()
            .filter(|p| !p.as_os_str().is_empty())
            .unwrap_or(Path::new("."));
        let directory = OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_DIRECTORY | libc::O_CLOEXEC)
            .open(parent)?;
        // Pin namespace operations to this directory even if an ancestor moves.
        let base = PathBuf::from(format!("/proc/self/fd/{}", directory.as_raw_fd()));
        let mut lock_name = name.to_os_string();
        lock_name.push(".lock");
        let lock = OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .custom_flags(libc::O_NOFOLLOW | libc::O_CLOEXEC)
            .open(base.join(lock_name))?;
        // SAFETY: live descriptor, no retained userspace pointers.
        if unsafe { libc::flock(lock.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) } != 0 {
            return Err(io::Error::last_os_error());
        }
        let mut candidate = name.to_os_string();
        candidate.push(".resize");
        let this = Self {
            _lock: lock,
            active: base.join(name),
            candidate: base.join(candidate),
            directory,
        };
        // An interrupted preparation is never authoritative. After rename the
        // candidate name is absent and active contains the complete new layout.
        this.discard()?;
        Ok(this)
    }
    pub fn active(&self) -> &Path {
        &self.active
    }
    fn discard(&self) -> io::Result<()> {
        match std::fs::remove_file(&self.candidate) {
            Ok(()) => self.directory.sync_all(),
            Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(()),
            Err(e) => Err(e),
        }
    }
    fn publish(&self) -> io::Result<()> {
        std::fs::rename(&self.candidate, &self.active)
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Phase {
    Idle,
    Stage,
    Fence,
    Install,
    Release,
    Abort,
}
struct Transaction {
    id: u64,
    phase: Phase,
    generation: Option<Arc<StorageGeneration>>,
    packets: Vec<Option<Vec<EmptyShard>>>,
    ack: Vec<bool>,
    failure: Option<String>,
}
impl Transaction {
    fn phase(&mut self, phase: Phase) {
        self.phase = phase;
        self.ack.fill(false);
    }
    fn complete(&self) -> bool {
        self.ack.iter().all(|v| *v)
    }
}
struct Shared {
    transaction: Mutex<Transaction>,
    // 0: precommit, 1: commit authorized, 2: stop. Authorization linearizes
    // against worker shutdown without making a reactor wait for rename/fsync.
    commit: std::sync::atomic::AtomicU8,
    stopping: AtomicBool,
    #[cfg(test)]
    faults: Mutex<TestFaults>,
}
#[cfg(test)]
#[derive(Default)]
struct TestFaults {
    prepare: bool,
    publish: bool,
    sync: bool,
    stage_worker: Option<usize>,
    drain_timeout: bool,
}

/// Owner of the single filesystem thread. Drop only after worker teardown.
pub struct StorageCoordinator {
    _path: Arc<StoragePath>,
    shared: Arc<Shared>,
    thread: Option<JoinHandle<()>>,
}
#[derive(Clone)]
pub struct StorageHandle(Arc<Shared>);

impl StorageCoordinator {
    pub fn start(
        path: StoragePath,
        slab: &Slab,
        placement: &Placement,
        budget: CheckpointBudget,
        updates: Arc<Updates>,
    ) -> io::Result<Self> {
        let placement = placement.clone();
        let workers = placement
            .storage_generation(slab.shard_count())?
            .worker_count();
        let shared = Arc::new(Shared {
            transaction: Mutex::new(Transaction {
                id: 0,
                phase: Phase::Idle,
                generation: None,
                packets: (0..workers).map(|_| None).collect(),
                ack: vec![false; workers],
                failure: None,
            }),
            stopping: AtomicBool::new(false),
            commit: std::sync::atomic::AtomicU8::new(0),
            #[cfg(test)]
            faults: Mutex::default(),
        });
        let state = shared.clone();
        let path = Arc::new(path);
        let thread_path = path.clone();
        let (capacity, resources, retirement) = (
            slab.size(),
            slab.resources(),
            slab.retirement().upgrade().unwrap(),
        );
        updates.observe_storage(capacity, slab.shard_count());
        let thread = thread::Builder::new()
            .name("racer-storage".into())
            .spawn(move || {
                run(
                    thread_path,
                    placement,
                    budget,
                    updates,
                    state,
                    capacity,
                    resources,
                    retirement,
                );
            })?;
        Ok(Self {
            _path: path,
            shared,
            thread: Some(thread),
        })
    }
    pub fn handle(&self) -> StorageHandle {
        StorageHandle(self.shared.clone())
    }
}
impl Drop for StorageCoordinator {
    fn drop(&mut self) {
        self.shared.commit.store(2, Ordering::Release);
        self.shared.stopping.store(true, Ordering::Release);
        if let Some(thread) = self.thread.take() {
            let _ = thread.join();
        }
    }
}

fn available_memory() -> io::Result<u64> {
    let info = std::fs::read_to_string("/proc/meminfo")?;
    let available = info
        .lines()
        .find_map(|line| {
            line.strip_prefix("MemAvailable:")?
                .split_whitespace()
                .next()?
                .parse::<u64>()
                .ok()
        })
        .ok_or_else(|| io::Error::other("MemAvailable missing"))?
        .saturating_mul(1024);
    cgroup_available_memory(
        available,
        Path::new("/sys/fs/cgroup"),
        &std::fs::read_to_string("/proc/self/cgroup")?,
    )
}

fn cgroup_available_memory(mut available: u64, root: &Path, cgroups: &str) -> io::Result<u64> {
    // Honor finite cgroup-v2 limits, including ancestor limits. A container's
    // cgroup namespace commonly exposes its own root as /sys/fs/cgroup.
    if let Some(relative) = cgroups.lines().find_map(|l| l.strip_prefix("0::")) {
        let relative = relative.trim_start_matches('/');
        let mut path = root.join(relative);
        if !path.join("memory.current").exists() {
            path = root.to_path_buf();
        }
        loop {
            match std::fs::read_to_string(path.join("memory.max")) {
                Ok(limit) if limit.trim() == "max" => {}
                Ok(limit) => {
                    let limit: u64 = limit.trim().parse().map_err(io::Error::other)?;
                    let current: u64 = std::fs::read_to_string(path.join("memory.current"))?
                        .trim()
                        .parse()
                        .map_err(io::Error::other)?;
                    let reclaimable =
                        clean_file_cache(&std::fs::read_to_string(path.join("memory.stat"))?)?;
                    // Buffered payloads are charged to memory.current but clean
                    // file pages can be reclaimed by the candidate's allocations.
                    // Each ancestor has its own usage/cache counters. Take the
                    // minimum headroom, never add credits across the hierarchy
                    // or to MemAvailable (which already accounts for reclaim).
                    available =
                        available.min(limit.saturating_sub(current.saturating_sub(reclaimable)));
                }
                Err(error) if error.kind() == io::ErrorKind::NotFound => {}
                Err(error) => return Err(error),
            }
            if path == root || !path.pop() || !path.starts_with(root) {
                break;
            }
        }
    }
    Ok(available)
}

fn clean_file_cache(stat: &str) -> io::Result<u64> {
    let names = [
        "file",
        "shmem",
        "active_file",
        "inactive_file",
        "file_dirty",
        "file_writeback",
        "unevictable",
    ];
    let mut values = [None; 7];
    for line in stat.lines() {
        let mut fields = line.split_whitespace();
        let Some(index) = fields
            .next()
            .and_then(|name| names.iter().position(|n| *n == name))
        else {
            continue;
        };
        let value = fields
            .next()
            .and_then(|v| v.parse::<u64>().ok())
            .ok_or_else(|| io::Error::other("invalid cgroup memory.stat counter"))?;
        if fields.next().is_some() || values[index].replace(value).is_some() {
            return Err(io::Error::other(
                "duplicate or malformed cgroup memory.stat counter",
            ));
        }
    }
    let [file, shmem, active, inactive, dirty, writeback, unevictable] =
        values.map(|v| v.unwrap_or(0));
    if values.iter().any(Option::is_none) {
        return Err(io::Error::other("missing cgroup memory.stat counter"));
    }
    // file includes shmem; file LRU counters overlap file, not extra memory.
    // Bound by both views, then conservatively exclude all dirty, writeback and
    // unevictable pages. Those exclusions may overlap, intentionally undercounting
    // rather than crediting pages that need I/O, swap, or cannot be reclaimed.
    Ok(file
        .saturating_sub(shmem)
        .min(active.saturating_add(inactive))
        .saturating_sub(dirty.saturating_add(writeback).saturating_add(unevictable)))
}
fn validate_resources(old: ResourceEstimate, plan: LayoutPlan, available: u64) -> io::Result<()> {
    // memory.current already includes the live generation. The candidate is
    // empty and never fills before install; it uses open_empty, not recovery.
    // Reserve incremental setup plus the old generation's bounded concurrent
    // checkpoint work while staging/draining. Keep a 64 MiB operational margin
    // for unrelated process growth during the transaction, not a user budget.
    let required = plan.empty_preparation_bytes() + old.checkpoint_peak_bytes + (64 << 20);
    validate_headroom(required, available)
}

fn validate_headroom(required: u64, available: u64) -> io::Result<()> {
    if required > available {
        return Err(io::Error::other(format!(
            "storage incremental allowance {required} bytes exceeds available memory {available} bytes"
        )));
    }
    Ok(())
}

/// Startup floor for bitmap backing and sequential per-worker recovery scratch.
/// Recovered indexes grow with persisted contents; the populated-tree diagnostic
/// maximum is not an up-front allocation. This check is not an RSS guarantee.
pub fn validate_startup_memory(slab: &Slab, workers: usize) -> io::Result<()> {
    validate_headroom(
        startup_allowance(slab.resources(), workers),
        available_memory()?,
    )
}

fn startup_allowance(resources: ResourceEstimate, workers: usize) -> u64 {
    2 * resources.allocation_bitmap_bytes
        + resources.shard_fixed_bytes
        + resources.recovery_scratch_bytes(workers)
        + (64 << 20)
}

fn wait(shared: &Shared, deadline: Option<Instant>) -> io::Result<()> {
    loop {
        let state = shared.transaction.lock().unwrap();
        if let Some(error) = &state.failure {
            return Err(io::Error::other(error.clone()));
        }
        if state.complete() {
            return Ok(());
        }
        drop(state);
        if shared.stopping.load(Ordering::Acquire) {
            return Err(io::Error::new(
                io::ErrorKind::Interrupted,
                "process stopping",
            ));
        }
        if deadline.is_some_and(|d| Instant::now() >= d) {
            return Err(io::Error::new(
                io::ErrorKind::TimedOut,
                "storage worker barrier timed out",
            ));
        }
        thread::sleep(TICK);
    }
}
fn phase(shared: &Shared, next: Phase) {
    shared.transaction.lock().unwrap().phase(next);
}

#[allow(clippy::too_many_arguments)]
fn run(
    path: Arc<StoragePath>,
    placement: Placement,
    budget: CheckpointBudget,
    updates: Arc<Updates>,
    shared: Arc<Shared>,
    mut capacity: u64,
    mut resources: ResourceEstimate,
    mut active: Arc<SlabFile>,
) {
    let mut retry = Retry::new(Instant::now());
    let mut previous: Option<StorageRequest> = None;
    let mut retiring: Option<Arc<SlabFile>> = None;
    while !shared.stopping.load(Ordering::Acquire) {
        thread::sleep(TICK);
        let Some(request) = updates.desired_storage() else {
            continue;
        };
        if request.desired_bytes == capacity {
            updates.report_storage(&request, StorageResult::Applied, capacity);
        }
        if retiring
            .as_ref()
            .is_some_and(|old| Arc::strong_count(old) != 1)
        {
            continue;
        }
        // The last close of a large unlinked inode belongs to this thread too.
        retiring = None;
        if previous.as_ref() != Some(&request) {
            retry = Retry::new(Instant::now());
            previous = Some(request.clone());
        }
        if request.desired_bytes == capacity {
            updates.report_storage(&request, StorageResult::Applied, capacity);
            continue;
        }
        if Instant::now() < retry.after {
            continue;
        }
        updates.report_storage(&request, StorageResult::Pending, capacity);
        let prepared = (|| {
            let workers = shared.transaction.lock().unwrap().ack.len();
            let plan = LayoutPlan::new(request.desired_bytes, workers)?;
            validate_resources(resources, plan, available_memory()?)?;
            path.discard()?;
            #[cfg(test)]
            if shared.faults.lock().unwrap().prepare {
                return Err(io::Error::from_raw_os_error(libc::ENOSPC));
            }
            let mut slab = Slab::prepare_replacement(&path.candidate, plan, budget.clone())?;
            let shards = slab.prepare_empty_shards()?;
            let generation = Arc::new(plan.authorize(&placement)?);
            Ok::<_, io::Error>((plan, slab, shards, generation))
        })();
        let (plan, slab, shards, generation) = match prepared {
            Ok(prepared) => prepared,
            Err(e) => {
                let _ = path.discard();
                updates.report_storage(&request, StorageResult::Failed(e.to_string()), capacity);
                retry.fail(Instant::now());
                continue;
            }
        };
        let candidate_retirement = slab.retirement().upgrade().unwrap();
        {
            let mut state = shared.transaction.lock().unwrap();
            state.id = state
                .id
                .checked_add(1)
                .expect("storage transaction counter exhausted");
            state.failure = None;
            let mut packets: Vec<Vec<EmptyShard>> =
                (0..state.ack.len()).map(|_| Vec::new()).collect();
            let count = packets.len();
            for (i, shard) in shards.into_iter().enumerate() {
                packets[i % count].push(shard);
            }
            state.packets = packets.into_iter().map(Some).collect();
            state.generation = Some(generation);
            state.phase(Phase::Stage);
        }
        let before_commit = (|| {
            wait(&shared, Some(Instant::now() + PRECOMMIT_TIMEOUT))?;
            if updates.desired_storage().as_ref() != Some(&request) {
                return Err(io::Error::other("storage request superseded"));
            }
            phase(&shared, Phase::Fence);
            let drain_deadline = Instant::now() + PRECOMMIT_TIMEOUT;
            #[cfg(test)]
            let drain_deadline = if shared.faults.lock().unwrap().drain_timeout {
                Instant::now()
            } else {
                drain_deadline
            };
            wait(&shared, Some(drain_deadline))?;
            if shared.stopping.load(Ordering::Acquire)
                || updates.desired_storage().as_ref() != Some(&request)
            {
                return Err(io::Error::other("storage request stopped or superseded"));
            }
            #[cfg(test)]
            if shared.faults.lock().unwrap().publish {
                return Err(io::Error::from_raw_os_error(libc::ENOSPC));
            }
            shared
                .commit
                .compare_exchange(0, 1, Ordering::AcqRel, Ordering::Acquire)
                .map_err(|_| {
                    io::Error::new(io::ErrorKind::Interrupted, "storage commit stopped")
                })?;
            path.publish()
        })();
        if let Err(e) = before_commit {
            {
                let mut state = shared.transaction.lock().unwrap();
                state.failure = None;
                state.packets.iter_mut().for_each(|p| {
                    p.take();
                });
                state.phase(Phase::Abort);
            }
            let _ = wait(&shared, None);
            drop(slab);
            retiring = Some(candidate_retirement);
            let _ = path.discard();
            updates.report_storage(&request, StorageResult::Failed(e.to_string()), capacity);
            retry.fail(Instant::now());
            let _ = shared
                .commit
                .compare_exchange(1, 0, Ordering::AcqRel, Ordering::Acquire);
            phase(&shared, Phase::Idle);
            continue;
        }
        // Rename has committed. A failed directory sync is ambiguous durability,
        // never permission to roll back/resume the old generation. Keep fenced
        // and retry until durable or shutdown. Restart opens either complete inode.
        while let Err(e) = sync_directory(&path, &shared) {
            updates.report_storage(
                &request,
                StorageResult::Failed(format!("published; directory sync pending: {e}")),
                capacity,
            );
            if shared.stopping.load(Ordering::Acquire) {
                return;
            }
            thread::sleep(Duration::from_millis(250));
        }
        phase(&shared, Phase::Install);
        if wait(&shared, None).is_err() {
            return;
        }
        capacity = plan.capacity();
        resources = plan.resources();
        retiring = Some(std::mem::replace(&mut active, candidate_retirement));
        drop(slab);
        phase(&shared, Phase::Release);
        if wait(&shared, None).is_err() {
            return;
        }
        updates.observe_storage(capacity, plan.shard_count());
        updates.report_storage(&request, StorageResult::Applied, capacity);
        phase(&shared, Phase::Idle);
        let _ = shared
            .commit
            .compare_exchange(1, 0, Ordering::AcqRel, Ordering::Acquire);
        retry = Retry::new(Instant::now());
    }
}

fn sync_directory(path: &StoragePath, _shared: &Shared) -> io::Result<()> {
    #[cfg(test)]
    if _shared.faults.lock().unwrap().sync {
        return Err(io::Error::from_raw_os_error(libc::EIO));
    }
    path.directory.sync_all()
}

pub(super) struct Local {
    handle: StorageHandle,
    context: WorkerContext,
    id: u64,
    staged: Option<Cache>,
    retiring: Option<Cache>,
    building: Option<(
        Arc<StorageGeneration>,
        std::collections::VecDeque<(crate::sharding::Assignment, EmptyShard)>,
        Vec<ShardState>,
    )>,
}
impl Local {
    pub(super) fn stop(&self) {
        self.handle.0.commit.store(2, Ordering::Release);
        self.handle.0.stopping.store(true, Ordering::Release);
    }
}
impl Volumes {
    pub fn with_storage(mut self, handle: StorageHandle, context: WorkerContext) -> Self {
        self.storage = Some(Local {
            handle,
            context,
            id: 0,
            staged: None,
            retiring: None,
            building: None,
        });
        self
    }
    pub(super) fn storage_maintenance(&mut self, enabled: bool) {
        self.maintenance = enabled;
        self.cache.borrow_mut().maintenance(enabled);
        for server in self
            .servers
            .values_mut()
            .chain(self.retired.values_mut().map(|(_, s)| s))
        {
            let handler = server.handler_mut();
            for generation in std::iter::once(&handler.current).chain(&handler.draining) {
                for handler in &generation.handlers {
                    handler.borrow_mut().maintenance(enabled);
                }
            }
        }
    }
    pub(super) fn poll_storage(&mut self, ring: &uring::Ring) -> io::Result<uring::Work> {
        let Some(mut local) = self.storage.take() else {
            return Ok(uring::Work::default());
        };
        if let Some(cache) = &mut local.retiring
            && cache.retire_one()
        {
            local.retiring = None;
        }
        let shared = local.handle.0.clone();
        let mut state = shared.transaction.lock().unwrap();
        let phase = state.phase;
        if state.id != local.id {
            local.id = state.id;
            local.staged = None;
            local.building = None;
        }
        let acknowledged = state.ack[self.worker];
        let packet = if phase == Phase::Stage && !acknowledged && local.building.is_none() {
            state.packets[self.worker]
                .take()
                .map(|packet| (state.generation.as_ref().unwrap().clone(), packet))
        } else {
            None
        };
        drop(state);
        let result = (|| {
            if let Some((generation, packet)) = packet {
                #[cfg(test)]
                if shared.faults.lock().unwrap().stage_worker == Some(self.worker) {
                    return Err(io::Error::other("injected worker staging failure"));
                }
                let assignments = generation.take_assignments(&local.context)?;
                if assignments.len() != packet.len() {
                    return Err(io::Error::other("incomplete storage packet"));
                }
                local.building = Some((
                    generation,
                    assignments.into_iter().zip(packet).collect(),
                    Vec::new(),
                ));
            }
            let mut ack = false;
            if !acknowledged {
                match phase {
                    Phase::Stage => {
                        if let Some((generation, pending, shards)) = &mut local.building {
                            // At most one bounded bitmap initialization per turn;
                            // all file reads, syncs and descriptor setup ran elsewhere.
                            if let Some((assignment, shard)) = pending.pop_front() {
                                shards.push(ShardState::activate_empty(
                                    &local.context,
                                    assignment,
                                    shard,
                                    ring.pool(),
                                )?);
                            }
                            if pending.is_empty() {
                                let mut cache = Cache::for_generation(
                                    &local.context,
                                    generation,
                                    Namespace::new("bootstrap").map_err(io::Error::other)?,
                                    std::mem::take(shards),
                                )
                                .map_err(io::Error::other)?;
                                cache.inherit_settings(&self.cache.borrow());
                                local.staged = Some(cache);
                                local.building = None;
                                ack = true;
                            }
                        }
                    }
                    Phase::Fence => {
                        self.storage_maintenance(true);
                        ack = self.cache.borrow_mut().seal_maintenance();
                    }
                    Phase::Install => {
                        if !self.maintenance || !self.cache.borrow().maintenance_idle() {
                            return Err(io::Error::other("storage install without drained fence"));
                        }
                        let mut cache = local
                            .staged
                            .take()
                            .ok_or_else(|| io::Error::other("missing staged cache"))?;
                        cache.maintenance(true);
                        local.retiring =
                            Some(std::mem::replace(&mut *self.cache.borrow_mut(), cache));
                        ack = true;
                    }
                    Phase::Release | Phase::Abort => {
                        local.staged = None;
                        local.building = None;
                        if !self.stopping && !shared.stopping.load(Ordering::Acquire) {
                            self.storage_maintenance(false);
                        }
                        ack = true;
                    }
                    Phase::Idle => {}
                }
            }
            if ack {
                let mut state = shared.transaction.lock().unwrap();
                if state.id == local.id && state.phase == phase {
                    state.ack[self.worker] = true;
                }
            }
            Ok(())
        })();
        if let Err(error) = result {
            if phase == Phase::Install {
                // No partial resume after durable publication. Driver failure
                // invokes supervised process shutdown, then startup recovers it.
                self.storage = Some(local);
                return Err(error);
            }
            local.building = None;
            local.staged = None;
            let mut state = shared.transaction.lock().unwrap();
            if state.id == local.id && state.phase == phase {
                state.failure = Some(error.to_string());
            }
        }
        let runnable = local.building.is_some() || local.retiring.is_some();
        self.storage = Some(local);
        Ok(uring::Work {
            runnable,
            deadline: Some(crate::environment::now() + TICK),
        })
    }
}

#[cfg(test)]
#[path = "../../tests/runtime/storage.rs"]
mod tests;

#[cfg(test)]
#[path = "../../tests/runtime/storage_memory.rs"]
mod memory_tests;
