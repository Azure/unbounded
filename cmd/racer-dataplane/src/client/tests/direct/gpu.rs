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
