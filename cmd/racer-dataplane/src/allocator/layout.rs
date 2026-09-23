// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Resource accounting for new layouts. Existing RACERS06 layouts remain valid
//! under their original geometry; planning never rewrites an existing inode.
use super::*;

pub const MIN_CAPACITY: u64 = 8 * WIDE;
/// Deliberately below the approximately 62 GiB RACERS06 root bitmap limit.
pub const TARGET_SHARD_SIZE: u64 = 16 * 1024 * 1024 * 1024;
/// Tested sparse/bitmap envelope, not a claim of full-device throughput or RSS.
pub const MAX_CAPACITY: u64 = 4 * 1024 * 1024 * 1024 * 1024;
pub const MAX_PLANNED_SHARDS: usize = 1024;

/// Validated, allocation-free plan for a fresh storage generation. Capacity is
/// the file length; an aligned tail smaller than shard_count * 64 MiB is unused.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct LayoutPlan {
    capacity: u64,
    shards: usize,
    workers: usize,
}
impl LayoutPlan {
    pub fn new(capacity: u64, workers: usize) -> io::Result<Self> {
        if !(MIN_CAPACITY..=MAX_CAPACITY).contains(&capacity) || !capacity.is_multiple_of(WIDE) {
            return Err(invalid(
                "planned capacity must be 512 MiB..=4 TiB, aligned to 64 MiB",
            ));
        }
        if workers == 0 || workers > MAX_PLANNED_SHARDS {
            return Err(invalid("planned worker count must be 1..=1024"));
        }
        let shards = (capacity.div_ceil(TARGET_SHARD_SIZE) as usize).max(workers);
        Geometry::new(capacity, shards, 0)?;
        Ok(Self {
            capacity,
            shards,
            workers,
        })
    }
    pub fn capacity(self) -> u64 {
        self.capacity
    }
    pub fn shard_count(self) -> usize {
        self.shards
    }
    pub fn worker_count(self) -> usize {
        self.workers
    }
    pub fn shard_size(self) -> u64 {
        self.geometry().len
    }
    pub fn unused_tail_bytes(self) -> u64 {
        self.capacity - self.shard_size() * self.shards as u64
    }
    pub fn authorize(
        self,
        placement: &crate::sharding::Placement,
    ) -> io::Result<crate::sharding::StorageGeneration> {
        let generation = placement.storage_generation(self.shards)?;
        if generation.worker_count() != self.workers {
            return Err(invalid(
                "layout planned for a different execution worker count",
            ));
        }
        Ok(generation)
    }
    /// Creates only at an unused path, persisting the existing RACERL01 placement
    /// contract alongside RACERS06 roots. Atomic replacement belongs to runtime.
    pub fn create(self, path: impl AsRef<Path>, budget: CheckpointBudget) -> io::Result<Slab> {
        let mut slab = Slab::create_inner(
            path.as_ref(),
            self.capacity,
            self.shards,
            Some(Layout::new(self.capacity, self.shards, self.workers)?),
            #[cfg(test)]
            |_| Ok(()),
        )?;
        slab.set_checkpoint_budget(budget)?;
        Ok(slab)
    }
    pub fn resources(self) -> ResourceEstimate {
        ResourceEstimate::for_geometry(self.geometry(), self.shards)
    }
    /// Incremental setup allowance for an EMPTY replacement, not a populated
    /// second cache. open_empty allocates bitmap backing and empty roots only.
    /// Double bitmap bytes for allocation overhead, allow 64 KiB per shard for
    /// roots, descriptors, assignment packets and cache bookkeeping, and 1 MiB
    /// per worker for setup stacks/scratch. No payload buffers are added.
    pub fn empty_preparation_bytes(self) -> u64 {
        2 * self.resources().allocation_bitmap_bytes
            + self.shards as u64 * (64 << 10)
            + self.workers as u64 * (1 << 20)
    }
    fn geometry(self) -> Geometry {
        Geometry::new(self.capacity, self.shards, 0).expect("validated layout")
    }
}

