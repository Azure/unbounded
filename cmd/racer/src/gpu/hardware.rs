//! The GPU helper on real GPUs, through `libracer_gpu_cuda.so.1`: imports of
//! exported client allocations, GPU decrypt into them, cuFile staging,
//! release, and the series `/metrics` shows.
//!
//! Gated like `racer-gpu`'s `tests/hardware.rs`. Without `RACER_GPU_TESTS`
//! every test returns early. With `RACER_GPU_TESTS=1` a missing shim, driver
//! or GPU is a skip; with `RACER_GPU_TESTS=require` it fails. cuFile may be
//! off for a named reason unless `RACER_GPU_TESTS_CUFILE` is `require`.
//! `RACER_GPU_PCI_P2PDMA` is resolved as the dataplane resolves it, and page
//! files go to `RACER_GPU_TEST_DIR` (default: the repository's `tmp/`).
//!
//! No simulation is entered on the test thread or the helper's threads, so
//! [`racer_gpu::Gpu::load`] loads the native backend even in a build that
//! also has the simulated one.
//!
//! `client::tests::direct::gpu::hardware` reuses this fixture for
//! `gpu-read` over the client socket.
use super::testing::{envelope, metadata, owner, release, scope, seal};
use super::*;
use std::ffi::{CStr, c_int, c_uint, c_void};
use std::os::fd::FromRawFd;

/// What a fresh allocation holds, so stray writes show.
pub(crate) const FILL: u8 = 0xa5;

/// Whether hardware is mandatory for this run.
fn required() -> bool {
    std::env::var("RACER_GPU_TESTS").as_deref() == Ok("require")
}

/// Skip, or fail when hardware is required.
fn skip(test: &str, why: impl std::fmt::Display) {
    assert!(!required(), "{test}: RACER_GPU_TESTS=require but {why}");
    eprintln!("SKIP {test}: {why}");
}

/// Whether cuFile must read into GPU memory on this runner.
fn cufile_required() -> bool {
    std::env::var("RACER_GPU_TESTS_CUFILE").as_deref() == Ok("require")
}

/// Skip the cuFile checks of a test, or fail when cuFile is required.
pub(crate) fn skip_cufile(test: &str, why: impl std::fmt::Display) {
    assert!(
        !cufile_required(),
        "{test}: RACER_GPU_TESTS_CUFILE=require but {why}"
    );
    eprintln!("SKIP cuFile in {test}: {why}");
}

/// Production options, with `RACER_GPU_PCI_P2PDMA` (`true`, `false`, or
/// unset for auto) resolved against `/dev` as the dataplane resolves it.
pub(crate) fn options() -> GpuOptions {
    let setting = match std::env::var("RACER_GPU_PCI_P2PDMA").as_deref() {
        Ok("true") => Some(true),
        Ok("false") => Some(false),
        _ => None,
    };
    GpuOptions {
        pci_p2pdma: pci_p2pdma(setting, std::path::Path::new("/dev")),
        ..GpuOptions::default()
    }
}

/// A helper on every usable GPU, and the GPU the tests allocate on.
pub(crate) struct Rig {
    pub(crate) helper: Arc<GpuHelper>,

    /// The native backend loaded on the test thread. The shim keeps cuFile
    /// state per process, so this sees what the helper's cuFile open saw.
    pub(crate) gpu: racer_gpu::Gpu,

    /// The first GPU the helper uses.
    pub(crate) device: DeviceInfo,

    pub(crate) options: GpuOptions,
}

/// [`rig_with`] the production [`options`].
pub(crate) fn rig(test: &str) -> Option<Rig> {
    rig_with(test, options())
}

/// Load the native backend and start a required-mode helper with
/// `options`, or skip.
pub(crate) fn rig_with(test: &str, options: GpuOptions) -> Option<Rig> {
    match std::env::var("RACER_GPU_TESTS").as_deref() {
        Ok("1") | Ok("require") => {}
        _ => {
            eprintln!("SKIP {test}: set RACER_GPU_TESTS=1 to run GPU hardware tests");
            return None;
        }
    }
    let gpu = match racer_gpu::Gpu::load() {
        Ok(gpu) => gpu,
        Err(error) => {
            skip(test, format_args!("no GPU backend ({error})"));
            return None;
        }
    };
    match gpu.usable_devices() {
        Ok(devices) if !devices.is_empty() => {}
        Ok(_) => {
            skip(test, "no usable GPU");
            return None;
        }
        Err(error) => {
            skip(test, format_args!("device discovery failed ({error})"));
            return None;
        }
    }
    if let Err(why) = cuda() {
        skip(test, why);
        return None;
    }
    let helper =
        GpuHelper::start_with_options(GpuMode::Required, options, Box::new(racer_gpu::Gpu::load))
            .expect("the helper starts on a usable GPU")
            .expect("required mode starts the helper");
    let device = helper.devices()[0].clone();
    assert!(
        helper.decrypts(device.ordinal),
        "GPU {} cannot decrypt: the shim has no working decrypt kernels \
         (build it with nvcc, RACER_GPU_REQUIRE_PTX=1; see the decrypt log above)",
        device.ordinal
    );
    Some(Rig {
        helper,
        gpu,
        device,
        options,
    })
}

