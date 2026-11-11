//! GPU landing registry: which GPU memory `gpu-read` pages may land in over
//! RDMA (GPUDirect RDMA), owned by the GPU side.
//!
//! The RDMA link node holds a handle and drives it: configuration from the
//! GPU helper's start, per-round registration of each rail's landing slice,
//! and exhaustion counts from grants. GPU telemetry reads it directly.
//! Locks are only held for map updates, never across an await.

use crate::gpu::GpuHelper;
use crate::gpu::GpuLanding;
use std::collections::HashMap;
use std::sync::Arc;
use std::sync::Mutex;
use std::sync::MutexGuard;
use std::sync::atomic::AtomicBool;
use std::sync::atomic::Ordering;

/// Landing state shared by the link node and GPU telemetry.
#[derive(Default)]
pub struct GpuLandingRegistry {
    /// GPU helper and per-GPU landing bytes, when landing is on.
    config: Mutex<Option<(Arc<GpuHelper>, u64)>>,

    /// Landing was configured after the current rails attached theirs, so
    /// the owner must attach landings on the rails already active. Set and
    /// cleared under the `config` lock, so an activation that reads the new
    /// configuration consumes it and rails never attach twice.
    pending: AtomicBool,

    /// Rails active this round, for metrics.
    rails: Mutex<Vec<u16>>,

    /// Landing slices registered this round, by rail and GPU.
    landings: Mutex<HashMap<(u16, i32), Arc<GpuLanding>>>,

    /// NIC slots in each registered landing slice this round, by rail and GPU.
    slots: Mutex<HashMap<(u16, i32), usize>>,

    /// Landing grants that found no free slot, by rail and GPU. Kept across
    /// rounds so the metric only grows.
    exhausted: Mutex<HashMap<(u16, i32), u64>>,
}

impl GpuLandingRegistry {
    fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
        mutex
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
    }

    /// Turn landing on with `bytes` per GPU (0 turns it off), and mark the
    /// rails already active as pending.
    pub fn configure(&self, helper: Arc<GpuHelper>, bytes: u64) {
        let mut config = Self::lock(&self.config);
        *config = (bytes != 0).then_some((helper, bytes));
        self.pending.store(config.is_some(), Ordering::Release);
    }

    /// Whether landing still needs attaching on the rails already active.
    /// Clears the mark; the caller must then attach them.
    pub fn take_pending(&self) -> bool {
        let _config = Self::lock(&self.config);
        self.pending.swap(false, Ordering::AcqRel)
    }

    /// The configuration, consuming any pending mark: the caller attaches
    /// with it.
    pub fn take_config(&self) -> Option<(Arc<GpuHelper>, u64)> {
        let config = Self::lock(&self.config);
        self.pending.store(false, Ordering::Release);
        config.clone()
    }

    /// Whether landing is configured (GPU decrypt and RDMA both on).
    pub fn configured(&self) -> bool {
        Self::lock(&self.config).is_some()
    }

    /// Record that `slice` with `slots` NIC slots is registered on `rail`.
    pub fn register(&self, rail: u16, ordinal: i32, slice: Arc<GpuLanding>, slots: usize) {
        Self::lock(&self.landings).insert((rail, ordinal), slice);
        Self::lock(&self.slots).insert((rail, ordinal), slots);
    }

    /// GPU `ordinal`'s landing slice on `rail` this round, if any.
    pub fn landing(&self, rail: u16, ordinal: i32) -> Option<Arc<GpuLanding>> {
        Self::lock(&self.landings).get(&(rail, ordinal)).cloned()
    }

    /// Set this round's active rails.
    pub fn set_rails(&self, mut rails: Vec<u16>) {
        rails.sort_unstable();
        rails.dedup();
        *Self::lock(&self.rails) = rails;
    }

    /// Forget this round's rails and registrations (not the counts).
    pub fn clear_round(&self) {
        Self::lock(&self.landings).clear();
        Self::lock(&self.slots).clear();
        Self::lock(&self.rails).clear();
    }

    /// Landing state of each active rail, for metrics: the rail, and the
    /// `(ordinal, slots)` of each GPU landing slice registered on it.
    pub fn state(&self) -> Vec<(u16, Vec<(i32, usize)>)> {
        let slots = Self::lock(&self.slots);
        Self::lock(&self.rails)
            .iter()
            .map(|&rail| {
                let mut gpus: Vec<(i32, usize)> = slots
                    .iter()
                    .filter(|((r, _), _)| *r == rail)
                    .map(|(&(_, ordinal), &count)| (ordinal, count))
                    .collect();
                gpus.sort_unstable();
                (rail, gpus)
            })
            .collect()
    }

    /// Count one landing grant on `rail` for GPU `ordinal` that found no
    /// free slot.
    pub fn count_exhausted(&self, rail: u16, ordinal: i32) {
        let mut counts = Self::lock(&self.exhausted);
        let count = counts.entry((rail, ordinal)).or_default();
        *count = count.saturating_add(1);
    }

    /// Exhaustion counts for metrics: every slice registered this round (0
    /// if none yet) and every slice that ever exhausted, sorted by rail then
    /// GPU ordinal.
    pub fn exhausted(&self) -> Vec<((u16, i32), u64)> {
        let counts = Self::lock(&self.exhausted);
        let mut keys: Vec<(u16, i32)> = Self::lock(&self.slots)
            .keys()
            .chain(counts.keys())
            .copied()
            .collect();
        keys.sort_unstable();
        keys.dedup();
        keys.into_iter()
            .map(|key| (key, counts.get(&key).copied().unwrap_or(0)))
            .collect()
    }
}
