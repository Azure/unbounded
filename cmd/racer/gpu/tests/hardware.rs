//! Tests on a real GPU through `libracer_gpu_cuda.so.1`.
//!
//! Opt in with `RACER_GPU_TESTS=1`; `make racer-gpu-test` builds the shim
//! with its kernels and runs these. Without the variable every test returns
//! early, so the normal CI build compiles them and they pass as no-ops. With
//! `RACER_GPU_TESTS=1` a missing shim, driver or GPU is reported as a skip;
//! with `RACER_GPU_TESTS=require` it fails instead, for dedicated GPU runners.
//! cuFile tests skip when cuFile cannot open unless `RACER_GPU_TESTS_CUFILE`
//! is `require`. cuFile opens once per process, with PCI P2PDMA when
//! `RACER_GPU_PCI_P2PDMA` is `true`; one test reruns the cuFile tests in a
//! child process with it on. dma-buf and two-GPU tests skip without driver
//! support or a second GPU unless `RACER_GPU_TESTS_DMABUF` or
//! `RACER_GPU_TESTS_MULTI` is `require`.
#![cfg(all(feature = "native", target_os = "linux"))]

use racer_gpu::{
    ComputeMode, Context, DecryptRequest, Destination, Error, Gpu, RdmaScope, StagingBuffer,
    Stream, Verdict, cufile_status_name, flag,
};
use std::{
    ffi::{CStr, c_int, c_uint, c_void},
    fs::OpenOptions,
    io::Write,
    os::{
        fd::{AsRawFd, FromRawFd, OwnedFd},
        unix::fs::OpenOptionsExt,
    },
    sync::{Arc, OnceLock, mpsc},
    time::Duration,
};

/// Makes cuFile tests fail instead of skip when cuFile cannot open.
const CUFILE: &str = "RACER_GPU_TESTS_CUFILE";

/// Whether hardware is mandatory for this run.
fn required() -> bool {
    std::env::var("RACER_GPU_TESTS").as_deref() == Ok("require")
}

/// Skip, or fail when hardware is required.
fn skip(test: &str, why: impl std::fmt::Display) {
    assert!(!required(), "{test}: RACER_GPU_TESTS=require but {why}");
    eprintln!("SKIP {test}: {why}");
}

/// Skip a test whose optional driver feature or hardware is missing, or
/// fail when `variable` is `require`.
fn skip_optional(test: &str, variable: &str, why: impl std::fmt::Display) {
    assert!(
        std::env::var(variable).as_deref() != Ok("require"),
        "{test}: {variable}=require but {why}"
    );
    eprintln!("SKIP {test}: {why}");
}

/// Load the backend and pick the first usable GPU, or skip.
fn setup(test: &str) -> Option<(Gpu, i32)> {
    match std::env::var("RACER_GPU_TESTS").as_deref() {
        Ok("1") | Ok("require") => {}
        _ => {
            eprintln!("SKIP {test}: set RACER_GPU_TESTS=1 to run GPU hardware tests");
            return None;
        }
    }
    let gpu = match Gpu::load() {
        Ok(gpu) => gpu,
        Err(err) => {
            skip(test, format_args!("no GPU backend ({err})"));
            return None;
        }
    };
    match gpu.usable_devices() {
        Ok(devices) if !devices.is_empty() => {
            let ordinal = devices[0].ordinal;
            Some((gpu, ordinal))
        }
        Ok(_) => {
            skip(test, "no usable GPU");
            None
        }
        Err(err) => {
            skip(test, format_args!("device discovery failed ({err})"));
            None
        }
    }
}

/// Deterministic xorshift bytes.
fn fill(seed: u64, out: &mut [u8]) {
    let mut state = seed | 1;
    for chunk in out.chunks_mut(8) {
        state ^= state << 13;
        state ^= state >> 7;
        state ^= state << 17;
        chunk.copy_from_slice(&state.to_le_bytes()[..chunk.len()]);
    }
}

/// A page sealed by `racer-crypto`, as the dataplane stores it.
struct Page {
    key: [u8; 32],
    nonce: [u8; 24],
    aad: Vec<u8>,
    plaintext: Vec<u8>,
    sealed: Vec<u8>,
}

impl Page {
    fn new(seed: u64, length: usize) -> Self {
        let mut key = crate::test_vector([0; 32]);
        let mut nonce = crate::test_vector([0; 24]);
        let mut aad = vec![0; 97];
        let mut plaintext = vec![0; length];
        fill(seed, &mut key);
        fill(seed + 1, &mut nonce);
        fill(seed + 2, &mut aad);
        fill(seed + 3, &mut plaintext);
        let mut sealed = vec![0; length + racer_crypto::TAG_LEN];
        racer_crypto::seal(&key, &nonce, &aad, &plaintext, &mut sealed).unwrap();
        Self {
            key,
            nonce,
            aad,
            plaintext,
            sealed,
        }
    }

    /// The CPU decrypt, the reference the GPU must match.
    fn cpu_open(&self) -> Vec<u8> {
        let mut out = vec![0; self.plaintext.len()];
        racer_crypto::open(&self.key, &self.nonce, &self.aad, &self.sealed, &mut out).unwrap();
        out
    }

    /// Decrypt `sealed` (already at `src` offset 0) into `dst` and return the verdict.
    fn gpu_decrypt(
        &self,
        context: &Context,
        src: &StagingBuffer,
        dst: Destination<'_>,
        dst_offset: u64,
    ) -> Verdict {
        let derived = racer_crypto::xaes_derive(&self.key, &self.nonce);
        let stream = context.stream().unwrap();
        let request = DecryptRequest {
            key: derived.key(),
            nonce: derived.nonce(),
            aad: &self.aad,
            length: self.plaintext.len() as u64,
        };
        queue_decrypt(&stream, &request, src, 0, dst, dst_offset);
        stream.synchronize().unwrap();
        stream.verdict().unwrap()
    }
}

/// Queue a decrypt, failing clearly when the shim has no kernels.
fn queue_decrypt(
    stream: &Stream,
    request: &DecryptRequest<'_>,
    src: &StagingBuffer,
    src_offset: u64,
    dst: Destination<'_>,
    dst_offset: u64,
) {
    match stream.decrypt(request, src, src_offset, dst, dst_offset) {
        Err(Error::KernelUnavailable) => {
            panic!("the shim has no decrypt kernels: build it with nvcc (RACER_GPU_REQUIRE_PTX=1)")
        }
        other => other.unwrap(),
    }
}

