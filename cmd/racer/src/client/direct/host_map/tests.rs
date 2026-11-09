use super::*;
use std::io::Write;
use std::os::fd::FromRawFd;

fn memfd(size: usize, seals: libc::c_int) -> std::fs::File {
    let fd = unsafe {
        libc::memfd_create(
            c"racer-host-map".as_ptr(),
            libc::MFD_CLOEXEC | libc::MFD_ALLOW_SEALING,
        )
    };
    assert!(fd >= 0);
    let mut file = unsafe { std::fs::File::from_raw_fd(fd) };
    file.write_all(&vec![0; size]).unwrap();
    if seals != 0 {
        assert_eq!(unsafe { libc::fcntl(fd, libc::F_ADD_SEALS, seals) }, 0);
    }
    file
}

fn window(offset: u64, length: u64) -> Destination {
    Destination { offset, length }
}

#[test]
fn maps_only_shrink_sealed_read_write_memfds_that_cover_the_window() {
    let sealed = memfd(10_000, libc::F_SEAL_SHRINK);
    assert!(HostMapping::new(&sealed, window(0, 10_000)).is_some());
    assert!(HostMapping::new(&sealed, window(4097, 5000)).is_some());
    assert!(
        HostMapping::new(&sealed, window(5000, 5001)).is_none(),
        "past the end"
    );
    assert!(HostMapping::new(&sealed, window(0, 0)).is_none(), "empty");

    let unsealed = memfd(10_000, 0);
    assert!(HostMapping::new(&unsealed, window(0, 100)).is_none());

    let write_sealed = memfd(10_000, libc::F_SEAL_SHRINK | libc::F_SEAL_WRITE);
    assert!(HostMapping::new(&write_sealed, window(0, 100)).is_none());

    let future_write = memfd(10_000, libc::F_SEAL_SHRINK | libc::F_SEAL_FUTURE_WRITE);
    assert!(HostMapping::new(&future_write, window(0, 100)).is_none());

    let path = format!("/proc/self/fd/{}", sealed.as_raw_fd());
    let write_only = std::fs::OpenOptions::new().write(true).open(&path).unwrap();
    assert!(HostMapping::new(&write_only, window(0, 100)).is_none());
}

#[test]
fn only_files_on_the_memfd_mount_count_as_memfds() {
    let file = memfd(4096, libc::F_SEAL_SHRINK);
    let mut stat: libc::stat = unsafe { std::mem::zeroed() };
    assert_eq!(unsafe { libc::fstat(file.as_raw_fd(), &mut stat) }, 0);
    assert!(is_memfd(&stat));
    // Another tmpfs mount, such as /dev/shm, has another device.
    stat.st_dev = stat.st_dev.wrapping_add(1);
    assert!(!is_memfd(&stat));
}

#[test]
fn does_not_map_files_outside_tmpfs() {
    // The crate directory is on a disk filesystem, never tmpfs.
    let file = std::fs::File::open(env!("CARGO_MANIFEST_DIR").to_string() + "/Cargo.toml").unwrap();
    assert!(HostMapping::new(&file, window(0, 1)).is_none());
}

#[test]
fn bytes_stay_inside_the_window_and_reach_the_file() {
    use std::os::unix::fs::FileExt;
    let file = memfd(3 * 4096, libc::F_SEAL_SHRINK);
    let mapping = HostMapping::new(&file, window(5000, 3000)).unwrap();
    unsafe {
        assert!(mapping.bytes_mut(4999, 1).is_err());
        assert!(mapping.bytes_mut(7999, 2).is_err());
        assert!(mapping.bytes_mut(u64::MAX, 2).is_err());
        let bytes = mapping.bytes_mut(5000, 3000).unwrap();
        populate(bytes).unwrap();
        bytes.fill(7);
    }
    drop(mapping);
    let mut contents = vec![0; 3 * 4096];
    file.read_exact_at(&mut contents, 0).unwrap();
    assert!(contents[..5000].iter().all(|&b| b == 0));
    assert!(contents[5000..8000].iter().all(|&b| b == 7));
    assert!(contents[8000..].iter().all(|&b| b == 0));
}

