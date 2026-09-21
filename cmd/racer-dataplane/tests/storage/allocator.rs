// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use crate::buffers::{self, WorkerPool};
use std::collections::BTreeMap;
use std::sync::atomic::{AtomicUsize, Ordering};

static NEXT: AtomicUsize = AtomicUsize::new(0);
struct Fixture {
    path: std::path::PathBuf,
}
impl Fixture {
    fn new() -> (Self, Slab) {
        let path = std::env::temp_dir().join(format!(
            "racer-allocator-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::Relaxed)
        ));
        let slab = Slab::create(&path, 64 * WIDE, 2).unwrap();
        (Self { path }, slab)
    }
}
impl Drop for Fixture {
    fn drop(&mut self) {
        let _ = std::fs::remove_file(&self.path);
    }
}
fn allocator(slab: &mut Slab) -> Allocator {
    Allocator::open_inner(
        slab.take_shard(ShardId::at(0)).unwrap(),
        Config {
            max_pending_values: 4096,
            ..Config::default()
        },
    )
    .unwrap()
}
fn key(n: u64) -> Key {
    let mut key = [0; 32];
    key[..8].copy_from_slice(&n.to_be_bytes());
    key
}
fn metadata(n: u64, expires: u64) -> Metadata {
    Metadata {
        checksum: crate::metadata::Checksum(key(n)),
        len: n,
        expires,
    }
}
fn buffer(pool: &WorkerPool, n: u64, len: usize, byte: u8) -> Buffer {
    let mut fill = pool.stage(buffers::Key::new(key(n))).unwrap();
    fill.as_mut_slice()[..len].fill(byte);
    fill.publish(len).unwrap()
}

#[test]
fn crc64_lengths_and_alignment() {
    let bytes: Vec<u8> = (0..buffers::BUFFER_SIZE + 15)
        .map(|i| (i ^ (i >> 8) ^ (i >> 16)) as u8)
        .collect();
    for offset in [0, 1, 7, 15] {
        for len in [
            0,
            1,
            7,
            8,
            15,
            16,
            17,
            31,
            32,
            33,
            63,
            64,
            65,
            127,
            128,
            129,
            255,
            256,
            257,
            4095,
            4096,
            4097,
            buffers::BUFFER_SIZE,
        ] {
            let input = &bytes[offset..offset + len];
            // Independent bitwise ECMA-182 reference exercises SIMD blocks and tails.
            let mut expected = 0u64;
            for &byte in input {
                expected ^= u64::from(byte) << 56;
                for _ in 0..8 {
                    expected = (expected << 1)
                        ^ if expected >> 63 != 0 {
                            0x42f0e1eba9ea3693
                        } else {
                            0
                        };
                }
            }
            assert_eq!(crc64(input), expected, "offset={offset} len={len}");
        }
    }
}

#[test]
fn crc_geometry_and_exclusive_shard_capabilities() {
    assert_eq!(crc64(b"123456789"), 0x6c40df5f0b497347);
    assert!(Geometry::new(1, 1, 0).is_err());
    assert!(Geometry::new(DEFAULT_SLAB_SIZE, 0, 0).is_err());
    let (fixture, mut slab) = Fixture::new();
    assert!(Slab::open(&fixture.path, 2).is_err());
    let a = allocator(&mut slab);
    assert!(slab.take_shard(ShardId::at(0)).is_err());
    let b =
        Allocator::open_inner(slab.take_shard(ShardId::at(1)).unwrap(), Config::default()).unwrap();
    assert!(a.space.geometry.base + a.space.geometry.len <= b.space.geometry.base);
    drop(slab);
    assert!(Slab::open(&fixture.path, 2).is_err());
    drop(a);
    drop(b);
    // Concurrent subprocess tests briefly inherit descriptors between fork
    // and exec (CLOEXEC closes them at exec). Allow that transient lock hold.
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(2);
    loop {
        match Slab::open(&fixture.path, 2) {
            Ok(_) => break,
            Err(error)
                if error.kind() == io::ErrorKind::WouldBlock
                    && std::time::Instant::now() < deadline =>
            {
                std::thread::sleep(std::time::Duration::from_millis(1));
            }
            Err(error) => panic!("slab lock not released: {error}"),
        }
    }
}

#[test]
fn bitmap_matches_reference_across_summary_boundaries() {
    let mut bitmap = Bitmap::new(13001);
    for i in 0..13001 {
        assert_eq!(bitmap.take(), Some(i));
    }
    assert_eq!(bitmap.take(), None);
    for i in (0..13001).step_by(3) {
        bitmap.release(i);
    }
    for i in (0..13001).step_by(3) {
        assert_eq!(bitmap.take(), Some(i));
    }
    assert_eq!(bitmap.free, 0);
}

#[test]
fn incompatible_slab_versions_are_rejected_without_modification() {
    let (fixture, slab) = Fixture::new();
    let g = Geometry::new(slab.size, 2, 0).unwrap();
    // Reject even a mixed-version pair rather than zeroing the old root as
    // ordinary corruption. The check precedes any recovery mutation.
    let mut old = magic(g, 2, 0, &[]);
    put(&mut old, 0, u64::from_le_bytes(*b"RACERS03"));
    seal(&mut old);
    slab.file.write_all_at(&old.0, g.offset(1)).unwrap();
    slab.file.sync_data().unwrap();
    let error = Allocator::open_inner(
        SlabShard {
            file: slab.file.clone(),
            pressure: slab.pressure.clone(),
            geometry: g,
        },
        Config::default(),
    )
    .err()
    .unwrap();
    assert!(error.to_string().contains("incompatible slab format"));
    assert_eq!(read_page(&slab.file, g, 1).unwrap().0, old.0);
    drop(slab);
    let error = Slab::open_or_create_layout(&fixture.path, 64 * WIDE, 2, 1)
        .err()
        .unwrap();
    assert!(
        error
            .to_string()
            .contains("no automatic migration/reformat")
    );
    let file = File::open(&fixture.path).unwrap();
    let mut after = [0; PAGE_SIZE];
    file.read_exact_at(&mut after, g.offset(1)).unwrap();
    assert_eq!(after, old.0);
}

#[test]
fn metadata_geometry_reserves_three_complete_checkpoints() {
    for size in [
        8 * WIDE,
        32 * WIDE,
        DEFAULT_SLAB_SIZE / WIDE * WIDE,
        MAX_SHARD_SIZE,
    ] {
        let g = Geometry::new(size, 1, 0).unwrap();
        let entries = g.metadata_limit() + g.range(Class::Payload).1;
        let bitmap_pages = g.pages().div_ceil(8).div_ceil(BIT_BYTES);
        assert!(3 * (2 * entries + 1 + bitmap_pages) <= g.range(Class::Index).1);
        assert!(g.metadata_limit() * 2048 <= RESIDENT_METADATA_BUDGET);
        assert_eq!(g.range(Class::Payload).0, g.index_end());
        assert!(32 + FANOUT * LEAF_ENTRY <= PAGE_SIZE);
    }
}

#[test]
fn metadata_bound_prefers_expired_and_replacements_do_not_grow() {
    let world = crate::simulation::World::new(63);
    let _scope = world.enter();
    let mut slab =
        Slab::simulated(crate::simulation::Disk::new(8 * WIDE), 8 * WIDE, 1, true).unwrap();
    let mut a = Allocator::open_inner(
        slab.take_shard(ShardId::at(0)).unwrap(),
        Config {
            eviction_samples: 4096,
            ..Config::default()
        },
    )
    .unwrap();
    let limit = a.metadata_capacity();
    for n in 0..limit as u64 {
        a.insert_metadata(key(n), metadata(n, if n == 0 { 10 } else { 100 }), 1)
            .unwrap();
    }
    for _ in 0..100 {
        a.record_hit(&key(0));
    }
    a.insert_metadata(key(999), metadata(999, 100), 10).unwrap();
    assert!(
        a.root.get(&key(0)).is_none(),
        "expiry outranks hotness in admission LFU"
    );
    assert_eq!(a.len(), limit);
    for n in 0..100 {
        a.insert_metadata(key(999), metadata(n, 100), 10).unwrap();
        assert_eq!(a.metadata_count, limit);
    }
    assert!(!a.insert_metadata(key(999), metadata(0, 0), 10).unwrap());
    assert_eq!(a.lookup_metadata(&key(999), 10), Some(metadata(99, 100)));
    assert_eq!(a.space.maps[Class::Payload.index()].borrow().free, 7);
    assert!(a.pending.is_empty());
    drop((a, slab));
    world.assert_clean();
}

