//! `gpu-read` end to end over the client socket, on simulated GPUs.
use super::*;
use crate::config::GpuMode;
use crate::gpu::GpuClient;
use crate::gpu::GpuHelper;
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
            GpuHelper::start(GpuMode::Auto, crate::config::DEFAULT_GPU_STAGING_BYTES)
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

/// Read `range` of the object into a fresh allocation at offset 64.
fn gpu_read(
    fixture: &Fixture,
    helper: &GpuHelper,
    range: Option<(u64, u64)>,
    length: u64,
) -> (String, Vec<(u8, u64, u64, u32)>, OwnedFd, u64) {
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
        let result = worker.memory().get(&page_id(number)).unwrap().unwrap();
        copies.push(result.copy());
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

#[test]
fn gpu_read_memory_plaintext_hit_copies_plaintext() {
    let helper = start(GpuMode::Required, 1).unwrap();
    let data = body(5000);
    let fixture = gpu_fixture(data.clone(), &helper);
    worker(&fixture).retain(true);
    let _ = gpu_read(&fixture, &helper, None, 5000);
    let mark = trace_len(&helper);
    let decrypts = worker(&fixture).cpu_decrypts();
    let (head, frames, allocation, _) = gpu_read(&fixture, &helper, Some((10, 99)), 90);
    assert!(head.starts_with("http/1.1 200"), "{head}");
    assert_eq!(frames, [(1, 0, 10, 90), (2, 1, 90, 0)]);
    assert_eq!(read_allocation(&allocation, 64, 90), &data[10..100]);
    assert_eq!(calls(&helper, mark, Operation::Decrypt), 0);
    assert_eq!(calls(&helper, mark, Operation::CopyH2d), 1);
    assert_eq!(worker(&fixture).cpu_decrypts(), decrypts);
    assert_eq!(counter(&helper.counters().decrypted), 0);
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
    let (head, frames, allocation, _) = gpu_read(&fixture, &helper, None, 5000);
    assert!(head.starts_with("http/1.1 200"), "{head}");
    assert_eq!(frames, [(1, 0, 0, 5000), (2, 1, 5000, 0)]);
    assert_eq!(read_allocation(&allocation, 64, 5000), data);
    assert_eq!(calls(&helper, mark, Operation::Decrypt), 0);
    assert_eq!(calls(&helper, mark, Operation::FileRead), 0);
    assert_eq!(worker(&fixture).cpu_decrypts(), 1);
    assert_idle(&fixture, &helper, default_slots());
}
