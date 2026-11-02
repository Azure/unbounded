//! `gpu-read` end to end over the client socket, on simulated GPUs.
use super::*;
use crate::config::GpuMode;
use crate::gpu::GpuClient;
use crate::gpu::GpuHelper;
use crate::telemetry::Event;
use racer_gpu::simulation::Environment;
use racer_gpu::simulation::Live;
use racer_gpu::simulation::Operation;
use racer_gpu::simulation::SimDevice;
use racer_gpu::simulation::Simulation;
use racer_gpu::simulation::read_allocation;

const GRANULE: u64 = racer_gpu::simulation::GRANULARITY;

thread_local! {
    /// The simulation, on the GPU helper thread only.
    static SIM: RefCell<Option<(Simulation, Environment)>> = const { RefCell::new(None) };
}

fn sim() -> Simulation {
    SIM.with(|sim| sim.borrow().as_ref().unwrap().0.clone())
}

/// A helper backed by `count` simulated GPUs.
fn start(mode: GpuMode, count: u8) -> Option<Arc<GpuHelper>> {
    let devices: Vec<SimDevice> = (0..count).map(SimDevice::new).collect();
    GpuHelper::start_with(
        mode,
        Box::new(move || {
            let simulation = Simulation::new(devices);
            let environment = simulation.enter();
            SIM.with(|sim| *sim.borrow_mut() = Some((simulation, environment)));
            racer_gpu::Gpu::load()
        }),
    )
    .unwrap()
}

fn allocate(helper: &GpuHelper, ordinal: i32, size: u64) -> OwnedFd {
    helper.run(move || sim().allocate(ordinal, size))
}

fn live(helper: &GpuHelper) -> Live {
    helper.run(|| sim().live())
}

/// No request holds GPU resources. Contexts stay retained by the helper.
fn assert_released(helper: &GpuHelper) {
    let live = live(helper);
    assert_eq!((live.imports, live.streams), (0, 0), "{live:?}");
    assert_eq!(helper.open_requests(), 0);
    assert_eq!(helper.leaked(), 0);
}

fn gpu_fixture(body: Vec<u8>, helper: &Arc<GpuHelper>) -> Fixture {
    direct_fixture(body, Some(Rc::new(GpuClient::new(helper.clone()))))
}

fn fields(range: &str, offset: u64, length: u64, size: u64) -> String {
    format!(
        "{range}Racer-Destination-Offset: {offset}\r\nRacer-Destination-Length: {length}\r\n\
         Racer-Allocation-Size: {size}\r\n"
    )
}

#[test]
fn gpu_read_writes_the_range_at_the_destination_offset() {
    let helper = start(GpuMode::Required, 2).unwrap();
    assert_eq!(helper.devices().len(), 2);
    let fixture = gpu_fixture(b"hello".to_vec(), &helper);
    // The allocation is on the second GPU, so the helper must find it.
    let allocation = allocate(&helper, 1, GRANULE);
    let output = exchange(
        &fixture,
        &direct_request(
            "gpu-read",
            &fields("Range: bytes=1-3\r\nIf-Match: \"v1\"\r\n", 100, 3, GRANULE),
        ),
        &[allocation.as_raw_fd()],
    );
    let (head, body) = split(&output);
    assert!(head.starts_with("http/1.1 200"), "{head}");
    assert!(head.contains("racer-delivery: gpu\r\n"), "{head}");
    assert!(head.contains("content-length: 42\r\n"), "{head}");
    assert_eq!(frames(body), [(1, 0, 1, 3), (2, 1, 3, 0)]);
    assert_eq!(read_allocation(&allocation, 99, 5), b"\0ell\0");
    settle(&fixture);
    assert_released(&helper);
    assert_no_body_leases(&fixture);
}

/// A UUID header as `nvidia-smi` prints it for simulated GPU `index`.
fn uuid_field(index: u8) -> String {
    format!(
        "Racer-Gpu-Uuid: {}\r\n",
        racer_gpu::uuid_string(&[index; 16])
    )
}

/// Serve a five-byte `gpu-read` into an allocation on GPU `ordinal` of four,
/// sending `extra` fields. Returns the status line and the import attempts.
fn read_on_four_gpus(ordinal: i32, extra: &str) -> (String, usize, Vec<u8>) {
    let helper = start(GpuMode::Required, 4).unwrap();
    let fixture = gpu_fixture(b"hello".to_vec(), &helper);
    let allocation = allocate(&helper, ordinal, GRANULE);
    let from = trace_len(&helper);
    let output = exchange(
        &fixture,
        &direct_request("gpu-read", &format!("{}{extra}", fields("", 0, 5, GRANULE))),
        &[allocation.as_raw_fd()],
    );
    let (head, _) = split(&output);
    let status = head.lines().next().unwrap().to_string();
    let imports = calls(&helper, from, Operation::Import);
    settle(&fixture);
    assert_released(&helper);
    assert_no_body_leases(&fixture);
    (status, imports, read_allocation(&allocation, 0, 5))
}

#[test]
fn gpu_read_uuid_selects_a_nonzero_ordinal_first() {
    // The client's container sees this GPU as device 0; the dataplane sees it
    // as device 3. The UUID is the same in both.
    let (status, imports, written) = read_on_four_gpus(3, &uuid_field(3));
    assert!(status.starts_with("http/1.1 200"), "{status}");
    assert_eq!(imports, 1, "only the named GPU is tried");
    assert_eq!(written, b"hello");
}

#[test]
fn gpu_read_without_uuid_tries_each_gpu() {
    let (status, imports, written) = read_on_four_gpus(3, "");
    assert!(status.starts_with("http/1.1 200"), "{status}");
    assert_eq!(imports, 4);
    assert_eq!(written, b"hello");
}