#[test]
/// The shim loads and CUDA initializes.
fn shim_loads_and_initializes() {
    let Some((gpu, _)) = setup("shim_loads_and_initializes") else {
        return;
    };
    assert_eq!(gpu.leaked(), 0);
}

#[test]
/// Discovery reports sane ordinals, UUIDs, PCI addresses and compute modes.
fn devices_are_enumerated() {
    let Some((gpu, _)) = setup("devices_are_enumerated") else {
        return;
    };
    let devices = gpu.devices().unwrap();
    assert!(!devices.is_empty());
    let mut uuids = std::collections::HashSet::new();
    for (index, device) in devices.iter().enumerate() {
        eprintln!(
            "GPU {} {} pci={} numa={:?} mode={:?} flags={:#x} memory={}",
            device.ordinal,
            device.display_id(),
            device.pci_bus_id,
            device.numa_node,
            device.compute_mode,
            device.flags,
            device.total_memory
        );
        assert_eq!(device.ordinal, index as i32);
        assert_ne!(device.uuid, [0; 16], "GPU {index} has no UUID");
        assert!(uuids.insert(device.uuid), "duplicate UUID");
        assert_eq!(device.pci_bus_id.len(), 12, "{}", device.pci_bus_id);
        assert_eq!(device.pci_bus_id, device.pci_bus_id.to_lowercase());
        assert!(device.total_memory > 0);
        assert!(!matches!(device.compute_mode, ComputeMode::Other(_)));
        assert_eq!(
            device.usable(),
            device.compute_mode == ComputeMode::Default
                && device.flags & (flag::VMM | flag::POSIX_FD) == flag::VMM | flag::POSIX_FD
        );
    }
    assert!(gpu.context(devices.len() as i32).is_err());
}

#[test]
/// A client allocation exported as a POSIX fd imports, accepts copies and
/// decrypts inside its window, and rejects writes outside it.
fn exported_allocation_imports_and_receives_decrypt() {
    let Some((gpu, ordinal)) = setup("exported_allocation_imports_and_receives_decrypt") else {
        return;
    };
    let cuda = match Cuda::load() {
        Ok(cuda) => cuda,
        Err(why) => return skip("exported_allocation_imports_and_receives_decrypt", why),
    };
    let page = Page::new(11, 70_001);
    let allocation = cuda.allocate(ordinal, 4 << 20);
    let size = allocation.size;
    let window = 4096..4096 + page.plaintext.len() as u64;

    let context = gpu.context(ordinal).unwrap();
    let import = context
        .import(allocation.export(), size, window.clone())
        .unwrap();
    assert_eq!(import.size(), size);
    assert_eq!(import.window(), window);

    let staging = context.staging(page.sealed.len() as u64).unwrap();
    let stream = context.stream().unwrap();
    stream
        .copy_from_host(Destination::Staging(&staging), 0, page.sealed.clone())
        .unwrap();
    assert_eq!(
        stream.copy_from_host(Destination::Import(&import), 0, vec![1u8; 16]),
        Err(Error::InvalidRange)
    );
    stream.synchronize().unwrap();

    let verdict = page.gpu_decrypt(
        &context,
        &staging,
        Destination::Import(&import),
        window.start,
    );
    assert_eq!(verdict, Verdict::Passed);
    let mut out = vec![0; page.plaintext.len()];
    allocation.read(window.start, &mut out);
    assert!(
        out == page.cpu_open(),
        "client allocation differs from CPU decrypt"
    );
    drop(import);
    drop(context);
    assert_eq!(gpu.leaked(), 0);
}

#[test]
/// GPU decrypt matches the CPU decrypt for several lengths, and a tampered
/// page is rejected without writing.
fn gpu_decrypt_matches_cpu_decrypt() {
    let Some((gpu, ordinal)) = setup("gpu_decrypt_matches_cpu_decrypt") else {
        return;
    };
    let context = gpu.context(ordinal).unwrap();
    for (seed, length) in [
        (1u64, 0usize),
        (2, 1),
        (3, 4097),
        (4, (1 << 20) + 13),
        (5, 16 << 20),
    ] {
        let page = Page::new(seed, length);
        let sealed_len = page.sealed.len() as u64;
        let src = context.staging(sealed_len).unwrap();
        let dst = context.staging(length.max(1) as u64).unwrap();
        let stream = context.stream().unwrap();
        stream
            .copy_from_host(Destination::Staging(&src), 0, page.sealed.clone())
            .unwrap();
        stream
            .copy_from_host(Destination::Staging(&dst), 0, vec![0xa5u8; length.max(1)])
            .unwrap();
        stream.synchronize().unwrap();

        let verdict = page.gpu_decrypt(&context, &src, Destination::Staging(&dst), 0);
        assert_eq!(verdict, Verdict::Passed, "length {length}");
        if length > 0 {
            let mut out = vec![0; length];
            stream.read_staging(&dst, 0, &mut out).unwrap();
            assert!(
                out == page.cpu_open(),
                "length {length}: GPU differs from CPU"
            );

            // Flip one ciphertext bit: tag check fails, destination untouched.
            let mut tampered = page.sealed.clone();
            tampered[length / 2] ^= 0x10;
            stream
                .copy_from_host(Destination::Staging(&src), 0, tampered)
                .unwrap();
            stream
                .copy_from_host(Destination::Staging(&dst), 0, vec![0xa5u8; length])
                .unwrap();
            stream.synchronize().unwrap();
            let verdict = page.gpu_decrypt(&context, &src, Destination::Staging(&dst), 0);
            assert_eq!(
                verdict,
                Verdict::TagMismatch,
                "length {length}: {verdict:?}"
            );
            stream.read_staging(&dst, 0, &mut out).unwrap();
            assert!(
                out.iter().all(|&b| b == 0xa5),
                "length {length}: tampered page was written"
            );
        }
    }
    assert_eq!(gpu.leaked(), 0);
}