#[test]
fn pressure_skips_response_file_pins_while_admitting_another_page() {
    fn flush(a: &mut Allocator, ring: &mut Ring, world: &crate::simulation::World) {
        for _ in 0..1000 {
            world.service_tick();
            ring.progress().unwrap();
            a.poll(ring, 32).unwrap();
            if a.is_idle() {
                return;
            }
        }
        panic!("allocator failed to finish scheduled publication");
    }
    let world = crate::simulation::World::new(61);
    let _scope = world.enter();
    world.enable_scheduler();
    let disk = crate::simulation::Disk::new(8 * WIDE);
    let mut slab = Slab::simulated(disk.clone(), 8 * WIDE, 1, true).unwrap();
    let mut a = Allocator::open_inner(
        slab.take_shard(ShardId::at(0)).unwrap(),
        Config {
            eviction_samples: 1024,
            ..Config::default()
        },
    )
    .unwrap();
    let pool = buffers::io_test_pool(2);
    let mut ring = Ring::http_test_ring(pool.clone(), uring::Config::default()).unwrap();
    let capacity = a.space.geometry.range(Class::Payload).1;
    assert_eq!(capacity, 7);
    for n in 0..capacity as u64 {
        a.insert_payload(key(n), buffer(&pool, n, 17, n as u8), None)
            .unwrap();
        flush(&mut a, &mut ring, &world);
    }
    // The 17-byte trailing page completed before the 4 MiB leading page.
    // Ordered HTTP delivery cannot release it before leading admission.
    let held = a.lookup(&key(0), 0).unwrap().ready().unwrap();
    for heat in &mut a.heat {
        heat.count = if heat.key == key(0) { 0 } else { 100 };
    }
    let bytes = buffer(&pool, 99, buffers::BUFFER_SIZE, 99);
    let error = a.insert_payload(key(99), bytes, None).unwrap_err();
    assert_eq!(error.error.kind(), io::ErrorKind::WouldBlock);
    assert!(
        a.root.get(&key(0)).is_some(),
        "evicted response-held extent"
    );
    assert_eq!(a.len(), capacity - 1, "must select a reclaimable victim");
    flush(&mut a, &mut ring, &world);
    assert_eq!(a.space.maps[Class::Payload.index()].borrow().free, 1);
    a.insert_payload(key(99), error.resource, None).unwrap();
    flush(&mut a, &mut ring, &world);
    let mut actual = [255; 17];
    disk.read_exact_at(&mut actual, held.offset()).unwrap();
    assert_eq!(actual, [0; 17], "held file changed during reclamation");
    // Genuine all-pinned capacity remains Busy without evicting every page
    // or scheduling futile checkpoint rotations.
    let pins: Vec<_> = a
        .heat
        .iter()
        .map(|h| {
            ReadLease {
                value: a.root.get(&h.key).unwrap().payload().unwrap().clone(),
            }
            .ready()
            .unwrap()
        })
        .collect();
    let generation = a.generation();
    let error = a
        .insert_payload(key(100), buffer(&pool, 100, 17, 100), None)
        .unwrap_err();
    assert_eq!(error.error.kind(), io::ErrorKind::WouldBlock);
    assert_eq!(a.len(), capacity);
    assert!(a.is_idle());
    assert_eq!(a.generation(), generation);
    drop((error, pins, held, a, slab));
    ring.shutdown().unwrap();
    drop(ring);
    pool.assert_recovered();
    world.assert_clean();
}

#[test]
fn tree_splits_merges_and_snapshot_versions_match_ordered_map() {
    let mut root = Rc::new(Node::empty());
    let mut reference = BTreeMap::new();
    // Thousands of shuffled insertions grow multiple internal levels without
    // allocating payload buffers or metadata extents.
    for n in 0..1800 {
        let n = (n * 997) % 1800;
        let value = Entry::Metadata(metadata(n, 100));
        if let Some(right) = Node::insert(&mut root, key(n), value.clone()) {
            root = Rc::new(Node {
                body: Body::Branch(vec![root, right]),
                disk: None,
            });
        }
        reference.insert(key(n), value);
    }
    let snapshot = root.clone();
    for n in (0..1800).step_by(2) {
        assert!(Node::remove(&mut root, &key(n)));
        reference.remove(&key(n));
    }
    let mut actual = Vec::new();
    root.visit(&mut |key, _| actual.push(*key));
    assert_eq!(actual, reference.keys().copied().collect::<Vec<_>>());
    for n in 0..1800 {
        assert!(snapshot.get(&key(n)).is_some());
        assert_eq!(root.get(&key(n)).is_some(), n % 2 != 0);
    }
}

#[test]
fn lfu_aging_expiration_and_class_aware_eviction() {
    let (_fixture, mut slab) = Fixture::new();
    let mut a = allocator(&mut slab);
    a.config.eviction_samples = 1024;
    a.config.aging_interval = 100;
    for n in 0..8 {
        a.insert_metadata(key(n), metadata(n, 100), 0).unwrap();
    }
    for _ in 0..90 {
        a.record_hit(&key(0));
    }
    assert_ne!(a.evict(Kind::Metadata, 0), Some(key(0)));
    a.insert_metadata(key(9), metadata(9, 10), 0).unwrap();
    assert_eq!(a.evict(Kind::Metadata, 11), Some(key(9)));
    a.insert_metadata(key(10), metadata(10, 12), 11).unwrap();
    assert!(a.lookup_metadata(&key(10), 12).is_none());
    assert!(a.evict(Kind::Payload, 0).is_none());
    let cold = a.heat.iter().find(|h| h.key != key(0)).unwrap().key;
    for _ in 0..2000 {
        a.record_hit(&cold);
    }
    a.record_hit(&key(0));
    assert_eq!(
        a.heat[a.positions[&key(0)]].count,
        1,
        "old hot entry must age out"
    );
    assert!(a.heat[a.positions[&cold]].count > 1);
}

#[test]
fn streaming_source_reuses_descriptor_without_pinning_completed_extents() {
    use std::os::fd::AsFd;
    let (_fixture, mut slab) = Fixture::new();
    let a = allocator(&mut slab);
    let allocation = a.space.allocate(Class::Payload).unwrap();
    let index = allocation.index;
    let value = FileValue {
        file: a.space._file.clone(),
        _pin: allocation.pin.clone(),
        offset: allocation.offset(),
        info: ValueInfo {
            kind: Kind::Payload,
            len: 1,
            crc64: 0,
            expires: 0,
        },
    };
    let mut source = FileSource::new(&value).unwrap();
    let descriptor = source.descriptor(&value).unwrap();
    let other = a.space.allocate(Class::Payload).unwrap();
    let next = FileValue {
        _pin: other.pin.clone(),
        offset: other.offset(),
        ..value.clone()
    };
    assert_eq!(
        descriptor.as_fd().as_raw_fd(),
        source.descriptor(&next).unwrap().as_fd().as_raw_fd()
    );
    drop((allocation, value));
    assert_eq!(a.space.allocate(Class::Payload).unwrap().index, index);

    let (_other_fixture, mut other_slab) = Fixture::new();
    let b = allocator(&mut other_slab);
    let allocation = b.space.allocate(Class::Payload).unwrap();
    let different = FileValue {
        file: b.space._file.clone(),
        _pin: allocation.pin.clone(),
        offset: allocation.offset(),
        info: next.info,
    };
    assert_ne!(
        descriptor.as_fd().as_raw_fd(),
        source.descriptor(&different).unwrap().as_fd().as_raw_fd()
    );
}

#[test]
fn shared_file_pin_defers_owner_thread_reclamation() {
    let (_fixture, mut slab) = Fixture::new();
    let a = allocator(&mut slab);
    let allocation = a.space.allocate(Class::Payload).unwrap();
    let index = allocation.index;
    let lease = FileValue {
        file: a.space._file.clone(),
        _pin: allocation.pin.clone(),
        offset: allocation.offset(),
        info: ValueInfo {
            kind: Kind::Payload,
            len: 1,
            crc64: 0,
            expires: 0,
        },
    };
    drop(allocation);
    let other = a.space.allocate(Class::Payload).unwrap();
    assert_ne!(index, other.index);
    std::thread::spawn(move || drop(lease)).join().unwrap();
    let reused = a.space.allocate(Class::Payload).unwrap();
    assert_eq!(index, reused.index);
}