#[test]
fn gpu_read_unknown_uuid_is_501_so_the_client_uses_the_host() {
    let (status, imports, written) = read_on_four_gpus(3, &uuid_field(9));
    assert!(status.starts_with("http/1.1 501"), "{status}");
    assert_eq!(imports, 0, "nothing is imported");
    assert_eq!(written, [0; 5]);
}

#[test]
fn gpu_read_wrong_uuid_still_finds_the_owning_gpu() {
    let (status, imports, written) = read_on_four_gpus(2, &uuid_field(1));
    assert!(status.starts_with("http/1.1 200"), "{status}");
    // GPU 1 first, then 0, then 2.
    assert_eq!(imports, 3);
    assert_eq!(written, b"hello");
}

#[test]
fn gpu_read_spans_pages() {
    let length = 2 * PAGE_BYTES as usize + 7;
    let body: Vec<u8> = (0..length).map(|i| (i % 251) as u8).collect();
    let helper = start(GpuMode::Auto, 1).unwrap();
    let fixture = gpu_fixture(body.clone(), &helper);
    let size = (length as u64 + 10).next_multiple_of(GRANULE);
    let allocation = allocate(&helper, 0, size);
    let output = exchange(
        &fixture,
        &direct_request("gpu-read", &fields("", 10, length as u64, size)),
        &[allocation.as_raw_fd()],
    );
    let (head, body_frames) = split(&output);
    assert!(head.starts_with("http/1.1 200"), "{head}");
    assert_eq!(
        frames(body_frames),
        [
            (1, 0, 0, PAGE_BYTES as u32),
            (1, 1, PAGE_BYTES, PAGE_BYTES as u32),
            (1, 2, 2 * PAGE_BYTES, 7),
            (2, 3, length as u64, 0)
        ]
    );
    assert!(read_allocation(&allocation, 10, length) == body);
    settle(&fixture);
    assert_released(&helper);
}

#[test]
fn gpu_read_errors_before_the_head_release_everything() {
    let helper = start(GpuMode::Required, 1).unwrap();
    let fixture = gpu_fixture(b"hello".to_vec(), &helper);
    let allocation = allocate(&helper, 0, GRANULE);
    // On a GPU the dataplane did not discover.
    let foreign = allocate(&helper, 3, GRANULE);
    for (fields, fd, status) in [
        (
            fields("If-Match: \"v2\"\r\n", 0, 5, GRANULE),
            &allocation,
            "412",
        ),
        // Larger than the real allocation.
        (fields("", 0, 5, 2 * GRANULE), &allocation, "400"),
        // Not a whole number of granules.
        (fields("", 0, 5, GRANULE - 1), &allocation, "400"),
        (fields("", 0, 5, GRANULE), &foreign, "501"),
    ] {
        let output = exchange(
            &fixture,
            &direct_request("gpu-read", &fields),
            &[fd.as_raw_fd()],
        );
        let (head, body) = split(&output);
        assert!(
            head.starts_with(&format!("http/1.1 {status}")),
            "{fields} {head}"
        );
        assert!(body.is_empty());
        settle(&fixture);
        assert_released(&helper);
    }
    assert_eq!(
        read_allocation(&allocation, 0, 5),
        [0; 5],
        "nothing written"
    );
    assert_no_body_leases(&fixture);
    // The 412 comes after the import, so it counts as a GPU open.
    use crate::gpu_telemetry::GpuReadOutcome;
    let gpu = fixture.listeners.metrics.gpu();
    assert_eq!(gpu.requests(GpuReadOutcome::Gpu), 1);
    assert_eq!(gpu.requests(GpuReadOutcome::Invalid), 2);
    assert_eq!(gpu.requests(GpuReadOutcome::WrongDevice), 1);
}

#[test]
fn gpu_read_driver_fault_fails_the_read_and_releases_the_import() {
    let length = 2 * PAGE_BYTES as usize;
    let helper = start(GpuMode::Required, 1).unwrap();
    let fixture = gpu_fixture(vec![b'x'; length], &helper);
    let size = (length as u64).next_multiple_of(GRANULE);
    let allocation = allocate(&helper, 0, size);
    helper.run(|| sim().fault(Operation::CopyH2d, -5));
    let output = exchange(
        &fixture,
        &direct_request("gpu-read", &fields("", 0, length as u64, size)),
        &[allocation.as_raw_fd()],
    );
    let (head, body) = split(&output);
    assert!(head.starts_with("http/1.1 200"), "{head}");
    assert!(
        frames(body).iter().all(|frame| frame.0 != 2),
        "no kind 2 frame after a failure"
    );
    settle(&fixture);
    assert_released(&helper);
    fixture.listeners.admission.reclaim_buffers();
    assert_no_body_leases(&fixture);
}

#[test]
fn gpu_read_client_disconnect_releases_the_import() {
    let length = 3 * PAGE_BYTES as usize + 1;
    let helper = start(GpuMode::Required, 1).unwrap();
    let fixture = gpu_fixture(vec![b'x'; length], &helper);
    let size = (length as u64).next_multiple_of(GRANULE);
    let allocation = allocate(&helper, 0, size);
    let mut socket = fixture.connect();
    send_with_fds(
        &socket,
        &direct_request("gpu-read", &fields("", 0, length as u64, size)),
        &[allocation.as_raw_fd()],
    );
    let output = fixture.receive(&mut socket, false);
    assert!(split(&output).0.starts_with("http/1.1 200"));
    drop(socket);
    settle(&fixture);
    assert_released(&helper);
    fixture.listeners.admission.reclaim_buffers();
    assert_no_body_leases(&fixture);
}