#[test]
/// cuFile reads a sealed page from an `O_DIRECT` file into staging memory,
/// directly or detached on another thread, and the GPU decrypts it to the
/// CPU result. A buffer a stream holds is refused.
fn cufile_reads_sealed_page_into_gpu() {
    const TEST: &str = "cufile_reads_sealed_page_into_gpu";
    let Some((gpu, ordinal)) = setup(TEST) else {
        return;
    };

    // A 64 KiB sealed page at a 4 KiB-aligned record offset, as in a slab.
    let page = Page::new(21, (64 << 10) - racer_crypto::TAG_LEN);
    let record_offset = 8192u64;
    let dir = std::env::var_os("RACER_GPU_TEST_DIR")
        .map(std::path::PathBuf::from)
        .unwrap_or_else(|| env!("CARGO_TARGET_TMPDIR").into());
    let path = dir.join(format!("racer-gpu-cufile-{}.slab", std::process::id()));
    {
        let mut file = std::fs::File::create(&path).unwrap();
        file.write_all(&vec![0u8; record_offset as usize]).unwrap();
        file.write_all(&page.sealed).unwrap();
        file.write_all(&vec![0u8; 4096]).unwrap();
        file.sync_all().unwrap();
    }
    let opened = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_DIRECT)
        .open(&path);
    let _ = std::fs::remove_file(&path);
    let file = match opened {
        Ok(file) => OwnedFd::from(file),
        Err(err) => {
            return skip_optional(
                TEST,
                CUFILE,
                format_args!(
                    "O_DIRECT open of {} failed ({err}); set RACER_GPU_TEST_DIR",
                    dir.display()
                ),
            );
        }
    };
    if let Err(err) = open_cufile(&gpu) {
        return skip_optional(TEST, CUFILE, format_args!("cuFile is unavailable ({err})"));
    }
    let gpu_file = match gpu.open_file(file) {
        Ok(file) => file,
        Err(err) => {
            return skip_optional(
                TEST,
                CUFILE,
                format_args!("cuFile cannot read {} ({err})", dir.display()),
            );
        }
    };

    let context = gpu.context(ordinal).unwrap();
    let length = page.sealed.len() as u64;
    let staging = context.staging(length).unwrap();
    staging.register_cufile().unwrap();
    gpu_file.read(&staging, 0, record_offset, length).unwrap();
    let stream = context.stream().unwrap();
    let mut readback = vec![0; page.sealed.len()];
    stream.read_staging(&staging, 0, &mut readback).unwrap();
    assert!(readback == page.sealed, "cuFile read the wrong bytes");

    // A detached read on another thread lands the same bytes, here into an
    // unregistered buffer at an offset.
    let reader = gpu.file_reader();
    let other = context.staging(4096 + length).unwrap();
    let read = unsafe { gpu_file.detach(&other, 4096, record_offset, length) }.unwrap();
    std::thread::scope(|scope| scope.spawn(|| reader.read(read)).join().unwrap()).unwrap();
    stream.read_staging(&other, 4096, &mut readback).unwrap();
    assert!(
        readback == page.sealed,
        "detached cuFile read the wrong bytes"
    );

    let dst = context.staging(page.plaintext.len() as u64).unwrap();
    let verdict = page.gpu_decrypt(&context, &staging, Destination::Staging(&dst), 0);
    assert_eq!(verdict, Verdict::Passed);
    let mut out = vec![0; page.plaintext.len()];
    stream.read_staging(&dst, 0, &mut out).unwrap();
    assert!(out == page.cpu_open());
    assert_eq!(
        gpu_file.read(&staging, 0, record_offset, length + 1),
        Err(Error::InvalidRange)
    );

    // A queued copy holds the buffer, so a direct read waits for the sync.
    stream
        .copy_from_host(Destination::Staging(&staging), 0, [0u8; 16])
        .unwrap();
    assert_eq!(
        gpu_file.read(&staging, 0, record_offset, length),
        Err(Error::Busy)
    );
    stream.synchronize().unwrap();
    gpu_file.read(&staging, 0, record_offset, length).unwrap();
    drop((gpu_file, staging, other, dst, stream, context));
    assert_eq!(gpu.leaked(), 0);
}

/// Whether this process opens cuFile with PCI P2PDMA.
fn pci_p2pdma() -> bool {
    std::env::var("RACER_GPU_PCI_P2PDMA").as_deref() == Ok("true")
}

/// Open cuFile the way every test in this process does. The shim's first
/// open is final for as long as it stays loaded, so one backend is kept for
/// the life of the process; otherwise the shim unloads between tests and
/// the next test would open cuFile again over a driver that is still open.
fn open_cufile(gpu: &Gpu) -> Result<(), Error> {
    static KEEP_LOADED: OnceLock<Gpu> = OnceLock::new();
    KEEP_LOADED.get_or_init(|| gpu.clone());
    gpu.set_pci_p2pdma(pci_p2pdma());
    gpu.open_cufile()
}

/// What the shim could not load from cuFile: the library, or the first
/// function it needs that is missing. `None` when everything loads.
fn cufile_missing() -> Option<String> {
    let library = unsafe {
        libc::dlopen(
            c"libcufile.so.0".as_ptr(),
            libc::RTLD_NOW | libc::RTLD_LOCAL,
        )
    };
    if library.is_null() {
        return Some("libcufile.so.0".into());
    }
    let mut needed = vec![
        c"cuFileDriverOpen",
        c"cuFileHandleRegister",
        c"cuFileHandleDeregister",
        c"cuFileBufRegister",
        c"cuFileBufDeregister",
        c"cuFileRead",
    ];
    if pci_p2pdma() {
        needed.push(c"cuFileSetParameterBool");
    }
    let missing = needed
        .into_iter()
        .find(|name| unsafe { libc::dlsym(library, name.as_ptr()) }.is_null());
    unsafe { libc::dlclose(library) };
    missing.map(|name| format!("{name:?}"))
}

#[test]
/// The first cuFile open is final: opening again, from another thread, or
/// from another backend asking for the other PCI P2PDMA setting returns the
/// same result. A failed open carries a cuFile status that has a name, and
/// is `Unavailable` only when libcufile or a function it needs is missing.
fn cufile_driver_open_is_final_and_named() {
    const TEST: &str = "cufile_driver_open_is_final_and_named";
    let Some((gpu, _)) = setup(TEST) else {
        return;
    };
    let first = open_cufile(&gpu);
    let shown = first.map_err(|error| error.to_string());
    eprintln!("{TEST}: pci_p2pdma={} open={shown:?}", pci_p2pdma());
    assert_eq!(gpu.open_cufile(), first, "a second open");
    let reader = gpu.file_reader();
    let threaded = std::thread::spawn(move || reader.open_driver());
    assert_eq!(threaded.join().unwrap(), first, "an open on another thread");
    let other = Gpu::load().unwrap();
    other.set_pci_p2pdma(!pci_p2pdma());
    assert_eq!(other.open_cufile(), first, "an open with the other mode");
    let reader = other.file_reader();
    let threaded = std::thread::spawn(move || reader.open_driver());
    assert_eq!(
        threaded.join().unwrap(),
        first,
        "a reader's open with the other mode"
    );
    match first {
        Ok(()) => return,
        Err(Error::Driver(status)) => assert!(
            cufile_status_name(status).is_some(),
            "{TEST}: cuFile failed to open with status {status}, which is not a cuFile status"
        ),
        Err(Error::Unavailable) => match cufile_missing() {
            Some(missing) => eprintln!("{TEST}: cuFile is unavailable: {missing} is missing"),
            None => {
                panic!("{TEST}: open is Unavailable, but libcufile.so.0 and its functions load")
            }
        },
        Err(error) => panic!("{TEST}: unexpected cuFile open error {error:?}"),
    }
    skip_optional(
        TEST,
        CUFILE,
        format_args!(
            "cuFile did not open ({}); checked its failure only",
            shown.unwrap_err()
        ),
    );
}

