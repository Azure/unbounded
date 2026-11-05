//! Safe Rust wrappers over the C adapter in `native/verbs.c`.
//!
//! Only the native thread uses these. Each type owns one RDMA object and frees
//! it on drop. If freeing fails, the object is leaked on purpose, along with
//! everything it depends on, because the NIC may still be using it.
//!
//! The adapter only wraps libibverbs and librdmacm calls. Policy lives here:
//! the device mode (window or registration, see `registration.rs`), the work
//! request ledger in [`NativeQueuePair`], and the connection manager state
//! machine.
use crate::{DeviceMemory, Error, GuardOwner, Result};

use std::{
    cell::{Cell, RefCell},
    collections::{BTreeMap, VecDeque},
    ffi::{CStr, c_char, c_int, c_void},
    ptr::NonNull,
    rc::{Rc, Weak},
    sync::{
        Arc, Mutex, PoisonError,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
};

#[cfg(any(test, feature = "simulation"))]
#[path = "simulation.rs"]
pub mod simulation;

#[cfg(all(test, feature = "native", target_os = "linux"))]
#[path = "discovery_fault_tests.rs"]
mod discovery_fault_tests;

#[path = "registration.rs"]
mod registration;

#[cfg(test)]
#[path = "registration_tests.rs"]
mod registration_tests;

pub(crate) use registration::HUGE;
use registration::{Cm, Conn, State};

/// One port from `discover`. Matches `struct rdma_verbs_port` in C.
#[repr(C)]
#[derive(Clone, Copy)]
struct Port {
    name: [c_char; 64],

    /// GID index 0.
    gid: [u8; 16],

    mtu: u32,

    lid: u16,

    port: u8,

    link_layer: u8,

    /// The IPv4-mapped RoCE GID at `ipv4_index`, if that is not -1.
    ipv4_gid: [u8; 16],

    ipv4_index: i32,

    /// `PORT_*` facts.
    flags: u32,
}

impl Port {
    /// An empty entry for `discover` to fill.
    const EMPTY: Self = Self {
        name: [0; 64],
        gid: [0; 16],
        mtu: 0,
        lid: 0,
        port: 0,
        link_layer: 0,
        ipv4_gid: [0; 16],
        ipv4_index: -1,
        flags: 0,
    };
}

/// `RDMA_VERBS_PORT_MW`: the device has type 2B memory windows.
const PORT_MW: u32 = 1;

/// `RDMA_VERBS_PORT_RC`: the device can create RC QPs.
const PORT_RC: u32 = 2;

/// A device's capabilities for one port. Matches `struct rdma_verbs_caps`.
#[repr(C)]
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub(crate) struct Caps {
    /// `CAP_*` bits.
    flags: u32,

    /// The port's maximum message size.
    max_msg_sz: u32,

    /// The IPv4-mapped GID's index, or -1.
    ipv4_index: i32,

    /// The IPv4 address in that GID.
    ipv4: [u8; 4],
}

/// `RDMA_VERBS_CAP_MW`: type 2B memory windows.
const CAP_MW: u32 = 1;

/// `RDMA_VERBS_CAP_ODP`: implicit on-demand paging for RC writes.
const CAP_ODP: u32 = 2;

/// One scatter-gather entry. Matches `struct rdma_verbs_sge`.
#[repr(C)]
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub(crate) struct Sge {
    addr: u64,

    length: u32,

    lkey: u32,
}

/// `RDMA_VERBS_POST_IMM`: the write carries an immediate.
const POST_IMM: u32 = 1;

/// `RDMA_VERBS_POST_SIGNALED`: the write completes on this side's CQ.
const POST_SIGNALED: u32 = 2;

/// One connection manager event. Matches `struct rdma_verbs_cm_event`.
#[repr(C)]
#[derive(Clone, Copy, Debug)]
pub(crate) struct CmEvent {
    /// Pass to `cm_ack`.
    handle: *mut c_void,

    id: *mut c_void,

    listen_id: *mut c_void,

    kind: i32,

    status: i32,

    private_len: u32,

    private: [u8; 56],

    reserved: u32,
}

impl Default for CmEvent {
    fn default() -> Self {
        Self {
            handle: std::ptr::null_mut(),
            id: std::ptr::null_mut(),
            listen_id: std::ptr::null_mut(),
            kind: -1,
            status: 0,
            private_len: 0,
            private: [0; 56],
            reserved: 0,
        }
    }
}

/// A QP's address. Swap these with the peer to connect.
#[repr(C)]
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Endpoint {
    /// Port GID. In registration mode, the IPv4-mapped GID.
    pub gid: [u8; 16],

    /// QP number. Nonzero, 24 bits. In registration mode, a token naming the
    /// QP in the connection request.
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
#[cfg(any(test, all(feature = "native", target_os = "linux")))]
const ABI_VERSION: u32 = 7;

/// Completions one [`NativeQueuePair::progress_with`] call reads. A full
/// batch means more may be waiting without a new channel event.
pub(crate) const POLL_BATCH: usize = 32;

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
    /// Page buffers: `LOCAL_WRITE | MW_BIND`. Remote writes only via windows
    /// (or, in registration mode, per-grant registrations).
    pub(crate) const PAGES: Self = Self(1);

    /// Control slots: `LOCAL_WRITE` only.
    pub(crate) const CONTROL: Self = Self(2);

    /// `REMOTE_WRITE` on a registration.
    const REMOTE_WRITE: u32 = 4;

    /// Page buffers plus `REMOTE_WRITE` on the region itself. Tests only:
    /// production grants remote access through memory windows alone.
    #[cfg(test)]
    pub(crate) const PAGES_REMOTE_WRITE: Self = Self(1 | 4);

    /// Marks an implicit ODP region from `rdma_verbs_register_implicit`, or
    /// a registration-mode source region. Never passed to
    /// `rdma_verbs_register`; only a local write source.
    pub(crate) const IMPLICIT: Self = Self(8);

    /// Marks a dma-buf page region from `rdma_verbs_register_dmabuf`. Never
    /// passed to `rdma_verbs_register`; peers write it through windows only.
    pub(crate) const DEVICE: Self = Self(1 | 16);

    /// The raw ABI value.
    pub(crate) fn bits(self) -> u32 {
        self.0
    }
}

/// How a device grants remote access and connects QPs.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum Mode {
    /// Type 2 memory windows bound per grant, implicit ODP for write
    /// sources, QPs connected with plain verbs (mlx5).
    Window,

    /// No windows and no ODP: per-grant and per-write registrations, QPs
    /// connected through the connection manager (MANA). See
    /// `registration.rs`.
    Registration,
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

/// The first failed completion that broke a QP. Recorded only for tests.
#[cfg(test)]
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct Failure {
    /// Work request id of the failed completion.
    pub(crate) id: u64,

    /// Verbs status, for example `IBV_WC_RNR_RETRY_EXC_ERR`. 0 means the
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

    open: unsafe extern "C" fn(*const c_char, c_int) -> *mut c_void,

    close: unsafe extern "C" fn(*mut c_void) -> c_int,

    query_caps: unsafe extern "C" fn(*mut c_void, u8, *mut Caps) -> c_int,

    comp_channel: unsafe extern "C" fn(*mut c_void) -> *mut c_void,

    comp_channel_fd: unsafe extern "C" fn(*mut c_void) -> c_int,

    comp_channel_free: unsafe extern "C" fn(*mut c_void) -> c_int,

    cq_create: unsafe extern "C" fn(*mut c_void, u32, *mut c_void) -> *mut c_void,

    cq_free: unsafe extern "C" fn(*mut c_void) -> c_int,

    req_notify: unsafe extern "C" fn(*mut c_void) -> c_int,

    get_cq_event: unsafe extern "C" fn(*mut c_void, *mut *mut c_void) -> c_int,

    ack_cq_events: unsafe extern "C" fn(*mut c_void, u32),

    qp: unsafe extern "C" fn(*mut c_void, *mut c_void, u8, u32, u32, *mut u32) -> *mut c_void,

    connect: unsafe extern "C" fn(*mut c_void, *const Endpoint, *const Endpoint) -> c_int,

    qp_error: unsafe extern "C" fn(*mut c_void) -> c_int,

    qp_free: unsafe extern "C" fn(*mut c_void) -> c_int,

    register: unsafe extern "C" fn(*mut c_void, u64, u32) -> *mut c_void,

    register_implicit: unsafe extern "C" fn(*mut c_void) -> *mut c_void,

    register_dmabuf: unsafe extern "C" fn(*mut c_void, u64, u64, u64, c_int) -> *mut c_void,

    deregister: unsafe extern "C" fn(*mut c_void) -> c_int,

    bytes: unsafe extern "C" fn(*mut c_void) -> *mut u8,

    lkey: unsafe extern "C" fn(*mut c_void) -> u32,

    reg_mr: unsafe extern "C" fn(*mut c_void, u64, u64, u32, *mut u32, *mut u32) -> *mut c_void,

    dereg_mr: unsafe extern "C" fn(*mut c_void) -> c_int,

    window: unsafe extern "C" fn(*mut c_void, *mut u32) -> *mut c_void,

    window_free: unsafe extern "C" fn(*mut c_void) -> c_int,

    bind: unsafe extern "C" fn(*mut c_void, *mut c_void, *mut c_void, u32, u64, u64, u32) -> c_int,

    invalidate: unsafe extern "C" fn(*mut c_void, u32, u64) -> c_int,

    post_write: unsafe extern "C" fn(*mut c_void, *const Sge, u32, u64, u32, u32, u32, u64) -> c_int,

    post_send: unsafe extern "C" fn(*mut c_void, *const Sge, u32, u64) -> c_int,

    post_recv: unsafe extern "C" fn(*mut c_void, *const Sge, u32, u64) -> c_int,

    poll: unsafe extern "C" fn(*mut c_void, *mut Completion, u32) -> c_int,

    cm_channel: unsafe extern "C" fn(*mut c_void) -> *mut c_void,

    cm_channel_fd: unsafe extern "C" fn(*mut c_void) -> c_int,

    cm_channel_free: unsafe extern "C" fn(*mut c_void) -> c_int,

    cm_listen: unsafe extern "C" fn(*mut c_void, *mut c_void, *const u8, u16, *mut *mut c_void) -> c_int,

    cm_id: unsafe extern "C" fn(*mut c_void, *mut *mut c_void) -> c_int,

    cm_resolve_addr: unsafe extern "C" fn(*mut c_void, *const u8, *const u8, u16, c_int) -> c_int,

    cm_resolve_route: unsafe extern "C" fn(*mut c_void, c_int) -> c_int,

    cm_create_qp: unsafe extern "C" fn(*mut c_void, *mut c_void, *mut c_void, u32, u32, *mut u32) -> *mut c_void,

    cm_connect: unsafe extern "C" fn(*mut c_void, *const u8, u32) -> c_int,

    cm_accept: unsafe extern "C" fn(*mut c_void, *const u8, u32) -> c_int,

    cm_reject: unsafe extern "C" fn(*mut c_void) -> c_int,

    cm_get_event: unsafe extern "C" fn(*mut c_void, *mut CmEvent) -> c_int,

    cm_ack: unsafe extern "C" fn(*mut c_void),

    cm_destroy_id: unsafe extern "C" fn(*mut c_void) -> c_int,
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

    pub(crate) mode: Mode,

    caps: Caps,

    /// The completion channel every CQ on the device reports to.
    comp: Option<NonNull<c_void>>,

    /// Whether each live CQ is armed, by CQ handle.
    armed: RefCell<BTreeMap<usize, Rc<Cell<bool>>>>,

    /// Registration mode: the connection manager.
    cm: Option<Cm>,

    /// Source registrations whose deregistration failed for good. Each one
    /// leaked and quarantined its owner's charge.
    leaked_registrations: Cell<usize>,
}

impl NativeDevice {
    /// A device over `raw` with nothing else created yet.
    fn new(
        api: Rc<Api>,
        raw: NonNull<c_void>,
        quota: Option<Arc<GuardOwner>>,
        name: String,
        endpoint: Endpoint,
        mode: Mode,
    ) -> Self {
        Self {
            api,
            raw,
            quota,
            name,
            endpoint,
            mode,
            caps: Caps::default(),
            comp: None,
            armed: RefCell::new(BTreeMap::new()),
            cm: None,
            leaked_registrations: Cell::new(0),
        }
    }

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

    /// True for the simulated fabric.
    fn simulated(&self) -> bool {
        #[cfg(any(test, feature = "simulation"))]
        return simulation::is_api(&self.api);
        #[cfg(not(any(test, feature = "simulation")))]
        false
    }

    /// File descriptors to wait on for this device's progress: the
    /// completion channel and, in registration mode, the connection manager
    /// channel. Both are nonblocking; poll the owner when one is readable.
    pub(crate) fn fds(&self) -> Vec<c_int> {
        let mut fds = Vec::new();
        if let Some(comp) = self.comp {
            fds.push(unsafe { (self.api.comp_channel_fd)(comp.as_ptr()) });
        }
        if let Some(cm) = &self.cm {
            fds.push(cm.fd());
        }
        fds
    }

    /// Handle every waiting completion channel and connection manager
    /// event without blocking. A CQ with an event is re-armed (then
    /// re-polled) by its QP's next progress.
    pub(crate) fn progress(&self) {
        if let Some(comp) = self.comp {
            loop {
                let mut cq = std::ptr::null_mut();
                if unsafe { (self.api.get_cq_event)(comp.as_ptr(), &mut cq) } != 0 {
                    break;
                }
                unsafe { (self.api.ack_cq_events)(cq, 1) };
                if let Some(armed) = self.armed.borrow().get(&(cq as usize)) {
                    armed.set(false);
                }
            }
        }
        if let Some(cm) = &self.cm {
            cm.progress(self);
        }
    }

    #[cfg(test)]
    /// Source registrations that leaked for good.
    pub(crate) fn leaked_registrations(&self) -> usize {
        self.leaked_registrations.get()
    }

    /// Keep an MR whose deregistration failed for good, charged forever.
    fn leak_registration(&self, quota: &Arc<GuardOwner>) {
        self.leaked_registrations
            .set(self.leaked_registrations.get() + 1);
        quota.quarantine();
        std::mem::forget(quota.clone());
    }

    /// Release this device's own objects before closing. False if any
    /// leaked.
    fn release(&mut self) -> bool {
        if let Some(cm) = &mut self.cm {
            if !cm.close(&self.api) {
                return false;
            }
            self.cm = None;
        }
        if let Some(comp) = self.comp {
            if unsafe { (self.api.comp_channel_free)(comp.as_ptr()) } != 0 {
                return false;
            }
            self.comp = None;
        }
        true
    }
}

impl Drop for NativeDevice {
    /// Close the device. On failure, keep the library and charged owner.
    fn drop(&mut self) {
        if !self.release() || unsafe { (self.api.close)(self.raw.as_ptr()) } != 0 {
            if let Some(quota) = self.quota.take() {
                quota.quarantine();
                std::mem::forget(quota);
            }
            std::mem::forget(self.api.clone());
        }
    }
}

/// List ports and open a device for each. Fails on more than 64 ports.
/// Skips ports whose device will not open or set up.
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
///
/// Picks each port's mode: window mode on devices with memory windows,
/// registration mode on devices that can create RC QPs and have an
/// IPv4-mapped GID (the connection manager addresses peers by IPv4).
fn discover_with_api(
    api: Rc<Api>,
    mut quota: impl FnMut() -> Option<Arc<GuardOwner>>,
) -> Result<Vec<NativeDevice>> {
    let discovery_quota = quota();
    let mut ports = [Port::EMPTY; 64];
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
        let (mode, gid) = if port.flags & PORT_MW != 0 {
            (Mode::Window, port.gid)
        } else if port.flags & PORT_RC != 0 && port.ipv4_index >= 0 {
            (Mode::Registration, port.ipv4_gid)
        } else {
            continue;
        };
        let open_quota = quota();
        let raw =
            unsafe { (api.open)(port.name.as_ptr(), c_int::from(mode == Mode::Registration)) };
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
        // From here dropping `device` releases whatever was set up.
        let mut device = NativeDevice::new(
            api.clone(),
            raw,
            open_quota,
            name,
            Endpoint {
                gid,
                qpn: 0,
                psn: 0,
                mtu: port.mtu,
                lid: port.lid,
                port: port.port,
                link_layer: port.link_layer,
            },
            mode,
        );
        if unsafe { (api.query_caps)(raw.as_ptr(), port.port, &mut device.caps) } != 0 {
            continue;
        }
        if mode == Mode::Registration
            && (u64::from(device.caps.max_msg_sz) < registration::CHUNK
                || device.caps.ipv4_index < 0
                || registration::ipv4_of(&gid) != Some(device.caps.ipv4))
        {
            continue;
        }
        let Some(comp) = NonNull::new(unsafe { (api.comp_channel)(raw.as_ptr()) }) else {
            continue;
        };
        device.comp = Some(comp);
        if mode == Mode::Registration {
            match Cm::start(&api, raw, device.caps.ipv4, registration::cm_port()) {
                Ok(Some(cm)) => device.cm = Some(cm),
                Ok(None) => continue,
                Err(()) => {
                    if let Some(quota) = device.quota.take() {
                        quota.quarantine();
                        std::mem::forget(quota);
                    }
                    std::mem::forget(device);
                    return Err(Error::Unavailable);
                }
            }
        }
        devices.push(device);
    }
    Ok(devices)
}

