//! Safe API behavior on the simulation backend: discovery, import windows,
//! decrypt verdicts, lifetimes, fault injection and leak counting.
#![cfg(feature = "simulation")]

use racer_gpu::{
    DecryptRequest, Destination, Error, Gpu, Verdict,
    simulation::{GRANULARITY, Operation, SimDevice, Simulation, read_allocation},
};
use std::os::fd::OwnedFd;

/// Two usable GPUs and one in EXCLUSIVE_PROCESS mode.
fn simulation() -> Simulation {
    let mut exclusive = SimDevice::new(2);
    exclusive.compute_mode = 3;
    Simulation::new(vec![SimDevice::new(0), SimDevice::new(1), exclusive])
}

/// A memfd with arbitrary bytes, standing in for an O_DIRECT file.
fn file_with(bytes: &[u8]) -> OwnedFd {
    use std::os::unix::fs::FileExt;
    let fd = unsafe { libc::memfd_create(c"file".as_ptr(), libc::MFD_CLOEXEC) };
    assert!(fd >= 0);
    let file = unsafe { <std::fs::File as std::os::fd::FromRawFd>::from_raw_fd(fd) };
    file.write_all_at(bytes, 0).unwrap();
    file.into()
}

/// A sealed page and what the GPU needs to open it.
struct Page {
    derived: racer_crypto::DerivedPageKey,
    aad: Vec<u8>,
    plaintext: Vec<u8>,
    sealed: Vec<u8>,
}

/// Seal `length` bytes with XAES-256-GCM.
fn page(length: usize) -> Page {
    let key = [9; 32];
    let nonce = *b"0123456789abcdefghijklmn";
    let aad = b"racer/page/aead/v1\0test".to_vec();
    let plaintext: Vec<u8> = (0..length).map(|i| (i * 7 + 3) as u8).collect();
    let mut sealed = vec![0; length + 16];
    racer_crypto::seal(&key, &nonce, &aad, &plaintext, &mut sealed).unwrap();
    Page {
        derived: racer_crypto::xaes_derive(&key, &nonce),
        aad,
        plaintext,
        sealed,
    }
}

impl Page {
    /// The GPU request.
    fn request(&self, expected_crc: Option<u64>) -> DecryptRequest<'_> {
        DecryptRequest {
            key: self.derived.key(),
            nonce: self.derived.nonce(),
            aad: &self.aad,
            length: self.plaintext.len() as u64,
            expected_crc,
        }
    }
}

#[test]
/// Discovery reports every GPU; only default-mode GPUs are usable.
fn discovery_reports_all_and_filters_unusable() {
    let sim = simulation();
    let _env = sim.enter();
    let gpu = Gpu::load().unwrap();
    let devices = gpu.devices().unwrap();
    assert_eq!(devices.len(), 3);
    assert_eq!(devices[1].pci_bus_id, "0000:11:00.0");
    assert_eq!(devices[1].numa_node, Some(1));
    let usable: Vec<_> = gpu
        .usable_devices()
        .unwrap()
        .iter()
        .map(|d| d.ordinal)
        .collect();
    assert_eq!(usable, [0, 1]);
}

#[test]
/// Init and context failures surface the driver status.
fn faults_surface_driver_errors() {
    let sim = simulation();
    let _env = sim.enter();
    sim.fault(Operation::Init, -3);
    assert_eq!(Gpu::load().err(), Some(Error::Unavailable));
    let gpu = Gpu::load().unwrap();
    sim.fault(Operation::Context, -5);
    assert_eq!(gpu.context(0).err(), Some(Error::Driver(999)));
    assert_eq!(gpu.context(7).err(), Some(Error::Driver(101)));
    assert_eq!(sim.live().contexts, 0);
}