#[test]
fn kernel_punch_preserves_slow_loopback_reader() {
    use std::io::Read;
    use std::net::{TcpListener, TcpStream};
    use std::os::fd::AsFd;
    use std::time::{Duration, Instant};
    let (_fixture, slab) = Fixture::new();
    let pool = buffers::io_test_pool(2);
    let mut ring = match Ring::http_test_ring(pool.clone(), uring::Config::default()) {
        Ok(ring) => ring,
        Err(e)
            if std::env::var_os("RACER_REQUIRE_URING").is_none()
                && matches!(
                    e.raw_os_error(),
                    Some(libc::EPERM | libc::ENOSYS | libc::EACCES)
                ) =>
        {
            return;
        }
        Err(e) => panic!("io_uring: {e}"),
    };
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let mut receiver = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
    let (sender, _) = listener.accept().unwrap();
    receiver
        .set_read_timeout(Some(Duration::from_secs(5)))
        .unwrap();
    let socket = uring::File::new(sender.as_fd().try_clone_to_owned().unwrap());
    let source = slab.file.descriptor().unwrap();
    let (read, write) = uring::File::pipe().unwrap();
    // Keep a TCP receive queue and a pipe populated across the punch. The
    // peer does not read until the same slab offsets contain new bytes.
    let offset = 4 * WIDE;
    let len = 64 * 1024;
    slab.file.write_all_at(&vec![71; len * 2], offset).unwrap();
    let deadline = Instant::now() + Duration::from_secs(5);
    let mut moved = 0;
    while moved < len {
        let mut fill = ring
            .splice(
                source.clone(),
                write.clone().into(),
                Some(FileOffset::new(offset + moved as u64).unwrap()),
                len - moved,
                Rc::new(()),
            )
            .unwrap();
        let n = loop {
            ring.progress().unwrap();
            if let Some(n) = ring.take_splice(&mut fill).unwrap() {
                break n.unwrap();
            }
            assert!(Instant::now() < deadline);
        };
        let mut sent = 0;
        while sent < n {
            let mut drain = ring
                .splice(
                    read.clone(),
                    socket.clone().into(),
                    None,
                    n - sent,
                    Rc::new(()),
                )
                .unwrap();
            sent += loop {
                ring.progress().unwrap();
                if let Some(n) = ring.take_splice(&mut drain).unwrap() {
                    break n.unwrap();
                }
                assert!(Instant::now() < deadline);
            };
        }
        moved += n;
    }
    let mut fill = ring
        .splice(
            source.clone(),
            write.clone().into(),
            Some(FileOffset::new(offset + len as u64).unwrap()),
            len,
            Rc::new(()),
        )
        .unwrap();
    let queued = loop {
        ring.progress().unwrap();
        if let Some(n) = ring.take_splice(&mut fill).unwrap() {
            break n.unwrap();
        }
        assert!(Instant::now() < deadline);
    };
    let mut punch = ring
        .punch_hole(
            source.clone().into(),
            FileOffset::new(offset).unwrap(),
            WIDE,
            Rc::new(()),
        )
        .unwrap();
    loop {
        ring.progress().unwrap();
        if let Some(result) = ring.take_punch(&mut punch).unwrap() {
            result.unwrap();
            break;
        }
        assert!(Instant::now() < deadline);
    }
    let replacement = buffer(&pool, 100, len * 2, 99);
    let mut writing = ring
        .write(
            source.into(),
            replacement,
            BufferRange::new(0..len * 2).unwrap(),
            FileOffset::new(offset).unwrap(),
        )
        .unwrap();
    loop {
        ring.progress().unwrap();
        if let Some(completion) = ring.take_write(&mut writing).unwrap() {
            assert_eq!(completion.result.unwrap(), len * 2);
            break;
        }
        assert!(Instant::now() < deadline);
    }
    let mut received = vec![0; len];
    receiver.read_exact(&mut received).unwrap();
    assert_eq!(
        received,
        vec![71; len],
        "socket references survive extent reuse"
    );
    let mut remaining = queued;
    while remaining > 0 {
        let mut drain = ring
            .splice(
                read.clone(),
                socket.clone().into(),
                None,
                remaining,
                Rc::new(()),
            )
            .unwrap();
        let n = loop {
            ring.progress().unwrap();
            if let Some(n) = ring.take_splice(&mut drain).unwrap() {
                break n.unwrap();
            }
            assert!(Instant::now() < deadline);
        };
        let mut received = vec![0; n];
        receiver.read_exact(&mut received).unwrap();
        assert_eq!(
            received,
            vec![71; n],
            "pipe references survive extent reuse"
        );
        remaining -= n;
    }
    let mut current = vec![0; len * 2];
    slab.file.read_exact_at(&mut current, offset).unwrap();
    assert_eq!(current, vec![99; len * 2]);
}

#[test]
fn kernel_checkpoint_read_and_abandoned_read() {
    let (fixture, mut slab) = Fixture::new();
    let mut a = allocator(&mut slab);
    let pool = buffers::io_test_pool(8);
    let mut ring = match Ring::http_test_ring(pool.clone(), uring::Config::default()) {
        Ok(ring) => ring,
        Err(error)
            if std::env::var_os("RACER_REQUIRE_URING").is_none()
                && matches!(
                    error.raw_os_error(),
                    Some(libc::EPERM | libc::ENOSYS | libc::EACCES)
                ) =>
        {
            eprintln!("allocator kernel test skipped: {error}");
            return;
        }
        Err(error) => panic!("io_uring setup: {error}"),
    };
    a.insert_payload(key(1), buffer(&pool, 1, PAGE_SIZE, 42), None)
        .unwrap();
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(5);
    while !a.is_idle() {
        ring.progress().unwrap();
        let work = a.poll(&mut ring, 32).unwrap();
        assert!(std::time::Instant::now() < deadline);
        if !work.runnable && !a.is_idle() {
            ring.wait(Some(deadline)).unwrap();
        }
    }
    let lease = a.lookup(&key(1), 0).unwrap();
    assert!(lease.buffer().is_none());
    let fill = pool.stage(buffers::Key::new(key(2))).unwrap();
    let mut handle = a.read(&mut ring, lease, fill).unwrap();
    loop {
        ring.progress().unwrap();
        a.poll(&mut ring, 32).unwrap();
        if let Some(result) = handle.take() {
            assert_eq!(result.unwrap().as_slice(), &[42; PAGE_SIZE]);
            break;
        }
        assert!(std::time::Instant::now() < deadline);
        ring.wait(Some(deadline)).unwrap();
    }
    let lease = a.lookup(&key(1), 0).unwrap();
    let fill = pool.stage(buffers::Key::new(key(3))).unwrap();
    drop(a.read(&mut ring, lease, fill).unwrap());
    // Both application ownership and observation disappear before target
    // completion. The ring alone must retain the slab's flock and extent.
    drop(a);
    drop(slab);
    assert!(Slab::open(&fixture.path, 2).is_err());
    drop(ring);
    // Ring teardown may intentionally leak unresolved requests, so reopening
    // is only asserted after ordinary successful checkpoint/recovery tests.
}

#[cfg(test)]
mod benchmark {
    //! Run: cargo test --release allocator::tests::benchmark::throughput -- --ignored --nocapture
    //! RACER_BENCH_DIR selects the parent of the temporary directory (default: temp_dir()).
    //! Measures checkpointed 4 MiB allocations, including pressure/eviction and concurrent
    //! metadata checkpoints across shards. Payload writes alone are elided. Two seconds of warmup,
    //! 18 seconds of measurement; setup, final drain and recovery are outside timing.
    use super::*;
    use crate::{buffers, workers};
    use std::num::NonZeroUsize;
    use std::path::PathBuf;
    use std::sync::{Mutex, OnceLock};
    use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

    const SLAB_SIZE: u64 = 4 * 1024 * 1024 * 1024;
    const WARMUP: Duration = Duration::from_secs(2);
    const MEASURE: Duration = Duration::from_secs(18);
    const BUDGET: usize = 64;

    struct TempDir(PathBuf);
    impl TempDir {
        fn new() -> io::Result<Self> {
            let parent =
                std::env::var_os("RACER_BENCH_DIR").map_or_else(std::env::temp_dir, PathBuf::from);
            let path = parent.join(format!(
                "racer-allocator-bench-{}-{}",
                std::process::id(),
                SystemTime::now()
                    .duration_since(UNIX_EPOCH)
                    .unwrap()
                    .as_nanos()
            ));
            std::fs::create_dir(&path)?;
            Ok(Self(path))
        }
    }
    impl Drop for TempDir {
        fn drop(&mut self) {
            let _ = std::fs::remove_dir_all(&self.0);
        }
    }

