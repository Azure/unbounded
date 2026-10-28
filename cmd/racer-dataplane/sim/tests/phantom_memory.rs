//! Measures per-node heap cost of phantom placement, replayed routes, and
//! payload-free disk entries, for simulator memory planning.

use page_alloc::{PhantomEntries, SegmentId};
use racer_sim::phantom::{Phantom, SharedRoutes};
use std::{
    alloc::{GlobalAlloc, Layout, System},
    num::NonZeroU32,
    sync::{
        Mutex,
        atomic::{AtomicUsize, Ordering},
    },
    task::{Context, Poll, Waker},
};
use topology::{Member, Membership, PathQuery, Paths, Placement, Rankings};

/// Poll a future to completion with a no-op waker.
fn block_on<F: Future>(future: F) -> F::Output {
    let mut future = std::pin::pin!(future);
    let mut cx = Context::from_waker(Waker::noop());
    loop {
        if let Poll::Ready(value) = future.as_mut().poll(&mut cx) {
            return value;
        }
    }
}

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

/// Tests share `LIVE`, so they measure one at a time.
static SERIAL: Mutex<()> = Mutex::new(());

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
    let _serial = SERIAL.lock().unwrap();
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

/// Payload-free disk entries stay within the simulator's per-mapping charge.
#[test]
fn phantom_entries_stay_small_per_page() {
    let _serial = SERIAL.lock().unwrap();
    let pages = 100_000u64;
    let before = LIVE.load(Ordering::Relaxed);
    let entries = PhantomEntries::<u64>::default();
    for page in 0..pages {
        entries.publish(page, 4096, SegmentId(page / 64)).unwrap();
    }
    let used = LIVE.load(Ordering::Relaxed) - before;
    let per_page = used / usize::try_from(pages).unwrap();
    println!("phantom segment entries: {per_page} bytes per page");
    assert!(per_page <= 160, "{per_page}");
    drop(entries);
}