/// A registered memory buffer (MR) the NIC can read and write.
///
/// Posts do not track which ranges the NIC may touch; their caller must.
/// Holds the node's guard and leaks it if deregister fails.
///
/// A dma-buf region (`memory` is set) has no CPU mapping. It can only be
/// bound to windows for receives; sends, writes, and receives reject it.
pub(crate) struct NativeRegion {
    device: Rc<NativeDevice>,

    /// `None` for a registration-mode source region, which registers each
    /// write's memory instead.
    raw: Option<NonNull<c_void>>,

    /// The local key. 0 without `raw`.
    lkey: u32,

    /// The dma-buf owner. Kept alive while registered, and leaked with the
    /// region if deregister fails.
    memory: Option<DeviceMemory>,

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
        let bits = register_bits(&device, access);
        let raw = NonNull::new(unsafe {
            (device.api.register)(device.raw.as_ptr(), length as u64, bits)
        })
        .ok_or(Error::Unavailable)?;
        let lkey = unsafe { (device.api.lkey)(raw.as_ptr()) };
        Ok(Rc::new(Self {
            device,
            raw: Some(raw),
            lkey,
            memory: None,
            length,
            quota: Some(quota),
            access,
        }))
    }

    /// Register all of `memory`'s dma-buf range as page buffers. `memory`
    /// stays alive until deregister succeeds, or forever if it fails.
    /// Window mode only. The owner registers through [`Registering::dmabuf`]
    /// instead; tests build device pools with this synchronous form.
    #[cfg(test)]
    pub(crate) fn dmabuf(
        device: Rc<NativeDevice>,
        memory: DeviceMemory,
        quota: Arc<GuardOwner>,
    ) -> Result<Rc<Self>> {
        use std::os::fd::AsRawFd;

        let length = memory.length();
        if length == 0 || u64::try_from(length).is_err() {
            return Err(Error::InvalidRange);
        }
        if device.mode != Mode::Window {
            return Err(Error::Unavailable);
        }
        let raw = NonNull::new(unsafe {
            (device.api.register_dmabuf)(
                device.raw.as_ptr(),
                memory.offset(),
                length as u64,
                memory.iova(),
                memory.fd().as_raw_fd(),
            )
        })
        .ok_or(Error::Unavailable)?;
        let lkey = unsafe { (device.api.lkey)(raw.as_ptr()) };
        Ok(Rc::new(Self {
            device,
            raw: Some(raw),
            lkey,
            memory: Some(memory),
            length,
            quota: Some(quota),
            access: Access::DEVICE,
        }))
    }

    /// The dma-buf owner, if this is a dma-buf region.
    pub(crate) fn memory(&self) -> Option<&DeviceMemory> {
        self.memory.as_ref()
    }

    /// True for a dma-buf region from [`Self::dmabuf`].
    pub(crate) fn is_device(&self) -> bool {
        self.memory.is_some()
    }

    /// The zero-copy write source region over the whole process address
    /// space. `None` if the device cannot provide one.
    ///
    /// Window mode registers one implicit on-demand paging MR, and needs
    /// the device to support implicit ODP for RC writes. Registration mode
    /// registers nothing here: each write registers its own source memory
    /// while it is in flight.
    ///
    /// The region owns no bytes: an `offset` is a virtual address, and
    /// [`Self::as_ptr`] is null. Callers must keep the source memory alive
    /// and unmodified until the post's completion or the QP's destruction.
    pub(crate) fn implicit(device: Rc<NativeDevice>, quota: Arc<GuardOwner>) -> Option<Rc<Self>> {
        let raw = match device.mode {
            Mode::Registration => None,
            Mode::Window => {
                if device.caps.flags & CAP_ODP == 0 {
                    return None;
                }
                Some(NonNull::new(unsafe {
                    (device.api.register_implicit)(device.raw.as_ptr())
                })?)
            }
        };
        let lkey = raw.map_or(0, |raw| unsafe { (device.api.lkey)(raw.as_ptr()) });
        Some(Rc::new(Self {
            device,
            raw,
            lkey,
            memory: None,
            length: usize::MAX,
            quota: Some(quota),
            access: Access::IMPLICIT,
        }))
    }

    /// True for an address-space region from [`Self::implicit`].
    pub(crate) fn is_implicit(&self) -> bool {
        self.access == Access::IMPLICIT
    }

    /// True for a registration-mode source region: writes register their
    /// own memory.
    fn is_dynamic(&self) -> bool {
        self.raw.is_none()
    }

    /// The start of the registered buffer, valid while `self` lives.
    ///
    /// The caller must not read or write any range the NIC may still access.
    /// Null for an implicit or dma-buf region.
    pub(crate) fn as_ptr(&self) -> *mut u8 {
        match self.raw {
            Some(raw) if !self.is_device() => unsafe { (self.device.api.bytes)(raw.as_ptr()) },
            _ => std::ptr::null_mut(),
        }
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
        if let Some(memory) = &self.memory {
            return memory.iova();
        }
        let Some(raw) = self.raw else {
            return 0;
        };
        #[cfg(any(test, feature = "simulation"))]
        if simulation::is_api(&self.device.api) {
            return simulation::address(raw);
        }
        unsafe { (self.device.api.bytes)(raw.as_ptr()) as u64 }
    }

    /// Registered size in bytes.
    pub(crate) fn length(&self) -> usize {
        self.length
    }

    /// The scatter-gather entry for `[offset, offset + length)`.
    fn sge(&self, offset: u64, length: u32) -> Sge {
        let addr = if self.is_implicit() {
            offset
        } else {
            self.as_ptr() as u64 + offset
        };
        Sge {
            addr,
            length,
            lkey: self.lkey,
        }
    }
}