    // Exercise the production checkpoint state machine and RingIo for every metadata
    // write and fdatasync, completing only payload writes without touching the file.
    struct MetadataIo<'a>(RingIo<'a>);
    impl Storage for MetadataIo<'_> {
        type Ticket = IoTicket;

        fn submit(&mut self, job: Job) -> Result<IoTicket, uring::Rejected<Job>> {
            if let Job::Value(value) = job {
                assert_eq!(value.info.kind, Kind::Payload);
                value.written.set(true);
                value.buffer.borrow_mut().take();
                Ok(IoTicket::Sim(0))
            } else {
                self.0.submit(job)
            }
        }

        fn complete(&mut self, ticket: &mut IoTicket) -> io::Result<Option<io::Result<()>>> {
            if matches!(ticket, IoTicket::Sim(_)) {
                Ok(Some(Ok(())))
            } else {
                self.0.complete(ticket)
            }
        }
    }

    struct BenchShard {
        id: ShardId,
        allocator: Allocator,
        sequence: u64,
        allocations: u64,
        pending: u64,
    }

    struct ResultRow {
        id: ShardId,
        allocations: u64,
        generation: u64,
        entries: Vec<(Key, ValueInfo)>,
    }

    struct App {
        shards: Vec<BenchShard>,
        payload: Buffer,
        start: Arc<OnceLock<Instant>>,
        results: Arc<Mutex<Vec<ResultRow>>>,
    }
    impl App {
        fn progress(allocator: &mut Allocator, ring: &mut Ring) -> io::Result<bool> {
            allocator.progress(
                &mut MetadataIo(RingIo {
                    ring,
                    file: allocator.file.clone(),
                    space: allocator.space.clone(),
                }),
                BUDGET,
            )
        }
    }
    impl uring::Application for App {
        fn poll(&mut self, ring: &mut Ring, _: usize) -> io::Result<Work> {
            let Some(&start) = self.start.get() else {
                return Ok(Work {
                    runnable: true,
                    deadline: None,
                });
            };
            for shard in &mut self.shards {
                // Finish each batch before admitting another: otherwise LFU can evict
                // uncheckpointed entries and turn this into an in-memory churn test.
                if !shard.allocator.is_idle() {
                    Self::progress(&mut shard.allocator, ring)?;
                    continue;
                }
                let elapsed = start.elapsed();
                if (WARMUP..WARMUP + MEASURE).contains(&elapsed) {
                    shard.allocations += shard.pending;
                }
                shard.pending = 0;
                // Never overfill a batch and evict one of its unpersisted entries.
                // At zero free extents, one rejected insert drives normal LFU/rotation.
                let free = shard.allocator.space.maps[Class::Payload.index()]
                    .borrow()
                    .free;
                for _ in 0..free.clamp(1, BUDGET) {
                    let elapsed = start.elapsed();
                    if elapsed >= WARMUP + MEASURE {
                        break;
                    }
                    let mut key = [0; 32];
                    key[..8].copy_from_slice(&shard.sequence.to_be_bytes());
                    match shard
                        .allocator
                        .insert_payload(key, self.payload.clone(), Some(0))
                    {
                        Ok(()) => {
                            shard.sequence += 1;
                            shard.pending += 1;
                        }
                        Err(e) if e.error.kind() == io::ErrorKind::WouldBlock => break,
                        Err(e) => return Err(e.error),
                    }
                }
                Self::progress(&mut shard.allocator, ring)?;
            }
            // Always runnable during the workload; the runtime observes stop between turns.
            Ok(Work {
                runnable: true,
                deadline: None,
            })
        }

        fn shutdown(&mut self, ring: &mut Ring) -> io::Result<()> {
            let deadline = Instant::now() + Duration::from_secs(10);
            while self.shards.iter().any(|s| !s.allocator.is_idle()) {
                if Instant::now() >= deadline {
                    return Err(io::Error::new(
                        io::ErrorKind::TimedOut,
                        "checkpoint drain timed out",
                    ));
                }
                ring.progress()?;
                for shard in &mut self.shards {
                    Self::progress(&mut shard.allocator, ring)?;
                }
            }
            let mut results = self.results.lock().unwrap();
            for shard in &self.shards {
                let mut entries = Vec::new();
                shard
                    .allocator
                    .root
                    .visit(&mut |key, value| entries.push((*key, value.payload().unwrap().info)));
                results.push(ResultRow {
                    id: shard.id,
                    allocations: shard.allocations,
                    generation: shard.allocator.generation(),
                    entries,
                });
            }
            Ok(())
        }
    }

    #[test]
    #[ignore = "20-second allocator throughput benchmark with real local metadata I/O"]
    fn throughput() -> io::Result<()> {
        assert!(!cfg!(debug_assertions), "run this benchmark with --release");
        let cores = workers::physical_core_count()?;
        let shard_count = cores.max(workers::Config::default().shard_count.get());
        assert!(
            shard_count <= 128,
            "4 GiB supports at most 128 shards of 32 MiB"
        );
        let dir = TempDir::new()?;
        let path = dir.0.join("allocator.slab");
        let slab = Mutex::new(Slab::create(&path, SLAB_SIZE, shard_count)?);
        assert_eq!(std::fs::metadata(&path)?.len(), SLAB_SIZE);
        // One immutable dummy buffer per worker, shared by all its admissions. No
        // payload checksumming or 4 MiB copying is part of the measured workload.
        let pools = buffers::Pools::new(buffers::Config {
            buffers_per_node: NonZeroUsize::new(cores).unwrap(),
            ..buffers::Config::new(NonZeroUsize::new(cores).unwrap())
        });
        let start = Arc::new(OnceLock::new());
        let results = Arc::new(Mutex::new(Vec::new()));
        let factory_start = start.clone();
        let factory_results = results.clone();
        let workers = workers::Workers::start(
            workers::Config {
                shard_count: NonZeroUsize::new(shard_count).unwrap(),
            },
            move |placement| {
                let shards = placement
                    .shard_ids()
                    .iter()
                    .map(|&id| {
                        let shard = slab.lock().unwrap().take_shard(id)?;
                        Ok(BenchShard {
                            id,
                            allocator: Allocator::open(placement, shard, Config::default())?,
                            sequence: 0,
                            allocations: 0,
                            pending: 0,
                        })
                    })
                    .collect::<io::Result<Vec<_>>>()?;
                let pool = pools.for_worker(placement)?;
                let mut key = [0; 32];
                key[..8].copy_from_slice(&(placement.worker_id().0 as u64).to_be_bytes());
                let fill = pool.stage(buffers::Key::new(key)).unwrap();
                let payload = fill.publish(BUFFER_SIZE)?;
                let ring = Ring::new(placement, pool, uring::Config::default())?;
                uring::Driver::new(
                    ring,
                    App {
                        shards,
                        payload,
                        start: factory_start.clone(),
                        results: factory_results.clone(),
                    },
                    BUDGET,
                )
            },
        )?;
        assert_eq!(workers.placements().len(), cores);
        println!(
            "allocator: {cores} pinned workers, {shard_count} shards, 4 GiB slab at {}",
            path.display()
        );
        let began = Instant::now();
        start.set(began).unwrap();
        std::thread::sleep(WARMUP + MEASURE);
        workers.stop_handle().request_stop();
        workers.join()?;
        let drained = began.elapsed().saturating_sub(WARMUP + MEASURE);

        // Reopen from disk after every worker/ring has released the slab lock. Verify
        // the exact final live key/value metadata and generation for every shard.
        let mut slab = Slab::open(&path, shard_count)?;
        let results = results.lock().unwrap();
        assert_eq!(results.len(), shard_count);
        let mut total = 0;
        for row in results.iter() {
            assert!(row.allocations > 0 && row.generation > 2);
            let recovered = Allocator::open_inner(slab.take_shard(row.id)?, Config::default())?;
            assert_eq!(recovered.generation(), row.generation);
            let mut entries = Vec::new();
            recovered
                .root
                .visit(&mut |key, value| entries.push((*key, value.payload().unwrap().info)));
            assert_eq!(entries, row.entries, "shard {} recovery", row.id.index());
            total += row.allocations;
        }
        println!(
            "allocator: {:.0} allocations/sec aggregate ({total} checkpointed allocations in {:.1}s; drain {:.3}s); metadata recovery verified",
            total as f64 / MEASURE.as_secs_f64(),
            MEASURE.as_secs_f64(),
            drained.as_secs_f64()
        );
        Ok(())
    }
}

mod create_tests {
    //! Real ext4 namespace, flock and restart tests. Each slab is sparse (64 MiB
    //! logical, four checkpoint pages allocated); no io_uring or large buffers.
    use super::*;
    use std::os::unix::fs::MetadataExt;
    use std::path::PathBuf;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::time::{Duration, Instant};

    const SIZE: u64 = 16 * WIDE;
    const SHARDS: usize = 2;
    const STEPS: [CreateStep; 10] = [
        CreateStep::Created,
        CreateStep::Sized,
        CreateStep::Checkpoint(0, 0),
        CreateStep::Checkpoint(0, 1),
        CreateStep::Checkpoint(1, 0),
        CreateStep::Checkpoint(1, 1),
        CreateStep::BeforeFileSync,
        CreateStep::FileSynced,
        CreateStep::Published,
        CreateStep::DirectorySynced,
    ];

    struct Directory(PathBuf);
    impl Directory {
        fn new() -> Self {
            static NEXT: AtomicUsize = AtomicUsize::new(0);
            let path = std::env::temp_dir().join(format!(
                "racer-create-{}-{}",
                std::process::id(),
                NEXT.fetch_add(1, Ordering::Relaxed)
            ));
            std::fs::create_dir(&path).unwrap();
            Self(path)
        }
        fn slab(&self) -> PathBuf {
            self.0.join("cache.slab")
        }
        fn entries(&self) -> Vec<PathBuf> {
            std::fs::read_dir(&self.0)
                .unwrap()
                .map(|e| e.unwrap().path())
                .collect()
        }
    }
    impl Drop for Directory {
        fn drop(&mut self) {
            std::fs::remove_dir_all(&self.0).unwrap();
        }
    }

    fn published(step: CreateStep) -> bool {
        matches!(step, CreateStep::Published | CreateStep::DirectorySynced)
    }

    fn recover(path: &Path) {
        let mut slab = Slab::open(path, SHARDS).unwrap();
        assert_eq!(slab.size, SIZE);
        for shard in 0..SHARDS {
            // Exercise the real recovery parser, not just Slab::open's geometry check.
            let mut allocator = Allocator::open_inner(
                slab.take_shard(ShardId::at(shard)).unwrap(),
                Config::default(),
            )
            .unwrap();
            assert!(allocator.is_idle());
            assert!(allocator.lookup(&[0; 32], 0).is_none());
        }
    }