#[test]
/// Writes are limited to the request window and the import's GPU.
fn import_rejects_out_of_window_and_wrong_device() {
    let sim = simulation();
    let _env = sim.enter();
    let gpu = Gpu::load().unwrap();
    let ctx = gpu.context(0).unwrap();
    let other = gpu.context(1).unwrap();
    let size = 2 * GRANULARITY;
    let client = sim.allocate(0, size);

    // Bad windows, granularity, and devices fail before or at import.
    let dup = || client.try_clone().unwrap();
    assert_eq!(
        ctx.import(dup(), size, 10..10).err(),
        Some(Error::InvalidRange)
    );
    assert_eq!(
        ctx.import(dup(), size, 0..size + 1).err(),
        Some(Error::InvalidRange)
    );
    assert_eq!(
        ctx.import(dup(), size - 1, 0..1).err(),
        Some(Error::InvalidRequest)
    );
    assert_eq!(
        ctx.import(dup(), 4 * GRANULARITY, 0..1).err(),
        Some(Error::Driver(1))
    );
    assert_eq!(
        other.import(dup(), size, 0..1).err(),
        Some(Error::WrongDevice)
    );
    let stranger = file_with(&[0; 16]);
    assert_eq!(
        ctx.import(stranger, size, 0..1).err(),
        Some(Error::Driver(1))
    );

    let import = ctx.import(dup(), size, 100..200).unwrap();
    assert_eq!(import.window(), 100..200);
    let stream = ctx.stream().unwrap();
    let foreign = other.stream().unwrap();
    let dst = Destination::Import(&import);
    stream.copy_from_host(dst, 100, [7; 100]).unwrap();
    assert_eq!(
        stream.copy_from_host(dst, 99, [1; 2]),
        Err(Error::InvalidRange)
    );
    assert_eq!(
        stream.copy_from_host(dst, 150, [1; 51]),
        Err(Error::InvalidRange)
    );
    assert_eq!(
        stream.copy_from_host(dst, u64::MAX, [1]),
        Err(Error::InvalidRange)
    );
    assert_eq!(
        stream.copy_from_host(dst, 100, []),
        Err(Error::InvalidRange)
    );
    assert_eq!(
        foreign.copy_from_host(dst, 100, [1]),
        Err(Error::WrongDevice)
    );
    stream.synchronize().unwrap();
    let bytes = read_allocation(&client, 98, 104);
    assert_eq!(&bytes[..2], &[0, 0]);
    assert!(bytes[2..102].iter().all(|&b| b == 7));
    assert_eq!(&bytes[102..], &[0, 0]);
}

