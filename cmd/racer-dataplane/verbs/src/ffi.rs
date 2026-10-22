//! Safe Rust wrappers over the C adapter in `native/verbs.c`.
//!
//! Only the native thread uses these. Each type owns one RDMA object and frees
//! it on drop. If freeing fails, the object is leaked on purpose, along with
//! everything it depends on, because the NIC may still be using it.
use crate::{Error, GuardOwner, Result};

use std::{
    cell::{Cell, RefCell},
    collections::BTreeMap,
    ffi::{CStr, c_char, c_int, c_void},
    ptr::NonNull,
    rc::Rc,
    sync::Arc,
};

#[cfg(any(test, feature = "simulation"))]
#[path = "simulation.rs"]
pub mod simulation;

#[cfg(all(test, feature = "native", target_os = "linux"))]
#[path = "discovery_fault_tests.rs"]
mod discovery_fault_tests;

/// One port from `discover`. Matches `struct rdma_verbs_port` in C.
#[repr(C)]
#[derive(Clone, Copy)]
struct Port {
    name: [c_char; 64],

    gid: [u8; 16],

    mtu: u32,

    lid: u16,

    port: u8,

    link_layer: u8,
}

/// A QP's address. Swap these with the peer to connect.
#[repr(C)]
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Endpoint {
    /// Port GID.
    pub gid: [u8; 16],

    /// QP number. Nonzero, 24 bits.
    pub qpn: u32,

    /// Starting packet sequence number. 24 bits.
    pub psn: u32,

    /// MTU code: 1 to 5 for 256 to 4096 bytes.
    pub mtu: u32,

    /// InfiniBand local ID.
    pub lid: u16,

    /// Port number, from 1.
    pub port: u8,

    /// 1 for InfiniBand, 2 for Ethernet (RoCE).
    pub link_layer: u8,
}

impl Endpoint {
    /// Check that each field is in range. Does not check that the peer is
    /// reachable.
    pub fn validate(&self) -> Result<()> {
        if self.qpn == 0
            || self.qpn > 0xffffff
            || self.psn > 0xffffff
            || !(1..=5).contains(&self.mtu)
            || self.port == 0
            || !matches!(self.link_layer, 1 | 2)
            || self.gid == [0; 16]
        {
            return Err(Error::InvalidRequest);
        }
        Ok(())
    }
}

/// One finished work request from the completion queue. Matches
/// `struct rdma_verbs_wc` in C.
#[repr(C)]
#[derive(Default, Clone, Copy, Debug, Eq, PartialEq)]
struct Completion {
    id: u64,

    status: u32,

    /// `u32::MAX` unless `status` is 0.
    opcode: u32,

    byte_len: u32,

    /// Host byte order. Valid only if `flags & WC_IMM`.
    imm: u32,

    flags: u32,

    vendor_err: u32,
}

/// `RDMA_VERBS_WC_IMM`: the completion carries an immediate.
const WC_IMM: u32 = 1;

/// Native adapter ABI version this crate speaks.
#[cfg(all(feature = "native", target_os = "linux"))]
const ABI_VERSION: u32 = 4;

/// Completion opcodes (`enum ibv_wc_opcode`), pinned by C static asserts.
pub(crate) mod opcode {
    /// `IBV_WC_SEND`.
    pub(crate) const SEND: u32 = 0;

    /// `IBV_WC_RDMA_WRITE`. Also the send-side opcode of a write with immediate.
    pub(crate) const RDMA_WRITE: u32 = 1;

    /// `IBV_WC_BIND_MW`.
    pub(crate) const BIND_MW: u32 = 5;

    /// `IBV_WC_LOCAL_INV`.
    pub(crate) const LOCAL_INV: u32 = 6;

    /// `IBV_WC_RECV`: a peer `SEND` landed.
    pub(crate) const RECV: u32 = 128;

    /// `IBV_WC_RECV_RDMA_WITH_IMM`: a peer write with immediate landed.
    pub(crate) const RECV_RDMA_WITH_IMM: u32 = 129;
}

/// Completion statuses (`enum ibv_wc_status`) the tests name.
#[cfg(test)]
pub(crate) mod status {
    /// `IBV_WC_WR_FLUSH_ERR`.
    pub(crate) const WR_FLUSH_ERR: u32 = 5;

    /// `IBV_WC_REM_ACCESS_ERR`.
    pub(crate) const REM_ACCESS_ERR: u32 = 10;

    /// `IBV_WC_RNR_RETRY_EXC_ERR`.
    pub(crate) const RNR_RETRY_EXC_ERR: u32 = 13;
}

/// Region access sets for `rdma_verbs_register`.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct Access(u32);

impl Access {
    /// Page buffers: `LOCAL_WRITE | MW_BIND`. Remote writes only via windows.
    pub(crate) const PAGES: Self = Self(1);

    /// Control slots: `LOCAL_WRITE` only.
    pub(crate) const CONTROL: Self = Self(2);

    /// Page buffers plus `REMOTE_WRITE` on the region itself. The
    /// `mr_remote_write` escape hatch uses this.
    pub(crate) const PAGES_REMOTE_WRITE: Self = Self(1 | 4);

    /// Marks an implicit ODP region from `rdma_verbs_register_implicit`.
    /// Never passed to `rdma_verbs_register`; only a local write source.
    pub(crate) const IMPLICIT: Self = Self(8);

    /// The raw ABI value.
    pub(crate) fn bits(self) -> u32 {
        self.0
    }
}

/// A successful completion, as reported by
/// [`NativeQueuePair::progress_with`].
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct WorkCompletion {
    /// The work request id the caller posted with.
    pub(crate) id: u64,

    /// One of [`opcode`].
    pub(crate) opcode: u32,

    /// Bytes received. Meaningful only for receive completions.
    pub(crate) byte_len: u32,

    /// Immediate data in host byte order, if any.
    pub(crate) imm: Option<u32>,
}

/// The first failed completion that broke a QP.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct Failure {
    /// Work request id of the failed completion.
    pub(crate) id: u64,

    /// Verbs status, for example [`status::RNR_RETRY_EXC_ERR`]. 0 means the
    /// completion succeeded but did not match any posted work.
    pub(crate) status: u32,

    /// Provider-specific error code.
    pub(crate) vendor_err: u32,
}