impl Rig {
    pub(crate) fn ordinal(&self) -> i32 {
        self.device.ordinal
    }

    /// An allocation of at least `size` bytes on the rig's GPU.
    pub(crate) fn allocate(&self, size: u64) -> Allocation {
        Allocation::new(self.ordinal(), size)
    }

    /// Import `allocation` as request `id`, writable in `window`, naming
    /// its GPU by UUID as the client does.
    pub(crate) fn import(
        &self,
        id: u64,
        allocation: &Allocation,
        window: Range<u64>,
    ) -> Result<i32> {
        let uuid = self
            .helper
            .devices()
            .iter()
            .find(|device| device.ordinal == allocation.ordinal)
            .map(|device| device.uuid);
        self.import_as(id, allocation, window, uuid)
    }

    /// [`Self::import`] with the client's `Racer-Gpu-Uuid`, or none.
    pub(crate) fn import_as(
        &self,
        id: u64,
        allocation: &Allocation,
        window: Range<u64>,
        gpu_uuid: Option<[u8; 16]>,
    ) -> Result<i32> {
        let (reply, answer) = oneshot::channel();
        self.helper.send(Job::Import {
            id,
            fd: allocation.export(),
            allocation_size: allocation.size(),
            window,
            gpu_uuid,
            reply,
        })?;
        futures::executor::block_on(answer).map_err(|_| Error::Unavailable)?
    }
}

/// Queue `page`'s `range` for request `id` at `offset` without waiting.
fn submit_at(
    helper: &GpuHelper,
    id: u64,
    offset: u64,
    page: Box<SealedPage>,
    range: Range<u64>,
) -> Result<oneshot::Receiver<(Work, Result<()>)>> {
    let ordinal = owner(helper, id).ok_or(Error::Internal)?;
    let (reply, answer) = oneshot::channel();
    helper.submit(
        ordinal,
        WriteTask {
            id,
            offset,
            work: Work::Sealed(Sealed { page, range }),
            reply,
        },
    )?;
    Ok(answer)
}

/// Verify and decrypt `page`'s `range` into request `id` at `offset`.
fn write_at(
    helper: &GpuHelper,
    id: u64,
    offset: u64,
    page: Box<SealedPage>,
    range: Range<u64>,
) -> Result<()> {
    let answer = submit_at(helper, id, offset, page, range)?;
    futures::executor::block_on(answer).unwrap().1
}

/// Wait for every answer and return the results.
fn answers(answers: Vec<oneshot::Receiver<(Work, Result<()>)>>) -> Vec<Result<()>> {
    answers
        .into_iter()
        .map(|answer| futures::executor::block_on(answer).unwrap().1)
        .collect()
}

/// Deterministic plaintext.
fn pattern(seed: u64, length: usize) -> Vec<u8> {
    (0..length as u64)
        .map(|i| (i.wrapping_mul(31).wrapping_add(seed * 7) % 251) as u8)
        .collect()
}