#[test]
fn gpu_mode_resolves_against_hardware() {
    assert!(start(GpuMode::Disabled, 1).is_none());
    assert!(start(GpuMode::Auto, 0).is_none());
    assert!(
        GpuHelper::start_with(
            GpuMode::Required,
            Box::new(|| Err(racer_gpu::Error::Unavailable))
        )
        .is_err()
    );
    let unusable = SimDevice {
        compute_mode: 3,
        ..SimDevice::new(0)
    };
    assert!(
        GpuHelper::start_with(
            GpuMode::Required,
            Box::new(move || {
                let simulation = Simulation::new(vec![unusable]);
                std::mem::forget(simulation.enter());
                racer_gpu::Gpu::load()
            })
        )
        .is_err()
    );
    // Without the cuda feature, auto never loads a backend.
    if !cfg!(feature = "cuda") {
        assert!(
            GpuHelper::start(GpuMode::Auto, crate::gpu::GpuOptions::default())
                .unwrap()
                .is_none()
        );
    }
}

/// A helper on one simulated GPU with a staging cap, after `setup` runs on
/// the fresh simulation.
fn start_with(staging: u64, setup: fn(&Simulation)) -> Arc<GpuHelper> {
    GpuHelper::start_with_staging(
        GpuMode::Required,
        staging,
        Box::new(move || {
            let simulation = Simulation::new(vec![SimDevice::new(0)]);
            let environment = simulation.enter();
            setup(&simulation);
            SIM.with(|sim| *sim.borrow_mut() = Some((simulation, environment)));
            racer_gpu::Gpu::load()
        }),
    )
    .unwrap()
    .unwrap()
}

fn page_id(number: u64) -> crate::model::PageId {
    crate::model::PageId {
        version: ObjectVersion {
            object: ObjectId {
                cache: definition().id,
                key: CacheKey([0; 32]),
            },
            etag: StrongEtag::parse(b"\"v1\"").unwrap(),
        },
        number: crate::model::PageNumber(number),
    }
}

fn worker(fixture: &Fixture) -> &crate::test_support::ReadWorker {
    fixture.worker.as_ref().unwrap()
}

/// Calls to `operation` in the trace from `from` on.
fn calls(helper: &GpuHelper, from: usize, operation: Operation) -> usize {
    helper.run(move || {
        sim().trace()[from..]
            .iter()
            .filter(|event| event.operation == operation)
            .count()
    })
}

fn trace_len(helper: &GpuHelper) -> usize {
    helper.run(|| sim().trace().len())
}

fn counter(value: &std::sync::atomic::AtomicU64) -> u64 {
    value.load(std::sync::atomic::Ordering::Relaxed)
}

/// A response head, its frames, the allocation and its size.
type Read = (String, Vec<(u8, u64, u64, u32)>, OwnedFd, u64);

/// Read `range` of the object into a fresh allocation at offset 64.
fn gpu_read(fixture: &Fixture, helper: &GpuHelper, range: Option<(u64, u64)>, length: u64) -> Read {
    let size = (length + 64).next_multiple_of(GRANULE);
    let allocation = allocate(helper, 0, size);
    let header = range.map_or(String::new(), |(start, end)| {
        format!("Range: bytes={start}-{end}\r\n")
    });
    let output = exchange(
        fixture,
        &direct_request("gpu-read", &fields(&header, 64, length, size)),
        &[allocation.as_raw_fd()],
    );
    let (head, body) = split(&output);
    let frames = frames(body);
    settle(fixture);
    (head, frames, allocation, size)
}

/// Fill the page cache with an ordinary read, then leave each page's
/// ciphertext in memory, on disk, or both, and no plaintext.
fn arrange(
    fixture: &Fixture,
    helper: &GpuHelper,
    pages: u64,
    length: u64,
    memory: bool,
    disk: bool,
) {
    let worker = worker(fixture);
    worker.retain(true);
    if disk {
        worker.open_disk();
    }
    let (head, _, _, _) = gpu_read(fixture, helper, None, length);
    assert!(head.starts_with("http/1.1 200"), "{head}");
    let mut copies = Vec::new();
    for number in 0..pages {
        let copy = worker
            .memory()
            .ciphertext(&page_id(number))
            .unwrap()
            .unwrap();
        copies.push(copy);
    }
    worker.memory().remove_cache(&definition().id).unwrap();
    for copy in copies {
        if disk {
            worker.persist(copy.clone());
        }
        if memory {
            worker
                .memory()
                .publish_ciphertext(crate::memory::UnverifiedPage {
                    copy,
                    disk_token: None,
                })
                .unwrap();
        }
    }
}

/// Nothing is held: no imports, streams, leaks or staged slots.
fn assert_idle(fixture: &Fixture, helper: &GpuHelper, slots: usize) {
    settle(fixture);
    assert_released(helper);
    assert_eq!(helper.free_slots(0), slots);
}

fn body(length: usize) -> Vec<u8> {
    (0..length).map(|i| (i * 7 % 251) as u8).collect()
}

/// Slots left for staged ciphertext with the default staging cap.
fn default_slots() -> usize {
    (crate::config::DEFAULT_GPU_STAGING_BYTES / crate::gpu::SLOT_BYTES) as usize - 2
}

/// The memory cache keeps only ciphertext, so a page read before is
/// decrypted again, on the GPU, from the cached ciphertext.
#[test]
fn gpu_read_memory_ciphertext_hit_decrypts_on_the_gpu() {
    let helper = start(GpuMode::Required, 1).unwrap();
    let data = body(5000);
    let fixture = gpu_fixture(data.clone(), &helper);
    worker(&fixture).retain(true);
    let _ = gpu_read(&fixture, &helper, None, 5000);
    assert!(
        worker(&fixture)
            .memory()
            .get(&page_id(0))
            .unwrap()
            .is_none()
    );
    assert!(
        worker(&fixture)
            .memory()
            .ciphertext(&page_id(0))
            .unwrap()
            .is_some()
    );
    let mark = trace_len(&helper);
    let decrypts = worker(&fixture).cpu_decrypts();
    let decrypted = counter(&helper.counters().decrypted);
    let (head, frames, allocation, _) = gpu_read(&fixture, &helper, Some((10, 99)), 90);
    assert!(head.starts_with("http/1.1 200"), "{head}");
    assert_eq!(frames, [(1, 0, 10, 90), (2, 1, 90, 0)]);
    assert_eq!(read_allocation(&allocation, 64, 90), &data[10..100]);
    assert_eq!(calls(&helper, mark, Operation::FileRead), 0);
    assert_eq!(calls(&helper, mark, Operation::CopyH2d), 1);
    assert_eq!(calls(&helper, mark, Operation::Decrypt), 1);
    assert_eq!(calls(&helper, mark, Operation::CopyD2d), 1);
    assert_eq!(worker(&fixture).cpu_decrypts(), decrypts);
    assert_eq!(counter(&helper.counters().decrypted), decrypted + 1);
    assert_eq!(worker(&fixture).metric(Event::GpuDirectDiskRead), 0);
    assert_idle(&fixture, &helper, default_slots());
}