#[test]
/// With PCI P2PDMA set before the first open, cuFile opens or fails with a
/// named status, as finally as without it, and the cuFile tests pass either
/// way. This process's open is already decided, so they rerun in a child
/// process with `RACER_GPU_PCI_P2PDMA=true`. There cuFile may fail to open
/// even with `RACER_GPU_TESTS_CUFILE=require`: P2PDMA needs kernel and NVMe
/// support.
fn cufile_tests_rerun_with_pci_p2pdma() {
    const TEST: &str = "cufile_tests_rerun_with_pci_p2pdma";
    const RERUN: [&str; 3] = [
        "cufile_driver_open_is_final_and_named",
        "cufile_staging_registration_matches_driver_open",
        "cufile_reads_sealed_page_into_gpu",
    ];
    if setup(TEST).is_none() {
        return;
    }
    if pci_p2pdma() {
        eprintln!("SKIP {TEST}: RACER_GPU_PCI_P2PDMA=true already opens this process with it");
        return;
    }
    let child = std::process::Command::new(std::env::current_exe().unwrap())
        .args(RERUN)
        .args(["--exact", "--nocapture", "--test-threads=1"])
        .env("RACER_GPU_PCI_P2PDMA", "true")
        .env_remove(CUFILE)
        .output()
        .unwrap();
    let stdout = String::from_utf8_lossy(&child.stdout);
    eprintln!("{stdout}{}", String::from_utf8_lossy(&child.stderr));
    assert!(child.status.success(), "{TEST}: the run with P2PDMA failed");
    assert!(
        stdout.contains(&format!("{} passed", RERUN.len())),
        "{TEST}: not every cuFile test ran"
    );
}

#[test]
/// Staging memory registers with cuFile when the driver opened, and
/// otherwise fails with the open's error, on any thread. Registering twice
/// is fine. Offsets are checked against the requested size, not the 2 MiB
/// rounded allocation. Registered buffers take stream copies across the
/// rounding, and deregister and free without leaks.
fn cufile_staging_registration_matches_driver_open() {
    const TEST: &str = "cufile_staging_registration_matches_driver_open";
    let Some((gpu, ordinal)) = setup(TEST) else {
        return;
    };
    let opened = open_cufile(&gpu);
    let context = gpu.context(ordinal).unwrap();
    let size = 3u64 << 20;
    let first = context.staging(size).unwrap();
    assert_eq!(first.size(), size);
    assert_eq!(first.register_cufile(), opened);
    assert_eq!(first.register_cufile(), opened, "a second registration");
    let second = context.staging(size).unwrap();
    let registered =
        std::thread::scope(|scope| scope.spawn(|| second.register_cufile()).join().unwrap());
    assert_eq!(registered, opened, "a registration on another thread");

    let mut bytes = vec![0; 1 << 20];
    fill(71, &mut bytes);
    let at = (2 << 20) - 4093;
    let stream = context.stream().unwrap();
    stream
        .copy_from_host(Destination::Staging(&first), at, bytes.clone())
        .unwrap();
    stream
        .copy_from_staging(
            Destination::Staging(&second),
            5,
            &first,
            at,
            bytes.len() as u64,
        )
        .unwrap();
    let mut out = vec![0; bytes.len()];
    stream.read_staging(&second, 5, &mut out).unwrap();
    assert!(out == bytes, "staging round trip differs");
    assert_eq!(
        stream.copy_from_host(Destination::Staging(&first), size - 1, [0u8; 2]),
        Err(Error::InvalidRange)
    );
    assert_eq!(
        stream.read_staging(&first, size, &mut [0u8; 1]),
        Err(Error::InvalidRange)
    );
    drop((stream, first, second, context));
    assert_eq!(gpu.leaked(), 0);
    if let Err(error) = opened {
        skip_optional(
            TEST,
            CUFILE,
            format_args!("cuFile did not open ({error}); checked registration fails the same way"),
        );
    }
}

/// The few CUDA driver calls a client makes to export an allocation, loaded
/// from `libcuda.so.1`, so the import path is tested against real exports.
struct Cuda {
    init: unsafe extern "C" fn(c_uint) -> c_int,
    device_get: unsafe extern "C" fn(*mut c_int, c_int) -> c_int,
    retain: unsafe extern "C" fn(*mut *mut c_void, c_int) -> c_int,
    set_current: unsafe extern "C" fn(*mut c_void) -> c_int,
    granularity: unsafe extern "C" fn(*mut usize, *const AllocationProp, c_int) -> c_int,
    create: unsafe extern "C" fn(*mut u64, usize, *const AllocationProp, u64) -> c_int,
    export: unsafe extern "C" fn(*mut c_void, u64, c_int, u64) -> c_int,
    reserve: unsafe extern "C" fn(*mut u64, usize, usize, u64, u64) -> c_int,
    map: unsafe extern "C" fn(u64, usize, usize, u64, u64) -> c_int,
    set_access: unsafe extern "C" fn(u64, usize, *const AccessDesc, usize) -> c_int,
    copy_d2h: unsafe extern "C" fn(*mut c_void, u64, usize) -> c_int,
    unmap: unsafe extern "C" fn(u64, usize) -> c_int,
    address_free: unsafe extern "C" fn(u64, usize) -> c_int,
    release: unsafe extern "C" fn(u64) -> c_int,
}

/// `CUmemAllocationProp`.
#[repr(C)]
struct AllocationProp {
    kind: c_int,
    handle_types: c_int,
    location_type: c_int,
    location_id: c_int,
    win32: *mut c_void,
    flags: [u8; 8],
}

/// `CUmemAccessDesc`.
#[repr(C)]
struct AccessDesc {
    location_type: c_int,
    location_id: c_int,
    flags: c_int,
}