#[test]
/// Ciphertext staged on the GPU decrypts into the client window; tampered
/// pages and wrong CRCs leave it untouched.
fn decrypt_into_import_verifies_before_writing() {
    let sim = simulation();
    let _env = sim.enter();
    let gpu = Gpu::load().unwrap();
    let ctx = gpu.context(1).unwrap();
    let client = sim.allocate(1, GRANULARITY);
    let import = ctx
        .import(client.try_clone().unwrap(), GRANULARITY, 64..64 + 5000)
        .unwrap();
    let staging = ctx.staging(8192).unwrap();
    let stream = ctx.stream().unwrap();
    let page = page(5000);
    let crc = racer_crypto::crc64(&page.sealed);
    let dst = Destination::Import(&import);

    assert_eq!(stream.verdict(), Err(Error::Busy));
    stream
        .copy_from_host(Destination::Staging(&staging), 16, page.sealed.clone())
        .unwrap();
    stream
        .decrypt(&page.request(Some(crc)), &staging, 16, dst, 64)
        .unwrap();
    // One decrypt per synchronize.
    assert_eq!(
        stream.decrypt(&page.request(None), &staging, 16, dst, 64),
        Err(Error::Busy)
    );
    stream.synchronize().unwrap();
    assert_eq!(stream.verdict(), Ok(Verdict::Passed { crc }));
    assert_eq!(stream.verdict(), Err(Error::Busy));
    assert_eq!(read_allocation(&client, 64, 5000), page.plaintext);

    // Fresh destination for the failure cases.
    let client2 = sim.allocate(1, GRANULARITY);
    let import2 = ctx
        .import(client2.try_clone().unwrap(), GRANULARITY, 0..5000)
        .unwrap();
    let dst2 = Destination::Import(&import2);
    stream
        .decrypt(&page.request(Some(crc ^ 1)), &staging, 16, dst2, 0)
        .unwrap();
    stream.synchronize().unwrap();
    assert_eq!(stream.verdict(), Ok(Verdict::CrcMismatch { crc }));

    let mut tampered = page.sealed.clone();
    tampered[4999] ^= 1;
    stream
        .copy_from_host(Destination::Staging(&staging), 16, tampered)
        .unwrap();
    stream
        .decrypt(&page.request(None), &staging, 16, dst2, 0)
        .unwrap();
    stream.synchronize().unwrap();
    assert!(matches!(stream.verdict(), Ok(Verdict::TagMismatch { .. })));
    assert!(read_allocation(&client2, 0, 5000).iter().all(|&b| b == 0));

    // Out-of-window destination and out-of-range source are refused.
    assert_eq!(
        stream.decrypt(&page.request(None), &staging, 16, dst2, 1),
        Err(Error::InvalidRange)
    );
    assert_eq!(
        stream.decrypt(&page.request(None), &staging, 8192 - 5000, dst2, 0),
        Err(Error::InvalidRange)
    );
    let mut big = page.request(None);
    big.length = racer_gpu::MAX_DECRYPT + 1;
    assert_eq!(
        stream.decrypt(&big, &staging, 0, dst2, 0),
        Err(Error::InvalidRequest)
    );
    let long_aad = vec![0; racer_gpu::MAX_AAD + 1];
    let mut long = page.request(None);
    long.aad = &long_aad;
    assert_eq!(
        stream.decrypt(&long, &staging, 0, dst2, 0),
        Err(Error::InvalidRequest)
    );
}

#[test]
/// A shim without kernels reports it instead of decrypting.
fn decrypt_without_kernels_is_unavailable() {
    let sim = simulation();
    sim.disable_kernels();
    let _env = sim.enter();
    let gpu = Gpu::load().unwrap();
    let ctx = gpu.context(0).unwrap();
    let staging = ctx.staging(64).unwrap();
    let out = ctx.staging(64).unwrap();
    let stream = ctx.stream().unwrap();
    let page = page(16);
    assert_eq!(
        stream.decrypt(
            &page.request(None),
            &staging,
            0,
            Destination::Staging(&out),
            0
        ),
        Err(Error::KernelUnavailable)
    );
}

#[test]
/// Dropping an import while work is queued defers the unmap to after the
/// stream synchronizes.
fn import_is_released_only_after_stream_sync() {
    let sim = simulation();
    let _env = sim.enter();
    let gpu = Gpu::load().unwrap();
    let ctx = gpu.context(0).unwrap();
    let client = sim.allocate(0, GRANULARITY);
    let import = ctx.import(client, GRANULARITY, 0..10).unwrap();
    let stream = ctx.stream().unwrap();
    stream
        .copy_from_host(Destination::Import(&import), 0, *b"0123456789")
        .unwrap();
    drop(import);
    assert_eq!(sim.live().imports, 1);
    stream.synchronize().unwrap();
    assert_eq!(sim.live().imports, 0);
    let order: Vec<_> = sim
        .trace()
        .iter()
        .map(|e| e.operation)
        .filter(|op| matches!(op, Operation::StreamSync | Operation::ImportFree))
        .collect();
    assert_eq!(order, [Operation::StreamSync, Operation::ImportFree]);

    // Dropping the stream itself synchronizes before releasing holds.
    let client = sim.allocate(0, GRANULARITY);
    let import = ctx.import(client, GRANULARITY, 0..10).unwrap();
    stream
        .copy_from_host(Destination::Import(&import), 0, *b"x")
        .unwrap();
    drop(import);
    drop(stream);
    assert_eq!(sim.live().imports, 0);
    drop(ctx);
    assert_eq!(sim.live().contexts, 0);
    assert_eq!(gpu.leaked(), 0);
}

