//! The C ABI of `libracer_gpu_cuda.so.1` (`native/gpu_cuda.h`) and its loader.
//!
//! Every safe type in `lib.rs` holds an `Rc<Shared>`, which holds the [`Api`]
//! symbol table, so the library stays loaded while any GPU object lives. The
//! simulation backend builds the same table from Rust functions.
use crate::{Error, Result};

use std::{
    cell::Cell,
    ffi::{c_int, c_void},
    rc::Rc,
};

pub(crate) use crate::gcm::RawVerdict;

/// `RACER_GPU_ABI_VERSION`.
#[cfg(all(feature = "native", target_os = "linux"))]
pub(crate) const ABI_VERSION: u32 = 1;

/// Result codes from `gpu_cuda.h`.
pub(crate) mod code {
    use std::ffi::c_int;

    pub const OK: c_int = 0;
    pub const EINVAL: c_int = -1;
    pub const ERANGE: c_int = -2;
    pub const EUNAVAILABLE: c_int = -3;
    pub const EDEVICE: c_int = -4;
    pub const EDRIVER: c_int = -5;
    pub const ENOMEM: c_int = -6;
    pub const ENOKERNEL: c_int = -7;
    pub const EIO: c_int = -8;
    pub const EBUSY: c_int = -9;
    pub const ELEAKED: c_int = -10;
}

/// Device capability bits (`RACER_GPU_FLAG_*`).
pub mod flag {
    /// Virtual memory management (import and map).
    pub const VMM: u32 = 1;
    /// POSIX fd shareable handles.
    pub const POSIX_FD: u32 = 2;
    /// dma-buf export.
    pub const DMA_BUF: u32 = 4;
    /// GPUDirect RDMA.
    pub const GDR: u32 = 8;
}

/// One device. Matches `struct racer_gpu_device`.
#[repr(C)]
#[derive(Clone, Copy)]
pub(crate) struct RawDevice {
    pub uuid: [u8; 16],

    pub pci_bus_id: [u8; 32],

    pub ordinal: i32,

    pub numa_node: i32,

    pub compute_mode: i32,

    pub flags: u32,

    pub total_memory: u64,
}

impl Default for RawDevice {
    /// Unknown NUMA node and compute mode.
    fn default() -> Self {
        Self {
            uuid: [0; 16],
            pci_bus_id: [0; 32],
            ordinal: 0,
            numa_node: -1,
            compute_mode: -1,
            flags: 0,
            total_memory: 0,
        }
    }
}

/// One decrypt request. Matches `struct racer_gpu_decrypt`. Holds a key, so
/// callers wipe it after the call.
#[repr(C)]
pub(crate) struct RawDecrypt {
    pub key: [u8; 32],

    pub nonce: [u8; 12],

    pub check_crc: u32,

    pub expected_crc: u64,

    pub length: u64,

    pub aad: *const u8,

    pub aad_length: u64,
}

impl Drop for RawDecrypt {
    /// Wipe the key and nonce.
    fn drop(&mut self) {
        use zeroize::Zeroize;
        self.key.zeroize();
        self.nonce.zeroize();
    }
}

/// `RACER_GPU_MAX_DECRYPT`.
pub const MAX_DECRYPT: u64 = 1 << 30;

/// `RACER_GPU_MAX_AAD`.
pub const MAX_AAD: usize = 4096;

/// Opaque handle out-parameter.
pub(crate) type Out = *mut *mut c_void;

/// Build [`Api`] and its symbol loader from `racer_gpu_<name>` functions.
macro_rules! native_api {
    ($($field:ident: $signature:ty),* $(,)?) => {
        /// The shim's functions.
        pub(crate) struct Api {
            /// `dlopen` handle; `None` for the simulation.
            #[cfg_attr(not(all(feature = "native", target_os = "linux")), allow(dead_code))]
            pub(crate) library: Option<std::ptr::NonNull<c_void>>,

            $(
                pub(crate) $field: $signature,

            )*
        }

        impl Api {
            #[cfg(all(feature = "native", target_os = "linux"))]
            /// Look up every function in `library`.
            unsafe fn symbols(library: std::ptr::NonNull<c_void>) -> Result<Self> {
                Ok(Self {
                    library: Some(library),
                    $($field: {
                        let pointer = unsafe { libc::dlsym(library.as_ptr(),
                            concat!("racer_gpu_", stringify!($field), "\0").as_ptr().cast()) };
                        if pointer.is_null() { return Err(Error::Unavailable); }
                        unsafe { std::mem::transmute::<*mut c_void, $signature>(pointer) }
                    },)*
                })
            }
        }
    };
}