    #[test]
    fn geometry_bitmap_boundary_and_rounding_without_storage() {
        // Independent format boundary: 503 pointers, 4064 bitmap bytes, one bit per
        // 4096-byte page. The last partial 4 MiB extent cannot be used.
        const MAX: u64 = 66_983_034_880;
        assert_eq!(MAX_SHARD_SIZE, MAX);
        for shards in [1, 32, 33] {
            for len in [32 * 1024 * 1024, MAX - WIDE, MAX] {
                let size = len * shards as u64;
                let first = Geometry::new(size, shards, 0).unwrap();
                let last = Geometry::new(size, shards, shards - 1).unwrap();
                assert_eq!(first.len, len);
                assert_eq!(last.base + last.len, size);
                assert!(first.pages().div_ceil(8).div_ceil(BIT_BYTES) <= 503);
            }
            assert!(Geometry::new((MAX + WIDE) * shards as u64, shards, 0).is_err());
            // Preserve persisted geometry: whole-WIDE division leaves an unused tail.
            let size = MAX * shards as u64 + (shards as u64 - 1) * WIDE;
            assert_eq!(Geometry::new(size, shards, 0).unwrap().len, MAX);
            assert!(Geometry::new(size + WIDE, shards, 0).is_err());
        }
        assert_eq!(
            Geometry::new(MAX, 1, 0)
                .unwrap()
                .pages()
                .div_ceil(8)
                .div_ceil(BIT_BYTES),
            503
        );
        let error = Geometry::new(2 << 40, 32, 0).unwrap_err().to_string();
        for text in [
            "517 bitmap pages",
            "at most 503",
            "66983034880 bytes",
            "RACER_SHARDS",
            "new RACER_SLAB_PATH",
        ] {
            assert!(error.contains(text), "{error}");
        }
        assert!(Geometry::new(2 << 40, 33, 32).is_ok());
        assert!(Geometry::new(DEFAULT_SLAB_SIZE, 32, 31).is_ok());
        for (size, shards, id) in [
            (0, 1, 0),
            (7 * WIDE, 1, 0),
            (8 * WIDE + 1, 1, 0),
            (DEFAULT_SLAB_SIZE, 0, 0),
            (DEFAULT_SLAB_SIZE, 32, 32),
            (u64::MAX, 1, 0),
            (1 << 63, 1, 0),
            (i64::MAX as u64 / WIDE * WIDE, 1, 0),
            (DEFAULT_SLAB_SIZE, usize::MAX, 0),
        ] {
            assert!(
                Geometry::new(size, shards, id).is_err(),
                "{size}/{shards}/{id}"
            );
        }
    }

    #[test]
    fn create_geometry_boundary_precedes_sizing_and_publication() {
        let directory = Directory::new();
        for len in [MAX_SHARD_SIZE - WIDE, MAX_SHARD_SIZE, MAX_SHARD_SIZE + WIDE] {
            let mut reached_create = false;
            let result = Slab::create_inner(&directory.slab(), len * 32, 32, None, |step| {
                assert_eq!(step, CreateStep::Created);
                reached_create = true;
                let entries = directory.entries();
                assert_eq!(entries.len(), 1);
                assert_eq!(std::fs::metadata(&entries[0])?.len(), 0);
                // Never set_len or format a huge slab, even on accepted boundaries.
                Err(io::Error::from_raw_os_error(libc::EIO))
            });
            let error = result.err().unwrap();
            assert_eq!(reached_create, len <= MAX_SHARD_SIZE);
            if reached_create {
                assert_eq!(error.raw_os_error(), Some(libc::EIO));
            } else {
                assert_eq!(error.kind(), io::ErrorKind::InvalidData);
            }
            assert!(directory.entries().is_empty());
        }
    }

    #[test]
    fn oversized_create_rejects_before_filesystem_access_and_preserves_existing_files() {
        let directory = Directory::new();
        let path = directory.slab();
        for destination in [directory.0.join("missing-parent/cache.slab"), path.clone()] {
            for result in [
                Slab::create(&destination, 2 << 40, 32),
                Slab::open_or_create_layout(&destination, 2 << 40, 32, 2),
            ] {
                assert_eq!(result.err().unwrap().kind(), io::ErrorKind::InvalidData);
            }
            assert!(directory.entries().is_empty());
        }
        let sentinel = b"existing file must survive invalid geometry";
        std::fs::write(&path, sentinel).unwrap();
        let before = std::fs::metadata(&path).unwrap();
        assert_eq!(
            Slab::create(&path, 2 << 40, 32).err().unwrap().kind(),
            io::ErrorKind::InvalidData
        );
        assert_eq!(std::fs::read(&path).unwrap(), sentinel);
        assert_eq!(std::fs::metadata(&path).unwrap().ino(), before.ino());
        assert_eq!(directory.entries(), vec![path]);
    }

    #[test]
    fn open_and_create_share_geometry_errors() {
        let directory = Directory::new();
        let path = directory.slab();
        let file = File::create(&path).unwrap();
        // Exercise the actual open path with small sparse files; large boundaries
        // above exercise the same Geometry::new without enormous logical files.
        for (size, shards) in [(0, 1), (7 * WIDE, 1), (8 * WIDE + 1, 1), (SIZE, 0)] {
            file.set_len(size).unwrap();
            let expected = Geometry::new(size, shards, 0).unwrap_err().to_string();
            let opened = Slab::open(&path, shards).err().unwrap();
            let created = Slab::create(&path, size, shards).err().unwrap();
            assert_eq!(opened.kind(), io::ErrorKind::InvalidData);
            assert_eq!(opened.to_string(), expected);
            assert_eq!(created.to_string(), expected);
            assert_eq!(file.metadata().unwrap().len(), size);
            assert_eq!(directory.entries(), vec![path.clone()]);
        }
    }

    #[test]
    fn errors_clean_private_names_and_only_publish_valid_locked_slabs() {
        for fail in STEPS {
            let directory = Directory::new();
            let path = directory.slab();
            let mut observed = Vec::new();
            let result = Slab::create_inner(&path, SIZE, SHARDS, None, |step| {
                observed.push(step);
                assert_eq!(path.exists(), published(step), "{step:?}");
                // The unpublished inode and published inode are both locked.
                let entries = directory.entries();
                assert_eq!(entries.len(), 1);
                let file = OpenOptions::new()
                    .read(true)
                    .write(true)
                    .open(&entries[0])?;
                assert_eq!(lock(&file).unwrap_err().kind(), io::ErrorKind::WouldBlock);
                if step == fail {
                    Err(io::Error::from_raw_os_error(libc::EIO))
                } else {
                    Ok(())
                }
            });
            assert_eq!(result.err().unwrap().raw_os_error(), Some(libc::EIO));
            assert_eq!(observed, STEPS[..observed.len()]);
            assert_eq!(directory.entries().len(), usize::from(published(fail)));
            if !published(fail) {
                drop(Slab::create(&path, SIZE, SHARDS).unwrap());
            }
            recover(&path);
        }
    }

    #[test]
    fn unwind_cleans_private_inode() {
        let directory = Directory::new();
        let path = directory.slab();
        let result = std::panic::catch_unwind(|| {
            let _ = Slab::create_inner(&path, SIZE, SHARDS, None, |step| {
                if step == CreateStep::Checkpoint(0, 0) {
                    panic!("interrupted formatting");
                }
                Ok(())
            });
        });
        assert!(result.is_err());
        assert!(directory.entries().is_empty());
        drop(Slab::create(&path, SIZE, SHARDS).unwrap());
        recover(&path);
    }

    #[test]
    fn existing_valid_slab_and_other_names_are_never_replaced() {
        let directory = Directory::new();
        let path = directory.slab();
        let slab = Slab::create(&path, SIZE, SHARDS).unwrap();
        let file = OpenOptions::new()
            .read(true)
            .write(true)
            .open(&path)
            .unwrap();
        let inode = file.metadata().unwrap().ino();
        let sentinel = b"existing cached bytes";
        file.write_all_at(sentinel, SIZE - PAGE_SIZE as u64)
            .unwrap();
        file.sync_all().unwrap();
        // Both a live locked slab and a closed valid slab must be preserved.
        for owner in [Some(slab), None] {
            assert_eq!(
                Slab::create(&path, SIZE * 2, SHARDS).err().unwrap().kind(),
                io::ErrorKind::AlreadyExists
            );
            assert_eq!(std::fs::metadata(&path).unwrap().ino(), inode);
            assert_eq!(file.metadata().unwrap().len(), SIZE);
            let mut bytes = vec![0; sentinel.len()];
            file.read_exact_at(&mut bytes, SIZE - PAGE_SIZE as u64)
                .unwrap();
            assert_eq!(bytes, sentinel);
            assert_eq!(directory.entries(), vec![path.clone()]);
            drop(owner);
        }
        recover(&path);
        std::fs::remove_file(&path).unwrap();
        std::os::unix::fs::symlink("absent-target", &path).unwrap();
        assert_eq!(
            Slab::create(&path, SIZE, SHARDS).err().unwrap().kind(),
            io::ErrorKind::AlreadyExists
        );
        assert_eq!(
            std::fs::read_link(&path).unwrap(),
            Path::new("absent-target")
        );
        std::fs::remove_file(&path).unwrap();
        std::fs::write(&path, b"invalid slabs also require explicit intervention").unwrap();
        assert_eq!(
            Slab::create(&path, SIZE, SHARDS).err().unwrap().kind(),
            io::ErrorKind::AlreadyExists
        );
        assert_eq!(
            std::fs::read(&path).unwrap(),
            b"invalid slabs also require explicit intervention"
        );
    }