/// A page cached in memory and on disk is staged from memory: cuFile does
/// not read the disk copy.
#[test]
fn gpu_read_prefers_memory_ciphertext_over_cufile() {
    let helper = start(GpuMode::Required, 1).unwrap();
    let data = body(5000);
    let fixture = gpu_fixture(data.clone(), &helper);
    arrange(&fixture, &helper, 1, 5000, true, true);
    assert!(worker(&fixture).disk_location(&page_id(0)).is_some());
    let mark = trace_len(&helper);
    let (head, frames, allocation, _) = gpu_read(&fixture, &helper, None, 5000);
    assert!(head.starts_with("http/1.1 200"), "{head}");
    assert_eq!(frames.len(), 2, "{frames:?}");
    assert_eq!(read_allocation(&allocation, 64, 5000), data);
    assert_eq!(calls(&helper, mark, Operation::FileRead), 0);
    assert_eq!(calls(&helper, mark, Operation::Decrypt), 1);
    assert_eq!(worker(&fixture).cpu_decrypts(), 0);
    assert_eq!(worker(&fixture).metric(Event::GpuDirectDiskRead), 0);
    assert_idle(&fixture, &helper, default_slots());
}

#[test]
fn gpu_read_retained_ciphertext_decrypts_part_of_a_page_on_the_gpu() {
    let helper = start(GpuMode::Required, 1).unwrap();
    let data = body(5000);
    let fixture = gpu_fixture(data.clone(), &helper);
    arrange(&fixture, &helper, 1, 5000, true, false);
    let mark = trace_len(&helper);
    let (head, frames, allocation, size) = gpu_read(&fixture, &helper, Some((100, 199)), 100);
    assert!(head.starts_with("http/1.1 200"), "{head}");
    assert_eq!(frames, [(1, 0, 100, 100), (2, 1, 100, 0)]);
    // Only the window holds bytes, though the GPU decrypted the whole page.
    let all = read_allocation(&allocation, 0, size as usize);
    assert_eq!(&all[64..164], &data[100..200]);
    assert!(all[..64].iter().chain(&all[164..]).all(|&b| b == 0));
    assert_eq!(calls(&helper, mark, Operation::Decrypt), 1);
    assert_eq!(calls(&helper, mark, Operation::CopyD2d), 1);
    assert_eq!(worker(&fixture).cpu_decrypts(), 0);
    assert_eq!(counter(&helper.counters().decrypted), 1);
    assert_idle(&fixture, &helper, default_slots());
}

#[test]
fn gpu_read_disk_pages_go_through_cufile_and_gpu_decrypt() {
    let length = 2 * PAGE_BYTES as usize + 4099;
    let helper = start(GpuMode::Required, 1).unwrap();
    let data = body(length);
    let fixture = gpu_fixture(data.clone(), &helper);
    arrange(&fixture, &helper, 3, length as u64, false, true);
    let mark = trace_len(&helper);
    let (head, frames, allocation, _) = gpu_read(&fixture, &helper, None, length as u64);
    assert!(head.starts_with("http/1.1 200"), "{head}");
    assert_eq!(frames.len(), 4, "{frames:?}");
    assert_eq!(frames[3], (2, 3, length as u64, 0));
    assert!(read_allocation(&allocation, 64, length) == data);
    assert_eq!(calls(&helper, mark, Operation::FileRead), 3);
    assert_eq!(calls(&helper, mark, Operation::Decrypt), 3);
    // Whole pages decrypt straight into the import.
    assert_eq!(calls(&helper, mark, Operation::CopyD2d), 0);
    assert_eq!(calls(&helper, mark, Operation::CopyH2d), 0);
    assert_eq!(worker(&fixture).cpu_decrypts(), 0);
    assert_eq!(counter(&helper.counters().staged), 3);
    assert_eq!(counter(&helper.counters().decrypted), 3);
    assert_eq!(worker(&fixture).metric(Event::GpuDirectDiskRead), 3);
    assert!(worker(&fixture).metric(Event::GpuDirectDiskReadBytes) >= length as u64);
    assert_idle(&fixture, &helper, default_slots());
}