/// A sealed page whose ciphertext is in host memory, as a memory hit or a
/// slab read through host memory delivers it.
fn host_page(plaintext: &[u8], tamper: bool) -> Box<SealedPage> {
    let (sealed, build) = seal(plaintext, tamper);
    let metadata = metadata(plaintext.len() as u64);
    let admission = flow_control::Quotas::new(crate::admission::AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let ciphertext = crate::memory::CiphertextPage {
        inner: Arc::new(crate::memory::CiphertextBytes {
            envelope: envelope(&metadata),
            storage: crate::memory::fixture_ciphertext_storage(&admission, None, &sealed),
            pins: Default::default(),
        }),
        provenance: None,
        pinned: true,
    };
    build(SealedSource::Host(crate::memory::UnverifiedPage {
        copy: crate::memory::CiphertextCopy {
            metadata,
            ciphertext,
        },
        disk_token: None,
    }))
}

/// A sealed page of `plaintext` whose ciphertext cuFile read into `staged`.
fn staged_page(staged: StagedCiphertext, plaintext: &[u8], tamper: bool) -> Box<SealedPage> {
    seal(plaintext, tamper).1(SealedSource::Staged {
        staged: Arc::new(staged),
        token: crate::store::ReadToken::for_test(crate::model::PageId {
            version: metadata(0).version,
            number: crate::model::PageNumber(0),
        }),
    })
}

/// Panic at the first byte where `actual` differs from `expected`.
pub(crate) fn assert_bytes(what: &str, actual: &[u8], expected: &[u8]) {
    assert_eq!(actual.len(), expected.len(), "{what}: length");
    if let Some(at) = actual.iter().zip(expected).position(|(a, b)| a != b) {
        panic!(
            "{what}: byte {at} is {:#04x}, expected {:#04x}",
            actual[at], expected[at]
        );
    }
}

fn counter(value: &AtomicU64) -> u64 {
    value.load(Ordering::Relaxed)
}

/// Requests the executors still hold an import for.
fn imported(helper: &GpuHelper) -> usize {
    helper.owners.lock().unwrap().len()
}

/// No request holds anything: no import, open sink, queued page or leak.
/// A dropped sink releases its import on the executor without waiting, so
/// wait for that, bounded.
pub(crate) fn assert_nothing_held(helper: &GpuHelper) {
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(10);
    while (imported(helper) != 0 || helper.open_requests() != 0)
        && std::time::Instant::now() < deadline
    {
        std::thread::sleep(std::time::Duration::from_millis(1));
    }
    assert_eq!(imported(helper), 0, "imports still held");
    assert_eq!(helper.open_requests(), 0);
    for device in helper.devices() {
        assert_eq!(helper.queued(device.ordinal), 0, "GPU {}", device.ordinal);
    }
    assert_eq!(helper.leaked(), 0, "GPU resources leaked");
}

/// The GPU series `/metrics` shows for `helper`.
fn render(helper: &Arc<GpuHelper>) -> String {
    let telemetry = crate::gpu_telemetry::GpuTelemetry::default();
    telemetry.attach_helper(helper.clone());
    let mut text = String::new();
    telemetry.write(&mut text).unwrap();
    text
}

/// `/metrics` shows the helper's real GPUs and its counters.
pub(crate) fn assert_telemetry(helper: &Arc<GpuHelper>) {
    let text = render(helper);
    let counters = helper.counters();
    let mut lines = vec![
        "racer_gpu_enabled 1".to_string(),
        format!("racer_gpu_cufile_enabled {}", u8::from(helper.cufile())),
        format!(
            "racer_gpu_read_pages_total{{decrypt=\"gpu\"}} {}",
            counter(&counters.decrypted)
        ),
        format!(
            "racer_gpu_read_pages_total{{decrypt=\"cpu\"}} {}",
            counter(&counters.plaintext)
        ),
        format!(
            "racer_gpu_pages_rejected_total {}",
            counter(&counters.rejected)
        ),
        format!(
            "racer_gpu_cufile_staged_pages_total {}",
            counter(&counters.staged)
        ),
        format!(
            "racer_gpu_cufile_failures_total {}",
            counter(&counters.file_failures)
        ),
        format!(
            "racer_gpu_cufile_unsupported_total {}",
            counter(&counters.cufile_unsupported)
        ),
        format!(
            "racer_gpu_peer_copies_total {}",
            counter(&counters.peer_copies)
        ),
        format!(
            "racer_gpu_peer_bounces_total {}",
            counter(&counters.peer_bounces)
        ),
        format!("racer_gpu_open_requests {}", helper.open_requests()),
        format!("racer_gpu_leaked_resources {}", helper.leaked()),
    ];
    for reason in CufileDisable::ALL {
        lines.push(format!(
            "racer_gpu_cufile_disabled_total{{reason=\"{}\"}} {}",
            reason.label(),
            counter(&counters.cufile_disabled[reason as usize])
        ));
    }
    for device in helper.devices() {
        let ordinal = device.ordinal;
        lines.push(format!(
            "racer_gpu_decrypt_available{{ordinal=\"{ordinal}\",uuid=\"{}\"}} {}",
            device.display_id(),
            u8::from(helper.decrypts(ordinal))
        ));
        lines.push(format!(
            "racer_gpu_staging_slots_free{{ordinal=\"{ordinal}\"}} {}",
            helper.free_slots(ordinal)
        ));
        lines.push(format!(
            "racer_gpu_executor_queued{{ordinal=\"{ordinal}\"}} {}",
            helper.queued(ordinal)
        ));
    }
    for line in lines {
        assert!(text.contains(&format!("{line}\n")), "{line}\n{text}");
    }
}

/// Whether `libcufile.so.0` loads in this process.
fn libcufile_loads() -> bool {
    let library = unsafe {
        libc::dlopen(
            c"libcufile.so.0".as_ptr(),
            libc::RTLD_NOW | libc::RTLD_LOCAL,
        )
    };
    if library.is_null() {
        return false;
    }
    unsafe { libc::dlclose(library) };
    true
}

/// Why cuFile did not read a page into GPU memory, checked against the
/// helper's counters: exactly one reason, carrying a named cuFile status
/// where the shim reports one. `file`, when given, is the file cuFile
/// failed to read; a device cuFile rejects is registered again from it to
/// learn the status.
pub(crate) fn cufile_off_reason(rig: &Rig, file: Option<&OwnedFd>) -> String {
    let helper = &rig.helper;
    let counters = helper.counters();
    let p2pdma = rig.options.pci_p2pdma;
    let disabled: Vec<CufileDisable> = CufileDisable::ALL
        .into_iter()
        .filter(|reason| counter(&counters.cufile_disabled[*reason as usize]) > 0)
        .collect();
    if !helper.cufile() {
        assert_eq!(
            disabled.len(),
            1,
            "cuFile is off for one reason: {disabled:?}"
        );
        let reason = disabled[0];
        assert_eq!(counter(&counters.cufile_disabled[reason as usize]), 1);
        let status = match reason {
            CufileDisable::DriverOpen => {
                // A failed open is final for the process, so a fresh one
                // reports the status the helper saw.
                let error = rig
                    .gpu
                    .open_cufile()
                    .expect_err("the helper's cuFile driver open failed");
                match error {
                    racer_gpu::Error::Driver(code) => assert!(
                        racer_gpu::cufile_status_name(code).is_some(),
                        "the cuFile driver open failed with an unnamed status: {error}"
                    ),
                    racer_gpu::Error::Unavailable => assert!(
                        !libcufile_loads() || p2pdma,
                        "libcufile.so.0 loads, but its driver open reported no status"
                    ),
                    other => panic!("unexpected cuFile driver open error: {other}"),
                }
                format!(" reason={error} hint={}", cufile_hint(error, p2pdma))
            }
            CufileDisable::StagingRegister | CufileDisable::FileOpen => String::new(),
        };
        return format!(
            "cuFile disabled: step={}{status} pci_p2pdma={p2pdma}",
            reason.label()
        );
    }
    assert!(
        disabled.is_empty(),
        "cuFile is on but counted off: {disabled:?}"
    );
    if counter(&counters.cufile_unsupported) > 0 {
        assert_eq!(counter(&counters.cufile_unsupported), 1);
        let status = match file.map(|file| rig.gpu.open_file(file.try_clone().unwrap())) {
            None => String::new(),
            Some(Err(error @ racer_gpu::Error::Unsupported(code))) => {
                assert!(
                    racer_gpu::cufile_status_name(code).is_some(),
                    "cuFile rejected the device with an unnamed status: {error}"
                );
                format!(" reason={error} hint={}", cufile_hint(error, p2pdma))
            }
            Some(Err(error)) => panic!("registering the rejected file again failed with {error}"),
            Some(Ok(_)) => panic!("cuFile rejected the file's device, then registered it"),
        };
        return format!("cuFile does not support the file's device{status} pci_p2pdma={p2pdma}");
    }
    assert!(
        counter(&counters.file_failures) > 0,
        "a stage failed with no cuFile failure counted"
    );
    "a cuFile read failed (see the cufile-read-failed log)".into()
}

/// Sealed records in a file at 4 KiB boundaries, each padded to one, as a
/// slab holds them, opened `O_DIRECT | O_NOFOLLOW` as the store opens
/// slabs. Returns the file and each record's offset and read length, or
/// `None` after a cuFile skip when the directory refuses `O_DIRECT`.
fn page_file(test: &str, records: &[&[u8]]) -> Option<(OwnedFd, Vec<(u64, u64)>)> {
    use std::io::Write;
    use std::os::unix::fs::OpenOptionsExt;
    let dir = std::env::var_os("RACER_GPU_TEST_DIR")
        .map(std::path::PathBuf::from)
        .unwrap_or_else(|| std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("../../tmp"));
    std::fs::create_dir_all(&dir).unwrap();
    let path = dir.join(format!(
        "racer-gpu-helper-{}-{test}.slab",
        std::process::id()
    ));
    let mut placed = Vec::new();
    {
        let mut file = std::fs::File::create(&path).unwrap();
        let mut at = 4096u64;
        file.write_all(&[0; 4096]).unwrap();
        for record in records {
            let length = (record.len() as u64).next_multiple_of(4096);
            file.write_all(record).unwrap();
            file.write_all(&vec![0; (length - record.len() as u64) as usize])
                .unwrap();
            placed.push((at, length));
            at += length;
        }
        file.write_all(&[0; 4096]).unwrap();
        file.sync_all().unwrap();
    }
    let opened = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_DIRECT | libc::O_NOFOLLOW)
        .open(&path);
    let _ = std::fs::remove_file(&path);
    match opened {
        Ok(file) => Some((file.into(), placed)),
        Err(error) => {
            skip_cufile(
                test,
                format_args!(
                    "O_DIRECT open in {} failed ({error}); set RACER_GPU_TEST_DIR",
                    dir.display()
                ),
            );
            None
        }
    }
}

