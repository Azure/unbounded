// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;

#[test]
fn planner_boundaries_and_worker_geometry() {
    for size in [
        0,
        MIN_CAPACITY - WIDE,
        MIN_CAPACITY + 1,
        MAX_CAPACITY + WIDE,
        u64::MAX,
    ] {
        assert!(LayoutPlan::new(size, 1).is_err());
    }
    assert!(LayoutPlan::new(MIN_CAPACITY, 0).is_err());
    assert!(LayoutPlan::new(MIN_CAPACITY, 2).is_err());
    assert!(LayoutPlan::new(MAX_CAPACITY, MAX_PLANNED_SHARDS + 1).is_err());
    for workers in [1, 2, 3, 32, 128, 1024] {
        for size in [
            MIN_CAPACITY * workers as u64,
            DEFAULT_SLAB_SIZE.max(MIN_CAPACITY * workers as u64),
            2 << 40,
            4 << 40,
        ] {
            let plan = LayoutPlan::new(size, workers).unwrap();
            assert!(plan.shard_count() >= workers);
            assert!((MIN_CAPACITY..=TARGET_SHARD_SIZE).contains(&plan.shard_size()));
            assert!(plan.shard_size() < MAX_SHARD_SIZE);
            assert!(plan.unused_tail_bytes() < plan.shard_count() as u64 * WIDE);
            let last = Geometry::new(size, plan.shard_count(), plan.shard_count() - 1).unwrap();
            assert_eq!(last.base + last.len + plan.unused_tail_bytes(), size);
        }
    }
    assert_eq!(LayoutPlan::new(2 << 40, 1).unwrap().shard_count(), 128);
    assert_eq!(LayoutPlan::new(4 << 40, 1).unwrap().shard_count(), 256);
}

#[test]
fn word_initialization_masks_every_boundary() {
    for count in [0, 1, 63, 64, 65, 4095, 4096, 4097, 65535] {
        let mut bitmap = Bitmap::new(count);
        assert_eq!(bitmap.free, count);
        assert_eq!(
            bitmap
                .words
                .iter()
                .map(|w| w.count_ones() as usize)
                .sum::<usize>(),
            count
        );
        for i in 0..count {
            assert_eq!(bitmap.take(), Some(i));
        }
        assert_eq!(bitmap.take(), None);
        assert!(bitmap.summary.iter().all(|w| *w == 0));
    }
}

#[test]
fn multi_tib_sparse_open_and_bitmap_accounting() {
    use std::os::unix::fs::MetadataExt;
    for size in [2 << 40, 4 << 40] {
        let plan = LayoutPlan::new(size, 1).unwrap();
        let path = std::env::temp_dir().join(format!("racer-layout-{}-{size}", std::process::id()));
        let mut slab = plan.create(&path, CheckpointBudget::default()).unwrap();
        let stat = std::fs::metadata(&path).unwrap();
        assert_eq!(stat.len(), size);
        assert!(
            stat.blocks() * 512 < 32 * 1024 * 1024,
            "fixture must remain sparse"
        );
        let mut allocators = Vec::new();
        let mut bitmap_bytes = 0;
        for id in 0..plan.shard_count() {
            let allocator =
                Allocator::open_inner(slab.take_shard(ShardId::at(id)).unwrap(), Config::default())
                    .unwrap();
            for map in &allocator.space.maps {
                let map = map.borrow();
                bitmap_bytes += ((map.words.capacity() + map.summary.capacity()) * 8) as u64;
            }
            assert!(allocator.is_empty());
            allocators.push(allocator);
        }
        let resources = plan.resources();
        assert_eq!(resources, slab.resources());
        assert_eq!(bitmap_bytes, resources.allocation_bitmap_bytes);
        assert!(bitmap_bytes < 32 * 1024 * 1024);
        assert!(resources.payload_extents > 400_000);
        assert_eq!(resources.metadata_entries, plan.shard_count() as u64 * 8192);
        println!("{size} sparse bytes: {resources:?}");
        assert!(
            slab.set_checkpoint_budget(CheckpointBudget::default())
                .is_err()
        );
        drop(allocators);
        drop(slab);
        assert!(Slab::open_existing_layout(&path, 2).is_err());
        let reopened = Slab::open_existing_layout(&path, 1).unwrap();
        assert_eq!(reopened.shard_count(), plan.shard_count());
        assert_eq!(reopened.size(), size);
        assert!(plan.create(&path, CheckpointBudget::default()).is_err());
        drop(reopened);
        std::fs::remove_file(path).unwrap();
    }
}