/// The native access bits for a region of `access` on `device`.
fn register_bits(device: &NativeDevice, access: Access) -> u32 {
    // Registration mode has no windows: page buffers need no MW_BIND, and
    // grants register them for remote write.
    match device.mode {
        Mode::Registration if access.bits() & Access::PAGES.bits() != 0 => {
            Access::CONTROL.bits() | (access.bits() & Access::REMOTE_WRITE)
        }
        _ => access.bits(),
    }
}

/// Where a background registration leaves its result.
struct RegisterSlot {
    done: AtomicBool,

    /// The native region handle, 0 on failure. Valid once `done`.
    raw: AtomicUsize,
}

/// Wakes the owner when a background registration finishes.
pub(crate) type RegisterWake = Arc<dyn Fn() + Send + Sync>;

/// A region registration running off the owner thread.
///
/// Pinning and registering a large buffer (`ibv_reg_mr`, `ibv_reg_dmabuf_mr`)
/// can block for a long time, so the native call runs on a short-lived
/// helper thread while the owner keeps serving. Only that one call leaves
/// the owner thread; the result becomes a [`NativeRegion`] on the owner in
/// [`Self::finish`]. Under the simulation the call runs inline (the
/// simulation is single-threaded) and `Simulation::hold_registrations`
/// keeps it pending.
///
/// Dropped unfinished, it frees a finished registration, but leaks the
/// device, the dma-buf owner, and the charge (quarantined) if the helper is
/// still running, since the helper may still use them.
pub(crate) struct Registering {
    device: Option<Rc<NativeDevice>>,

    memory: Option<DeviceMemory>,

    length: usize,

    access: Access,

    quota: Option<Arc<GuardOwner>>,

    slot: Arc<RegisterSlot>,
}

impl Registering {
    /// Start allocating and registering `length` bytes with `access`, as
    /// [`NativeRegion::with_access`] does. `wake` runs once it finishes.
    pub(crate) fn host(
        device: Rc<NativeDevice>,
        length: usize,
        access: Access,
        quota: Arc<GuardOwner>,
        wake: RegisterWake,
    ) -> Result<Self> {
        if length == 0 || u64::try_from(length).is_err() {
            return Err(Error::InvalidRange);
        }
        let bits = register_bits(&device, access);
        let register = device.api.register;
        let raw = device.raw.as_ptr() as usize;
        Ok(Self::start(
            device,
            None,
            length,
            access,
            quota,
            wake,
            move || unsafe { register(raw as *mut c_void, length as u64, bits) } as usize,
        ))
    }

    /// Start registering all of `memory`'s dma-buf range, as
    /// [`NativeRegion::dmabuf`] does. `wake` runs once it finishes.
    pub(crate) fn dmabuf(
        device: Rc<NativeDevice>,
        memory: DeviceMemory,
        quota: Arc<GuardOwner>,
        wake: RegisterWake,
    ) -> Result<Self> {
        use std::os::fd::AsRawFd;

        let length = memory.length();
        if length == 0 || u64::try_from(length).is_err() {
            return Err(Error::InvalidRange);
        }
        if device.mode != Mode::Window {
            return Err(Error::Unavailable);
        }
        let register = device.api.register_dmabuf;
        let raw = device.raw.as_ptr() as usize;
        let (offset, iova, fd) = (memory.offset(), memory.iova(), memory.fd().as_raw_fd());
        // `memory` (and so `fd`) lives in the returned value until the
        // helper is done, or forever if it is dropped first.
        Ok(Self::start(
            device,
            Some(memory),
            length,
            Access::DEVICE,
            quota,
            wake,
            move || unsafe { register(raw as *mut c_void, offset, length as u64, iova, fd) }
                as usize,
        ))
    }

    fn start(
        device: Rc<NativeDevice>,
        memory: Option<DeviceMemory>,
        length: usize,
        access: Access,
        quota: Arc<GuardOwner>,
        wake: RegisterWake,
        call: impl FnOnce() -> usize + Send + 'static,
    ) -> Self {
        let slot = Arc::new(RegisterSlot {
            done: AtomicBool::new(false),
            raw: AtomicUsize::new(0),
        });
        #[cfg(any(test, feature = "simulation"))]
        let inline = simulation::is_api(&device.api);
        #[cfg(not(any(test, feature = "simulation")))]
        let inline = false;
        let finish = {
            let slot = slot.clone();
            move |raw: usize| {
                slot.raw.store(raw, Ordering::Release);
                slot.done.store(true, Ordering::Release);
                wake();
            }
        };
        if inline {
            finish(call());
        } else {
            // A shared slot so a failed spawn can still run the call here.
            let call = Arc::new(Mutex::new(Some(call)));
            let spawned = {
                let call = call.clone();
                let finish = finish.clone();
                std::thread::Builder::new()
                    .name("racer-rdma-register".into())
                    .spawn(move || {
                        let call = call.lock().unwrap_or_else(PoisonError::into_inner).take();
                        if let Some(call) = call {
                            finish(call());
                        }
                    })
            };
            if spawned.is_err() {
                let call = call.lock().unwrap_or_else(PoisonError::into_inner).take();
                if let Some(call) = call {
                    finish(call());
                }
            }
        }
        Self {
            device: Some(device),
            memory,
            length,
            access,
            quota: Some(quota),
            slot,
        }
    }