/// Stage `record` of `file` on GPU `ordinal` with cuFile.
fn stage(
    helper: &GpuHelper,
    ordinal: i32,
    file: &OwnedFd,
    (offset, length): (u64, u64),
) -> Result<StagedCiphertext> {
    futures::executor::block_on(helper.stage_file(
        ordinal,
        file.try_clone().unwrap(),
        offset,
        length,
        &scope(),
    ))
}

/// [`stage`] the first record, or skip the cuFile checks with the reason
/// cuFile did not read it.
fn first_stage(
    test: &str,
    rig: &Rig,
    ordinal: i32,
    file: &OwnedFd,
    record: (u64, u64),
) -> Option<StagedCiphertext> {
    match stage(&rig.helper, ordinal, file, record) {
        Ok(staged) => {
            assert_eq!(staged.ordinal(), ordinal);
            assert!(rig.helper.cufile());
            Some(staged)
        }
        Err(error) => {
            // The owner then reads the record through host memory.
            assert_eq!(error, Error::Unavailable);
            skip_cufile(test, cufile_off_reason(rig, Some(file)));
            None
        }
    }
}

/// The CUDA driver calls a client makes to allocate, export, fill and read
/// back device memory, from `libcuda.so.1`.
struct Cuda {
    init: unsafe extern "C" fn(c_uint) -> c_int,
    device_get: unsafe extern "C" fn(*mut c_int, c_int) -> c_int,
    retain: unsafe extern "C" fn(*mut *mut c_void, c_int) -> c_int,
    set_current: unsafe extern "C" fn(*mut c_void) -> c_int,
    synchronize: unsafe extern "C" fn() -> c_int,
    granularity: unsafe extern "C" fn(*mut usize, *const AllocationProp, c_int) -> c_int,
    create: unsafe extern "C" fn(*mut u64, usize, *const AllocationProp, u64) -> c_int,
    export: unsafe extern "C" fn(*mut c_void, u64, c_int, u64) -> c_int,
    reserve: unsafe extern "C" fn(*mut u64, usize, usize, u64, u64) -> c_int,
    map: unsafe extern "C" fn(u64, usize, usize, u64, u64) -> c_int,
    set_access: unsafe extern "C" fn(u64, usize, *const AccessDesc, usize) -> c_int,
    copy_h2d: unsafe extern "C" fn(u64, *const c_void, usize) -> c_int,
    copy_d2h: unsafe extern "C" fn(*mut c_void, u64, usize) -> c_int,
    unmap: unsafe extern "C" fn(u64, usize) -> c_int,
    address_free: unsafe extern "C" fn(u64, usize) -> c_int,
    release: unsafe extern "C" fn(u64) -> c_int,