impl Cuda {
    /// Resolve the driver entry points. Field types give the signatures.
    #[allow(clippy::missing_transmute_annotations)]
    fn load() -> Result<Self, String> {
        let library = unsafe { libc::dlopen(c"libcuda.so.1".as_ptr(), libc::RTLD_NOW) };
        if library.is_null() {
            return Err("libcuda.so.1 not found".into());
        }
        let symbol = |name: &CStr| -> Result<*mut c_void, String> {
            let ptr = unsafe { libc::dlsym(library, name.as_ptr()) };
            if ptr.is_null() {
                Err(format!("libcuda.so.1 lacks {name:?}"))
            } else {
                Ok(ptr)
            }
        };
        macro_rules! load {
            ($name:literal) => {
                unsafe { std::mem::transmute::<*mut c_void, _>(symbol($name)?) }
            };
        }
        let cuda = Self {
            init: load!(c"cuInit"),
            device_get: load!(c"cuDeviceGet"),
            retain: load!(c"cuDevicePrimaryCtxRetain"),
            set_current: load!(c"cuCtxSetCurrent"),
            granularity: load!(c"cuMemGetAllocationGranularity"),
            create: load!(c"cuMemCreate"),
            export: load!(c"cuMemExportToShareableHandle"),
            reserve: load!(c"cuMemAddressReserve"),
            map: load!(c"cuMemMap"),
            set_access: load!(c"cuMemSetAccess"),
            copy_d2h: load!(c"cuMemcpyDtoH_v2"),
            unmap: load!(c"cuMemUnmap"),
            address_free: load!(c"cuMemAddressFree"),
            release: load!(c"cuMemRelease"),
        };
        check("cuInit", unsafe { (cuda.init)(0) });
        Ok(cuda)
    }

    /// Make the GPU's primary context current on this thread.
    fn bind(&self, ordinal: i32) {
        let mut device = 0;
        check("cuDeviceGet", unsafe {
            (self.device_get)(&mut device, ordinal)
        });
        let mut context = std::ptr::null_mut();
        check("cuDevicePrimaryCtxRetain", unsafe {
            (self.retain)(&mut context, device)
        });
        check("cuCtxSetCurrent", unsafe { (self.set_current)(context) });
    }

    /// The minimum granularity of exportable memory on the GPU.
    fn granule(&self, ordinal: i32) -> u64 {
        self.bind(ordinal);
        let mut granularity = 0;
        check("cuMemGetAllocationGranularity", unsafe {
            (self.granularity)(&mut granularity, &AllocationProp::device(ordinal), 0)
        });
        granularity as u64
    }

    /// Allocate exportable device memory, as the Python client does.
    fn allocate(&self, ordinal: i32, size: usize) -> Allocation<'_> {
        let granularity = self.granule(ordinal) as usize;
        let size = size.div_ceil(granularity) * granularity;
        let mut handle = 0;
        check("cuMemCreate", unsafe {
            (self.create)(&mut handle, size, &AllocationProp::device(ordinal), 0)
        });
        Allocation {
            cuda: self,
            ordinal,
            handle,
            size: size as u64,
        }
    }
}

impl AllocationProp {
    /// Pinned device memory on the GPU, exportable as a POSIX fd.
    fn device(ordinal: i32) -> Self {
        Self {
            kind: 1,         // CU_MEM_ALLOCATION_TYPE_PINNED
            handle_types: 1, // CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR
            location_type: 1,
            location_id: ordinal,
            win32: std::ptr::null_mut(),
            flags: [0; 8],
        }
    }
}

/// One exportable allocation.
struct Allocation<'a> {
    cuda: &'a Cuda,
    ordinal: i32,
    handle: u64,
    size: u64,
}

impl Allocation<'_> {
    /// Export as a POSIX fd.
    fn export(&self) -> OwnedFd {
        let mut fd: c_int = -1;
        check("cuMemExportToShareableHandle", unsafe {
            (self.cuda.export)((&mut fd as *mut c_int).cast(), self.handle, 1, 0)
        });
        assert!(fd >= 0);
        unsafe { OwnedFd::from_raw_fd(fd) }
    }

    /// Map the allocation and copy `out.len()` bytes at `offset` to the host.
    fn read(&self, offset: u64, out: &mut [u8]) {
        let cuda = self.cuda;
        cuda.bind(self.ordinal);
        let size = self.size as usize;
        let mut ptr = 0;
        check("cuMemAddressReserve", unsafe {
            (cuda.reserve)(&mut ptr, size, 0, 0, 0)
        });
        check("cuMemMap", unsafe {
            (cuda.map)(ptr, size, 0, self.handle, 0)
        });
        let access = AccessDesc {
            location_type: 1,
            location_id: self.ordinal,
            flags: 3, // CU_MEM_ACCESS_FLAGS_PROT_READWRITE
        };
        check("cuMemSetAccess", unsafe {
            (cuda.set_access)(ptr, size, &access, 1)
        });
        check("cuMemcpyDtoH", unsafe {
            (cuda.copy_d2h)(out.as_mut_ptr().cast(), ptr + offset, out.len())
        });
        check("cuMemUnmap", unsafe { (cuda.unmap)(ptr, size) });
        check("cuMemAddressFree", unsafe {
            (cuda.address_free)(ptr, size)
        });
    }
}

impl Drop for Allocation<'_> {
    fn drop(&mut self) {
        unsafe { (self.cuda.release)(self.handle) };
    }
}

/// Panic on a CUDA driver error.
fn check(call: &str, rc: c_int) {
    assert_eq!(rc, 0, "{call} failed with CUresult {rc}");
}