fn cache(max_entries: usize, max_bytes: u64) -> MappingCache {
    MappingCache::new(max_entries, max_bytes, Duration::from_secs(5))
}

fn region_of(mapping: &HostMapping) -> &Arc<Region> {
    mapping.region.as_ref().unwrap()
}

#[test]
fn cache_reuses_a_mapping_of_the_same_file() {
    let cache = cache(8, 1 << 30);
    let file = memfd(8192, libc::F_SEAL_SHRINK);
    let first = HostMapping::new_in(&cache, &file, window(0, 4096)).unwrap();
    let second = HostMapping::new_in(&cache, &file, window(4096, 4096)).unwrap();
    assert!(Arc::ptr_eq(region_of(&first), region_of(&second)));
    // Another descriptor for the same file shares the mapping too.
    let path = format!("/proc/self/fd/{}", file.as_raw_fd());
    let reopened = std::fs::OpenOptions::new()
        .read(true)
        .write(true)
        .open(&path)
        .unwrap();
    let third = HostMapping::new_in(&cache, &reopened, window(0, 8192)).unwrap();
    assert!(Arc::ptr_eq(region_of(&first), region_of(&third)));
    let stats = cache.stats();
    assert_eq!((stats.created, stats.reuses), (1, 2));
    assert_eq!((stats.cached, stats.cached_bytes), (1, 8192));

    let other = memfd(4096, libc::F_SEAL_SHRINK);
    let fourth = HostMapping::new_in(&cache, &other, window(0, 4096)).unwrap();
    assert!(!Arc::ptr_eq(region_of(&first), region_of(&fourth)));
    assert_eq!(cache.stats().created, 2);
}

#[test]
fn cache_checks_every_request() {
    let cache = cache(8, 1 << 30);
    let file = memfd(8192, libc::F_SEAL_SHRINK);
    assert!(HostMapping::new_in(&cache, &file, window(0, 4096)).is_some());
    // A read-only descriptor for the cached file is still refused.
    let path = format!("/proc/self/fd/{}", file.as_raw_fd());
    let read_only = std::fs::File::open(&path).unwrap();
    assert!(HostMapping::new_in(&cache, &read_only, window(0, 4096)).is_none());
    // So is a window past the end of the file.
    assert!(HostMapping::new_in(&cache, &file, window(4096, 8192)).is_none());
    // A later write seal stops reuse.
    assert_eq!(
        unsafe { libc::fcntl(file.as_raw_fd(), libc::F_ADD_SEALS, libc::F_SEAL_WRITE) },
        -1,
        "a writable shared mapping blocks F_SEAL_WRITE"
    );
    let unsealed = memfd(8192, 0);
    assert!(HostMapping::new_in(&cache, &unsealed, window(0, 4096)).is_none());
    assert_eq!(cache.stats().reuses, 0);
}

#[test]
fn cache_remaps_a_file_that_grew() {
    use std::os::unix::fs::FileExt;
    let cache = cache(8, 1 << 30);
    let file = memfd(4096, libc::F_SEAL_SHRINK);
    let small = HostMapping::new_in(&cache, &file, window(0, 4096)).unwrap();
    file.set_len(3 * 4096).unwrap();
    let large = HostMapping::new_in(&cache, &file, window(4096, 8192)).unwrap();
    assert!(!Arc::ptr_eq(region_of(&small), region_of(&large)));
    assert_eq!(region_of(&large).length, 3 * 4096);
    let stats = cache.stats();
    assert_eq!(
        (stats.created, stats.cached, stats.cached_bytes),
        (2, 1, 3 * 4096)
    );
    // The old mapping still works for its request.
    unsafe { small.bytes_mut(0, 4096).unwrap().fill(1) };
    unsafe { large.bytes_mut(4096, 8192).unwrap().fill(2) };
    let mut contents = vec![0; 3 * 4096];
    file.read_exact_at(&mut contents, 0).unwrap();
    assert!(contents[..4096].iter().all(|&b| b == 1));
    assert!(contents[4096..].iter().all(|&b| b == 2));
    // Small windows reuse the larger mapping.
    let again = HostMapping::new_in(&cache, &file, window(0, 10)).unwrap();
    assert!(Arc::ptr_eq(region_of(&again), region_of(&large)));
}

