// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use std::collections::HashSet;

#[test]
fn filesystem_admission_reports_headroom_and_keeps_retry_contract() {
    // The live resize regression grows to 20 GiB. A generic 1 GiB free-disk
    // prerequisite cannot cover its full index reserve, even for metadata.
    let plan = LayoutPlan::new(20 << 30, 1).unwrap();
    let geometry = plan.geometry();
    let required =
        geometry.range(Class::Index).1 as u64 * PAGE_SIZE as u64 * plan.shard_count() as u64
            + HEADROOM;
    assert_eq!(required, 2_751_447_040);
    let error = check_disk_headroom(Ok(1 << 30), required).unwrap_err();
    assert_eq!(error.kind(), io::ErrorKind::WouldBlock);
    assert!(error.to_string().contains("available=1073741824 bytes"));
    assert!(error.to_string().contains("required=2751447040 bytes"));
    assert!(check_disk_headroom(Ok(required - 1), required).is_err());
    assert!(check_disk_headroom(Ok(required), required).is_ok());
    assert!(check_disk_headroom(Ok(u64::MAX), u64::MAX).is_ok());
    let error =
        check_disk_headroom(Err(io::Error::other("statvfs fixture")), required).unwrap_err();
    assert_eq!(error.kind(), io::ErrorKind::WouldBlock);
    assert!(
        error
            .to_string()
            .contains("cannot query available bytes: statvfs fixture")
    );
}

#[derive(Debug, Default)]
struct TreeFootprint {
    nodes: u64,
    leaf_entries: u64,
    leaf_capacity: u64,
    node_bytes: u64,
    leaf_bytes: u64,
    branch_bytes: u64,
    allocation_bytes: u64,
    payload_bytes: u64,
    header_bytes: u64,
}
impl TreeFootprint {
    fn bytes(&self) -> u64 {
        self.node_bytes
            + self.leaf_bytes
            + self.branch_bytes
            + self.allocation_bytes
            + self.payload_bytes
            + self.header_bytes
    }
    fn allocation(a: &Rc<Allocation>, seen: &mut HashSet<usize>) -> u64 {
        if seen.insert(Rc::as_ptr(a) as usize) {
            rc_bytes::<Allocation>() + rc_bytes::<()>()
        } else {
            0
        }
    }
    fn tree(&mut self, node: &Rc<Node>, seen: &mut HashSet<usize>) {
        if !seen.insert(Rc::as_ptr(node) as usize) {
            return;
        }
        self.nodes += 1;
        self.node_bytes += rc_bytes::<Node>();
        self.allocation_bytes += node.disk.as_ref().map_or(0, |a| Self::allocation(a, seen));
        match &node.body {
            Body::Leaf(values) => {
                self.leaf_entries += values.len() as u64;
                self.leaf_capacity += values.capacity() as u64;
                self.leaf_bytes +=
                    values.capacity() as u64 * std::mem::size_of::<(Key, Entry)>() as u64;
                for (_, entry) in values {
                    match entry {
                        Entry::Metadata(metadata) => {
                            if let Some((address, bytes)) = metadata.header_allocation()
                                && seen.insert(address)
                            {
                                self.header_bytes += bytes;
                            }
                        }
                        Entry::Payload(payload) => {
                            if seen.insert(Rc::as_ptr(payload) as usize) {
                                self.payload_bytes += rc_bytes::<PayloadExtent>();
                                self.allocation_bytes +=
                                    Self::allocation(&payload.allocation, seen);
                            }
                        }
                    }
                }
            }
            Body::Branch(children) => {
                self.branch_bytes +=
                    children.capacity() as u64 * std::mem::size_of::<Rc<Node>>() as u64;
                for child in children {
                    self.tree(child, seen);
                }
            }
        }
    }
}