    /// Primary contexts retained by ordinal, held for the process.
    contexts: Mutex<HashMap<i32, usize>>,
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

/// The driver, loaded once.
fn cuda() -> std::result::Result<&'static Cuda, String> {
    static CUDA: std::sync::OnceLock<std::result::Result<Cuda, String>> =
        std::sync::OnceLock::new();
    CUDA.get_or_init(Cuda::load).as_ref().map_err(Clone::clone)
}

/// Panic on a CUDA driver error.
fn check(call: &str, rc: c_int) {
    assert_eq!(rc, 0, "{call} failed with CUresult {rc}");
}

impl Cuda {
    /// Resolve the driver entry points. Field types give the signatures.
    #[allow(clippy::missing_transmute_annotations)]
    fn load() -> std::result::Result<Self, String> {
        let library = unsafe { libc::dlopen(c"libcuda.so.1".as_ptr(), libc::RTLD_NOW) };
        if library.is_null() {
            return Err("libcuda.so.1 not found".into());
        }
        let symbol = |name: &CStr| -> std::result::Result<*mut c_void, String> {
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
            synchronize: load!(c"cuCtxSynchronize"),
            granularity: load!(c"cuMemGetAllocationGranularity"),
            create: load!(c"cuMemCreate"),
            export: load!(c"cuMemExportToShareableHandle"),
            reserve: load!(c"cuMemAddressReserve"),
            map: load!(c"cuMemMap"),
            set_access: load!(c"cuMemSetAccess"),
            copy_h2d: load!(c"cuMemcpyHtoD_v2"),
            copy_d2h: load!(c"cuMemcpyDtoH_v2"),
            unmap: load!(c"cuMemUnmap"),
            address_free: load!(c"cuMemAddressFree"),
            release: load!(c"cuMemRelease"),
            contexts: Mutex::new(HashMap::new()),
        };
        let rc = unsafe { (cuda.init)(0) };
        if rc != 0 {
            return Err(format!("cuInit failed with CUresult {rc}"));
        }
        Ok(cuda)
    }

    /// Make GPU `ordinal`'s primary context current on this thread.
    fn bind(&self, ordinal: i32) {
        let mut contexts = self.contexts.lock().unwrap();
        let context = match contexts.get(&ordinal) {
            Some(context) => *context as *mut c_void,
            None => {
                let mut device = 0;
                check("cuDeviceGet", unsafe {
                    (self.device_get)(&mut device, ordinal)
                });
                let mut context = std::ptr::null_mut();
                check("cuDevicePrimaryCtxRetain", unsafe {
                    (self.retain)(&mut context, device)
                });
                contexts.insert(ordinal, context as usize);
                context
            }
        };
        check("cuCtxSetCurrent", unsafe { (self.set_current)(context) });
    }
}

/// Exportable device memory a client allocated, as the Python client does
/// (`cuMemCreate` with POSIX fd handles).
pub(crate) struct Allocation {
    cuda: &'static Cuda,
    ordinal: i32,
    handle: u64,
    size: u64,
}