#[test]
fn cache_evicts_least_recently_used_beyond_its_caps() {
    let cache = cache(2, 1 << 30);
    let files: Vec<_> = (0..3).map(|_| memfd(4096, libc::F_SEAL_SHRINK)).collect();
    let first = HostMapping::new_in(&cache, &files[0], window(0, 4096)).unwrap();
    let held = region_of(&first).clone();
    drop(first);
    HostMapping::new_in(&cache, &files[1], window(0, 4096)).unwrap();
    HostMapping::new_in(&cache, &files[2], window(0, 4096)).unwrap();
    let stats = cache.stats();
    assert_eq!((stats.cached, stats.evicted), (2, 1));
    // The evicted mapping stays valid while something holds it.
    assert!(Arc::strong_count(&held) >= 1);
    unsafe { std::slice::from_raw_parts_mut(held.base.as_ptr(), 4096).fill(3) };
    // The byte cap evicts too.
    let cache = super::tests::cache(8, 8192);
    let files: Vec<_> = (0..3).map(|_| memfd(4096, libc::F_SEAL_SHRINK)).collect();
    for file in &files {
        HostMapping::new_in(&cache, file, window(0, 4096)).unwrap();
    }
    let stats = cache.stats();
    assert_eq!(
        (stats.cached, stats.cached_bytes, stats.evicted),
        (2, 8192, 1)
    );
    // A file larger than the byte cap is mapped but not cached.
    let big = memfd(3 * 4096, libc::F_SEAL_SHRINK);
    let mapping = HostMapping::new_in(&cache, &big, window(0, 4096)).unwrap();
    assert_eq!(Arc::strong_count(region_of(&mapping)), 1);
}

#[test]
fn cache_evicts_idle_mappings_but_not_ones_in_use() {
    let cache = cache(8, 1 << 30);
    let busy = memfd(4096, libc::F_SEAL_SHRINK);
    let idle = memfd(4096, libc::F_SEAL_SHRINK);
    let in_use = HostMapping::new_in(&cache, &busy, window(0, 4096)).unwrap();
    drop(HostMapping::new_in(&cache, &idle, window(0, 4096)).unwrap());
    // HostMapping::drop hands the region to the reaper; wait for it to let go.
    let deadline = Instant::now() + Duration::from_secs(10);
    let later = Instant::now() + Duration::from_secs(6);
    let mut evicted = Vec::new();
    while evicted.is_empty() && Instant::now() < deadline {
        evicted = cache.take_idle(later);
        std::thread::sleep(Duration::from_millis(10));
    }
    assert_eq!(evicted.len(), 1);
    assert!(cache.take_idle(Instant::now()).is_empty(), "not idle yet");
    let stats = cache.stats();
    assert_eq!((stats.cached, stats.evicted), (1, 1));
    unsafe { in_use.bytes_mut(0, 4096).unwrap().fill(1) };
    drop(in_use);
}