#[test]
/// Two concurrent `gpu-read`s on two GPUs that can copy from each other
/// read each disk page once with cuFile. The second GPU copies the staged
/// ciphertext from the first and decrypts it itself.
fn concurrent_gpu_reads_on_peer_gpus_share_cufile_reads() {
    let length = 2 * PAGE_BYTES as usize + 4099;
    let helper = start(GpuMode::Required, 2).unwrap();
    let data = body(length);
    let fixture = gpu_fixture(data.clone(), &helper);
    arrange(&fixture, &helper, 3, length as u64, false, true);
    let reads = worker(&fixture).metric(Event::GpuDirectDiskRead);
    let mark = trace_len(&helper);
    let size = (length as u64 + 64).next_multiple_of(GRANULE);
    let allocations = [allocate(&helper, 0, size), allocate(&helper, 1, size)];
    let request = direct_request("gpu-read", &fields("", 64, length as u64, size));
    let mut sockets: Vec<_> = allocations
        .iter()
        .map(|allocation| {
            let socket = fixture.connect();
            send_with_fds(&socket, &request, &[allocation.as_raw_fd()]);
            socket
        })
        .collect();
    for (socket, allocation) in sockets.iter_mut().zip(&allocations) {
        let output = fixture.receive(socket, true);
        let (head, body) = split(&output);
        assert!(head.starts_with("http/1.1 200"), "{head}");
        assert_eq!(frames(body).len(), 4);
        assert!(read_allocation(allocation, 64, length) == data);
    }
    settle(&fixture);
    // One cuFile read per page; the other read joined each one.
    assert_eq!(worker(&fixture).metric(Event::GpuDirectDiskRead) - reads, 3);
    assert_eq!(calls(&helper, mark, Operation::FileRead), 3);
    assert_eq!(worker(&fixture).metric(Event::GpuDirectJoined), 3);
    assert_eq!(counter(&helper.counters().joined), 3);
    assert_eq!(counter(&helper.counters().peer_copies), 3);
    assert_eq!(calls(&helper, mark, Operation::Decrypt), 6);
    assert_eq!(calls(&helper, mark, Operation::CopyH2d), 0);
    assert_eq!(worker(&fixture).cpu_decrypts(), 0);
    drop(allocations);
    assert_idle(&fixture, &helper, default_slots());
}

#[test]
fn gpu_read_tampered_disk_ciphertext_fails_and_drops_the_record() {
    use std::os::unix::fs::FileExt;
    let helper = start(GpuMode::Required, 1).unwrap();
    let data = body(5000);
    let fixture = gpu_fixture(data, &helper);
    arrange(&fixture, &helper, 1, 5000, false, true);
    let location = worker(&fixture).disk_location(&page_id(0)).unwrap();
    let slab = std::fs::OpenOptions::new()
        .read(true)
        .write(true)
        .open(worker(&fixture).slab_path())
        .unwrap();
    let mut record = vec![0; location.extent.length()];
    slab.read_exact_at(&mut record, location.extent.offset())
        .unwrap();
    let decoded = crate::store::parse_bytes(&record, location.extent).unwrap();
    let at = location.extent.offset() + decoded.ciphertext.start as u64 + 10;
    let mut byte = [0];
    slab.read_exact_at(&mut byte, at).unwrap();
    slab.write_all_at(&[byte[0] ^ 1], at).unwrap();
    slab.sync_all().unwrap();
    let (head, frames, allocation, size) = gpu_read(&fixture, &helper, None, 5000);
    assert!(head.starts_with("http/1.1 200"), "{head}");
    assert!(frames.is_empty(), "{frames:?}");
    assert!(
        read_allocation(&allocation, 0, size as usize)
            .iter()
            .all(|&b| b == 0)
    );
    assert_eq!(counter(&helper.counters().rejected), 1);
    // The owner dropped the record, as after a failed CPU decrypt.
    assert!(worker(&fixture).disk_location(&page_id(0)).is_none());
    assert_eq!(worker(&fixture).cpu_decrypts(), 0);
    assert_idle(&fixture, &helper, default_slots());
}

/// Replace the retained ciphertext of page 0 with tampered bytes, keeping
/// the original CRC (`keep_crc`) or the CRC of the tampered bytes.
fn tamper_retained(fixture: &Fixture, keep_crc: bool) {
    use crate::memory::CiphertextBytes;
    use crate::memory::CiphertextPage;
    let memory = worker(fixture).memory();
    let copy = memory.unverified(&page_id(0)).unwrap().unwrap().copy;
    let mut bytes = copy.ciphertext.bytes().to_vec();
    bytes[3] ^= 0x40;
    let crc = if keep_crc {
        copy.ciphertext.checksum()
    } else {
        racer_crypto::crc64(&bytes)
    };
    let reservation = fixture
        .listeners
        .admission
        .reserve(
            Some(&definition().id),
            crate::admission::ResourceClass::Ciphertext,
            bytes.len(),
        )
        .unwrap();
    let tampered = CiphertextPage {
        provenance: None,
        inner: Arc::new(CiphertextBytes {
            checksum: std::sync::OnceLock::from(crc),
            envelope: copy.ciphertext.envelope().clone(),
            storage: flow_control::ChargedBytes { bytes, reservation }.into(),
        }),
    };
    memory.remove_cache(&definition().id).unwrap();
    memory
        .publish_ciphertext(crate::memory::UnverifiedPage {
            copy: crate::memory::CiphertextCopy {
                metadata: copy.metadata,
                ciphertext: tampered,
            },
            disk_token: None,
        })
        .unwrap();
}

#[test]
fn gpu_read_crc_and_tag_mismatches_fail_without_writing() {
    for keep_crc in [true, false] {
        let helper = start(GpuMode::Required, 1).unwrap();
        let fixture = gpu_fixture(body(5000), &helper);
        arrange(&fixture, &helper, 1, 5000, true, false);
        tamper_retained(&fixture, keep_crc);
        let mark = trace_len(&helper);
        let (head, frames, allocation, size) = gpu_read(&fixture, &helper, Some((0, 9)), 10);
        assert!(head.starts_with("http/1.1 200"), "{head}");
        assert!(frames.is_empty(), "{frames:?}");
        assert_eq!(calls(&helper, mark, Operation::Decrypt), 1);
        // A failed verdict copies nothing out of scratch.
        assert_eq!(calls(&helper, mark, Operation::CopyD2d), 0);
        assert!(
            read_allocation(&allocation, 0, size as usize)
                .iter()
                .all(|&b| b == 0)
        );
        assert_eq!(counter(&helper.counters().rejected), 1);
        // The rejected copy left memory.
        assert!(
            worker(&fixture)
                .memory()
                .unverified(&page_id(0))
                .unwrap()
                .is_none()
        );
        assert_idle(&fixture, &helper, default_slots());
    }
}