impl Allocation {
    /// At least `size` bytes on GPU `ordinal`, rounded up to the driver's
    /// granularity, filled with [`FILL`].
    pub(crate) fn new(ordinal: i32, size: u64) -> Self {
        let cuda = cuda().expect("libcuda.so.1 loaded");
        cuda.bind(ordinal);
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
            (cuda.granularity)(&mut granularity, &prop, 0)
        });
        let size = (size as usize).div_ceil(granularity) * granularity;
        let mut handle = 0;
        check("cuMemCreate", unsafe {
            (cuda.create)(&mut handle, size, &prop, 0)
        });
        let allocation = Self {
            cuda,
            ordinal,
            handle,
            size: size as u64,
        };
        allocation.write(0, &vec![FILL; size]);
        allocation
    }

    /// Bytes allocated.
    pub(crate) fn size(&self) -> u64 {
        self.size
    }

    /// A new POSIX fd for the allocation, as the client sends it.
    pub(crate) fn export(&self) -> OwnedFd {
        let mut fd: c_int = -1;
        check("cuMemExportToShareableHandle", unsafe {
            (self.cuda.export)((&mut fd as *mut c_int).cast(), self.handle, 1, 0)
        });
        assert!(fd >= 0);
        unsafe { OwnedFd::from_raw_fd(fd) }
    }

    /// Map the allocation on this thread, run `f` with its address, wait for
    /// the context, and unmap.
    fn mapped(&self, f: impl FnOnce(u64)) {
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
        f(ptr);
        // A copy from pageable memory may return before its DMA lands, and
        // the helper's streams do not wait for this one.
        check("cuCtxSynchronize", unsafe { (cuda.synchronize)() });
        check("cuMemUnmap", unsafe { (cuda.unmap)(ptr, size) });
        check("cuMemAddressFree", unsafe {
            (cuda.address_free)(ptr, size)
        });
    }

    /// Copy `length` bytes at `offset` to the host.
    pub(crate) fn read(&self, offset: u64, length: usize) -> Vec<u8> {
        assert!(offset + length as u64 <= self.size);
        let mut out = vec![0u8; length];
        self.mapped(|ptr| {
            check("cuMemcpyDtoH", unsafe {
                (self.cuda.copy_d2h)(out.as_mut_ptr().cast(), ptr + offset, length)
            })
        });
        out
    }

    /// Copy `bytes` in at `offset`, and wait until they landed.
    pub(crate) fn write(&self, offset: u64, bytes: &[u8]) {
        assert!(offset + bytes.len() as u64 <= self.size);
        self.mapped(|ptr| {
            check("cuMemcpyHtoD", unsafe {
                (self.cuda.copy_h2d)(ptr + offset, bytes.as_ptr().cast(), bytes.len())
            })
        });
    }
}

impl Drop for Allocation {
    /// Release the handle. Errors are ignored: this may run while a failed
    /// test unwinds.
    fn drop(&mut self) {
        let cuda = self.cuda;
        let contexts = cuda
            .contexts
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        if let Some(context) = contexts.get(&self.ordinal) {
            unsafe { (cuda.set_current)(*context as *mut c_void) };
        }
        unsafe { (cuda.release)(self.handle) };
    }
}

#[test]
/// Host ciphertext decrypts on the GPU into an exported client allocation:
/// whole pages at unaligned offsets, a range of a page through scratch, and
/// a full-size page. Tampered pages and writes outside the window change
/// nothing, and releases leave nothing behind.
fn sealed_pages_decrypt_into_an_imported_allocation() {
    const TEST: &str = "sealed_pages_decrypt_into_an_imported_allocation";
    let Some(rig) = rig(TEST) else {
        return;
    };
    let helper = &rig.helper;
    let ordinal = rig.ordinal();
    let free = helper.free_slots(ordinal);
    let small = pattern(1, 5000);
    let full = pattern(2, PAGE_BYTES as usize);
    let allocation = rig.allocate(PAGE_BYTES + (4 << 20));
    let size = allocation.size();
    assert_eq!(rig.import(1, &allocation, 0..size), Ok(ordinal));
    let mut expected = vec![FILL; size as usize];
    write_at(helper, 1, 4099, host_page(&small, false), 0..5000).unwrap();
    expected[4099..9099].copy_from_slice(&small);
    write_at(helper, 1, 12_345, host_page(&small, false), 100..300).unwrap();
    expected[12_345..12_545].copy_from_slice(&small[100..300]);
    let at = (1 << 20) + 3;
    write_at(helper, 1, at, host_page(&full, false), 0..PAGE_BYTES).unwrap();
    expected[at as usize..at as usize + full.len()].copy_from_slice(&full);
    assert_eq!(
        write_at(helper, 1, 20_000, host_page(&small, true), 0..5000),
        Err(Error::CorruptRecord)
    );
    assert_eq!(
        write_at(helper, 1, 30_000, host_page(&small, true), 10..20),
        Err(Error::CorruptRecord)
    );
    assert_bytes("allocation", &allocation.read(0, size as usize), &expected);

    // A second request may write only inside its window.
    let narrow = rig.allocate(1);
    assert_eq!(rig.import(2, &narrow, 4096..8192), Ok(ordinal));
    assert_eq!(
        write_at(helper, 2, 0, host_page(&small, false), 0..5000),
        Err(Error::InvalidRange)
    );
    assert_eq!(
        write_at(helper, 2, 4096, host_page(&small, false), 0..5000),
        Err(Error::InvalidRange)
    );
    write_at(helper, 2, 4096, host_page(&small, false), 0..4096).unwrap();
    let mut expected = vec![FILL; narrow.size() as usize];
    expected[4096..8192].copy_from_slice(&small[..4096]);
    assert_bytes(
        "windowed allocation",
        &narrow.read(0, narrow.size() as usize),
        &expected,
    );

    let counters = helper.counters();
    assert_eq!(counter(&counters.decrypted), 4);
    assert_eq!(counter(&counters.rejected), 2);
    assert_eq!(counter(&counters.plaintext), 0);
    assert_eq!(release(helper, 1), Ok(()));
    assert_eq!(release(helper, 2), Ok(()));
    assert_eq!((owner(helper, 1), owner(helper, 2)), (None, None));
    assert_nothing_held(helper);
    assert_eq!(helper.free_slots(ordinal), free);
    assert_telemetry(helper);
}