/// Compare mapping, populating, and unmapping a 64 MiB window per request
/// with reusing a cached mapping. Run with
/// `cargo test -p racer --release --lib host_map_bench -- --ignored --nocapture`.
#[test]
#[ignore = "benchmark"]
fn host_map_bench() {
    const WINDOW: u64 = 64 << 20;
    const SIZE: u64 = 4 << 30;
    const ROUNDS: usize = 4;
    let file = memfd(0, 0);
    file.set_len(SIZE).unwrap();
    assert_eq!(
        unsafe { libc::fcntl(file.as_raw_fd(), libc::F_ADD_SEALS, libc::F_SEAL_SHRINK) },
        0
    );
    let run = |cache: &MappingCache| {
        let started = Instant::now();
        for _ in 0..ROUNDS {
            for offset in (0..SIZE).step_by(WINDOW as usize) {
                let mapping = HostMapping::new_in(cache, &file, window(offset, WINDOW)).unwrap();
                let bytes = unsafe { mapping.bytes_mut(offset, WINDOW as usize).unwrap() };
                mapping.populate(offset, bytes).unwrap();
                // Unmap here, as the uncached path did per request.
                let region = mapping.region.as_ref().unwrap().clone();
                drop(mapping);
                while Arc::strong_count(&region) > 1 + usize::from(cache.stats().cached > 0) {
                    std::thread::yield_now();
                }
                drop(region);
            }
        }
        started.elapsed() / (ROUNDS as u32 * (SIZE / WINDOW) as u32)
    };
    let uncached = run(&MappingCache::new(0, 0, Duration::ZERO));
    let cached = run(&MappingCache::new(8, SIZE, Duration::from_secs(60)));
    eprintln!("per 64 MiB request: uncached {uncached:?}, cached {cached:?}");
}

#[test]
fn populated_chunks_are_tracked_per_mapping() {
    const CHUNK: usize = POPULATED_CHUNK;
    let cache = cache(8, 1 << 40);
    let file = memfd(0, 0);
    file.set_len(3 * CHUNK as u64).unwrap();
    assert_eq!(
        unsafe { libc::fcntl(file.as_raw_fd(), libc::F_ADD_SEALS, libc::F_SEAL_SHRINK) },
        0
    );
    let length = 3 * CHUNK as u64;
    let mapping = HostMapping::new_in(&cache, &file, window(0, length)).unwrap();
    let region = region_of(&mapping);
    assert!(!region.is_populated(0, 1));
    // A partial chunk is populated but not marked.
    let bytes = unsafe { mapping.bytes_mut(1, CHUNK).unwrap() };
    mapping.populate(1, bytes).unwrap();
    assert!(!region.is_populated(0, 1));
    assert!(!region.is_populated(CHUNK, 1));
    let bytes = unsafe { mapping.bytes_mut(CHUNK as u64, CHUNK).unwrap() };
    mapping.populate(CHUNK as u64, bytes).unwrap();
    assert!(region.is_populated(CHUNK, CHUNK));
    assert!(!region.is_populated(CHUNK, CHUNK + 1));
    // A later request for the same file sees the marks.
    let again = HostMapping::new_in(&cache, &file, window(0, length)).unwrap();
    assert!(region_of(&again).is_populated(CHUNK + 5, 10));
}

/// The destination node is unknown until a page is resident, is then the
/// node the page is on, is cached per chunk for later requests, and is never
/// reported outside the window.
#[test]
fn destination_node_is_found_without_faulting_and_cached() {
    const CHUNK: usize = POPULATED_CHUNK;
    let cache = cache(8, 1 << 40);
    let file = memfd(0, 0);
    file.set_len(2 * CHUNK as u64).unwrap();
    assert_eq!(
        unsafe { libc::fcntl(file.as_raw_fd(), libc::F_ADD_SEALS, libc::F_SEAL_SHRINK) },
        0
    );
    let mapping = HostMapping::new_in(&cache, &file, window(CHUNK as u64, CHUNK as u64)).unwrap();
    let start = CHUNK as u64;
    assert_eq!(mapping.node_at(start), None, "nothing resident yet");
    assert_eq!(mapping.node_at(start), None, "the query does not fault");
    assert_eq!(mapping.node_at(0), None, "outside the window");
    assert_eq!(mapping.node_at(2 * start), None, "past the window");
    let bytes = unsafe { mapping.bytes_mut(start, 4096).unwrap() };
    bytes.fill(1);
    let Some(node) = crate::affinity::resident_numa_node(bytes.as_ptr()) else {
        // Seccomp denies move_pages here: the node stays unknown.
        assert_eq!(mapping.node_at(start), None);
        return;
    };
    let node = u16::try_from(node).unwrap();
    assert_eq!(mapping.node_at(start + 100), Some(node));
    let region = region_of(&mapping);
    assert_eq!(region.nodes[1].load(Ordering::Relaxed), node + 1);
    // Another request for the same file reads the cached node.
    let again = HostMapping::new_in(&cache, &file, window(0, 2 * start)).unwrap();
    assert_eq!(again.node_at(start + CHUNK as u64 - 1), Some(node));
    assert_eq!(again.node_at(0), None);
}