native_api! {
    last_driver_error: unsafe extern "C" fn() -> i32,

    init: unsafe extern "C" fn() -> c_int,

    discover: unsafe extern "C" fn(*mut RawDevice, u32) -> c_int,

    context: unsafe extern "C" fn(i32, Out) -> c_int,

    context_free: unsafe extern "C" fn(*mut c_void) -> c_int,

    stream: unsafe extern "C" fn(*mut c_void, Out) -> c_int,

    stream_sync: unsafe extern "C" fn(*mut c_void) -> c_int,

    stream_free: unsafe extern "C" fn(*mut c_void) -> c_int,

    import: unsafe extern "C" fn(*mut c_void, c_int, u64, Out) -> c_int,

    import_free: unsafe extern "C" fn(*mut c_void) -> c_int,

    staging: unsafe extern "C" fn(*mut c_void, u64, Out) -> c_int,

    staging_free: unsafe extern "C" fn(*mut c_void) -> c_int,

    copy_h2d: unsafe extern "C" fn(*mut c_void, *mut c_void, u64, *const c_void, u64) -> c_int,

    copy_d2d: unsafe extern "C" fn(*mut c_void, *mut c_void, u64, *mut c_void, u64, u64) -> c_int,

    copy_d2h: unsafe extern "C" fn(*mut c_void, *mut c_void, *mut c_void, u64, u64) -> c_int,

    decrypt: unsafe extern "C" fn(
        *mut c_void,
        *const RawDecrypt,
        *mut c_void,
        u64,
        *mut c_void,
        u64,
    ) -> c_int,

    verdict: unsafe extern "C" fn(*mut c_void, *mut RawVerdict) -> c_int,

    file_open: unsafe extern "C" fn() -> c_int,

    file: unsafe extern "C" fn(c_int, Out) -> c_int,

    file_free: unsafe extern "C" fn(*mut c_void) -> c_int,

    staging_register: unsafe extern "C" fn(*mut c_void) -> c_int,

    staging_unregister: unsafe extern "C" fn(*mut c_void) -> c_int,

    file_read: unsafe extern "C" fn(*mut c_void, *mut c_void, u64, u64, u64) -> i64,

    dmabuf: unsafe extern "C" fn(*mut c_void, u64, u64, *mut c_int) -> c_int,
}

impl Api {
    #[cfg(not(all(feature = "native", target_os = "linux")))]
    /// No native backend in this build.
    pub(crate) fn load() -> Result<Rc<Self>> {
        Err(Error::Unavailable)
    }

    #[cfg(all(feature = "native", target_os = "linux"))]
    /// Load `libracer_gpu_cuda.so.1` and check its ABI version. Does not
    /// initialize CUDA.
    pub(crate) fn load() -> Result<Rc<Self>> {
        use std::ptr::NonNull;

        unsafe {
            let library = NonNull::new(libc::dlopen(
                c"libracer_gpu_cuda.so.1".as_ptr(),
                libc::RTLD_NOW | libc::RTLD_LOCAL,
            ))
            .ok_or(Error::Unavailable)?;

            /// Unloads the library if we bail out early.
            struct Guard(NonNull<c_void>);

            impl Drop for Guard {
                /// Unload.
                fn drop(&mut self) {
                    unsafe {
                        libc::dlclose(self.0.as_ptr());
                    }
                }
            }

            let guard = Guard(library);
            let version = libc::dlsym(library.as_ptr(), c"racer_gpu_abi".as_ptr());
            if version.is_null() {
                return Err(Error::Unavailable);
            }
            let version =
                std::mem::transmute::<*mut c_void, unsafe extern "C" fn() -> u32>(version);
            if version() != ABI_VERSION {
                return Err(Error::Unavailable);
            }
            let api = Self::symbols(library)?;
            std::mem::forget(guard);
            Ok(Rc::new(api))
        }
    }
}