#[test]
fn compact_metadata_cow_versions_account_headers_and_preserve_disk_bytes() {
    assert!(std::mem::size_of::<(Key, Entry)>() <= 104);
    let original = Metadata {
        checksum: crate::metadata::Checksum([7; 32]),
        len: 123,
        expires: 456,
        content_type: crate::metadata::ContentType::new(&[b'x'; 256]).unwrap(),
    };
    let mut root = Rc::new(Node::empty());
    for n in 0..64u8 {
        let mut value = original;
        if n % 2 == 0 {
            value.content_type = Default::default();
        }
        if let Some(right) = Node::insert(&mut root, [n; 32], Entry::Metadata(value.into())) {
            root = Rc::new(Node {
                body: Body::Branch(vec![root, right]),
                disk: None,
            });
        }
    }
    let snapshot = root.clone();
    let mut before = TreeFootprint::default();
    before.tree(&snapshot, &mut HashSet::new());
    let header_bytes = 32 * ResidentMetadata::header_allocation_bytes(256);
    assert_eq!(before.header_bytes, header_bytes);
    assert_eq!(before.leaf_entries, 64);
    assert!(before.leaf_capacity >= before.leaf_entries);
    assert_eq!(
        before.leaf_bytes,
        before.leaf_capacity * std::mem::size_of::<(Key, Entry)>() as u64
    );

    let newer = Metadata {
        expires: 789,
        ..original
    };
    assert!(Node::insert(&mut root, [1; 32], Entry::Metadata(newer.into())).is_none());
    assert!(Node::remove(&mut root, &[3; 32]));
    let Entry::Metadata(old) = snapshot.get(&[1; 32]).unwrap() else {
        panic!("metadata")
    };
    let Entry::Metadata(current) = root.get(&[1; 32]).unwrap() else {
        panic!("metadata")
    };
    assert_eq!(old.to_metadata(), original);
    assert_eq!(current.to_metadata(), newer);
    assert!(snapshot.get(&[3; 32]).is_some());
    assert!(root.get(&[3; 32]).is_none());
    let mut both = TreeFootprint::default();
    let mut seen = HashSet::new();
    both.tree(&snapshot, &mut seen);
    both.tree(&root, &mut seen);
    // Unchanged headers share backing even in copied leaves; only the replaced
    // header adds an allocation. Removal cannot release the retained version.
    assert_eq!(
        both.header_bytes,
        header_bytes + ResidentMetadata::header_allocation_bytes(256)
    );
    let bytes = both.bytes();
    both.tree(&root, &mut seen);
    assert_eq!(both.bytes(), bytes);

    let leaf = Node {
        body: Body::Leaf(vec![([1; 32], Entry::Metadata(newer.into()))]),
        disk: None,
    };
    let page = encode(&leaf);
    assert_eq!(&page.0[72..32 + LEAF_ENTRY], &newer.to_bytes());
    assert_eq!(get(&page, 64), 0);
}

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
        assert!(resources.payload_extents > 25_000);
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
    // Counts are capacity-derived; transient 64 MiB buffers are never multiplied
    // by payload extent count or preallocated by the planner.
    assert_eq!(big.payload_extents, 57_344);
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

const CHILD_OUTPUT_LIMIT: usize = 64 * 1024;

#[derive(Default)]
struct ChildCapture {
    bytes: Vec<u8>,
    truncated: bool,
}
impl ChildCapture {
    fn drain(&mut self, input: &mut impl io::Read) -> io::Result<()> {
        let mut buffer = [0; 4096];
        // Bound work per poll as well as retained output: a noisy child must not
        // keep its supervisor from checking the deadline.
        for _ in 0..16 {
            let count = match input.read(&mut buffer) {
                Ok(0) => break,
                Ok(count) => count,
                Err(error) if error.kind() == io::ErrorKind::WouldBlock => break,
                Err(error) if error.kind() == io::ErrorKind::Interrupted => continue,
                Err(error) => return Err(error),
            };
            let keep = count.min(CHILD_OUTPUT_LIMIT - self.bytes.len());
            self.bytes.extend_from_slice(&buffer[..keep]);
            self.truncated |= keep != count;
        }
        Ok(())
    }
    fn text(&self) -> String {
        format!(
            "{}{}",
            String::from_utf8_lossy(&self.bytes),
            if self.truncated {
                "\n[output truncated]"
            } else {
                ""
            }
        )
    }
}