    #[test]
    fn concurrent_creators_have_one_winner_and_keep_its_lock() {
        let directory = Directory::new();
        let path = directory.slab();
        let ready = std::sync::Barrier::new(6);
        let release = std::sync::Barrier::new(7);
        let (send, receive) = std::sync::mpsc::channel();
        std::thread::scope(|scope| {
            for _ in 0..6 {
                let send = send.clone();
                let path = &path;
                let ready = &ready;
                let release = &release;
                scope.spawn(move || {
                    let result = Slab::create_inner(path, SIZE, SHARDS, None, |step| {
                        if step == CreateStep::FileSynced {
                            ready.wait();
                        }
                        Ok(())
                    });
                    send.send(result.as_ref().map(|_| ()).map_err(|e| e.kind()))
                        .unwrap();
                    release.wait();
                    drop(result);
                });
            }
            let results: Vec<_> = (0..6)
                .map(|_| receive.recv_timeout(Duration::from_secs(15)).unwrap())
                .collect();
            let locked = Slab::open(&path, SHARDS).err().unwrap().kind();
            let entries = directory.entries();
            release.wait();
            assert_eq!(results.iter().filter(|r| r.is_ok()).count(), 1);
            assert_eq!(
                results
                    .iter()
                    .filter(|r| **r == Err(io::ErrorKind::AlreadyExists))
                    .count(),
                5
            );
            assert_eq!(locked, io::ErrorKind::WouldBlock);
            assert_eq!(entries, vec![path.clone()]);
        });
        recover(&path);
        // A shard, independently of the Slab handle, retains the publication lock.
        let mut slab = Slab::open(&path, SHARDS).unwrap();
        let shard = slab.take_shard(ShardId::at(0)).unwrap();
        drop(slab);
        assert_eq!(
            Slab::open(&path, SHARDS).err().unwrap().kind(),
            io::ErrorKind::WouldBlock
        );
        drop(shard);
        recover(&path);
    }

    #[test]
    fn publication_and_cleanup_use_the_original_parent_directory() {
        let directory = Directory::new();
        let original = directory.0.join("original");
        let moved = directory.0.join("moved");
        std::fs::create_dir(&original).unwrap();
        let path = original.join("cache.slab");
        drop(
            Slab::create_inner(&path, SIZE, SHARDS, None, |step| {
                if step == CreateStep::FileSynced {
                    std::fs::rename(&original, &moved)?;
                    std::fs::create_dir(&original)?;
                    std::fs::write(&path, b"different directory")?;
                }
                Ok(())
            })
            .unwrap(),
        );
        assert_eq!(std::fs::read(&path).unwrap(), b"different directory");
        recover(&moved.join("cache.slab"));
        assert_eq!(std::fs::read_dir(&moved).unwrap().count(), 1);
    }

    // Exec a fresh test process rather than fork a multithreaded harness. SIGKILL
    // bypasses every Rust destructor, at each actual create/publish boundary.
    #[test]
    fn interrupted_create_child() {
        let Some(path) = std::env::var_os("RACER_CREATE_KILL_PATH") else {
            return;
        };
        let stop: usize = std::env::var("RACER_CREATE_KILL_STEP")
            .unwrap()
            .parse()
            .unwrap();
        let path = PathBuf::from(path);
        let _slab = Slab::create_inner(&path, SIZE, SHARDS, None, |step| {
            if step == STEPS[stop] {
                std::fs::write(path.with_extension("ready"), b"ready")?;
                loop {
                    std::thread::park();
                }
            }
            Ok(())
        })
        .unwrap();
        panic!("child missed interruption point");
    }

    #[test]
    fn killed_creators_leave_absent_or_recoverable_final_names() {
        for (index, step) in STEPS.into_iter().enumerate() {
            let directory = Directory::new();
            let path = directory.slab();
            let mut child = std::process::Command::new(std::env::current_exe().unwrap())
                .args([
                    "--exact",
                    "allocator::tests::create_tests::interrupted_create_child",
                    "--test-threads=2",
                ])
                .env("RACER_CREATE_KILL_PATH", &path)
                .env("RACER_CREATE_KILL_STEP", index.to_string())
                .stdout(std::process::Stdio::null())
                .spawn()
                .unwrap();
            let deadline = Instant::now() + Duration::from_secs(15);
            while !path.with_extension("ready").exists() {
                assert!(
                    child.try_wait().unwrap().is_none(),
                    "child exited at {step:?}"
                );
                if Instant::now() > deadline {
                    child.kill().unwrap();
                    child.wait().unwrap();
                    panic!("child timeout at {step:?}");
                }
                std::thread::sleep(Duration::from_millis(5));
            }
            child.kill().unwrap();
            assert!(!child.wait().unwrap().success());
            assert_eq!(path.exists(), published(step), "{step:?}");
            let abandoned: Vec<_> = directory
                .entries()
                .into_iter()
                .filter(|p| p.extension().is_some_and(|e| e == "tmp"))
                .collect();
            assert_eq!(abandoned.len(), usize::from(!published(step)));
            if !published(step) {
                // An abandoned, possibly partial inode neither owns the final name
                // nor prevents restart; a fresh inode is formatted and published.
                drop(Slab::create(&path, SIZE, SHARDS).unwrap());
                assert_ne!(
                    std::fs::metadata(&path).unwrap().ino(),
                    std::fs::metadata(&abandoned[0]).unwrap().ino()
                );
            }
            recover(&path);
        }
    }
}

mod layout_tests {
    //! Real ext4 xattr/publication/recovery coverage for the daemon startup gate.
    use super::*;
    use crate::buffers;
    use std::os::unix::fs::MetadataExt;
    use std::path::PathBuf;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::time::{Duration, Instant};

    const SHARDS: usize = 5; // Unequal local counts with two workers: 3 and 2.
    const SIZE: u64 = SHARDS as u64 * 8 * WIDE;

    struct Directory(PathBuf);
    impl Directory {
        fn new() -> Self {
            static NEXT: AtomicUsize = AtomicUsize::new(0);
            let path = std::env::temp_dir().join(format!(
                "racer-layout-{}-{}",
                std::process::id(),
                NEXT.fetch_add(1, Ordering::Relaxed)
            ));
            std::fs::create_dir(&path).unwrap();
            Self(path)
        }
        fn path(&self) -> PathBuf {
            self.0.join("cache.slab")
        }
    }
    impl Drop for Directory {
        fn drop(&mut self) {
            std::fs::remove_dir_all(&self.0).unwrap();
        }
    }
    fn open(path: &Path, workers: usize) -> io::Result<Slab> {
        Slab::open_or_create_layout(path, SIZE, SHARDS, workers)
    }
    fn file(path: &Path) -> File {
        OpenOptions::new()
            .read(true)
            .write(true)
            .open(path)
            .unwrap()
    }
    fn replace_attribute(file: &File, value: &[u8]) {
        assert_eq!(
            unsafe {
                libc::fsetxattr(
                    file.as_raw_fd(),
                    ATTRIBUTE.as_ptr(),
                    value.as_ptr().cast(),
                    value.len(),
                    0,
                )
            },
            0
        );
        file.sync_all().unwrap();
    }
    fn key(n: u64) -> Key {
        let mut key = [0; 32];
        key[..8].copy_from_slice(&n.to_le_bytes());
        key
    }