#[test]
fn gpu_read_without_free_staging_reads_disk_through_host_memory() {
    // Room for the two scratch slots only.
    let staging = (2 * crate::gpu::SLOT_BYTES).next_multiple_of(crate::config::GPU_STAGING_UNIT);
    let helper = start_with(staging, |_| {});
    let data = body(5000);
    let fixture = gpu_fixture(data.clone(), &helper);
    arrange(&fixture, &helper, 1, 5000, false, true);
    let mark = trace_len(&helper);
    let (head, frames, allocation, _) = gpu_read(&fixture, &helper, None, 5000);
    assert!(head.starts_with("http/1.1 200"), "{head}");
    assert_eq!(frames, [(1, 0, 0, 5000), (2, 1, 5000, 0)]);
    assert_eq!(read_allocation(&allocation, 64, 5000), data);
    assert_eq!(calls(&helper, mark, Operation::FileRead), 0);
    assert_eq!(calls(&helper, mark, Operation::Decrypt), 1);
    assert!(counter(&helper.counters().exhausted) >= 1);
    assert_eq!(worker(&fixture).cpu_decrypts(), 0);
    assert_idle(&fixture, &helper, 0);
}

#[test]
fn gpu_read_falls_back_to_host_ciphertext_when_cufile_fails() {
    let helper = start(GpuMode::Required, 1).unwrap();
    let data = body(5000);
    let fixture = gpu_fixture(data.clone(), &helper);
    arrange(&fixture, &helper, 1, 5000, false, true);
    helper.run(|| sim().fault(Operation::FileRead, -5));
    let mark = trace_len(&helper);
    let (head, frames, allocation, _) = gpu_read(&fixture, &helper, None, 5000);
    assert!(head.starts_with("http/1.1 200"), "{head}");
    assert_eq!(frames, [(1, 0, 0, 5000), (2, 1, 5000, 0)]);
    assert_eq!(read_allocation(&allocation, 64, 5000), data);
    assert_eq!(calls(&helper, mark, Operation::FileRead), 1);
    assert_eq!(calls(&helper, mark, Operation::CopyH2d), 1);
    assert_eq!(calls(&helper, mark, Operation::Decrypt), 1);
    assert_eq!(counter(&helper.counters().file_failures), 1);
    assert_eq!(worker(&fixture).metric(Event::GpuDirectDiskRead), 0);
    assert_eq!(worker(&fixture).metric(Event::GpuDirectDiskReadBytes), 0);
    assert_eq!(worker(&fixture).cpu_decrypts(), 0);
    assert_idle(&fixture, &helper, default_slots());
}

#[test]
fn gpu_read_without_decrypt_kernels_decrypts_on_the_cpu() {
    let helper = start_with(crate::config::DEFAULT_GPU_STAGING_BYTES, |simulation| {
        simulation.disable_kernels()
    });
    assert!(!helper.decrypts(0));
    let data = body(5000);
    let fixture = gpu_fixture(data.clone(), &helper);
    arrange(&fixture, &helper, 1, 5000, false, true);
    let mark = trace_len(&helper);
    let plaintext = counter(&helper.counters().plaintext);
    let (head, frames, allocation, _) = gpu_read(&fixture, &helper, None, 5000);
    assert!(head.starts_with("http/1.1 200"), "{head}");
    assert_eq!(frames, [(1, 0, 0, 5000), (2, 1, 5000, 0)]);
    assert_eq!(read_allocation(&allocation, 64, 5000), data);
    assert_eq!(calls(&helper, mark, Operation::Decrypt), 0);
    assert_eq!(calls(&helper, mark, Operation::FileRead), 0);
    assert_eq!(worker(&fixture).cpu_decrypts(), 1);
    assert_eq!(counter(&helper.counters().plaintext), plaintext + 1);
    assert_eq!(counter(&helper.counters().decrypted), 0);
    assert_idle(&fixture, &helper, default_slots());
}

/// A loader on `count` simulated GPUs that counts its calls.
fn counted_loader(count: u8, calls: &Arc<std::sync::atomic::AtomicUsize>) -> crate::gpu::Loader {
    let calls = calls.clone();
    Box::new(move || {
        calls.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        let devices: Vec<SimDevice> = (0..count).map(SimDevice::new).collect();
        let simulation = Simulation::new(devices);
        let environment = simulation.enter();
        SIM.with(|sim| *sim.borrow_mut() = Some((simulation, environment)));
        racer_gpu::Gpu::load()
    })
}

/// The process GPU slot for `mode` in a `cuda` build, as the app makes it.
fn slot(
    mode: GpuMode,
    loader: crate::gpu::Loader,
    telemetry: Arc<crate::gpu_telemetry::GpuTelemetry>,
    links: Option<Arc<crate::rdma::LinkNode>>,
) -> crate::error::Result<Arc<crate::gpu_slot::GpuSlot>> {
    let mut config = crate::test_support::cluster::config(links.is_some());
    config.enable_gpu = mode;
    crate::app::gpu_slot(&config, true, telemetry, links, loader)
}

fn lazy_fixture(body: Vec<u8>, slot: &Arc<crate::gpu_slot::GpuSlot>) -> Fixture {
    let slot = slot.clone();
    direct_fixture_with(body, move |responses| responses.with_lazy_gpu(slot))
}

fn render(telemetry: &crate::gpu_telemetry::GpuTelemetry) -> String {
    let mut out = String::new();
    telemetry.write(&mut out).unwrap();
    out
}

fn calls_now(calls: &std::sync::atomic::AtomicUsize) -> usize {
    calls.load(std::sync::atomic::Ordering::SeqCst)
}