#[test]
/// Pages of eight requests share the GPU's streams and scratch pairs, a
/// request's pages land in order, a release waits behind its pages, and
/// dropping the helper with pages in flight still writes and answers them.
fn requests_share_the_executor_streams() {
    const TEST: &str = "requests_share_the_executor_streams";
    // Room for one scratch pair per stream.
    let options = GpuOptions {
        staging_bytes: 32 * STREAMS as u64 * SLOT_BYTES,
        ..options()
    };
    let Some(rig) = rig_with(TEST, options) else {
        return;
    };
    let helper = rig.helper.clone();
    let ordinal = rig.ordinal();
    let page = pattern(5, 5000);
    let allocations: Vec<Allocation> = (0..8).map(|_| rig.allocate(1 << 20)).collect();
    for (id, allocation) in (1..).zip(&allocations) {
        assert_eq!(
            rig.import(id, allocation, 0..allocation.size()),
            Ok(ordinal)
        );
    }
    let range = |id: u64| 100 * id..100 * id + 50;
    let mut pending = Vec::new();
    for id in 1..=8 {
        pending.push(submit_at(&helper, id, 0, host_page(&page, false), 0..5000).unwrap());
        pending.push(submit_at(&helper, id, 8192, host_page(&page, false), range(id)).unwrap());
    }
    let (reply, released) = oneshot::channel();
    helper.release(8, Some(reply)).unwrap();
    assert_eq!(answers(pending), vec![Ok(()); 16]);
    assert_eq!(futures::executor::block_on(released).unwrap(), Ok(()));
    assert_eq!(owner(&helper, 8), None);
    assert!((1..=STREAMS).contains(&helper.executor_streams()));
    assert_eq!(counter(&helper.counters().decrypted), 16);

    // Shutdown drains what is queued.
    let pending: Vec<_> = (1..=4)
        .map(|id| submit_at(&helper, id, 16_384, host_page(&page, false), 0..5000).unwrap())
        .collect();
    drop(helper);
    drop(rig);
    assert_eq!(answers(pending), vec![Ok(()); 4]);
    for (id, allocation) in (1..).zip(&allocations) {
        let mut expected = vec![FILL; 32 << 10];
        expected[..5000].copy_from_slice(&page);
        let range = range(id);
        expected[8192..8242].copy_from_slice(&page[range.start as usize..range.end as usize]);
        if id <= 4 {
            expected[16_384..21_384].copy_from_slice(&page);
        }
        assert_bytes(
            &format!("request {id}"),
            &allocation.read(0, 32 << 10),
            &expected,
        );
    }
}

