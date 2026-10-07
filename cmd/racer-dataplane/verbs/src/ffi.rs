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

/// One finished work request from the completion queue.
#[repr(C)]
#[derive(Default, Clone, Copy)]
struct Completion {
    id: u64,

    status: u32,

    opcode: u32,
}

/// Build the [`Api`] function table and its symbol loader from a list of
/// `rdma_verbs_<name>` functions.
macro_rules! native_api {
    ($($field:ident: $signature:ty),* $(,)?) => {
        /// The C adapter's functions. Every native object holds an `Rc` to it
        /// so the library stays loaded.
        struct Api {
            library: Option<NonNull<c_void>>,

            $(
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

    qp: unsafe extern "C" fn(*mut c_void, u8, u32, *mut u32) -> *mut c_void,

    connect: unsafe extern "C" fn(*mut c_void, *const Endpoint, *const Endpoint) -> c_int,

    stop: unsafe extern "C" fn(*mut c_void) -> c_int,

    qp_free: unsafe extern "C" fn(*mut c_void) -> c_int,

    register: unsafe extern "C" fn(*mut c_void, u32) -> *mut c_void,

    deregister: unsafe extern "C" fn(*mut c_void) -> c_int,

    bytes: unsafe extern "C" fn(*mut c_void) -> *mut u8,

    window: unsafe extern "C" fn(*mut c_void, *mut u32) -> *mut c_void,

    window_free: unsafe extern "C" fn(*mut c_void) -> c_int,

    bind: unsafe extern "C" fn(*mut c_void, *mut c_void, *mut c_void, u32, u64, u32) -> c_int,

    invalidate: unsafe extern "C" fn(*mut c_void, u32, u64) -> c_int,

    write: unsafe extern "C" fn(*mut c_void, *mut c_void, u64, u32, u64, u32) -> c_int,

    poll: unsafe extern "C" fn(*mut c_void, *mut Completion, u32) -> c_int,
}

impl Api {
    #[cfg(not(all(feature = "native", target_os = "linux")))]
    /// No native backend in this build.
    fn load() -> Result<Rc<Self>> {
        Err(Error::Unavailable)
    }

    #[cfg(all(feature = "native", target_os = "linux"))]
    /// Load `librdma_verbs.so.1` and check it speaks ABI version 2.
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
            if version() != 2 {
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
    mut quota: impl FnMut() -> Option<Arc<GuardOwner>>,
) -> Result<Vec<NativeDevice>> {
    #[cfg(any(test, feature = "simulation"))]
    let api = match simulation::current() {
        Some(_) => simulation::api(),
        None => Api::load()?,
    };
    #[cfg(not(any(test, feature = "simulation")))]
    let api = Api::load()?;
    let mut ports = [Port {
        name: [0; 64],
        gid: [0; 16],
        mtu: 0,
        lid: 0,
        port: 0,
        link_layer: 0,
    }; 64];
    let count = unsafe { (api.discover)(ports.as_mut_ptr(), ports.len() as u32) };
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
        let Some(raw) = NonNull::new(unsafe { (api.open)(port.name.as_ptr()) }) else {
            continue;
        };
        devices.push(NativeDevice {
            api: api.clone(),
            raw,
            quota: quota(),
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
/// `busy` is set while the NIC may touch it. The CPU may not copy in or out
/// while busy. Holds the slot's guard and leaks it if deregister fails.
pub(crate) struct NativeRegion {
    device: Rc<NativeDevice>,

    raw: NonNull<c_void>,

    /// Allocated size.
    length: usize,

    /// Bytes in use for the current transfer.
    used: Cell<usize>,

    quota: Option<Arc<GuardOwner>>,

    busy: Cell<bool>,
}

impl NativeRegion {
    /// Allocate and register `length` bytes.
    pub(crate) fn new(
        device: Rc<NativeDevice>,
        length: usize,
        quota: Arc<GuardOwner>,
    ) -> Result<Rc<Self>> {
        if length == 0 || length > u32::MAX as usize {
            return Err(Error::InvalidRange);
        }
        let raw =
            NonNull::new(unsafe { (device.api.register)(device.raw.as_ptr(), length as u32) })
                .ok_or(Error::Unavailable)?;
        Ok(Rc::new(Self {
            device,
            raw,
            length,
            used: Cell::new(length),
            quota: Some(quota),
            busy: Cell::new(false),
        }))
    }

    /// The address peers use to write here.
    pub(crate) fn address(&self) -> u64 {
        #[cfg(any(test, feature = "simulation"))]
        if simulation::is_api(&self.device.api) {
            return simulation::address(self.raw);
        }
        unsafe { (self.device.api.bytes)(self.raw.as_ptr()) as u64 }
    }

    /// Bytes in use.
    pub(crate) fn length(&self) -> usize {
        self.used.get()
    }

    /// Set bytes in use. Not while busy.
    pub(crate) fn resize(&self, length: usize) -> Result<()> {
        if self.busy.get() || length == 0 || length > self.length {
            return Err(Error::InvalidRange);
        }
        self.used.set(length);
        Ok(())
    }

    /// Copy the buffer out. Not while busy.
    pub(crate) fn copy_into(&self, bytes: &mut [u8]) -> Result<()> {
        if self.busy.get() || bytes.len() != self.length() {
            return Err(Error::Unavailable);
        }
        unsafe {
            std::ptr::copy_nonoverlapping(
                (self.device.api.bytes)(self.raw.as_ptr()),
                bytes.as_mut_ptr(),
                bytes.len(),
            );
        }
        Ok(())
    }

    /// Copy into the buffer. Not while busy.
    pub(crate) fn copy_from(&self, bytes: &[u8]) -> Result<()> {
        if self.busy.get() || bytes.len() != self.length() {
            return Err(Error::InvalidRequest);
        }
        unsafe {
            std::ptr::copy_nonoverlapping(
                bytes.as_ptr(),
                (self.device.api.bytes)(self.raw.as_ptr()),
                self.length(),
            );
        }
        Ok(())
    }

    /// Clear the full allocation, not just the active range. Not while busy.
    pub(crate) fn clear(&self) -> Result<()> {
        if self.busy.get() {
            return Err(Error::Unavailable);
        }
        unsafe {
            std::ptr::write_bytes((self.device.api.bytes)(self.raw.as_ptr()), 0, self.length);
        }
        Ok(())
    }

    #[cfg(test)]
    /// Copy the buffer out, for tests.
    pub(crate) fn copy_to(&self) -> Result<Vec<u8>> {
        if self.busy.get() {
            return Err(Error::Unavailable);
        }
        Ok(unsafe {
            std::slice::from_raw_parts((self.device.api.bytes)(self.raw.as_ptr()), self.length())
        }
        .to_vec())
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

/// A memory window (MW): lets one peer write into a region.
pub(crate) struct Window {
    device: Rc<NativeDevice>,

    raw: NonNull<c_void>,

    /// Remote key for the peer.
    pub(crate) key: u32,

    region: Rc<NativeRegion>,
}

impl Drop for Window {
    /// Free the window. On failure, leak the device and region.
    fn drop(&mut self) {
        if unsafe { (self.device.api.window_free)(self.raw.as_ptr()) } != 0 {
            self.region.quota.as_ref().unwrap().quarantine();
            std::mem::forget(self.device.clone());
            std::mem::forget(self.region.clone());
        }
    }
}

/// Where a work request's result lands.
#[derive(Default)]
struct TicketState {
    result: Cell<Option<Result<()>>>,
}

/// The result of one posted work request.
#[derive(Clone)]
pub(crate) struct Ticket(Rc<TicketState>);

impl Ticket {
    /// The result, or `None` if still in flight.
    pub(crate) fn result(&self) -> Option<Result<()>> {
        self.0.result.get()
    }
}

/// A posted work request. Keeps its region and window alive until it
/// completes or the QP stops.
struct Pending {
    ticket: Ticket,

    /// Expected completion opcode.
    opcode: u32,

    region: Option<Rc<NativeRegion>>,

    _window: Option<Rc<Window>>,
}

impl Pending {
    /// Set the result and mark the region not busy.
    fn finish(self, result: Result<()>) {
        std::sync::atomic::fence(std::sync::atomic::Ordering::Acquire);
        if let Some(region) = &self.region {
            region.busy.set(false);
        }
        self.ticket.0.result.set(Some(result));
    }
}

/// A queue pair (QP) and its completion queue.
///
/// Tracks posted work (up to 32) and the bound window. [`Self::stop`] is the
/// only way to be sure the NIC is done with its memory.
pub(crate) struct NativeQueuePair {
    /// Keeps every QP, CQ, and probe leak charged to its slot.
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

    /// Next work request ID.
    next: Cell<u64>,

    /// Posted work by ID.
    pending: RefCell<BTreeMap<u64, Pending>>,

    /// Bound windows. They stay open until stop, even after the bind completes.
    windows: RefCell<Vec<Rc<Window>>>,

    #[cfg(test)]
    expires: Cell<Option<std::time::Instant>>,
}

impl NativeQueuePair {
    #[cfg(test)]
    /// Create an independently charged QP for low-level tests.
    pub(crate) fn new(device: Rc<NativeDevice>) -> Result<Rc<Self>> {
        Self::new_charged(
            device,
            GuardOwner::new(Arc::new(())),
            Arc::new(std::sync::atomic::AtomicBool::new(false)),
        )
    }

    /// Create a QP with its slot's charge and a random starting PSN.
    pub(crate) fn new_charged(
        device: Rc<NativeDevice>,
        quota: Arc<GuardOwner>,
        stop_observer: Arc<std::sync::atomic::AtomicBool>,
    ) -> Result<Rc<Self>> {
        let mut qpn = 0;
        let raw = NonNull::new(unsafe {
            (device.api.qp)(device.raw.as_ptr(), device.endpoint.port, 64, &mut qpn)
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
            next: Cell::new(1),
            pending: RefCell::new(BTreeMap::new()),
            windows: RefCell::new(Vec::new()),
            #[cfg(test)]
            expires: Cell::new(None),
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

    #[cfg(test)]
    /// The QP's device, for tests.
    pub(crate) fn device(&self) -> &Rc<NativeDevice> {
        &self.device
    }

    /// Check the device can create memory windows, by making and freeing one.
    pub(crate) fn probe_window(&self) -> Result<()> {
        let mut key = 0;
        let window =
            NonNull::new(unsafe { (self.device.api.window)(self.device.raw.as_ptr(), &mut key) })
                .ok_or(Error::Unavailable)?;
        if unsafe { (self.device.api.window_free)(window.as_ptr()) } != 0 {
            self.quota.quarantine();
            std::mem::forget(self.quota.clone());
            std::mem::forget(self.device.clone());
            return Err(Error::Io);
        }
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

    #[cfg(test)]
    /// Set a deadline for [`Self::progress`], for tests.
    pub(crate) fn expire_at(&self, deadline: std::time::Instant) {
        self.expires.set(Some(deadline));
    }

    /// Record a work request before posting it. Up to 32 at once.
    fn reserve(
        &self,
        opcode: u32,
        region: Option<Rc<NativeRegion>>,
        window: Option<Rc<Window>>,
    ) -> Result<(u64, Ticket)> {
        if !self.ready() {
            return Err(Error::Unavailable);
        }
        if self.pending.borrow().len() >= 32 {
            return Err(Error::Overloaded);
        }
        let id = self.next.get();
        self.next.set(id.checked_add(1).ok_or(Error::Overloaded)?);
        let ticket = Ticket(Rc::new(TicketState::default()));
        self.pending.borrow_mut().insert(
            id,
            Pending {
                ticket: ticket.clone(),
                opcode,
                region,
                _window: window,
            },
        );
        Ok((id, ticket))
    }

    /// Check the post result. If the post failed, nothing reached the NIC, so
    /// drop the record.
    fn submitted(&self, id: u64, rc: i32) -> Result<()> {
        if rc != 0 {
            if let Some(pending) = self.pending.borrow_mut().remove(&id) {
                pending.finish(Err(Error::Io));
            }
            return Err(Error::Io);
        }
        Ok(())
    }

    /// Open `region` to remote writes through a new window. One window at a
    /// time. The region stays busy until the QP stops.
    pub(crate) fn bind(&self, region: Rc<NativeRegion>) -> Result<(Rc<Window>, Ticket)> {
        if !Rc::ptr_eq(&region.device, &self.device)
            || region.busy.get()
            || !self.windows.borrow().is_empty()
        {
            return Err(Error::InvalidRequest);
        }
        let mut key = 0;
        let raw =
            NonNull::new(unsafe { (self.device.api.window)(self.device.raw.as_ptr(), &mut key) })
                .ok_or(Error::Unavailable)?;
        let window = Rc::new(Window {
            device: self.device.clone(),
            raw,
            key,
            region: region.clone(),
        });
        let (id, ticket) = self.reserve(5, None, Some(window.clone()))?; // IBV_WC_BIND_MW
        region.busy.set(true);
        std::sync::atomic::fence(std::sync::atomic::Ordering::Release);
        self.windows.borrow_mut().push(window.clone());
        let rc = unsafe {
            (self.device.api.bind)(
                self.raw.as_ptr(),
                raw.as_ptr(),
                region.raw.as_ptr(),
                key,
                id,
                region.length() as u32,
            )
        };
        if let Err(error) = self.submitted(id, rc) {
            self.stop()?;
            return Err(error);
        }
        Ok((window, ticket))
    }

    /// Revoke `window`. The region stays busy until the QP stops.
    pub(crate) fn invalidate(&self, window: Rc<Window>) -> Result<Ticket> {
        if !self.windows.borrow().iter().any(|w| Rc::ptr_eq(w, &window)) {
            return Err(Error::InvalidRequest);
        }
        let (id, ticket) = self.reserve(6, None, Some(window.clone()))?; // IBV_WC_LOCAL_INV
        let rc = unsafe { (self.device.api.invalidate)(self.raw.as_ptr(), window.key, id) };
        self.submitted(id, rc)?;
        Ok(ticket)
    }

    /// Write `region` to the peer at `address` with `key`. The region is busy
    /// until the write completes.
    pub(crate) fn write(&self, region: Rc<NativeRegion>, address: u64, key: u32) -> Result<Ticket> {
        if !Rc::ptr_eq(&region.device, &self.device)
            || region.busy.get()
            || address.checked_add(region.length as u64).is_none()
        {
            return Err(Error::InvalidRequest);
        }
        let (id, ticket) = self.reserve(1, Some(region.clone()), None)?; // IBV_WC_RDMA_WRITE
        region.busy.set(true);
        std::sync::atomic::fence(std::sync::atomic::Ordering::Release);
        let rc = unsafe {
            (self.device.api.write)(
                self.raw.as_ptr(),
                region.raw.as_ptr(),
                address,
                key,
                id,
                region.length() as u32,
            )
        };
        self.submitted(id, rc)?;
        Ok(ticket)
    }

    /// Read up to 32 completions without blocking. Returns how many.
    ///
    /// Any failed or unexpected completion stops the QP and returns
    /// [`Error::Io`].
    pub(crate) fn progress(&self) -> Result<usize> {
        if self.stopped.get() {
            return Ok(0);
        }
        if self.terminating.get() {
            self.stop()?;
            return Err(Error::Cancelled);
        }
        #[cfg(test)]
        if self
            .expires
            .get()
            .is_some_and(|deadline| uring_runtime::environment::now() >= deadline)
        {
            self.stop()?;
            return Err(Error::DeadlineExceeded);
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
                .is_some_and(|p| c.status == 0 && p.opcode == c.opcode);
            if !valid {
                self.stop()?;
                return Err(Error::Io);
            }
            let pending = self.pending.borrow_mut().remove(&c.id).ok_or(Error::Io)?;
            pending.finish(Ok(()));
        }
        Ok(n as usize)
    }

    /// Stop the QP for good. After success the NIC will not touch its memory
    /// again: pending work is canceled and regions are no longer busy.
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
        for (_, pending) in std::mem::take(&mut *self.pending.borrow_mut()) {
            pending.finish(Err(Error::Cancelled));
        }
        for window in self.windows.borrow_mut().drain(..) {
            window.region.busy.set(false);
        }
        Ok(())
    }
}

impl Drop for NativeQueuePair {
    /// Stop, then free. If stop fails, leak the QP's work, windows, and
    /// device, since the NIC may still use them.
    fn drop(&mut self) {
        if self.stop().is_err() {
            self.quota.quarantine();
            std::mem::forget(self.quota.clone());
            std::mem::forget(std::mem::take(self.pending.get_mut()));
            std::mem::forget(std::mem::take(self.windows.get_mut()));
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
        let api = Api::load().expect("built native adapter must load with ABI 2 and all symbols");
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
    use std::time::Duration;

    use std::{collections::VecDeque, time::Instant};

    /// Scripted native failures, queued completions, and teardown observations.
    #[derive(Default)]
    struct Faults {
        events: Vec<&'static str>,

        cq: VecDeque<Completion>,

        stop_fails: bool,

        post_fails: bool,

        poll_fails: bool,
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

    /// Allocate a fixture queue pair with a fixed valid number.
    unsafe extern "C" fn qp(_: *mut c_void, _: u8, _: u32, qpn: *mut u32) -> *mut c_void {
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
    unsafe extern "C" fn register(_: *mut c_void, length: u32) -> *mut c_void {
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
        _: u32,
    ) -> c_int {
        0
    }

    /// Accept invalidation whose completion is injected separately.
    unsafe extern "C" fn invalidate(_: *mut c_void, _: u32, _: u64) -> c_int {
        0
    }

    /// Accept or synchronously reject a write according to the fault script.
    unsafe extern "C" fn write(
        _: *mut c_void,
        _: *mut c_void,
        _: u64,
        _: u32,
        _: u64,
        _: u32,
    ) -> c_int {
        if FAULTS.with_borrow(|f| f.post_fails) {
            5
        } else {
            0
        }
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
            deregister,
            bytes,
            window,
            window_free,
            bind,
            invalidate,
            write,
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
        let qp = NativeQueuePair::new(device.clone()).unwrap();
        qp.connect(qp.endpoint).unwrap();
        let (quota, charged) = quota();
        let region = NativeRegion::new(device, 32, quota).unwrap();
        (qp, region, charged)
    }

    /// Queue a completion for the next fixture progress call.
    pub(crate) fn complete(id: u64, status: u32, opcode: u32) {
        FAULTS.with_borrow_mut(|f| f.cq.push_back(Completion { id, status, opcode }));
    }

    /// Enable or clear repeated terminal-fence failures.
    pub(crate) fn fail_stop(fail: bool) {
        FAULTS.with_borrow_mut(|f| f.stop_fails = fail);
    }

    /// Create an unconnected queue pair alongside charged source memory.
    pub(crate) fn fresh_fixture() -> (Rc<NativeQueuePair>, Rc<NativeRegion>, QuotaObserver) {
        let (qp, region, charged) = fixture();
        let fresh = NativeQueuePair::new(qp.device.clone()).unwrap();
        (fresh, region, charged)
    }

    #[test]
    /// Dropping the waiter must not release memory still owned by native work.
    fn canceled_waiter_retains_source_and_quota_until_terminal_fence() {
        let (qp, region, charged) = fixture();
        let ticket = qp.write(region.clone(), 4096, 7).unwrap();
        drop(ticket);
        drop(region);
        assert_eq!(charged.get(), 1);
        assert_eq!(qp.progress().unwrap(), 0);
        assert_eq!(charged.get(), 1);
        qp.stop().unwrap();
        assert_eq!(charged.get(), 0);
        drop(qp);
        FAULTS.with_borrow(|f| assert_eq!(f.events, ["stop", "mr", "cq", "pd-context"]));
    }

    #[test]
    /// Failed fencing retains the grant and quota while late DMA remains possible.
    fn failed_fence_quarantines_late_remote_writes_and_quota() {
        let (qp, region, charged) = fixture();
        let (window, ticket) = qp.bind(region.clone()).unwrap();
        complete(1, 0, 5);
        qp.progress().unwrap();
        assert_eq!(ticket.result(), Some(Ok(())));
        // Bind completion permits remote DMA, not CPU reuse.
        assert_eq!(region.copy_to(), Err(Error::Unavailable));
        FAULTS.with_borrow_mut(|f| f.stop_fails = true);
        assert_eq!(qp.stop(), Err(Error::Io));
        // Inject a late NIC write while the owner is quarantined.
        unsafe {
            *bytes(region.raw.as_ptr()) = 99;
        }
        assert_eq!(region.copy_to(), Err(Error::Unavailable));
        drop(region);
        drop(window);
        drop(ticket);
        assert_eq!(charged.get(), 1);
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
            let ticket = qp.write(region.clone(), 4096, 7).unwrap();
            drop(region);
            FAULTS.with_borrow_mut(|f| {
                f.stop_fails = true;
                f.poll_fails = mode == 2;
            });
            if mode == 0 {
                complete(1, 10, u32::MAX);
            }
            if mode == 1 {
                complete(99, 0, 1);
            }
            assert_eq!(qp.progress(), Err(Error::Io));
            assert_eq!(ticket.result(), None);
            assert_eq!(charged.get(), 1);
            FAULTS.with_borrow_mut(|f| f.stop_fails = false);
            qp.stop().unwrap();
            assert_eq!(ticket.result(), Some(Err(Error::Cancelled)));
            assert_eq!(charged.get(), 0);
        }
    }

    #[test]
    /// Immediate rejection and successful completion each restore source access.
    fn write_success_and_post_rejection_have_distinct_release_paths() {
        let (qp, region, charged) = fixture();
        FAULTS.with_borrow_mut(|f| f.post_fails = true);
        assert!(qp.write(region.clone(), 4096, 7).is_err());
        assert!(region.copy_from(&[4; 32]).is_ok());
        FAULTS.with_borrow_mut(|f| f.post_fails = false);
        let ticket = qp.write(region.clone(), 4096, 7).unwrap();
        assert!(region.copy_from(&[5; 32]).is_err());
        complete(2, 0, 1);
        qp.progress().unwrap();
        assert_eq!(ticket.result(), Some(Ok(())));
        assert_eq!(region.copy_to().unwrap(), [4; 32]);
        drop(region);
        assert_eq!(charged.get(), 0);
    }

    #[test]
    /// Out-of-order completions release only the corresponding allocation.
    fn reversed_completions_release_only_their_own_allocation() {
        let (qp, first, first_charge) = fixture();
        let (quota, second_charge) = quota();
        let second = NativeRegion::new(qp.device.clone(), 32, quota).unwrap();
        let one = qp.write(first.clone(), 4096, 7).unwrap();
        let two = qp.write(second.clone(), 8192, 8).unwrap();
        drop(first);
        drop(second);
        complete(2, 0, 1);
        qp.progress().unwrap();
        assert_eq!(one.result(), None);
        assert_eq!(two.result(), Some(Ok(())));
        assert_eq!(first_charge.get(), 1);
        assert_eq!(second_charge.get(), 0);
        complete(1, 0, 1);
        qp.progress().unwrap();
        assert_eq!(first_charge.get(), 0);
    }

    #[test]
    /// Destruction after an unrecoverable fence deliberately retains charged owners.
    fn unrecoverable_destroy_never_releases_quarantined_quota() {
        let (qp, region, charged) = fixture();
        let (window, ticket) = qp.bind(region.clone()).unwrap();
        drop(region);
        drop(window);
        drop(ticket);
        FAULTS.with_borrow_mut(|f| f.stop_fails = true);
        drop(qp);
        assert_eq!(charged.get(), 1);
        FAULTS.with_borrow(|f| assert_eq!(f.events, ["stop"]));
        // The intentional bounded quarantine leak is the required last-resort policy.
    }

    #[test]
    /// Expiry fences idle grants and the work table rejects its thirty-third entry.
    fn deadline_retires_idle_remote_grant_and_bounds_submission_table() {
        let (qp, region, _) = fixture();
        let (_window, _ticket) = qp.bind(region.clone()).unwrap();
        qp.expire_at(Instant::now());
        assert_eq!(qp.progress(), Err(Error::DeadlineExceeded));
        assert!(!qp.ready());
        assert!(region.copy_to().is_ok());
        let (qp, region, _) = fixture();
        for _ in 0..32 {
            qp.reserve(1, None, None).unwrap();
        }
        assert!(matches!(qp.reserve(1, None, None), Err(Error::Overloaded)));
        drop(region);
    }

    #[test]
    /// Key revocation alone never permits CPU reuse of remotely writable memory.
    fn invalidation_cqe_does_not_release_remote_buffer_before_terminal_fence() {
        let (qp, region, charged) = fixture();
        let (window, bind) = qp.bind(region.clone()).unwrap();
        complete(1, 0, 5);
        qp.progress().unwrap();
        assert_eq!(bind.result(), Some(Ok(())));
        let invalidation = qp.invalidate(window.clone()).unwrap();
        complete(2, 0, 6);
        qp.progress().unwrap();
        assert_eq!(invalidation.result(), Some(Ok(())));
        assert_eq!(region.copy_to(), Err(Error::Unavailable));
        assert_eq!(charged.get(), 1);
        qp.stop().unwrap();
        assert!(region.copy_to().is_ok());
        drop(region);
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
        assert_eq!(std::mem::size_of::<Completion>(), 16);
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
    /// Real loopback DMA copies bytes and rejects a capability after invalidation.
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
        let receiver = NativeQueuePair::new(device.clone()).unwrap();
        let sender = NativeQueuePair::new(device.clone()).unwrap();
        receiver.connect(sender.endpoint).unwrap();
        sender.connect(receiver.endpoint).unwrap();
        let destination = NativeRegion::new(device.clone(), 4096, quota().0).unwrap();
        let source = NativeRegion::new(device.clone(), 4096, quota().0).unwrap();
        source.copy_from(&vec![0xa5; 4096]).unwrap();
        let (window, bind) = receiver.bind(destination.clone()).unwrap();

        /// Poll a real provider with a bounded completion deadline.
        fn wait(qp: &NativeQueuePair, ticket: &Ticket) {
            let until = Instant::now() + Duration::from_secs(10);
            while ticket.result().is_none() {
                assert!(Instant::now() < until, "native CQ timeout");
                qp.progress().unwrap();
                std::thread::sleep(Duration::from_millis(1));
            }
            ticket.result().unwrap().unwrap();
        }

        wait(&receiver, &bind);
        let write = sender
            .write(source, destination.address(), window.key)
            .unwrap();
        wait(&sender, &write);
        let stale_key = window.key;
        let inv = receiver.invalidate(window).unwrap();
        wait(&receiver, &inv);
        // A completed local invalidation must reject a subsequent write using the
        // old capability. It still does not authorize CPU access before the fence.
        assert_eq!(destination.copy_to(), Err(Error::Unavailable));
        let stale = NativeRegion::new(device, 4096, quota().0).unwrap();
        stale.copy_from(&vec![0xff; 4096]).unwrap();
        let rejected = sender
            .write(stale, destination.address(), stale_key)
            .unwrap();
        let until = Instant::now() + Duration::from_secs(10);
        while rejected.result().is_none() {
            assert!(Instant::now() < until, "stale-key completion timeout");
            let _ = sender.progress();
            std::thread::sleep(Duration::from_millis(1));
        }
        assert!(
            rejected.result().unwrap().is_err(),
            "invalidated rkey still allowed a write"
        );
        receiver.stop().unwrap();
        sender.stop().unwrap();
        assert_eq!(destination.copy_to().unwrap(), vec![0xa5; 4096]);
    }
}