#[test]
/// `auto` touches no GPU at startup, for host reads, or for metrics. The
/// first `gpu-read` starts the helper once, and later reads reuse it.
fn auto_starts_the_gpu_on_the_first_gpu_read_only() {
    let calls = Arc::new(std::sync::atomic::AtomicUsize::new(0));
    let telemetry = Arc::new(crate::gpu_telemetry::GpuTelemetry::default());
    let links = Arc::new(crate::rdma::LinkNode::default());
    let slot = slot(
        GpuMode::Auto,
        counted_loader(1, &calls),
        telemetry.clone(),
        Some(links.clone()),
    )
    .unwrap();
    telemetry.attach_slot(slot.clone());
    assert!(slot.standby());
    assert!(slot.get().is_none());
    let fixture = lazy_fixture(b"hello".to_vec(), &slot);
    // A host read, and a metrics scrape, never start it.
    let memfd = Memfd::new();
    let output = exchange(
        &fixture,
        &direct_request(
            "host-read",
            "Racer-Destination-Offset: 0\r\nRacer-Destination-Length: 5\r\nRacer-Page-Credits: 1\r\n",
        ),
        &[memfd.file.as_raw_fd()],
    );
    assert!(split(&output).0.starts_with("http/1.1 200"));
    settle(&fixture);
    let text = render(&telemetry);
    assert!(text.contains("racer_gpu_standby 1\n"), "{text}");
    assert!(text.contains("racer_gpu_enabled 0\n"), "{text}");
    assert_eq!(calls_now(&calls), 0);
    assert!(!links.gpu_landing_configured());
    // The first gpu-read starts it. A host buffer is not a GPU allocation,
    // so the import rejects this one as invalid.
    let output = exchange(
        &fixture,
        &direct_request("gpu-read", &fields("", 0, 5, GRANULE)),
        &[memfd.file.as_raw_fd()],
    );
    let (head, _) = split(&output);
    assert!(head.starts_with("http/1.1 400"), "{head}");
    settle(&fixture);
    assert_eq!(calls_now(&calls), 1);
    assert!(!slot.standby());
    let helper = slot.get().unwrap().clone();
    let text = render(&telemetry);
    assert!(text.contains("racer_gpu_standby 0\n"), "{text}");
    assert!(text.contains("racer_gpu_enabled 1\n"), "{text}");
    // RDMA landing was configured for the rails, and marked pending.
    assert!(links.gpu_landing_configured());
    assert!(links.take_landing_pending());
    use crate::gpu_telemetry::GpuReadOutcome;
    let gpu = fixture.listeners.metrics.gpu();
    assert_eq!(gpu.requests(GpuReadOutcome::Invalid), 1);
    // Later reads use the started helper.
    let allocation = allocate(&helper, 0, GRANULE);
    let output = exchange(
        &fixture,
        &direct_request("gpu-read", &fields("", 0, 5, GRANULE)),
        &[allocation.as_raw_fd()],
    );
    let (head, _) = split(&output);
    assert!(head.starts_with("http/1.1 200"), "{head}");
    assert_eq!(read_allocation(&allocation, 0, 5), b"hello");
    settle(&fixture);
    assert_released(&helper);
    assert_eq!(calls_now(&calls), 1);
    assert_eq!(gpu.requests(GpuReadOutcome::Gpu), 1);
}

#[test]
/// Concurrent first `gpu-read` requests share one start.
fn concurrent_first_gpu_reads_start_the_gpu_once() {
    let calls = Arc::new(std::sync::atomic::AtomicUsize::new(0));
    let slot = slot(
        GpuMode::Auto,
        counted_loader(1, &calls),
        Default::default(),
        None,
    )
    .unwrap();
    let fixture = lazy_fixture(b"hello".to_vec(), &slot);
    let memfd = Memfd::new();
    let request = direct_request("gpu-read", &fields("", 0, 5, GRANULE));
    let mut sockets: Vec<_> = (0..4)
        .map(|_| {
            let socket = fixture.connect();
            send_with_fds(&socket, &request, &[memfd.file.as_raw_fd()]);
            socket
        })
        .collect();
    for socket in &mut sockets {
        let output = fixture.receive(socket, true);
        let (head, _) = split(&output);
        // A host buffer is not a GPU allocation; the helper did start.
        assert!(head.starts_with("http/1.1 400"), "{head}");
    }
    drop(sockets);
    settle(&fixture);
    assert_eq!(calls_now(&calls), 1);
    assert!(slot.get().is_some());
}

#[test]
/// A failed start or a node with no usable GPU leaves `gpu-read` off (501)
/// for good, so clients use host reads, and it is never retried.
fn auto_start_failure_is_501_and_not_retried() {
    let failing = Arc::new(std::sync::atomic::AtomicUsize::new(0));
    let counted = failing.clone();
    let failed: crate::gpu::Loader = Box::new(move || {
        counted.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        Err(racer_gpu::Error::Unavailable)
    });
    let empty = Arc::new(std::sync::atomic::AtomicUsize::new(0));
    for (loader, calls) in [(failed, &failing), (counted_loader(0, &empty), &empty)] {
        let telemetry = Arc::new(crate::gpu_telemetry::GpuTelemetry::default());
        let links = Arc::new(crate::rdma::LinkNode::default());
        let slot = slot(
            GpuMode::Auto,
            loader,
            telemetry.clone(),
            Some(links.clone()),
        )
        .unwrap();
        telemetry.attach_slot(slot.clone());
        let fixture = lazy_fixture(b"hello".to_vec(), &slot);
        let memfd = Memfd::new();
        for _ in 0..3 {
            let output = exchange(
                &fixture,
                &direct_request("gpu-read", &fields("", 0, 5, GRANULE)),
                &[memfd.file.as_raw_fd()],
            );
            let (head, _) = split(&output);
            assert!(head.starts_with("http/1.1 501"), "{head}");
            settle(&fixture);
        }
        assert_eq!(calls_now(calls), 1);
        assert!(!slot.standby());
        assert!(slot.get().is_none());
        assert!(!links.gpu_landing_configured());
        use crate::gpu_telemetry::GpuReadOutcome;
        let gpu = fixture.listeners.metrics.gpu();
        assert_eq!(gpu.requests(GpuReadOutcome::NotEnabled), 3);
        let text = render(&telemetry);
        assert!(text.contains("racer_gpu_standby 0\n"), "{text}");
        assert!(text.contains("racer_gpu_enabled 0\n"), "{text}");
    }
}