#[test]
/// Failed frees leak the resource and count it; nothing is freed early.
fn failed_frees_leak_and_count() {
    let sim = simulation();
    let _env = sim.enter();
    let gpu = Gpu::load().unwrap();
    let ctx = gpu.context(0).unwrap();
    let import = ctx
        .import(sim.allocate(0, GRANULARITY), GRANULARITY, 0..1)
        .unwrap();
    sim.fault(Operation::ImportFree, -5);
    drop(import);
    assert_eq!(gpu.leaked(), 1);
    assert_eq!(sim.live().imports, 1);

    // A stream that cannot be freed keeps everything it touched.
    let stream = ctx.stream().unwrap();
    let staging = ctx.staging(16).unwrap();
    stream
        .copy_from_host(Destination::Staging(&staging), 0, [1; 16])
        .unwrap();
    assert!(staging.in_flight());
    sim.fault(Operation::StreamFree, -5);
    drop(stream);
    drop(staging);
    assert_eq!(gpu.leaked(), 2);
    assert_eq!(sim.live().streams, 1);
    assert_eq!(sim.live().staging, 1);

    // A failed synchronize keeps holds; the buffer cannot be freed yet.
    let stream = ctx.stream().unwrap();
    let staging = ctx.staging(16).unwrap();
    stream
        .copy_from_host(Destination::Staging(&staging), 0, [1; 16])
        .unwrap();
    sim.fault(Operation::StreamSync, -5);
    assert_eq!(stream.synchronize(), Err(Error::Driver(999)));
    assert!(staging.in_flight());
    stream.synchronize().unwrap();
    assert!(!staging.in_flight());

    // Context leaks when release fails.
    let leaked_ctx = gpu.context(1).unwrap();
    sim.fault(Operation::ContextFree, -5);
    drop(leaked_ctx);
    assert_eq!(gpu.leaked(), 3);
}

#[test]
/// A file cuFile does not support fails with its cuFile status, apart from
/// driver failures.
fn unsupported_file_is_its_own_error() {
    let sim = simulation();
    let _env = sim.enter();
    let gpu = Gpu::load().unwrap();
    sim.fault(Operation::File, -11);
    assert_eq!(
        gpu.open_file(file_with(&[1; 16])).err(),
        Some(Error::Unsupported(5008))
    );
    sim.fault(Operation::File, -5);
    assert_eq!(
        gpu.open_file(file_with(&[1; 16])).err(),
        Some(Error::Driver(999))
    );
    assert!(gpu.open_file(file_with(&[1; 16])).is_ok());
}

#[test]
/// cuFile reads land in staging, respect bounds, and wait for streams.
fn cufile_reads_into_staging() {
    let sim = simulation();
    let _env = sim.enter();
    let gpu = Gpu::load().unwrap();
    let ctx = gpu.context(0).unwrap();
    let staging = ctx.staging(8192).unwrap();
    staging.register_cufile().unwrap();
    let data: Vec<u8> = (0..10000u32).map(|i| i as u8).collect();
    let file = gpu.open_file(file_with(&data)).unwrap();
    file.read(&staging, 100, 4096, 1000).unwrap();
    let stream = ctx.stream().unwrap();
    let mut out = vec![0; 1000];
    stream.read_staging(&staging, 100, &mut out).unwrap();
    assert_eq!(out, &data[4096..5096]);
    assert_eq!(file.read(&staging, 8000, 0, 200), Err(Error::InvalidRange));
    assert_eq!(file.read(&staging, 0, 0, 0), Err(Error::InvalidRange));
    assert_eq!(file.read(&staging, 0, 9990, 100), Err(Error::Io));

    // A queued copy holds the buffer.
    stream
        .copy_from_host(Destination::Staging(&staging), 0, [1])
        .unwrap();
    assert_eq!(file.read(&staging, 0, 0, 1), Err(Error::Busy));
    stream.synchronize().unwrap();
    file.read(&staging, 0, 0, 1).unwrap();
    drop(file);
    drop(staging);
    assert_eq!(sim.live().files, 0);
    assert_eq!(sim.live().staging, 0);
    assert_eq!(gpu.leaked(), 0);
}

