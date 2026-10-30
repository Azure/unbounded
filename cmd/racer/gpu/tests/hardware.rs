//! Tests on a real GPU through `libracer_gpu_cuda.so.1`.
//!
//! Opt in with `RACER_GPU_TESTS=1`; `make racer-gpu-test` builds the shim
//! with its kernels and runs these. Without the variable every test returns
//! early, so the normal CI build compiles them and they pass as no-ops. With
//! `RACER_GPU_TESTS=1` a missing shim, driver or GPU is reported as a skip;
//! with `RACER_GPU_TESTS=require` it fails instead, for dedicated GPU runners.
//! cuFile tests skip when cuFile cannot open unless `RACER_GPU_TESTS_CUFILE`
//! is `require`.
#![cfg(all(feature = "native", target_os = "linux"))]

use racer_gpu::{
    ComputeMode, Context, DecryptRequest, Destination, Error, Gpu, StagingBuffer, Verdict, flag,
};
use std::{
    ffi::{CStr, c_int, c_uint, c_void},
    fs::OpenOptions,
    io::Write,
    os::{
        fd::{FromRawFd, OwnedFd},
        unix::fs::OpenOptionsExt,
    },
};

/// Whether hardware is mandatory for this run.
fn required() -> bool {
    std::env::var("RACER_GPU_TESTS").as_deref() == Ok("require")
}

/// Skip, or fail when hardware is required.
fn skip(test: &str, why: impl std::fmt::Display) {
    assert!(!required(), "{test}: RACER_GPU_TESTS=require but {why}");
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
        let mut key = [0; 32];
        let mut nonce = [0; 24];
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
        expected_crc: Option<u64>,
    ) -> Verdict {
        let derived = racer_crypto::xaes_derive(&self.key, &self.nonce);
        let stream = context.stream().unwrap();
        let request = DecryptRequest {
            key: derived.key(),
            nonce: derived.nonce(),
            aad: &self.aad,
            length: self.plaintext.len() as u64,
            expected_crc,
        };
        match stream.decrypt(&request, src, 0, dst, dst_offset) {
            Err(Error::KernelUnavailable) => {
                panic!(
                    "the shim has no decrypt kernels: build it with nvcc (RACER_GPU_REQUIRE_PTX=1)"
                )
            }
            other => other.unwrap(),
        }
        stream.synchronize().unwrap();
        stream.verdict().unwrap()
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
            device.uuid_string(),
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

    let crc = racer_crypto::crc64(&page.sealed);
    let verdict = page.gpu_decrypt(
        &context,
        &staging,
        Destination::Import(&import),
        window.start,
        Some(crc),
    );
    assert_eq!(verdict, Verdict::Passed { crc });
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

        let crc = racer_crypto::crc64(&page.sealed);
        let verdict = page.gpu_decrypt(&context, &src, Destination::Staging(&dst), 0, Some(crc));
        assert_eq!(verdict, Verdict::Passed { crc }, "length {length}");
        let verdict = page.gpu_decrypt(&context, &src, Destination::Staging(&dst), 0, None);
        assert_eq!(verdict, Verdict::Passed { crc }, "length {length}");
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
            let verdict = page.gpu_decrypt(&context, &src, Destination::Staging(&dst), 0, None);
            assert!(
                matches!(verdict, Verdict::TagMismatch { .. }),
                "length {length}: {verdict:?}"
            );
            let verdict =
                page.gpu_decrypt(&context, &src, Destination::Staging(&dst), 0, Some(crc));
            assert!(
                matches!(verdict, Verdict::CrcMismatch { .. }),
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
/// and the GPU decrypts it to the CPU result.
fn cufile_reads_sealed_page_into_gpu() {
    const TEST: &str = "cufile_reads_sealed_page_into_gpu";
    let Some((gpu, ordinal)) = setup(TEST) else {
        return;
    };
    let cufile_required = std::env::var("RACER_GPU_TESTS_CUFILE").as_deref() == Ok("require");
    let skip_cufile = |why: String| {
        assert!(
            !cufile_required,
            "{TEST}: RACER_GPU_TESTS_CUFILE=require but {why}"
        );
        eprintln!("SKIP {TEST}: {why}");
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
            return skip_cufile(format!(
                "O_DIRECT open of {} failed ({err}); set RACER_GPU_TEST_DIR",
                dir.display()
            ));
        }
    };
    let gpu_file = match gpu.open_file(file) {
        Ok(file) => file,
        Err(err) => return skip_cufile(format!("cuFile is unavailable ({err})")),
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

    // A detached read on another thread lands the same bytes.
    let reader = gpu.file_reader();
    let other = context.staging(length).unwrap();
    let read = unsafe { gpu_file.detach(&other, 0, record_offset, length) }.unwrap();
    std::thread::scope(|scope| scope.spawn(|| reader.read(read)).join().unwrap()).unwrap();
    stream.read_staging(&other, 0, &mut readback).unwrap();
    assert!(
        readback == page.sealed,
        "detached cuFile read the wrong bytes"
    );

    let dst = context.staging(page.plaintext.len() as u64).unwrap();
    let crc = racer_crypto::crc64(&page.sealed);
    let verdict = page.gpu_decrypt(&context, &staging, Destination::Staging(&dst), 0, Some(crc));
    assert_eq!(verdict, Verdict::Passed { crc });
    let mut out = vec![0; page.plaintext.len()];
    stream.read_staging(&dst, 0, &mut out).unwrap();
    assert!(out == page.cpu_open());
    assert_eq!(
        gpu_file.read(&staging, 0, record_offset, length + 1),
        Err(Error::InvalidRange)
    );
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

    /// Allocate exportable device memory, as the Python client does.
    fn allocate(&self, ordinal: i32, size: usize) -> Allocation<'_> {
        self.bind(ordinal);
        let prop = AllocationProp {
            kind: 1,         // CU_MEM_ALLOCATION_TYPE_PINNED
            handle_types: 1, // CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR
            location_type: 1,
            location_id: ordinal,
            win32: std::ptr::null_mut(),
            flags: [0; 8],
        };
        let mut granularity = 0;
        check("cuMemGetAllocationGranularity", unsafe {
            (self.granularity)(&mut granularity, &prop, 0)
        });
        let size = size.div_ceil(granularity) * granularity;
        let mut handle = 0;
        check("cuMemCreate", unsafe {
            (self.create)(&mut handle, size, &prop, 0)
        });
        Allocation {
            cuda: self,
            ordinal,
            handle,
            size: size as u64,
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