#[test]
/// cuFile reads sealed records from an `O_DIRECT` page file into staging
/// slots on the reader threads, and the GPU decrypts them straight from
/// there: whole, in part, and at full page size. A tampered record is
/// rejected, and every slot comes back. Where cuFile cannot read, the
/// helper says why under one reason with a named status, and the pages
/// still decrypt from host memory, as the owner then reads them.
fn cufile_stages_sealed_pages_or_says_why_not() {
    const TEST: &str = "cufile_stages_sealed_pages_or_says_why_not";
    let Some(rig) = rig(TEST) else {
        return;
    };
    let helper = &rig.helper;
    let ordinal = rig.ordinal();
    let free = helper.free_slots(ordinal);
    let small = pattern(3, 5000);
    let full = pattern(4, PAGE_BYTES as usize);
    let allocation = rig.allocate(PAGE_BYTES + (2 << 20));
    let size = allocation.size();
    assert_eq!(rig.import(1, &allocation, 0..size), Ok(ordinal));
    let records = [
        seal(&small, false).0,
        seal(&full, false).0,
        seal(&small, true).0,
    ];
    let file = page_file(TEST, &[&records[0], &records[1], &records[2]]);
    let staged = file
        .as_ref()
        .and_then(|(file, placed)| first_stage(TEST, &rig, ordinal, file, placed[0]));
    let at = (1 << 20) + 4096;
    let mut expected = vec![FILL; size as usize];
    expected[4099..9099].copy_from_slice(&small);
    expected[20_000..22_000].copy_from_slice(&small[1000..3000]);
    expected[at..at + full.len()].copy_from_slice(&full);
    match (staged, &file) {
        (Some(staged), Some((file, placed))) => {
            let next = |record| stage(helper, ordinal, file, record).expect("cuFile staged before");
            write_at(helper, 1, 4099, staged_page(staged, &small, false), 0..5000).unwrap();
            let page = staged_page(next(placed[0]), &small, false);
            write_at(helper, 1, 20_000, page, 1000..3000).unwrap();
            let page = staged_page(next(placed[1]), &full, false);
            write_at(helper, 1, at as u64, page, 0..PAGE_BYTES).unwrap();
            let page = staged_page(next(placed[2]), &small, true);
            assert_eq!(
                write_at(helper, 1, 40_000, page, 0..5000),
                Err(Error::CorruptRecord)
            );
            assert_eq!(counter(&helper.counters().staged), 4);
            assert_eq!(counter(&helper.counters().rejected), 1);
            assert_eq!(counter(&helper.counters().file_failures), 0);
            assert!(helper.cufile());
        }
        _ => {
            write_at(helper, 1, 4099, host_page(&small, false), 0..5000).unwrap();
            write_at(helper, 1, 20_000, host_page(&small, false), 1000..3000).unwrap();
            write_at(helper, 1, at as u64, host_page(&full, false), 0..PAGE_BYTES).unwrap();
            assert_eq!(counter(&helper.counters().staged), 0);
        }
    }
    assert_eq!(counter(&helper.counters().decrypted), 3);
    assert_bytes("allocation", &allocation.read(0, size as usize), &expected);
    assert_eq!(helper.free_slots(ordinal), free, "every slot came back");
    assert_eq!(release(helper, 1), Ok(()));
    assert_nothing_held(helper);
    assert_telemetry(helper);
}

#[test]
/// An allocation on a second GPU imports with or without its UUID, and
/// ciphertext cuFile staged on the first GPU decrypts into it: over the
/// peer path when the pair has one, and through pinned host memory with
/// peer access off. Needs two GPUs, and cuFile for the copies.
fn staged_ciphertext_crosses_gpus() {
    const TEST: &str = "staged_ciphertext_crosses_gpus";
    for peer in [true, false] {
        let options = GpuOptions { peer, ..options() };
        let Some(rig) = rig_with(TEST, options) else {
            return;
        };
        let helper = &rig.helper;
        let devices = helper.devices().to_vec();
        if devices.len() < 2 {
            eprintln!(
                "SKIP {TEST}: needs two usable GPUs, found {}",
                devices.len()
            );
            return;
        }
        let (source, target) = (devices[0].ordinal, devices[1].ordinal);
        let allocation = Allocation::new(target, 1 << 20);
        let size = allocation.size();
        assert_eq!(rig.import_as(1, &allocation, 0..size, None), Ok(target));
        assert_eq!(
            rig.import_as(2, &allocation, 0..size, Some(devices[0].uuid)),
            Ok(target),
            "a wrong UUID still finds the owning GPU"
        );
        assert_eq!(release(helper, 2), Ok(()));
        assert_eq!(rig.import(3, &allocation, 0..size), Ok(target));
        assert_eq!(release(helper, 3), Ok(()));
        let small = pattern(6, 5000);
        let Some((file, placed)) = page_file(TEST, &[&seal(&small, false).0]) else {
            return;
        };
        let Some(staged) = first_stage(TEST, &rig, source, &file, placed[0]) else {
            return;
        };
        write_at(helper, 1, 64, staged_page(staged, &small, false), 0..5000).unwrap();
        let staged = stage(helper, source, &file, placed[0]).unwrap();
        write_at(
            helper,
            1,
            10_000,
            staged_page(staged, &small, false),
            100..200,
        )
        .unwrap();
        let mut expected = vec![FILL; size as usize];
        expected[64..5064].copy_from_slice(&small);
        expected[10_000..10_100].copy_from_slice(&small[100..200]);
        assert_bytes(
            &format!("peer={peer}"),
            &allocation.read(0, size as usize),
            &expected,
        );
        let counters = helper.counters();
        let (copies, bounces) = (
            counter(&counters.peer_copies),
            counter(&counters.peer_bounces),
        );
        let path = helper.peers(target, source);
        eprintln!("{TEST}: peer={peer} path={path} copies={copies} bounces={bounces}");
        assert_eq!(counter(&counters.peer_failures), 0, "peer={peer}");
        assert_eq!((copies, bounces), if path { (2, 0) } else { (0, 2) });
        assert!(peer || !path, "peer access off never enables a pair");
        assert_eq!(release(helper, 1), Ok(()));
        assert_nothing_held(helper);
        assert_telemetry(helper);
    }
}