#[test]
fn shared_checkpoint_permits_release_on_drop_and_unwind() {
    let budget = CheckpointBudget::default();
    let first = budget.acquire().unwrap();
    let second = budget.clone().acquire().unwrap();
    assert!(budget.acquire().is_none());
    drop(first);
    let result = std::panic::catch_unwind(|| {
        let _permit = budget.acquire().unwrap();
        assert!(budget.acquire().is_none());
        panic!("preparation failed");
    });
    assert!(result.is_err());
    assert!(budget.acquire().is_some());
    drop(second);
    assert_eq!(budget.0.load(std::sync::atomic::Ordering::Acquire), 0);
}

#[test]
fn resource_accounting_scales_and_bounds_replacement_overlap() {
    let small = LayoutPlan::new(MIN_CAPACITY, 1).unwrap().resources();
    let big = LayoutPlan::new(MAX_CAPACITY, 1).unwrap().resources();
    assert!(big.steady_bytes() > small.steady_bytes());
    assert_eq!(
        small.replacement_peak_bytes(big),
        small.steady_bytes() + big.checkpoint_peak_total_bytes()
    );
    // Counts are capacity-derived; transient 4 MiB buffers are never multiplied
    // by payload extent count or preallocated by the planner.
    assert_eq!(big.payload_extents, 917_504);
    assert_eq!(big.metadata_entries, 2_097_152);
}

#[test]
fn managed_memory_envelope_covers_automatic_range_and_admission_headroom() {
    // Matches dataplaneResources in internal/operator/components/racer. Keep
    // this static: changing Site/Node capacity must never change the Pod spec.
    let mib = 1u64 << 20;
    let largest = LayoutPlan::new(MAX_CAPACITY, 1).unwrap().resources();
    // Full target shards and either side of shard-count boundaries cover the
    // sawtooth geometry. Include the existing managed 10 GiB creation layout.
    let mut capacities = vec![MIN_CAPACITY, DEFAULT_SLAB_SIZE];
    for shards in 1..=256 {
        let size = shards * TARGET_SHARD_SIZE;
        capacities.extend([size - WIDE, size]);
        if size < MAX_CAPACITY {
            capacities.push(size + WIDE);
        }
    }
    for capacity in capacities {
        let old = LayoutPlan::new(capacity, 1).unwrap().resources();
        assert!(old.steady_bytes() <= largest.steady_bytes());
        let empty = LayoutPlan::new(capacity, 1)
            .unwrap()
            .empty_preparation_bytes();
        assert!(empty < 50 * mib);
        assert!(empty + largest.checkpoint_peak_bytes + 64 * mib < 512 * mib);
    }
    println!(
        "managed 4 TiB: empty preparation={}, checkpoint drain allowance={}",
        LayoutPlan::new(MAX_CAPACITY, 1)
            .unwrap()
            .empty_preparation_bytes(),
        largest.checkpoint_peak_bytes
    );
}

#[test]
fn populated_two_tib_memory() {
    let output = std::process::Command::new(std::env::current_exe().unwrap())
        .args([
            "--exact",
            "allocator::layout::tests::populated_two_tib_memory_child",
            "--ignored",
            "--nocapture",
        ])
        .env("RACER_POPULATED_MEMORY_CHILD", "1")
        .output()
        .unwrap();
    println!("{}", String::from_utf8_lossy(&output.stdout));
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    assert!(String::from_utf8_lossy(&output.stdout).contains("1 passed"));
}