/// Build the [`Api`] function table and its symbol loader from a list of
/// `rdma_verbs_<name>` functions.
macro_rules! native_api {
    ($($(#[$meta:meta])* $field:ident: $signature:ty),* $(,)?) => {
        /// The C adapter's functions. Every native object holds an `Rc` to it
        /// so the library stays loaded.
        struct Api {
            library: Option<NonNull<c_void>>,

            $(
                $(#[$meta])*
                $field: $signature,

            )*
        }

        impl Api {
            #[cfg(all(feature = "native", target_os = "linux"))]
            /// Look up every function in `library`.
            unsafe fn symbols(library: NonNull<c_void>) -> Result<Self> {
                Ok(Self {
                    library: Some(library),
                    $($field: {
                        let pointer = unsafe { libc::dlsym(library.as_ptr(),
                            concat!("rdma_verbs_", stringify!($field), "\0").as_ptr().cast()) };
                        if pointer.is_null() { return Err(Error::Unavailable); }
                        unsafe { std::mem::transmute::<*mut c_void, $signature>(pointer) }
                    },)*
                })
            }
        }
    };
}

native_api! {
    discover: unsafe extern "C" fn(*mut Port, u32) -> c_int,

    open: unsafe extern "C" fn(*const c_char) -> *mut c_void,

    close: unsafe extern "C" fn(*mut c_void) -> c_int,

    qp: unsafe extern "C" fn(*mut c_void, u8, u32, u32, *mut u32) -> *mut c_void,

    connect: unsafe extern "C" fn(*mut c_void, *const Endpoint, *const Endpoint) -> c_int,

    stop: unsafe extern "C" fn(*mut c_void) -> c_int,

    qp_free: unsafe extern "C" fn(*mut c_void) -> c_int,

    register: unsafe extern "C" fn(*mut c_void, u64, u32) -> *mut c_void,

    register_implicit: unsafe extern "C" fn(*mut c_void) -> *mut c_void,

    deregister: unsafe extern "C" fn(*mut c_void) -> c_int,

    bytes: unsafe extern "C" fn(*mut c_void) -> *mut u8,

    window: unsafe extern "C" fn(*mut c_void, *mut u32) -> *mut c_void,

    window_free: unsafe extern "C" fn(*mut c_void) -> c_int,

    bind: unsafe extern "C" fn(*mut c_void, *mut c_void, *mut c_void, u32, u64, u64, u32) -> c_int,

    invalidate: unsafe extern "C" fn(*mut c_void, u32, u64) -> c_int,

    write_imm: unsafe extern "C" fn(*mut c_void, *mut c_void, u64, u32, u64, u32, u32, u64) -> c_int,

    send: unsafe extern "C" fn(*mut c_void, *mut c_void, u64, u32, u64) -> c_int,

    post_recv: unsafe extern "C" fn(*mut c_void, *mut c_void, u64, u32, u64) -> c_int,

    poll: unsafe extern "C" fn(*mut c_void, *mut Completion, u32) -> c_int,
}

/// C's `RDMA_VERBS_CLEANUP_LEAK`, outside the negative errno range.
const CLEANUP_LEAK: c_int = c_int::MIN;

/// C's `RDMA_VERBS_OPEN_CLEANUP_LEAK`, never a usable device handle.
const OPEN_CLEANUP_LEAK: *mut c_void = std::ptr::without_provenance_mut(usize::MAX);

impl Api {
    #[cfg(not(all(feature = "native", target_os = "linux")))]
    /// No native backend in this build.
    fn load() -> Result<Rc<Self>> {
        Err(Error::Unavailable)
    }

    #[cfg(all(feature = "native", target_os = "linux"))]
    /// Load `librdma_verbs.so.1` and check it speaks [`ABI_VERSION`].
    fn load() -> Result<Rc<Self>> {
        // Fixed library name, found via the normal loader path.
        unsafe {
            let library = NonNull::new(libc::dlopen(
                c"librdma_verbs.so.1".as_ptr(),
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

            /// Look up one function by name.
            macro_rules! sym {
                ($name:literal, $ty:ty) => {{
                    let pointer =
                        libc::dlsym(library.as_ptr(), concat!($name, "\0").as_ptr().cast());
                    if pointer.is_null() {
                        return Err(Error::Unavailable);
                    }
                    std::mem::transmute::<*mut c_void, $ty>(pointer)
                }};
            }

            let version = sym!("rdma_verbs_abi", unsafe extern "C" fn() -> u32);
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
        if let Some(library) = self.library {
            unsafe {
                libc::dlclose(library.as_ptr());
            }
        }
    }
}

/// An open RDMA device.
pub(crate) struct NativeDevice {
    api: Rc<Api>,

    raw: NonNull<c_void>,

    pub(crate) quota: Option<Arc<GuardOwner>>,

    pub(crate) name: String,

    /// Port info. `qpn` and `psn` are unset.
    pub(crate) endpoint: Endpoint,
}

impl NativeDevice {
    /// NUMA node from sysfs (or the simulation), if known.
    pub(crate) fn numa_node(&self) -> Option<usize> {
        #[cfg(any(test, feature = "simulation"))]
        if simulation::is_api(&self.api) {
            return simulation::numa_node(self.raw);
        }
        if self.name.contains('/') || self.name.contains("..") {
            return None;
        }
        std::fs::read_to_string(format!(
            "/sys/class/infiniband/{}/device/numa_node",
            self.name
        ))
        .ok()?
        .trim()
        .parse()
        .ok()
    }
}

impl Drop for NativeDevice {
    /// Close the device. On failure, keep the library and charged owner.
    fn drop(&mut self) {
        if unsafe { (self.api.close)(self.raw.as_ptr()) } != 0 {
            if let Some(quota) = self.quota.take() {
                quota.quarantine();
                std::mem::forget(quota);
            }
            std::mem::forget(self.api.clone());
        }
    }
}

/// List ports and open a device for each. Fails on more than 64 ports.
/// Skips ports whose device will not open.
pub(crate) fn discover() -> Result<Vec<NativeDevice>> {
    discover_with_quota(|| None)
}

/// Attach a tracked owner to each opened device before any discovery cleanup.
pub(crate) fn discover_with_quota(
    quota: impl FnMut() -> Option<Arc<GuardOwner>>,
) -> Result<Vec<NativeDevice>> {
    #[cfg(any(test, feature = "simulation"))]
    let api = match simulation::current() {
        Some(_) => simulation::api(),
        None => Api::load()?,
    };
    #[cfg(not(any(test, feature = "simulation")))]
    let api = Api::load()?;
    discover_with_api(api, quota)
}

/// Charge temporary discovery contexts before calling into the adapter.
fn discover_with_api(
    api: Rc<Api>,
    mut quota: impl FnMut() -> Option<Arc<GuardOwner>>,
) -> Result<Vec<NativeDevice>> {
    let discovery_quota = quota();
    let mut ports = [Port {
        name: [0; 64],
        gid: [0; 16],
        mtu: 0,
        lid: 0,
        port: 0,
        link_layer: 0,
    }; 64];
    let count = unsafe { (api.discover)(ports.as_mut_ptr(), ports.len() as u32) };
    if count == CLEANUP_LEAK {
        if let Some(quota) = discovery_quota {
            quota.quarantine();
            std::mem::forget(quota);
        }
        std::mem::forget(api);
        return Err(Error::Unavailable);
    }
    if count < 0 || count as usize > ports.len() {
        return Err(Error::Unavailable);
    }
    let mut devices = Vec::new();
    for port in &ports[..count as usize] {
        if !port.name.contains(&0) {
            return Err(Error::Io);
        }
        let name = unsafe { CStr::from_ptr(port.name.as_ptr()) }
            .to_str()
            .map_err(|_| Error::Io)?
            .to_owned();
        let open_quota = quota();
        let raw = unsafe { (api.open)(port.name.as_ptr()) };
        if raw == OPEN_CLEANUP_LEAK {
            if let Some(quota) = open_quota {
                quota.quarantine();
                std::mem::forget(quota);
            }
            std::mem::forget(api);
            return Err(Error::Unavailable);
        }
        let Some(raw) = NonNull::new(raw) else {
            continue;
        };
        devices.push(NativeDevice {
            api: api.clone(),
            raw,
            quota: open_quota,
            name,
            endpoint: Endpoint {
                gid: port.gid,
                qpn: 0,
                psn: 0,
                mtu: port.mtu,
                lid: port.lid,
                port: port.port,
                link_layer: port.link_layer,
            },
        });
    }
    Ok(devices)
}

/// A registered memory buffer (MR) the NIC can read and write.
///
/// Posts do not track which ranges the NIC may touch; their caller must.
/// Holds the node's guard and leaks it if deregister fails.
pub(crate) struct NativeRegion {
    device: Rc<NativeDevice>,

    raw: NonNull<c_void>,

    /// Allocated size.
    length: usize,

    quota: Option<Arc<GuardOwner>>,

    access: Access,
}

impl NativeRegion {
    /// Allocate and register `length` bytes with `access`.
    pub(crate) fn with_access(
        device: Rc<NativeDevice>,
        length: usize,
        access: Access,
        quota: Arc<GuardOwner>,
    ) -> Result<Rc<Self>> {
        if length == 0 || u64::try_from(length).is_err() {
            return Err(Error::InvalidRange);
        }
        let raw = NonNull::new(unsafe {
            (device.api.register)(device.raw.as_ptr(), length as u64, access.bits())
        })
        .ok_or(Error::Unavailable)?;
        Ok(Rc::new(Self {
            device,
            raw,
            length,
            quota: Some(quota),
            access,
        }))
    }

    /// Register the whole process address space as one implicit on-demand
    /// paging MR, for zero-copy write sources. `None` if the device lacks
    /// implicit ODP for RC writes.
    ///
    /// The region owns no bytes: an `offset` is a virtual address, and
    /// [`Self::as_ptr`] is null. Callers must keep the source memory alive
    /// and unmodified until the post's completion or the QP's destruction.
    pub(crate) fn implicit(device: Rc<NativeDevice>, quota: Arc<GuardOwner>) -> Option<Rc<Self>> {
        let raw = NonNull::new(unsafe { (device.api.register_implicit)(device.raw.as_ptr()) })?;
        Some(Rc::new(Self {
            device,
            raw,
            length: usize::MAX,
            quota: Some(quota),
            access: Access::IMPLICIT,
        }))
    }

    /// True for an implicit address-space region from [`Self::implicit`].
    pub(crate) fn is_implicit(&self) -> bool {
        self.access == Access::IMPLICIT
    }

    /// The start of the registered buffer, valid while `self` lives.
    ///
    /// The caller must not read or write any range the NIC may still access.
    pub(crate) fn as_ptr(&self) -> *mut u8 {
        unsafe { (self.device.api.bytes)(self.raw.as_ptr()) }
    }

    /// True if `[offset, offset + length)` is a nonempty range of the region.
    fn contains(&self, offset: u64, length: u32) -> bool {
        length != 0
            && offset
                .checked_add(length.into())
                .is_some_and(|end| end <= self.length as u64)
    }

    /// The address peers use to write here.
    pub(crate) fn address(&self) -> u64 {
        #[cfg(any(test, feature = "simulation"))]
        if simulation::is_api(&self.device.api) {
            return simulation::address(self.raw);
        }
        unsafe { (self.device.api.bytes)(self.raw.as_ptr()) as u64 }
    }

    /// Registered size in bytes.
    pub(crate) fn length(&self) -> usize {
        self.length
    }
}

impl Drop for NativeRegion {
    /// Deregister. On failure, leak the device and guard.
    fn drop(&mut self) {
        if unsafe { (self.device.api.deregister)(self.raw.as_ptr()) } != 0 {
            std::mem::forget(self.device.clone());
            self.quota.as_ref().unwrap().quarantine();
            std::mem::forget(self.quota.take());
        }
    }
}

/// A reusable type 2 memory window for ranged binds.
///
/// Each bind uses a fresh rkey (`ibv_inc_rkey` of the previous one), so a
/// stale key from an earlier grant never matches. The window keeps the last
/// region it was bound to alive until it is freed, because the NIC may still
/// accept writes through it.
pub(crate) struct MemoryWindow {
    device: Rc<NativeDevice>,

    raw: NonNull<c_void>,

    /// Key the next bind will use.
    next_key: Cell<u32>,

    region: RefCell<Option<Rc<NativeRegion>>>,
}

impl MemoryWindow {
    /// Allocate a window on `device`.
    pub(crate) fn new(device: &Rc<NativeDevice>) -> Result<Rc<Self>> {
        let mut key = 0;
        let raw = NonNull::new(unsafe { (device.api.window)(device.raw.as_ptr(), &mut key) })
            .ok_or(Error::Unavailable)?;
        Ok(Rc::new(Self {
            device: device.clone(),
            raw,
            next_key: Cell::new(key),
            region: RefCell::new(None),
        }))
    }

    #[cfg(test)]
    /// The key the next bind will use.
    pub(crate) fn next_key(&self) -> u32 {
        self.next_key.get()
    }
}

/// `ibv_inc_rkey`: bump the low 8 key bits, keep the 24-bit index.
pub(crate) fn inc_rkey(key: u32) -> u32 {
    (key & 0xffff_ff00) | (key.wrapping_add(1) & 0xff)
}

impl Drop for MemoryWindow {
    /// Free the window. On failure, leak the device and the bound region.
    fn drop(&mut self) {
        if unsafe { (self.device.api.window_free)(self.raw.as_ptr()) } != 0 {
            if let Some(region) = self.region.get_mut().take() {
                if let Some(quota) = &region.quota {
                    quota.quarantine();
                }
                std::mem::forget(region);
            }
            std::mem::forget(self.device.clone());
        }
    }
}

/// A posted work request. Keeps its region and window alive until it
/// completes or the QP stops.
struct Pending {
    /// Expected completion opcode. Ignored for receives.
    opcode: u32,

    /// Posted on the receive queue.
    recv: bool,

    /// Receive buffer length.
    length: u32,

    _region: Rc<NativeRegion>,

    _window: Option<Rc<MemoryWindow>>,
}

impl Pending {
    /// True if a successful completion `c` is what this work should produce.
    fn matches(&self, c: &Completion) -> bool {
        if c.status != 0 {
            return false;
        }
        if !self.recv {
            return c.opcode == self.opcode;
        }
        let imm = c.flags & WC_IMM != 0;
        match c.opcode {
            opcode::RECV => !imm && c.byte_len <= self.length,
            opcode::RECV_RDMA_WITH_IMM => imm,
            _ => false,
        }
    }
}

/// A queue pair (QP) and its completion queue.
///
/// Tracks posted work. [`Self::stop`] is the only way to be sure the NIC is
/// done with its memory.
pub(crate) struct NativeQueuePair {
    /// Keeps every QP and CQ leak charged to its owner.
    quota: Arc<GuardOwner>,

    /// Publishes successful DMA stop even when it occurs during destructor retry.
    stop_observer: Arc<std::sync::atomic::AtomicBool>,

    device: Rc<NativeDevice>,

    raw: NonNull<c_void>,

    pub(crate) endpoint: Endpoint,

    /// Stop succeeded. The NIC is done.
    stopped: Cell<bool>,

    /// Stop was requested. No new work.
    terminating: Cell<bool>,

    connected: Cell<bool>,

    /// Posted work by ID.
    pending: RefCell<BTreeMap<u64, Pending>>,

    /// Send queue limit and current use.
    send_capacity: u32,

    sends: Cell<u32>,

    /// Receive queue limit and current use.
    recv_capacity: u32,

    recvs: Cell<u32>,

    /// The first completion that broke the QP.
    failure: Cell<Option<Failure>>,
}

impl NativeQueuePair {
    /// Create a QP with `send_depth` send and `recv_depth` receive WRs, and
    /// a CQ for both. Both depths must be nonzero.
    pub(crate) fn new_with_depth(
        device: Rc<NativeDevice>,
        quota: Arc<GuardOwner>,
        stop_observer: Arc<std::sync::atomic::AtomicBool>,
        send_depth: u32,
        recv_depth: u32,
    ) -> Result<Rc<Self>> {
        if send_depth == 0 || recv_depth == 0 || send_depth.checked_add(recv_depth).is_none() {
            return Err(Error::InvalidConfiguration);
        }
        let mut qpn = 0;
        let raw = NonNull::new(unsafe {
            (device.api.qp)(
                device.raw.as_ptr(),
                device.endpoint.port,
                send_depth,
                recv_depth,
                &mut qpn,
            )
        });
        let Some(raw) = raw else {
            if qpn == u32::MAX {
                quota.quarantine();
                std::mem::forget(quota);
                std::mem::forget(device);
            }
            return Err(Error::Unavailable);
        };
        let mut psn = [0; 4];
        #[cfg(any(test, feature = "simulation"))]
        let simulated = simulation::is_api(&device.api);
        #[cfg(not(any(test, feature = "simulation")))]
        let simulated = false;
        if simulated {
            psn = qpn.to_be_bytes();
        } else if uring_runtime::environment::fill_random(&mut psn).is_err() {
            if unsafe { (device.api.qp_free)(raw.as_ptr()) } != 0 {
                quota.quarantine();
                std::mem::forget(quota);
                std::mem::forget(device);
            }
            return Err(Error::Io);
        }
        let endpoint = Endpoint {
            qpn,
            psn: u32::from_be_bytes(psn) & 0xffffff,
            ..device.endpoint
        };
        Ok(Rc::new(Self {
            quota,
            stop_observer,
            device,
            raw,
            endpoint,
            stopped: Cell::new(false),
            terminating: Cell::new(false),
            connected: Cell::new(false),
            pending: RefCell::new(BTreeMap::new()),
            send_capacity: send_depth,
            sends: Cell::new(0),
            recv_capacity: recv_depth,
            recvs: Cell::new(0),
            failure: Cell::new(None),
        }))
    }

    /// Connect to `remote`. Once only. On failure, stops the QP.
    pub(crate) fn connect(&self, remote: Endpoint) -> Result<()> {
        remote.validate()?;
        if self.stopped.get()
            || self.terminating.get()
            || self.connected.get()
            || remote.link_layer != self.endpoint.link_layer
        {
            return Err(Error::InvalidRequest);
        }
        if unsafe { (self.device.api.connect)(self.raw.as_ptr(), &self.endpoint, &remote) } != 0 {
            self.stop()?;
            return Err(Error::Unavailable);
        }
        self.connected.set(true);
        Ok(())
    }

    /// True if connected and not stopping.
    pub(crate) fn ready(&self) -> bool {
        self.connected.get() && !self.stopped.get() && !self.terminating.get()
    }

    #[cfg(test)]
    /// True once stop succeeded.
    pub(crate) fn stopped(&self) -> bool {
        self.stopped.get()
    }

    /// Add `pending` under `id` and count it against its queue.
    fn track(&self, id: u64, pending: Pending) {
        let counter = if pending.recv {
            &self.recvs
        } else {
            &self.sends
        };
        counter.set(counter.get() + 1);
        self.pending.borrow_mut().insert(id, pending);
    }

    /// Remove the work under `id` and release its queue slot.
    fn untrack(&self, id: u64) -> Option<Pending> {
        let pending = self.pending.borrow_mut().remove(&id)?;
        let counter = if pending.recv {
            &self.recvs
        } else {
            &self.sends
        };
        counter.set(counter.get() - 1);
        Some(pending)
    }

    /// Check the post result. If the post failed, nothing reached the NIC, so
    /// drop the record.
    fn submitted(&self, id: u64, rc: i32) -> Result<()> {
        if rc != 0 {
            self.untrack(id);
            return Err(Error::Io);
        }
        Ok(())
    }

    /// Read up to 32 completions without blocking and report each successful
    /// completion to `sink` after its work is released. Returns how many.
    ///
    /// Any failed or unexpected completion stops the QP, records it in
    /// [`Self::failure`], and returns [`Error::Io`].
    pub(crate) fn progress_with(&self, mut sink: impl FnMut(WorkCompletion)) -> Result<usize> {
        if self.stopped.get() {
            return Ok(0);
        }
        if self.terminating.get() {
            self.stop()?;
            return Err(Error::Cancelled);
        }
        let mut completions = [Completion::default(); 32];
        let n = unsafe { (self.device.api.poll)(self.raw.as_ptr(), completions.as_mut_ptr(), 32) };
        if !(0..=32).contains(&n) {
            self.stop()?;
            return Err(Error::Io);
        }
        for c in &completions[..n as usize] {
            let valid = self
                .pending
                .borrow()
                .get(&c.id)
                .is_some_and(|p| p.matches(c));
            if !valid {
                if self.failure.get().is_none() {
                    self.failure.set(Some(Failure {
                        id: c.id,
                        status: c.status,
                        vendor_err: c.vendor_err,
                    }));
                }
                self.stop()?;
                return Err(Error::Io);
            }
            std::sync::atomic::fence(std::sync::atomic::Ordering::Acquire);
            drop(self.untrack(c.id).ok_or(Error::Io)?);
            sink(WorkCompletion {
                id: c.id,
                opcode: c.opcode,
                byte_len: c.byte_len,
                imm: (c.flags & WC_IMM != 0).then_some(c.imm),
            });
        }
        Ok(n as usize)
    }

    /// Stop the QP for good. After success the NIC will not touch its memory
    /// again, and pending work and its regions and windows are released.
    ///
    /// On failure nothing is released. Safe to retry.
    pub(crate) fn stop(&self) -> Result<()> {
        if self.stopped.get() {
            return Ok(());
        }
        self.terminating.set(true);
        if unsafe { (self.device.api.stop)(self.raw.as_ptr()) } != 0 {
            return Err(Error::Io);
        }
        self.stopped.set(true);
        self.stop_observer
            .store(true, std::sync::atomic::Ordering::Release);
        std::sync::atomic::fence(std::sync::atomic::Ordering::Acquire);
        let pending = std::mem::take(&mut *self.pending.borrow_mut());
        self.sends.set(0);
        self.recvs.set(0);
        drop(pending);
        Ok(())
    }

    /// Validate and record a ranged post under the caller's `id`, then run
    /// `post`. A rejected post leaves nothing behind; the QP stays usable.
    #[allow(clippy::too_many_arguments)]
    fn post_ranged(
        &self,
        id: u64,
        recv: bool,
        opcode: u32,
        region: &Rc<NativeRegion>,
        offset: u64,
        length: u32,
        window: Option<Rc<MemoryWindow>>,
        post: impl FnOnce() -> c_int,
    ) -> Result<()> {
        if (region.is_implicit() && (recv || opcode != opcode::RDMA_WRITE))
            || !Rc::ptr_eq(&region.device, &self.device)
            || !region.contains(offset, length)
            || self.pending.borrow().contains_key(&id)
        {
            return Err(Error::InvalidRequest);
        }
        // Receives may be posted before connect (verbs INIT state), so the
        // peer never finds the queue empty once it can send.
        let open = if recv {
            !self.stopped.get() && !self.terminating.get()
        } else {
            self.ready()
        };
        if !open {
            return Err(Error::Unavailable);
        }
        let (used, capacity) = if recv {
            (self.recvs.get(), self.recv_capacity)
        } else {
            (self.sends.get(), self.send_capacity)
        };
        if used >= capacity {
            return Err(Error::Overloaded);
        }
        self.track(
            id,
            Pending {
                opcode,
                recv,
                length,
                _region: region.clone(),
                _window: window,
            },
        );
        std::sync::atomic::fence(std::sync::atomic::Ordering::Release);
        self.submitted(id, post())
    }

    /// Post a signaled `SEND` of `length` bytes of `region` at `offset`.
    /// Completes with [`opcode::SEND`]. Consumes one peer receive.
    pub(crate) fn send(
        &self,
        id: u64,
        region: &Rc<NativeRegion>,
        offset: u64,
        length: u32,
    ) -> Result<()> {
        self.post_ranged(
            id,
            false,
            opcode::SEND,
            region,
            offset,
            length,
            None,
            || unsafe {
                (self.device.api.send)(self.raw.as_ptr(), region.raw.as_ptr(), offset, length, id)
            },
        )
    }

    /// Post a receive into `length` bytes of `region` at `offset`. Allowed
    /// before [`Self::connect`]. Completes
    /// with [`opcode::RECV`] (`byte_len <= length`, no immediate) or
    /// [`opcode::RECV_RDMA_WITH_IMM`] (with an immediate).
    pub(crate) fn post_recv(
        &self,
        id: u64,
        region: &Rc<NativeRegion>,
        offset: u64,
        length: u32,
    ) -> Result<()> {
        self.post_ranged(
            id,
            true,
            opcode::RECV,
            region,
            offset,
            length,
            None,
            || unsafe {
                (self.device.api.post_recv)(
                    self.raw.as_ptr(),
                    region.raw.as_ptr(),
                    offset,
                    length,
                    id,
                )
            },
        )
    }

    /// Post a signaled `RDMA_WRITE_WITH_IMM` of `length` bytes of `region` at
    /// `offset` to the peer's `address` through window `key`, carrying `imm`
    /// (host byte order). Completes with [`opcode::RDMA_WRITE`]. Consumes one
    /// peer receive.
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn write_imm(
        &self,
        id: u64,
        region: &Rc<NativeRegion>,
        offset: u64,
        length: u32,
        address: u64,
        key: u32,
        imm: u32,
    ) -> Result<()> {
        if address.checked_add(length.into()).is_none() {
            return Err(Error::InvalidRequest);
        }
        self.post_ranged(
            id,
            false,
            opcode::RDMA_WRITE,
            region,
            offset,
            length,
            None,
            || unsafe {
                (self.device.api.write_imm)(
                    self.raw.as_ptr(),
                    region.raw.as_ptr(),
                    offset,
                    length,
                    address,
                    key,
                    imm,
                    id,
                )
            },
        )
    }

    /// Post a signaled, fenced bind of `window` over `length` bytes of
    /// `region` at `offset`, with remote write access. Returns the window's new rkey.
    /// Completes with [`opcode::BIND_MW`]. The region must be registered
    /// with [`Access::PAGES`] (with or without remote write), and a window
    /// stays tied to the first region it is bound to.
    pub(crate) fn bind_window(
        &self,
        id: u64,
        window: &Rc<MemoryWindow>,
        region: &Rc<NativeRegion>,
        offset: u64,
        length: u32,
    ) -> Result<u32> {
        if !Rc::ptr_eq(&window.device, &self.device)
            || region.access.bits() & Access::PAGES.bits() == 0
            || window
                .region
                .borrow()
                .as_ref()
                .is_some_and(|bound| !Rc::ptr_eq(bound, region))
        {
            return Err(Error::InvalidRequest);
        }
        let key = window.next_key.get();
        self.post_ranged(
            id,
            false,
            opcode::BIND_MW,
            region,
            offset,
            length,
            Some(window.clone()),
            || unsafe {
                (self.device.api.bind)(
                    self.raw.as_ptr(),
                    window.raw.as_ptr(),
                    region.raw.as_ptr(),
                    key,
                    id,
                    offset,
                    length,
                )
            },
        )?;
        *window.region.borrow_mut() = Some(region.clone());
        window.next_key.set(inc_rkey(key));
        Ok(key)
    }

    /// Post a signaled `LOCAL_INV` of `key` on `window`. Not fenced. Completes
    /// with [`opcode::LOCAL_INV`].
    pub(crate) fn invalidate_window(
        &self,
        id: u64,
        window: &Rc<MemoryWindow>,
        key: u32,
    ) -> Result<()> {
        let Some(region) = window.region.borrow().clone() else {
            return Err(Error::InvalidRequest);
        };
        if !Rc::ptr_eq(&window.device, &self.device) {
            return Err(Error::InvalidRequest);
        }
        // Hold the region until the CQE. No data moves, so any valid range works.
        self.post_ranged(
            id,
            false,
            opcode::LOCAL_INV,
            &region,
            0,
            1,
            Some(window.clone()),
            || unsafe { (self.device.api.invalidate)(self.raw.as_ptr(), key, id) },
        )
    }

    /// The first completion that broke this QP, if any.
    pub(crate) fn failure(&self) -> Option<Failure> {
        self.failure.get()
    }
}

impl Drop for NativeQueuePair {
    /// Stop, then free. If stop fails, leak the QP's work and device, since
    /// the NIC may still use them.
    fn drop(&mut self) {
        if self.stop().is_err() {
            self.quota.quarantine();
            std::mem::forget(self.quota.clone());
            std::mem::forget(std::mem::take(self.pending.get_mut()));
            std::mem::forget(self.device.clone());
            return;
        }
        if unsafe { (self.device.api.qp_free)(self.raw.as_ptr()) } != 0 {
            self.quota.quarantine();
            std::mem::forget(self.quota.clone());
            std::mem::forget(self.device.clone());
        }
    }
}

#[cfg(test)]
mod tests {
    //! Endpoint validation and optional native discovery contracts.
    use super::*;

    /// Require the real adapter's ABI and every symbol without opening any device.
    #[cfg(all(feature = "native", target_os = "linux"))]
    #[test]
    #[ignore = "requires built C adapter on loader path; no RDMA hardware required"]
    fn required_native_adapter_loads_abi_and_symbols() {
        let api = Api::load()
            .expect("built native adapter must load with the current ABI and all symbols");
        assert!(api.library.is_some(), "must load C adapter, not simulation");
    }

    #[test]
    /// Reject queue pair numbers outside the native 24-bit field.
    fn endpoint_rejects_invalid_native_parameters() {
        let mut e = Endpoint {
            gid: [1; 16],
            qpn: 1,
            psn: 0,
            mtu: 3,
            lid: 1,
            port: 1,
            link_layer: 1,
        };
        assert!(e.validate().is_ok());
        e.qpn = 1 << 24;
        assert_eq!(e.validate(), Err(Error::InvalidRequest));
    }

    #[test]
    /// Discovery returns real port descriptions or an explicit unavailable result.
    fn optional_runtime_never_fabricates_a_device() {
        match discover() {
            Ok(devices) => {
                for d in devices {
                    assert!(!d.name.is_empty());
                    assert_ne!(d.endpoint.port, 0);
                }
            }
            Err(error) => assert_eq!(error, Error::Unavailable),
        }
    }
}

#[cfg(test)]
pub(crate) mod lifetime_tests {
    //! FFI fault injection exercises production ownership, ABI, and quarantine safety.
    use super::*;

    #[cfg(feature = "native")]
    use std::time::{Duration, Instant};

    use std::collections::VecDeque;

    /// Scripted native failures, queued completions, and teardown observations.
    #[derive(Default)]
    struct Faults {
        events: Vec<&'static str>,

        cq: VecDeque<Completion>,

        stop_fails: bool,

        post_fails: bool,

        poll_fails: bool,

        /// Native post calls that reached the adapter.
        posts: usize,
    }

    thread_local! { static FAULTS: RefCell<Faults> = RefCell::new(Faults::default()); }

    /// Record the order of native ownership releases.
    fn event(value: &'static str) {
        FAULTS.with_borrow_mut(|f| f.events.push(value));
    }

    /// Allocate an opaque fixture handle with a stable address.
    fn pointer() -> *mut c_void {
        Box::into_raw(Box::new(0u8)).cast()
    }

    /// Report no discoverable ports for the direct ownership fixture.
    unsafe extern "C" fn discover(_: *mut Port, _: u32) -> c_int {
        0
    }

    /// Allocate a fixture device context.
    unsafe extern "C" fn open(_: *const c_char) -> *mut c_void {
        pointer()
    }

    /// Record and release a fixture device context.
    unsafe extern "C" fn close(p: *mut c_void) -> c_int {
        event("pd-context");
        unsafe {
            drop(Box::from_raw(p.cast::<u8>()));
        }
        0
    }

    /// The fixture device has no implicit ODP.
    unsafe extern "C" fn register_implicit(_: *mut c_void) -> *mut c_void {
        std::ptr::null_mut()
    }

    /// Allocate a fixture queue pair with a fixed valid number.
    unsafe extern "C" fn qp(_: *mut c_void, _: u8, _: u32, _: u32, qpn: *mut u32) -> *mut c_void {
        unsafe {
            *qpn = 7;
        }
        pointer()
    }

    /// Accept connection setup without a physical peer.
    unsafe extern "C" fn connect(_: *mut c_void, _: *const Endpoint, _: *const Endpoint) -> c_int {
        0
    }

    /// Record a terminal fence and apply the scripted failure state.
    unsafe extern "C" fn stop(_: *mut c_void) -> c_int {
        event("stop");
        if FAULTS.with_borrow(|f| f.stop_fails) {
            5
        } else {
            0
        }
    }

    /// Record and release the fixture queue pair and completion queue.
    unsafe extern "C" fn qp_free(p: *mut c_void) -> c_int {
        event("cq");
        unsafe {
            drop(Box::from_raw(p.cast::<u8>()));
        }
        0
    }

    /// Allocate zeroed fixture memory behind an opaque region handle.
    unsafe extern "C" fn register(_: *mut c_void, length: u64, _: u32) -> *mut c_void {
        Box::into_raw(Box::new(vec![0u8; length as usize])).cast()
    }

    /// Record and release the fixture region's allocation.
    unsafe extern "C" fn deregister(p: *mut c_void) -> c_int {
        event("mr");
        unsafe {
            drop(Box::from_raw(p.cast::<Vec<u8>>()));
        }
        0
    }

    /// Return the fixture region's stable backing allocation.
    unsafe extern "C" fn bytes(p: *mut c_void) -> *mut u8 {
        unsafe { (&mut *p.cast::<Vec<u8>>()).as_mut_ptr() }
    }

    /// Allocate a fixture window with a fixed remote key.
    unsafe extern "C" fn window(_: *mut c_void, key: *mut u32) -> *mut c_void {
        unsafe {
            *key = 71;
        }
        pointer()
    }

    /// Record and release a fixture memory window.
    unsafe extern "C" fn window_free(p: *mut c_void) -> c_int {
        event("mw");
        unsafe {
            drop(Box::from_raw(p.cast::<u8>()));
        }
        0
    }

    /// Accept a bind whose completion is injected separately.
    unsafe extern "C" fn bind(
        _: *mut c_void,
        _: *mut c_void,
        _: *mut c_void,
        _: u32,
        _: u64,
        _: u64,
        _: u32,
    ) -> c_int {
        post_result()
    }

    /// The scripted synchronous post result.
    fn post_result() -> c_int {
        FAULTS.with_borrow_mut(|f| {
            f.posts += 1;
            if f.post_fails { 5 } else { 0 }
        })
    }

    /// Accept or reject a write with immediate according to the fault script.
    unsafe extern "C" fn write_imm(
        _: *mut c_void,
        _: *mut c_void,
        _: u64,
        _: u32,
        _: u64,
        _: u32,
        _: u32,
        _: u64,
    ) -> c_int {
        post_result()
    }

    /// Accept or reject a send according to the fault script.
    unsafe extern "C" fn send(_: *mut c_void, _: *mut c_void, _: u64, _: u32, _: u64) -> c_int {
        post_result()
    }

    /// Accept or reject a receive according to the fault script.
    unsafe extern "C" fn post_recv(
        _: *mut c_void,
        _: *mut c_void,
        _: u64,
        _: u32,
        _: u64,
    ) -> c_int {
        post_result()
    }

    /// Accept or reject an invalidation according to the fault script.
    unsafe extern "C" fn invalidate(_: *mut c_void, _: u32, _: u64) -> c_int {
        post_result()
    }

    /// Drain injected completions unless polling is scripted to fail.
    unsafe extern "C" fn poll(_: *mut c_void, out: *mut Completion, cap: u32) -> c_int {
        FAULTS.with_borrow_mut(|f| {
            if f.poll_fails {
                return -1;
            }
            let n = f.cq.len().min(cap as usize);
            for i in 0..n {
                unsafe {
                    *out.add(i) = f.cq.pop_front().unwrap();
                }
            }
            n as c_int
        })
    }

    pub(crate) use crate::test_guard::Observer as QuotaObserver;

    /// Create a quota owner and an observer of its retained charge.
    pub(crate) fn quota() -> (Arc<GuardOwner>, QuotaObserver) {
        let (guard, observer) = crate::test_guard::guard();
        (GuardOwner::new(guard), observer)
    }

    /// Reset faults and create a connected queue pair with charged source memory.
    pub(crate) fn fixture() -> (Rc<NativeQueuePair>, Rc<NativeRegion>, QuotaObserver) {
        FAULTS.with_borrow_mut(|f| *f = Faults::default());
        let api = Rc::new(Api {
            library: None,
            discover,
            open,
            close,
            qp,
            connect,
            stop,
            qp_free,
            register,
            register_implicit,
            deregister,
            bytes,
            window,
            window_free,
            bind,
            invalidate,
            write_imm,
            send,
            post_recv,
            poll,
        });
        let device = Rc::new(NativeDevice {
            api,
            raw: NonNull::new(pointer()).unwrap(),
            quota: None,
            name: "test-only".into(),
            endpoint: Endpoint {
                gid: [1; 16],
                qpn: 0,
                psn: 0,
                mtu: 3,
                lid: 1,
                port: 1,
                link_layer: 1,
            },
        });
        let qp = NativeQueuePair::new_with_depth(
            device.clone(),
            GuardOwner::new(Arc::new(())),
            Arc::new(std::sync::atomic::AtomicBool::new(false)),
            32,
            4,
        )
        .unwrap();
        qp.connect(qp.endpoint).unwrap();
        let (quota, charged) = quota();
        let region = NativeRegion::with_access(device, 32, Access::PAGES, quota).unwrap();
        (qp, region, charged)
    }

    /// Queue a completion for the next fixture progress call.
    pub(crate) fn complete(id: u64, status: u32, opcode: u32) {
        push(Completion {
            id,
            status,
            opcode,
            ..Completion::default()
        });
    }

    /// Queue a full completion record for the next fixture progress call.
    fn push(completion: Completion) {
        FAULTS.with_borrow_mut(|f| f.cq.push_back(completion));
    }

    #[test]
    /// Dropping the caller's handle must not release memory still owned by
    /// native work.
    fn posted_write_retains_source_and_quota_until_terminal_fence() {
        let (qp, region, charged) = fixture();
        qp.write_imm(wr(1), &region, 0, 32, 4096, 7, 0).unwrap();
        drop(region);
        assert_eq!(charged.get(), 1);
        assert_eq!(drain(&qp).unwrap(), []);
        assert_eq!(charged.get(), 1);
        qp.stop().unwrap();
        assert_eq!(charged.get(), 0);
        drop(qp);
        FAULTS.with_borrow(|f| assert_eq!(f.events, ["stop", "mr", "cq", "pd-context"]));
    }

    #[test]
    /// Failed fencing keeps an in-flight bind's window and region charged
    /// while late DMA remains possible. The window is freed before its region.
    fn failed_fence_quarantines_late_remote_writes_and_quota() {
        let (qp, region, charged) = fixture();
        let window = MemoryWindow::new(&qp.device).unwrap();
        qp.bind_window(wr(1), &window, &region, 0, 32).unwrap();
        FAULTS.with_borrow_mut(|f| f.stop_fails = true);
        assert_eq!(qp.stop(), Err(Error::Io));
        // Inject a late NIC write while the owner is quarantined.
        unsafe {
            *bytes(region.raw.as_ptr()) = 99;
        }
        drop(region);
        drop(window);
        assert_eq!(charged.get(), 1);
        FAULTS.with_borrow(|f| assert!(!f.events.contains(&"mw")));
        FAULTS.with_borrow_mut(|f| f.stop_fails = false);
        qp.stop().unwrap();
        assert_eq!(charged.get(), 0);
        FAULTS.with_borrow(|f| {
            let mw = f.events.iter().position(|e| *e == "mw").unwrap();
            let mr = f.events.iter().position(|e| *e == "mr").unwrap();
            assert!(mw < mr);
        });
    }

    #[test]
    /// Invalid completions and poll failures retain owners until fencing succeeds.
    fn cq_errors_and_unknown_ids_require_fence_before_release() {
        for mode in 0..3 {
            let (qp, region, charged) = fixture();
            qp.write_imm(wr(1), &region, 0, 32, 4096, 7, 0).unwrap();
            drop(region);
            FAULTS.with_borrow_mut(|f| {
                f.stop_fails = true;
                f.poll_fails = mode == 2;
            });
            if mode == 0 {
                complete(wr(1), status::REM_ACCESS_ERR, u32::MAX);
            }
            if mode == 1 {
                complete(wr(99), 0, opcode::RDMA_WRITE);
            }
            assert_eq!(drain(&qp), Err(Error::Io));
            assert_eq!(charged.get(), 1);
            FAULTS.with_borrow_mut(|f| f.stop_fails = false);
            qp.stop().unwrap();
            assert_eq!(charged.get(), 0);
        }
    }

    #[test]
    /// A rejected post holds nothing; a posted one holds its region until
    /// its completion.
    fn write_success_and_post_rejection_have_distinct_release_paths() {
        let (qp, region, charged) = fixture();
        FAULTS.with_borrow_mut(|f| f.post_fails = true);
        assert_eq!(
            qp.write_imm(wr(1), &region, 0, 32, 4096, 7, 0),
            Err(Error::Io)
        );
        assert_eq!(Rc::strong_count(&region), 1);
        FAULTS.with_borrow_mut(|f| f.post_fails = false);
        qp.write_imm(wr(1), &region, 0, 32, 4096, 7, 0).unwrap();
        assert_eq!(Rc::strong_count(&region), 2);
        complete(wr(1), 0, opcode::RDMA_WRITE);
        assert_eq!(drain(&qp).unwrap().len(), 1);
        assert_eq!(Rc::strong_count(&region), 1);
        drop(region);
        assert_eq!(charged.get(), 0);
    }

    #[test]
    /// Out-of-order completions release only the corresponding allocation.
    fn reversed_completions_release_only_their_own_allocation() {
        let (qp, first, first_charge) = fixture();
        let (quota, second_charge) = quota();
        let second =
            NativeRegion::with_access(qp.device.clone(), 32, Access::PAGES, quota).unwrap();
        qp.write_imm(wr(1), &first, 0, 32, 4096, 7, 0).unwrap();
        qp.write_imm(wr(2), &second, 0, 32, 8192, 8, 0).unwrap();
        drop(first);
        drop(second);
        complete(wr(2), 0, opcode::RDMA_WRITE);
        drain(&qp).unwrap();
        assert_eq!(first_charge.get(), 1);
        assert_eq!(second_charge.get(), 0);
        complete(wr(1), 0, opcode::RDMA_WRITE);
        drain(&qp).unwrap();
        assert_eq!(first_charge.get(), 0);
    }

    #[test]
    /// Destruction after an unrecoverable fence deliberately retains charged owners.
    fn unrecoverable_destroy_never_releases_quarantined_quota() {
        let (qp, region, charged) = fixture();
        let window = MemoryWindow::new(&qp.device).unwrap();
        qp.bind_window(wr(1), &window, &region, 0, 32).unwrap();
        drop(region);
        drop(window);
        FAULTS.with_borrow_mut(|f| f.stop_fails = true);
        drop(qp);
        assert_eq!(charged.get(), 1);
        FAULTS.with_borrow(|f| assert_eq!(f.events, ["stop"]));
        // The intentional bounded quarantine leak is the required last-resort policy.
    }

    #[test]
    /// Key revocation alone never releases remotely writable memory: the
    /// window holds its last region until the window is freed.
    fn invalidation_cqe_does_not_release_remote_buffer() {
        let (qp, region, charged) = fixture();
        let window = MemoryWindow::new(&qp.device).unwrap();
        let key = qp.bind_window(wr(1), &window, &region, 0, 32).unwrap();
        complete(wr(1), 0, opcode::BIND_MW);
        qp.invalidate_window(wr(2), &window, key).unwrap();
        complete(wr(2), 0, opcode::LOCAL_INV);
        assert_eq!(drain(&qp).unwrap().len(), 2);
        drop(region);
        assert_eq!(charged.get(), 1);
        qp.stop().unwrap();
        assert_eq!(charged.get(), 1);
        drop(window);
        assert_eq!(charged.get(), 0);
    }

    #[test]
    /// Rust ABI records match the native adapter's asserted sizes and offsets.
    fn private_abi_layout_matches_c_static_assertions() {
        assert_eq!(std::mem::size_of::<Port>(), 88);
        assert_eq!(std::mem::offset_of!(Port, mtu), 80);
        assert_eq!(std::mem::size_of::<Endpoint>(), 32);
        assert_eq!(std::mem::offset_of!(Endpoint, qpn), 16);
        assert_eq!(std::mem::size_of::<Completion>(), 32);
        assert_eq!(std::mem::offset_of!(Completion, status), 8);
        assert_eq!(std::mem::offset_of!(Completion, opcode), 12);
        assert_eq!(std::mem::offset_of!(Completion, byte_len), 16);
        assert_eq!(std::mem::offset_of!(Completion, imm), 20);
        assert_eq!(std::mem::offset_of!(Completion, flags), 24);
        assert_eq!(std::mem::offset_of!(Completion, vendor_err), 28);
        assert_eq!(std::mem::align_of::<Completion>(), 8);
    }

    /// A connected fixture QP with explicit depths and a control region.
    fn ranged_fixture(send: u32, recv: u32) -> (Rc<NativeQueuePair>, Rc<NativeRegion>) {
        let (qp, _, _) = fixture();
        let device = qp.device.clone();
        let qp = NativeQueuePair::new_with_depth(
            device.clone(),
            GuardOwner::new(Arc::new(())),
            Arc::new(std::sync::atomic::AtomicBool::new(false)),
            send,
            recv,
        )
        .unwrap();
        qp.connect(qp.endpoint).unwrap();
        let region = NativeRegion::with_access(device, 256, Access::PAGES, quota().0).unwrap();
        (qp, region)
    }

    /// A caller-chosen work request id.
    const fn wr(n: u64) -> u64 {
        (1 << 56) | n
    }

    /// Collect successful completions from one progress call.
    fn drain(qp: &NativeQueuePair) -> Result<Vec<WorkCompletion>> {
        let mut out = Vec::new();
        qp.progress_with(|c| out.push(c))?;
        Ok(out)
    }

    #[test]
    /// Ranged posts complete with the reported length, opcode, and immediate.
    fn ranged_posts_report_opcode_length_and_immediate() {
        let (qp, region) = ranged_fixture(4, 2);
        qp.post_recv(wr(1), &region, 0, 64).unwrap();
        qp.post_recv(wr(2), &region, 64, 64).unwrap();
        qp.send(wr(3), &region, 128, 64).unwrap();
        qp.write_imm(wr(4), &region, 192, 64, 4096, 71, 0xdead_beef)
            .unwrap();
        FAULTS.with_borrow(|f| assert_eq!(f.posts, 4));
        push(Completion {
            id: wr(1),
            opcode: opcode::RECV,
            byte_len: 64,
            ..Completion::default()
        });
        push(Completion {
            id: wr(2),
            opcode: opcode::RECV_RDMA_WITH_IMM,
            byte_len: 4096,
            imm: 7,
            flags: WC_IMM,
            ..Completion::default()
        });
        push(Completion {
            id: wr(4),
            opcode: opcode::RDMA_WRITE,
            ..Completion::default()
        });
        push(Completion {
            id: wr(3),
            opcode: opcode::SEND,
            ..Completion::default()
        });
        let got = drain(&qp).unwrap();
        assert_eq!(
            got,
            [
                WorkCompletion {
                    id: wr(1),
                    opcode: opcode::RECV,
                    byte_len: 64,
                    imm: None,
                },
                WorkCompletion {
                    id: wr(2),
                    opcode: opcode::RECV_RDMA_WITH_IMM,
                    byte_len: 4096,
                    imm: Some(7),
                },
                WorkCompletion {
                    id: wr(4),
                    opcode: opcode::RDMA_WRITE,
                    byte_len: 0,
                    imm: None,
                },
                WorkCompletion {
                    id: wr(3),
                    opcode: opcode::SEND,
                    byte_len: 0,
                    imm: None,
                },
            ]
        );
        assert!(qp.ready());
        assert_eq!(qp.failure(), None);
        // Queue slots are released, so both queues accept work again.
        qp.post_recv(wr(5), &region, 0, 64).unwrap();
        qp.post_recv(wr(6), &region, 64, 64).unwrap();
    }

    #[test]
    /// Bad ids, ranges, regions, and full queues are refused before the adapter.
    fn ranged_posts_validate_before_reaching_the_adapter() {
        let (qp, region) = ranged_fixture(2, 1);
        // Zero length, past the end, and offset overflow.
        assert_eq!(qp.send(wr(1), &region, 0, 0), Err(Error::InvalidRequest));
        assert_eq!(qp.send(wr(1), &region, 250, 8), Err(Error::InvalidRequest));
        assert_eq!(
            qp.post_recv(wr(1), &region, u64::MAX, 8),
            Err(Error::InvalidRequest)
        );
        assert_eq!(
            qp.write_imm(wr(1), &region, 0, 8, u64::MAX - 3, 1, 1),
            Err(Error::InvalidRequest)
        );
        // A region on another device.
        let (other, _) = ranged_fixture(1, 1);
        let foreign =
            NativeRegion::with_access(other.device.clone(), 64, Access::CONTROL, quota().0)
                .unwrap();
        assert_eq!(qp.send(wr(1), &foreign, 0, 8), Err(Error::InvalidRequest));
        // Exactly at the end is fine; duplicate ids are not.
        qp.send(wr(1), &region, 248, 8).unwrap();
        assert_eq!(qp.send(wr(1), &region, 0, 8), Err(Error::InvalidRequest));
        qp.send(wr(2), &region, 0, 8).unwrap();
        assert_eq!(qp.send(wr(3), &region, 0, 8), Err(Error::Overloaded));
        qp.post_recv(wr(4), &region, 0, 8).unwrap();
        assert_eq!(qp.post_recv(wr(5), &region, 0, 8), Err(Error::Overloaded));
        FAULTS.with_borrow(|f| assert_eq!(f.posts, 3));
        // Zero depths are a configuration error.
        for (send, recv) in [(0, 1), (1, 0), (u32::MAX, 1)] {
            assert!(matches!(
                NativeQueuePair::new_with_depth(
                    qp.device.clone(),
                    GuardOwner::new(Arc::new(())),
                    Arc::new(std::sync::atomic::AtomicBool::new(false)),
                    send,
                    recv,
                ),
                Err(Error::InvalidConfiguration)
            ));
        }
        // Not connected and stopped QPs refuse work.
        qp.stop().unwrap();
        assert_eq!(qp.send(wr(9), &region, 0, 8), Err(Error::Unavailable));
    }

    #[test]
    /// A rejected post leaves no record and keeps the QP usable.
    fn ranged_post_rejection_releases_its_slot() {
        let (qp, region) = ranged_fixture(1, 1);
        FAULTS.with_borrow_mut(|f| f.post_fails = true);
        assert_eq!(qp.send(wr(1), &region, 0, 8), Err(Error::Io));
        assert_eq!(qp.post_recv(wr(2), &region, 0, 8), Err(Error::Io));
        FAULTS.with_borrow_mut(|f| f.post_fails = false);
        assert!(qp.ready());
        qp.send(wr(1), &region, 0, 8).unwrap();
        qp.post_recv(wr(2), &region, 0, 8).unwrap();
    }

    #[test]
    /// Wrong opcodes, missing or stray immediates, overlong receives, error
    /// statuses, and unknown ids break the QP and record the failure.
    fn ranged_completion_mismatches_break_the_qp() {
        let cases = [
            // A receive reported as a send.
            (
                Completion {
                    id: wr(1),
                    opcode: opcode::SEND,
                    ..Completion::default()
                },
                0,
            ),
            // Write with immediate without the immediate flag.
            (
                Completion {
                    id: wr(1),
                    opcode: opcode::RECV_RDMA_WITH_IMM,
                    ..Completion::default()
                },
                0,
            ),
            // Plain receive with an immediate.
            (
                Completion {
                    id: wr(1),
                    opcode: opcode::RECV,
                    byte_len: 8,
                    flags: WC_IMM,
                    ..Completion::default()
                },
                0,
            ),
            // More bytes than the receive buffer holds.
            (
                Completion {
                    id: wr(1),
                    opcode: opcode::RECV,
                    byte_len: 65,
                    ..Completion::default()
                },
                0,
            ),
            // A send reported as a receive.
            (
                Completion {
                    id: wr(2),
                    opcode: opcode::RECV,
                    ..Completion::default()
                },
                0,
            ),
            // RNR on the send.
            (
                Completion {
                    id: wr(2),
                    status: status::RNR_RETRY_EXC_ERR,
                    opcode: u32::MAX,
                    vendor_err: 0x88,
                    ..Completion::default()
                },
                status::RNR_RETRY_EXC_ERR,
            ),
            // Unknown id.
            (
                Completion {
                    id: wr(77),
                    opcode: opcode::SEND,
                    ..Completion::default()
                },
                0,
            ),
        ];
        for (completion, expected) in cases {
            let (qp, region) = ranged_fixture(2, 2);
            qp.post_recv(wr(1), &region, 0, 64).unwrap();
            qp.send(wr(2), &region, 64, 64).unwrap();
            push(completion);
            assert_eq!(drain(&qp), Err(Error::Io));
            assert!(qp.stopped());
            assert_eq!(
                qp.failure(),
                Some(Failure {
                    id: completion.id,
                    status: expected,
                    vendor_err: completion.vendor_err,
                })
            );
            // Stop released every queue slot.
            assert_eq!((qp.sends.get(), qp.recvs.get()), (0, 0));
        }
    }

    #[test]
    /// Ranged binds use fresh rkeys, stay tied to one region, and hold it
    /// until the window is freed.
    fn memory_window_binds_with_fresh_keys_and_holds_its_region() {
        let (qp, region) = ranged_fixture(4, 1);
        assert_eq!(region.as_ptr(), unsafe { bytes(region.raw.as_ptr()) });
        let window = MemoryWindow::new(&qp.device).unwrap();
        assert_eq!(window.next_key(), 71);
        let first = qp.bind_window(wr(1), &window, &region, 64, 32).unwrap();
        assert_eq!(first, 71);
        assert_eq!(window.next_key(), 72);
        complete(wr(1), 0, opcode::BIND_MW);
        qp.invalidate_window(wr(2), &window, first).unwrap();
        complete(wr(2), 0, opcode::LOCAL_INV);
        assert_eq!(drain(&qp).unwrap().len(), 2);
        let second = qp.bind_window(wr(3), &window, &region, 0, 256).unwrap();
        assert_ne!(first, second);
        // Low byte wraps, index bits are kept.
        assert_eq!(inc_rkey(0x1234_56ff), 0x1234_5600);
        // Another region, a control region, and out-of-range binds are refused.
        let other =
            NativeRegion::with_access(qp.device.clone(), 256, Access::PAGES, quota().0).unwrap();
        assert_eq!(
            qp.bind_window(wr(4), &window, &other, 0, 8),
            Err(Error::InvalidRequest)
        );
        let control =
            NativeRegion::with_access(qp.device.clone(), 64, Access::CONTROL, quota().0).unwrap();
        let fresh = MemoryWindow::new(&qp.device).unwrap();
        assert_eq!(
            qp.bind_window(wr(4), &fresh, &control, 0, 8),
            Err(Error::InvalidRequest)
        );
        assert_eq!(
            qp.bind_window(wr(4), &fresh, &region, 200, 57),
            Err(Error::InvalidRequest)
        );
        // Invalidating a never-bound window is refused.
        assert_eq!(
            qp.invalidate_window(wr(4), &fresh, 1),
            Err(Error::InvalidRequest)
        );
        // A failed post keeps the key for the next attempt.
        FAULTS.with_borrow_mut(|f| f.post_fails = true);
        assert_eq!(qp.bind_window(wr(4), &fresh, &region, 0, 8), Err(Error::Io));
        FAULTS.with_borrow_mut(|f| f.post_fails = false);
        assert_eq!(fresh.next_key(), 71);
        qp.stop().unwrap();
        drop(qp);
        // The window still holds the region after the QP is gone; freeing
        // the window frees the window before the region.
        let weak = Rc::downgrade(&region);
        drop(region);
        assert!(weak.upgrade().is_some());
        FAULTS.with_borrow_mut(|f| f.events.clear());
        drop(window);
        assert!(weak.upgrade().is_none());
        FAULTS.with_borrow(|f| assert_eq!(f.events, ["mw", "mr"]));
    }

    #[cfg(feature = "native")]
    #[test]
    #[ignore = "requires the installed native adapter and a host with zero usable RDMA ports"]
    /// An installed adapter reports no ports on a deliberately empty test host.
    fn native_no_device() {
        let devices = super::discover().expect("native adapter must be installed for this test");
        assert!(devices.is_empty(), "this is not a no-device host");
    }

    #[cfg(feature = "native")]
    #[test]
    #[ignore = "requires an active type-2B provider; exercises real loopback RC DMA"]
    /// Real loopback write with immediate lands through a bound window, and
    /// an invalidated key rejects the next write.
    fn native_available_provider_write_bind_invalidate_and_fence() {
        let devices = super::discover().expect("native adapter unavailable");
        let name =
            std::env::var("RDMA_VERBS_TEST_DEVICE").expect("explicitly select a test device");
        let device = Rc::new(
            devices
                .into_iter()
                .find(|d| d.name == name)
                .expect("selected provider not active or no type-2B support"),
        );
        let qp = || {
            NativeQueuePair::new_with_depth(
                device.clone(),
                quota().0,
                Arc::new(std::sync::atomic::AtomicBool::new(false)),
                4,
                4,
            )
            .unwrap()
        };
        let receiver = qp();
        let sender = qp();
        receiver.connect(sender.endpoint).unwrap();
        sender.connect(receiver.endpoint).unwrap();
        let region = |access| NativeRegion::with_access(device.clone(), 4096, access, quota().0);
        let destination = region(Access::PAGES_REMOTE_WRITE).unwrap();
        let source = region(Access::PAGES).unwrap();
        let control = region(Access::CONTROL).unwrap();
        unsafe { std::ptr::write_bytes(source.as_ptr(), 0xa5, 4096) };

        /// Poll a real provider until `id` completes or the QP breaks.
        fn wait(qp: &NativeQueuePair, id: u64) -> Result<()> {
            let until = Instant::now() + Duration::from_secs(10);
            loop {
                assert!(Instant::now() < until, "native CQ timeout");
                let mut done = false;
                qp.progress_with(|c| done |= c.id == id)?;
                if done {
                    return Ok(());
                }
                std::thread::sleep(Duration::from_millis(1));
            }
        }

        let window = MemoryWindow::new(&device).unwrap();
        let key = receiver
            .bind_window(wr(1), &window, &destination, 0, 4096)
            .unwrap();
        wait(&receiver, wr(1)).unwrap();
        receiver.post_recv(wr(2), &control, 0, 64).unwrap();
        sender
            .write_imm(wr(3), &source, 0, 4096, destination.address(), key, 7)
            .unwrap();
        wait(&sender, wr(3)).unwrap();
        wait(&receiver, wr(2)).unwrap();
        receiver.invalidate_window(wr(4), &window, key).unwrap();
        wait(&receiver, wr(4)).unwrap();
        // A completed invalidation rejects a write with the old key.
        unsafe { std::ptr::write_bytes(source.as_ptr(), 0xff, 4096) };
        receiver.post_recv(wr(5), &control, 0, 64).unwrap();
        sender
            .write_imm(wr(6), &source, 0, 4096, destination.address(), key, 8)
            .unwrap();
        assert!(
            wait(&sender, wr(6)).is_err(),
            "invalidated rkey still allowed a write"
        );
        receiver.stop().unwrap();
        sender.stop().unwrap();
        let landed = unsafe { std::slice::from_raw_parts(destination.as_ptr(), 4096) };
        assert_eq!(landed, [0xa5; 4096]);
    }
}
