//! Table sizes, as a runtime parameter rather than a compile-time constant.
//!
//! Every fixed-size table in the runtime - the io_uring rings, the op slab, the buffer
//! pool, the hop rings and cells, the cache sketch - is sized for a machine that owns its
//! cores. A simulated cluster puts hundreds of nodes in one address space, so the same
//! tables have to be small. These used to be build-time forks, which meant
//! the two shapes could not coexist in one binary and a test could not choose.
//!
//! A [`Limits`] is installed per thread and read from there. It is `&'static`, so reading
//! it is a pointer load, and the hot paths cache it on the worker's `Local` instead. The
//! default is [`PRODUCTION`]; a simulated worker installs [`COMPACT`].

use std::cell::Cell;

/// Hardware queues each worker owns per exported device. Not a size knob: the tag layout
/// and the queue assignment both assume it, and it is the same in every world.
pub(crate) const QUEUES_PER_WORKER: usize = 2;

/// The table sizes one thread runs with.
#[derive(Copy, Clone, Debug)]
pub(crate) struct Limits {
    /// ublk tags per hardware queue.
    pub(super) queue_depth: u16,
    /// Devices one worker can hold open at once; also the fixed-file slots reserved for
    /// their char devices, so a device slot and a file slot are the same number.
    pub(super) max_devices: u16,
    /// Fixed-file slots per worker: `max_devices` char devices then the handler's disks.
    pub(super) file_slots: u32,
    /// Concurrent io_uring operations one worker can have outstanding.
    pub(super) ops_per_worker: u32,
    /// Submission queue entries per worker ring; the completion queue gets four times it.
    pub(super) sq_entries: u32,
    /// Messages one core-to-core hop ring holds before the sender must retry.
    pub(super) ring_slots: u32,
    /// Detached tasks one worker can have alive at once.
    pub(super) tasks: u32,
    /// Outstanding hop replies one worker can be waiting on.
    pub(super) hop_cells: u32,
    /// Buffer pool size classes, as `(bytes, count)`, smallest first.
    pub(super) pool_classes: &'static [(usize, usize)],
    /// Counters per count-min row when a core holds no cache slots, and the floor.
    pub(crate) cache_cols_min: usize,
    /// Counters per count-min row at the ceiling.
    pub(crate) cache_cols_max: usize,
    /// Reader-side width hints when a core holds no cache slots, and the floor.
    pub(crate) cache_hints_min: usize,
    /// Reader-side width hints at the ceiling.
    pub(crate) cache_hints_max: usize,
}

impl Limits {
    /// ublk tags one worker holds for one device, across all of its queues.
    pub(super) const fn tags_per_dev(&self) -> u32 {
        QUEUES_PER_WORKER as u32 * self.queue_depth as u32
    }

    /// First fixed-file slot the handler's disks may use; below it are char devices.
    pub(super) const fn disk_file_base(&self) -> u32 {
        self.max_devices as u32
    }

    /// Registered buffer indices reserved for guest request buffers. A tag id indexes
    /// straight into this range, so it is also the executor's future slab length.
    pub(super) const fn req_buf_slots(&self) -> u32 {
        self.max_devices as u32 * self.tags_per_dev()
    }

    /// First registered buffer index the worker's own pool occupies. An index at or above
    /// it names pool memory; below it names guest memory.
    pub(super) const fn pool_buf_base(&self) -> u32 {
        self.req_buf_slots()
    }

    /// Buffers one pool holds, summed over its classes.
    pub(super) const fn pool_bufs(&self) -> u32 {
        let mut n = 0;
        let mut i = 0;
        while i < self.pool_classes.len() {
            n += self.pool_classes[i].1;
            i += 1;
        }
        n as u32
    }

    /// Registered buffer slots one worker reserves: guest requests then pool memory.
    pub(super) const fn total_buf_slots(&self) -> u32 {
        self.req_buf_slots() + self.pool_bufs()
    }
}

/// Class 0 (4 KiB) is the metadata block, so it gets the most.
static PRODUCTION_CLASSES: [(usize, usize); 7] = [
    (4 << 10, 512),
    // A fabric accept is a page plus a 4 KiB trailer, so 8 KiB is as hot as the page class.
    (8 << 10, 512),
    (16 << 10, 64),
    (64 << 10, 16),
    (256 << 10, 4),
    (1 << 20, 4),
    (4 << 20, 2),
];

/// The same classes for a simulated worker; its lone huge buffer stages huge-page repair.
static COMPACT_CLASSES: [(usize, usize); 4] =
    [(4 << 10, 32), (8 << 10, 32), (16 << 10, 8), (4 << 20, 1)];