#[test]
/// Detached cuFile reads run on other threads, while a stream uses the same
/// staging buffer, and report errors like a direct read.
fn detached_cufile_reads_run_on_other_threads() {
    let sim = simulation();
    let _env = sim.enter();
    let gpu = Gpu::load().unwrap();
    let ctx = gpu.context(0).unwrap();
    let staging = ctx.staging(8192).unwrap();
    staging.register_cufile().unwrap();
    let data: Vec<u8> = (0..10000u32).map(|i| (i * 7) as u8).collect();
    let file = gpu.open_file(file_with(&data)).unwrap();
    let reader = gpu.file_reader();

    // A queued copy into the first page does not block a read of the second.
    let stream = ctx.stream().unwrap();
    stream
        .copy_from_host(Destination::Staging(&staging), 0, [1])
        .unwrap();
    let reads = [(4096, 0, 1000), (6000, 5000, 2000)].map(|(offset, file_offset, length)| {
        unsafe { file.detach(&staging, offset, file_offset, length) }.unwrap()
    });
    let threads = reads.map(|read| {
        let reader = reader.clone();
        std::thread::spawn(move || reader.read(read))
    });
    for thread in threads {
        thread.join().unwrap().unwrap();
    }
    stream.synchronize().unwrap();
    let mut out = vec![0; 1000];
    stream.read_staging(&staging, 4096, &mut out).unwrap();
    assert_eq!(out, &data[..1000]);
    let mut out = vec![0; 2000];
    stream.read_staging(&staging, 6000, &mut out).unwrap();
    assert_eq!(out, &data[5000..7000]);

    // Bounds are checked up front; I/O and driver errors come from the read.
    assert_eq!(
        unsafe { file.detach(&staging, 8000, 0, 200) }.err(),
        Some(Error::InvalidRange)
    );
    assert_eq!(
        unsafe { file.detach(&staging, 0, 0, 0) }.err(),
        Some(Error::InvalidRange)
    );
    let short = unsafe { file.detach(&staging, 0, 9990, 100) }.unwrap();
    let thread = std::thread::spawn({
        let reader = reader.clone();
        move || reader.read(short)
    });
    assert_eq!(thread.join().unwrap(), Err(Error::Io));
    sim.fault(Operation::FileRead, -5);
    let faulted = unsafe { file.detach(&staging, 0, 0, 100) }.unwrap();
    let thread = std::thread::spawn(move || reader.read(faulted));
    assert!(matches!(thread.join().unwrap(), Err(Error::Driver(_))));
    drop(file);
    drop(staging);
    assert_eq!(sim.live().files, 0);
    assert_eq!(gpu.leaked(), 0);
}

#[test]
/// dma-buf export checks page alignment and bounds.
fn dma_buf_export_checks_alignment() {
    let sim = simulation();
    let _env = sim.enter();
    let gpu = Gpu::load().unwrap();
    let ctx = gpu.context(0).unwrap();
    let staging = ctx.staging(16384).unwrap();
    staging.dma_buf(4096, 8192).unwrap();
    assert_eq!(staging.dma_buf(1, 4096).err(), Some(Error::InvalidRange));
    assert_eq!(staging.dma_buf(0, 100).err(), Some(Error::InvalidRange));
    assert_eq!(
        staging.dma_buf(8192, 16384).err(),
        Some(Error::InvalidRange)
    );
}