    /// True once [`Self::finish`] has a result.
    pub(crate) fn ready(&self) -> bool {
        if !self.slot.done.load(Ordering::Acquire) {
            return false;
        }
        #[cfg(any(test, feature = "simulation"))]
        if let Some(device) = &self.device
            && simulation::is_api(&device.api)
            && simulation::registrations_held(device.raw.as_ptr())
        {
            return false;
        }
        true
    }

    /// The registered region. Call once [`Self::ready`].
    pub(crate) fn finish(mut self) -> Result<Rc<NativeRegion>> {
        debug_assert!(self.ready());
        self.take().ok_or(Error::Unavailable)
    }

    /// Turn a finished registration into its region; `None` if it failed.
    fn take(&mut self) -> Option<Rc<NativeRegion>> {
        let raw = NonNull::new(self.slot.raw.swap(0, Ordering::AcqRel) as *mut c_void);
        let device = self.device.take()?;
        let memory = self.memory.take();
        let quota = self.quota.take();
        let raw = raw?;
        let lkey = unsafe { (device.api.lkey)(raw.as_ptr()) };
        Some(Rc::new(NativeRegion {
            device,
            raw: Some(raw),
            lkey,
            memory,
            length: self.length,
            quota,
            access: self.access,
        }))
    }
}

impl Drop for Registering {
    fn drop(&mut self) {
        if self.device.is_none() {
            return;
        }
        if self.slot.done.load(Ordering::Acquire) {
            // Deregisters (or leaks) through `NativeRegion::drop`.
            drop(self.take());
            return;
        }
        // The helper may still use the device and the dma-buf: leak them
        // with the charge, which can then never drain.
        std::mem::forget(self.device.take());
        std::mem::forget(self.memory.take());
        if let Some(quota) = self.quota.take() {
            quota.quarantine();
            std::mem::forget(quota);
        }
    }
}

impl Drop for NativeRegion {
    /// Deregister. On failure, leak the device, guard, and dma-buf owner.
    fn drop(&mut self) {
        let Some(raw) = self.raw else {
            return;
        };
        if unsafe { (self.device.api.deregister)(raw.as_ptr()) } != 0 {
            std::mem::forget(self.device.clone());
            std::mem::forget(self.memory.take());
            self.quota.as_ref().unwrap().quarantine();
            std::mem::forget(self.quota.take());
        }
    }
}

/// A reusable grant slot.
///
/// Window mode: a type 2 memory window for ranged binds. Each bind uses a
/// fresh rkey (`ibv_inc_rkey` of the previous one), so a stale key from an
/// earlier grant never matches.
///
/// Registration mode: the holder of one grant's remote-write registration.
/// Each bind registers the granted bytes and uses that registration's rkey.
///
/// Either way the window keeps the last region it was bound to alive until
/// it is freed, because the NIC may still accept writes through it.
pub(crate) struct MemoryWindow {
    device: Rc<NativeDevice>,

    /// The memory window. `None` in registration mode.
    raw: Option<NonNull<c_void>>,

    /// Key the next bind will use (window mode).
    next_key: Cell<u32>,

    region: RefCell<Option<Rc<NativeRegion>>>,

    /// Registration mode: the live grant's MR and rkey.
    grant: Cell<Option<(NonNull<c_void>, u32)>>,
}