/// What the shipped binary runs with: sized for a core that owns its hardware.
pub(crate) static PRODUCTION: Limits = Limits {
    queue_depth: 64,
    max_devices: 64,
    file_slots: 1024,
    ops_per_worker: 4096,
    sq_entries: 4096,
    ring_slots: 256,
    tasks: 1024,
    hop_cells: 1024,
    pool_classes: &PRODUCTION_CLASSES,
    // A power of two so the index is a mask; as packed nibbles that is 128 KiB, which is
    // L2-resident. The cap keeps the grown sketch L3-resident.
    cache_cols_min: 1 << 16,
    cache_cols_max: 1 << 21,
    cache_hints_min: 1 << 16,
    cache_hints_max: 1 << 22,
};

/// What a simulated node runs with: every table shrunk so hundreds of nodes fit in one
/// address space. Shapes, ratios and code paths are unchanged; only the counts differ.
pub(crate) static COMPACT: Limits = Limits {
    queue_depth: 4,
    max_devices: 16,
    file_slots: 64,
    ops_per_worker: 256,
    sq_entries: 32,
    ring_slots: 32,
    tasks: 64,
    hop_cells: 64,
    pool_classes: &COMPACT_CLASSES,
    cache_cols_min: 1 << 10,
    cache_cols_max: 1 << 12,
    cache_hints_min: 1 << 10,
    cache_hints_max: 1 << 13,
};

/// What a thread runs with until something installs otherwise.
///
/// A node serving a real machine wants every table the machine can afford, so that is
/// what a thread gets by default. A simulated cluster installs [`COMPACT`] before it
/// builds anything, because hundreds of nodes have to share one address space.
const DEFAULT: &Limits = &PRODUCTION;

thread_local! {
    static CURRENT: Cell<&'static Limits> = const { Cell::new(DEFAULT) };
}

/// The limits this thread runs with. Hot paths should read the copy cached on `Local`.
pub(crate) fn limits() -> &'static Limits {
    CURRENT.with(|c| c.get())
}

/// Installs `l` for the calling thread, returning what it replaced. Every table this
/// thread has already built keeps the shape it was built with, so a caller changing
/// limits must do it before anything is constructed. Only the simulator calls this today:
/// a production thread runs on the default and never changes it.
pub(crate) fn install_limits(l: &'static Limits) -> &'static Limits {
    CURRENT.with(|c| c.replace(l))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn derived_sizes_follow_the_installed_limits() {
        assert_eq!(PRODUCTION.tags_per_dev(), 128);
        assert_eq!(PRODUCTION.req_buf_slots(), 64 * 128);
        assert_eq!(PRODUCTION.pool_buf_base(), PRODUCTION.req_buf_slots());
        assert_eq!(PRODUCTION.disk_file_base(), 64);
        assert_eq!(PRODUCTION.pool_bufs(), 512 + 512 + 64 + 16 + 4 + 4 + 2);
        assert_eq!(
            PRODUCTION.total_buf_slots(),
            PRODUCTION.req_buf_slots() + PRODUCTION.pool_bufs()
        );

        assert_eq!(COMPACT.tags_per_dev(), 8);
        assert_eq!(COMPACT.req_buf_slots(), 16 * 8);
        assert_eq!(COMPACT.pool_bufs(), 32 + 32 + 8 + 1);
    }

    #[test]
    fn pool_classes_are_ordered_and_the_largest_holds_a_huge_page() {
        for l in [&PRODUCTION, &COMPACT] {
            assert!(!l.pool_classes.is_empty());
            for w in l.pool_classes.windows(2) {
                assert!(w[0].0 < w[1].0, "classes must be strictly increasing");
            }
            let (largest, count) = *l.pool_classes.last().unwrap();
            assert_eq!(largest, 4 << 20);
            assert!(count >= 1);
        }
    }

    #[test]
    fn install_swaps_the_thread_local_and_restores() {
        // A fresh thread starts on the build's default and every read agrees with it.
        assert_eq!(limits().queue_depth, DEFAULT.queue_depth);

        let prev = install_limits(&PRODUCTION);
        assert_eq!(prev.queue_depth, DEFAULT.queue_depth);
        assert_eq!(limits().queue_depth, PRODUCTION.queue_depth);
        assert_eq!(limits().ring_slots, PRODUCTION.ring_slots);

        install_limits(&COMPACT);
        assert_eq!(limits().queue_depth, COMPACT.queue_depth);
        assert_eq!(limits().ring_slots, COMPACT.ring_slots);

        // Restoring is just another install: nothing is stacked or remembered for us.
        install_limits(prev);
        assert_eq!(limits().queue_depth, DEFAULT.queue_depth);
    }
}
