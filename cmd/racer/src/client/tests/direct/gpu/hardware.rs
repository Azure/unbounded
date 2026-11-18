//! `gpu-read` end to end over the client socket on a real GPU: the listener,
//! the read worker, and the helper's executors and cuFile, writing into an
//! exported client allocation. Gated as [`crate::gpu::hardware`].
use super::*;
use crate::gpu::hardware::Allocation;
use crate::gpu::hardware::FILL;
use crate::gpu::hardware::Rig;
use crate::gpu::hardware::assert_bytes;
use crate::gpu::hardware::assert_nothing_held;
use crate::gpu::hardware::assert_telemetry;
use crate::gpu::hardware::cufile_off_reason;
use crate::gpu::hardware::rig;
use crate::gpu::hardware::skip_cufile;

/// Bytes before the destination, left as they were.
const AT: u64 = 64;

/// A response head, its frames, and the allocation the read wrote.
type Landed = (String, Vec<(u8, u64, u64, u32)>, Allocation);

/// Read `range` of the object into a fresh allocation at [`AT`], naming
/// the GPU by UUID as the client does.
fn read(fixture: &Fixture, rig: &Rig, range: Option<(u64, u64)>, length: u64) -> Landed {
    let allocation = rig.allocate(AT + length);
    let mut header = range.map_or(String::new(), |(start, end)| {
        format!("Range: bytes={start}-{end}\r\n")
    });
    header.push_str(&format!("Racer-Gpu-Uuid: {}\r\n", rig.device.display_id()));
    let fd = allocation.export();
    let output = exchange(
        fixture,
        &direct_request("gpu-read", &fields(&header, AT, length, allocation.size())),
        &[fd.as_raw_fd()],
    );
    drop(fd);
    let (head, body) = split(&output);
    let frames = frames(body);
    settle(fixture);
    (head, frames, allocation)
}

/// `expected` landed at [`AT`] of `allocation`, and nothing else changed.
fn assert_landed(what: &str, allocation: &Allocation, expected: &[u8]) {
    let mut image = vec![FILL; allocation.size() as usize];
    image[AT as usize..AT as usize + expected.len()].copy_from_slice(expected);
    assert_bytes(what, &allocation.read(0, image.len()), &image);
}

#[test]
/// A cold read from the origin, a memory ciphertext hit, and a read of
/// pages on disk land in real GPU memory, and each is timed as a
/// `gpu-read`. The GPU decrypts the sealed pages; disk pages go through
/// cuFile, or through host memory when cuFile is off for a named reason.
/// A client that disconnects mid-read leaves nothing held.
fn gpu_read_lands_origin_memory_and_disk_pages() {
    const TEST: &str = "gpu_read_lands_origin_memory_and_disk_pages";
    let Some(rig) = rig(TEST) else {
        return;
    };
    let env = racer_io::Env::host();
    let length = 2 * PAGE_BYTES as usize + 4099;
    let data = body(length);
    let fixture = gpu_fixture(&env, data.clone(), &rig.helper);
    let worker = worker(&fixture);
    let counters = rig.helper.counters();
    let metrics = &fixture.listeners.metrics;
    worker.retain(true);
    worker.open_disk();
    let whole = [
        (1, 0, 0, PAGE_BYTES as u32),
        (1, 1, PAGE_BYTES, PAGE_BYTES as u32),
        (1, 2, 2 * PAGE_BYTES, 4099),
        (2, 3, length as u64, 0),
    ];

    // Cold: fetched from the origin, sealed for the cache.
    let pages = counter(&counters.decrypted) + counter(&counters.plaintext);
    let (head, frames, allocation) = read(&fixture, &rig, None, length as u64);
    assert!(head.starts_with("http/1.1 200"), "{head}");
    assert!(head.contains("racer-delivery: gpu\r\n"), "{head}");
    assert_eq!(frames, whole);
    assert_landed("cold read", &allocation, &data);
    assert_eq!(
        counter(&counters.decrypted) + counter(&counters.plaintext) - pages,
        3
    );

    // The cache keeps ciphertext only: the GPU decrypts part of a page.
    let decrypted = counter(&counters.decrypted);
    let cpu = worker.cpu_decrypts();
    let (head, frames, allocation) = read(&fixture, &rig, Some((10, 99)), 90);
    assert!(head.starts_with("http/1.1 200"), "{head}");
    assert_eq!(frames, [(1, 0, 10, 90), (2, 1, 90, 0)]);
    assert_landed("memory hit", &allocation, &data[10..100]);
    assert_eq!(counter(&counters.decrypted), decrypted + 1);
    assert_eq!(worker.cpu_decrypts(), cpu);

    // Only on disk now.
    let copies: Vec<_> = (0..3)
        .map(|number| {
            worker
                .memory()
                .ciphertext(&page_id(number))
                .unwrap()
                .unwrap()
        })
        .collect();
    worker.memory().remove_cache(&definition().id).unwrap();
    for copy in copies {
        worker.persist(copy);
    }
    let decrypted = counter(&counters.decrypted);
    let staged = counter(&counters.staged);
    let direct = worker.metric(Event::GpuDirectDiskRead);
    let (head, frames, allocation) = read(&fixture, &rig, None, length as u64);
    assert!(head.starts_with("http/1.1 200"), "{head}");
    assert_eq!(frames, whole);
    assert_landed("disk read", &allocation, &data);
    assert_eq!(counter(&counters.decrypted), decrypted + 3);
    assert_eq!(worker.cpu_decrypts(), cpu, "the GPU decrypted every page");
    let staged = counter(&counters.staged) - staged;
    let direct = worker.metric(Event::GpuDirectDiskRead) - direct;
    if staged == 3 {
        assert_eq!(direct, 3);
    } else {
        assert_eq!((staged, direct), (0, 0), "cuFile read some pages only");
        use std::os::unix::fs::OpenOptionsExt;
        let slab: OwnedFd = std::fs::OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_DIRECT)
            .open(worker.slab_path())
            .unwrap()
            .into();
        skip_cufile(TEST, cufile_off_reason(&rig, Some(&slab)));
    }

    // Each read was timed as a gpu-read, from its first page and in whole.
    assert_eq!(metrics.count(Event::GpuReadRequestCount), 3);
    assert_eq!(metrics.count(Event::GpuReadFirstPageCount), 3);
    let first = metrics.count(Event::GpuReadFirstPageNs);
    assert!(first > 0);
    assert!(metrics.count(Event::GpuReadRequestNs) >= first);
    assert_eq!(metrics.count(Event::HostReadRequestCount), 0);
    let mut text = String::new();
    metrics.write_prometheus(&mut text).unwrap();
    for line in [
        "racer_gpu_read_request_nanoseconds_count 3\n",
        "racer_gpu_read_first_page_nanoseconds_count 3\n",
        "racer_gpu_read_requests_total{outcome=\"gpu\"} 3\n",
    ] {
        assert!(text.contains(line), "{line}{text}");
    }

    // A client that goes away mid-read releases its import.
    let allocation = rig.allocate(length as u64);
    let fd = allocation.export();
    let mut socket = fixture.connect();
    send_with_fds(
        &socket,
        &direct_request("gpu-read", &fields("", 0, length as u64, allocation.size())),
        &[fd.as_raw_fd()],
    );
    drop(fd);
    let output = fixture.receive(&mut socket, false);
    assert!(split(&output).0.starts_with("http/1.1 200"));
    drop(socket);
    settle(&fixture);
    assert_nothing_held(&rig.helper);
    assert_eq!(rig.helper.free_slots(rig.ordinal()), default_slots());
    assert_telemetry(&rig.helper);
}