impl MemoryWindow {
    /// Allocate a window on `device`.
    pub(crate) fn new(device: &Rc<NativeDevice>) -> Result<Rc<Self>> {
        let mut key = 0;
        let raw = match device.mode {
            Mode::Registration => None,
            Mode::Window => Some(
                NonNull::new(unsafe { (device.api.window)(device.raw.as_ptr(), &mut key) })
                    .ok_or(Error::Unavailable)?,
            ),
        };
        Ok(Rc::new(Self {
            device: device.clone(),
            raw,
            next_key: Cell::new(key),
            region: RefCell::new(None),
            grant: Cell::new(None),
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
    /// Free the window, or deregister a live grant. On failure, leak the
    /// device and the bound region.
    fn drop(&mut self) {
        let freed = match (self.raw, self.grant.get()) {
            (Some(raw), _) => unsafe { (self.device.api.window_free)(raw.as_ptr()) == 0 },
            (None, Some((mr, _))) => unsafe { (self.device.api.dereg_mr)(mr.as_ptr()) == 0 },
            (None, None) => true,
        };
        if !freed {
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
struct Pending<T> {
    /// Expected completion opcode. Ignored for receives.
    opcode: u32,

    /// Posted on the receive queue.
    recv: bool,

    /// Receive buffer length.
    length: u32,

    _region: Rc<NativeRegion>,

    _window: Option<Rc<MemoryWindow>>,

    /// Registration mode: the write's source registrations, released when it
    /// completes.
    mrs: Vec<NonNull<c_void>>,

    /// The caller's record for this work, attached after a successful post.
    user: Option<T>,
}

impl<T> Pending<T> {
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
/// Tracks posted work: this is the only ledger of work requests, and callers
/// attach their own record `T` to each posted request with [`Self::attach`].
/// [`Self::stop`] is the only way to be sure the NIC is done with its memory.
pub(crate) struct NativeQueuePair<T = ()> {
    /// Keeps every QP and CQ leak charged to its owner.
    quota: Arc<GuardOwner>,

    /// Publishes successful DMA stop even when it occurs during destructor retry.
    stop_observer: Arc<std::sync::atomic::AtomicBool>,

    device: Rc<NativeDevice>,

    /// The CQ both queues report to.
    cq: NonNull<c_void>,

    /// The CQ is armed on the device's completion channel.
    armed: Rc<Cell<bool>>,

    /// The QP handle and its connection state.
    conn: Rc<Conn>,

    pub(crate) endpoint: Endpoint,

    /// Stop succeeded. The NIC is done.
    stopped: Cell<bool>,

    /// Stop was requested. No new work.
    terminating: Cell<bool>,

    connected: Cell<bool>,

    /// Posted work by ID.
    pending: RefCell<BTreeMap<u64, Pending<T>>>,

    /// Records of work released by a successful stop, kept for
    /// [`Self::take_records`] so the caller can still answer them.
    released: RefCell<Vec<T>>,

    /// Registration mode: bind and invalidate completions produced here.
    synthetic: RefCell<VecDeque<Completion>>,

    /// Registration mode: source registrations to release at stop, because
    /// their deregistration failed or their write was only partly posted.
    held: RefCell<Vec<NonNull<c_void>>>,

    /// Send queue limit and current use.
    send_capacity: u32,

    sends: Cell<u32>,

    /// Receive queue limit and current use.
    recv_capacity: u32,

    recvs: Cell<u32>,

    /// The first completion that broke the QP.
    #[cfg(test)]
    failure: Cell<Option<Failure>>,
}

impl<T> NativeQueuePair<T> {
    /// Create a QP with `send_depth` send and `recv_depth` receive WRs, and
    /// a CQ for both. Both depths must be nonzero.
    ///
    /// The CQ holds a full error flush. In registration mode a counted send
    /// may post up to `WRITE_WRS` pieces, so the hardware send queue and the
    /// CQ hold that many entries per send, and the QP itself only exists
    /// once the connection manager connects; the endpoint's `qpn` is a
    /// random 24-bit token naming it in the setup exchange.
    pub(crate) fn new_with_depth(
        device: Rc<NativeDevice>,
        quota: Arc<GuardOwner>,
        stop_observer: Arc<std::sync::atomic::AtomicBool>,
        send_depth: u32,
        recv_depth: u32,
        rng: &racer_io::Rng,
    ) -> Result<Rc<Self>> {
        if send_depth == 0 || recv_depth == 0 || send_depth.checked_add(recv_depth).is_none() {
            return Err(Error::InvalidConfiguration);
        }
        let send_wrs = match device.mode {
            Mode::Window => send_depth,
            Mode::Registration => send_depth
                .checked_mul(registration::WRITE_WRS)
                .ok_or(Error::InvalidConfiguration)?,
        };
        let depth = send_wrs
            .checked_add(recv_depth)
            .filter(|d| *d <= i32::MAX as u32)
            .ok_or(Error::InvalidConfiguration)?;
        let api = &device.api;
        let channel = device.comp.map_or(std::ptr::null_mut(), NonNull::as_ptr);
        let cq = NonNull::new(unsafe { (api.cq_create)(device.raw.as_ptr(), depth, channel) })
            .ok_or(Error::Unavailable)?;

        /// Free `cq` after a failed setup, leaking the device on failure.
        fn abandon(device: Rc<NativeDevice>, quota: Arc<GuardOwner>, cq: NonNull<c_void>) {
            if unsafe { (device.api.cq_free)(cq.as_ptr()) } != 0 {
                quota.quarantine();
                std::mem::forget(quota);
                std::mem::forget(device);
            }
        }

        let mut qpn = 0;
        let qp = match device.mode {
            Mode::Window => {
                let raw = NonNull::new(unsafe {
                    (api.qp)(
                        device.raw.as_ptr(),
                        cq.as_ptr(),
                        device.endpoint.port,
                        send_depth,
                        recv_depth,
                        &mut qpn,
                    )
                });
                let Some(raw) = raw else {
                    if qpn == u32::MAX {
                        // The QP leaked and may still use the CQ.
                        quota.quarantine();
                        std::mem::forget(quota);
                        std::mem::forget(device);
                    } else {
                        abandon(device, quota, cq);
                    }
                    return Err(Error::Unavailable);
                };
                Some(raw)
            }
            Mode::Registration => {
                let cm = device.cm.as_ref().expect("registration mode has a CM");
                match cm.token(device.simulated(), rng) {
                    Ok(token) => qpn = token,
                    Err(error) => {
                        abandon(device, quota, cq);
                        return Err(error);
                    }
                }
                None
            }
        };
        let mut psn = [0; 4];
        if device.simulated() {
            psn = qpn.to_be_bytes();
        } else if rng.fill(&mut psn).is_err() {
            if let Some(raw) = qp
                && unsafe { (api.qp_free)(raw.as_ptr()) } != 0
            {
                quota.quarantine();
                std::mem::forget(quota);
                std::mem::forget(device);
                return Err(Error::Io);
            }
            abandon(device, quota, cq);
            return Err(Error::Io);
        }
        let endpoint = Endpoint {
            qpn,
            psn: u32::from_be_bytes(psn) & 0xffffff,
            ..device.endpoint
        };
        let conn = Rc::new(Conn::new(cq, send_wrs, recv_depth, qpn));
        conn.qp.set(qp);
        if let Some(cm) = &device.cm {
            cm.register(&conn);
        }
        let armed = Rc::new(Cell::new(false));
        device
            .armed
            .borrow_mut()
            .insert(cq.as_ptr() as usize, armed.clone());
        Ok(Rc::new(Self {
            quota,
            stop_observer,
            device,
            cq,
            armed,
            conn,
            endpoint,
            stopped: Cell::new(false),
            terminating: Cell::new(false),
            connected: Cell::new(false),
            pending: RefCell::new(BTreeMap::new()),
            released: RefCell::new(Vec::new()),
            synthetic: RefCell::new(VecDeque::new()),
            held: RefCell::new(Vec::new()),
            send_capacity: send_depth,
            sends: Cell::new(0),
            recv_capacity: recv_depth,
            recvs: Cell::new(0),
            #[cfg(test)]
            failure: Cell::new(None),
        }))
    }

    /// Connect to `remote`. Once only. On failure, stops the QP.
    ///
    /// `initiator` is the side that opened the link. In window mode the QP
    /// is ready at once and `initiator` is ignored. In registration mode the
    /// connection manager connects asynchronously, driven by
    /// [`NativeDevice::progress`] (which [`Self::progress_with`] also runs
    /// until connected): the initiator starts resolving the responder's
    /// address, and the responder registers the initiator's token so its
    /// listener accepts that initiator's request, and only that one. Posts
    /// other than receives fail with [`Error::Unavailable`] until
    /// [`Self::ready`].
    pub(crate) fn connect(&self, remote: Endpoint, initiator: bool) -> Result<()> {
        remote.validate()?;
        if self.stopped.get()
            || self.terminating.get()
            || self.connected.get()
            || remote.link_layer != self.endpoint.link_layer
        {
            return Err(Error::InvalidRequest);
        }
        let connected = match (self.device.mode, &self.device.cm) {
            (Mode::Registration, Some(cm)) => {
                self.conn.peer.set(remote.qpn);
                match registration::ipv4_of(&remote.gid) {
                    None => Err(Error::Unavailable),
                    Some(_) if !initiator => Ok(()),
                    Some(address) => cm.connect(&self.device.api, &self.conn, address),
                }
            }
            _ => {
                let qp = self.conn.qp.get().ok_or(Error::Unavailable)?;
                if unsafe { (self.device.api.connect)(qp.as_ptr(), &self.endpoint, &remote) } != 0 {
                    Err(Error::Unavailable)
                } else {
                    self.conn.state.set(State::Established);
                    Ok(())
                }
            }
        };
        if let Err(error) = connected {
            self.stop()?;
            return Err(error);
        }
        self.connected.set(true);
        Ok(())
    }

    /// True if connected, established, and not stopping.
    /// Completions this QP made up itself (registration mode binds and
    /// invalidates) that the next progress call reports. They raise no
    /// channel event, so the owner must poll again without waiting.
    pub(crate) fn has_synthetic(&self) -> bool {
        !self.synthetic.borrow().is_empty()
    }

    pub(crate) fn ready(&self) -> bool {
        self.connected.get()
            && !self.stopped.get()
            && !self.terminating.get()
            && self.conn.state.get() == State::Established
            && self.conn.qp.get().is_some()
    }

    #[cfg(test)]
    /// True once stop succeeded.
    pub(crate) fn stopped(&self) -> bool {
        self.stopped.get()
    }

    /// Attach the caller's record to posted work `id`. It is handed back
    /// with the work's completion, or by [`Self::take_records`].
    pub(crate) fn attach(&self, id: u64, record: T) {
        if let Some(pending) = self.pending.borrow_mut().get_mut(&id) {
            pending.user = Some(record);
        }
    }

    /// Posted send-queue work not yet completed.
    pub(crate) fn sends(&self) -> u32 {
        self.sends.get()
    }

    /// Take every attached record, leaving the work itself (and the memory
    /// it holds) tracked until it completes or the QP stops.
    pub(crate) fn take_records(&self) -> Vec<T> {
        let mut records = std::mem::take(&mut *self.released.borrow_mut());
        records.extend(
            self.pending
                .borrow_mut()
                .values_mut()
                .filter_map(|pending| pending.user.take()),
        );
        records
    }

    /// Add `pending` under `id` and count it against its queue.
    fn track(&self, id: u64, pending: Pending<T>) {
        let counter = if pending.recv {
            &self.recvs
        } else {
            &self.sends
        };
        counter.set(counter.get() + 1);
        self.pending.borrow_mut().insert(id, pending);
    }

    /// Remove the work under `id` and release its queue slot. Its source
    /// registrations are released too.
    fn untrack(&self, id: u64) -> Option<Pending<T>> {
        let mut pending = self.pending.borrow_mut().remove(&id)?;
        let counter = if pending.recv {
            &self.recvs
        } else {
            &self.sends
        };
        counter.set(counter.get() - 1);
        registration::release(&self.device, std::mem::take(&mut pending.mrs), &self.held);
        Some(pending)
    }

    /// Read up to 32 completions without blocking and report each successful
    /// completion, with its attached record, to `sink` after its work is
    /// released. Returns how many.
    ///
    /// Re-arms the CQ on the device's completion channel before polling, so
    /// a completion that lands after this poll wakes the channel.
    ///
    /// Any failed or unexpected completion, or a failed connection, stops
    /// the QP and returns [`Error::Io`]. Tests can read the first failed
    /// completion through `failure`.
    pub(crate) fn progress_with(
        &self,
        mut sink: impl FnMut(WorkCompletion, Option<T>),
    ) -> Result<usize> {
        if self.stopped.get() {
            return Ok(0);
        }
        if self.terminating.get() {
            self.stop()?;
            return Err(Error::Cancelled);
        }
        let api = &self.device.api;
        if self.device.mode == Mode::Registration && self.conn.state.get() != State::Established {
            self.device.progress();
        }
        if self.device.comp.is_some() && !self.armed.get() {
            // Arm, then poll: a completion between the two still raises an
            // event, and one before is polled below.
            let _ = unsafe { (api.req_notify)(self.cq.as_ptr()) };
            self.armed.set(true);
        }
        let mut completions = [Completion::default(); POLL_BATCH];
        let mut n = 0;
        {
            let mut synthetic = self.synthetic.borrow_mut();
            while n < completions.len()
                && let Some(c) = synthetic.pop_front()
            {
                completions[n] = c;
                n += 1;
            }
        }
        let room = (completions.len() - n) as u32;
        if room > 0 {
            let got = unsafe { (api.poll)(self.cq.as_ptr(), completions[n..].as_mut_ptr(), room) };
            if !(0..=room as c_int).contains(&got) {
                self.stop()?;
                return Err(Error::Io);
            }
            n += got as usize;
        }
        for c in &mut completions[..n] {
            if self.device.mode == Mode::Registration
                && c.status == 0
                && c.opcode == opcode::RECV_RDMA_WITH_IMM
                && c.flags & WC_IMM != 0
            {
                // The top byte counts the pieces before the last one.
                let whole = u64::from(c.imm >> 24) * registration::CHUNK + u64::from(c.byte_len);
                c.imm &= registration::TAG_MASK;
                c.byte_len = u32::try_from(whole).unwrap_or(u32::MAX);
            }
            let valid = self
                .pending
                .borrow()
                .get(&c.id)
                .is_some_and(|p| p.matches(c));
            if !valid {
                #[cfg(test)]
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
            let mut pending = self.untrack(c.id).ok_or(Error::Io)?;
            let user = pending.user.take();
            drop(pending);
            sink(
                WorkCompletion {
                    id: c.id,
                    opcode: c.opcode,
                    byte_len: c.byte_len,
                    imm: (c.flags & WC_IMM != 0).then_some(c.imm),
                },
                user,
            );
        }
        if self.conn.state.get() == State::Failed {
            self.stop()?;
            return Err(Error::Io);
        }
        Ok(n)
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
        let api = &self.device.api;
        if self.conn.state.get() != State::Failed {
            self.conn.state.set(State::Failed);
        }
        if let Some(qp) = self.conn.qp.get() {
            let _ = unsafe { (api.qp_error)(qp.as_ptr()) };
            if unsafe { (api.qp_free)(qp.as_ptr()) } != 0 {
                return Err(Error::Io);
            }
            self.conn.qp.set(None);
        }
        if let Some(cm) = &self.device.cm
            && !cm.destroy_id(api, &self.conn)
        {
            return Err(Error::Io);
        }
        self.stopped.set(true);
        self.stop_observer
            .store(true, std::sync::atomic::Ordering::Release);
        std::sync::atomic::fence(std::sync::atomic::Ordering::Acquire);
        let pending = std::mem::take(&mut *self.pending.borrow_mut());
        self.sends.set(0);
        self.recvs.set(0);
        self.synthetic.borrow_mut().clear();
        self.conn.queued.borrow_mut().clear();
        let mut records = Vec::new();
        for (_, mut p) in pending {
            registration::release(&self.device, std::mem::take(&mut p.mrs), &self.held);
            records.extend(p.user.take());
        }
        // No QP can use them now: retry once, then keep them for good.
        let held = std::mem::take(&mut *self.held.borrow_mut());
        for mr in held {
            if unsafe { (api.dereg_mr)(mr.as_ptr()) } != 0 {
                self.device.leak_registration(&self.quota);
            }
        }
        self.released.borrow_mut().extend(records);
        Ok(())
    }

    /// Validate and record a ranged post under the caller's `id`, then run
    /// `post`, which may add source registrations to its argument. A
    /// rejected post leaves nothing behind; the QP stays usable.
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
        post: impl FnOnce(&mut Vec<NonNull<c_void>>) -> c_int,
    ) -> Result<()> {
        if (region.is_implicit() && (recv || opcode != opcode::RDMA_WRITE))
            || (region.is_device() && !matches!(opcode, opcode::BIND_MW | opcode::LOCAL_INV))
            || !Rc::ptr_eq(&region.device, &self.device)
            || !region.contains(offset, length)
            || self.pending.borrow().contains_key(&id)
        {
            return Err(Error::InvalidRequest);
        }
        // Receives may be posted before connect (verbs INIT state, or queued
        // until the connection manager creates the QP), so the peer never
        // finds the queue empty once it can send.
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
                mrs: Vec::new(),
                user: None,
            },
        );
        std::sync::atomic::fence(std::sync::atomic::Ordering::Release);
        let mut mrs = Vec::new();
        let rc = post(&mut mrs);
        if rc != 0 {
            self.untrack(id);
            return Err(Error::Io);
        }
        if let Some(pending) = self.pending.borrow_mut().get_mut(&id) {
            pending.mrs = mrs;
        }
        Ok(())
    }

    /// The QP, which a successful [`Self::ready`] check guarantees.
    fn qp(&self) -> *mut c_void {
        self.conn
            .qp
            .get()
            .map_or(std::ptr::null_mut(), NonNull::as_ptr)
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
            |_| {
                let sge = region.sge(offset, length);
                unsafe { (self.device.api.post_send)(self.qp(), &sge, 1, id) }
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
        self.post_ranged(id, true, opcode::RECV, region, offset, length, None, |_| {
            let sge = region.sge(offset, length);
            match self.conn.qp.get() {
                Some(qp) => unsafe { (self.device.api.post_recv)(qp.as_ptr(), &sge, 1, id) },
                // Registration mode before the QP exists.
                None => {
                    self.conn.queued.borrow_mut().push((id, sge));
                    0
                }
            }
        })
    }

    /// Post a signaled `RDMA_WRITE_WITH_IMM` of `length` bytes of `region` at
    /// `offset` to the peer's `address` through `key`, carrying `imm`
    /// (host byte order). Completes with [`opcode::RDMA_WRITE`]. Consumes one
    /// peer receive. In registration mode `imm` must fit in 24 bits, and a
    /// write from the source region registers its memory while in flight.
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
        if address.checked_add(length.into()).is_none()
            || (self.device.mode == Mode::Registration && imm & !registration::TAG_MASK != 0)
        {
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
            |mrs| {
                let qp = self.qp();
                if region.is_dynamic() {
                    let Some(qp) = NonNull::new(qp) else {
                        return libc::EAGAIN;
                    };
                    return registration::post_dynamic(
                        &self.device,
                        qp,
                        offset,
                        length,
                        address,
                        key,
                        imm,
                        id,
                        mrs,
                        &self.held,
                    );
                }
                let sge = region.sge(offset, length);
                unsafe {
                    (self.device.api.post_write)(
                        qp,
                        &sge,
                        1,
                        address,
                        key,
                        imm,
                        POST_IMM | POST_SIGNALED,
                        id,
                    )
                }
            },
        )
    }

    /// Post a signaled, fenced bind of `window` over `length` bytes of
    /// `region` at `offset`, with remote write access. Returns the new rkey.
    /// Completes with [`opcode::BIND_MW`]. The region must be registered
    /// with [`Access::PAGES`] (with or without remote write), and a window
    /// stays tied to the first region it is bound to.
    ///
    /// In registration mode this registers the granted bytes for remote
    /// write (see `registration::grant_span`; `limit` is how many bytes the
    /// granted buffer has from `offset`) and completes at once.
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn bind_window(
        &self,
        id: u64,
        window: &Rc<MemoryWindow>,
        region: &Rc<NativeRegion>,
        offset: u64,
        length: u32,
        limit: u64,
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
        let api = &self.device.api;
        let mut key = window.next_key.get();
        self.post_ranged(
            id,
            false,
            opcode::BIND_MW,
            region,
            offset,
            length,
            Some(window.clone()),
            |_| {
                let Some(raw) = window.raw else {
                    return self
                        .bind_registration(id, window, region, offset, length, limit, &mut key);
                };
                let Some(mr) = region.raw else {
                    return libc::EINVAL;
                };
                unsafe {
                    (api.bind)(
                        self.qp(),
                        raw.as_ptr(),
                        mr.as_ptr(),
                        key,
                        id,
                        offset,
                        length,
                    )
                }
            },
        )?;
        *window.region.borrow_mut() = Some(region.clone());
        if window.raw.is_some() {
            window.next_key.set(inc_rkey(key));
        }
        Ok(key)
    }

    /// Registration-mode bind: register the granted bytes for remote write
    /// and queue the completion.
    #[allow(clippy::too_many_arguments)]
    fn bind_registration(
        &self,
        id: u64,
        window: &MemoryWindow,
        region: &NativeRegion,
        offset: u64,
        length: u32,
        limit: u64,
        key: &mut u32,
    ) -> c_int {
        let api = &self.device.api;
        if region.as_ptr().is_null() {
            return libc::EINVAL;
        }
        if let Some((mr, _)) = window.grant.get() {
            if unsafe { (api.dereg_mr)(mr.as_ptr()) } != 0 {
                return libc::EBUSY;
            }
            window.grant.set(None);
        }
        let start = region.as_ptr() as u64 + offset;
        let limit = limit.min(region.length as u64 - offset);
        let span = registration::grant_span(start, length, limit);
        let (mut lkey, mut rkey) = (0, 0);
        let Some(mr) = NonNull::new(unsafe {
            (api.reg_mr)(
                self.device.raw.as_ptr(),
                start,
                span,
                Access::CONTROL.bits() | Access::REMOTE_WRITE,
                &mut lkey,
                &mut rkey,
            )
        }) else {
            return libc::ENOMEM;
        };
        window.grant.set(Some((mr, rkey)));
        *key = rkey;
        self.synthetic.borrow_mut().push_back(Completion {
            id,
            opcode: opcode::BIND_MW,
            ..Completion::default()
        });
        0
    }

    /// Post a signaled `LOCAL_INV` of `key` on `window`. Not fenced. Completes
    /// with [`opcode::LOCAL_INV`]. In registration mode this deregisters the
    /// grant, so the key stops working, and completes at once.
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
        let api = &self.device.api;
        if window.raw.is_none() && window.grant.get().is_none_or(|(_, rkey)| rkey != key) {
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
            |_| {
                if window.raw.is_some() {
                    return unsafe { (api.invalidate)(self.qp(), key, id) };
                }
                let Some((mr, _)) = window.grant.get() else {
                    return libc::EINVAL;
                };
                if unsafe { (api.dereg_mr)(mr.as_ptr()) } != 0 {
                    return libc::EBUSY;
                }
                window.grant.set(None);
                self.synthetic.borrow_mut().push_back(Completion {
                    id,
                    opcode: opcode::LOCAL_INV,
                    ..Completion::default()
                });
                0
            },
        )
    }

    /// The first completion that broke this QP, if any.
    #[cfg(test)]
    pub(crate) fn failure(&self) -> Option<Failure> {
        self.failure.get()
    }
}

impl<T> Drop for NativeQueuePair<T> {
    /// Stop, then free the CQ. If stop fails, leak the QP's work and device,
    /// since the NIC may still use them.
    fn drop(&mut self) {
        if let Some(cm) = &self.device.cm {
            cm.unregister(&self.conn);
        }
        if self.stop().is_err() {
            self.quota.quarantine();
            std::mem::forget(self.quota.clone());
            std::mem::forget(std::mem::take(self.pending.get_mut()));
            std::mem::forget(std::mem::take(self.held.get_mut()));
            std::mem::forget(self.device.clone());
            return;
        }
        self.device
            .armed
            .borrow_mut()
            .remove(&(self.cq.as_ptr() as usize));
        if unsafe { (self.device.api.cq_free)(self.cq.as_ptr()) } != 0 {
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
    /// The C adapter reports the ABI version Rust requires and exports the
    /// dma-buf registration with the signature Rust calls.
    fn c_adapter_abi_version_and_dmabuf_signature_match() {
        let source = include_str!("../native/verbs.c");
        assert!(source.contains(&format!(
            "uint32_t rdma_verbs_abi(void) {{ return {ABI_VERSION}; }}"
        )));
        assert!(source.contains(
            "void *rdma_verbs_register_dmabuf(struct rdma_verbs_device *d, uint64_t offset, \
             uint64_t length,\n                                 uint64_t iova, int fd)"
        ));
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
    unsafe extern "C" fn open(_: *const c_char, _: c_int) -> *mut c_void {
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

    /// The fixture reports no capabilities.
    unsafe extern "C" fn query_caps(_: *mut c_void, _: u8, _: *mut Caps) -> c_int {
        0
    }

    /// The fixture has no completion channel.
    unsafe extern "C" fn comp_channel(_: *mut c_void) -> *mut c_void {
        std::ptr::null_mut()
    }

    /// No channel, no fd.
    unsafe extern "C" fn comp_channel_fd(_: *mut c_void) -> c_int {
        -1
    }

    /// Nothing to free.
    unsafe extern "C" fn comp_channel_free(_: *mut c_void) -> c_int {
        0
    }

    /// Allocate a fixture completion queue.
    unsafe extern "C" fn cq_create(_: *mut c_void, _: u32, _: *mut c_void) -> *mut c_void {
        pointer()
    }

    /// Record and release the fixture completion queue.
    unsafe extern "C" fn cq_free(p: *mut c_void) -> c_int {
        event("cq");
        unsafe {
            drop(Box::from_raw(p.cast::<u8>()));
        }
        0
    }

    /// Arming always succeeds.
    unsafe extern "C" fn req_notify(_: *mut c_void) -> c_int {
        0
    }

    /// No channel events.
    unsafe extern "C" fn get_cq_event(_: *mut c_void, _: *mut *mut c_void) -> c_int {
        -libc::EAGAIN
    }

    /// Nothing to acknowledge.
    unsafe extern "C" fn ack_cq_events(_: *mut c_void, _: u32) {}

    /// The fixture device has no implicit ODP.
    unsafe extern "C" fn register_implicit(_: *mut c_void) -> *mut c_void {
        std::ptr::null_mut()
    }

    /// The fixture device has no dma-buf support.
    unsafe extern "C" fn register_dmabuf(
        _: *mut c_void,
        _: u64,
        _: u64,
        _: u64,
        _: c_int,
    ) -> *mut c_void {
        std::ptr::null_mut()
    }

    /// Allocate a fixture queue pair with a fixed valid number.
    unsafe extern "C" fn qp(
        _: *mut c_void,
        _: *mut c_void,
        _: u8,
        _: u32,
        _: u32,
        qpn: *mut u32,
    ) -> *mut c_void {
        unsafe {
            *qpn = 7;
        }
        pointer()
    }

    /// Accept connection setup without a physical peer.
    unsafe extern "C" fn connect(_: *mut c_void, _: *const Endpoint, _: *const Endpoint) -> c_int {
        0
    }

    /// Moving to the error state always succeeds.
    unsafe extern "C" fn qp_error(_: *mut c_void) -> c_int {
        0
    }

    /// Record a terminal fence (QP destruction) and apply the scripted
    /// failure state.
    unsafe extern "C" fn qp_free(p: *mut c_void) -> c_int {
        event("stop");
        if FAULTS.with_borrow(|f| f.stop_fails) {
            return 5;
        }
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

    /// Every fixture region has local key 0.
    unsafe extern "C" fn lkey(_: *mut c_void) -> u32 {
        0
    }

    /// The fixture registers nothing.
    unsafe extern "C" fn reg_mr(
        _: *mut c_void,
        _: u64,
        _: u64,
        _: u32,
        _: *mut u32,
        _: *mut u32,
    ) -> *mut c_void {
        std::ptr::null_mut()
    }

    /// Nothing to deregister.
    unsafe extern "C" fn dereg_mr(_: *mut c_void) -> c_int {
        -1
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

    /// Accept or reject a write according to the fault script.
    unsafe extern "C" fn post_write(
        _: *mut c_void,
        _: *const Sge,
        _: u32,
        _: u64,
        _: u32,
        _: u32,
        _: u32,
        _: u64,
    ) -> c_int {
        post_result()
    }

    /// Accept or reject a send according to the fault script.
    unsafe extern "C" fn post_send(_: *mut c_void, _: *const Sge, _: u32, _: u64) -> c_int {
        post_result()
    }

    /// Accept or reject a receive according to the fault script.
    unsafe extern "C" fn post_recv(_: *mut c_void, _: *const Sge, _: u32, _: u64) -> c_int {
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

    /// The fixture has no connection manager.
    unsafe extern "C" fn cm_channel(_: *mut c_void) -> *mut c_void {
        std::ptr::null_mut()
    }

    unsafe extern "C" fn cm_channel_fd(_: *mut c_void) -> c_int {
        -1
    }

    unsafe extern "C" fn cm_channel_free(_: *mut c_void) -> c_int {
        0
    }

    unsafe extern "C" fn cm_listen(
        _: *mut c_void,
        _: *mut c_void,
        _: *const u8,
        _: u16,
        _: *mut *mut c_void,
    ) -> c_int {
        -1
    }

    unsafe extern "C" fn cm_id(_: *mut c_void, _: *mut *mut c_void) -> c_int {
        -1
    }

    unsafe extern "C" fn cm_resolve_addr(
        _: *mut c_void,
        _: *const u8,
        _: *const u8,
        _: u16,
        _: c_int,
    ) -> c_int {
        -1
    }

    unsafe extern "C" fn cm_resolve_route(_: *mut c_void, _: c_int) -> c_int {
        -1
    }

    unsafe extern "C" fn cm_create_qp(
        _: *mut c_void,
        _: *mut c_void,
        _: *mut c_void,
        _: u32,
        _: u32,
        _: *mut u32,
    ) -> *mut c_void {
        std::ptr::null_mut()
    }

    unsafe extern "C" fn cm_connect(_: *mut c_void, _: *const u8, _: u32) -> c_int {
        -1
    }

    unsafe extern "C" fn cm_accept(_: *mut c_void, _: *const u8, _: u32) -> c_int {
        -1
    }

    unsafe extern "C" fn cm_reject(_: *mut c_void) -> c_int {
        -1
    }

    unsafe extern "C" fn cm_get_event(_: *mut c_void, _: *mut CmEvent) -> c_int {
        -libc::EAGAIN
    }

    unsafe extern "C" fn cm_ack(_: *mut c_void) {}

    unsafe extern "C" fn cm_destroy_id(_: *mut c_void) -> c_int {
        -1
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
            query_caps,
            comp_channel,
            comp_channel_fd,
            comp_channel_free,
            cq_create,
            cq_free,
            req_notify,
            get_cq_event,
            ack_cq_events,
            qp,
            connect,
            qp_error,
            qp_free,
            register,
            register_implicit,
            register_dmabuf,
            deregister,
            bytes,
            lkey,
            reg_mr,
            dereg_mr,
            window,
            window_free,
            bind,
            invalidate,
            post_write,
            post_send,
            post_recv,
            poll,
            cm_channel,
            cm_channel_fd,
            cm_channel_free,
            cm_listen,
            cm_id,
            cm_resolve_addr,
            cm_resolve_route,
            cm_create_qp,
            cm_connect,
            cm_accept,
            cm_reject,
            cm_get_event,
            cm_ack,
            cm_destroy_id,
        });
        let device = Rc::new(NativeDevice::new(
            api,
            NonNull::new(pointer()).unwrap(),
            None,
            "test-only".into(),
            Endpoint {
                gid: [1; 16],
                qpn: 0,
                psn: 0,
                mtu: 3,
                lid: 1,
                port: 1,
                link_layer: 1,
            },
            Mode::Window,
        ));
        let qp = NativeQueuePair::new_with_depth(
            device.clone(),
            GuardOwner::new(Arc::new(())),
            Arc::new(std::sync::atomic::AtomicBool::new(false)),
            32,
            4,
            &racer_io::Rng::os(),
        )
        .unwrap();
        qp.connect(qp.endpoint, true).unwrap();
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
        qp.bind_window(wr(1), &window, &region, 0, 32, u64::MAX)
            .unwrap();
        FAULTS.with_borrow_mut(|f| f.stop_fails = true);
        assert_eq!(qp.stop(), Err(Error::Io));
        // Inject a late NIC write while the owner is quarantined.
        unsafe {
            *bytes(region.raw.unwrap().as_ptr()) = 99;
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
        qp.bind_window(wr(1), &window, &region, 0, 32, u64::MAX)
            .unwrap();
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
        let key = qp
            .bind_window(wr(1), &window, &region, 0, 32, u64::MAX)
            .unwrap();
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
        assert_eq!(std::mem::size_of::<Port>(), 112);
        assert_eq!(std::mem::offset_of!(Port, mtu), 80);
        assert_eq!(std::mem::offset_of!(Port, ipv4_gid), 88);
        assert_eq!(std::mem::offset_of!(Port, flags), 108);
        assert_eq!(std::mem::size_of::<Caps>(), 16);
        assert_eq!(std::mem::offset_of!(Caps, ipv4), 12);
        assert_eq!(std::mem::size_of::<Sge>(), 16);
        assert_eq!(std::mem::offset_of!(Sge, lkey), 12);
        assert_eq!(std::mem::size_of::<CmEvent>(), 96);
        assert_eq!(std::mem::offset_of!(CmEvent, kind), 24);
        assert_eq!(std::mem::offset_of!(CmEvent, private_len), 32);
        assert_eq!(std::mem::offset_of!(CmEvent, private), 36);
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
            &racer_io::Rng::os(),
        )
        .unwrap();
        qp.connect(qp.endpoint, true).unwrap();
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
        qp.progress_with(|c, _| out.push(c))?;
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
    /// The QP is the only ledger: an attached record comes back with its
    /// completion, a failed post holds no record, and stop hands the rest back.
    fn ledger_records_return_with_completion_or_after_stop() {
        let (plain, _, _) = fixture();
        let region =
            NativeRegion::with_access(plain.device.clone(), 64, Access::CONTROL, quota().0)
                .unwrap();
        let qp = NativeQueuePair::<&'static str>::new_with_depth(
            plain.device.clone(),
            GuardOwner::new(Arc::new(())),
            Arc::new(std::sync::atomic::AtomicBool::new(false)),
            4,
            1,
            &racer_io::Rng::os(),
        )
        .unwrap();
        qp.connect(qp.endpoint, true).unwrap();
        qp.send(wr(1), &region, 0, 8).unwrap();
        qp.attach(wr(1), "first");
        qp.send(wr(2), &region, 8, 8).unwrap();
        qp.attach(wr(2), "second");
        FAULTS.with_borrow_mut(|f| f.post_fails = true);
        assert_eq!(qp.send(wr(3), &region, 16, 8), Err(Error::Io));
        qp.attach(wr(3), "never posted");
        FAULTS.with_borrow_mut(|f| f.post_fails = false);
        assert_eq!(qp.sends(), 2);
        complete(wr(2), 0, opcode::SEND);
        let mut seen = Vec::new();
        qp.progress_with(|c, record| seen.push((c.id, record)))
            .unwrap();
        assert_eq!(seen, [(wr(2), Some("second"))]);
        assert_eq!(qp.sends(), 1);
        qp.stop().unwrap();
        assert_eq!(qp.sends(), 0);
        assert_eq!(qp.take_records(), ["first"]);
        assert!(qp.take_records().is_empty());
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
                NativeQueuePair::<()>::new_with_depth(
                    qp.device.clone(),
                    GuardOwner::new(Arc::new(())),
                    Arc::new(std::sync::atomic::AtomicBool::new(false)),
                    send,
                    recv,
                    &racer_io::Rng::os(),
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
        assert_eq!(region.as_ptr(), unsafe {
            bytes(region.raw.unwrap().as_ptr())
        });
        let window = MemoryWindow::new(&qp.device).unwrap();
        assert_eq!(window.next_key(), 71);
        let first = qp
            .bind_window(wr(1), &window, &region, 64, 32, u64::MAX)
            .unwrap();
        assert_eq!(first, 71);
        assert_eq!(window.next_key(), 72);
        complete(wr(1), 0, opcode::BIND_MW);
        qp.invalidate_window(wr(2), &window, first).unwrap();
        complete(wr(2), 0, opcode::LOCAL_INV);
        assert_eq!(drain(&qp).unwrap().len(), 2);
        let second = qp
            .bind_window(wr(3), &window, &region, 0, 256, u64::MAX)
            .unwrap();
        assert_ne!(first, second);
        // Low byte wraps, index bits are kept.
        assert_eq!(inc_rkey(0x1234_56ff), 0x1234_5600);
        // Another region, a control region, and out-of-range binds are refused.
        let other =
            NativeRegion::with_access(qp.device.clone(), 256, Access::PAGES, quota().0).unwrap();
        assert_eq!(
            qp.bind_window(wr(4), &window, &other, 0, 8, u64::MAX),
            Err(Error::InvalidRequest)
        );
        let control =
            NativeRegion::with_access(qp.device.clone(), 64, Access::CONTROL, quota().0).unwrap();
        let fresh = MemoryWindow::new(&qp.device).unwrap();
        assert_eq!(
            qp.bind_window(wr(4), &fresh, &control, 0, 8, u64::MAX),
            Err(Error::InvalidRequest)
        );
        assert_eq!(
            qp.bind_window(wr(4), &fresh, &region, 200, 57, u64::MAX),
            Err(Error::InvalidRequest)
        );
        // Invalidating a never-bound window is refused.
        assert_eq!(
            qp.invalidate_window(wr(4), &fresh, 1),
            Err(Error::InvalidRequest)
        );
        // A failed post keeps the key for the next attempt.
        FAULTS.with_borrow_mut(|f| f.post_fails = true);
        assert_eq!(
            qp.bind_window(wr(4), &fresh, &region, 0, 8, u64::MAX),
            Err(Error::Io)
        );
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
                &racer_io::Rng::os(),
            )
            .unwrap()
        };
        let receiver = qp();
        let sender = qp();
        receiver.connect(sender.endpoint, false).unwrap();
        sender.connect(receiver.endpoint, true).unwrap();
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
                qp.progress_with(|c, _| done |= c.id == id)?;
                if done {
                    return Ok(());
                }
                std::thread::sleep(Duration::from_millis(1));
            }
        }

        let window = MemoryWindow::new(&device).unwrap();
        let key = receiver
            .bind_window(wr(1), &window, &destination, 0, 4096, u64::MAX)
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

    /// Wait up to a few seconds for a helper thread to finish.
    fn settle(registering: &Registering) {
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(10);
        while !registering.ready() {
            assert!(
                std::time::Instant::now() < deadline,
                "helper never finished"
            );
            std::thread::yield_now();
        }
    }

    #[test]
    /// A native registration runs on a helper thread, wakes the owner when
    /// done, and its region deregisters normally.
    fn registering_runs_off_thread_and_wakes() {
        let (_qp, region, _charged) = fixture();
        let (quota, charged) = quota();
        let wakes = Arc::new(AtomicUsize::new(0));
        let wake: RegisterWake = {
            let wakes = wakes.clone();
            Arc::new(move || {
                wakes.fetch_add(1, Ordering::AcqRel);
            })
        };
        let owner = std::thread::current().id();
        let registering = Registering::start(
            region.device.clone(),
            None,
            64,
            Access::PAGES,
            quota,
            wake,
            move || {
                assert_ne!(std::thread::current().id(), owner, "ran on the owner");
                Box::into_raw(Box::new(vec![0u8; 64])) as usize
            },
        );
        settle(&registering);
        assert_eq!(wakes.load(Ordering::Acquire), 1);
        let built = registering.finish().unwrap();
        assert_eq!(built.length(), 64);
        drop(built);
        assert_eq!(charged.get(), 0, "charge returned after deregister");
    }

    #[test]
    /// A failed registration reports `Unavailable` and returns the charge.
    fn registering_failure_returns_the_charge() {
        let (_qp, region, _charged) = fixture();
        let (quota, charged) = quota();
        let registering = Registering::start(
            region.device.clone(),
            None,
            64,
            Access::PAGES,
            quota,
            Arc::new(|| {}),
            || 0,
        );
        settle(&registering);
        assert_eq!(registering.finish().err(), Some(Error::Unavailable));
        assert_eq!(charged.get(), 0);
    }

    #[test]
    /// Dropped while the helper still runs, a registration leaks its device
    /// and charge (quarantined) instead of freeing what the helper uses; a
    /// finished but unclaimed one deregisters.
    fn registering_dropped_mid_flight_leaks() {
        let (_qp, region, _charged) = fixture();
        let (charge, charged) = quota();
        let observed = charge.clone();
        let (go, wait) = std::sync::mpsc::channel::<()>();
        let (finished, done) = std::sync::mpsc::channel::<()>();
        let registering = Registering::start(
            region.device.clone(),
            None,
            64,
            Access::PAGES,
            charge,
            Arc::new(move || {
                let _ = finished.send(());
            }),
            move || {
                wait.recv().unwrap();
                0
            },
        );
        assert!(!registering.ready());
        drop(registering);
        assert!(observed.quarantined());
        drop(observed);
        go.send(()).unwrap();
        done.recv().unwrap();
        assert_eq!(charged.get(), 1, "a leaked charge never returns");

        let (quota, charged) = quota();
        let registering = Registering::start(
            region.device.clone(),
            None,
            64,
            Access::PAGES,
            quota,
            Arc::new(|| {}),
            || Box::into_raw(Box::new(vec![0u8; 64])) as usize,
        );
        settle(&registering);
        drop(registering);
        assert_eq!(charged.get(), 0, "an unclaimed region deregisters");
    }
}