#[test]
/// Events, pinned host memory and an RDMA flush query work on a real GPU:
/// bytes go host -> staging -> pinned through fences, without a stream sync.
fn fences_and_pinned_copies_round_trip() {
    let Some((gpu, ordinal)) = setup("fences_and_pinned_copies_round_trip") else {
        return;
    };
    let context = gpu.context(ordinal).unwrap();
    let staging = context.staging(1 << 20).unwrap();
    let pinned = context.pinned(1 << 20).unwrap();
    let stream = context.stream().unwrap();
    let bytes: Vec<u8> = (0..1 << 20).map(|i| (i % 253) as u8).collect();
    pinned.write(0, &bytes).unwrap();
    stream
        .copy_from_pinned(Destination::Staging(&staging), 0, &pinned, 0, 1 << 20)
        .unwrap();
    let first = stream.fence().unwrap();
    first.wait().unwrap();
    pinned.write(0, &vec![0; 1 << 20]).unwrap();
    stream
        .copy_to_pinned(&staging, 0, &pinned, 0, 1 << 20)
        .unwrap();
    let second = stream.fence().unwrap();
    while !second.poll().unwrap() {
        std::thread::yield_now();
    }
    let mut out = vec![0; 1 << 20];
    pinned.read(0, &mut out).unwrap();
    assert!(out == bytes, "pinned round trip differs");
    // Either ordered natively, flushed, or reported unsupported.
    match context.flush_rdma_writes(racer_gpu::RdmaScope::Owner) {
        Ok(()) | Err(racer_gpu::Error::Unsupported(_)) => {}
        Err(error) => panic!("flush failed: {error}"),
    }
    drop((first, second, pinned, staging, stream, context));
    assert_eq!(gpu.leaked(), 0);
}

#[test]
/// A client allocation imports only at its exact size, a multiple of the
/// granularity. Copies from pageable host, registered host and staging
/// memory land at unaligned offsets across the boundary between two
/// granules; writes outside the window are refused and leave the bytes
/// around it as they were. An import dropped with a copy queued is released
/// only once the stream syncs.
fn imported_allocation_takes_copies_across_granules() {
    const TEST: &str = "imported_allocation_takes_copies_across_granules";
    let Some((gpu, ordinal)) = setup(TEST) else {
        return;
    };
    let cuda = match Cuda::load() {
        Ok(cuda) => cuda,
        Err(why) => return skip(TEST, why),
    };
    let granule = cuda.granule(ordinal);
    let allocation = cuda.allocate(ordinal, 2 * granule as usize);
    let size = allocation.size;
    let context = gpu.context(ordinal).unwrap();
    for (claimed, window, error) in [
        (size, 0..0, Error::InvalidRange),
        (size, 0..size + 1, Error::InvalidRange),
        (size - 1, 0..1, Error::InvalidRequest),
    ] {
        let result = context.import(allocation.export(), claimed, window.clone());
        assert_eq!(
            result.err(),
            Some(error),
            "size {claimed}, window {window:?}"
        );
    }
    // Larger than the allocation: the map fails and the import is undone.
    let result = context.import(allocation.export(), size + granule, 0..1);
    assert!(
        matches!(result.as_ref().err(), Some(Error::Driver(_))),
        "an import past the allocation: {:?}",
        result.err()
    );

    // Unaligned at both ends, straddling the boundary between the granules.
    let window = granule - 8195..granule + 8197;
    let span = (window.end - window.start) as usize;
    let at = |offset: u64| (offset - window.start) as usize;
    let mut before = vec![0; span + 32];
    allocation.read(window.start - 16, &mut before);
    let import = context
        .import(allocation.export(), size, window.clone())
        .unwrap();
    assert_eq!(import.window(), window);
    assert_eq!(import.ordinal(), ordinal);
    let dst = Destination::Import(&import);
    let stream = context.stream().unwrap();

    let mut expected = vec![0; span];
    fill(81, &mut expected);
    stream
        .copy_from_host(dst, window.start, expected.clone())
        .unwrap();
    let mut staged = vec![0; 9000];
    fill(82, &mut staged);
    let staging = context.staging(7 + staged.len() as u64).unwrap();
    stream
        .copy_from_host(Destination::Staging(&staging), 7, staged.clone())
        .unwrap();
    stream
        .copy_from_staging(dst, granule - 4001, &staging, 7, staged.len() as u64)
        .unwrap();
    expected[at(granule - 4001)..][..staged.len()].copy_from_slice(&staged);
    let mut registered = vec![0; 5003];
    fill(83, &mut registered);
    let host: Arc<[u8]> = registered.into();
    let registration =
        unsafe { context.register_host(host.as_ptr().cast_mut(), host.len()) }.unwrap();
    stream
        .copy_from_host(dst, granule - 2500, host.clone())
        .unwrap();
    expected[at(granule - 2500)..][..host.len()].copy_from_slice(&host);
    for (offset, length) in [(window.start - 1, 2), (window.end - 1, 2), (window.end, 1)] {
        assert_eq!(
            stream.copy_from_host(dst, offset, vec![0xffu8; length]),
            Err(Error::InvalidRange),
            "{offset}+{length}"
        );
    }
    assert_eq!(
        stream.copy_from_staging(dst, window.end - 8, &staging, 7, 9),
        Err(Error::InvalidRange)
    );
    stream.synchronize().unwrap();
    drop(registration);

    let mut after = vec![0; span + 32];
    allocation.read(window.start - 16, &mut after);
    assert!(
        after[16..16 + span] == expected[..],
        "the window differs from the copies"
    );
    assert_eq!(after[..16], before[..16], "bytes below the window changed");
    assert_eq!(
        after[16 + span..],
        before[16 + span..],
        "bytes above the window changed"
    );
    drop(import);

    // An early unmap would fault the queued copy.
    let late = context.import(allocation.export(), size, 0..16).unwrap();
    stream
        .copy_from_host(Destination::Import(&late), 0, *b"0123456789abcdef")
        .unwrap();
    drop(late);
    stream.synchronize().unwrap();
    let mut head = [0; 16];
    allocation.read(0, &mut head);
    assert_eq!(&head, b"0123456789abcdef");
    drop((stream, staging, context));
    assert_eq!(gpu.leaked(), 0);
}