impl Drop for Api {
    /// Unload the library. Leaked objects keep it loaded forever.
    fn drop(&mut self) {
        #[cfg(all(feature = "native", target_os = "linux"))]
        if let Some(library) = self.library {
            unsafe {
                libc::dlclose(library.as_ptr());
            }
        }
    }
}

/// Map a shim result code to an error. `last_driver_error` must be the
/// calling thread's view of the shim, since the driver error is per thread.
pub(crate) fn check(rc: c_int, last_driver_error: unsafe extern "C" fn() -> i32) -> Result<()> {
    match rc {
        code::OK => Ok(()),
        code::EINVAL => Err(Error::InvalidRequest),
        code::ERANGE => Err(Error::InvalidRange),
        code::EUNAVAILABLE => Err(Error::Unavailable),
        code::EDEVICE => Err(Error::WrongDevice),
        code::EDRIVER => Err(Error::Driver(unsafe { last_driver_error() })),
        code::ENOMEM => Err(Error::OutOfMemory),
        code::ENOKERNEL => Err(Error::KernelUnavailable),
        code::EIO => Err(Error::Io),
        code::EBUSY => Err(Error::Busy),
        code::ELEAKED => Err(Error::Leaked),
        _ => Err(Error::Driver(rc)),
    }
}

/// The loaded backend and the leak counter every object shares.
pub(crate) struct Shared {
    pub(crate) api: Rc<Api>,

    pub(crate) leaks: Cell<u64>,
}

impl Shared {
    /// Map a shim result code to an error.
    pub(crate) fn check(&self, rc: c_int) -> Result<()> {
        if rc == code::ELEAKED {
            self.leaks.set(self.leaks.get() + 1);
        }
        check(rc, self.api.last_driver_error)
    }

    /// Count a leaked resource and keep the library loaded for good.
    pub(crate) fn leak(self: &Rc<Self>) {
        self.leaks.set(self.leaks.get() + 1);
        std::mem::forget(self.clone());
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    /// Rust ABI records match the shim's `_Static_assert`s in gpu_cuda.h.
    fn abi_layout_matches_c_static_assertions() {
        assert_eq!(std::mem::size_of::<RawDevice>(), 72);
        assert_eq!(std::mem::offset_of!(RawDevice, ordinal), 48);
        assert_eq!(std::mem::offset_of!(RawDevice, total_memory), 64);
        assert_eq!(std::mem::size_of::<RawDecrypt>(), 80);
        assert_eq!(std::mem::offset_of!(RawDecrypt, check_crc), 44);
        assert_eq!(std::mem::offset_of!(RawDecrypt, aad), 64);
        assert_eq!(std::mem::size_of::<RawVerdict>(), 16);
    }

    #[cfg(all(feature = "native", target_os = "linux"))]
    #[test]
    #[ignore = "requires libracer_gpu_cuda.so.1 on the loader path"]
    /// The installed shim loads, speaks this ABI, and exports every symbol.
    /// Without a CUDA driver, every entry point fails cleanly.
    fn required_native_shim_loads_abi_and_symbols() {
        let api = Api::load().expect("libracer_gpu_cuda.so.1 must be installed for this test");
        assert!(api.library.is_some());
        let rc = unsafe { (api.init)() };
        assert!(
            rc == code::OK || rc == code::EUNAVAILABLE,
            "init returned {rc}"
        );
        if rc == code::EUNAVAILABLE {
            let mut device = RawDevice::default();
            assert_eq!(
                unsafe { (api.discover)(&mut device, 1) },
                code::EUNAVAILABLE
            );
            let mut out = std::ptr::null_mut();
            assert_eq!(unsafe { (api.context)(0, &mut out) }, code::EUNAVAILABLE);
            assert_eq!(unsafe { (api.file)(3, &mut out) }, code::EUNAVAILABLE);
        }
    }
}