/// Structural accounting in bytes, not measured RSS or an admission budget.
/// Includes Vec spare capacity, Rc/Arc headers and hash-table load/slack; excludes
/// malloc bookkeeping/fragmentation, external lease holders, thread stacks,
/// network state, registered pools and OS page cache. No large tree is allocated
/// up front: payload indexes and admitted metadata grow with actual use.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ResourceEstimate {
    pub payload_extents: u64,
    pub metadata_entries: u64,
    /// Exact initialized allocation bitmap word/summary backing bytes.
    pub allocation_bitmap_bytes: u64,
    /// Both durable roots' bitmap pages, allocations and vector capacity.
    pub retained_bitmap_bytes: u64,
    /// Full live + two durable tree versions, payload descriptors, heat/map.
    pub resident_index_bytes: u64,
    /// Extra frozen tree, encoded page jobs, scratch bits and duplicate bitmap
    /// pages, for at most CheckpointBudget::CONCURRENT shards.
    pub checkpoint_peak_bytes: u64,
    pub checkpoint_per_shard_bytes: u64,
    /// Recovery scratch bitmap and allocation interning per opening worker.
    pub recovery_scratch_per_worker: u64,
    pub shard_fixed_bytes: u64,
}
impl ResourceEstimate {
    pub fn steady_bytes(self) -> u64 {
        self.allocation_bitmap_bytes
            + self.retained_bitmap_bytes
            + self.resident_index_bytes
            + self.shard_fixed_bytes
    }
    pub fn checkpoint_peak_total_bytes(self) -> u64 {
        self.steady_bytes() + self.checkpoint_peak_bytes
    }
    /// Runtime holds at most active + one prepared/retiring generation. Share a
    /// checkpoint budget and retire before starting another replacement. Taking
    /// the larger per-shard cost covers any distribution of shared permits,
    /// including a one-shard old generation alongside a multi-shard new one.
    pub fn replacement_peak_bytes(self, next: Self) -> u64 {
        self.steady_bytes()
            + next.steady_bytes()
            + CheckpointBudget::CONCURRENT as u64
                * self
                    .checkpoint_per_shard_bytes
                    .max(next.checkpoint_per_shard_bytes)
    }
    /// Add this to the replacement estimate while workers synchronously open
    /// the new slab. Workers must open their local shards sequentially.
    pub fn recovery_scratch_bytes(self, workers: usize) -> u64 {
        self.recovery_scratch_per_worker
            .saturating_mul(workers as u64)
    }
    pub(super) fn for_geometry(g: Geometry, shards: usize) -> Self {
        let n = shards as u64;
        let payload = g.range(Class::Payload).1 as u64;
        let metadata = g.metadata_limit() as u64;
        let entries = payload + metadata;
        let bitmap_pages = g.pages().div_ceil(8).div_ceil(BIT_BYTES) as u64;
        let allocation = rc_bytes::<Allocation>() + rc_bytes::<()>();
        // Deletion may leave sparse non-root leaves. Use the same 2N+1 node
        // bound as physical admission, not the typical 8..15-entry occupancy.
        // Vec backing can retain 2*FANOUT capacity after split/merge/removal.
        let nodes = 2 * entries + 1;
        let node = rc_bytes::<Node>()
            + allocation
            + 2 * FANOUT as u64 * std::mem::size_of::<(Key, Entry)>() as u64;
        let tree = nodes * node + payload * (rc_bytes::<PayloadExtent>() + allocation);
        // Vec doubling and at most 4 buckets/entry including hash control bytes.
        let heat = entries
            * (2 * std::mem::size_of::<Heat>() as u64
                + 4 * (std::mem::size_of::<(Key, usize)>() as u64 + 1))
            + 2 * (nodes + payload)
                * std::mem::size_of::<(Class, usize, std::sync::Weak<()>)>() as u64;
        let bitmap = bitmap_pages
            * (PAGE_SIZE as u64
                + allocation
                + 2 * std::mem::size_of::<(Rc<Allocation>, Box<uring::Page>)>() as u64);
        let jobs =
            (nodes + bitmap_pages) * (PAGE_SIZE as u64 + 2 * std::mem::size_of::<Job>() as u64);
        let scratch = g.pages().div_ceil(8) as u64;
        let maps = [Class::Index, Class::Payload]
            .into_iter()
            .map(|c| {
                let count = g.range(c).1;
                ((count.div_ceil(64) + count.div_ceil(4096)) * 8) as u64
            })
            .sum::<u64>();
        Self {
            payload_extents: payload * n,
            metadata_entries: metadata * n,
            allocation_bitmap_bytes: maps * n,
            retained_bitmap_bytes: 2 * bitmap * n,
            resident_index_bytes: (3 * tree + heat) * n,
            checkpoint_peak_bytes: (tree + bitmap + jobs + scratch)
                * n.min(CheckpointBudget::CONCURRENT as u64),
            checkpoint_per_shard_bytes: tree + bitmap + jobs + scratch,
            recovery_scratch_per_worker: scratch
                + 4 * (2 * nodes + payload + 2 * bitmap_pages)
                    * (std::mem::size_of::<(usize, (u64, Weak<Allocation>))>() as u64 + 1),
            shard_fixed_bytes: n * (std::mem::size_of::<Allocator>() as u64 + rc_bytes::<Space>()),
        }
    }
}
fn rc_bytes<T>() -> u64 {
    (std::mem::size_of::<T>() + 2 * std::mem::size_of::<usize>()) as u64
}

#[cfg(test)]
#[path = "../../tests/storage/layout.rs"]
mod tests;