#[test]
/// A whole-buffer export is a live view: writes through the dma-buf (as a
/// NIC would make) are what the GPU then reads, and the other way around.
fn dma_buf_export_from_zero_is_live() {
    use std::os::unix::fs::FileExt;
    let sim = simulation();
    let _env = sim.enter();
    let gpu = Gpu::load().unwrap();
    let ctx = gpu.context(0).unwrap();
    let staging = ctx.staging(16384).unwrap();
    let file = std::fs::File::from(staging.dma_buf(0, 16384).unwrap());
    file.write_all_at(b"from the nic", 8192).unwrap();
    let stream = ctx.stream().unwrap();
    let mut out = [0; 12];
    stream.read_staging(&staging, 8192, &mut out).unwrap();
    stream.synchronize().unwrap();
    assert_eq!(&out, b"from the nic");
    stream
        .copy_from_host(Destination::Staging(&staging), 4096, *b"gpu")
        .unwrap();
    stream.synchronize().unwrap();
    let mut back = [0; 3];
    file.read_exact_at(&mut back, 4096).unwrap();
    assert_eq!(&back, b"gpu");
    drop((stream, staging, file));
    assert_eq!(sim.live().staging, 0);
}

#[test]
/// Device copies move staged bytes into the window; reads of staging work.
fn device_copy_from_staging() {
    let sim = simulation();
    let _env = sim.enter();
    let gpu = Gpu::load().unwrap();
    let ctx = gpu.context(0).unwrap();
    let client = sim.allocate(0, GRANULARITY);
    let import = ctx
        .import(client.try_clone().unwrap(), GRANULARITY, 10..20)
        .unwrap();
    let staging = ctx.staging(32).unwrap();
    let stream = ctx.stream().unwrap();
    stream
        .copy_from_host(Destination::Staging(&staging), 0, *b"abcdefghij")
        .unwrap();
    stream
        .copy_from_staging(Destination::Import(&import), 10, &staging, 0, 10)
        .unwrap();
    assert_eq!(
        stream.copy_from_staging(Destination::Import(&import), 10, &staging, 0, 11),
        Err(Error::InvalidRange)
    );
    assert_eq!(
        stream.copy_from_staging(Destination::Import(&import), 10, &staging, 30, 5),
        Err(Error::InvalidRange)
    );
    stream.synchronize().unwrap();
    assert_eq!(read_allocation(&client, 10, 10), b"abcdefghij");
}

#[test]
fn host_source_is_held_until_stream_sync() {
    let sim = simulation();
    let _env = sim.enter();
    let gpu = Gpu::load().unwrap();
    let ctx = gpu.context(0).unwrap();
    let client = sim.allocate(0, GRANULARITY);
    let import = ctx
        .import(client.try_clone().unwrap(), GRANULARITY, 0..10)
        .unwrap();
    let stream = ctx.stream().unwrap();
    let source = std::sync::Arc::<[u8]>::from(&b"0123456789"[..]);
    let weak = std::sync::Arc::downgrade(&source);
    stream
        .copy_from_host(Destination::Import(&import), 0, source.clone())
        .unwrap();
    // The caller drops its handle early; the stream still owns the bytes.
    drop(source);
    assert_eq!(weak.strong_count(), 1);
    assert_eq!(stream.retained_host_sources(), 1);
    stream.synchronize().unwrap();
    assert_eq!(weak.strong_count(), 0);
    assert_eq!(stream.retained_host_sources(), 0);
    assert_eq!(read_allocation(&client, 0, 10), b"0123456789");

    // A rejected copy does not keep the source.
    let source = std::sync::Arc::<[u8]>::from(vec![1u8; 11]);
    let weak = std::sync::Arc::downgrade(&source);
    assert_eq!(
        stream.copy_from_host(Destination::Import(&import), 0, source),
        Err(Error::InvalidRange)
    );
    assert_eq!(weak.strong_count(), 0);
    assert_eq!(stream.retained_host_sources(), 0);
}