    #[test]
    fn populate_recover_same_layout_and_reject_changed_count_without_mutation() {
        let directory = Directory::new();
        let path = directory.path();
        let mut slab = open(&path, 2).unwrap();
        let pool = buffers::io_test_pool(8);
        let mut ring = Ring::http_test_ring(pool.clone(), uring::Config::default()).unwrap();
        // Use the production assignment builder, including unequal replica counts.
        let placements = crate::sharding::placements(
            vec![
                (crate::workers::CpuId(0), crate::workers::NumaNodeId(0)),
                (crate::workers::CpuId(1), crate::workers::NumaNodeId(0)),
            ],
            SHARDS,
        )
        .unwrap();
        for placement in &placements {
            let ids = placement.shard_ids();
            let mut allocators: Vec<_> = ids
                .iter()
                .map(|id| {
                    Allocator::open_inner(slab.take_shard(*id).unwrap(), Config::default()).unwrap()
                })
                .collect();
            for n in 0..12 {
                let k = key(n);
                let index = n as usize % ids.len();
                let a = &mut allocators[index];
                a.insert_metadata(k, metadata(n, 100), 0).unwrap();
                let deadline = Instant::now() + Duration::from_secs(5);
                while !a.is_idle() {
                    ring.progress().unwrap();
                    a.poll(&mut ring, 32).unwrap();
                    assert!(Instant::now() < deadline);
                }
            }
        }
        drop((ring, slab));
        let digest = blake3::hash(&std::fs::read(&path).unwrap());
        let inode = std::fs::metadata(&path).unwrap().ino();
        for workers in [1, 3, 5] {
            let error = open(&path, workers).err().unwrap();
            assert_eq!(error.kind(), io::ErrorKind::InvalidData);
            assert!(error.to_string().contains("requires 2 total I/O workers"));
            assert!(
                error
                    .to_string()
                    .contains("RACER_IO_WORKERS is per NUMA node")
            );
        }
        assert!(Slab::open_or_create_layout(&path, SIZE, 4, 2).is_err());
        assert_eq!(std::fs::metadata(&path).unwrap().ino(), inode);
        assert_eq!(blake3::hash(&std::fs::read(&path).unwrap()), digest);
        // Size is creation-only. Recovery still checks actual length against xattr.
        // Even unsupported creation geometry is irrelevant when opening a valid
        // persisted layout: actual inode size, not configured size, is authoritative.
        let mut slab = Slab::open_or_create_layout(&path, 2 << 40, SHARDS, 2).unwrap();
        for placement in &placements {
            let ids = placement.shard_ids();
            for (index, id) in ids.iter().enumerate() {
                let mut a = Allocator::open_inner(slab.take_shard(*id).unwrap(), Config::default())
                    .unwrap();
                for n in 0..12 {
                    let value = a.lookup_metadata(&key(n), 0);
                    assert_eq!(value.is_some(), n as usize % ids.len() == index);
                    if let Some(value) = value {
                        assert_eq!(value, metadata(n, 100));
                    }
                }
            }
        }
    }

    #[test]
    fn legacy_empty_and_populated_unknown_slabs_are_never_adopted() {
        let directory = Directory::new();
        let path = directory.path();
        drop(Slab::create(&path, SIZE, SHARDS).unwrap());
        for sentinel in [b"".as_slice(), b"legacy cached value".as_slice()] {
            let f = file(&path);
            f.write_all_at(sentinel, SIZE - PAGE_SIZE as u64).unwrap();
            f.sync_all().unwrap();
            let before = blake3::hash(&std::fs::read(&path).unwrap());
            for workers in [1, 2] {
                let error = open(&path, workers).err().unwrap().to_string();
                assert!(error.contains("missing user.racer.layout"), "{error}");
                assert!(error.contains("new RACER_SLAB_PATH"));
            }
            assert_eq!(blake3::hash(&std::fs::read(&path).unwrap()), before);
        }
    }

    #[test]
    fn malformed_missing_future_metadata_and_resized_slab_fail_closed() {
        let directory = Directory::new();
        let path = directory.path();
        drop(open(&path, 2).unwrap());
        let f = file(&path);
        assert!(Layout::new(SIZE, SHARDS, 3).unwrap().write(&f).is_err());
        // A complete xattr can never be replaced by initialization, even if compatible.
        assert!(Layout::new(SIZE, SHARDS, 2).unwrap().write(&f).is_err());
        for bytes in [vec![], vec![0; 12], vec![0; 64], vec![0; 65]] {
            replace_attribute(&f, &bytes);
            assert!(open(&path, 2).is_err());
        }
        let mut future = [0; 64];
        future[..8].copy_from_slice(b"RACERL02");
        let digest = blake3::hash(&future[..32]);
        future[32..].copy_from_slice(digest.as_bytes());
        replace_attribute(&f, &future);
        assert!(open(&path, 2).is_err());
        assert_eq!(
            unsafe { libc::fremovexattr(f.as_raw_fd(), ATTRIBUTE.as_ptr()) },
            0
        );
        assert!(
            open(&path, 2)
                .err()
                .unwrap()
                .to_string()
                .contains("missing")
        );
        Layout::new(SIZE, SHARDS, 2).unwrap().write(&f).unwrap();
        f.set_len(SIZE + WIDE).unwrap();
        assert!(
            open(&path, 2)
                .err()
                .unwrap()
                .to_string()
                .contains("slab bytes")
        );
    }

    const STEPS: [CreateStep; 6] = [
        CreateStep::Checkpoint(0, 0),
        CreateStep::LayoutWritten,
        CreateStep::BeforeFileSync,
        CreateStep::FileSynced,
        CreateStep::Published,
        CreateStep::DirectorySynced,
    ];

    #[test]
    fn initialization_io_errors_never_publish_an_untagged_slab() {
        for fail in STEPS {
            let directory = Directory::new();
            let path = directory.path();
            let result = Slab::create_inner(
                &path,
                SIZE,
                SHARDS,
                Some(Layout::new(SIZE, SHARDS, 2).unwrap()),
                |step| {
                    if step == fail {
                        Err(io::Error::from_raw_os_error(libc::EIO))
                    } else {
                        Ok(())
                    }
                },
            );
            assert_eq!(result.err().unwrap().raw_os_error(), Some(libc::EIO));
            let published = matches!(fail, CreateStep::Published | CreateStep::DirectorySynced);
            assert_eq!(path.exists(), published);
            assert_eq!(
                std::fs::read_dir(&directory.0).unwrap().count(),
                usize::from(published)
            );
            if published {
                assert!(open(&path, 3).is_err());
            }
            // Another test can fork while create_inner owns the flock. CLOEXEC
            // closes the child's inherited descriptor at exec, not at fork, so
            // closing our last descriptor need not release the lock immediately.
            // Only this post-owner recovery assertion may wait; live-owner lock
            // exclusion assertions below must remain immediate.
            let deadline = Instant::now() + Duration::from_secs(5);
            loop {
                match open(&path, 2) {
                    Ok(slab) => {
                        drop(slab);
                        break;
                    }
                    Err(error)
                        if error.kind() == io::ErrorKind::WouldBlock
                            && Instant::now() < deadline =>
                    {
                        std::thread::sleep(Duration::from_millis(1));
                    }
                    Err(error) => panic!("reopen after {fail:?}: {error}"),
                }
            }
        }
    }

    #[test]
    fn layout_interrupted_child() {
        let Some(path) = std::env::var_os("RACER_LAYOUT_KILL_PATH") else {
            return;
        };
        let index: usize = std::env::var("RACER_LAYOUT_KILL_STEP")
            .unwrap()
            .parse()
            .unwrap();
        let path = PathBuf::from(path);
        let _slab = Slab::create_inner(
            &path,
            SIZE,
            SHARDS,
            Some(Layout::new(SIZE, SHARDS, 2).unwrap()),
            |step| {
                if step == STEPS[index] {
                    std::fs::write(path.with_extension("ready"), b"ready")?;
                    loop {
                        std::thread::park();
                    }
                }
                Ok(())
            },
        )
        .unwrap();
        panic!("missed interruption point");
    }

    #[test]
    fn interrupted_layout_initialization_is_atomic_with_slab_publication() {
        for (index, step) in STEPS.into_iter().enumerate() {
            let directory = Directory::new();
            let path = directory.path();
            let mut child = std::process::Command::new(std::env::current_exe().unwrap())
                .args([
                    "--exact",
                    "allocator::tests::layout_tests::layout_interrupted_child",
                    "--test-threads=2",
                ])
                .env("RACER_LAYOUT_KILL_PATH", &path)
                .env("RACER_LAYOUT_KILL_STEP", index.to_string())
                .stdout(std::process::Stdio::null())
                .spawn()
                .unwrap();
            let deadline = Instant::now() + Duration::from_secs(15);
            while !path.with_extension("ready").exists() {
                assert!(child.try_wait().unwrap().is_none());
                if Instant::now() > deadline {
                    child.kill().unwrap();
                    child.wait().unwrap();
                    panic!("child timeout at {step:?}");
                }
                std::thread::sleep(Duration::from_millis(5));
            }
            child.kill().unwrap();
            child.wait().unwrap();
            let published = matches!(step, CreateStep::Published | CreateStep::DirectorySynced);
            assert_eq!(path.exists(), published);
            if published {
                assert!(open(&path, 3).is_err());
            }
            let mut slab = open(&path, if published { 2 } else { 3 }).unwrap();
            for id in 0..SHARDS {
                assert!(
                    Allocator::open_inner(
                        slab.take_shard(ShardId::at(id)).unwrap(),
                        Config::default()
                    )
                    .unwrap()
                    .is_idle()
                );
            }
        }
    }