/// A host target maps each page of a range to the file offset its first
/// byte lands at, and reports that chunk's node.
#[test]
fn sealed_target_finds_each_page_destination_node() {
    const CHUNK: usize = POPULATED_CHUNK;
    const MIB: u64 = 1 << 20;
    let cache = cache(8, 1 << 40);
    let file = memfd(0, 0);
    file.set_len(4 * CHUNK as u64).unwrap();
    assert_eq!(
        unsafe { libc::fcntl(file.as_raw_fd(), libc::F_ADD_SEALS, libc::F_SEAL_SHRINK) },
        0
    );
    let mapping = Arc::new(HostMapping::new_in(&cache, &file, window(MIB, 6 * MIB)).unwrap());
    // 2 MiB pages; the range starts 1 MiB into page 0 and lands at file
    // offset 1 MiB, so page n > 0 starts at file offset 2n MiB.
    let size = crate::model::PageSize::new(2 * MIB).unwrap();
    let range = crate::model::ByteRange::closed(MIB, 7 * MIB - 1)
        .unwrap()
        .resolve_paged(16 * MIB, size)
        .unwrap();
    let target = crate::read::sealed::SealedTarget::host_into(mapping.clone(), MIB);
    let page = |n| target.destination_node(&range, crate::model::PageNumber(n));
    assert!((0..4).all(|n| page(n).is_none()), "nothing resident");
    // Touch file offset 4 MiB, the start of page 2 and of chunk 2.
    let bytes = unsafe { mapping.bytes_mut(4 * MIB, 4096).unwrap() };
    bytes.fill(1);
    let Some(node) = crate::affinity::resident_numa_node(bytes.as_ptr()) else {
        return;
    };
    let node = u16::try_from(node).unwrap();
    assert_eq!(page(2), Some(node));
    assert_eq!(page(1), None, "page 1 starts in chunk 1");
    assert_eq!(page(4), None, "outside the range");
    assert_eq!(
        crate::read::sealed::SealedTarget::host()
            .destination_node(&range, crate::model::PageNumber(2)),
        None
    );
}

/// Each buffer's locality maps to exactly one labeled event, and an empty or
/// unknown buffer is never reported as local or remote.
#[test]
fn decrypt_numa_events_follow_locality() {
    use crate::telemetry::Event as E;
    let numa = DecryptNuma {
        registered: Locality::Remote,
        store: Locality::Local,
        destination: Locality::Unknown,
    };
    assert_eq!(
        numa.events(),
        [
            E::HostReadFusedNumaRegisteredRemote,
            E::HostReadFusedNumaStoreLocal,
            E::HostReadFusedNumaDestinationUnknown,
        ]
    );
    assert_eq!(Locality::of(Some(0), &[]), Locality::Unknown);
    let written = vec![7u8; 4096];
    assert_eq!(Locality::of(None, &written), Locality::Unknown);
    if let Some(node) = crate::affinity::page_numa_node(&written) {
        assert_eq!(Locality::of(Some(node), &written), Locality::Local);
        assert_eq!(Locality::of(Some(node + 1), &written), Locality::Remote);
    }
}