#[test]
#[ignore = "isolated RSS helper for populated_two_tib_memory"]
fn populated_two_tib_memory_child() {
    if std::env::var_os("RACER_POPULATED_MEMORY_CHILD").is_none() {
        return;
    }
    fn rss() -> u64 {
        std::fs::read_to_string("/proc/self/status")
            .unwrap()
            .lines()
            .find_map(|line| {
                line.strip_prefix("VmHWM:")?
                    .split_whitespace()
                    .next()?
                    .parse::<u64>()
                    .ok()
            })
            .unwrap()
            * 1024
    }
    fn key(n: u64, hashed: bool) -> Key {
        if hashed {
            *blake3::hash(&n.to_le_bytes()).as_bytes()
        } else {
            let mut key = [0; 32];
            key[..8].copy_from_slice(&n.to_be_bytes());
            key
        }
    }
    fn metadata(n: u64, revision: u64) -> Entry {
        Entry::Metadata(Metadata {
            checksum: crate::metadata::Checksum([revision as u8; 32]),
            len: n,
            expires: revision,
        })
    }
    fn checkpoint(a: &mut Allocator) {
        let Pipeline::Writes(writes) = a.prepare().unwrap() else {
            unreachable!()
        };
        // Retain real CoW roots and bitmap pages. This fixture measures index
        // memory only: it does not write payload or claim durable disk recovery.
        let (slot, checkpoint) = writes.into_checkpoint();
        a.checkpoints[slot] = Some(checkpoint);
    }
    fn structural(a: &Allocator) -> u64 {
        use std::collections::HashSet;
        fn allocation(a: &Rc<Allocation>, seen: &mut HashSet<usize>) -> u64 {
            if seen.insert(Rc::as_ptr(a) as usize) {
                rc_bytes::<Allocation>() + rc_bytes::<()>()
            } else {
                0
            }
        }
        fn tree(n: &Rc<Node>, seen: &mut HashSet<usize>) -> u64 {
            if !seen.insert(Rc::as_ptr(n) as usize) {
                return 0;
            }
            rc_bytes::<Node>()
                + n.disk.as_ref().map_or(0, |a| allocation(a, seen))
                + match &n.body {
                    Body::Leaf(v) => {
                        v.capacity() as u64 * std::mem::size_of::<(Key, Entry)>() as u64
                            + v.iter()
                                .map(|(_, e)| {
                                    if let Entry::Payload(p) = e
                                        && seen.insert(Rc::as_ptr(p) as usize)
                                    {
                                        rc_bytes::<PayloadExtent>()
                                            + allocation(&p.allocation, seen)
                                    } else {
                                        0
                                    }
                                })
                                .sum::<u64>()
                    }
                    Body::Branch(v) => {
                        v.capacity() as u64 * std::mem::size_of::<Rc<Node>>() as u64
                            + v.iter().map(|n| tree(n, seen)).sum::<u64>()
                    }
                }
        }
        let mut seen = HashSet::new();
        let mut bytes = tree(&a.root, &mut seen)
            + std::mem::size_of::<Allocator>() as u64
            + rc_bytes::<Space>();
        for c in a.checkpoints.iter().flatten() {
            bytes += tree(&c.root, &mut seen)
                + c.bitmaps.capacity() as u64
                    * std::mem::size_of::<(Rc<Allocation>, Box<uring::Page>)>() as u64;
            for (a, _) in &c.bitmaps {
                bytes += PAGE_SIZE as u64 + allocation(a, &mut seen);
            }
        }
        bytes += a.heat.capacity() as u64 * std::mem::size_of::<Heat>() as u64;
        // HashMap capacity reports entries, not buckets. Use a conservative
        // power-of-two bucket count including control bytes and load slack.
        bytes += (a.positions.capacity() + 1).next_power_of_two() as u64
            * (std::mem::size_of::<(Key, usize)>() as u64 + 1);
        for map in &a.space.maps {
            let map = map.borrow();
            bytes += ((map.words.capacity() + map.summary.capacity()) * 8) as u64;
        }
        bytes
    }
    let baseline = rss();
    let plan = LayoutPlan::new(2 << 40, 1).unwrap();
    let path = std::env::temp_dir().join(format!("racer-populated-memory-{}", std::process::id()));
    let mut slab = plan.create(&path, CheckpointBudget::default()).unwrap();
    std::fs::remove_file(path).unwrap();
    let mut allocators = Vec::new();
    let mut bytes = 0;
    for id in 0..plan.shard_count() {
        let mut a =
            Allocator::open_inner(slab.take_shard(ShardId::at(id)).unwrap(), Config::default())
                .unwrap();
        let g = a.space.geometry;
        let metadata_count = g.metadata_limit() as u64;
        for n in 0..metadata_count + g.range(Class::Payload).1 as u64 {
            let entry = if n < metadata_count {
                metadata(n, 1)
            } else {
                Entry::Payload(Rc::new(PayloadExtent {
                    allocation: a.space.allocate(Class::Payload).unwrap(),
                    info: ValueInfo {
                        kind: Kind::Payload,
                        len: BUFFER_SIZE,
                        crc64: 0,
                        expires: 0,
                    },
                    buffer: RefCell::new(None),
                    written: Cell::new(true),
                }))
            };
            a.put_entry(key(n, id % 2 == 0), entry);
        }
        checkpoint(&mut a);
        // Force distinct live and two retained metadata tree versions, with
        // payload descriptors shared as in production. Alternate sorted/hash
        // insertion to cover dense and split-heavy representative trees.
        for revision in [2, 3] {
            for n in 0..metadata_count {
                a.put_entry(key(n, id % 2 == 0), metadata(n, revision));
            }
            if revision == 2 {
                checkpoint(&mut a);
            }
        }
        bytes += structural(&a);
        allocators.push(a);
    }
    let delta = rss().saturating_sub(baseline);
    println!(
        "populated 2 TiB: {} payload descriptors, {} metadata entries, three tree versions: structural={bytes}, RSS high-water delta={delta}",
        plan.resources().payload_extents,
        plan.resources().metadata_entries
    );
    // Two times this index population (4 TiB), <512 MiB transaction headroom,
    // and 1.5 GiB non-storage allowance fit the static managed 4 GiB envelope.
    // This is representative coverage, not an adversarial tree/RSS guarantee.
    assert!(bytes < 1 << 30);
    assert!(delta < 1 << 30);
    std::hint::black_box(&allocators);
}