fn bounded_child(
    command: &mut std::process::Command,
    timeout: std::time::Duration,
) -> io::Result<(std::process::ExitStatus, ChildCapture, ChildCapture)> {
    use std::os::unix::process::CommandExt;
    use std::process::Stdio;
    use std::time::{Duration, Instant};

    // Kill the whole private group, including any descendant holding a pipe.
    // Never use wait_with_output or a blocking reader/join in cleanup.
    struct ChildGuard(std::process::Child);
    impl ChildGuard {
        fn kill_group(&mut self) {
            unsafe { libc::kill(-(self.0.id() as i32), libc::SIGKILL) };
        }
        fn reap(&mut self) -> io::Result<std::process::ExitStatus> {
            let deadline = Instant::now() + Duration::from_secs(5);
            loop {
                if let Some(status) = self.0.try_wait()? {
                    return Ok(status);
                }
                if Instant::now() >= deadline {
                    return Err(io::Error::new(
                        io::ErrorKind::TimedOut,
                        "child did not reap after SIGKILL",
                    ));
                }
                std::thread::sleep(Duration::from_millis(10));
            }
        }
    }
    impl Drop for ChildGuard {
        fn drop(&mut self) {
            self.kill_group();
            let _ = self.reap();
        }
    }
    fn nonblocking(fd: &impl AsRawFd) -> io::Result<()> {
        let flags = unsafe { libc::fcntl(fd.as_raw_fd(), libc::F_GETFL) };
        if flags < 0
            || unsafe { libc::fcntl(fd.as_raw_fd(), libc::F_SETFL, flags | libc::O_NONBLOCK) } < 0
        {
            return Err(io::Error::last_os_error());
        }
        Ok(())
    }

    let mut child = ChildGuard(
        command
            .process_group(0)
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()?,
    );
    let mut stdout = child.0.stdout.take().unwrap();
    let mut stderr = child.0.stderr.take().unwrap();
    nonblocking(&stdout)?;
    nonblocking(&stderr)?;
    let mut out = ChildCapture::default();
    let mut err = ChildCapture::default();
    let deadline = Instant::now() + timeout;
    loop {
        out.drain(&mut stdout)?;
        err.drain(&mut stderr)?;
        if let Some(status) = child.0.try_wait()? {
            out.drain(&mut stdout)?;
            err.drain(&mut stderr)?;
            return Ok((status, out, err));
        }
        if Instant::now() >= deadline {
            child.kill_group();
            let reaped = child.reap();
            out.drain(&mut stdout)?;
            err.drain(&mut stderr)?;
            return Err(io::Error::new(
                io::ErrorKind::TimedOut,
                format!(
                    "memory child exceeded {timeout:?}; reap={reaped:?}\nstdout:\n{}\nstderr:\n{}",
                    out.text(),
                    err.text()
                ),
            ));
        }
        std::thread::sleep(Duration::from_millis(10));
    }
}

#[test]
fn memory_child_capture_bounds_output_and_wait() {
    use std::time::{Duration, Instant};
    let mut capture = ChildCapture::default();
    let bytes = vec![b'x'; 3 * CHILD_OUTPUT_LIMIT];
    let mut input = io::Cursor::new(bytes);
    for _ in 0..4 {
        capture.drain(&mut input).unwrap();
    }
    assert_eq!(input.position(), (3 * CHILD_OUTPUT_LIMIT) as u64);
    assert_eq!(capture.bytes.len(), CHILD_OUTPUT_LIMIT);
    assert!(capture.truncated);
    for code in [0, 7] {
        let (status, out, err) = bounded_child(
            std::process::Command::new("sh").args([
                "-c",
                &format!("printf stdout; printf stderr >&2; exit {code}"),
            ]),
            Duration::from_secs(5),
        )
        .unwrap();
        assert_eq!(status.code(), Some(code));
        assert_eq!(out.bytes, b"stdout");
        assert_eq!(err.bytes, b"stderr");
    }
    let (status, out, err) = bounded_child(
        std::process::Command::new("sh").args([
            "-c",
            "i=0; while [ \"$i\" -lt 4096 ]; do printf '%064d' 0; printf '%064d' 0 >&2; i=$((i+1)); done",
        ]),
        Duration::from_secs(10),
    ).unwrap();
    assert!(status.success());
    assert_eq!(out.bytes.len(), CHILD_OUTPUT_LIMIT);
    assert_eq!(err.bytes.len(), CHILD_OUTPUT_LIMIT);
    assert!(out.truncated && err.truncated);
    // A descendant can outlive the direct child and hold both pipes open.
    // Collecting output must not wait for EOF from that descendant.
    let (status, _, _) = bounded_child(
        std::process::Command::new("sh").args(["-c", "sleep 30 & exit 0"]),
        Duration::from_secs(5),
    )
    .unwrap();
    assert!(status.success());
    let start = Instant::now();
    let error = bounded_child(
        std::process::Command::new("sh").args(["-c", "exec sleep 30"]),
        Duration::from_millis(100),
    )
    .err()
    .expect("sleeping child must time out");
    assert_eq!(error.kind(), io::ErrorKind::TimedOut);
    assert!(start.elapsed() < Duration::from_secs(15));
}

