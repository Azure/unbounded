//! Measures per-node heap cost of phantom placement and replayed routes at
//! production default capacities, for simulator memory planning.

use futures::executor::block_on;
use std::{
    alloc::{GlobalAlloc, Layout, System},
    num::NonZeroU32,
    sync::atomic::{AtomicUsize, Ordering},
};
use topology::{Member, Membership, PathQuery, Paths, Phantom, Placement, Rankings, SharedRoutes};

/// Counts live heap bytes for this test binary.
struct Counting;

static LIVE: AtomicUsize = AtomicUsize::new(0);

// SAFETY: forwards to the system allocator and only adds counting.
unsafe impl GlobalAlloc for Counting {
    unsafe fn alloc(&self, layout: Layout) -> *mut u8 {
        LIVE.fetch_add(layout.size(), Ordering::Relaxed);
        // SAFETY: same layout contract as the caller's.
        unsafe { System.alloc(layout) }
    }

    unsafe fn dealloc(&self, ptr: *mut u8, layout: Layout) {
        LIVE.fetch_sub(layout.size(), Ordering::Relaxed);
        // SAFETY: same pointer and layout the caller allocated with.
        unsafe { System.dealloc(ptr, layout) }
    }
}

#[global_allocator]
static GLOBAL: Counting = Counting;

/// Test member.
struct Node(Vec<u8>);

impl Member for Node {
    const DOMAIN: &'static str = "phantom-memory";

    fn id(&self) -> &[u8] {
        &self.0
    }

    fn weight(&self) -> NonZeroU32 {
        NonZeroU32::new(4).unwrap()
    }
}

fn members(count: usize) -> Membership<Node> {
    Membership::new(
        (0..count)
            .map(|i| Node(format!("node-{i:06}").into_bytes()))
            .collect(),
    )
    .unwrap()
}

/// Production default: 16 MiB placement budget at 1024 bytes per entry.
const PLACEMENT_ENTRIES: usize = 16 * 1024 * 1024 / 1024;

#[test]
fn phantom_placement_and_routes_per_node_bytes() {
    let membership = members(64);
    let shared = Phantom::new();
    // Warm the shared memo outside the measured region.
    let warm = Placement::with_rankings(PLACEMENT_ENTRIES, shared.clone());
    for key in 0..PLACEMENT_ENTRIES as u64 * 2 {
        warm.rank(&membership, &key.to_be_bytes());
    }
    let before = LIVE.load(Ordering::Relaxed);
    let node = Placement::with_rankings(PLACEMENT_ENTRIES, shared.clone());
    for key in 0..PLACEMENT_ENTRIES as u64 * 2 {
        node.rank(&membership, &key.to_be_bytes());
    }
    let placement = LIVE.load(Ordering::Relaxed) - before;
    let per_entry = placement / PLACEMENT_ENTRIES;
    println!(
        "phantom placement: {placement} bytes for {PLACEMENT_ENTRIES} entries ({per_entry} B/entry)"
    );
    assert!(
        per_entry <= <Phantom as Rankings>::ENTRY_BYTES,
        "{per_entry}"
    );

    let graph = members(3000);
    let routes = SharedRoutes::new();
    let warm = Paths::with_searches(1024, 8 << 20, 8, routes.clone());
    let query = |to| PathQuery {
        from: 0,
        to,
        links: 8,
        visited: &[],
        blocked: &[],
        seed: &[],
    };
    for to in 1..1025 {
        block_on(warm.route(&graph, query(to))).unwrap();
    }
    let before = LIVE.load(Ordering::Relaxed);
    let node = Paths::with_searches(1024, 8 << 20, 8, routes.clone());
    for to in 1..1025 {
        block_on(node.route(&graph, query(to))).unwrap();
    }
    let paths = LIVE.load(Ordering::Relaxed) - before;
    println!(
        "replayed paths: {paths} bytes for {} entries ({} B/entry, {} charged)",
        node.cached_entries(),
        paths / node.cached_entries(),
        node.cached_bytes()
    );
    assert!(paths / node.cached_entries() <= 256);
    drop((node, warm));
}