    #[test]
    fn racing_different_layouts_publish_exactly_one_contract_and_keep_lock() {
        let directory = Directory::new();
        let path = directory.path();
        let ready = std::sync::Barrier::new(2);
        let release = std::sync::Barrier::new(3);
        let (send, receive) = std::sync::mpsc::channel();
        std::thread::scope(|scope| {
            for workers in [2, 3] {
                let (path, ready, release, send) = (&path, &ready, &release, send.clone());
                scope.spawn(move || {
                    let result = Slab::create_inner(
                        path,
                        SIZE,
                        SHARDS,
                        Some(Layout::new(SIZE, SHARDS, workers).unwrap()),
                        |step| {
                            if step == CreateStep::FileSynced {
                                ready.wait();
                            }
                            Ok(())
                        },
                    );
                    send.send((workers, result.as_ref().map(|_| ()).map_err(|e| e.kind())))
                        .unwrap();
                    release.wait();
                    drop(result);
                });
            }
            let results: Vec<_> = (0..2)
                .map(|_| receive.recv_timeout(Duration::from_secs(15)).unwrap())
                .collect();
            assert_eq!(results.iter().filter(|(_, r)| r.is_ok()).count(), 1);
            assert_eq!(
                results
                    .iter()
                    .filter(|(_, r)| *r == Err(io::ErrorKind::AlreadyExists))
                    .count(),
                1
            );
            assert_eq!(
                open(&path, 2).err().unwrap().kind(),
                io::ErrorKind::WouldBlock
            );
            release.wait();
            results
        })
        .into_iter()
        .for_each(|(workers, result)| {
            assert_eq!(open(&path, workers).is_ok(), result.is_ok());
        });
    }
}

mod pressure_tests {
    use super::*;
    use crate::{
        buffers,
        simulation::{Disk, World},
    };
    use std::time::Duration;

    fn buffer(ring: &Ring, n: u8) -> Buffer {
        let mut fill = ring.pool().stage(buffers::Key::new([n; 32])).unwrap();
        fill.as_mut_slice()[..PAGE_SIZE].fill(n);
        fill.publish(PAGE_SIZE).unwrap()
    }
    fn tick(world: &World, ring: &mut Ring, a: &mut Allocator) {
        world.advance(Duration::from_millis(1));
        world.run_tasks();
        ring.progress().unwrap();
        a.poll_contained(ring, 32).unwrap();
    }
    fn drain(world: &World, ring: &mut Ring, a: &mut Allocator) {
        for _ in 0..1000 {
            tick(world, ring, a);
            if a.is_idle() {
                return;
            }
        }
        panic!("checkpoint drain stalled");
    }

    #[test]
    fn b17_physical_admission_shared_reservations_and_recovery() {
        let world = World::new(717);
        let _scope = world.enter();
        let mut ring = crate::conformance::ring(8, Default::default());
        let disk = Disk::new(64 * 1024 * 1024);
        let mut slab = Slab::simulated(disk.clone(), 64 * 1024 * 1024, 2, true).unwrap();
        let mut a =
            Allocator::open_inner(slab.take_shard(ShardId::at(0)).unwrap(), Config::default())
                .unwrap();
        let mut b =
            Allocator::open_inner(slab.take_shard(ShardId::at(1)).unwrap(), Config::default())
                .unwrap();
        let headroom =
            HEADROOM + 2 * a.space.geometry.range(Class::Index).1 as u64 * PAGE_SIZE as u64;
        disk.set_available_bytes(headroom + WIDE);
        a.insert_payload([1; 32], buffer(&ring, 1), None).unwrap();
        let before = b.space.maps.each_ref().map(|m| m.borrow().free);
        assert_eq!(
            b.insert_payload([2; 32], buffer(&ring, 2), None)
                .unwrap_err()
                .error
                .kind(),
            io::ErrorKind::WouldBlock
        );
        assert_eq!(before, b.space.maps.each_ref().map(|m| m.borrow().free));
        assert!(b.is_idle() && b.is_empty() && !b.is_failed());
        drain(&world, &mut ring, &mut a);
        assert_eq!(*a.pressure.0.lock().unwrap(), 0);
        b.insert_payload([2; 32], buffer(&ring, 2), None).unwrap();
        drain(&world, &mut ring, &mut b);
        disk.set_available_bytes(0);
        assert!(a.insert_payload([3; 32], buffer(&ring, 3), None).is_err());
        assert_eq!(
            a.insert_metadata([9; 32], metadata(9, 100), 0)
                .unwrap_err()
                .kind(),
            io::ErrorKind::WouldBlock
        );
        assert!(a.lookup_metadata(&[9; 32], 0).is_none());
        assert!(a.lookup(&[1; 32], 0).unwrap().ready().is_some());
        disk.set_available_bytes(u64::MAX);
        a.insert_payload([3; 32], buffer(&ring, 3), None).unwrap();
        drain(&world, &mut ring, &mut a);
        drop((a, b, slab, ring));
        world.assert_clean();
    }

    #[test]
    fn b17_enospc_quarantines_write_punch_and_every_checkpoint_barrier() {
        // WRITE_FIXED, FALLOCATE, tree/bitmap WRITE, data fsync, root WRITE, root fsync.
        for after_effect in [false, true] {
            for stage in 0..6 {
                let world = World::new(718 + stage);
                let _scope = world.enter();
                world.enable_scheduler(); // Separate effects and delayed terminal CQEs.
                let mut ring = crate::conformance::ring(8, Default::default());
                let disk = Disk::new(64 * 1024 * 1024);
                let mut slab = Slab::simulated(disk.clone(), 64 * 1024 * 1024, 2, true).unwrap();
                let mut a = Allocator::open_inner(
                    slab.take_shard(ShardId::at(0)).unwrap(),
                    Config::default(),
                )
                .unwrap();
                let mut healthy = Allocator::open_inner(
                    slab.take_shard(ShardId::at(1)).unwrap(),
                    Config::default(),
                )
                .unwrap();
                a.insert_payload([1; 32], buffer(&ring, 1), None).unwrap();
                drain(&world, &mut ring, &mut a);
                let stable = a.lookup(&[1; 32], 0).unwrap().ready().unwrap();
                let generation = a.generation();
                a.insert_payload([2; 32], buffer(&ring, 2), None).unwrap();
                let ambiguous = a.lookup(&[2; 32], 0).unwrap();
                if stage >= 2 {
                    for i in 0..1000 {
                        let reached = match stage {
                            2 => matches!(a.pipeline, Some(Pipeline::Writes(_))),
                            3 => matches!(a.pipeline, Some(Pipeline::DataSync(_))),
                            4 => matches!(a.pipeline, Some(Pipeline::DataSynced(_))),
                            5 => matches!(a.pipeline, Some(Pipeline::MagicWritten(_))),
                            _ => unreachable!(),
                        };
                        if reached {
                            break;
                        }
                        assert!(i < 999);
                        tick(&world, &mut ring, &mut a);
                    }
                }
                let op = match stage {
                    0 => 5,
                    1 => 17,
                    2 | 4 => 23,
                    _ => 3,
                };
                if after_effect {
                    disk.fail_after_effect(op);
                } else {
                    world.fail_next_errno(op, libc::ENOSPC);
                }
                for _ in 0..1000 {
                    tick(&world, &mut ring, &mut a);
                    if a.is_failed() {
                        break;
                    }
                }
                assert!(world.fault_fired(), "stage {stage} not injected");
                assert!(disk.completion_fault_fired());
                assert!(a.is_failed(), "stage {stage} not quarantined");
                assert_eq!(a.generation(), generation);
                assert!(a.lookup(&[1; 32], 0).is_none());
                assert!(
                    ambiguous.buffer().is_none(),
                    "quarantine retained a pool slot"
                );
                if stage < 2 {
                    assert!(ambiguous.ready().is_none());
                }
                let maps = a.space.maps.each_ref().map(|m| m.borrow().free);
                for _ in 0..100 {
                    assert!(!a.poll_contained(&mut ring, 32).unwrap().runnable);
                    assert!(a.insert_payload([3; 32], buffer(&ring, 3), None).is_err());
                }
                assert_eq!(maps, a.space.maps.each_ref().map(|m| m.borrow().free));
                // Same inode, same ring, same pool: poison does not escape its shard.
                healthy
                    .insert_payload([4; 32], buffer(&ring, 4), None)
                    .unwrap();
                drain(&world, &mut ring, &mut healthy);
                assert!(healthy.lookup(&[4; 32], 0).unwrap().ready().is_some());
                assert!(a.is_failed());
                let mut bytes = vec![0; PAGE_SIZE];
                disk.read_exact_at(&mut bytes, stable.offset()).unwrap();
                assert_eq!(bytes, vec![1; PAGE_SIZE]);
                drop((ambiguous, stable, a, healthy, slab));
                ring.shutdown().unwrap();
                assert!(
                    ring.pool()
                        .invariant_snapshot()
                        .refs
                        .iter()
                        .all(|n| *n == 0)
                );
                // Restart only after ring quiescence. Real recovery validates roots;
                // a failed final sync may have committed either generation.
                disk.crash(0);
                let mut slab = Slab::simulated(disk, 64 * 1024 * 1024, 2, false).unwrap();
                let mut recovered = Allocator::open_inner(
                    slab.take_shard(ShardId::at(0)).unwrap(),
                    Config::default(),
                )
                .unwrap();
                assert!(!recovered.is_failed());
                assert!(recovered.lookup(&[1; 32], 0).unwrap().ready().is_some());
                if stage == 5 && after_effect {
                    assert!(
                        recovered.lookup(&[2; 32], 0).unwrap().ready().is_some(),
                        "ambiguous final sync actually committed"
                    );
                }
                drop((recovered, slab, ring));
                world.assert_clean();
            }
        }
    }
}