#[test]
fn populated_two_tib_memory() {
    let (status, stdout, stderr) = bounded_child(
        std::process::Command::new(std::env::current_exe().unwrap())
            .args([
                "--exact",
                "allocator::layout::tests::populated_two_tib_memory_child",
                "--ignored",
                "--nocapture",
            ])
            .env("RACER_POPULATED_MEMORY_CHILD", "1"),
        std::time::Duration::from_secs(120),
    )
    .unwrap();
    println!("{}", stdout.text());
    assert!(status.success(), "{status}: {}", stderr.text());
    assert!(stdout.text().contains("1 passed"));
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
        Entry::Metadata(
            Metadata {
                content_type: Default::default(),
                checksum: crate::metadata::Checksum([revision as u8; 32]),
                len: n,
                expires: revision,
            }
            .into(),
        )
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
    fn structural(a: &Allocator, trees: &mut TreeFootprint) -> u64 {
        let mut seen = HashSet::new();
        let before = trees.bytes();
        trees.tree(&a.root, &mut seen);
        let mut bytes = std::mem::size_of::<Allocator>() as u64 + rc_bytes::<Space>();
        for c in a.checkpoints.iter().flatten() {
            trees.tree(&c.root, &mut seen);
            bytes += c.bitmaps.capacity() as u64
                * std::mem::size_of::<(Rc<Allocation>, Box<uring::Page>)>() as u64;
            for (a, _) in &c.bitmaps {
                bytes += PAGE_SIZE as u64 + TreeFootprint::allocation(a, &mut seen);
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
        bytes + trees.bytes() - before
    }
    let baseline = rss();
    let plan = LayoutPlan::new(2 << 40, 1).unwrap();
    let path = std::env::temp_dir().join(format!("racer-populated-memory-{}", std::process::id()));
    let mut slab = plan.create(&path, CheckpointBudget::default()).unwrap();
    std::fs::remove_file(path).unwrap();
    let mut allocators = Vec::new();
    let mut bytes = 0;
    let mut trees = TreeFootprint::default();
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
        bytes += structural(&a, &mut trees);
        allocators.push(a);
    }
    let delta = rss().saturating_sub(baseline);
    println!(
        "resident metadata={}, Entry={}, leaf slot={}, Rc<Node>={}; {trees:?}; non-tree bytes={}",
        std::mem::size_of::<ResidentMetadata>(),
        std::mem::size_of::<Entry>(),
        std::mem::size_of::<(Key, Entry)>(),
        rc_bytes::<Node>(),
        bytes - trees.bytes()
    );
    println!(
        "populated 2 TiB: {} payload descriptors, {} metadata entries, three tree versions: structural={bytes}, RSS high-water delta={delta}",
        plan.resources().payload_extents,
        plan.resources().metadata_entries
    );
    // Two times this index population (4 TiB), <512 MiB transaction headroom,
    // and 1.5 GiB non-storage allowance fit the static managed 4 GiB envelope.
    // This is representative coverage, not an adversarial tree/RSS guarantee.
    assert!(bytes < 1 << 30, "structural index bytes={bytes}");
    assert!(delta < 1 << 30, "RSS high-water delta={delta}");
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
            Entry::Metadata(
                Metadata {
                    content_type: crate::metadata::ContentType::new(&[b'x'; 256]).unwrap(),
                    checksum: crate::metadata::Checksum(key),
                    len: n,
                    expires: 100,
                }
                .into(),
            )
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
    fn tree_bytes(node: &Rc<Node>) -> u64 {
        let mut tree = TreeFootprint::default();
        tree.tree(node, &mut HashSet::new());
        tree.bytes()
    }
    let mut full_headers = TreeFootprint::default();
    full_headers.tree(&a.root, &mut HashSet::new());
    assert_eq!(
        full_headers.header_bytes,
        resources.metadata_entries * ResidentMetadata::header_allocation_bytes(256)
    );
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