#[test]
/// Staging memory exports page-aligned ranges of its allocation as dma-buf
/// fds the size of the range, as a NIC registers them, and the shim refuses
/// unaligned, empty or outside ranges before asking the driver.
fn staging_exports_dma_buf_ranges() {
    const TEST: &str = "staging_exports_dma_buf_ranges";
    const DMABUF: &str = "RACER_GPU_TESTS_DMABUF";
    let Some((gpu, ordinal)) = setup(TEST) else {
        return;
    };
    let context = gpu.context(ordinal).unwrap();
    let page = unsafe { libc::sysconf(libc::_SC_PAGESIZE) } as u64;
    // A multiple of 2 MiB, so the device allocation is exactly this size.
    let size = 4u64 << 20;
    let staging = context.staging(size).unwrap();
    for (offset, length) in [
        (1, page),
        (0, page + 1),
        (0, 0),
        (size, page),
        (0, size + page),
        (u64::MAX - page + 1, page),
    ] {
        assert_eq!(
            staging.dma_buf(offset, length).err(),
            Some(Error::InvalidRange),
            "{offset}+{length}"
        );
    }
    let device = gpu
        .devices()
        .unwrap()
        .into_iter()
        .find(|device| device.ordinal == ordinal)
        .unwrap();
    if device.flags & flag::DMA_BUF == 0 {
        return skip_optional(
            TEST,
            DMABUF,
            "the GPU or driver does not support dma-buf export (needs the open kernel modules)",
        );
    }
    let mut exported = Vec::new();
    for (offset, length) in [(0, size), (page, page), (size - (2 << 20), 2 << 20)] {
        match staging.dma_buf(offset, length) {
            Ok(fd) => exported.push(fd),
            Err(Error::Driver(801)) => {
                return skip_optional(
                    TEST,
                    DMABUF,
                    "the driver reports dma-buf export unsupported (CUDA_ERROR_NOT_SUPPORTED)",
                );
            }
            Err(error) => panic!("{TEST}: exporting {offset}+{length} failed: {error}"),
        }
    }
    // A dma-buf reports its size as its end.
    let end = unsafe { libc::lseek(exported[0].as_raw_fd(), 0, libc::SEEK_END) };
    assert_eq!(end, size as i64, "the dma-buf is not the size of the range");
    drop(exported);
    drop((staging, context));
    assert_eq!(gpu.leaked(), 0);
}

#[test]
/// A stream takes one decrypt at a time. Its verdict is ready once a fence
/// after it completes, and is read once. A page with a corrupted tag or the
/// wrong AAD, empty or not, fails authentication and leaves the destination
/// as it was. Unaligned source and destination offsets decrypt exactly.
fn decrypt_verdicts_follow_fences_and_reject_forgeries() {
    const TEST: &str = "decrypt_verdicts_follow_fences_and_reject_forgeries";
    let Some((gpu, ordinal)) = setup(TEST) else {
        return;
    };
    let context = gpu.context(ordinal).unwrap();
    let stream = context.stream().unwrap();
    assert_eq!(stream.verdict(), Err(Error::Busy), "no decrypt queued");
    let (src_offset, dst_offset) = (4099u64, 517u64);
    for (seed, length) in [(31u64, 10_003usize), (32, 0)] {
        let page = Page::new(seed, length);
        let derived = racer_crypto::xaes_derive(&page.key, &page.nonce);
        let src = context
            .staging(src_offset + page.sealed.len() as u64)
            .unwrap();
        let dst = context.staging(dst_offset + length as u64 + 1).unwrap();
        let fresh = vec![0xa5u8; dst.size() as usize];
        let mut bad_tag = page.sealed.clone();
        *bad_tag.last_mut().unwrap() ^= 0x80;
        let mut bad_aad = page.aad.clone();
        bad_aad[0] ^= 1;
        for (case, sealed, aad) in [
            ("valid", &page.sealed, &page.aad),
            ("corrupted tag", &bad_tag, &page.aad),
            ("wrong AAD", &page.sealed, &bad_aad),
        ] {
            stream
                .copy_from_host(Destination::Staging(&src), src_offset, sealed.clone())
                .unwrap();
            stream
                .copy_from_host(Destination::Staging(&dst), 0, fresh.clone())
                .unwrap();
            let request = DecryptRequest {
                key: derived.key(),
                nonce: derived.nonce(),
                aad,
                length: length as u64,
            };
            let to = Destination::Staging(&dst);
            queue_decrypt(&stream, &request, &src, src_offset, to, dst_offset);
            assert_eq!(
                stream.decrypt(&request, &src, src_offset, to, dst_offset),
                Err(Error::Busy),
                "length {length}, {case}: a second decrypt before the first completes"
            );
            stream.fence().unwrap().wait().unwrap();
            let expected = if case == "valid" {
                Verdict::Passed
            } else {
                Verdict::TagMismatch
            };
            assert_eq!(stream.verdict(), Ok(expected), "length {length}, {case}");
            assert_eq!(
                stream.verdict(),
                Err(Error::Busy),
                "length {length}, {case}: the verdict is read once"
            );
            let mut wanted = fresh.clone();
            if expected == Verdict::Passed {
                wanted[dst_offset as usize..][..length].copy_from_slice(&page.cpu_open());
            }
            let mut out = vec![0; fresh.len()];
            stream.read_staging(&dst, 0, &mut out).unwrap();
            assert!(
                out == wanted,
                "length {length}, {case}: wrong destination bytes"
            );
        }
    }
    drop((stream, context));
    assert_eq!(gpu.leaked(), 0);
}

#[test]
/// Fences order work across streams and threads: a stream on a thread that
/// never made a context current waits for another stream's fence before it
/// copies what that stream wrote. `on_complete` runs once, after the work
/// queued before it. Pooled events are reused, and each fence still waits
/// for its own work.
fn fences_order_streams_and_wake_callbacks() {
    const TEST: &str = "fences_order_streams_and_wake_callbacks";
    const LEN: u64 = 32 << 20;
    let Some((gpu, ordinal)) = setup(TEST) else {
        return;
    };
    let context = gpu.context(ordinal).unwrap();
    let mut bytes = vec![0; LEN as usize];
    fill(91, &mut bytes);
    let pinned = context.pinned(LEN).unwrap();
    pinned.write(0, &bytes).unwrap();
    let source = context.staging(LEN).unwrap();
    let writer = context.stream().unwrap();
    writer
        .copy_from_pinned(Destination::Staging(&source), 0, &pinned, 0, LEN)
        .unwrap();
    let written = writer.fence().unwrap();
    let copied = std::thread::scope(|scope| {
        scope
            .spawn(|| {
                let _thread = gpu.enter_thread();
                let reader = context.stream().unwrap();
                let copy = context.staging(LEN).unwrap();
                reader.wait_for(&written).unwrap();
                reader
                    .copy_from_staging(Destination::Staging(&copy), 0, &source, 0, LEN)
                    .unwrap();
                let mut out = vec![0; LEN as usize];
                reader.read_staging(&copy, 0, &mut out).unwrap();
                out
            })
            .join()
            .unwrap()
    });
    assert!(
        copied == bytes,
        "the reader copied before the writer's fence"
    );
    assert!(written.poll().unwrap());

    let (sender, receiver) = mpsc::channel();
    writer
        .copy_from_pinned(Destination::Staging(&source), 0, &pinned, 0, LEN)
        .unwrap();
    let queued = writer.fence().unwrap();
    writer
        .on_complete(Box::new(move || {
            let _ = sender.send(());
        }))
        .unwrap();
    receiver
        .recv_timeout(Duration::from_secs(60))
        .expect("the on_complete callback did not run");
    assert!(
        queued.poll().unwrap(),
        "the callback ran before the work queued ahead of it"
    );
    assert_eq!(
        receiver.recv_timeout(Duration::from_secs(60)),
        Err(mpsc::RecvTimeoutError::Disconnected),
        "the callback was not dropped after it ran"
    );

    let pool = context.event_pool(2);
    let round = 1u64 << 20;
    for value in 1..=4u8 {
        writer
            .copy_from_host(
                Destination::Staging(&source),
                0,
                vec![value; round as usize],
            )
            .unwrap();
        writer
            .copy_to_pinned(&source, 0, &pinned, 0, round)
            .unwrap();
        writer.fence_in(&pool).unwrap().wait().unwrap();
        let mut out = vec![0; round as usize];
        pinned.read(0, &mut out).unwrap();
        assert!(
            out.iter().all(|&b| b == value),
            "round {value}: a reused event did not wait for its work"
        );
    }
    assert_eq!(pool.created(), 1, "one event serves every fence");
    assert_eq!(pool.idle(), 1);
    let fences: Vec<_> = (0..3).map(|_| writer.fence_in(&pool).unwrap()).collect();
    assert_eq!(pool.created(), 3);
    drop(fences);
    assert_eq!(pool.idle(), 2, "the pool keeps at most its limit");
    drop((pool, queued, written, pinned, source, writer, context));
    assert_eq!(gpu.leaked(), 0);
}