#[test]
fn populated_target_shard_structural_footprint_fits_accounting() {
    // Populate index descriptors without allocating/writing 16 GiB of payload.
    // This measures Rust container backing and object sizes, not allocator RSS.
    let plan = LayoutPlan::new(TARGET_SHARD_SIZE, 1).unwrap();
    let path = std::env::temp_dir().join(format!("racer-layout-footprint-{}", std::process::id()));
    let mut slab = plan.create(&path, CheckpointBudget::default()).unwrap();
    std::fs::remove_file(path).unwrap();
    let mut a =
        Allocator::open_inner(slab.take_shard(ShardId::at(0)).unwrap(), Config::default()).unwrap();
    let resources = plan.resources();
    for n in 0..resources.metadata_entries + resources.payload_extents {
        let mut key = [0; 32];
        key[..8].copy_from_slice(&n.to_be_bytes());
        let entry = if n < resources.metadata_entries {
            Entry::Metadata(Metadata {
                checksum: crate::metadata::Checksum(key),
                len: n,
                expires: 100,
            })
        } else {
            Entry::Payload(Rc::new(PayloadExtent {
                allocation: a.space.allocate(Class::Payload).unwrap(),
                info: ValueInfo {
                    kind: Kind::Payload,
                    len: 1,
                    crc64: 0,
                    expires: 0,
                },
                buffer: RefCell::new(None),
                written: Cell::new(true),
            }))
        };
        a.put_entry(key, entry);
    }
    fn tree_bytes(node: &Node) -> u64 {
        rc_bytes::<Node>()
            + match &node.body {
                Body::Leaf(values) => {
                    values.capacity() as u64 * std::mem::size_of::<(Key, Entry)>() as u64
                        + values
                            .iter()
                            .filter(|(_, e)| matches!(e, Entry::Payload(_)))
                            .count() as u64
                            * (rc_bytes::<PayloadExtent>()
                                + rc_bytes::<Allocation>()
                                + rc_bytes::<()>())
                }
                Body::Branch(children) => {
                    children.capacity() as u64 * std::mem::size_of::<Rc<Node>>() as u64
                        + children.iter().map(|c| tree_bytes(c)).sum::<u64>()
                }
            }
    }
    let measured = tree_bytes(&a.root)
        + a.heat.capacity() as u64 * std::mem::size_of::<Heat>() as u64
        + a.positions.capacity() as u64 * std::mem::size_of::<(Key, usize)>() as u64;
    println!(
        "populated 16 GiB shard live structural bytes (map control/slack excluded): {measured}"
    );
    assert!(measured > 1 << 20);
    assert!(measured < resources.resident_index_bytes);
    let pipeline = a.prepare().unwrap();
    let Pipeline::Writes(writes) = pipeline else {
        unreachable!()
    };
    let encoded_bytes = writes
        .jobs()
        .iter()
        .filter(|j| matches!(j, Job::Page(..)))
        .count() as u64
        * PAGE_SIZE as u64;
    assert!(encoded_bytes > 1 << 20);
    assert!(encoded_bytes + tree_bytes(&a.root) < resources.checkpoint_peak_bytes);
    println!("populated target shard encoded checkpoint pages: {encoded_bytes} bytes");
}