#[test]
/// `true` starts eagerly and fails startup without a usable GPU; `false`
/// and an `auto` build without `cuda` never load a driver.
fn required_starts_eagerly_and_off_modes_never_load() {
    let calls = Arc::new(std::sync::atomic::AtomicUsize::new(0));
    let links = Arc::new(crate::rdma::LinkNode::default());
    let telemetry = Arc::new(crate::gpu_telemetry::GpuTelemetry::default());
    let required = slot(
        GpuMode::Required,
        counted_loader(1, &calls),
        telemetry.clone(),
        Some(links.clone()),
    )
    .unwrap();
    assert_eq!(calls_now(&calls), 1);
    assert!(!required.standby());
    assert!(required.get().is_some());
    assert!(links.gpu_landing_configured());
    assert!(render(&telemetry).contains("racer_gpu_enabled 1\n"));
    {
        let count = 0;
        let none = Arc::new(std::sync::atomic::AtomicUsize::new(0));
        assert!(
            slot(
                GpuMode::Required,
                counted_loader(count, &none),
                Default::default(),
                None
            )
            .is_err()
        );
        assert_eq!(calls_now(&none), 1);
    }
    assert!(
        slot(
            GpuMode::Required,
            Box::new(|| Err(racer_gpu::Error::Unavailable)),
            Default::default(),
            None
        )
        .is_err()
    );
    let off = Arc::new(std::sync::atomic::AtomicUsize::new(0));
    let disabled = slot(
        GpuMode::Disabled,
        counted_loader(1, &off),
        Default::default(),
        None,
    )
    .unwrap();
    assert!(!disabled.standby());
    assert!(disabled.get().is_none());
    let mut config = crate::test_support::cluster::config(false);
    config.enable_gpu = GpuMode::Auto;
    let unbuilt = crate::app::gpu_slot(
        &config,
        false,
        Default::default(),
        None,
        counted_loader(1, &off),
    )
    .unwrap();
    assert!(!unbuilt.standby());
    assert!(futures::executor::block_on(unbuilt.get_or_start()).is_none());
    assert_eq!(calls_now(&off), 0);
}

/// A loader like [`counted_loader`] on one GPU that first waits for `gate`,
/// standing in for a CUDA initialization that stalls.
fn gated_loader(
    calls: &Arc<std::sync::atomic::AtomicUsize>,
    gate: std::sync::mpsc::Receiver<()>,
) -> crate::gpu::Loader {
    let load = counted_loader(1, calls);
    Box::new(move || {
        let _ = gate.recv();
        load()
    })
}

#[test]
/// A `gpu-read` that expires or is cancelled while the GPU start stalls
/// stops waiting and releases its connection; the shared start keeps going
/// and a later `gpu-read` uses the helper it started.
fn stalled_gpu_start_honors_request_expiry_and_cancellation() {
    for mode in ["expire", "cancel"] {
        let calls = Arc::new(std::sync::atomic::AtomicUsize::new(0));
        let (open, gate) = std::sync::mpsc::channel();
        let slot = slot(
            GpuMode::Auto,
            gated_loader(&calls, gate),
            Default::default(),
            None,
        )
        .unwrap();
        let mut fixture = lazy_fixture(b"hello".to_vec(), &slot);
        let timeout = Duration::from_millis(200);
        if mode == "expire" {
            fixture.listeners = fixture.listeners.with_request_timeout(timeout);
        }
        let memfd = Memfd::new();
        let mut socket = fixture.connect();
        send_with_fds(
            &socket,
            &direct_request("gpu-read", &fields("", 0, 5, GRANULE)),
            &[memfd.file.as_raw_fd()],
        );
        let started = Instant::now();
        for _ in 0..16 {
            fixture.pump(16);
        }
        // The start is pending, so the request is still waiting on it.
        assert!(slot.standby(), "{mode}");
        assert_eq!(fixture.listeners.active_connections(), 1, "{mode}");
        if mode == "expire" {
            std::thread::sleep(
                (started + timeout + Duration::from_millis(20))
                    .saturating_duration_since(Instant::now()),
            );
        } else {
            fixture.listeners.cancel_cache(&definition().id).unwrap();
        }
        let output = fixture.receive(&mut socket, true);
        let (head, _) = split(&output);
        assert!(head.starts_with("http/1.1 503"), "{mode}: {head}");
        settle(&fixture);
        // Giving up did not cancel the shared start.
        assert!(slot.standby(), "{mode}");
        open.send(()).unwrap();
        let deadline = Instant::now() + Duration::from_secs(5);
        while slot.standby() {
            assert!(Instant::now() < deadline, "{mode}: start did not finish");
            std::thread::yield_now();
        }
        assert_eq!(calls_now(&calls), 1, "{mode}");
        let helper = slot.get().unwrap().clone();
        if mode == "expire" {
            // A later request gets the helper the stalled start produced.
            fixture.listeners = fixture
                .listeners
                .with_request_timeout(Duration::from_secs(30));
            let allocation = allocate(&helper, 0, GRANULE);
            let output = exchange(
                &fixture,
                &direct_request("gpu-read", &fields("", 0, 5, GRANULE)),
                &[allocation.as_raw_fd()],
            );
            let (head, _) = split(&output);
            assert!(head.starts_with("http/1.1 200"), "{head}");
            assert_eq!(read_allocation(&allocation, 0, 5), b"hello");
            settle(&fixture);
            assert_released(&helper);
            assert_eq!(calls_now(&calls), 1);
        }
    }
}