#[test]
/// Across two GPUs: an allocation imports only on the GPU that owns it.
/// Peer copies need enabled access, wait for the source GPU's fence, and
/// land in the other GPU's staging and client memory. Without a peer path,
/// bytes bounce through portable pinned memory after a pooled fence on the
/// source GPU, as the dataplane does.
fn two_gpus_copy_by_peer_access_or_pinned_bounce() {
    const TEST: &str = "two_gpus_copy_by_peer_access_or_pinned_bounce";
    const LEN: u64 = (4 << 20) + 13;
    let Some((gpu, _)) = setup(TEST) else {
        return;
    };
    let devices = gpu.usable_devices().unwrap();
    let [first, second, ..] = devices.as_slice() else {
        return skip_optional(
            TEST,
            "RACER_GPU_TESTS_MULTI",
            format_args!("{} usable GPU, two needed", devices.len()),
        );
    };
    let (a, b) = (first.ordinal, second.ordinal);
    let cuda = match Cuda::load() {
        Ok(cuda) => cuda,
        Err(why) => return skip(TEST, why),
    };
    let (near, far) = (gpu.context(a).unwrap(), gpu.context(b).unwrap());
    let allocation = cuda.allocate(a, (LEN + 8192) as usize);
    let size = allocation.size;
    assert_eq!(
        far.import(allocation.export(), size, 0..1).err(),
        Some(Error::WrongDevice),
        "an import on the GPU that does not own the allocation"
    );
    let window = 4103..4103 + LEN;
    let import = near
        .import(allocation.export(), size, window.clone())
        .unwrap();

    // The bytes land on the far GPU.
    let mut bytes = vec![0; LEN as usize];
    fill(101, &mut bytes);
    let remote = far.staging(LEN).unwrap();
    let far_stream = far.stream().unwrap();
    far_stream
        .copy_from_host(Destination::Staging(&remote), 0, bytes.clone())
        .unwrap();
    let landed = far_stream.fence().unwrap();

    let local = near.staging(LEN).unwrap();
    let near_stream = near.stream().unwrap();
    assert_eq!(
        near_stream.copy_from_peer(Destination::Staging(&local), 0, &remote, 0, LEN),
        Err(Error::WrongDevice),
        "a peer copy before enable_peer"
    );
    assert_eq!(near.enable_peer(&near), Ok(true));
    let peer = near.enable_peer(&far).unwrap();
    assert_eq!(near.enable_peer(&far), Ok(peer), "enable_peer again");
    match near.flush_rdma_writes(RdmaScope::AllDevices) {
        Ok(()) | Err(Error::Unsupported(_)) => {}
        Err(error) => panic!("{TEST}: flush failed: {error}"),
    }
    near_stream.wait_for(&landed).unwrap();
    if peer {
        near_stream
            .copy_from_peer(Destination::Staging(&local), 0, &remote, 0, LEN)
            .unwrap();
        near_stream
            .copy_from_peer(Destination::Import(&import), window.start, &remote, 0, LEN)
            .unwrap();
    } else {
        eprintln!("{TEST}: GPUs {a} and {b} have no peer path; checking the bounce only");
        assert_eq!(
            near_stream.copy_from_peer(Destination::Staging(&local), 0, &remote, 0, LEN),
            Err(Error::WrongDevice)
        );
    }

    let pinned = far.pinned(LEN).unwrap();
    let events = far.event_pool(1);
    far_stream
        .copy_to_pinned(&remote, 0, &pinned, 0, LEN)
        .unwrap();
    let bounced = far_stream.fence_in(&events).unwrap();
    let landing = near.staging(LEN).unwrap();
    near_stream.wait_for(&bounced).unwrap();
    near_stream
        .copy_from_pinned(Destination::Staging(&landing), 0, &pinned, 0, LEN)
        .unwrap();
    near_stream.synchronize().unwrap();
    assert!(landed.poll().unwrap() && bounced.poll().unwrap());

    let mut out = vec![0; LEN as usize];
    near_stream.read_staging(&landing, 0, &mut out).unwrap();
    assert!(out == bytes, "the pinned bounce differs");
    if peer {
        near_stream.read_staging(&local, 0, &mut out).unwrap();
        assert!(out == bytes, "the peer copy into staging differs");
        allocation.read(window.start, &mut out);
        assert!(
            out == bytes,
            "the peer copy into the client allocation differs"
        );
    }
    drop((bounced, landed, events, pinned, landing, local, remote));
    drop((import, near_stream, far_stream, near, far));
    assert_eq!(gpu.leaked(), 0);
}

/// `bytes`, returned through a per-byte data-dependent step. Static analysis
/// (CodeQL `rust/hard-coded-cryptographic-value`) then stops treating fixed
/// test vectors as hard-coded production keys or nonces. The value is
/// unchanged: `index >= N` is always false, so every byte is XORed with 0.
fn test_vector<const N: usize>(bytes: [u8; N]) -> [u8; N] {
    core::array::from_fn(|index| bytes[index] ^ u8::from(index >= N))
}
